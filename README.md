# nimdeploy

[![CI](https://github.com/aitorroma/nimdeploy/actions/workflows/ci.yml/badge.svg)](https://github.com/aitorroma/nimdeploy/actions/workflows/ci.yml)

Receives GitHub `push` webhooks and runs a deploy command per repository. Every
run gets its own log file, `latest.log` always points at the newest one, and
`/status` tells you what each deploy is doing.

- Pushes that arrive during a deploy are queued; the latest one wins.
- Failure (and recovery) notifications to Slack, Discord, Telegram or any URL.
- ✅/❌ commit statuses on GitHub.
- `nimdeploy run` / `nimdeploy status` from the server's shell.
- `systemctl reload nimdeploy` applies config changes without interrupting deploys.

```text
/var/log/nimdeploy/
├── agency/
│   ├── 20260929-125433-51af02.log      # <date>-<time>-<first 6 chars of X-GitHub-Delivery>
│   ├── 20260929-131002-c93a11.log
│   ├── latest.log -> 20260929-131002-c93a11.log
│   └── status.json                     # last state, survives restarts
└── frontend/
    └── ...
```

## Install

From a [release](https://github.com/aitorroma/nimdeploy/releases) (Linux amd64/arm64, static binary):

```bash
VERSION=v0.1.0 ARCH=amd64   # or arm64
curl -fsSL https://github.com/aitorroma/nimdeploy/releases/download/$VERSION/nimdeploy_${VERSION}_linux_${ARCH}.tar.gz | tar xz
cd nimdeploy_${VERSION}_linux_${ARCH}
sudo ./install.sh
```

From source (Go 1.26+):

```bash
git clone https://github.com/aitorroma/nimdeploy && cd nimdeploy
make install                      # build + sudo ./install.sh
```

`install.sh` installs the binary to `/usr/local/bin`, the example config and a
secrets template to `/etc/nimdeploy/` (existing ones are never overwritten), and
the systemd unit, then enables the service. It generates `NIMDEPLOY_API_TOKEN`
in `secrets.env` if missing, and starts the service only once `secrets.env` has
no `change-me` placeholders; on upgrades it restarts it.

Deploys run as the `deploy` user (created if missing). Use another one with
`sudo SERVICE_USER=www-data ./install.sh`; it must own the working directories.

```bash
sudo ./install.sh uninstall           # keeps /etc/nimdeploy and the logs
sudo ./install.sh uninstall --purge   # removes them too
```

`nimdeploy -check` validates the config; the unit runs it as `ExecStartPre`
and before every reload.

```bash
sudo systemctl reload nimdeploy   # apply config.toml changes, running deploys continue
sudo systemctl restart nimdeploy  # needed after editing secrets.env, listen or logging.directory
```

## Configuration

See [`config.example.toml`](config.example.toml). Per deploy:

| key | default | |
|---|---|---|
| `path` | required | URL path GitHub posts to |
| `repository` | required | `owner/repo`, must match the payload |
| `branch` | `main` | other branches are ignored |
| `secret_env` | required | env var holding the webhook secret; startup fails if empty |
| `command`, `args` | required | run directly, no shell. Use `command = "/bin/bash"`, `args = ["-c", "..."]` for inline scripts |
| `working_directory` | service cwd | |
| `env` | – | extra `KEY=VALUE` entries |
| `timeout` | `30m` | on timeout the whole process group gets SIGTERM, then SIGKILL 10s later |
| `lock` | `true` | one deploy at a time (`false` allows parallel runs) |
| `queue` | `true` | with `lock`, a push during a deploy runs after it; later pushes replace the queued one. `false` answers `409 Conflict` |
| `log_output` | `true` | `false` keeps only the header/footer lines |

The command also receives `DEPLOY_NAME`, `DEPLOY_TRIGGER` (`webhook` or
`manual`), `DEPLOY_REPOSITORY`, `DEPLOY_REF`, `DEPLOY_BRANCH`, `DEPLOY_COMMIT`,
`DEPLOY_PUSHER` and `DEPLOY_DELIVERY`. Every secret named in the config (`*_env`)
is removed from its environment.

`DEPLOY_COMMIT` is empty for a manual run without `-commit`, so scripts should
fall back to the branch: `git reset --hard "${DEPLOY_COMMIT:-origin/$DEPLOY_BRANCH}"`.

Duplicate deliveries (same `X-GitHub-Delivery`, e.g. "Redeliver" in GitHub) are
ignored; use `nimdeploy run` to deploy again on purpose.

## Command line

Talks to the running service on its `listen` address, with the API token read
from `secrets.env` (hence `sudo`).

```bash
sudo nimdeploy status                  # table of all deploys
sudo nimdeploy status -json agency
sudo nimdeploy run agency              # deploy the branch head
sudo nimdeploy run -commit 9f1c2e7 agency
sudo nimdeploy run -f agency           # follow the log; exit code 1 if it fails
```

```text
DEPLOY    STATUS               STARTED              DURATION  COMMIT   BY   LOG
agency    running (+1 queued)  2026-09-29 15:57:53  -         1111111  dev  20260929-155753-d11111.log
frontend  success              2026-09-29 13:10:02  1m12s     c93a11f  ana  20260929-131002-c93a11.log
```

Flags go before the deploy name.

## Notifications

```toml
[notify]
format = "slack"            # slack | discord | telegram | json
on = "failure"              # failure | always | never
url_env = "NOTIFY_WEBHOOK_URL"
log_lines = 20
```

`on = "failure"` sends every failure and the first success after one
("recovered"), so a broken deploy is never silent and a fixed one tells you.
Failure messages include the last `log_lines` lines of the log. Telegram uses
`telegram_token_env` and `telegram_chat_id` instead of `url_env`; `json` POSTs
`{"event":"deploy.finished","state":{...},"log_tail":[...]}` to any URL.

## GitHub commit statuses

```toml
[github]
token_env = "GITHUB_TOKEN"
```

Marks each pushed commit as pending → success/failure under the context
`nimdeploy/<deploy>`, visible next to the commit and in pull requests. Use a
fine-grained token limited to the repositories with **Commit statuses: Read and
write**. Coalesced pushes that never ran get no status.

Tip: start deploy scripts with `set -euxo pipefail` so each step appears in the
log as `+ npm ci` and the script stops at the first failure.

## pm2

pm2 runs one daemon per user (`$HOME/.pm2`), so `pm2 restart` only sees the
apps of the user that runs it. To make it work from nimdeploy:

1. **Run nimdeploy as the user that owns the pm2 apps**, e.g. `www`:
   `sudo SERVICE_USER=www ./install.sh`. Check with `sudo -u www pm2 ls`.
2. **Start pm2 with its own systemd unit** (`pm2 startup systemd -u www --hp /home/www`,
   then `pm2 save`). Otherwise the first `pm2` call from a deploy would spawn the
   daemon inside nimdeploy's cgroup, and stopping nimdeploy would kill your apps.
3. **Set PATH** if node/pm2 come from nvm or anywhere outside `/usr/bin` and
   `/usr/local/bin`: systemd services get a minimal PATH. Add it in the deploy's
   `env`, e.g. `env = ["PATH=/home/www/.nvm/versions/node/v22.11.0/bin:/usr/bin:/bin"]`.

Prefer `pm2 reload <app> --update-env`: zero downtime in cluster mode, same as
restart in fork mode. See [`deploy/examples/deploy-pm2.sh`](deploy/examples/deploy-pm2.sh).

## Log format

```text
2026-09-29T12:54:33+02:00 deploy=agency status=started
2026-09-29T12:54:33+02:00 trigger=webhook
2026-09-29T12:54:33+02:00 repository=acme/agency
2026-09-29T12:54:33+02:00 branch=main
2026-09-29T12:54:33+02:00 commit=9f1c2e...
2026-09-29T12:54:33+02:00 pusher=dev
2026-09-29T12:54:33+02:00 delivery=51af02d4-...
2026-09-29T12:54:33+02:00 command=/usr/local/bin/deploy-agency.sh
2026-09-29T12:54:33+02:00 working_directory=/var/www/agency
2026-09-29T12:54:33+02:00 timeout=20m0s

...command output...

2026-09-29T12:56:01+02:00 status=success
2026-09-29T12:56:01+02:00 duration=1m28s
2026-09-29T12:56:01+02:00 exit_code=0
```

A failure ends with `status=failed`, the exit code and `error="exit status 1"`
(or `error="timeout after 20m0s"`).

```bash
tail -f /var/log/nimdeploy/agency/latest.log
journalctl -u nimdeploy -f        # one line per webhook and per finished deploy
```

## Endpoints

| | |
|---|---|
| `POST <path>` | webhook. `202` started/queued · `200` ping, ignored or duplicate · `401` bad signature · `409` running and `queue = false` |
| `POST /deploy/{name}` | manual deploy, optional body `{"commit":"...","user":"..."}`. Token required |
| `GET /status` | state of every deploy. Token required if `api_token_env` is set |
| `GET /status/{name}` | state of one deploy. Same |
| `GET /healthz` | liveness, always open |

With `api_token_env` set, send `Authorization: Bearer <token>`.

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
  "trigger": "webhook",
  "exit_code": 0,
  "log": "20260929-125433-51af02.log",
  "queued": {"commit": "a41f...", "since": "2026-09-29T12:55:10+02:00"}
}
```

`status` is one of `never`, `running`, `success`, `failed`, or `interrupted`
(the service stopped mid-deploy). `queued` appears only while a push waits.
A queued push is lost if the service stops before it runs; it is logged.

Keep `listen` on localhost and have the reverse proxy forward only the hook
paths:

```nginx
location /hooks/ {
    proxy_pass http://127.0.0.1:9000;
}
```

## GitHub setup

Repository → Settings → Webhooks → Add webhook: payload URL
`https://your-host/hooks/agency`, content type `application/json`, the same
secret as `AGENCY_WEBHOOK_SECRET`, event "Just the push event".

## Development

```bash
make test    # go test -race
make lint    # gofmt, go vet, shellcheck
make build   # static binary, version from git describe
```

Pushing a `v*` tag builds the release archives (binary + installer + examples).

## License

[MIT](LICENSE)
