# Generic webhooks and Ansible

Besides git pushes, nimdeploy accepts **any JSON webhook**: from Ansible or
AWX, a CI job, a monitoring alert, a chat bot, a cron on another server. You
declare which values to take from the JSON, how each must look, and nimdeploy
hands them to your command as environment variables after validating them.

```text
POST /hooks/services   {"action":"deploy","service":"api","release":{"tag":"v1.4.2"}}
        │  signature or token ✓   when: action = "deploy" ✓
        │  SERVICE = "api"  (enum api|worker|web ✓)   VERSION = "v1.4.2"  (match ✓)
        ▼
/home/deploy/bin/deploy-services.sh   with SERVICE=api VERSION=v1.4.2
```

## A complete example

```toml
[deploy.services]
provider = "generic"
path = "/hooks/services"
secret_env = "SERVICES_WEBHOOK_SECRET"
timestamp_header = "X-Timestamp"      # signed timestamp: refuses replays older than max_skew
pusher_from = "requested_by"          # shown as BY in status/history
queue_key = "SERVICE"                 # one lock and queue per service
command = "/home/deploy/bin/deploy-services.sh"
timeout = "20m"

[deploy.services.when]                # only run when the JSON says so
action = "deploy"

[deploy.services.params]
SERVICE = { from = "service",     enum = ["api", "worker", "web"], required = true }
VERSION = { from = "release.tag", match = '^(v\d+\.\d+\.\d+|latest)$', default = "latest" }
TARGET  = { from = "target",      match = '^[a-z0-9.-]+$', default = "all" }
```

```bash title="/home/deploy/bin/deploy-services.sh"
#!/usr/bin/env bash
export PLAYBOOK=/srv/ansible/deploy.yml
export INVENTORY=/srv/ansible/hosts.ini
export ANSIBLE_VARS="SERVICE VERSION"      # → extra vars service, version
export LIMIT_PARAM=TARGET                  # → --limit
exec /home/deploy/bin/run-ansible.sh
```

[`deploy/examples/run-ansible.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/run-ansible.sh)
turns the params into **JSON extra vars** (`--extra-vars '{"service":"api",...}'`),
so no value is ever pasted into a command line.

Trigger it:

```bash
export NIMDEPLOY_SECRET=...           # the value of SERVICES_WEBHOOK_SECRET
nimdeploy send -timestamp-header X-Timestamp \
  -data '{"action":"deploy","service":"api","release":{"tag":"v1.4.2"},"requested_by":"ci"}' \
  https://example.com/hooks/services
```

```text
HTTP 202
{ "result": "started", ... }
```

Or by hand on the server, with the same validation:

```bash
nimdeploy run -f -p SERVICE=worker -p VERSION=v1.4.2 services
```

## What happens with each request

| Check | Fails with |
|---|---|
| Authentication: HMAC signature (default) or token | `401`, nothing runs |
| Timestamp within `max_skew`, when `timestamp_header` is set | `401` |
| Body is JSON | `400` |
| `when` conditions | `200 ignored` (logged with the reason) |
| Every param present if `required`, and valid | `400` with the reason, nothing runs |
| Duplicate delivery ID (`delivery_header`) | `200 ignored` |
| Otherwise | `202`: started, or queued behind the same lane |

## Authentication

=== "HMAC signature (default)"

    The sender signs the raw body with the shared secret and sends
    `X-Signature: sha256=<hex>` (or just `<hex>`):

    ```toml
    auth = "hmac"                       # default
    signature_header = "X-Signature"    # default
    timestamp_header = "X-Timestamp"    # optional but recommended
    max_skew = "5m"                     # default
    ```

    With `timestamp_header` the signature covers `"<timestamp>.<body>"` and
    the timestamp (Unix seconds or RFC 3339) must be within `max_skew` of the
    server's clock, so a captured request can't be replayed later.

    ```bash
    ts=$(date +%s)
    sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
    curl -fsS https://example.com/hooks/services \
      -H "Content-Type: application/json" -H "X-Timestamp: $ts" -H "X-Signature: sha256=$sig" \
      -H "X-Delivery-ID: $(uuidgen)" -d "$body"
    ```

    `nimdeploy send` does all of that for you.

=== "Token"

    For senders that can't sign (Ansible's `uri`, some SaaS webhooks):

    ```toml
    auth = "token"
    token_header = "Authorization"      # default: expects "Bearer <token>"
    # token_header = "X-Deploy-Token"   # or any header, compared as is
    ```

    The token travels in the request (over HTTPS), so prefer HMAC when the
    sender supports it.

The secret is per deploy, in `secrets.env` like any other (`secret_env`).
Generate it with `openssl rand -hex 32`.

## Params

```toml
[deploy.services.params]
NAME = { from = "json.path", enum = [...] | match = 'regex', required = true | default = "x", max_len = 256 }
```

| Key | |
|---|---|
| `from` | JSON path: `service`, `release.tag`, `hosts[0].name`, `["key.with.dots"]` |
| `enum` | allowed values, exact match |
| `match` | regular expression the value must match (anchor it: `^...$`) |
| *(neither)* | the value may only contain `A-Z a-z 0-9 . _ : / @ + = , -` |
| `required` | missing or empty → `400` |
| `default` | used when missing or empty (must itself be valid) |
| `max_len` | default 256 bytes |

- Strings, numbers and booleans are accepted (`true`/`false`, numbers as
  written); objects, arrays and control characters (newlines…) are refused.
- Names are `UPPER_CASE` and become environment variables. Names that change
  how programs behave are refused: `PATH`, `HOME`, `IFS`, `BASH_ENV`, `LD_*`,
  `NODE_OPTIONS`, `PYTHONPATH`, `GIT_SSH_COMMAND`, `DEPLOY_*`…
- Values appear in the deploy log (`param.SERVICE=api`), in
  `nimdeploy status` / `history` and in notifications. **Don't send secrets
  as params.**

Params and `when` also work with the **git providers**, reading the push
payload. For example, deploy only pushes from some people and tell the script
who it was:

```toml
[deploy.shop.when]
"head_commit.author.username" = ["ana", "leo"]
[deploy.shop.params]
AUTHOR = { from = "head_commit.author.username", required = true }
```

!!! warning "A param that doesn't validate rejects the request"
    Only declare params whose values you can predict. A commit message, for
    instance, may contain newlines, which are always refused: as a param it
    would block that deploy with a `400`.

## `when`

Each key is a JSON path; the value is what it must equal, or a list of
accepted values. All conditions must hold.

```toml
[deploy.services.when]
action = "deploy"
"meta.environment" = ["stage", "production"]
"meta.dry_run" = false
```

Quote keys that contain dots (`"meta.environment"`): in TOML an unquoted
dotted key would create nested tables.

### Operators

Instead of a value, a table of operators; all of them must hold:

```toml
[deploy.releases.when]
ref = { match = "^refs/tags/v[0-9]+", not_match = "-rc" }   # regular expressions
"sender.login" = { not = ["dependabot[bot]", "renovate[bot]"] }
"pull_request.additions" = { lte = 500 }                    # numbers: gt, gte, lt, lte
"meta.dry_run" = { exists = false }                         # present or not
title = { prefix = "[deploy]", contains = "api" }           # also suffix
environment = { in = ["stage", "production"] }              # same as a list
```

### `when_any`: one of them is enough

`when` needs every condition; `when_any` needs one (same syntax). With both,
the request must pass `when` **and** one of `when_any`:

```toml
[deploy.releases.when]
action = "published"
[deploy.releases.when_any]
"release.tag_name" = { match = "^v[0-9]+\\.[0-9]+\\.0$" }   # a minor or major release
"release.body" = { contains = "[deploy]" }                  # or one that asks for it
```

These work with every provider (git, WooCommerce, payments), not only generic.

## Several deploys on one path

Several deploys can share a `path`: each request goes to all of them, and each
one decides on its own with its `when`/`when_any`, its params and its queue.
That is how one webhook triggers different actions ("event → rules →
actions"):

```toml
[deploy.app]                     # pushes to main: deploy the app
path = "/hooks/gitea"
provider = "generic"
secret_env = "GITEA_SECRET"
command = "/opt/deploy/app.sh"
[deploy.app.when]
ref = "refs/heads/main"

[deploy.docs]                    # changes under docs/: rebuild the site
path = "/hooks/gitea"
provider = "generic"
secret_env = "GITEA_SECRET"
command = "/opt/deploy/docs.sh"
[deploy.docs.when]
ref = "refs/heads/main"
[deploy.docs.when_any]
"head_commit.modified[0]" = { prefix = "docs/" }
```

- They run in name order, independently: one being ignored or failing doesn't
  stop the others, and each has its own lock and queue.
- They must authenticate the request the same way: same `provider`,
  `secret_env` and signature/token headers (checked when the config loads).
- The answer lists each one: `{"results": [{"deploy": "app", "code": 202, ...}, {"deploy": "docs", "code": 200, ...}]}`.
  The status is `202` if any started or queued, `200` if all ignored it, else
  the first refusal (`401`, `400`...).

## Queues per value: `queue_key`

Without it, a deploy runs one at a time and a request arriving meanwhile is
queued, with newer requests replacing the queued one. That is right for "deploy
the latest commit", but wrong when requests differ: "restart api" must not be
replaced by "restart web".

`queue_key = "SERVICE"` gives each value its own lane: `api` and `web` run in
parallel, two `api` requests are serialized, and a third `api` replaces the
queued one.

## From Ansible

The [Ansible collection](ansible.md#the-ansible-collection) wraps this:
`aitorroma.nimdeploy.nimdeploy_send` signs and posts the request. Without it:

=== "nimdeploy send (HMAC)"

    ```yaml
    - name: Deploy the new release
      ansible.builtin.command:
        argv:
          - nimdeploy
          - send
          - -timestamp-header
          - X-Timestamp
          - -data
          - "{{ {'action': 'deploy', 'service': service, 'release': {'tag': version}, 'requested_by': 'ansible'} | to_json }}"
          - https://deploy.example.com/hooks/services
      environment:
        NIMDEPLOY_SECRET: "{{ vault_services_webhook_secret }}"
      delegate_to: localhost
      no_log: true
    ```

=== "uri module (token)"

    With `auth = "token"` on the deploy:

    ```yaml
    - name: Deploy the new release
      ansible.builtin.uri:
        url: https://deploy.example.com/hooks/services
        method: POST
        body_format: json
        body:
          action: deploy
          service: "{{ service }}"
          release: { tag: "{{ version }}" }
        headers:
          Authorization: "Bearer {{ vault_services_webhook_secret }}"
          X-Delivery-ID: "{{ lookup('ansible.builtin.password', '/dev/null', chars=['ascii_lowercase', 'digits'], length=32) }}"
        status_code: [200, 202]
      delegate_to: localhost
      no_log: true
    ```

## `nimdeploy send`

Signs and posts a JSON body; it needs no config, only the URL and the secret.

```text
nimdeploy send [flags] <url>
  -data JSON | @file | @-        body (default {})
  -secret-env NAME               variable holding the secret (default NIMDEPLOY_SECRET)
  -auth hmac|token               (default hmac)
  -signature-header NAME         (default X-Signature)
  -token-header NAME             (default Authorization → "Bearer <token>")
  -timestamp-header NAME         also send and sign the current time
  -delivery-header NAME          random ID per request (default X-Delivery-ID; "" to omit)
  -timeout 30s
```

It prints the HTTP status and body, and exits `0` on 2xx, `1` otherwise.

## Reference

| Key (generic only) | Default | |
|---|---|---|
| `auth` | `hmac` | `hmac` or `token` |
| `signature_header` | `X-Signature` | hmac |
| `timestamp_header` | – | hmac: sign `<ts>.<body>` and refuse old requests |
| `max_skew` | `5m` | |
| `token_header` | `Authorization` | token: `Authorization` expects `Bearer <token>` |
| `delivery_header` | `X-Delivery-ID` | unique ID per request; duplicates are ignored (`X-Request-ID` also read) |
| `pusher_from` | – | JSON path of who triggered it (shown as BY) |

| Key (any provider) | |
|---|---|
| `when` | conditions on the JSON |
| `params` | values passed to the command |
| `queue_key` | param giving each value its own lock and queue |

`repository` is optional for generic deploys (it only appears in logs);
`branch` and `wait_for_ci` don't apply.
