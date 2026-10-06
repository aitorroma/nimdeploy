# Stripe, Paddle and Lemon Squeezy

The same idea as [WooCommerce](woocommerce.md) for shops that sell with a
payment platform: a payment event arrives, nimdeploy checks its signature and
runs your provisioning script with the event in a file.

```toml
[deploy.checkout]
provider = "stripe"                     # or paddle, lemonsqueezy
path = "/hooks/stripe"
secret_env = "STRIPE_WEBHOOK_SECRET"    # whsec_... / pdl_ntfset_... / the signing secret
events = ["checkout.session.completed", "invoice.paid"]
command = "/home/deploy/bin/provision.sh"
timeout = "30m"

# Optional: validated values for the script
[deploy.checkout.params]
PLAN = { from = "data.object.metadata.plan", enum = ["basic", "pro"] }
```

| Provider | Create the webhook in | Signature | Event type → `DEPLOY_EVENT` | Object → `DEPLOY_RESOURCE_ID` |
|---|---|---|---|---|
| `stripe` | Developers → Webhooks → Add endpoint | `Stripe-Signature` (HMAC of `t.body`, ±5 min) | `type` | `data.object.id` |
| `paddle` (Billing) | Developer tools → Notifications → New destination | `Paddle-Signature` (HMAC of `ts:body`, ±5 min) | `event_type` | `data.id` |
| `lemonsqueezy` | Settings → Webhooks | `X-Signature` (HMAC of the body) | `meta.event_name` | `data.id` |

The URL is `https://<your domain>/hooks/<path>` and the secret goes in
`secrets.env` under `secret_env`. With signatures that carry a timestamp
(Stripe, Paddle), deliveries older than `max_skew` (default `5m`) are refused.

## What your script gets

`DEPLOY_EVENT`, `DEPLOY_RESOURCE_ID`, `DEPLOY_PAYLOAD_FILE` (the whole event,
`600`, deleted after the run) and your params. For example, with Stripe
Checkout:

```bash
session=$DEPLOY_RESOURCE_ID
customer_email=$(jq -r '.data.object.customer_details.email' "$DEPLOY_PAYLOAD_FILE")
plan=$(jq -r '.data.object.metadata.plan' "$DEPLOY_PAYLOAD_FILE")
```

Put what you need to provision (plan, account ID…) in the checkout's
`metadata` (Stripe), `custom_data` (Paddle) or `checkout_data.custom`
(Lemon Squeezy, arrives as `meta.custom_data`).

## Delivery rules

These platforms **retry** failed deliveries (Stripe for up to 3 days, Lemon
Squeezy 3 times), so nimdeploy answers accordingly:

| Situation | Answer | Why |
|---|---|---|
| bad or old signature | `401` | not from the platform |
| event type not in `events`, `when` not met | `200` ignored | nothing to do |
| invalid params, broken JSON | `200` rejected + log + notification | retrying would fail the same way |
| same event again (event ID, or body hash for Lemon Squeezy) | `200` duplicate | already queued or run |
| queue full (`queue_max`) | `503` | the platform retries later |
| accepted | `202` | queued on disk, in order (`queue_mode = "all"` by default) |

As with WooCommerce, make the script **idempotent** (keyed by the session,
transaction or order ID): retries and replays from the platform's dashboard
must not provision twice.
