# Install without root

Any normal user can run nimdeploy for their own apps, as a systemd **user**
service. Deploys run as you, so they can only touch what you can.

## In one command

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh -s -- \
  --repo acme/shop --dir /srv/shop --command ./deploy.sh
```

or, with the binary already downloaded:

```bash
curl -fsSLo nimdeploy https://github.com/aitorroma/nimdeploy/releases/latest/download/nimdeploy_linux_amd64
chmod +x nimdeploy
./nimdeploy install --repo acme/shop --dir /srv/shop --command ./deploy.sh
```

It copies itself to `~/.local/bin`, writes the config with that deploy,
generates the webhook secret and the API token, starts the user service and
ends with the webhook URL and secret, the nginx block for your proxy admin,
and the next steps.

| Option | Default | |
|---|---|---|
| `--repo` | – | `owner/repo` as the provider names it. Without it, an example config is installed to edit by hand |
| `--dir` | – | working directory of the command (the checkout) |
| `--command` | – | run with `bash -eo pipefail -c`: a script or a chain like `git pull && npm ci && npm run build` |
| `--provider` | `github` | `gitea`, `forgejo`, `gitlab`, `bitbucket` |
| `--branch` | `main` | |
| `--name` | repository name | deploy name: hook path `/hooks/<name>`, log directory |
| `--listen` | `127.0.0.1:9000` | use a port above 1024 |

Running it again keeps existing secrets; with another `--repo` it adds a
second deploy. If the resulting config doesn't validate, nothing is changed.

## Files

| | |
|---|---|
| binary | `~/.local/bin/nimdeploy` |
| config, secrets | `~/.config/nimdeploy/config.toml`, `secrets.env` (`600`) |
| deploy logs | `~/.local/state/nimdeploy/<deploy>/` |
| service | `~/.config/systemd/user/nimdeploy.service` |

```bash
systemctl --user status nimdeploy
systemctl --user reload nimdeploy       # after editing config.toml
systemctl --user restart nimdeploy      # after editing secrets.env
journalctl --user -u nimdeploy -f
nimdeploy status                        # finds ~/.config/nimdeploy by itself
```

## Lingering: keep it running after you log out

User services stop when your last session ends and don't start at boot
unless *lingering* is enabled. The installer runs `loginctl enable-linger`;
where the system doesn't allow that to normal users it says so, and an
administrator has to run once:

```bash
sudo loginctl enable-linger <user>
```

## Step by step

`./install.sh` from the release archive, run without root, does the same as
`nimdeploy install`. To do it by hand: put the binary in `~/.local/bin`, copy
`config.example.toml` to `~/.config/nimdeploy/config.toml`, create
`secrets.env` with one `NAME=value` per secret, copy
`deploy/nimdeploy-user.service` to `~/.config/systemd/user/`, then
`systemctl --user daemon-reload && systemctl --user enable --now nimdeploy`.

## Remove

```bash
nimdeploy uninstall            # keeps config, secrets and logs
nimdeploy uninstall --purge    # removes them too
```
