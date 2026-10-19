# Helm (Kubernetes)

`[deploy.<name>.helm]` runs `helm upgrade --install` for one or more
releases, **in order**, instead of a wrapper script. The typical case is a
product installed once per customer: the same chart, one release per
customer with its own values, all updated to the same version when a tag is
pushed. The first release is your own: if it breaks, nobody else gets it.

```toml
[deploy.proyecto]
provider = "forgejo"
path = "/hooks/proyecto"
repository = "org/proyecto"
tags = "v*"                                  # every production deploy is a version
secret_env = "PROYECTO_WEBHOOK_SECRET"
working_directory = "/srv/proyecto"          # a checkout of the repository
before = "git fetch --tags -q && git checkout -q $DEPLOY_TAG"
timeout = "30m"

[deploy.proyecto.helm]
chart = "deploy/helm/proyecto"               # relative to working_directory
kubeconfig = "/etc/rancher/k3s/k3s.yaml"
set = { "image.tag" = "${DEPLOY_TAG}" }      # the image built for that tag
timeout = "5m"                               # per release
on_failure = "stop"                          # or "rollback"

[[deploy.proyecto.helm.release]]             # first: the canary
name = "proyecto"
namespace = "proyecto"
values = ["deploy/helm/instances/proyecto.yaml"]

[[deploy.proyecto.helm.release]]
client = "cliente1"                          # shown with the outcome
name = "proyecto"
namespace = "proyecto-cliente1"
values = ["deploy/helm/instances/cliente1.yaml"]

[[deploy.proyecto.helm.release]]
client = "cliente2"
name = "proyecto"
namespace = "proyecto-cliente2"
values = ["deploy/helm/instances/cliente2.yaml"]
```

For each release nimdeploy runs, without a shell:

```text
helm upgrade --install proyecto deploy/helm/proyecto --namespace proyecto \
  --kubeconfig /etc/rancher/k3s/k3s.yaml --wait --timeout 5m0s --atomic \
  --values deploy/helm/instances/proyecto.yaml --set-string image.tag=v1.4.0
```

## What happens when one fails

- `--wait` makes a release count only when its pods are ready, and
  `--atomic` makes helm undo a release that does not get there: the failed
  release is back on the revision it had.
- **The run stops there.** The releases after it are not touched (`skipped`
  in the state): one broken version never reaches every customer.
- With `on_failure = "rollback"`, the releases this run already upgraded go
  back to their previous revision too (`helm rollback <release> <revision>`),
  newest first. Releases this run installed for the first time stay.
- The deploy fails with the helm error (`helm: cliente1 (proyecto-cliente1/proyecto):
  UPGRADE FAILED: ...`), and `after_failure` and the notifications run as
  usual.

## How nimdeploy reaches the cluster

nimdeploy doesn't talk to Kubernetes itself: it runs `helm`, and helm talks
to the cluster's API server with a kubeconfig. Either:

- run nimdeploy on a machine that already administers the cluster and point
  `kubeconfig` (or `KUBECONFIG` in `env`) at its file, or
- run it in the cluster with a ServiceAccount that may manage the releases'
  namespaces (helm uses the in-cluster credentials when there is no
  kubeconfig). Give it a Role per namespace, not `cluster-admin`.

`kube_context` picks a context when the kubeconfig has several.

## Values

- `values` (shared) are passed before each release's own `values`.
- `set` (shared, then the release's) becomes `--set-string key=value`, so
  `image.tag = "1.10"` stays the string `1.10`. Values may use the deploy's
  params (`${VERSION}`) and `${DEPLOY_TAG}`, `${DEPLOY_COMMIT}`,
  `${DEPLOY_BRANCH}`; commas are escaped so a value is never split in two.
- A `set` that comes out **empty** stops the run before helm starts: an empty
  `image.tag` would silently mean "whatever the chart defaults to".
- Secrets don't belong in `set` (they would be in the log and in helm's
  release history): keep them in Kubernetes Secrets the chart references.

## Every customer, without a list

With a list, every new customer means editing the config and adding a values
file. `[discover]` instead finds the chart's releases in the cluster
(`helm list --all-namespaces`) and adds them after the listed ones, sorted
by namespace:

```toml
[deploy.proyecto.helm]
chart = "deploy/helm/proyecto"
set = { "image.tag" = "${DEPLOY_TAG}" }

[deploy.proyecto.helm.discover]
chart = "proyecto"           # releases of this chart (and/or filter = "^proyecto$")

[[deploy.proyecto.helm.release]]  # still first: the canary
client = "interno"
name = "proyecto"
namespace = "proyecto"
values = ["deploy/helm/instances/proyecto.yaml"]
```

A discovered release is upgraded with **the values it already has**
(`--reset-then-reuse-values`), plus the shared `values` and `set`: only
the version changes, and no values file is needed. Its values were stored by
helm when it was installed, e.g. by the provisioning deploy below. A listed
release can do the same with `reuse_values = true`.

`discover` needs `chart` or `filter`: it never takes every release in the
cluster. If it finds nothing and nothing is listed, the run fails.

## The customer as a variable

`client`, `name`, `namespace` and `values` of a release may use the deploy's
params, so one release can be whichever customer the webhook names. That is
a provisioning flow: a sale (see [Stripe, Paddle and Lemon
Squeezy](payments.md)) or any JSON webhook installs that customer's release,
with its values from the params (no values file), and from then on
`[discover]` updates it with the rest.

```toml
[deploy.alta]
provider = "generic"
path = "/hooks/alta"
secret_env = "ALTA_WEBHOOK_SECRET"
working_directory = "/srv/proyecto"

[deploy.alta.params.CLIENTE]
from = "customer.slug"
match = "^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$"

[deploy.alta.helm]
chart = "deploy/helm/proyecto"
create_namespace = true
set = { "image.tag" = "v1.4.0", "tunnel.name" = "${CLIENTE}" }

[[deploy.alta.helm.release]]
client = "${CLIENTE}"
name = "proyecto"
namespace = "proyecto-${CLIENTE}"
```

```bash
nimdeploy send -data '{"customer":{"slug":"cliente1"}}' https://deploy.example.com/hooks/alta
```

The result, the notification and the hub say `cliente1
(proyecto-cliente1/proyecto)`. Whatever the param holds, the release name
and namespace must still be lowercase letters, digits and `-` (DNS labels),
a values file from a param can't contain `..`, and two releases can't end up
the same: otherwise the run fails before helm starts. Validate the param
itself too (`match`), as above.

## Options

| Key | Default | |
|---|---|---|
| `chart` | required | a path (relative to `working_directory`), `repo/chart` or `oci://...` |
| `version` | – | chart version, for repository charts |
| `values` | – | values files for every release |
| `set` | – | `--set-string` for every release, with `${PARAM}` and `${DEPLOY_TAG}`/`${DEPLOY_COMMIT}`/`${DEPLOY_BRANCH}` |
| `kubeconfig`, `kube_context` | helm's | which cluster |
| `create_namespace` | `false` | `--create-namespace` |
| `timeout` | `5m` | per release (`--timeout`); the deploy's `timeout` bounds the whole run |
| `atomic` | `true` | `--atomic`: undo a release that doesn't become ready |
| `on_failure` | `stop` | `rollback`: also roll back the releases this run upgraded |
| `binary` | `helm` | |
| `args` | – | more arguments for every `helm upgrade`, as given |
| `discover` | – | table: `chart` and/or `filter` (regular expression on release names); the releases found are upgraded after the listed ones, with their own values |
| `[[release]]` | at least one, unless `discover` | `client` (who it is for, shown with the outcome), `name`, `namespace`, and its own `values` and `set`; all of them may use `${PARAM}`; `reuse_values = true` keeps the values it has |

## What you see

- **State and `/status`**: `helm.releases`, each with `client`, `release`,
  `namespace`, `outcome` (`upgraded`, `installed`, `failed`, `rolled_back`,
  `skipped`), `revision` and `previous`.
- **Notifications**: `helm 3 releases: 1 upgraded, failed: cliente1 (proyecto-cliente1/proyecto), 1 not reached`.
- **Hub**: one box per release on the deploy's page; the MCP server returns
  the same summary to assistants.
- **Metrics**: `nimdeploy_helm_releases_total{deploy,result}` and
  `nimdeploy_helm_last_run_releases{deploy,result}`; the helm phase is in
  `nimdeploy_phase_duration_seconds{phase="command"}`.
