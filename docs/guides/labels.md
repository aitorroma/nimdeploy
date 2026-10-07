# Labels: client, environment…

Labels are free `key = "value"` pairs that say what a deploy is: whose it
is, which environment, which part of the product. nimdeploy doesn't give
them meaning, except `client` and `environment`, which make messages easier
to read. Set them for the whole server and override them per deploy:

```toml
[labels]                      # every deploy on this server
client = "Acme"
environment = "stage"

[deploy.shop.labels]           # this deploy only (merged over [labels])
component = "frontend"
url = "https://stage.acme.example"
```

Keys are lowercase letters, digits and `_` (up to 32 characters, starting
with a letter; `deploy` is reserved); values up to 200 characters. At most 32
labels.

## Where they show up

| | |
|---|---|
| Notifications | the title reads `Acme · stage · shop deploy FAILED`; with `environment = "production"` (or `prod`) it starts with `[PRODUCTION]`, and the `url` label is added as a link |
| Commit statuses | the context becomes `nimdeploy/<environment>/<deploy>`, so stage and production get separate checks on the same commit |
| The command | every label as `DEPLOY_LABEL_<KEY>`: `DEPLOY_LABEL_CLIENT`, `DEPLOY_LABEL_ENVIRONMENT`… |
| Emails | `{{Labels.client}}` in Handlebars templates |
| `/status`, history | a `labels` object on each deploy; filter with `/status?label=environment=production` |
| CLI | `nimdeploy status -wide` adds a LABELS column; `-l key=value` filters `status` and `history` (repeat it to require several) |
| `/metrics` | `nimdeploy_deploy_info{deploy="shop",client="Acme",environment="stage",…} 1`: join it with the other metrics on `deploy` |
| [Hub](hub.md) | the dashboard groups every server's deploys by client and environment |

```bash
nimdeploy status -wide -l client="Acme"
nimdeploy history -l environment=production -n 50
```

Labels come from the current config: change them and reload (`systemctl
reload nimdeploy`); past runs in the history show the new labels too.
