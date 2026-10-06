# Development

```bash
git clone https://github.com/aitorroma/nimdeploy && cd nimdeploy
make test     # go test -race ./...
make lint     # gofmt, go vet, shellcheck on every shell script
make build    # static binary, version from git describe
```

Go 1.26+, no CGO, a single dependency (a TOML parser). The tests cover
signatures for every provider, the queue and lock, timeouts, the CLI, the
installer and the history.

## Layout

| File | |
|---|---|
| `main.go` | flags, commands, service entry point |
| `config.go` | config loading, defaults, validation |
| `server.go` | HTTP endpoints, client IP, API token |
| `providers.go` | per-provider signature checks and payload parsing |
| `generic.go` | generic webhooks: auth, `when`, params, JSON paths |
| `woocommerce.go` | WooCommerce webhooks, REST API client, `nimdeploy woocommerce` |
| `runner.go` | locking, queue, process groups, log files |
| `history.go` | history rebuilt from log files |
| `ci.go` | wait for GitHub Actions |
| `notify.go` | Slack, Discord, Telegram, JSON notifications, commit statuses |
| `install.go`, `quick.go` | `nimdeploy install` / `uninstall` |
| `cloudflare.go` | Cloudflare's IP ranges for `trusted_proxies = ["cloudflare"]` |
| `cli.go` | `run`, `status`, `history`, `nginx` |
| `install.sh` | root and user installer shipped in the release archive |
| `get.sh` | one-line installer published at `nimdeploy.nimbox360.com/install.sh` |
| `contrib/setup-root.sh` | service account + operator setup |
| `deploy/` | systemd units and example deploy scripts |
| `docs/`, `mkdocs.yml` | this site |

## Releases

Pushing a `v*` tag runs the tests, builds the static binaries for amd64 and
arm64, the archives (binary, installer, example config, units, deploy
scripts) and `checksums.txt`, and publishes the GitHub release.

## Documentation

The site is built with [MkDocs Material](https://squidfunk.github.io/mkdocs-material/)
and published to GitHub Pages on every push to `main` that touches `docs/`,
`mkdocs.yml` or `get.sh`.

```bash
pip install mkdocs-material        # or: uvx --with mkdocs-material mkdocs serve
mkdocs serve                       # http://127.0.0.1:8000, live reload
mkdocs build --strict              # what CI runs
```
