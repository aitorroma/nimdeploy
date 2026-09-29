#!/usr/bin/env bash
# Example deploy for a Laravel app. nimdeploy runs it in working_directory
# (the git checkout) and passes DEPLOY_* variables.
#
# PHP/Node commands run on the host, or inside a docker compose service
# (Laravel Sail and similar). Settings, from the deploy's `env` in config.toml:
#
#   COMPOSE_SERVICE=laravel.test      run commands in this compose service
#                                     (empty: run them on the host)
#   RESTART_SERVICES="worker mail"    compose services to restart at the end,
#                                     e.g. consumers that ignore queue:restart
#   PHP_FPM_SERVICE=php8.3-fpm        host only: reload it to clear OPcache
#                                     (needs a sudoers rule, see README)
#   OWNER=www-data:www-data           chown storage/ and bootstrap/cache/
#   COMPOSER_NO_DEV=1                 composer install --no-dev (default 1)
#   ARTISAN_CACHE="config:cache view:cache"
#                                     add route:cache/event:cache if your
#                                     routes have no closures
#   EXTRA_MIGRATIONS="tenants:migrate"  more artisan migrate-like commands
#   MAINTENANCE=1                     artisan down during migrations
set -Eeuo pipefail
LAST=""
trap 'echo "ERROR: deploy failed running: ${LAST:-$BASH_COMMAND}" >&2' ERR

COMPOSE_SERVICE="${COMPOSE_SERVICE:-}"
RESTART_SERVICES="${RESTART_SERVICES:-}"
PHP_FPM_SERVICE="${PHP_FPM_SERVICE:-}"
OWNER="${OWNER:-}"
COMPOSER_NO_DEV="${COMPOSER_NO_DEV:-1}"
ARTISAN_CACHE="${ARTISAN_CACHE:-config:cache view:cache}"
EXTRA_MIGRATIONS="${EXTRA_MIGRATIONS:-}"
MAINTENANCE="${MAINTENANCE:-0}"
BRANCH="${DEPLOY_BRANCH:-main}"

if [[ -n "$COMPOSE_SERVICE" ]]; then
	RUN=(docker compose exec -T "$COMPOSE_SERVICE")
else
	RUN=()
fi
# Each command is printed to the log as "$ command" before it runs.
host() {
	LAST="$*"
	echo "\$ $*"
	"$@"
}
run() {
	LAST="$*"
	echo "\$ $*"
	"${RUN[@]}" "$@"
}
artisan() { run php artisan "$@"; }

# --- Code -------------------------------------------------------------------
# Fast-forward only: stop if the checkout has local changes or diverged.
# Deploys the pushed commit, or the branch head for a manual run.
host git fetch origin "$BRANCH"
host git merge --ff-only "${DEPLOY_COMMIT:-origin/$BRANCH}"

if [[ -n "$COMPOSE_SERVICE" ]]; then
	# The checkout is owned by a host user; without this git (used by
	# composer) refuses to work in the container ("dubious ownership").
	run sh -c 'git config --global --get-all safe.directory | grep -qx "$PWD" || git config --global --add safe.directory "$PWD"'
fi

# --- Dependencies and assets ------------------------------------------------
composer_flags=(--no-interaction --prefer-dist --optimize-autoloader)
[[ "$COMPOSER_NO_DEV" == 1 ]] && composer_flags+=(--no-dev)
run composer install "${composer_flags[@]}"

if [[ -f package.json ]]; then
	run npm ci --no-audit --no-fund
	run npm run build
fi

# --- Database -----------------------------------------------------------------
if [[ "$MAINTENANCE" == 1 ]]; then
	artisan down --retry=15
	# Bring the site back even if a migration fails.
	trap 'artisan up' EXIT
fi

artisan migrate --force
for cmd in $EXTRA_MIGRATIONS; do
	artisan "$cmd" --force
done

# --- Files and caches -------------------------------------------------------
artisan storage:link --force
if [[ -n "$OWNER" ]]; then
	run chown -R "$OWNER" storage bootstrap/cache
fi

# shellcheck disable=SC2086 # word splitting intended
for cmd in $ARTISAN_CACHE; do
	artisan "$cmd"
done

if [[ "$MAINTENANCE" == 1 ]]; then
	artisan up
	trap - EXIT
fi

# --- Background processes ---------------------------------------------------
# Workers keep the old code in memory until told to restart.
artisan queue:restart
if "${RUN[@]}" php artisan list --raw | grep -q '^horizon:terminate'; then
	artisan horizon:terminate
fi

if [[ -n "$PHP_FPM_SERVICE" && -z "$COMPOSE_SERVICE" ]]; then
	host sudo -n systemctl reload "$PHP_FPM_SERVICE"
fi

for service in $RESTART_SERVICES; do
	host docker compose restart "$service"
done

echo "Deployed ${DEPLOY_COMMIT:-origin/$BRANCH}"
