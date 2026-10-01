#!/usr/bin/env bash
# Install nimdeploy as a systemd service.
#
#   sudo ./install.sh                 # install or upgrade (system service)
#   sudo ./install.sh uninstall       # remove binary and unit, keep config and logs
#   sudo ./install.sh uninstall --purge   # also remove /etc/nimdeploy and the logs
#
# Without root (automatic when not run as root, or with --user): everything
# goes under $HOME and runs as a systemd user service.
#   ./install.sh                      # ~/.local/bin, ~/.config/nimdeploy, ~/.local/state/nimdeploy
#   ./install.sh uninstall [--purge]
#
# HestiaCP: publish the webhook paths on one of its web domains (root only)
#   sudo ./install.sh hestia deploy.example.com [hestia-user] [--api]
#   sudo ./install.sh hestia-remove deploy.example.com [hestia-user]
#
# Environment:
#   SERVICE_USER=deploy   user the deploys run as (created if missing; system mode)
#   DESTDIR=/some/root    stage files under a root dir (skips useradd/systemctl)
#   HESTIA=/usr/local/hestia  HestiaCP installation
set -euo pipefail

DESTDIR="${DESTDIR:-}"
SRC="$(cd "$(dirname "$0")" && pwd)"

# --user anywhere in the arguments selects user mode; so does running the
# install/uninstall commands without root.
USER_MODE=0
args=()
for arg in "$@"; do
	if [[ "$arg" == --user ]]; then USER_MODE=1; else args+=("$arg"); fi
done
set -- "${args[@]+"${args[@]}"}"
case "${1:-install}" in
install | uninstall) [[ $EUID -ne 0 && -z "$DESTDIR" ]] && USER_MODE=1 ;;
esac

if ((USER_MODE)); then
	CONF_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
	STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
	SERVICE_USER="$(id -un)"
	BIN="$DESTDIR$HOME/.local/bin/nimdeploy"
	ETC="$DESTDIR$CONF_HOME/nimdeploy"
	UNIT="$DESTDIR$CONF_HOME/systemd/user/nimdeploy.service"
	LOGS="$DESTDIR$STATE_HOME/nimdeploy"
	LOGS_REAL="$STATE_HOME/nimdeploy"
	UNIT_SRC="$SRC/deploy/nimdeploy-user.service"
	SUDO=""
	JOURNAL="journalctl --user -u nimdeploy"
else
	SERVICE_USER="${SERVICE_USER:-deploy}"
	BIN="$DESTDIR/usr/local/bin/nimdeploy"
	ETC="$DESTDIR/etc/nimdeploy"
	UNIT="$DESTDIR/etc/systemd/system/nimdeploy.service"
	LOGS="$DESTDIR/var/log/nimdeploy"
	LOGS_REAL="/var/log/nimdeploy"
	UNIT_SRC="$SRC/deploy/nimdeploy.service"
	SUDO="sudo "
	JOURNAL="journalctl -u nimdeploy"
fi

HESTIA="${HESTIA:-/usr/local/hestia}"
HESTIA_HOME="$DESTDIR${HESTIA_HOME:-/home}"
SNIPPETS=(nginx.conf_nimdeploy nginx.ssl.conf_nimdeploy)

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

live() { [[ -z "$DESTDIR" ]]; }

# sc runs systemctl for the system or the user manager.
sc() {
	if ((USER_MODE)); then systemctl --user "$@"; else systemctl "$@"; fi
}

systemctl_() { if live; then sc "$@"; fi; }

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

# ensure_linger keeps the user manager (and nimdeploy) running without an
# open session and starts it on boot.
ensure_linger() {
	if [[ "$(loginctl show-user "$SERVICE_USER" -p Linger --value 2>/dev/null)" == yes ]]; then
		return
	fi
	if loginctl enable-linger "$SERVICE_USER" 2>/dev/null; then
		log "enabled lingering: nimdeploy keeps running after logout and starts on boot"
		return
	fi
	warn "lingering is off and this user may not enable it: nimdeploy stops when your last"
	warn "session ends and does not start on boot. Ask an administrator to run once:"
	warn "    sudo loginctl enable-linger $SERVICE_USER"
}

do_install() {
	((USER_MODE)) || require_root install
	build_binary
	((USER_MODE)) || ensure_user
	if ((USER_MODE)); then
		log "user mode: installing for $SERVICE_USER under $HOME (no root needed)"
		if live && ! systemctl --user show-environment >/dev/null 2>&1; then
			die "no systemd user manager (systemctl --user); log in through SSH or a real session and retry"
		fi
	fi

	local was_active=0
	if live && sc is-active --quiet nimdeploy 2>/dev/null; then
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
		sed "s|^directory = \"/var/log/nimdeploy\"|directory = \"$LOGS_REAL\"|" "$SRC/config.example.toml" >"$ETC/config.toml"
		chmod 0640 "$ETC/config.toml"
	fi
	if [[ -e "$ETC/secrets.env" ]]; then
		log "keeping existing $ETC/secrets.env"
	else
		log "installing secrets template to $ETC/secrets.env"
		install -m 0600 "$SRC/deploy/secrets.env.example" "$ETC/secrets.env"
	fi
	ensure_api_token
	if ((USER_MODE)); then
		install -d -m 0750 "$LOGS"
	elif live; then
		chown root:"$SERVICE_USER" "$ETC" "$ETC/config.toml"
		chown root:root "$ETC/secrets.env"
	fi

	log "installing unit to $UNIT"
	install -d "$(dirname "$UNIT")"
	if ((USER_MODE)); then
		cp "$UNIT_SRC" "$UNIT"
	else
		sed -e "s/^User=.*/User=$SERVICE_USER/" -e "s/^Group=.*/Group=$SERVICE_USER/" "$UNIT_SRC" >"$UNIT"
	fi
	chmod 0644 "$UNIT"

	if ((!USER_MODE)) && [[ -x "$HESTIA/bin/v-list-web-domain" ]]; then
		log "HestiaCP detected: publish the hooks on a domain with: sudo $0 hestia <domain>"
	fi

	live || { log "staged under $DESTDIR"; return; }

	sc daemon-reload
	((USER_MODE)) && ensure_linger

	if grep -q 'change-me' "$ETC/secrets.env"; then
		# Not enabled either: an enabled unit would start on the next boot (or,
		# for a user service without lingering, on the next login).
		warn "service installed but NOT enabled: $ETC/secrets.env still has placeholder secrets"
		cat <<EOF

Next steps:
  1. Edit $ETC/config.toml   (deploys, repositories, commands)
  2. Edit $ETC/secrets.env   (one secret per secret_env, e.g. openssl rand -hex 32)
  3. Start:  ${SUDO}systemctl $( ((USER_MODE)) && echo "--user ")enable --now nimdeploy   (validates the config first)
  4. Try:    ${SUDO}nimdeploy status
             ${SUDO}nimdeploy run -f <deploy>
  5. Logs:   $JOURNAL -f      (each deploy: $LOGS_REAL/<deploy>/latest.log)
  6. Proxy:  ${SUDO}nimdeploy nginx   prints the nginx locations for the hooks
EOF
		return
	fi

	sc enable nimdeploy >/dev/null 2>&1
	if ((was_active)); then
		log "restarting nimdeploy"
		sc restart nimdeploy
	else
		log "starting nimdeploy"
		sc start nimdeploy
	fi
	sleep 1
	if sc is-active --quiet nimdeploy; then
		log "nimdeploy is running"
	else
		sc status nimdeploy --no-pager || true
		die "nimdeploy failed to start, see: $JOURNAL -e"
	fi
}

# --- HestiaCP ----------------------------------------------------------------
# HestiaCP's nginx templates include /home/USER/conf/web/DOMAIN/nginx.conf_*
# (http) and nginx.ssl.conf_* (https) inside the domain's server block, and
# keeps those files when it rebuilds the domain. The hook locations go there,
# so no template has to be changed.

hestia_owner() {
	local domain=$1 user=$2
	if [[ -z "$user" ]]; then
		user="$("$HESTIA/bin/v-search-domain-owner" "$domain" web 2>/dev/null || true)"
	fi
	[[ -n "$user" ]] || die "web domain $domain not found in HestiaCP; add it first: v-add-web-domain <user> $domain"
	"$HESTIA/bin/v-list-web-domain" "$user" "$domain" >/dev/null 2>&1 ||
		die "web domain $domain does not belong to HestiaCP user $user"
	printf '%s' "$user"
}

nginx_reload() {
	live || return 0
	nginx -t 2>&1 || return 1
	systemctl reload nginx
}

do_hestia() {
	require_root hestia
	local domain="" user="" api=0 arg
	for arg in "$@"; do
		case "$arg" in
		--api) api=1 ;;
		-*) die "unknown option $arg" ;;
		*) if [[ -z "$domain" ]]; then domain=$arg; else user=$arg; fi ;;
		esac
	done
	[[ -n "$domain" ]] || die "usage: $0 hestia <domain> [hestia-user] [--api]"
	[[ -x "$HESTIA/bin/v-list-web-domain" ]] || die "HestiaCP not found in $HESTIA"
	[[ -x "$BIN" ]] || die "install nimdeploy first: sudo $0"
	if live; then
		command -v nginx >/dev/null || die "nginx not found: HestiaCP must run nginx (alone or as proxy in front of Apache)"
	fi
	user="$(hestia_owner "$domain" "$user")"

	local dir="$HESTIA_HOME/$user/conf/web/$domain"
	[[ -d "$dir" ]] || die "$dir not found"

	local flags=() snippet
	((api)) && flags+=(-api)
	snippet="$("$BIN" -config "$ETC/config.toml" nginx "${flags[@]}")" ||
		die "cannot generate the nginx config from $ETC/config.toml"

	if ! grep -qs 'conf_\*' "$dir/nginx.conf" "$dir/nginx.ssl.conf"; then
		warn "$domain's nginx config does not include nginx.conf_* files (custom template?); the hooks may not be reachable"
	fi
	if ! "$HESTIA/bin/v-list-web-domain" "$user" "$domain" json 2>/dev/null | grep -q '"SSL": *"yes"'; then
		warn "$domain has no SSL: enable it (v-add-letsencrypt-domain $user $domain) and use https in GitHub"
	fi

	local name backup
	backup="$(mktemp -d)"
	for name in "${SNIPPETS[@]}"; do
		[[ -e "$dir/$name" ]] && cp -p "$dir/$name" "$backup/"
		printf '%s\n' "$snippet" >"$dir/$name"
		chmod 0644 "$dir/$name"
	done
	if ! nginx_reload; then
		for name in "${SNIPPETS[@]}"; do
			if [[ -e "$backup/$name" ]]; then cp -p "$backup/$name" "$dir/$name"; else rm -f "$dir/$name"; fi
		done
		rm -rf "$backup"
		die "nginx -t failed; changes reverted"
	fi
	rm -rf "$backup"
	log "published on $domain ($dir/${SNIPPETS[0]}, ${SNIPPETS[1]})"

	local paths path
	paths="$(sed -n 's/^location = \([^ ]*\) {$/\1/p' <<<"$snippet" | grep -v '/status$' || true)"
	echo
	echo "GitHub payload URLs:"
	for path in $paths; do
		echo "  https://$domain$path"
	done

	if live && [[ -n "$paths" ]]; then
		path="$(head -1 <<<"$paths")"
		local code
		code="$(curl -sk -o /dev/null -w '%{http_code}' -X POST --resolve "$domain:443:127.0.0.1" "https://$domain$path" || true)"
		case "$code" in
		401) log "check: POST https://$domain$path reaches nimdeploy (401 without signature, as expected)" ;;
		502) warn "check: nginx answers 502; is nimdeploy running? systemctl status nimdeploy" ;;
		*) warn "check: POST https://$domain$path answered $code instead of 401; see nginx and nimdeploy logs" ;;
		esac
	fi

	local service_user
	service_user="$(sed -n 's/^User=//p' "$UNIT" 2>/dev/null || true)"
	if [[ -n "$service_user" && "$service_user" != "$user" ]]; then
		echo
		warn "nimdeploy runs as $service_user, but $domain belongs to $user."
		warn "To deploy $user's sites (owned by $user): sudo SERVICE_USER=$user $0"
	fi
	echo
	echo "Sites of $user live in $HESTIA_HOME/$user/web/<domain>/: use that path as working_directory."
}

do_hestia_remove() {
	require_root hestia-remove
	local domain=${1:-} user=${2:-}
	[[ -n "$domain" ]] || die "usage: $0 hestia-remove <domain> [hestia-user]"
	[[ -x "$HESTIA/bin/v-list-web-domain" ]] || die "HestiaCP not found in $HESTIA"
	user="$(hestia_owner "$domain" "$user")"
	local dir="$HESTIA_HOME/$user/conf/web/$domain" name
	for name in "${SNIPPETS[@]}"; do
		rm -f "$dir/$name"
	done
	nginx_reload || die "nginx -t failed after removing the nimdeploy config; check nginx"
	log "removed nimdeploy from $domain"
}

# remove_hestia_snippets drops the nginx config from every HestiaCP domain.
remove_hestia_snippets() {
	local found=0 f
	for f in "$HESTIA_HOME"/*/conf/web/*/nginx.conf_nimdeploy "$HESTIA_HOME"/*/conf/web/*/nginx.ssl.conf_nimdeploy; do
		[[ -e "$f" ]] || continue
		rm -f "$f"
		found=1
		log "removed $f"
	done
	if ((found)); then
		nginx_reload || warn "nginx -t failed after removing the hooks; check nginx"
	fi
}

do_uninstall() {
	((USER_MODE)) || require_root uninstall
	local purge=0
	[[ "${1:-}" == "--purge" ]] && purge=1

	if live && sc list-unit-files nimdeploy.service >/dev/null 2>&1; then
		log "stopping and disabling nimdeploy"
		sc disable --now nimdeploy 2>/dev/null || true
	fi
	rm -f "$UNIT" "$BIN"
	systemctl_ daemon-reload
	((USER_MODE)) || remove_hestia_snippets

	if ((purge)); then
		log "removing $ETC and $LOGS"
		rm -rf "$ETC" "$LOGS"
	else
		log "kept $ETC and $LOGS (use --purge to remove them)"
	fi
	if ((USER_MODE)); then
		log "nimdeploy uninstalled (lingering, if enabled, was left as is: loginctl disable-linger)"
	else
		log "nimdeploy uninstalled (user $SERVICE_USER was not removed)"
	fi
}

case "${1:-install}" in
install) do_install ;;
uninstall) shift; do_uninstall "$@" ;;
hestia) shift; do_hestia "$@" ;;
hestia-remove) shift; do_hestia_remove "$@" ;;
*) die "usage: $0 [--user] [install | uninstall [--purge] | hestia <domain> [user] [--api] | hestia-remove <domain> [user]]" ;;
esac
