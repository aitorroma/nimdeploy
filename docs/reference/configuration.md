# Configuration

One TOML file plus a secrets file:

| Install | Config | Secrets |
|---|---|---|
| without root | `~/.config/nimdeploy/config.toml` | `~/.config/nimdeploy/secrets.env` |
| service account | `~deploy/.config/nimdeploy/config.toml` | `~deploy/.config/nimdeploy/secrets.env` |
| as root | `/etc/nimdeploy/config.toml` | `/etc/nimdeploy/secrets.env` |

The config only holds the **names** of environment variables (`*_env`); the
values live in `secrets.env` (`NAME=value`, mode `600`), which systemd loads
as the service's environment. The config can therefore be shared or
committed; the secrets file never.

```bash
nimdeploy -check                 # validate config and secrets, exit 0/1
systemctl reload nimdeploy       # apply config.toml; running deploys continue
systemctl restart nimdeploy      # after changing secrets.env, listen or logging.directory
```

A complete annotated example is in
[`config.example.toml`](https://github.com/aitorroma/nimdeploy/blob/main/config.example.toml).

## `[server]`

| Key | Default | |
|---|---|---|
| `listen` | `127.0.0.1:9000` | `host:port` or `unix:/path/to.sock` |
| `socket_mode` | `0666` | permissions of the unix socket |
| `base_path` | – | only if the proxy forwards `/prefix/hooks/...` without stripping `/prefix` |
| `trusted_proxies` | `["127.0.0.0/8", "::1"]` | proxies whose `X-Forwarded-For` / `X-Real-IP` are believed; `"cloudflare"` adds Cloudflare's ranges |
| `client_ip_header` | – | e.g. `CF-Connecting-IP`, read only from trusted proxies |
| `max_body_bytes` | `26214400` (25 MB) | larger requests are rejected |
| `shutdown_timeout` | `5m` | on stop, wait this long for running deploys |
| `api_token_env` | – | env var with the Bearer token for `/status`, `/history`, `/deploy` and the CLI. Without it `/status` is open and manual deploys are disabled |
| `tls_cert_file`, `tls_key_file` | – | serve HTTPS ([details](../guides/tls.md)); re-read when they change |
| `tls_client_ca_file` | – | CA of client certificates (mTLS) |
| `tls_client_auth` | `optional` with a CA | `optional` or `require` |
| `tls_min_version` | `1.2` | or `1.3` |
| `api_client_names` | – | the API also needs a client certificate with one of these names |
| `pprof` | `false` | `/debug/pprof/` behind the token ([details](../guides/observability.md#pprof)) |

## `[logging]`

| Key | Default | |
|---|---|---|
| `directory` | `/var/log/nimdeploy` | one subdirectory per deploy (user install: `~/.local/state/nimdeploy`) |
| `retain` | `30` | log files kept per deploy; `0` keeps all |
| `format` | `text` | service log as `text` or `json` ([details](../guides/observability.md#json-logs)); secrets are hidden in both |

## `[notify]`

| Key | Default | |
|---|---|---|
| `format` | – | `slack`, `discord`, `telegram` or `json` |
| `on` | `failure` | `failure` (failures and recoveries), `always`, `never` |
| `url_env` | – | env var with the webhook URL (all formats except Telegram) |
| `telegram_token_env`, `telegram_chat_id` | – | Telegram bot token variable and chat ID |
| `log_lines` | `20` | last log lines included in failure messages |

See [Notifications](../guides/notifications.md).

## `[github]`

| Key | Default | |
|---|---|---|
| `token_env` | – | env var with a GitHub token, for commit statuses and `wait_for_ci` |
| `commit_status` | `true` | `false`: use the token only for `wait_for_ci` |
| `api_url` | `https://api.github.com` | GitHub Enterprise Server API URL |

## `[smtp]`

| Key | Default | |
|---|---|---|
| `host` | – | SMTP server |
| `port` | `587` (`465` with `tls = "tls"`) | |
| `tls` | `starttls` | `starttls`, `tls`, or `none` (only for a relay on localhost) |
| `user_env`, `password_env` | – | env vars with the credentials |
| `from` | – | `Name <address>` |

## `[pwpush]`

| Key | Default | |
|---|---|---|
| `url` | `https://eu.pwpush.com` | Password Pusher instance (API v2) |
| `token_env` | – | API token (optional if the instance allows anonymous pushes) |
| `expire_after_views` | `3` | |
| `expire_after_days` | instance default | open source instances |
| `expire_after_duration` | instance default | pwpush.com / Pro: their duration index 0-17 |
| `retrieval_step` | `true` | extra click before showing the secret (protects against link scanners) |
| `deletable_by_viewer` | `true` | |

## `[cloudflare]`

| Key | Default | |
|---|---|---|
| `api_token_env` | – | env var with an API token allowed to purge the cache |
| `api_url` | `https://api.cloudflare.com/client/v4` | |

## `[hub]` TLS

Besides `url`, `agent`, `token_env`, `send_log_tail` and `heartbeat`, the
agent takes `ca_file` (the hub's CA) and `cert_file`/`key_file` (its client
certificate, when the hub requires one). See [TLS](../guides/tls.md#hub-and-agents).

## `[otel]`

OpenTelemetry export over OTLP/HTTP ([details](../guides/observability.md)).

| Key | Default | |
|---|---|---|
| `endpoint` | – | e.g. `http://otel-collector:4318`; nothing is exported without it |
| `traces` | `true` | |
| `logs` | `false` | the service log as OTLP log records |
| `metrics` | `false` | the `/metrics` values every `metrics_interval` |
| `metrics_interval` | `1m` | at least `5s` |
| `sample_ratio` | `1.0` | fraction of new traces kept |
| `propagate` | `true` | continue an authenticated request's `traceparent` |
| `service_name` | `nimdeploy` | |
| `headers_env` | – | env var with `key=value,key2=value2` headers |
| `timeout` | `10s` | per export request |
| `ca_file`, `cert_file`, `key_file` | – | TLS and mTLS towards the collector |

## `[labels]`

`key = "value"` pairs for every deploy on this server, e.g. `client`,
`environment`. See [Labels](../guides/labels.md).

## `[hub]`

Send events to a [central hub](../guides/hub.md).

| Key | Default | |
|---|---|---|
| `url` | – | the hub's base URL (`https://hub.example.com`); without it, nothing is sent |
| `agent` | short hostname | this server's name on the hub (`nimdeploy hub agent add <name>`) |
| `token_env` | required with `url` | env var with this agent's token |
| `send_log_tail` | `20` | last log lines sent with a failed deploy (0-500) |
| `heartbeat` | `1m` | inventory interval (at least `10s`) |

## `[deploy.<name>]`

One table per deploy. The name is used in the CLI, the log directory and
`DEPLOY_NAME`.

| Key | Default | |
|---|---|---|
| `path` | required (unless `schedule`) | URL path the git host posts to, e.g. `/hooks/shop`. Several deploys may [share one](../guides/generic.md#several-deploys-on-one-path) |
| `provider` | `github` | `github`, `gitea`, `forgejo`, `gitlab`, `bitbucket` ([details](../guides/providers.md)), `generic` for any JSON webhook ([details](../guides/generic.md)), `woocommerce` ([details](../guides/woocommerce.md)), `stripe`, `paddle`, `lemonsqueezy` ([details](../guides/payments.md)) |
| `repository` | required | repository the pushes must come from, as the provider names it; any other is ignored. Optional for `generic` |
| `branch` | `main` | pushes to other branches are ignored. Not for `generic` |
| `secret_env` | required | env var holding this deploy's webhook secret; startup fails if it is empty |
| `command` | required, unless `ansible` | run directly, no shell |
| `args` | – | arguments; for an inline script use `command = "/bin/bash"`, `args = ["-c", "..."]` |
| `working_directory` | service's cwd | |
| `env` | – | extra `KEY=VALUE` entries, e.g. `PATH=...` |
| `timeout` | `30m` | then the whole process group gets `SIGTERM`, and `SIGKILL` 10 s later |
| `lock` | `true` | one run at a time; `false` allows parallel runs |
| `queue` | `true` | with `lock`, a push during a run waits for it; later pushes replace the queued one. `false`: answer `409` |
| `log_output` | `true` | `false` keeps only the header and footer lines in the log |
| `wait_for_ci` | – | GitHub Actions workflow names that must pass first ([details](../guides/wait-for-ci.md)) |
| `ci_timeout` | `30m` | |
| `when` | – | table of `"json.path" = value`, a list of values, or a table of [operators](../guides/generic.md#operators) (`match`, `not`, `gt`, `exists`...), that must all match; otherwise `200 ignored` |
| `params` | – | table of values taken from the JSON and passed as environment variables, each validated ([details](../guides/generic.md#params)) |
| `queue_key` | – | param whose value gets its own lock and queue |
| `queue_mode` | `latest` (`all` for woocommerce) | `latest`: only the newest waiting run is kept. `all`: every run waits its turn, in order, saved in `queue.json` so a restart resumes them (and runs again one it interrupted) |
| `queue_max` | `1000` | with `all`: beyond it, `503` |
| `schedule` | – | cron schedule (`*/15 * * * *`, `@daily`, `@every 10m`); without `path` the deploy only runs on it ([details](../guides/scheduled.md)) |
| `before`, `after_success`, `after_failure` | – | bash run around the command ([details](../guides/hooks-rollback.md)) |
| `health_url`, `health_timeout` | –, `60s` | must answer 2xx/3xx after the command, or the deploy fails |
| `rollback_on_failure` | `false` | git deploys: redeploy the last good commit when a deploy fails |
| `cloudflare_zone_id`, `cloudflare_purge` | – | purge `["everything"]` or URLs after a successful deploy (needs `[cloudflare]`) |
| `events` | – | stripe, paddle, lemonsqueezy: event types that run ([details](../guides/payments.md)) |
| `email` | – | table: `on`, `to`, `to_from`, `bcc`, `subject`, `template`, `secrets`, `once` ([details](../guides/email.md)) |
| `labels` | – | table merged over `[labels]` for this deploy ([details](../guides/labels.md)) |
| `ansible` | – | table: run `ansible-playbook`/`ansible-pull` instead of `command` ([details](../guides/ansible.md#options)) |
| `when_any` | – | like `when`, but one condition is enough ([details](../guides/generic.md#when_any-one-of-them-is-enough)) |
| `client_names` | – | only a client certificate with one of these names may call the webhook ([details](../guides/tls.md)) |
| `payload_file` | `true` for generic, woocommerce and payments | pass the request body to the command as `DEPLOY_PAYLOAD_FILE` (mode `600`, deleted after the run) |

WooCommerce adds `store_url`, `webhook_url`, `api_key_env`, `api_secret_env`,
`topics` and `statuses`: see [WooCommerce](../guides/woocommerce.md#configuration).

Generic webhooks add `auth`, `signature_header`, `timestamp_header`,
`max_skew`, `token_header`, `delivery_header` and `pusher_from`: see
[Generic webhooks](../guides/generic.md#reference).

## Example

```toml
[server]
listen = "127.0.0.1:9000"
api_token_env = "NIMDEPLOY_API_TOKEN"

[logging]
directory = "/var/log/nimdeploy"
retain = 30

[notify]
format = "slack"
url_env = "NOTIFY_WEBHOOK_URL"

[deploy.agency-frontend]
path = "/hooks/agency-frontend"
repository = "acme/agency-frontend"
branch = "main"
secret_env = "AGENCY_FRONTEND_WEBHOOK_SECRET"
working_directory = "/var/www/frontend/agency"
command = "/home/deploy/bin/deploy-agency-frontend.sh"

[deploy.agency-backend]
path = "/hooks/agency-backend"
repository = "acme/agency-backend"
branch = "main"
secret_env = "AGENCY_BACKEND_WEBHOOK_SECRET"
working_directory = "/var/www/backend/agency"
command = "/home/deploy/bin/deploy-agency-backend.sh"
timeout = "20m"
```

```bash title="secrets.env (600)"
NIMDEPLOY_API_TOKEN=3f9c...
AGENCY_FRONTEND_WEBHOOK_SECRET=8a21...
AGENCY_BACKEND_WEBHOOK_SECRET=d07e...
NOTIFY_WEBHOOK_URL=https://hooks.slack.com/services/...
```

Generate secrets with `openssl rand -hex 32`. The same values go into each
repository's webhook settings.
