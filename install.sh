#!/usr/bin/env bash
# Install nimdeploy as a systemd service.
#
#   sudo ./install.sh                 # install or upgrade
#   sudo ./install.sh uninstall       # remove binary and unit, keep config and logs
#   sudo ./install.sh uninstall --purge   # also remove /etc/nimdeploy and the logs
#
# Environment:
#   SERVICE_USER=deploy   user the deploys run as (created if missing)
#   DESTDIR=/some/root    stage files under a root dir (skips useradd/systemctl)
set -euo pipefail

SERVICE_USER="${SERVICE_USER:-deploy}"
DESTDIR="${DESTDIR:-}"

BIN="$DESTDIR/usr/local/bin/nimdeploy"
ETC="$DESTDIR/etc/nimdeploy"
UNIT="$DESTDIR/etc/systemd/system/nimdeploy.service"
LOGS="$DESTDIR/var/log/nimdeploy"

SRC="$(cd "$(dirname "$0")" && pwd)"

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

live() { [[ -z "$DESTDIR" ]]; }

systemctl_() { if live; then systemctl "$@"; fi; }

require_root() {
	if live && [[ $EUID -ne 0 ]]; then
		die "run as root: sudo $0 $*"
	fi
}

build_binary() {
	if [[ -x "$SRC/nimdeploy" ]]; then
		return
	fi
	command -v go >/dev/null || die "no ./nimdeploy binary and Go is not installed; build it first with: make build"
	log "building nimdeploy"
	(cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o nimdeploy .)
}

ensure_user() {
	live || return 0
	if id -u "$SERVICE_USER" >/dev/null 2>&1; then
		log "using existing user $SERVICE_USER"
	else
		log "creating system user $SERVICE_USER"
		useradd --system --create-home --home-dir "/var/lib/$SERVICE_USER" --shell /usr/sbin/nologin "$SERVICE_USER"
	fi
}

ensure_api_token() {
	local file="$ETC/secrets.env" token
	if grep -qE '^NIMDEPLOY_API_TOKEN=.+' "$file"; then
		return
	fi
	token="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
	if grep -q '^NIMDEPLOY_API_TOKEN=' "$file"; then
		sed -i "s/^NIMDEPLOY_API_TOKEN=.*/NIMDEPLOY_API_TOKEN=$token/" "$file"
	else
		printf '\n# API token for /status, /deploy and nimdeploy run\nNIMDEPLOY_API_TOKEN=%s\n' "$token" >>"$file"
	fi
	log "generated NIMDEPLOY_API_TOKEN in $file"
}

do_install() {
	require_root install
	build_binary
	ensure_user

	local was_active=0
	if live && systemctl is-active --quiet nimdeploy 2>/dev/null; then
		was_active=1
	fi

	# Look for the daemon without calling pm2, which would spawn one.
	if live && pgrep -f 'PM2 v.*God Daemon' >/dev/null && ! pgrep -u "$SERVICE_USER" -f 'PM2 v.*God Daemon' >/dev/null; then
		warn "pm2 is running, but not as $SERVICE_USER: deploys run as $SERVICE_USER and will not see those apps."
		warn "pm2 runs as: $(pgrep -f 'PM2 v.*God Daemon' | xargs -r ps -o user= -p | sort -u | xargs). Reinstall with SERVICE_USER=<that user>."
	fi

	log "installing binary to $BIN"
	install -D -m 0755 "$SRC/nimdeploy" "$BIN"

	install -d -m 0750 "$ETC"
	if [[ -e "$ETC/config.toml" ]]; then
		log "keeping existing $ETC/config.toml"
	else
		log "installing example config to $ETC/config.toml"
		install -m 0640 "$SRC/config.example.toml" "$ETC/config.toml"
	fi
	if [[ -e "$ETC/secrets.env" ]]; then
		log "keeping existing $ETC/secrets.env"
	else
		log "installing secrets template to $ETC/secrets.env"
		install -m 0600 "$SRC/deploy/secrets.env.example" "$ETC/secrets.env"
	fi
	ensure_api_token
	if live; then
		chown root:"$SERVICE_USER" "$ETC" "$ETC/config.toml"
		chown root:root "$ETC/secrets.env"
	fi

	log "installing unit to $UNIT (User=$SERVICE_USER)"
	install -d "$(dirname "$UNIT")"
	sed -e "s/^User=.*/User=$SERVICE_USER/" -e "s/^Group=.*/Group=$SERVICE_USER/" \
		"$SRC/deploy/nimdeploy.service" >"$UNIT"
	chmod 0644 "$UNIT"

	live || { log "staged under $DESTDIR"; return; }

	systemctl daemon-reload
	systemctl enable nimdeploy >/dev/null

	if grep -q 'change-me' "$ETC/secrets.env"; then
		warn "service enabled but NOT started: $ETC/secrets.env still has placeholder secrets"
		cat <<EOF

Next steps:
  1. Edit $ETC/config.toml   (deploys, repositories, commands)
  2. Edit $ETC/secrets.env   (one secret per secret_env, e.g. openssl rand -hex 32)
  3. Start:  sudo systemctl start nimdeploy   (validates the config first)
  4. Try:    sudo nimdeploy status
             sudo nimdeploy run -f <deploy>
  5. Logs:   journalctl -u nimdeploy -f
EOF
		return
	fi

	if ((was_active)); then
		log "restarting nimdeploy"
		systemctl restart nimdeploy
	else
		log "starting nimdeploy"
		systemctl start nimdeploy
	fi
	sleep 1
	if systemctl is-active --quiet nimdeploy; then
		log "nimdeploy is running"
	else
		systemctl status nimdeploy --no-pager || true
		die "nimdeploy failed to start, see: journalctl -u nimdeploy -e"
	fi
}

do_uninstall() {
	require_root uninstall
	local purge=0
	[[ "${1:-}" == "--purge" ]] && purge=1

	if live && systemctl list-unit-files nimdeploy.service >/dev/null 2>&1; then
		log "stopping and disabling nimdeploy"
		systemctl disable --now nimdeploy 2>/dev/null || true
	fi
	rm -f "$UNIT" "$BIN"
	systemctl_ daemon-reload

	if ((purge)); then
		log "removing $ETC and $LOGS"
		rm -rf "$ETC" "$LOGS"
	else
		log "kept $ETC and $LOGS (use --purge to remove them)"
	fi
	log "nimdeploy uninstalled (user $SERVICE_USER was not removed)"
}

case "${1:-install}" in
install) do_install ;;
uninstall) shift; do_uninstall "$@" ;;
*) die "usage: $0 [install|uninstall [--purge]]" ;;
esac
