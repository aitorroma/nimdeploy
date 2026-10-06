# Any app with pm2

pm2 runs **one daemon per user** (`$HOME/.pm2`), so `pm2 reload` only sees the
apps of the user that runs it. Three things make it work from nimdeploy:

1. **Run nimdeploy as the user that owns the pm2 apps.** With the root
   install: `sudo SERVICE_USER=www ./install.sh`; check with `sudo -u www pm2 ls`.
   With [setup-root.sh](../install/service-account.md) both run as the
   service account already.
2. **Start pm2 with its own systemd unit**, not from a deploy. Otherwise the
   first `pm2` call from a deploy spawns the daemon inside nimdeploy's cgroup,
   and stopping or restarting nimdeploy kills your apps.
    - setup-root.sh installs `pm2-deploy.service` (`User=deploy`,
      `pm2 resurrect` at boot);
    - otherwise: `pm2 startup systemd -u www --hp /home/www`, then `pm2 save`.
3. **Set `PATH`** when node or pm2 come from nvm, corepack or `~/.local/bin`:
   systemd services get a minimal `PATH`. Export it in the deploy script, or
   add it to the deploy's `env`:
   `env = ["PATH=/home/www/.nvm/versions/node/v22.11.0/bin:/usr/bin:/bin"]`.

Prefer `pm2 reload <app> --update-env`: zero downtime in cluster mode, same
as restart in fork mode. Run `pm2 save` after changes so a reboot brings the
same apps back.

## Example

[`deploy/examples/deploy-pm2.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/deploy-pm2.sh):

```bash
--8<-- "deploy/examples/deploy-pm2.sh"
```

For Nuxt with releases, health check and rollback, use
[deploy-nuxt.sh](nuxt.md) instead.
