#!/usr/bin/env bash
# setup-root.sh - one-time root setup for nimdeploy as a system service that
# runs as a service account (deploy), operated by a nominal admin user through
# sudo with the minimum privileges.
#
# What it does (idempotent; existing config and secrets are kept):
#   - installs the nimdeploy binary for the service account (~/.local/bin),
#     downloaded from the GitHub release and checked against checksums.txt
#   - creates its config and secrets (~/.config/nimdeploy, owned by the account)
#   - installs pm2 for the account (~/.local/bin/pm2) unless --no-pm2
#   - installs and enables the system units nimdeploy.service and
#     pm2-deploy.service (User=<service account>)
#   - lets the admin user act as the service account and start/stop/restart/
#     reload those two units, nothing else as root (/etc/sudoers.d)
#   - adds the admin user to systemd-journal (read logs without sudo)
#   - creates missing application directories (--app-dir)
#   - checks nginx for a /hooks/ location (never changes nginx)
#
# Usage:
#   sudo ./setup-root.sh --dry-run          # show what it would do, change nothing
#   sudo ./setup-root.sh                    # do it
#   sudo ./setup-root.sh --uninstall        # remove units and sudoers (keeps the account's files)
#
# Options:
#   --service-user NAME   service account (default: deploy)
#   --admin-user NAME     nominal admin who operates it (default: aitor)
#   --app-dir DIR         application directory owned by the service account;
#                         repeat it (default: /var/www/frontend and /var/www/backend)
#   --version vX.Y.Z      nimdeploy release (default: latest)
#   --binary PATH         use this nimdeploy binary instead of downloading
#   --nopasswd            sudo rules without password (for admins who log in with SSH keys only)
#   --no-pm2              don't install pm2 nor pm2-deploy.service
#   --dry-run             print every action, change nothing
set -euo pipefail

SERVICE_USER=deploy
ADMIN_USER=aitor
APP_DIRS=()
VERSION=latest
BINARY=""
NOPASSWD=0
WITH_PM2=1
DRY=0
ACTION=install
REPO_URL=https://github.com/aitorroma/nimdeploy

while (($#)); do
	case "$1" in
	--service-user) SERVICE_USER=$2; shift ;;
	--admin-user) ADMIN_USER=$2; shift ;;
	--app-dir) APP_DIRS+=("$2"); shift ;;
	--version) VERSION=$2; shift ;;
	--binary) BINARY=$2; shift ;;
	--nopasswd) NOPASSWD=1 ;;
	--no-pm2) WITH_PM2=0 ;;
	--dry-run) DRY=1 ;;
	--uninstall) ACTION=uninstall ;;
	-h | --help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
	*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done
((${#APP_DIRS[@]})) || APP_DIRS=(/var/www/frontend /var/www/backend)

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==> %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31m==> %s\033[0m\n' "$*" >&2; exit 1; }

# run executes a command, or prints it with --dry-run.
run() {
	if ((DRY)); then
		printf '    [dry-run] %s\n' "$(printf '%q ' "$@")"
	else
		"$@"
	fi
}

# write_file PATH MODE OWNER: writes stdin to PATH, or shows it with --dry-run.
write_file() {
	local path=$1 mode=$2 owner=$3 tmp
	tmp=$(mktemp)
	cat >"$tmp"
	if ((DRY)); then
		printf '    [dry-run] write %s (%s %s):\n' "$path" "$mode" "$owner"
		sed 's/^/        | /' "$tmp"
		rm -f "$tmp"
		return
	fi
	install -D -m "$mode" -o "${owner%%:*}" -g "${owner##*:}" "$tmp" "$path"
	rm -f "$tmp"
}

as_service() { runuser -u "$SERVICE_USER" -- env HOME="$SVC_HOME" PATH="$SVC_HOME/.local/bin:/usr/local/bin:/usr/bin:/bin" "$@"; }

[[ $EUID -eq 0 ]] || die "run as root: sudo $0 $*"
command -v systemctl >/dev/null || die "systemd is required"
SYSTEMCTL=$(command -v systemctl)
id -u "$SERVICE_USER" >/dev/null 2>&1 || die "service account '$SERVICE_USER' does not exist (create it: useradd -m $SERVICE_USER)"
id -u "$ADMIN_USER" >/dev/null 2>&1 || die "admin user '$ADMIN_USER' does not exist"
SVC_HOME=$(getent passwd "$SERVICE_USER" | cut -d: -f6)
SVC_GROUP=$(id -gn "$SERVICE_USER")
BIN="$SVC_HOME/.local/bin/nimdeploy"
CONF_DIR="$SVC_HOME/.config/nimdeploy"
PM2="$SVC_HOME/.local/bin/pm2"
SUDOERS="/etc/sudoers.d/nimdeploy-$ADMIN_USER"
UNITS=(nimdeploy.service)
((WITH_PM2)) && UNITS+=(pm2-deploy.service)
((DRY)) && warn "dry run: nothing will be changed"

uninstall() {
	log "stopping and removing ${UNITS[*]} and $SUDOERS"
	for u in nimdeploy.service pm2-deploy.service; do
		if [[ -e /etc/systemd/system/$u ]]; then
			run "$SYSTEMCTL" disable --now "$u" || true
			run rm -f "/etc/systemd/system/$u"
		fi
	done
	run "$SYSTEMCTL" daemon-reload
	run rm -f "$SUDOERS"
	log "kept $SERVICE_USER's files ($BIN, $CONF_DIR, ~/.pm2) and /var/log/nimdeploy; remove them by hand if needed"
	log "$ADMIN_USER is still in the systemd-journal group: gpasswd -d $ADMIN_USER systemd-journal"
}
if [[ $ACTION == uninstall ]]; then
	uninstall
	exit 0
fi

# --- 1. binary -----------------------------------------------------------------
log "nimdeploy binary for $SERVICE_USER: $BIN"
run install -d -m 755 -o "$SERVICE_USER" -g "$SVC_GROUP" "$SVC_HOME/.local" "$SVC_HOME/.local/bin" "$SVC_HOME/bin"
if [[ -n "$BINARY" ]]; then
	[[ -x "$BINARY" ]] || die "--binary $BINARY is not executable"
	run install -m 755 -o "$SERVICE_USER" -g "$SVC_GROUP" "$BINARY" "$BIN"
else
	case "$(uname -m)" in
	x86_64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
	esac
	if [[ $VERSION == latest ]]; then BASE="$REPO_URL/releases/latest/download"; else BASE="$REPO_URL/releases/download/$VERSION"; fi
	if ((DRY)); then
		run curl -fsSLO "$BASE/nimdeploy_linux_$ARCH"
		run sha256sum -c --ignore-missing checksums.txt
		run install -m 755 -o "$SERVICE_USER" -g "$SVC_GROUP" "nimdeploy_linux_$ARCH" "$BIN"
	else
		TMP=$(mktemp -d)
		trap 'rm -rf "$TMP"' EXIT
		(cd "$TMP" && curl -fsSLO "$BASE/nimdeploy_linux_$ARCH" && curl -fsSLO "$BASE/checksums.txt" &&
			sha256sum -c --ignore-missing --quiet checksums.txt) || die "download or checksum of $BASE/nimdeploy_linux_$ARCH failed"
		install -m 755 -o "$SERVICE_USER" -g "$SVC_GROUP" "$TMP/nimdeploy_linux_$ARCH" "$BIN"
	fi
fi
if ((!DRY)); then
	log "  $("$BIN" -version)"
	"$BIN" install -h 2>&1 | grep -q -- -system-service ||
		warn "this nimdeploy has no 'install --system-service'; deploys must be added by editing $CONF_DIR/config.toml"
fi

# --- 2. config and secrets -------------------------------------------------------
log "config and secrets in $CONF_DIR (owned by $SERVICE_USER)"
run install -d -m 750 -o "$SERVICE_USER" -g "$SVC_GROUP" "$SVC_HOME/.config" "$CONF_DIR"
if [[ -e "$CONF_DIR/config.toml" ]]; then
	log "  keeping existing config.toml"
else
	write_file "$CONF_DIR/config.toml" 640 "$SERVICE_USER:$SVC_GROUP" <<EOF
# nimdeploy, set up by setup-root.sh. The service runs as $SERVICE_USER
# (nimdeploy.service). Deploys are added as $SERVICE_USER, for example:
#   sudo -iu $SERVICE_USER nimdeploy install --system-service \\
#       --repo OWNER/REPO --dir /var/www/APP --command $SVC_HOME/bin/deploy-APP.sh
#   sudo systemctl restart nimdeploy
# Put the deploy logic in scripts under $SVC_HOME/bin and pass their full path:
# through "sudo -i", \$VARIABLES and ~ in --command would be expanded too early.
# Every option: $REPO_URL/blob/main/config.example.toml

[server]
listen = "127.0.0.1:9000"
api_token_env = "NIMDEPLOY_API_TOKEN"

[logging]
directory = "/var/log/nimdeploy"
retain = 30
EOF
fi
if [[ -e "$CONF_DIR/secrets.env" ]]; then
	log "  keeping existing secrets.env"
else
	TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
	((DRY)) && TOKEN="<generated>"
	write_file "$CONF_DIR/secrets.env" 600 "$SERVICE_USER:$SVC_GROUP" <<EOF
# nimdeploy secrets (systemd reads them on start: restart after changing).
# Webhook secrets are added by "nimdeploy install --system-service".
NIMDEPLOY_API_TOKEN=$TOKEN
EOF
fi

# --- 3. pm2 --------------------------------------------------------------------------
if ((WITH_PM2)); then
	if [[ -x "$PM2" ]]; then
		log "pm2 already installed for $SERVICE_USER: $PM2"
	elif command -v npm >/dev/null; then
		log "installing pm2 for $SERVICE_USER in $SVC_HOME/.local"
		if ((DRY)); then
			run runuser -u "$SERVICE_USER" -- npm install -g pm2 --prefix "$SVC_HOME/.local"
		else
			as_service npm install -g --no-fund --no-audit --loglevel=error pm2 --prefix "$SVC_HOME/.local" >/dev/null
		fi
	else
		warn "npm not found: pm2 not installed; install Node.js and rerun, or use --no-pm2"
		WITH_PM2=0
		UNITS=(nimdeploy.service)
	fi
fi

# --- 4. systemd units ------------------------------------------------------------------
log "systemd units: ${UNITS[*]} (User=$SERVICE_USER)"
write_file /etc/systemd/system/nimdeploy.service 644 root:root <<EOF
[Unit]
Description=nimdeploy - git webhook deployer (runs as $SERVICE_USER)
# Listens on 127.0.0.1: no need to wait for network-online at boot.
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SVC_GROUP
# Binary, config and secrets belong to $SERVICE_USER: updated without root.
EnvironmentFile=$CONF_DIR/secrets.env
ExecStartPre=$BIN -config $CONF_DIR/config.toml -check
ExecStart=$BIN -config $CONF_DIR/config.toml
ExecReload=$BIN -config $CONF_DIR/config.toml -check
ExecReload=/bin/kill -HUP \$MAINPID
# /var/log/nimdeploy, owned by $SERVICE_USER: one log file per deploy.
LogsDirectory=nimdeploy
Restart=on-failure
RestartSec=5
# Let a running deploy finish before stopping.
TimeoutStopSec=6min
KillMode=mixed

[Install]
WantedBy=multi-user.target
EOF
if ((WITH_PM2)); then
	write_file /etc/systemd/system/pm2-deploy.service 644 root:root <<EOF
[Unit]
Description=pm2 of $SERVICE_USER (Node.js apps)
After=network.target

[Service]
Type=forking
User=$SERVICE_USER
Group=$SVC_GROUP
LimitNOFILE=infinity
Environment=PM2_HOME=$SVC_HOME/.pm2
Environment=PATH=$SVC_HOME/.local/bin:/usr/local/bin:/usr/bin:/bin
PIDFile=$SVC_HOME/.pm2/pm2.pid
Restart=on-failure
ExecStart=$PM2 resurrect
ExecReload=$PM2 reload all
ExecStop=$PM2 kill

[Install]
WantedBy=multi-user.target
EOF
fi
run "$SYSTEMCTL" daemon-reload
run "$SYSTEMCTL" enable "${UNITS[@]}"
if ((WITH_PM2)); then
	run "$SYSTEMCTL" start pm2-deploy.service || warn "pm2-deploy.service did not start: journalctl -u pm2-deploy"
fi
# nimdeploy starts once it has a valid config with at least one deploy.
if ((!DRY)) && as_service bash -c "set -a; . '$CONF_DIR/secrets.env'; '$BIN' -config '$CONF_DIR/config.toml' -check" >/dev/null 2>&1; then
	run "$SYSTEMCTL" restart nimdeploy.service
	NIMDEPLOY_STATE="running"
else
	NIMDEPLOY_STATE="enabled, starts when the first deploy is added"
fi

# --- 5. admin user's privileges -----------------------------------------------------------
log "sudo rules for $ADMIN_USER: $SUDOERS"
TAG=""
((NOPASSWD)) && TAG="NOPASSWD: "
ALIAS="NIMDEPLOY_SVC_$(printf '%s' "${ADMIN_USER^^}" | tr -c 'A-Z0-9_' '_')"
SVC_CMDS=()
for u in "${UNITS[@]}"; do
	for verb in start stop restart reload; do
		SVC_CMDS+=("$SYSTEMCTL $verb $u" "$SYSTEMCTL $verb ${u%.service}")
	done
done
SUDO_TMP=$(mktemp)
{
	echo "# Created by nimdeploy setup-root.sh. $ADMIN_USER operates the service account"
	echo "# $SERVICE_USER with their own nominal user: sudo logs every command as $ADMIN_USER."
	echo "# No service runs files owned by $SERVICE_USER as root, so acting as $SERVICE_USER"
	echo "# gives no root. As root, only these units can be controlled."
	echo "$ADMIN_USER ALL=($SERVICE_USER) ${TAG}ALL"
	printf 'Cmnd_Alias %s = ' "$ALIAS"
	(IFS=,; printf '%s\n' "${SVC_CMDS[*]}" | sed 's/,/, /g')
	echo "$ADMIN_USER ALL=(root) ${TAG}$ALIAS"
} >"$SUDO_TMP"
visudo -cqf "$SUDO_TMP" || { rm -f "$SUDO_TMP"; die "generated sudoers does not validate"; }
write_file "$SUDOERS" 440 root:root <"$SUDO_TMP"
rm -f "$SUDO_TMP"
if ((!NOPASSWD)) && passwd -S "$ADMIN_USER" 2>/dev/null | awk '{print $2}' | grep -qE '^(L|LK|NP)$'; then
	warn "$ADMIN_USER has no usable password, so sudo will ask for one they don't have: set a password or rerun with --nopasswd"
fi
if getent group systemd-journal >/dev/null; then
	log "journal access for $ADMIN_USER (systemd-journal group, read-only)"
	if id -nG "$ADMIN_USER" | tr ' ' '\n' | grep -qx systemd-journal; then
		log "  already a member"
	else
		run usermod -aG systemd-journal "$ADMIN_USER"
	fi
fi

# --- 6. application directories --------------------------------------------------------------
for dir in "${APP_DIRS[@]}"; do
	if [[ -d "$dir" ]]; then
		if ((DRY)) || runuser -u "$SERVICE_USER" -- test -w "$dir"; then
			log "app dir $dir: $(stat -c '%U:%G %a' "$dir"), writable by $SERVICE_USER"
		else
			warn "app dir $dir exists but $SERVICE_USER cannot write it ($(stat -c '%U:%G %a' "$dir"))"
		fi
	else
		# Same group as the app dirs that already exist (e.g. a shared dev group).
		group=$SVC_GROUP
		for other in "${APP_DIRS[@]}"; do
			if [[ -d "$other" ]] && [[ "$(stat -c %G "$other")" != root ]]; then
				group=$(stat -c %G "$other")
				break
			fi
		done
		log "app dir $dir: creating ($SERVICE_USER:$group 2775)"
		run install -d -m 2775 -o "$SERVICE_USER" -g "$group" "$dir"
	fi
done

# --- 7. checks (no changes) -------------------------------------------------------------------
if command -v nginx >/dev/null; then
	if nginx -T 2>/dev/null | grep -qE 'location[[:space:]]+[^{]*/hooks/'; then
		log "nginx: a /hooks/ location exists"
	else
		warn "nginx: no /hooks/ location found; the webhooks need it (print it with: sudo -iu $SERVICE_USER nimdeploy nginx)"
	fi
fi
if command -v getenforce >/dev/null && [[ "$(getenforce)" == Enforcing ]]; then
	warn "SELinux is enforcing: systemd may refuse to run $BIN from a home directory; check 'journalctl -u nimdeploy'"
fi

# --- summary ---------------------------------------------------------------------------------
((DRY)) && { warn "dry run finished: nothing was changed"; exit 0; }
cat <<EOF

nimdeploy is set up. nimdeploy.service: $NIMDEPLOY_STATE.$( ((WITH_PM2)) && printf '\npm2-deploy.service: %s.' "$($SYSTEMCTL is-active pm2-deploy.service 2>/dev/null)")

What $ADMIN_USER can do now (log out and in once for the journal group):
  work as $SERVICE_USER    sudo -iu $SERVICE_USER      (git, composer, npm, pm2, scripts in $SVC_HOME/bin, nimdeploy)
  add a deploy       sudo -iu $SERVICE_USER nimdeploy install --system-service \\
                         --repo OWNER/REPO --dir /var/www/APP --command $SVC_HOME/bin/deploy-APP.sh
                     (deploy logic in a script with its full path: through sudo -i,
                      \$VARIABLES and ~ in --command would be expanded too early)
  apply changes      sudo systemctl restart nimdeploy
  status and logs    sudo -iu $SERVICE_USER nimdeploy status
                     journalctl -u nimdeploy -f  (no sudo)
                     /var/log/nimdeploy/<deploy>/latest.log
EOF
