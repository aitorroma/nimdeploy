# Logs, traces and OpenTelemetry

nimdeploy covers three of OpenTelemetry's signals without extra agents:

| | |
|---|---|
| **Metrics** | `GET /metrics` in the Prometheus format (see [Metrics](metrics.md)), and optionally pushed over OTLP |
| **Logs** | the service log as text or JSON lines, with secrets hidden; optionally over OTLP |
| **Traces** | a trace per webhook and deploy, over OTLP, continued into your scripts and playbooks |

For profiling, there is Go's `pprof` (below); continuous profiling is better
done from outside (eBPF agents such as Grafana Alloy or Parca).

## JSON logs

```toml
[logging]
format = "json"     # default "text"
```

Each line of the service log (journald, `docker logs`) becomes one object:

```json
{"time":"2026-10-08T21:03:41.1Z","level":"info","service":"nimdeploy","msg":"","deploy":"svc","status":"success","duration":"1s","run":"20261008-230340-real1","trace_id":"0af7651916cd43dd8448eb211c80319c","log":"/var/log/nimdeploy/svc/20261008-230340-real1.log"}
{"time":"2026-10-08T21:03:40.0Z","level":"info","service":"nimdeploy","msg":"http POST /hooks/svc","status":"202","client":"203.0.113.7","delivery":"72d4…","duration":"1ms","trace_id":"0af7651916cd43dd8448eb211c80319c"}
```

- `level` is `error` for failures, `warn` for rejected or dropped requests,
  else `info`.
- `run` is the deploy's log file without `.log`: it ties the service log, the
  history and the hub together. `trace_id` ties them to the trace.
- The values of every configured secret (webhook secrets, tokens, the
  notification URL, OTLP headers) are replaced by `[REDACTED]`, in JSON and
  in text.
- The per-deploy log files (`nimdeploy history`) stay plain text.

The hub has `-log-format json` / `NIMDEPLOY_HUB_LOG_FORMAT=json`.

## OpenTelemetry (OTLP)

```toml
[otel]
endpoint = "http://otel-collector:4318"   # OTLP/HTTP; /v1/traces, /v1/logs, /v1/metrics are added
# traces = true                           # default with an endpoint
logs = true                               # the service log, as OTLP log records
metrics = true                            # the /metrics values every metrics_interval
# metrics_interval = "1m"
# sample_ratio = 1.0                      # fraction of new traces kept
# propagate = true                        # continue a caller's trace (see below)
# service_name = "nimdeploy"
# headers_env = "OTEL_HEADERS"            # "Authorization=Basic xxx,X-Scope-OrgID=acme"
# ca_file = "/etc/nimdeploy/tls/collector-ca.pem"
# cert_file = "/etc/nimdeploy/tls/nimdeploy.pem"   # mTLS towards the collector
# key_file = "/etc/nimdeploy/tls/nimdeploy.key"
```

It speaks OTLP/HTTP with the JSON encoding, so it works with the
OpenTelemetry Collector, Grafana Alloy, Jaeger, Tempo, Honeycomb, Dash0,
SigNoz and others. It has no SDK and no dependencies. Exporting never slows a
deploy: if the endpoint can't keep up, records are dropped and counted in
`nimdeploy_otel_dropped_total`, and the log says when exporting stops and
resumes working. Changing `[otel]` needs a restart.

### Traces

```text
POST /hooks/svc                 1 ms   http.response.status_code=202
└─ deploy svc                1016 ms   nimdeploy.status=success
   ├─ queue.wait                       (when it waited for a running deploy)
   ├─ ci.wait                          (wait_for_ci)
   ├─ hook before                4 ms
   ├─ command                 1010 ms   process.exit.code=0   (or ansible-playbook, with host counts)
   ├─ health_check               1 ms
   ├─ cloudflare.purge
   ├─ hook after_success
   └─ email
```

- Spans carry the deploy, run, trigger, repository, branch, commit, delivery,
  labels (`nimdeploy.label.client`...) and the result. The `environment` label
  also becomes the resource's `deployment.environment.name`.
- **Propagation**: a webhook or API call with a W3C `traceparent` header
  continues that trace, **only after the request authenticated** (signature,
  token). An unsigned request can't attach spans to someone else's trace.
- **Scripts and playbooks** get `TRACEPARENT` for their own span (`command`,
  each hook). Tools that read it, like Ansible's
  `community.general.opentelemetry` callback or `otel-cli`, nest their spans
  under the deploy.
- The service log's `trace_id` and the OTLP log records' `traceId` link logs
  to traces.

### Metrics over OTLP

`metrics = true` sends the same values as `/metrics`: counters as cumulative
sums, histograms with their buckets, and gauges. Prometheus can still scrape
`/metrics`; use one or the other per backend to avoid counting twice.

## A collector next to it

```yaml title="otel-collector.yaml"
receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:4318
exporters:
  otlphttp:
    endpoint: https://otlp.example.com
processors:
  batch: {}
service:
  pipelines:
    traces:  {receivers: [otlp], processors: [batch], exporters: [otlphttp]}
    logs:    {receivers: [otlp], processors: [batch], exporters: [otlphttp]}
    metrics: {receivers: [otlp], processors: [batch], exporters: [otlphttp]}
```

## pprof

```toml
[server]
api_token_env = "NIMDEPLOY_API_TOKEN"
pprof = true
```

`/debug/pprof/` then serves Go's profiler, behind the API token (and
`api_client_names` with [mTLS](tls.md)):

```bash
curl -H "Authorization: Bearer $NIMDEPLOY_API_TOKEN" -o cpu.pprof "http://127.0.0.1:9000/debug/pprof/profile?seconds=30"
go tool pprof cpu.pprof
```

Leave it off unless you are investigating something.
