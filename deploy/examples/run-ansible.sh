#!/usr/bin/env bash
# Run an Ansible playbook with the params a generic webhook captured.
# nimdeploy validates each param (enum/match) and passes it as an environment
# variable; this script hands them to Ansible as JSON extra vars, so no value
# is ever pasted into a command line or interpreted by a shell.
#
#   PLAYBOOK=/srv/ansible/deploy.yml      required
#   INVENTORY=/srv/ansible/hosts.ini      optional (-i)
#   ANSIBLE_VARS="SERVICE VERSION"        params passed as extra vars, lowercased:
#                                         SERVICE=api -> service: "api"
#   LIMIT_PARAM=TARGET                    param used as --limit (optional)
#   ANSIBLE_ARGS="--diff"                 extra arguments, fixed in the wrapper (optional)
#
# Example deploy (config.toml):
#   command = "/home/deploy/bin/deploy-services.sh"   # exports the above, execs this
#   [deploy.services.params]
#   SERVICE = { from = "service", enum = ["api", "worker"], required = true }
#   VERSION = { from = "version", match = '^v\d+\.\d+\.\d+$', required = true }
set -Eeuo pipefail
: "${PLAYBOOK:?set PLAYBOOK}"
command -v ansible-playbook >/dev/null || { echo "ERROR: ansible-playbook not found in PATH" >&2; exit 1; }

# Build {"service": "...", ...} from the listed variables (python ships with Ansible).
extra=$(python3 - "${ANSIBLE_VARS:-}" <<'PY'
import json, os, sys
names = sys.argv[1].split()
print(json.dumps({n.lower(): os.environ[n] for n in names if os.environ.get(n, "") != ""}))
PY
)

args=("$PLAYBOOK" --extra-vars "$extra")
if [[ -n "${INVENTORY:-}" ]]; then
	args+=(-i "$INVENTORY")
fi
if [[ -n "${LIMIT_PARAM:-}" && -n "${!LIMIT_PARAM:-}" ]]; then
	args+=(--limit "${!LIMIT_PARAM}")
fi
if [[ -n "${ANSIBLE_ARGS:-}" ]]; then
	# Fixed by the wrapper, never by the webhook: split on spaces on purpose.
	read -r -a fixed <<<"$ANSIBLE_ARGS"
	args+=("${fixed[@]}")
fi

echo "trigger: ${DEPLOY_TRIGGER:-?} by ${DEPLOY_PUSHER:-?} delivery ${DEPLOY_DELIVERY:-?}"
echo "extra vars: $extra"
echo "\$ ansible-playbook ${args[*]}"
export ANSIBLE_FORCE_COLOR=0 PYTHONUNBUFFERED=1
exec ansible-playbook "${args[@]}"
