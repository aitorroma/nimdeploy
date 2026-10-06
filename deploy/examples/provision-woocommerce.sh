#!/usr/bin/env bash
# Provision what a WooCommerce order bought, then tell the shop.
# nimdeploy runs it for each paid order (provider = "woocommerce"):
#   DEPLOY_RESOURCE_ID   the order ID
#   DEPLOY_PAYLOAD_FILE  the order as JSON (0600, deleted after the run)
#   DEPLOY_EVENT         order.created / order.updated
#   NIMDEPLOY            nimdeploy itself, to write back to the order
#
# Rules it follows, because WooCommerce does not retry and sends updates often:
#   - idempotent: an order already provisioned is skipped, so replays and
#     repeated "order.updated" events are harmless;
#   - nothing personal in the log: the order's email/address stay in the file;
#   - the shop always learns the outcome: a note, and the order completed or on hold.
#
# Settings (export them in a wrapper, or edit below):
#   STATE_DIR=~/.local/state/provision   where finished orders are remembered
#   PROVISION=/home/deploy/bin/create-service.sh   what actually creates the service;
#            it gets ORDER, PLAN and EMAIL in its environment
set -Eeuo pipefail
: "${DEPLOY_RESOURCE_ID:?run by nimdeploy}" "${DEPLOY_PAYLOAD_FILE:?run by nimdeploy}"
command -v jq >/dev/null || { echo "ERROR: jq is required" >&2; exit 1; }
ORDER=$DEPLOY_RESOURCE_ID
STATE_DIR=${STATE_DIR:-$HOME/.local/state/provision}
PROVISION=${PROVISION:-}

note() { # note <text> [status]
	local args=()
	[[ -n "${2:-}" ]] && args+=(-status "$2")
	"${NIMDEPLOY:-nimdeploy}" woocommerce note "$DEPLOY_NAME" "${args[@]}" "$ORDER" "$1" ||
		echo "warning: could not write to order $ORDER" >&2
}
failed() {
	note "Provisioning failed on $(hostname -s): see \"nimdeploy history $DEPLOY_NAME\". We are on it." on-hold
}

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"
done_file="$STATE_DIR/order-$ORDER.done"
if [[ -e "$done_file" ]]; then
	echo "order $ORDER already provisioned on $(cat "$done_file"); nothing to do"
	exit 0
fi
# One run per order at a time, even across deploys.
exec 9>"$STATE_DIR/order-$ORDER.lock"
flock -n 9 || { echo "order $ORDER is being provisioned by another run"; exit 0; }

status=$(jq -r '.status' "$DEPLOY_PAYLOAD_FILE")
plan=$(jq -r '[.line_items[]?.sku | select(. != null and . != "")][0] // empty' "$DEPLOY_PAYLOAD_FILE")
email=$(jq -r '.billing.email // empty' "$DEPLOY_PAYLOAD_FILE")
echo "order $ORDER ($DEPLOY_EVENT, status $status): plan '${plan:-none}'"

case "$plan" in
basic | pro) ;;
*)
	note "Order $ORDER has no product this server provisions (sku '${plan:-none}'); left as is."
	exit 0
	;;
esac

trap failed ERR
if [[ -n "$PROVISION" ]]; then
	ORDER=$ORDER PLAN=$plan EMAIL=$email "$PROVISION"
else
	echo "PROVISION is not set: this is where the service for plan '$plan' would be created"
fi
trap - ERR

date -Iseconds >"$done_file"
note "Provisioned: plan $plan on $(hostname -s)." completed
echo "order $ORDER provisioned"
