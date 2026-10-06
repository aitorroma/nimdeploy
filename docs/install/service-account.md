# Service account + operator

The setup for servers run by a sysadmin team, where the people who deploy are
**not** root:

- nimdeploy (and pm2) run as **system units** that start at boot, but as a
  **service account** such as `deploy`, never as root;
- a **named operator** (e.g. `aitor`) controls them with their own SSH key and
  a minimal sudoers rule, so every action is traceable to a person and nobody
  shares the service account's credentials.

The admin runs one script, once, and can read everything it will do first.

## 1. Admin: run `setup-root.sh`

```bash
curl -fsSLO https://raw.githubusercontent.com/aitorroma/nimdeploy/main/contrib/setup-root.sh
less setup-root.sh
sudo bash setup-root.sh --dry-run          # prints every command and file, changes nothing
sudo bash setup-root.sh --service-user deploy --admin-user aitor \
     --app-dir /var/www/frontend --app-dir /var/www/backend
```

It is idempotent (existing config and secrets are kept) and does:

1. installs the nimdeploy binary in `~deploy/.local/bin`, downloaded from the
   release and **checked against `checksums.txt`**;
2. creates `~deploy/.config/nimdeploy/{config.toml,secrets.env}` owned by
   `deploy` (`640` / `600`), with logs in `/var/log/nimdeploy`;
3. installs pm2 for `deploy` in `~deploy/.local` (or reuses the account's own
   pm2 if it already has one); skip with `--no-pm2`;
4. installs and enables **`nimdeploy.service`** and **`pm2-deploy.service`**,
   both with `User=deploy`, `Restart=on-failure`, started at boot. It never
   overwrites units it didn't create;
5. writes **`/etc/sudoers.d/nimdeploy-aitor`** (validated with `visudo`);
6. adds `aitor` to the **`systemd-journal`** group;
7. creates the `--app-dir` directories owned by `deploy` if missing;
8. checks whether nginx already has a `/hooks/` location (it never edits nginx).

| Option | Default | |
|---|---|---|
| `--service-user` | `deploy` | the account that runs everything |
| `--admin-user` | `aitor` | the named operator |
| `--app-dir DIR` | `/var/www/frontend`, `/var/www/backend` | repeat it; created and owned by the service user |
| `--version vX.Y.Z` | latest | |
| `--binary PATH` | – | use a local binary instead of downloading |
| `--nopasswd` | off | sudo rules without password, for operators who log in with SSH keys only |
| `--no-pm2` | off | |
| `--dry-run` | off | |
| `--uninstall` | – | removes the units and the sudoers file; keeps the account's files and logs |

## What the operator can do

The generated sudoers file, in full:

```text
# Created by nimdeploy setup-root.sh. aitor operates the service account
# deploy with their own nominal user: sudo logs every command as aitor.
# No service runs files owned by deploy as root, so acting as deploy
# gives no root. As root, only these units can be controlled.
aitor ALL=(deploy) NOPASSWD: ALL
Cmnd_Alias NIMDEPLOY_SVC_AITOR = /usr/bin/systemctl start nimdeploy.service, /usr/bin/systemctl start nimdeploy, …
aitor ALL=(root) NOPASSWD: NIMDEPLOY_SVC_AITOR
```

The alias lists `start`, `stop`, `restart` and `reload` of `nimdeploy` and
`pm2-deploy`, with and without `.service`. (`NOPASSWD:` only with `--nopasswd`.)

| Operator wants to… | Command |
|---|---|
| work as the service account (git, composer, npm, pm2, scripts) | `sudo -iu deploy` |
| see deploys | `sudo -iu deploy nimdeploy status` / `history` |
| deploy by hand | `sudo -iu deploy nimdeploy run -f <deploy>` |
| apply config changes | `sudo systemctl restart nimdeploy` |
| restart the apps' pm2 | `sudo systemctl restart pm2-deploy` |
| read service logs | `journalctl -u nimdeploy -f` (no sudo) |
| read a deploy log | `sudo -iu deploy tail -n 100 /var/log/nimdeploy/<deploy>/latest.log` |

!!! warning "Why no `sudo systemctl status` or `sudo journalctl`"
    Both open a pager (`less`) running as root, and `!sh` inside the pager is
    a root shell. Membership of `systemd-journal` gives the same read access
    without that hole. The same applies to `systemctl edit` (opens an editor).

!!! danger "The one rule that keeps this safe"
    No root service, cron job or timer may ever execute a file owned by the
    service account. If one did, anyone who controls `deploy` would get root.
    `setup-root.sh` follows this rule (the units run as `deploy`); keep it when
    you add anything later.

## 2. Operator: add the apps

Each app gets its deploy logic in a script under `/home/deploy/bin`, then is
registered:

```bash
sudo -iu deploy nimdeploy install --system-service \
     --repo acme/shop --dir /var/www/shop --command /home/deploy/bin/deploy-shop.sh
sudo systemctl restart nimdeploy
```

Use the script's **full path**: through `sudo -i`, `$VARIABLES` and `~` in
`--command` would be expanded by your shell, not the service's.
`--system-service` only writes the config and the secret (no user unit). The
output shows the webhook URL and secret, and the nginx block for the admins.

For repositories that are private, give `deploy` a read-only **deploy key**
per repository (`~deploy/.ssh/`, with a `Host` alias per repo in
`~deploy/.ssh/config`) and use that alias in the clone URL, e.g.
`git@github-shop:acme/shop.git`.

## Tested on

Amazon Linux 2023 with systemd: dry run, install, rerun, deploys through
signed webhooks, sudo restrictions, reboot, uninstall.
