# Quick start

From nothing to "a push deploys my app" in five steps. This uses the
[no-root install](install/user.md); for a system service run by a service
account see [the other install options](install/index.md).

You need a Linux server with systemd, a domain served by a reverse proxy
(nginx, Caddy, Traefik…) with HTTPS, and a repository on GitHub, Gitea,
Forgejo, GitLab or Bitbucket.

## 1. Write the deploy script

nimdeploy runs a command in a directory; everything specific to your app goes
in that command. Start with a script in the repository or on the server:

```bash title="/srv/shop/deploy.sh"
#!/usr/bin/env bash
set -euxo pipefail              # print each step, stop at the first error
git fetch origin
git reset --hard "${DEPLOY_COMMIT:-origin/$DEPLOY_BRANCH}"   # the exact pushed commit
npm ci
npm run build
pm2 reload shop --update-env
```

!!! tip "Use a ready-made script"
    For Nuxt and Laravel, use the scripts shipped with each release instead:
    [Nuxt](guides/nuxt.md), [Laravel](guides/laravel.md).

## 2. Install and register the repository

As the user that owns the app (no sudo):

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh -s -- \
  --repo acme/shop --dir /srv/shop --command ./deploy.sh
```

This puts the binary in `~/.local/bin`, writes `~/.config/nimdeploy/config.toml`
with a `shop` deploy, generates its webhook secret and an API token, starts a
systemd user service listening on `127.0.0.1:9000`, and prints what is left:

```text
Webhook for acme/shop (github, branch main):
  URL      https://<the domain nginx serves>/hooks/shop
  Secret   892082e5cd30...
  Where    Settings → Webhooks → Add webhook; content type application/json; Just the push event

nginx: add this inside the site's server { } block (it is also printed by "nimdeploy nginx"):

    location = /hooks/shop {
        proxy_pass http://127.0.0.1:9000;
        ...
    }
```

Options: `--provider gitlab|gitea|forgejo|bitbucket` (default `github`),
`--branch` (default `main`), `--name` (default: repository name),
`--listen` (default `127.0.0.1:9000`). Run it again with another `--repo` to
add a second deploy; the existing secrets are kept.

## 3. Publish `/hooks/` on your proxy

Paste the printed block into your site's `server {}` (or ask whoever manages
nginx), then `nginx -t && systemctl reload nginx`. Only `/hooks/...` needs to be
public. Details and other proxies: [Reverse proxy](proxy/index.md).

## 4. Create the webhook

In the repository: **Settings → Webhooks → Add webhook**

- Payload URL: the printed URL, e.g. `https://example.com/hooks/shop`
- Content type: `application/json`
- Secret: the printed secret
- Events: *Just the push event*

GitHub sends a `ping` right away; nimdeploy answers `200`. A `401` means the
secret doesn't match. Other providers: [Git providers](guides/providers.md).

## 5. Push and watch

```bash
git push origin main
```

```bash
nimdeploy status                 # what each deploy is doing
nimdeploy history shop           # past deploys with their log files
journalctl --user -u nimdeploy -f
tail -f ~/.local/state/nimdeploy/shop/latest.log
nimdeploy run -f shop            # deploy again by hand and follow the log
```

## Next

- [How it works](how-it-works.md): queue, locking, what the script receives.
- [Configuration](reference/configuration.md): every option.
- [Security model](reference/security.md): what is protected and how.
