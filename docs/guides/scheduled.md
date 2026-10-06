# Scheduled tasks

A deploy can also run on a schedule, with everything a webhook deploy gets:
its own log per run, history, lock and queue, notifications, metrics. It
replaces cron for the jobs that belong to your apps, which helps on systems
without cron (Amazon Linux 2023 ships without it) and when you don't have root.

```toml
# A job that only runs on its schedule: no path, no webhook, no secret.
[deploy.reconcile-orders]
schedule = "*/15 * * * *"
command = "/bin/bash"
args = ["-c", "nimdeploy woocommerce replay orders -status processing -after $(date -d '-2 days' +%F)"]
timeout = "10m"

# A webhook deploy that also redeploys the branch head every night.
[deploy.site]
path = "/hooks/site"
repository = "acme/site"
secret_env = "SITE_WEBHOOK_SECRET"
schedule = "@daily"
command = "/home/deploy/bin/deploy-site.sh"
```

## Syntax

Five fields, in the server's local time (set `TZ` in the service if needed):

```text
┌───────── minute        0-59
│ ┌─────── hour          0-23
│ │ ┌───── day of month  1-31
│ │ │ ┌─── month         1-12 or jan-dec
│ │ │ │ ┌─ day of week   0-7 (0 and 7 are Sunday) or sun-sat
* * * * *
```

| Example | Runs |
|---|---|
| `*/15 * * * *` | every 15 minutes |
| `30 3 * * *` | every day at 03:30 |
| `0 9 * * mon-fri` | weekdays at 09:00 |
| `0 0 1 * *` | the 1st of every month |
| `5,35 * * * *` | at :05 and :35 |
| `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly` | the usual aliases |
| `@every 10m` | every 10 minutes from when the service started (at least `1s`) |

As in cron, when both day fields are restricted (`0 0 13 * fri`) either one
matching is enough.

## What a scheduled run gets

`DEPLOY_TRIGGER=schedule`, `DEPLOY_PROVIDER=schedule` for jobs without a
webhook, and an empty `DEPLOY_COMMIT` (scripts fall back to the branch head,
as with a manual run).

- If the previous run is still going, the new one waits (`queue`) instead of
  overlapping; a slow job never piles up more than one waiting run.
- `nimdeploy status` shows the next run; `nimdeploy history` shows each run as
  `schedule`; `nimdeploy run -f <job>` runs it now.
- Reloading the config (`systemctl reload nimdeploy`) applies schedule changes.
- A run missed while nimdeploy was stopped is not made up for: the schedule
  continues from the next time.

## Examples

```toml
# Laravel's scheduler, as an alternative to "schedule:work" under pm2 or cron
[deploy.laravel-schedule]
schedule = "* * * * *"
working_directory = "/var/www/backend/agency"
command = "/usr/bin/php"
args = ["artisan", "schedule:run"]
log_output = false             # keep only start/end lines: it runs every minute
timeout = "5m"

# Nightly database dump
[deploy.backup]
schedule = "15 2 * * *"
command = "/home/deploy/bin/backup-db.sh"
timeout = "1h"
```

!!! tip "Running every minute"
    With `retain = 30` per deploy, a job running every minute keeps half an
    hour of logs. Use `log_output = false` for chatty jobs, and the
    [metrics](metrics.md) (`nimdeploy_deploy_last_success`) to alert when one
    starts failing.
