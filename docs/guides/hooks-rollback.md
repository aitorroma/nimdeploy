# Hooks, health checks and rollback

Around the deploy command, nimdeploy can run your own steps, check that the
app answers, purge Cloudflare's cache, and go back to the previous version,
by hand or automatically.

```toml
[deploy.shop]
path = "/hooks/shop"
repository = "acme/shop"
secret_env = "SHOP_WEBHOOK_SECRET"
working_directory = "/var/www/shop"
command = "/home/deploy/bin/deploy-shop.sh"

before        = "php artisan down --retry=15"
after_success = "php artisan up"
after_failure = "php artisan up; curl -fsS -X POST https://hooks.example.com/oncall"
health_url    = "https://shop.example.com/up"
health_timeout = "60s"
rollback_on_failure = true
cloudflare_zone_id = "023e105f4ecef8ad9ca31a8372d0c353"
cloudflare_purge = ["everything"]
```

## Order of a deploy

```text
before ──fails──▶ after_failure ─▶ failed (the command never runs)
  │
command ──fails──▶ after_failure ─▶ failed ─▶ rollback (if enabled)
  │
health_url ──no 2xx/3xx in health_timeout──▶ after_failure ─▶ failed ─▶ rollback
  │
cloudflare purge ─▶ after_success ─▶ success
```

- Hooks run with `bash -eo pipefail -c`, in the deploy's directory, with the
  same environment (`DEPLOY_*`, params) and timeout, and their output goes to
  the same log, under `hook=before`, `hook=after_success`…
- A failing `before` fails the deploy. Failures of `after_success` and of the
  Cloudflare purge are logged but don't change the result: the deploy itself
  worked.

## Health check

`health_url` is requested every 2 s after the command succeeds, until it
answers 2xx/3xx or `health_timeout` (default 60 s) runs out, which fails the
deploy. Point it at something that really exercises the app: Laravel's
`/up`, a Nuxt page, an API endpoint that touches the database.

## Rollback

```bash
nimdeploy rollback -n shop      # which commit it would deploy
nimdeploy rollback -f shop      # do it and follow the log
nimdeploy rollback -to 9f1c2e7 shop
```

It deploys again the **last successful commit before the latest deploy**, found
in the history, through the same command and hooks; it shows up as
`rollback` in `nimdeploy history` and in notifications. Your script just has
to deploy `DEPLOY_COMMIT`, which all the example scripts do.

With `rollback_on_failure = true`, a failed deploy (command, `before` or health
check) triggers the rollback by itself. A failed rollback doesn't trigger
another one.

!!! warning "Code goes back, data doesn't"
    A rollback redeploys the previous code; it doesn't undo database
    migrations or files the new version wrote. Keep migrations backwards
    compatible (add columns before using them, drop them a release later) and
    take snapshots before risky ones. For the Nuxt script, a release switch
    is already instant; this rollback is for everything else.

Rollback needs a git provider (it redeploys commits) and only finds commits
whose logs are still kept (`logging.retain`, 30 by default).

## Cloudflare cache purge

```toml
[cloudflare]
api_token_env = "CLOUDFLARE_API_TOKEN"   # token with Zone → Cache Purge → Purge

[deploy.shop]
cloudflare_zone_id = "023e105f4ecef8ad9ca31a8372d0c353"   # Overview page of the zone
cloudflare_purge = ["everything"]
# or specific URLs (any number; sent 30 at a time):
# cloudflare_purge = ["https://shop.example.com/", "https://shop.example.com/catalog"]
```

Runs after a successful deploy, before `after_success`. The token lives in
`secrets.env`; create it in Cloudflare → My Profile → API Tokens with only the
*Cache Purge* permission for that zone.

## Maintenance mode

With Laravel:

```toml
before        = "php artisan down --retry=15 --render=errors::503"
after_success = "php artisan up"
after_failure = "php artisan up"
```

With nginx and a flag file instead (any app):

```toml
before        = "touch /var/www/shop/maintenance.on"
after_success = "rm -f /var/www/shop/maintenance.on"
after_failure = "rm -f /var/www/shop/maintenance.on"
```

```nginx
if (-f /var/www/shop/maintenance.on) { return 503; }
```

For apps deployed with atomic releases (the Nuxt script) maintenance mode is
unnecessary: the site keeps serving the old release until the switch.
