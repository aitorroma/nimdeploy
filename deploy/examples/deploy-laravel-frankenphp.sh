#!/usr/bin/env bash
# Deploy a Laravel app served on a local port by FrankenPHP (classic mode,
# like PHP-FPM: no changes to the project), managed by pm2. Optional queue
# worker and scheduler, also under pm2. No root needed.
# nimdeploy runs it and passes DEPLOY_*; the rest comes from the environment
# (usually a small wrapper script that exports these and execs this one):
#
#   APP_NAME=agency-backend            pm2 process name (worker/scheduler get -queue/-scheduler)
#   APP_DIR=/var/www/backend/agency    the checkout; .env lives here (not in git)
#   REPO_URL=git@github.com:org/repo.git
#   PORT=8000                          HOST=127.0.0.1 (default)
#   PHP=php                            PHP CLI for composer and artisan (default php)
#   FRANKENPHP=~/.local/bin/frankenphp downloaded on the first deploy if missing
#   QUEUE=1                            run "artisan queue:work" under pm2 (default 0)
#   SCHEDULER=1                        run "artisan schedule:work" under pm2 (default 0)
#   HEALTH_PATH=/up                    any answer below 500 counts as alive (default /up)
#
# Like the post-receive hook it replaces: code at the pushed commit, stop if
# there's no .env, composer --no-dev, migrations, storage link, optimize.
set -Eeuo pipefail
LAST=""
trap 'echo "ERROR: deploy failed running: ${LAST:-$BASH_COMMAND}" >&2' ERR
step() {
	LAST="$*"
	echo "\$ $*"
	"$@"
}

: "${APP_NAME:?set APP_NAME}" "${APP_DIR:?set APP_DIR}" "${REPO_URL:?set REPO_URL}" "${PORT:?set PORT}"
HOST=${HOST:-127.0.0.1}
PHP=${PHP:-php}
FRANKENPHP=${FRANKENPHP:-$HOME/.local/bin/frankenphp}
QUEUE=${QUEUE:-0}
SCHEDULER=${SCHEDULER:-0}
HEALTH_PATH=${HEALTH_PATH:-/up}
BRANCH=${DEPLOY_BRANCH:-main}
ECOSYSTEM="$HOME/.config/pm2/$APP_NAME.config.cjs"
command -v pm2 >/dev/null || { echo "ERROR: pm2 not found in PATH" >&2; exit 1; }
artisan() { step "$PHP" artisan "$@"; }

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

# --- 2. dependencies, database, caches ---------------------------------------------------
step "$PHP" "$(command -v composer)" install --no-dev --no-interaction --prefer-dist --optimize-autoloader
artisan migrate --force
if [[ ! -e public/storage ]]; then
	artisan storage:link
fi
artisan optimize

# --- 3. PHP server and workers under pm2 ----------------------------------------------------
if [[ ! -x "$FRANKENPHP" ]]; then
	case "$(uname -m)" in
	x86_64) arch=x86_64 ;;
	aarch64) arch=aarch64 ;;
	*) echo "ERROR: no FrankenPHP build for $(uname -m)" >&2; exit 1 ;;
	esac
	mkdir -p "$(dirname "$FRANKENPHP")"
	step curl -fsSLo "$FRANKENPHP.tmp" "https://github.com/php/frankenphp/releases/latest/download/frankenphp-linux-$arch"
	chmod +x "$FRANKENPHP.tmp"
	mv "$FRANKENPHP.tmp" "$FRANKENPHP"
fi
echo "frankenphp: $("$FRANKENPHP" version 2>/dev/null | head -1)"

mkdir -p "$(dirname "$ECOSYSTEM")"
{
	echo "// Written by deploy-laravel-frankenphp.sh on each deploy; edit the wrapper script instead."
	echo "module.exports = { apps: ["
	cat <<EOF
  {
    name: "$APP_NAME",
    cwd: "$APP_DIR",
    script: "$FRANKENPHP",
    args: ["php-server", "--listen", "$HOST:$PORT", "--root", "$APP_DIR/public"],
    interpreter: "none",
    exec_mode: "fork",
    max_memory_restart: "300M",
    time: true,
    env: { XDG_DATA_HOME: "$HOME/.local/share", XDG_CONFIG_HOME: "$HOME/.config" },
  },
EOF
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
# Restarting the server also clears OPcache, so the new code is served.
step pm2 startOrReload "$ECOSYSTEM" --update-env
if [[ "$QUEUE" == 1 ]]; then
	artisan queue:restart
fi

# --- 4. health check ------------------------------------------------------------------------
code=""
for _ in $(seq 1 15); do
	code=$(curl -s -o /dev/null -w '%{http_code}' "http://$HOST:$PORT$HEALTH_PATH" || true)
	[[ "$code" =~ ^[234] ]] && break
	sleep 2
done
if [[ ! "$code" =~ ^[234] ]]; then
	echo "ERROR: GET $HEALTH_PATH -> ${code:-no answer}; check: pm2 logs $APP_NAME, storage/logs/laravel.log" >&2
	exit 1
fi
echo "health: GET $HEALTH_PATH -> $code"
step pm2 save --force >/dev/null
echo "deployed $(git rev-parse HEAD)"
