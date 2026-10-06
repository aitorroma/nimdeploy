#!/usr/bin/env bash
# Deploy a Nuxt app with server-side rendering: atomic releases, pm2 in
# cluster mode (zero-downtime reload), health check and automatic rollback.
# nimdeploy runs it and passes DEPLOY_*; the rest comes from the environment
# (usually a small wrapper script that exports these and execs this one):
#
#   APP_NAME=agency-frontend           pm2 process name
#   APP_DIR=/var/www/frontend/agency   holds repo/, releases/, shared/, current
#   REPO_URL=git@github.com:org/repo.git
#   PORT=3001                          HOST=127.0.0.1 (default)
#   BASE_URL=/agency/                  Nuxt app.baseURL (NUXT_APP_BASE_URL); also
#                                      the path the health check requests (default /)
#   INSTANCES=1                        pm2 cluster instances (default 1)
#   KEEP_RELEASES=5                    releases kept for rollback (default 5)
#   BUILD_MEMORY_MB=1536               Node heap for the build (optional)
#
# Layout:
#   repo/                git cache, fetched on each deploy
#   releases/<date>-<sha>/  one build per deploy (code + node_modules + .output)
#   shared/.env          app settings (NUXT_PUBLIC_*...), used at build and runtime;
#                        it is read by bash, so quote values with spaces:
#                        NUXT_PUBLIC_APP_NAME="Acme Agency"
#   current -> releases/...  what pm2 runs
#   ecosystem.config.cjs pm2 definition (rewritten on each deploy)
#
# The new release is built next to the running one. Only when the build
# succeeds is "current" switched and pm2 reloaded; if the app then doesn't
# answer, "current" goes back to the previous release.
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
BASE_URL=${BASE_URL:-/}
INSTANCES=${INSTANCES:-1}
KEEP=${KEEP_RELEASES:-5}
BRANCH=${DEPLOY_BRANCH:-main}
REPO="$APP_DIR/repo"
RELEASES="$APP_DIR/releases"
SHARED="$APP_DIR/shared"
CURRENT="$APP_DIR/current"
ECOSYSTEM="$APP_DIR/ecosystem.config.cjs"
command -v pm2 >/dev/null || { echo "ERROR: pm2 not found in PATH" >&2; exit 1; }

mkdir -p "$RELEASES" "$SHARED"
touch "$SHARED/.env"

# --- 1. code ---------------------------------------------------------------------------
if [[ ! -d "$REPO/.git" ]]; then
	step git clone --quiet "$REPO_URL" "$REPO"
fi
step git -C "$REPO" fetch --quiet --prune origin "+refs/heads/$BRANCH:refs/remotes/origin/$BRANCH"
COMMIT=$(git -C "$REPO" rev-parse --verify "${DEPLOY_COMMIT:-origin/$BRANCH}^{commit}")
RELEASE="$RELEASES/$(date +%Y%m%d-%H%M%S)-${COMMIT:0:7}"
echo "release $RELEASE (commit $COMMIT)"
mkdir -p "$RELEASE"
LAST="git archive $COMMIT"
git -C "$REPO" archive "$COMMIT" | tar -x -C "$RELEASE"
ln -sfn "$SHARED/.env" "$RELEASE/.env"

# --- 2. build, next to the running release ----------------------------------------------
cd "$RELEASE"
set -a
# shellcheck disable=SC1091
. "$SHARED/.env"
set +a
export NUXT_APP_BASE_URL="$BASE_URL"
if [[ -n "${BUILD_MEMORY_MB:-}" ]]; then
	export NODE_OPTIONS="--max-old-space-size=$BUILD_MEMORY_MB"
fi
# The build needs devDependencies, which NODE_ENV=production (often in .env)
# would make the package manager skip: install without it, build with it.
build_env() { env NODE_ENV=production "$@"; }
if [[ -f pnpm-lock.yaml ]]; then
	step env -u NODE_ENV pnpm install --frozen-lockfile --prefer-offline
	step build_env pnpm run build
elif [[ -f package-lock.json ]]; then
	step env -u NODE_ENV npm ci --include=dev --no-audit --no-fund
	step build_env npm run build
elif [[ -f yarn.lock ]]; then
	step env -u NODE_ENV yarn install --frozen-lockfile --production=false
	step build_env yarn build
else
	echo "ERROR: no pnpm-lock.yaml, package-lock.json or yarn.lock in the repository" >&2
	exit 1
fi
unset NODE_OPTIONS
if [[ ! -f .output/server/index.mjs ]]; then
	echo "ERROR: the build did not produce .output/server/index.mjs (SSR build with the node-server preset expected)" >&2
	exit 1
fi

# --- 3. switch and reload -------------------------------------------------------------------
PREVIOUS=$(readlink "$CURRENT" 2>/dev/null || true)
cat >"$ECOSYSTEM" <<EOF
// Written by deploy-nuxt.sh on each deploy; edit the wrapper script instead.
module.exports = {
  apps: [{
    name: "$APP_NAME",
    cwd: "$CURRENT",
    script: "$CURRENT/.output/server/index.mjs",
    exec_mode: "cluster",
    instances: $INSTANCES,
    max_memory_restart: "400M",
    time: true,
    env: {
      NODE_ENV: "production",
      HOST: "$HOST", PORT: "$PORT",
      NITRO_HOST: "$HOST", NITRO_PORT: "$PORT",
      NUXT_APP_BASE_URL: "$BASE_URL",
    },
  }],
};
EOF
switch_to() {
	ln -sfn "$1" "$CURRENT.next"
	mv -T "$CURRENT.next" "$CURRENT"
}
step switch_to "$RELEASE"
# --update-env passes shared/.env (already exported) to the processes.
step pm2 startOrReload "$ECOSYSTEM" --update-env

# --- 4. health check, rollback ----------------------------------------------------------------
healthy() {
	local code
	for _ in $(seq 1 30); do
		code=$(curl -s -o /dev/null -w '%{http_code}' "http://$HOST:$PORT$BASE_URL" || true)
		if [[ "$code" =~ ^[23] ]]; then
			echo "health: GET $BASE_URL -> $code"
			return 0
		fi
		sleep 2
	done
	echo "health: GET $BASE_URL -> ${code:-no answer} after 60s" >&2
	return 1
}
if ! healthy; then
	if [[ -n "$PREVIOUS" && -d "$PREVIOUS" ]]; then
		echo "rolling back to $PREVIOUS" >&2
		switch_to "$PREVIOUS"
		pm2 startOrReload "$ECOSYSTEM" --update-env || true
	fi
	echo "ERROR: the new release did not answer; check: pm2 logs $APP_NAME" >&2
	exit 1
fi
step pm2 save --force >/dev/null

# --- 5. keep the last releases ---------------------------------------------------------------
mapfile -t old < <(find "$RELEASES" -mindepth 1 -maxdepth 1 -type d | sort -r | tail -n +$((KEEP + 1)))
for dir in "${old[@]}"; do
	[[ "$dir" == "$(readlink "$CURRENT")" ]] || rm -rf "$dir"
done
echo "deployed $COMMIT ($(basename "$RELEASE"))"
