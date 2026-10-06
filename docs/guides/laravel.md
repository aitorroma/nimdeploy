# Laravel

Three ready-made scripts, depending on how PHP is served:

| Script | PHP served by | Pick it when |
|---|---|---|
| [`deploy-laravel-fpm.sh`](#php-fpm) | the system's PHP-FPM behind nginx | the usual production setup; recommended |
| [`deploy-laravel-frankenphp.sh`](#frankenphp) | FrankenPHP on a local port, under pm2 | no PHP-FPM available and no root to set it up |
| [`deploy-laravel.sh`](#docker-or-host) | the host or a Docker Compose service (Sail) | the app runs in containers |

All of them deploy **the exact pushed commit**, keep untracked files (`.env`,
`storage/`), stop with a clear message if `.env` is missing, and run
`composer install --no-dev`, `migrate --force`, `storage:link` and the
artisan caches.

!!! warning "Migrations are not rolled back"
    If a migration fails, the deploy stops there and the database stays as the
    failed migration left it. Review long or locking migrations (new `NOT NULL`
    columns, indexes on big tables) before merging, and keep database
    snapshots.

## PHP-FPM

[`deploy/examples/deploy-laravel-fpm.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/deploy-laravel-fpm.sh):
nginx passes PHP to a PHP-FPM pool running as its own user (e.g. `laravel`),
while deploys run as the service account (e.g. `deploy`). Both share
`storage/` and `bootstrap/cache/` through a group.

What a deploy does:

1. `git fetch` + `git reset --hard <pushed commit>` in `APP_DIR` (first run:
   clones next to any existing files, so a `.env` put there beforehand is kept).
2. Stops if `.env` doesn't exist: the code is in place, create `.env` and run
   `nimdeploy run -f <app>`.
3. Shared permissions: `storage/` and `bootstrap/cache/` get the pool's group,
   group write and setgid; `.env` becomes `640` with the pool's group (PHP-FPM
   reads it, never writes it). The script runs with `umask 0002`.
4. `composer install --no-dev --optimize-autoloader`, `artisan migrate --force`,
   `storage:link` if missing, `artisan optimize` (config, routes, events, views).
5. Optional queue worker and scheduler under pm2, reloaded with the new code
   (`queue:restart` for the worker).
6. Optional health check against a public URL.

PHP-FPM is **not restarted**: with `opcache.validate_timestamps=1` (the
default) it serves the new files within `opcache.revalidate_freq` seconds
(2 s by default).

```bash title="/home/deploy/bin/deploy-agency-backend.sh"
#!/usr/bin/env bash
export PATH="$HOME/.local/bin:$PATH"
export APP_NAME=agency-backend
export APP_DIR=/var/www/backend/agency
export REPO_URL=git@github-agency-backend:acme/agency-backend.git
export WEB_GROUP=laravel                     # PHP-FPM pool group; deploy must be a member
export SCHEDULER=1                           # artisan schedule:work under pm2
export HEALTH_URL=https://example.com/agency/up
exec /home/deploy/bin/deploy-laravel-fpm.sh
```

| Variable | Default | |
|---|---|---|
| `APP_NAME` | required | prefix of the pm2 names (`-queue`, `-scheduler`) |
| `APP_DIR` | required | the checkout; `.env` lives here |
| `REPO_URL` | required | |
| `WEB_GROUP` | `laravel` | the pool's group. Empty: leave groups alone |
| `PHP` | `php` | PHP CLI for composer and artisan |
| `QUEUE` | `0` | `1`: `artisan queue:work` under pm2 |
| `SCHEDULER` | `0` | `1`: `artisan schedule:work` under pm2 |
| `HEALTH_URL` | – | checked at the end; any answer below 500 counts as alive |

### Server setup (admins, once)

```ini title="/etc/php-fpm.d/laravel.conf"
[laravel]
user = laravel
group = laravel
listen = /run/php-fpm/laravel.sock
listen.owner = nginx
listen.group = nginx
listen.mode = 0660
pm = dynamic
pm.max_children = 10
pm.start_servers = 2
pm.min_spare_servers = 1
pm.max_spare_servers = 4
pm.max_requests = 500
request_terminate_timeout = 120s
php_admin_value[error_log] = /var/log/php-fpm/laravel-error.log
security.limit_extensions = .php
```

```ini title="/etc/systemd/system/php-fpm.service.d/umask.conf"
[Service]
UMask=0002
```

```bash
useradd --system --no-create-home --shell /sbin/nologin laravel
usermod -aG laravel deploy                     # then: systemctl restart nimdeploy
systemctl daemon-reload && systemctl restart php-fpm
```

`UMask=0002` makes the files PHP-FPM creates (logs, cache, sessions,
compiled views) group-writable, so the next deploy can update or delete them.

### nginx

A site where Laravel answers the whole domain:

```nginx
root /var/www/backend/shop/public;
index index.php;
location / { try_files $uri $uri/ /index.php?$query_string; }
location ~ \.php$ {
    include fastcgi_params;
    fastcgi_pass unix:/run/php-fpm/laravel.sock;
    fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;
}
```

Laravel **under a path prefix** next to other apps (e.g. `/agency/api` and
`/agency/admin`, while `/agency/` is a Nuxt frontend): send those paths to
`index.php` and tell PHP the app lives in `/agency`. Laravel keeps its own
routes (`/api`, `/admin`) and generates links, redirects and assets with the
prefix, without code changes:

```nginx title="/etc/nginx/agency-laravel.fastcgi (outside conf.d)"
include fastcgi_params;
fastcgi_pass unix:/run/php-fpm/laravel.sock;
fastcgi_param SCRIPT_FILENAME /var/www/backend/agency/public/index.php;
fastcgi_param SCRIPT_NAME     /agency/index.php;
fastcgi_param DOCUMENT_ROOT   /var/www/backend/agency/public;
fastcgi_param HTTPS on;
```

```nginx
location = /agency/api     { include /etc/nginx/agency-laravel.fastcgi; }
location ^~ /agency/api/   { include /etc/nginx/agency-laravel.fastcgi; }
location = /agency/admin   { include /etc/nginx/agency-laravel.fastcgi; }
location ^~ /agency/admin/ { include /etc/nginx/agency-laravel.fastcgi; }
location ^~ /agency/storage/ { alias /var/www/backend/agency/public/storage/; }
location ^~ /agency/css/     { alias /var/www/backend/agency/public/css/; }
```

## FrankenPHP

[`deploy/examples/deploy-laravel-frankenphp.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/deploy-laravel-frankenphp.sh)
serves the app on a local port (e.g. `127.0.0.1:8000`) with
[FrankenPHP](https://frankenphp.dev) in **classic mode** (one fresh PHP
request per HTTP request, like PHP-FPM), managed by pm2. No root needed:
FrankenPHP is downloaded to `~/.local/bin` on the first deploy. Same steps as
above, then `pm2 startOrReload` (which also clears OPcache) and a health check
on `HEALTH_PATH` (default `/up`). Variables: `APP_NAME`, `APP_DIR`,
`REPO_URL`, `PORT`, `HOST`, `PHP`, `FRANKENPHP`, `QUEUE`, `SCHEDULER`,
`HEALTH_PATH`. nginx then proxies the Laravel paths to that port.

!!! note "Classic mode on purpose"
    FrankenPHP's *worker mode* keeps the app in memory between requests. It is
    faster, but state can leak between requests (singletons, static
    properties, connections) and not every package supports it. Validate your
    app thoroughly before enabling it.

## Docker or host

[`deploy/examples/deploy-laravel.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/deploy-laravel.sh)
runs in the deploy's `working_directory` (the checkout): fast-forward to the
pushed commit, `composer install`, `npm ci && npm run build`, `migrate`,
`storage:link`, the artisan caches, `queue:restart` (and `horizon:terminate`
if Horizon is installed). PHP/Node commands run on the host or inside a
docker compose service (Laravel Sail). Settings go in the deploy's `env`:

```toml
[deploy.shop]
path = "/hooks/shop"
repository = "acme/shop"
branch = "main"
secret_env = "SHOP_WEBHOOK_SECRET"
working_directory = "/srv/shop"            # the git checkout, with docker-compose.yml
command = "/usr/local/bin/deploy-laravel.sh"
timeout = "20m"
env = [
  "COMPOSE_SERVICE=laravel.test",          # empty/absent: run on the host
  "RESTART_SERVICES=laravel.worker",       # workers that ignore queue:restart
  "OWNER=www-data:www-data",               # chown storage/ bootstrap/cache/
  "EXTRA_MIGRATIONS=tenants:migrate",      # e.g. stancl/tenancy
  "WWWUSER=1000", "WWWGROUP=1000",         # used by Sail's docker-compose.yml
]
```

Other options: `PHP_FPM_SERVICE=php8.3-fpm` (host only, reloads PHP-FPM to
clear OPcache; needs a sudoers rule such as
`deploy ALL=(root) NOPASSWD: /usr/bin/systemctl reload php8.3-fpm`),
`MAINTENANCE=1` (`artisan down` around the migrations, `up` even if they
fail), `ARTISAN_CACHE="config:cache route:cache view:cache event:cache"`
(the default leaves out `route:cache` and `event:cache`, which fail with
closure routes), `COMPOSER_NO_DEV=0`.

!!! danger "Docker means root"
    To run `docker compose` the service user must be in the `docker` group,
    which is equivalent to root. Only do this on servers where that is
    acceptable.

## Scheduler: pm2 or cron

Laravel's scheduler can run as a cron entry or as a long-running process.
Use **one**, never both, or every task runs twice:

=== "pm2 (SCHEDULER=1)"

    `artisan schedule:work` under pm2, reloaded by each deploy, restarted if
    it dies, started at boot by pm2's unit. Needs nothing from root, which is
    why the scripts offer it (Amazon Linux 2023, for example, ships without
    cron).

=== "cron"

    Laravel's recommended way: no resident PHP process, every run starts
    with the current code. In the **service account's** crontab, never
    root's:

    ```cron
    * * * * * cd /var/www/backend/agency && php artisan schedule:run >> /dev/null 2>&1
    ```

## Private packages (Backpack PRO, Nova, Spark…)

Store the credentials in the service account's Composer config, never in the
repository:

```bash
sudo -iu deploy
composer config -g http-basic.repo.backpackforlaravel.com <user> <password>
chmod 600 ~/.config/composer/auth.json
```

Some vendors use more than one host (Backpack: `repo.backpackforlaravel.com`
and `backpackforlaravel.com`); add an entry for each one the lock file uses.

## PHP version mismatch

If `composer.lock` is generated on a newer PHP than the server's (8.4 locally,
8.3 on the server), pin the platform in `composer.json` so every machine
resolves dependencies for production's PHP:

```json
"config": {
    "platform": { "php": "8.3.0" }
}
```
