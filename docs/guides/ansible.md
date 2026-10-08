# Ansible

nimdeploy and Ansible meet in three ways:

| | |
|---|---|
| **nimdeploy runs Ansible** | `[deploy.<name>.ansible]` runs `ansible-playbook` (or `ansible-pull`) when a webhook, a schedule or `nimdeploy run` asks for it. No wrapper script |
| **Ansible calls nimdeploy** | the `aitorroma.nimdeploy` collection starts deploys, waits for their result, sends signed webhooks and reads their state |
| **Pull on each host** | `mode = "pull"` with a `schedule`: each server fetches its playbooks from git and applies them, with nimdeploy's logs, locks, notifications and metrics |

Compared with Event-Driven Ansible (rulebooks), nimdeploy covers the common
case with less: authenticated webhooks, [rules](generic.md#operators) on their
JSON, validated params, queues, and a playbook as the action.

## Run a playbook

```toml
[deploy.infra]
provider = "generic"
path = "/hooks/infra"
secret_env = "INFRA_WEBHOOK_SECRET"
working_directory = "/srv/ansible"        # where the playbooks are
timeout = "1h"

[deploy.infra.ansible]
playbook = "site.yml"
inventory = ["inventories/production"]   # files, directories or plugin configs (aws_ec2.yml...)
limit = "${GROUP}"                       # from a param, validated below
tags = ["deploy"]
extra_vars = { app_version = "${VERSION}", notify = true }

[deploy.infra.params.GROUP]
from = "group"
enum = ["web", "workers", "db"]
[deploy.infra.params.VERSION]
from = "version"
match = "^[0-9]+\\.[0-9]+\\.[0-9]+$"
```

```bash
nimdeploy send -data '{"group":"web","version":"1.4.0"}' https://deploy.example.com/hooks/infra
```

nimdeploy runs, without a shell:

```text
ansible-playbook --inventory inventories/production --limit web --tags deploy --extra-vars @<file> site.yml
```

- **Extra vars** go in a temporary file (mode `600`, deleted after the run),
  never on the command line. They hold `extra_vars`, every param lowercased
  (`version`, `group`; turn off with `params_as_vars = false`) and a
  `nimdeploy` dict:

    ```yaml
    nimdeploy:
      deploy: infra
      run: 20261008-101500-a1b2c3     # the log file, without .log
      trigger: webhook                # webhook, manual, schedule, rollback
      commit: ...                     # git deploys
      delivery: ...
      labels: {client: Acme, environment: production}
      params: {GROUP: web, VERSION: 1.4.0}
    ```

- **`${PARAM}`** works in `limit`, `tags`, `skip_tags`, `extra_vars` and
  `checkout`. Only declared params, already validated by `enum`/`match`.
- **An empty limit never runs.** If `limit` uses a param that has no value,
  the deploy fails instead of running on every host.
- **The output is plain**: `ANSIBLE_NOCOLOR=1`, no retry files,
  `ANSIBLE_CONFIG` from `config`. The rest of the environment is the deploy's,
  without nimdeploy's secrets.

### The result

The `PLAY RECAP` is summarised:

- in `nimdeploy status -json` / `/status` (`"ansible": {"hosts": 12, "hosts_failed": 1, "failed_hosts": ["web3"], ...}`)
  and in the [hub](hub.md)'s deploy page;
- in the notification: `ansible 12 hosts: ok=84 changed=6 unreachable=0 failed=1 ...`;
- as the error: `ansible: failed on web3; unreachable: db2`;
- in [metrics](metrics.md): `nimdeploy_ansible_hosts_total{deploy,result}` and
  `nimdeploy_ansible_last_run_hosts{deploy,result}` (`ok`, `changed`, `unreachable`, `failed`).

The playbook's exit code decides success, as with any command. Hooks,
`health_url`, notifications, emails and the hub work the same as for a
script.

### Options

| Key | Default | |
|---|---|---|
| `mode` | `playbook` | or `pull` |
| `playbook` | required (`playbook`) | relative to `working_directory`; with `pull`, to the checkout (if empty, ansible-pull looks for `<fqdn>.yml`, `<hostname>.yml`, then `local.yml`) |
| `inventory` | – | list of `-i` |
| `limit` | – | `--limit`; may use `${PARAM}` |
| `tags`, `skip_tags` | – | lists |
| `extra_vars` | – | table; values may be nested tables and lists, strings may use `${PARAM}` |
| `params_as_vars` | `true` | pass every param as an extra var, lowercased |
| `check`, `diff` | `false` | `--check`, `--diff` |
| `forks` | – | `--forks` |
| `verbosity` | `0` | `-v` … `-vvvv` |
| `config` | – | `ANSIBLE_CONFIG` |
| `vault_password_file` | – | passed as `--vault-password-file`; nimdeploy never reads it |
| `binary` | `ansible-playbook` / `ansible-pull` | full path if it isn't in `PATH` (e.g. a virtualenv) |
| `args` | – | more arguments, as given |
| `url`, `checkout`, `directory`, `only_if_changed` | – | `mode = "pull"` |

`command` and `[ansible]` are exclusive. Everything else of the deploy
(`timeout`, `env`, `queue_key`, `when`, hooks, `email`...) applies.

## ansible-pull on each host

```toml
[deploy.config]
schedule = "*/30 * * * *"
timeout = "20m"

[deploy.config.ansible]
mode = "pull"
url = "https://git.example.com/infra/playbooks.git"
checkout = "main"
directory = "/var/lib/nimdeploy/playbooks"
only_if_changed = true          # run only when the repository changed
playbook = "local.yml"
inventory = ["localhost,"]
```

nimdeploy adds what `ansible-pull` lacks on its own: one run at a time (the
deploy's lock), a log per run, history, notifications on failure, metrics and
the hub. It can also run on demand from a webhook (add a `path`), e.g. a push
to the playbooks repository.

The hub doesn't send orders to the agents: agents only make outgoing
requests, and that is deliberate. To run everywhere at once, give each server
a webhook or a schedule.

## The Ansible collection

`aitorroma.nimdeploy` is in each release
(`aitorroma-nimdeploy-<version>.tar.gz`):

```bash
ansible-galaxy collection install https://github.com/aitorroma/nimdeploy/releases/download/v0.9.0/aitorroma-nimdeploy-0.9.0.tar.gz
```

| Module | |
|---|---|
| `nimdeploy_run` | start a deploy (or `rollback: true`) through the API, wait for its result, fail the task if it fails |
| `nimdeploy_send` | send a signed webhook to a generic deploy |
| `nimdeploy_status` | read the deploys' state, filtered by `labels` |

```yaml
- name: Release
  hosts: localhost
  gather_facts: false
  tasks:
    - name: Deploy the API
      aitorroma.nimdeploy.nimdeploy_run:
        url: https://deploy.example.com
        api_token: "{{ vault_nimdeploy_api_token }}"
        deploy: api
        params:
          VERSION: "{{ version }}"
        delivery_id: "release-{{ version }}"   # retries never deploy twice
        wait_timeout: 1800
      register: api

    - ansible.builtin.debug:
        msg: "{{ api.state.status }} in {{ api.state.duration }}"

    - name: Every production deploy is green
      aitorroma.nimdeploy.nimdeploy_status:
        url: https://deploy.example.com
        api_token: "{{ vault_nimdeploy_api_token }}"
        labels: { environment: production }
      register: prod
    - ansible.builtin.assert:
        that: prod.deploys | dict2items | selectattr('value.status', 'equalto', 'failed') | list | length == 0
```

- **Idempotent**: `nimdeploy_run` sends a `delivery_id` (random by default). A
  retried task with the same id runs nothing (`result: duplicate`) and
  reports that run's result. Set it to something stable (a version, a job
  id) to make re-running the whole playbook safe too.
- **Check mode** changes nothing and reports `would_run`.
- **TLS**: `validate_certs`, `ca_path`, and `client_cert`/`client_key` for
  [mTLS](tls.md).
- The token can come from `$NIMDEPLOY_API_TOKEN`, the secret from
  `$NIMDEPLOY_SECRET`. Both are `no_log`.

## Traces

With [OpenTelemetry](observability.md) on, the playbook gets `TRACEPARENT` for
its span. Ansible's `community.general.opentelemetry` callback continues
that trace, so each task appears under the deploy in your tracing backend.
