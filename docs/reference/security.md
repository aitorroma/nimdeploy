# Security model

A deploy receiver is, by design, an HTTP endpoint that makes the server run
code. This page describes what nimdeploy protects, how, and what stays your
responsibility.

## The webhook endpoint

| Threat | Protection |
|---|---|
| Someone forges a push | Every request must carry a valid **HMAC SHA-256** signature (GitLab: the secret token) made with that deploy's secret, compared in constant time. Otherwise `401` and nothing runs. |
| A leaked secret affects every app | **One secret per deploy**, each in its own environment variable. |
| A push from another repository or branch | `repository` and `branch` must match the config; anything else is ignored. |
| Replaying a captured delivery | Duplicate delivery IDs are ignored; deploys use the commit from the signed payload, so a replay can at most redeploy that same commit. |
| Command injection through the payload | The payload is **never** turned into a command or arguments. Only the configured `command` runs; push data reaches it as plain environment variables (`DEPLOY_*`). |
| Malicious values in params | Only **declared** params reach the command, each checked against its `enum`, `match` or a conservative default pattern, with a length limit; control characters, objects and arrays are refused. Anything invalid: `400`, nothing runs. Names like `PATH`, `LD_*`, `BASH_ENV` or `NODE_OPTIONS` can't be params. |
| Replay of a generic webhook | Optional signed timestamp (`timestamp_header`, `max_skew`) plus delivery IDs. |
| Secrets in welcome emails | Values listed in `secrets` are turned into Password Pusher links (with a retrieval step against link scanners) before the email is rendered; they never reach the email, the log or the outbox. Recipients are masked in logs (`a***@example.com`). |
| Personal data in payloads (e.g. WooCommerce orders) | Never written to logs or notifications; only to `DEPLOY_PAYLOAD_FILE` and `queue.json`, both `600`. Shop API keys are used by the CLI only and removed from the script's environment. |
| Huge or slow requests | Body capped at 25 MB (`max_body_bytes`); deploys run in the background so requests return immediately. |
| Flooding with valid pushes | One run at a time per deploy; queued pushes collapse into the latest one. |

## Network exposure

- nimdeploy listens on **`127.0.0.1:9000`** (or a unix socket) and is reached
  only through your reverse proxy, which terminates TLS.
- Publish only `/hooks/`. The API (`/status`, `/history`, `/deploy`) needs
  a **Bearer token** (`api_token_env`) and normally stays on localhost.
  nimdeploy warns at startup if it listens on a non-local address without one.
- The client IP in the logs comes from `X-Forwarded-For` only when the
  request came from a `trusted_proxies` address, read right to left.

## The deploy process

- Runs as the **unprivileged user** nimdeploy runs as, never as root (unless
  you install it that way on purpose).
- Every secret named in the config is **removed from the script's
  environment**: a compromised build step cannot read the webhook secrets or
  the API token from its environment.
- Runs in its own **process group**: on timeout the whole group gets `SIGTERM`
  and then `SIGKILL`, so no orphan build keeps running.
- Started directly with `exec`, no shell, unless you configure one.

## Privilege separation on shared servers

With [setup-root.sh](../install/service-account.md):

```text
root (admins)         installs once; owns nginx, PHP-FPM, certificates
  │
  ├── operator (named) SSH key, own account; sudo only to:
  │                      · act as the service account
  │                      · start/stop/restart/reload nimdeploy and pm2-deploy
  │                    reads logs through the systemd-journal group
  │
  └── deploy (service) owns code, releases, config, logs
                       runs nimdeploy, deploy scripts, pm2 (User=deploy units)
                       no password, no SSH login of its own
```

- Every action is traceable: sudo logs the operator's name; the journal logs
  each webhook and deploy; each deploy has its own log file.
- No `sudo systemctl status`, `journalctl` or `systemctl edit`: they open a
  pager or editor as root, from which a root shell is one keystroke away.
- **No root service ever executes a file owned by the service account.** This
  is what makes "acting as `deploy`" not equivalent to root. Keep it that way
  when you add cron jobs, timers or units.

### File permissions

| Path | Owner | Mode |
|---|---|---|
| `config.toml` | service account | `640` |
| `secrets.env` | service account | `600` |
| `~/.ssh/` deploy keys | service account | `600`, read-only keys, one per repository |
| `~/.config/composer/auth.json` | service account | `600` |
| app directories | service account, team group | `2775` (setgid) |
| Laravel `.env` | service account, PHP-FPM group | `640` |
| `storage/`, `bootstrap/cache/` | shared through the PHP-FPM group | group-writable, PHP-FPM with `UMask=0002` |

## Params in your scripts

Validated params are still input from outside. In scripts, always quote them
(`"$SERVICE"`), never `eval` them or build shell strings with them, and pass
them to tools as single arguments or as data (e.g. Ansible JSON extra vars,
as [`run-ansible.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/run-ansible.sh)
does). Prefer `enum` over `match` when the set of values is known.

## What stays your responsibility

!!! warning "Your deploy script runs your repository's code"
    `npm install`, `composer install` and build steps execute code from your
    dependencies with the service account's permissions. Use lockfiles
    (`--frozen-lockfile`, `npm ci`), approve install scripts explicitly
    (pnpm's `allowBuilds`), and review changes to them like any other change
    to the server.

- **Who can push** to the deploy branch can deploy. Protect that branch
  (reviews, required checks) and consider [wait for CI](../guides/wait-for-ci.md).
- **Secrets in git:** `.env` files, `secrets.env` and `auth.json` never belong
  in a repository or in documentation. Rotate any secret that was ever shared
  in a chat or a document.
- **Separate environments:** production gets its own webhook secrets, deploy
  keys, `APP_KEY` and database credentials, never reused from staging.
- **Migrations** are not rolled back automatically; keep database snapshots.

## Reporting a vulnerability

Please open a [private security advisory](https://github.com/aitorroma/nimdeploy/security/advisories/new)
on GitHub instead of a public issue.
