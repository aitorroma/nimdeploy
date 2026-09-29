#!/usr/bin/env bash
# Example deploy for a Node app managed by pm2.
# nimdeploy runs it in working_directory and passes DEPLOY_* variables.
#
# Must run as the same user that owns the pm2 processes (see README, "pm2").
set -euxo pipefail

APP="${PM2_APP:-$DEPLOY_NAME}"   # pm2 process name, defaults to the deploy name

git fetch origin "$DEPLOY_BRANCH"
# Manual runs (`nimdeploy run`) may have no commit: use the branch head.
git reset --hard "${DEPLOY_COMMIT:-origin/$DEPLOY_BRANCH}"

npm ci
npm run build

# reload = zero downtime in cluster mode; falls back to restart in fork mode.
pm2 reload "$APP" --update-env
pm2 save
