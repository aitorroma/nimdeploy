# HTTP endpoints

| Endpoint | |
|---|---|
| `POST <path>` | webhook. `202` started or queued · `200` ping, ignored (other branch, `when` not met) or duplicate · `400` invalid JSON or invalid param · `401` bad signature or token · `409` running and `queue = false` |
| `POST /deploy/{name}` | manual deploy, optional body `{"commit":"...","user":"...","params":{"NAME":"value"},"delivery":"id"}`. The same `delivery` twice runs once (`200` duplicate). Token required |
| `GET /status` | state of every deploy (`?label=key=value` filters, repeatable). Token required if `api_token_env` is set |
| `GET /status/{name}` | state of one deploy. Same |
| `GET /history/{name}?limit=N` | past deploys from the kept logs, newest first. Same |
| `POST /rollback/{name}` | redeploy the last good commit, or `{"commit":"..."}`. Token required |
| `GET /metrics` | Prometheus metrics ([details](../guides/metrics.md)). Token required if `api_token_env` is set |
| `GET /healthz` | liveness, always open |
| `GET /debug/pprof/` | Go profiler, with `server.pprof = true` ([details](../guides/observability.md#pprof)). Token required |

A path shared by [several deploys](../guides/generic.md#several-deploys-on-one-path)
answers `{"results": [{"deploy": "...", "code": 202, "response": {...}}, ...]}`.

With [client certificates](../guides/tls.md), deploys with `client_names` and
the API with `api_client_names` answer `403` to other certificates.

Every POST continues a W3C `traceparent` once authenticated, when
[OpenTelemetry](../guides/observability.md) is on.

With `api_token_env` set, send `Authorization: Bearer <token>`.

## Status object

```json
{
  "deploy": "agency",
  "status": "success",
  "started_at": "2026-09-29T12:54:33+02:00",
  "finished_at": "2026-09-29T12:56:01+02:00",
  "duration": "1m28s",
  "delivery": "51af02d4-...",
  "repository": "acme/agency",
  "branch": "main",
  "commit": "9f1c2e...",
  "params": [{"name": "SERVICE", "value": "api"}],
  "trigger": "webhook",
  "exit_code": 0,
  "log": "20260929-125433-51af02.log",
  "queued": {"commit": "a41f...", "since": "2026-09-29T12:55:10+02:00"}
}
```

`status` is one of:

| | |
|---|---|
| `never` | no run since the deploy was configured |
| `waiting` | waiting for CI |
| `running` | |
| `success` | |
| `failed` | non-zero exit or timeout |
| `skipped` | CI failed, timed out, or a newer push superseded it |
| `interrupted` | the service stopped mid-deploy |

`trigger` is `webhook`, `manual`, `rollback` or `schedule`; scheduled deploys
also have `next_run`.

`queued` appears only while a push waits. A queued push is lost if the
service stops before it runs; that is logged.
