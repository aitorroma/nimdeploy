# nimdeploy

[![CI](https://github.com/aitorroma/nimdeploy/actions/workflows/ci.yml/badge.svg)](https://github.com/aitorroma/nimdeploy/actions/workflows/ci.yml)
[![Docs](https://github.com/aitorroma/nimdeploy/actions/workflows/docs.yml/badge.svg)](https://nimdeploy.nimbox360.com)

**Push to your git host, and your server deploys itself.**

nimdeploy receives `push` webhooks from GitHub, Gitea, Forgejo, GitLab or
Bitbucket, checks the signature, the repository and the branch, and runs the
deploy command you configured for that repository. One static Go binary and a
systemd unit; no root needed.

📖 **Documentation: <https://nimdeploy.nimbox360.com>**

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh -s -- \
  --repo acme/shop --dir /srv/shop --command ./deploy.sh
```

That installs it for your user (`~/.local/bin`, systemd user service), adds a
deploy for `acme/shop`, generates its webhook secret and prints the webhook
URL and the nginx block to publish it. The installer verifies the download
against the release checksums; [other ways to install](https://nimdeploy.nimbox360.com/install/).

## Features

- **Signed webhooks**: HMAC SHA-256 (GitLab: secret token), one secret per
  repository, repository and branch filters, duplicate deliveries ignored.
- **One deploy at a time** per app; pushes during a deploy are queued and only
  the latest runs. Exact pushed commit, timeout that kills the whole process group.
- **A log file per deploy**, `latest.log`, `nimdeploy history`, automatic retention.
- **No root**: user service, or a system unit run by a service account that a
  named operator controls with a minimal sudoers rule
  ([`contrib/setup-root.sh`](contrib/setup-root.sh)).
- **Any webhook, with parameters**: Ansible, CI jobs or scripts can trigger a
  deploy with a signed JSON body; declared values (`SERVICE=api`) are validated
  and passed to the command. `nimdeploy send` signs and posts for you.
- **Deploy from a WooCommerce sale**: `nimdeploy woocommerce add` registers the
  shop's webhooks; each paid order runs your provisioning script (queued on
  disk, never dropped), which can write back to the order. Missed orders can be
  replayed.
- **Wait for CI** (GitHub Actions) and ✅/❌ **commit statuses**.
- **Notifications** to Slack, Discord, Telegram or any URL, failures and recoveries.
- **Behind any proxy**: nginx, HestiaCP, Caddy, Traefik, Cloudflare (proxied or Tunnel), unix socket.
- `nimdeploy run | status | history | nginx` from the server's shell;
  `systemctl reload` applies config changes without interrupting deploys.

```text
$ nimdeploy status
DEPLOY           STATUS               STARTED              DURATION  COMMIT   BY    LOG
agency-backend   success              2026-10-06 10:12:03  48s       a41f09c  ana   20261006-101203-7f3a21.log
agency-frontend  running (+1 queued)  2026-10-06 10:15:40  -         c93a11f  marc  20261006-101540-c93a11.log
```

## Ready-made deploy scripts

In [`deploy/examples/`](deploy/examples), shipped with every release:

| Script | For |
|---|---|
| [`deploy-nuxt.sh`](deploy/examples/deploy-nuxt.sh) | Nuxt SSR under pm2: atomic releases, health check, automatic rollback |
| [`deploy-laravel-fpm.sh`](deploy/examples/deploy-laravel-fpm.sh) | Laravel on the system's PHP-FPM, shared permissions, optional queue/scheduler |
| [`deploy-laravel-frankenphp.sh`](deploy/examples/deploy-laravel-frankenphp.sh) | Laravel on a local port with FrankenPHP under pm2 |
| [`deploy-laravel.sh`](deploy/examples/deploy-laravel.sh) | Laravel on the host or in Docker Compose (Sail) |
| [`deploy-pm2.sh`](deploy/examples/deploy-pm2.sh) | Any Node app managed by pm2 |
| [`run-ansible.sh`](deploy/examples/run-ansible.sh) | An Ansible playbook with the webhook's params as JSON extra vars |
| [`provision-woocommerce.sh`](deploy/examples/provision-woocommerce.sh) | Provision a paid WooCommerce order, idempotent, and report back to the order |

## Documentation

| | |
|---|---|
| [Quick start](https://nimdeploy.nimbox360.com/getting-started/) | from nothing to "a push deploys my app" |
| [How it works](https://nimdeploy.nimbox360.com/how-it-works/) | checks, queue, what the script receives |
| [Install](https://nimdeploy.nimbox360.com/install/) | without root, service account + operator, as root |
| [Guides](https://nimdeploy.nimbox360.com/guides/nuxt/) | Nuxt, Laravel, pm2, wait for CI, notifications, git providers, generic webhooks and Ansible, WooCommerce |
| [Reverse proxy](https://nimdeploy.nimbox360.com/proxy/) | nginx, HestiaCP, Caddy, Traefik, Cloudflare |
| [Configuration](https://nimdeploy.nimbox360.com/reference/configuration/) | every option |
| [Security model](https://nimdeploy.nimbox360.com/reference/security/) | what is protected and how |

The source of the site is in [`docs/`](docs).

## Development

```bash
make test     # go test -race
make lint     # gofmt, go vet, shellcheck
make build    # static binary, version from git describe
```

Pushing a `v*` tag builds and publishes the release. See
[Development](https://nimdeploy.nimbox360.com/development/).

## License

[MIT](LICENSE) · a [Nimbox360](https://nimbox360.com) project
