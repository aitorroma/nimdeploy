# Metrics and alerts

`GET /metrics` serves Prometheus metrics about every deploy. It follows the
same rule as `/status`: with `api_token_env` set, it needs the Bearer token.

```yaml title="prometheus.yml"
scrape_configs:
  - job_name: nimdeploy
    authorization:
      credentials_file: /etc/prometheus/nimdeploy.token   # the NIMDEPLOY_API_TOKEN value
    static_configs:
      - targets: ["127.0.0.1:9000"]
```

nimdeploy listens on localhost: scrape it from an agent on the same host (a
local Prometheus or Grafana Alloy), or publish `/metrics` through your proxy
only to your monitoring network.

## Metrics

| Metric | Type | |
|---|---|---|
| `nimdeploy_build_info{version,goversion}` | gauge | always 1 |
| `nimdeploy_start_time_seconds` | gauge | when the service started |
| `nimdeploy_deploys_total{deploy,status}` | counter | finished runs: `success`, `failed`, `skipped`, `interrupted` |
| `nimdeploy_deploy_duration_seconds{deploy}` | histogram | duration of runs that ran (buckets 5 s … 1 h) |
| `nimdeploy_deploy_running{deploy}` | gauge | runs in progress |
| `nimdeploy_deploy_queued{deploy}` | gauge | runs waiting |
| `nimdeploy_deploy_last_success{deploy}` | gauge | 1 if the last run succeeded, 0 if it failed |
| `nimdeploy_deploy_last_finished_timestamp_seconds{deploy}` | gauge | |
| `nimdeploy_deploy_last_success_timestamp_seconds{deploy}` | gauge | |
| `nimdeploy_deploy_last_failure_timestamp_seconds{deploy}` | gauge | |
| `nimdeploy_deploy_next_run_timestamp_seconds{deploy}` | gauge | scheduled deploys |
| `nimdeploy_webhook_requests_total{deploy,code}` | counter | webhook answers: `202` accepted, `200` ignored/duplicate, `401` bad signature, `400` invalid, `409`/`503` busy |

Counters start at zero when the service starts; Prometheus' `rate()` and
`increase()` handle that.

## Alerts

```yaml title="nimdeploy.rules.yml"
groups:
  - name: nimdeploy
    rules:
      - alert: DeployFailing
        expr: nimdeploy_deploy_last_success == 0
        for: 5m
        annotations:
          summary: "{{ $labels.deploy }}: last deploy failed"

      - alert: DeploySlow
        expr: histogram_quantile(0.9, sum by (deploy, le) (rate(nimdeploy_deploy_duration_seconds_bucket[1d]))) > 900
        annotations:
          summary: "{{ $labels.deploy }}: deploys take over 15 min"

      - alert: DeployQueueGrowing
        expr: nimdeploy_deploy_queued > 10
        for: 15m
        annotations:
          summary: "{{ $labels.deploy }}: {{ $value }} runs waiting"

      - alert: ScheduledJobLate
        expr: time() - nimdeploy_deploy_last_success_timestamp_seconds{deploy="backup"} > 26 * 3600
        annotations:
          summary: "backup has not succeeded in over a day"

      - alert: WebhookSignatureFailures
        expr: increase(nimdeploy_webhook_requests_total{code="401"}[1h]) > 5
        annotations:
          summary: "{{ $labels.deploy }}: webhooks with a wrong secret (rotated? attack?)"

      - alert: NimdeployDown
        expr: up{job="nimdeploy"} == 0
        for: 5m
```

`up == 0` covers nimdeploy itself; for "no deploys arriving at all" use the
webhook counters, e.g. `increase(nimdeploy_webhook_requests_total{code="202"}[7d]) == 0`.
