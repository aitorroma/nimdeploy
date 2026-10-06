# Command line

```text
nimdeploy install                     install for the current user, no root needed
nimdeploy uninstall [--purge]         remove a user install
nimdeploy send [flags] <url>          POST a signed JSON body to a generic webhook (no config needed)
nimdeploy [flags] serve               run the webhook server (what the service does)
nimdeploy [flags] run [-commit SHA] [-p NAME=VALUE]... [-f] <deploy>
nimdeploy [flags] rollback [-to SHA] [-n] [-f] <deploy>
nimdeploy [flags] mail test|send|retry
nimdeploy [flags] pwpush [-views N] [-days N] [-note TEXT] < secret
nimdeploy [flags] status [-json] [deploy]
nimdeploy [flags] history [-n 20] [-json] [deploy]
nimdeploy [flags] nginx [-api]        print nginx location blocks for the hooks
nimdeploy [flags] woocommerce add|register|status|replay|note
```

| Flag | |
|---|---|
| `-config PATH` | config file (`$NIMDEPLOY_CONFIG`; default `/etc/nimdeploy/config.toml`, or, when not root, `~/.config/nimdeploy/config.toml` if it exists) |
| `-env-file PATH` | secrets file the CLI reads the API token from (default: `secrets.env` next to the config) |
| `-check` | validate the config and secrets, then exit |
| `-version` | print the version |

Flags go **before** the command, and command flags before the deploy name:
`nimdeploy -config /etc/x.toml run -f shop`.

`run`, `status` and `history` talk to the running service on its `listen`
address with the API token from `secrets.env`, so run them as the user who
can read that file (`sudo` for the root install, `sudo -iu deploy` for the
service-account install, yourself for a user install). Run without a command
in a terminal, nimdeploy prints this help instead of starting a server.

## status

```bash
nimdeploy status
nimdeploy status -json agency
```

```text
DEPLOY    STATUS               STARTED              DURATION  COMMIT   BY   LOG
agency    running (+1 queued)  2026-09-29 15:57:53  -         1111111  dev  20260929-155753-d11111.log
frontend  success              2026-09-29 13:10:02  1m12s     c93a11f  ana  20260929-131002-c93a11.log
```

## run

```bash
nimdeploy run agency                  # deploy the branch head
nimdeploy run -commit 9f1c2e7 agency  # a specific commit
nimdeploy run -f agency               # follow the log; exit code 1 if it fails
nimdeploy run -p SERVICE=api services # params, validated like the webhook's
```

Manual runs go through the same lock and queue as webhooks, don't wait for
CI, and appear as `manual` in the history. Use it after creating a missing
`.env`, to redeploy after fixing something on the server, or to retry.

## rollback

```bash
nimdeploy rollback -n shop         # show the commit it would deploy
nimdeploy rollback -f shop         # deploy the last good commit and follow the log
nimdeploy rollback -to 9f1c2e7 shop
```

See [Hooks, health checks and rollback](../guides/hooks-rollback.md#rollback).

## mail and pwpush

```bash
nimdeploy mail test -to you@example.com          # check [smtp]
nimdeploy mail send -to x@example.com -subject "…" -template t.html.hbs [-data file] [-secrets K]
nimdeploy mail retry [-n]                        # resend what failed (outbox)
printf '%s' "$secret" | nimdeploy pwpush -views 1   # print a Password Pusher link
```

See [Emails and secret links](../guides/email.md).

## send

```bash
NIMDEPLOY_SECRET=... nimdeploy send -data '{"service":"api"}' https://example.com/hooks/services
```

Signs (or adds the token to) a JSON body and posts it to a
[generic webhook](../guides/generic.md#nimdeploy-send). Useful from CI jobs,
Ansible or another server; it reads no config.

## woocommerce

```bash
nimdeploy woocommerce add -store https://shop.example.com -url https://deploy.example.com -command ./provision.sh
nimdeploy woocommerce register orders            # create/update the shop's webhooks
nimdeploy woocommerce status orders [-enable]    # are they active? reactivate
nimdeploy woocommerce replay orders 1234 | -status processing [-after 2026-10-01]
nimdeploy woocommerce note orders [-status completed] [-customer] 1234 "text"
```

See [Deploy from a WooCommerce sale](../guides/woocommerce.md).

## history

```bash
nimdeploy history                     # all deploys, newest first
nimdeploy history -n 50 agency
```

```text
DEPLOY  STATUS   STARTED              DURATION  TRIGGER  COMMIT   BY         LOG                         NOTE
lotes   skipped  2026-09-30 10:12:03  2m10s     webhook  a41f09c  ana        20260930-101203-7f3a21.log  CI failed: linter=success tests=failure
lotes   success  2026-09-29 15:54:23  50s       webhook  05e8108  aitorroma  20260929-155423-09c5ee.log
lotes   success  2026-09-29 15:49:13  50s       manual   -        root       20260929-154913-c49e52.log
```

Rebuilt from the log files themselves, so it survives restarts and covers
every run whose log is still kept (`logging.retain`).

## nginx

```bash
nimdeploy nginx > /etc/nginx/snippets/nimdeploy.conf
nimdeploy nginx -api
```

Prints one exact `location` per hook path (plus `/status` and `/deploy` with
`-api`), with the right upstream, socket, prefix and body size.
