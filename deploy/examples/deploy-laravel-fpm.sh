#!/usr/bin/env bash
# Deploy a Laravel app served by the system's PHP-FPM (nginx -> PHP-FPM pool).
# The deploy runs as the service account (e.g. deploy), PHP-FPM as the pool's
# user (e.g. laravel); both share storage/ and bootstrap/cache through a group.
# Optional queue worker and scheduler under pm2. No root needed.
# nimdeploy runs it and passes DEPLOY_*; the rest comes from the environment
# (usually a small wrapper script that exports these and execs this one):
#
#   APP_NAME=agency-backend            pm2 names for the worker/scheduler (-queue/-scheduler)
#   APP_DIR=/var/www/backend/agency    the checkout; .env lives here (not in git)
#   REPO_URL=git@github.com:org/repo.git
#   WEB_GROUP=laravel                  group of the PHP-FPM pool; the deploy user must be
#                                      a member. storage/, bootstrap/cache/ and .env get it
#                                      (empty: leave groups alone)
#   PHP=php                            PHP CLI for composer and artisan (default php)
#   QUEUE=1                            run "artisan queue:work" under pm2 (default 0)
#   SCHEDULER=1                        run "artisan schedule:work" under pm2 (default 0)
#   HEALTH_URL=https://example.com/up  checked after the deploy; any answer below 500
#                                      counts as alive (default: no check)
#
# PHP-FPM is not restarted: with opcache.validate_timestamps=1 (the default) it
# picks up the new files within opcache.revalidate_freq seconds. The pool should
# run with UMask=0002 so the files it creates in storage/ stay group-writable.
set -Eeuo pipefail
LAST=""
trap 'echo "ERROR: deploy failed running: ${LAST:-$BASH_COMMAND}" >&2' ERR
step() {
	LAST="$*"
	echo "\$ $*"
	"$@"
}

: "${APP_NAME:?set APP_NAME}" "${APP_DIR:?set APP_DIR}" "${REPO_URL:?set REPO_URL}"
WEB_GROUP=${WEB_GROUP-laravel}
PHP=${PHP:-php}
QUEUE=${QUEUE:-0}
SCHEDULER=${SCHEDULER:-0}
HEALTH_URL=${HEALTH_URL:-}
BRANCH=${DEPLOY_BRANCH:-main}
ECOSYSTEM="$HOME/.config/pm2/$APP_NAME.config.cjs"
artisan() { step "$PHP" artisan "$@"; }
# Files this deploy creates (caches, compiled views) must stay writable by PHP-FPM.
umask 0002

if [[ -n "$WEB_GROUP" ]] && ! id -nG | tr ' ' '\n' | grep -qx "$WEB_GROUP"; then
	echo "ERROR: $(id -un) is not in group $WEB_GROUP; an administrator runs once:" >&2
	echo "       usermod -aG $WEB_GROUP $(id -un)   (then restart nimdeploy)" >&2
	exit 1
fi

# --- 1. code at the pushed commit --------------------------------------------------------
mkdir -p "$APP_DIR"
if [[ ! -d "$APP_DIR/.git" ]]; then
	# The directory may hold files already (e.g. .env): clone next to it and move .git in.
	tmp=$(mktemp -d)
	step git clone --quiet --no-checkout "$REPO_URL" "$tmp/repo"
	mv "$tmp/repo/.git" "$APP_DIR/.git"
	rm -rf "$tmp"
fi
cd "$APP_DIR"
step git fetch --quiet --prune origin "+refs/heads/$BRANCH:refs/remotes/origin/$BRANCH"
# Untracked files (.env, storage/*) are kept; tracked files match the commit.
step git reset --quiet --hard "${DEPLOY_COMMIT:-origin/$BRANCH}"
echo "code at $(git log --oneline -1)"

if [[ ! -f .env ]]; then
	echo "ERROR: $APP_DIR/.env does not exist. The code is in place; create it" >&2
	echo "       (cp .env.example .env, set APP_KEY/DB_*/APP_URL...) and deploy again:" >&2
	echo "       nimdeploy run -f $APP_NAME" >&2
	exit 1
fi

# --- 2. shared permissions with PHP-FPM --------------------------------------------------
if [[ -n "$WEB_GROUP" ]]; then
	mkdir -p storage/app/public storage/framework/{cache,sessions,views} storage/logs bootstrap/cache
	# Only what this user owns can be changed; PHP-FPM's own files already have the group.
	me=$(id -un)
	find storage bootstrap/cache -user "$me" ! -group "$WEB_GROUP" -exec chgrp "$WEB_GROUP" {} +
	find storage bootstrap/cache -user "$me" -type d ! -perm -2070 -exec chmod g+rwxs {} +
	find storage bootstrap/cache -user "$me" -type f ! -perm -0060 -exec chmod g+rw {} +
	# PHP-FPM reads .env (readable by the group, never writable).
	if [[ -O .env ]]; then
		chgrp "$WEB_GROUP" .env
		chmod 640 .env
	fi
fi

# --- 3. dependencies, database, caches ---------------------------------------------------
step "$PHP" "$(command -v composer)" install --no-dev --no-interaction --prefer-dist --optimize-autoloader
artisan migrate --force
if [[ ! -e public/storage ]]; then
	artisan storage:link
fi
artisan optimize

# --- 4. queue worker and scheduler under pm2 (optional) ------------------------------------
if [[ "$QUEUE" == 1 || "$SCHEDULER" == 1 ]]; then
	command -v pm2 >/dev/null || { echo "ERROR: pm2 not found in PATH" >&2; exit 1; }
	mkdir -p "$(dirname "$ECOSYSTEM")"
	{
		echo "// Written by deploy-laravel-fpm.sh on each deploy; edit the wrapper script instead."
		echo "module.exports = { apps: ["
		if [[ "$QUEUE" == 1 ]]; then
			cat <<EOF
  { name: "$APP_NAME-queue", cwd: "$APP_DIR", script: "artisan", interpreter: "$PHP",
    args: ["queue:work", "--sleep=3", "--tries=3", "--max-time=3600"], exec_mode: "fork", time: true },
EOF
		fi
		if [[ "$SCHEDULER" == 1 ]]; then
			cat <<EOF
  { name: "$APP_NAME-scheduler", cwd: "$APP_DIR", script: "artisan", interpreter: "$PHP",
    args: ["schedule:work"], exec_mode: "fork", time: true },
EOF
		fi
		echo "] };"
	} >"$ECOSYSTEM"
	# Reloading restarts the PHP processes, so they run the new code.
	step pm2 startOrReload "$ECOSYSTEM" --update-env
	if [[ "$QUEUE" == 1 ]]; then
		artisan queue:restart
	fi
	step pm2 save --force >/dev/null
fi

# --- 5. health check (optional) ------------------------------------------------------------
if [[ -n "$HEALTH_URL" ]]; then
	code=""
	for _ in $(seq 1 15); do
		code=$(curl -s -o /dev/null -w '%{http_code}' "$HEALTH_URL" || true)
		[[ "$code" =~ ^[234] ]] && break
		sleep 2
	done
	if [[ ! "$code" =~ ^[234] ]]; then
		echo "ERROR: GET $HEALTH_URL -> ${code:-no answer}; check storage/logs/laravel.log and the PHP-FPM error log" >&2
		exit 1
	fi
	echo "health: GET $HEALTH_URL -> $code"
fi
echo "deployed $(git rev-parse HEAD)"
