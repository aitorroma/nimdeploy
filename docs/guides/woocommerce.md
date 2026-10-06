# Deploy from a WooCommerce sale

Sell something in WordPress, and have your server set it up the moment it's
paid: a hosting account, an app instance, a license, a VPN profile, a course
environment. nimdeploy listens to your shop's webhooks and runs your
provisioning script (or Ansible playbook) for each paid order, then writes
the result back to the order.

```text
Customer pays ──▶ WooCommerce ──webhook (signed)──▶ nimdeploy ──▶ provision.sh / Ansible
                       ▲                                                │
                       └──── note "Provisioned" + status completed ─────┘
```

## Set it up in one command

1. In WordPress: **WooCommerce → Settings → Advanced → REST API → Add key**,
   with *Read/Write* permissions. Copy the consumer key and secret.
2. On the server, as the user nimdeploy runs as:

```bash
export WC_CONSUMER_KEY=ck_...  WC_CONSUMER_SECRET=cs_...   # or it asks for them
nimdeploy woocommerce add \
  --store https://shop.example.com \
  --url https://deploy.example.com \
  --name orders \
  --command /home/deploy/bin/provision.sh
```

```text
==> config   /home/deploy/.config/nimdeploy/config.toml (deploy.orders added)
==> secrets  /home/deploy/.config/nimdeploy/secrets.env (ORDERS_WEBHOOK_SECRET, ORDERS_WC_KEY, ORDERS_WC_SECRET)
==> shop     https://shop.example.com answers (REST API keys OK)
==> webhook  order.created → https://deploy.example.com/hooks/orders (#41 created, active)
==> webhook  order.updated → https://deploy.example.com/hooks/orders (#42 created, active)

Next:
  1. Apply the new secrets:   restart nimdeploy
  2. nginx must pass /hooks/orders to nimdeploy ("nimdeploy nginx" prints the block)
```

That is the whole setup. Like n8n's WooCommerce trigger, nimdeploy creates
the webhooks in the shop through its REST API, with a secret it generates, so
nothing has to be copied by hand into WordPress. Unlike it, nimdeploy also
queues orders on disk, can replay missed ones and reports back to the order.

| Option | Default | |
|---|---|---|
| `--store` | required | the shop's URL |
| `--url` | – | public base URL where your proxy serves nimdeploy (must be `https://`). Without it, register later |
| `--name` | `woocommerce` | deploy name: hook `/hooks/<name>`, logs, CLI |
| `--command` | required | what to run for each order (bash) |
| `--dir` | current | working directory |
| `--topics` | `order.created,order.updated` | WooCommerce events |
| `--statuses` | `processing,completed` | order statuses that run the command; `""` for any |
| `--no-register` | off | only write the config |

## What your script gets

| Variable | Example |
|---|---|
| `DEPLOY_RESOURCE_ID` | `1234` (the order ID) |
| `DEPLOY_EVENT` | `order.updated` |
| `DEPLOY_PAYLOAD_FILE` | path to the order as JSON (mode `600`, deleted after the run) |
| `DEPLOY_REPOSITORY` | the shop URL |
| `NIMDEPLOY` | path to nimdeploy, to write back to the order |
| your `params` | values you declared, validated (see below) |

Read anything from the order with `jq`, without it ever reaching the logs:

```bash
plan=$(jq -r '.line_items[0].sku' "$DEPLOY_PAYLOAD_FILE")
email=$(jq -r '.billing.email' "$DEPLOY_PAYLOAD_FILE")
```

And close the loop on the order:

```bash
"$NIMDEPLOY" woocommerce note "$DEPLOY_NAME" -status completed "$DEPLOY_RESOURCE_ID" "Your site is ready"
"$NIMDEPLOY" woocommerce note "$DEPLOY_NAME" -customer "$DEPLOY_RESOURCE_ID" "Access details sent by email"
"$NIMDEPLOY" woocommerce note "$DEPLOY_NAME" -status on-hold "$DEPLOY_RESOURCE_ID" "Provisioning failed, we're on it"
```

`-customer` makes it a customer note: WooCommerce emails it to the buyer.

## A provisioning script

[`deploy/examples/provision-woocommerce.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/provision-woocommerce.sh)
is a starting point that follows the rules below: it skips orders already
provisioned, takes a lock per order, reads the plan from the order, calls your
`PROVISION` command with `ORDER`, `PLAN` and `EMAIL`, and marks the order
*completed* (or *on hold* with a note if anything fails).

```bash title="/home/deploy/bin/provision.sh"
#!/usr/bin/env bash
export PROVISION=/home/deploy/bin/create-service.sh   # or a wrapper around ansible-playbook
exec /home/deploy/bin/provision-woocommerce.sh
```

## How WooCommerce sends webhooks (and why it matters)

These come from WooCommerce's own code, and they shape how nimdeploy and your
script behave:

| WooCommerce… | so nimdeploy… | and your script… |
|---|---|---|
| **never retries** a failed delivery | queues every accepted order **on disk** (`queue_mode = "all"`): a restart resumes the queue and runs again an order it interrupted | should be **idempotent** per order |
| **disables the webhook after 5 failures** in a row (any non-2xx) | never answers a signed delivery with an error: invalid orders get `200` + a log line + a notification | — |
| fires `order.updated` on **every save** (notes, stock, status) and sends the state **at delivery time** | filters by `statuses` and drops exact duplicates | must tolerate being called several times for one order |
| reuses the **delivery ID within the same second** | dedupes on delivery ID **plus** a hash of the body | — |
| treats a **redirect as delivered** | refuses `http://` webhook URLs | — |
| sends an **unsigned ping** on creation that must get exactly 200 | answers it | — |
| delivers through WP-Cron (**can be late** on quiet sites) | — | use `replay` for anything missed |

## Missed something? `replay`

Orders can be missed outside nimdeploy too (WP-Cron not running, the webhook
disabled, your server down). Replay fetches orders from the shop and runs them
through the same checks as a webhook:

```bash
nimdeploy woocommerce replay orders 1234 1235                          # these orders
nimdeploy woocommerce replay orders -status processing -after 2026-10-01 # every paid order since
nimdeploy woocommerce replay orders -status processing -n               # dry run: list only
```

Because the script is idempotent, replaying everything that is still
`processing` is safe, which makes a simple **reconciliation job** possible, e.g.
an hourly timer of the service account:

```bash
nimdeploy woocommerce replay orders -status processing -after "$(date -d '-2 days' +%F)"
```

## Check the webhooks

```bash
nimdeploy woocommerce status orders
```

```text
ID  TOPIC          STATUS    DELIVERY URL
41  order.created  active    https://deploy.example.com/hooks/orders
42  order.updated  disabled  https://deploy.example.com/hooks/orders

1 not active: WooCommerce disables a webhook after 5 failed deliveries in a row.
Fix the cause (journalctl -u nimdeploy), then: nimdeploy woocommerce status orders -enable
and recover what was missed: nimdeploy woocommerce replay orders -status processing -after <date>
```

`nimdeploy woocommerce register orders` creates or updates them again (for
example after rotating the secret); it never duplicates them.

## Configuration

What `add` writes, and what you can tune:

```toml
[deploy.orders]
provider = "woocommerce"
path = "/hooks/orders"
store_url = "https://shop.example.com"
webhook_url = "https://deploy.example.com/hooks/orders"
secret_env = "ORDERS_WEBHOOK_SECRET"
api_key_env = "ORDERS_WC_KEY"          # REST API keys: used by the CLI, never passed to the script
api_secret_env = "ORDERS_WC_SECRET"
topics = ["order.created", "order.updated"]
statuses = ["processing", "completed"]
working_directory = "/home/deploy"
command = "/bin/bash"
args = ["-eo", "pipefail", "-c", "/home/deploy/bin/provision.sh"]
timeout = "30m"
# queue_mode = "all"                   # default for woocommerce: no order is dropped
# queue_max = 1000                     # beyond it: 503 and a notification
# queue_key = "PLAN"                   # one queue per plan, in parallel

# Optional: validated values as environment variables
[deploy.orders.params]
PLAN = { from = "line_items[0].sku", enum = ["basic", "pro"] }

# Optional: more conditions on the order
[deploy.orders.when]
"payment_method" = ["stripe", "paypal"]
```

| Key | Default | |
|---|---|---|
| `store_url` | – | the shop; deliveries from another `X-WC-Webhook-Source` are ignored |
| `webhook_url` | – | public URL the shop calls; used by `register` and `status` |
| `api_key_env`, `api_secret_env` | – | REST API keys for the CLI (`register`, `status`, `replay`, `note`) |
| `topics` | `order.created`, `order.updated` | also `product.*`, `customer.*`, `coupon.*`, `action.<hook>` |
| `statuses` | – (any) | only these order statuses run |
| `queue_mode` | `all` | `latest` would keep only the newest waiting order: not for sales |
| `queue_max` | `1000` | |
| `payload_file` | `true` | `false`: no `DEPLOY_PAYLOAD_FILE` |

!!! warning "Personal data"
    The order JSON contains names, emails and addresses. nimdeploy keeps it
    out of logs and notifications: it lives only in `DEPLOY_PAYLOAD_FILE`
    (`600`, deleted after the run) and, while an order waits, in
    `/var/log/nimdeploy/<deploy>/queue.json` (`600`). Don't echo it from your
    script, and don't declare personal fields as `params` (those are logged).

## Subscriptions, refunds and cancellations

The same pattern covers the rest of the lifecycle: one deploy per action, each
with its own script.

| Event | Topic + statuses | Script |
|---|---|---|
| paid | `order.updated` · `processing`, `completed` | provision |
| refunded / cancelled | `order.updated` · `refunded`, `cancelled` | deprovision |
| subscription renewals, suspensions | `action.woocommerce_subscription_status_updated` (WooCommerce Subscriptions), **without** `statuses` | suspend / reactivate |

`action.<hook>` topics send `{"action": "...", "arg": ...}` instead of the
order, so leave `statuses` empty for them and filter with `when` (e.g.
`"arg" = ...`) or in the script.

## nimdeploy or n8n?

n8n's WooCommerce trigger registers the webhook the same way and is great for
business workflows without code (CRM, email, Slack). It doesn't queue, dedupe
or serialize runs per order, and running something on your server ends up in
an SSH or HTTP node anyway. nimdeploy is the piece that **runs it on your
server**: queue on disk, a log per order, no root, replays. They combine
well: n8n can call a [generic webhook](generic.md) of nimdeploy for the
server-side part.
