---
hide:
  - navigation
  - toc
---

<div class="hero" markdown>

# nimdeploy

<p class="tagline">Push to your git host, and your server deploys itself.
One static binary, signed webhooks, a log per deploy, no root needed.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[How it works](how-it-works.md){ .md-button }

</div>

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh
```

nimdeploy receives `push` webhooks from **GitHub, Gitea, Forgejo, GitLab or
Bitbucket**, checks the signature, the repository and the branch, and runs the
deploy command you configured for that repository. It also accepts
[any signed JSON webhook](guides/generic.md), from Ansible, CI jobs or
scripts, passing validated values to the command. It is the small piece
between "git push" and "the new version is live" for servers that are not
Kubernetes: a VPS, an EC2 instance, a HestiaCP box.

<div class="grid cards" markdown>

-   :material-shield-lock-outline:{ .lg } __Safe by default__

    ---

    HMAC signature checked on every request, one secret per repository,
    listens on localhost behind your proxy, runs as an unprivileged user.
    The payload is never turned into a command.

    [:octicons-arrow-right-24: Security model](reference/security.md)

-   :material-account-lock-outline:{ .lg } __No root needed__

    ---

    Installs into your home with a systemd user service, or as a system unit
    run by a service account that a named operator controls with a tiny
    sudoers rule.

    [:octicons-arrow-right-24: Install options](install/index.md)

-   :material-file-document-multiple-outline:{ .lg } __A log per deploy__

    ---

    Every run gets its own log file, `latest.log` points at the newest,
    `nimdeploy history` lists them, old ones are pruned automatically.

    [:octicons-arrow-right-24: Logs and history](reference/logs.md)

-   :material-format-list-numbered:{ .lg } __Queue, not chaos__

    ---

    One deploy at a time per app; pushes that arrive meanwhile are queued
    and only the latest one runs. Exact commit, 30 min timeout, process group
    killed on timeout.

    [:octicons-arrow-right-24: How it works](how-it-works.md)

-   :material-check-decagram-outline:{ .lg } __Waits for CI__

    ---

    Optionally deploy only when your GitHub Actions workflows pass for the
    pushed commit, and mark each commit with ✅ / ❌.

    [:octicons-arrow-right-24: Wait for CI](guides/wait-for-ci.md)

-   :material-code-json:{ .lg } __Any webhook, with parameters__

    ---

    Beyond git: let Ansible, CI jobs or scripts trigger deploys with a signed
    JSON body, and pass validated values (`SERVICE=api`) to your command.

    [:octicons-arrow-right-24: Generic webhooks](guides/generic.md)

-   :material-cart-outline:{ .lg } __Deploy from a sale__

    ---

    WooCommerce, Stripe, Paddle or Lemon Squeezy payment → your provisioning
    script runs → the order gets a note. Nothing lost on restarts.

    [:octicons-arrow-right-24: WooCommerce](guides/woocommerce.md)

-   :material-backup-restore:{ .lg } __Safe deploys and rollback__

    ---

    Hooks before and after, health checks, Cloudflare purges, scheduled jobs,
    and `nimdeploy rollback` (or automatic rollback when a deploy fails).

    [:octicons-arrow-right-24: Hooks and rollback](guides/hooks-rollback.md)

-   :material-email-fast-outline:{ .lg } __Welcome emails__

    ---

    What your script or playbook created, emailed to the customer with a
    Handlebars template; passwords go as expiring Password Pusher links.

    [:octicons-arrow-right-24: Emails and secret links](guides/email.md)

-   :material-chart-line:{ .lg } __Prometheus metrics__

    ---

    Deploys, durations, queues, failures and webhook answers on `/metrics`,
    with ready-made alert rules.

    [:octicons-arrow-right-24: Metrics](guides/metrics.md)

-   :material-bell-ring-outline:{ .lg } __Tells you when it breaks__

    ---

    Failure and recovery messages to Slack, Discord, Telegram or any URL,
    with the last lines of the log.

    [:octicons-arrow-right-24: Notifications](guides/notifications.md)

</div>

## Ready-made deploy scripts

nimdeploy runs any command. The release ships tested scripts for common stacks:

| Script | For |
|---|---|
| [`deploy-nuxt.sh`](guides/nuxt.md) | Nuxt SSR under pm2: atomic releases, health check, automatic rollback |
| [`deploy-laravel-fpm.sh`](guides/laravel.md#php-fpm) | Laravel served by the system's PHP-FPM, shared permissions, optional queue/scheduler |
| [`deploy-laravel-frankenphp.sh`](guides/laravel.md#frankenphp) | Laravel on a local port with FrankenPHP under pm2 |
| [`deploy-laravel.sh`](guides/laravel.md#docker-or-host) | Laravel on the host or in Docker Compose (Sail) |
| [`deploy-pm2.sh`](guides/pm2.md) | Any Node app managed by pm2 |
| [`run-ansible.sh`](guides/generic.md) | An Ansible playbook with the webhook's params as extra vars |
| [`provision-woocommerce.sh`](guides/woocommerce.md) | Provision a paid WooCommerce order, idempotent, and report back to the order |

## At a glance

```text
$ nimdeploy status
DEPLOY           STATUS               STARTED              DURATION  COMMIT   BY    LOG
agency-backend   success              2026-10-06 10:12:03  48s       a41f09c  ana   20261006-101203-7f3a21.log
agency-frontend  running (+1 queued)  2026-10-06 10:15:40  -         c93a11f  leo   20261006-101540-c93a11.log
```

nimdeploy is open source (MIT) and maintained by [Nimbox360](https://nimbox360.com).
