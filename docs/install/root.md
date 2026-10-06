# Install as root

The classic system install, for your own server.

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sudo sh
```

or from a release archive: `sudo ./install.sh`.

It installs:

| | |
|---|---|
| binary | `/usr/local/bin/nimdeploy` |
| config | `/etc/nimdeploy/config.toml` (example, never overwritten if it exists) |
| secrets | `/etc/nimdeploy/secrets.env` (template; `NIMDEPLOY_API_TOKEN` generated if missing) |
| logs | `/var/log/nimdeploy/<deploy>/` |
| unit | `/etc/systemd/system/nimdeploy.service`, enabled |

Deploys run as the **`deploy`** user, created if missing (system user, home
`/var/lib/deploy`, no login shell). Use another user, for example the owner of
your pm2 apps or a HestiaCP user:

```bash
sudo SERVICE_USER=www-data ./install.sh
```

The user must own the working directories of your deploys.

!!! note "The service starts only when it's configured"
    On a fresh install the service is enabled but **not started** while
    `secrets.env` still contains `change-me` placeholders. Fill in the secrets,
    then `sudo systemctl start nimdeploy`. On upgrades it is restarted.

## Configure

```bash
sudo editor /etc/nimdeploy/config.toml        # your deploys
sudo editor /etc/nimdeploy/secrets.env        # one NAME=value per secret_env
sudo nimdeploy -check                          # validate config and secrets
sudo systemctl start nimdeploy
```

Generate webhook secrets with `openssl rand -hex 32`. See
[Configuration](../reference/configuration.md) for every option.

```bash
sudo systemctl reload nimdeploy    # apply config.toml changes; running deploys continue
sudo systemctl restart nimdeploy   # needed after editing secrets.env, listen or logging.directory
```

The unit runs `nimdeploy -check` before starting and before every reload, so a
broken config never replaces a working one.

## HestiaCP

On a HestiaCP server the installer can also publish the hook paths on one of
its web domains: see [HestiaCP](../proxy/hestiacp.md).
