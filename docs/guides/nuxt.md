# Nuxt (SSR) with pm2

[`deploy/examples/deploy-nuxt.sh`](https://github.com/aitorroma/nimdeploy/blob/main/deploy/examples/deploy-nuxt.sh)
deploys a Nuxt app with server-side rendering using **atomic releases**: the
new version is built next to the running one, switched in one step, checked,
and rolled back automatically if it doesn't answer. Visitors never see a
half-built site.

## Layout

```text
/var/www/frontend/agency/          APP_DIR
├── repo/                          git cache, fetched on each deploy
├── releases/
│   ├── 20261006-101540-c93a11f/   one build per deploy (code + node_modules + .output)
│   └── 20261005-173012-51af02d/   the last KEEP_RELEASES are kept
├── shared/.env                    app settings, used at build and at runtime
├── current -> releases/20261006-101540-c93a11f
└── ecosystem.config.cjs           pm2 definition, rewritten on each deploy
```

## What a deploy does

1. **Code:** fetch into `repo/`, extract exactly the pushed commit into a new
   `releases/<date>-<sha>/` with `git archive`, link `shared/.env` into it.
2. **Build, next to the live release:** install dependencies **with**
   devDependencies (the build needs them, so `NODE_ENV` is unset for the
   install) and build with `NODE_ENV=production`. pnpm, npm or yarn is chosen
   from the lockfile, always with the lockfile frozen. If install or build
   fails, the deploy stops here and the site is untouched.
3. **Switch:** `current` is replaced atomically (`ln` + `mv -T`), then
   `pm2 startOrReload` in **cluster mode**: workers are replaced one by one,
   open connections are not cut.
4. **Health check:** `GET http://HOST:PORT/BASE_URL` for up to 60 s; any 2xx or
   3xx is healthy. Otherwise `current` goes back to the previous release, pm2
   is reloaded again and the deploy fails.
5. **Clean up:** `pm2 save` (so it survives reboots) and old releases are
   deleted, never the one in use.

## Wrapper script

Keep the per-app settings in a tiny wrapper that nimdeploy runs:

```bash title="/home/deploy/bin/deploy-agency-frontend.sh"
#!/usr/bin/env bash
export PATH="$HOME/.local/bin:$PATH"           # pm2, pnpm via corepack
export APP_NAME=agency-frontend                # pm2 process name
export APP_DIR=/var/www/frontend/agency
export REPO_URL=git@github-agency-frontend:acme/agency-frontend.git
export PORT=3001
export BASE_URL=/agency/                       # Nuxt app.baseURL; also the health check path
export INSTANCES=1                             # pm2 cluster instances
exec /home/deploy/bin/deploy-nuxt.sh
```

| Variable | Default | |
|---|---|---|
| `APP_NAME` | required | pm2 process name |
| `APP_DIR` | required | holds `repo/`, `releases/`, `shared/`, `current` |
| `REPO_URL` | required | clone URL (SSH with a deploy key for private repos) |
| `PORT` | required | local port Nuxt listens on |
| `HOST` | `127.0.0.1` | |
| `BASE_URL` | `/` | `NUXT_APP_BASE_URL`, and the path the health check requests |
| `INSTANCES` | `1` | pm2 cluster instances |
| `KEEP_RELEASES` | `5` | releases kept for rollback |
| `BUILD_MEMORY_MB` | – | Node heap for the build (`--max-old-space-size`) |

Register it:

```bash
nimdeploy install --system-service --repo acme/agency-frontend \
  --name agency-frontend --dir /var/www/frontend/agency \
  --command /home/deploy/bin/deploy-agency-frontend.sh
```

## `shared/.env`

Created empty on the first deploy. It is loaded by bash before the build and
passed to pm2, so `NUXT_PUBLIC_*` values work both at build time and at
runtime. Because bash reads it, **quote values with spaces**:

```bash
NUXT_PUBLIC_API_BASE="https://example.com/agency/api"
NUXT_PUBLIC_APP_NAME="Acme Agency"
NUXT_SITE_INDEXABLE=false
```

## nginx

```nginx
location = /agency  { return 301 /agency/; }
location ^~ /agency/ {
    proxy_pass http://127.0.0.1:3001;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;   # map in the http context
}
```

## Requirements and gotchas

!!! warning "pnpm 10+ and build scripts"
    pnpm no longer runs dependencies' install scripts (esbuild, sharp…) unless
    they are approved, and the install fails with `ERR_PNPM_IGNORED_BUILDS`.
    Approve them **in the repository**: run `pnpm approve-builds` and commit
    the resulting `allowBuilds` in `pnpm-workspace.yaml` (pnpm 10 also reads
    `onlyBuiltDependencies`). Review that list like any change that runs code
    on the server.

- **Memory:** installing and building a Nuxt app peaks at 600–850 MB. On a
  1 GB server add swap (or set `BUILD_MEMORY_MB`), otherwise the build is
  killed by the OOM killer; 2 GB of RAM is comfortable.
- **pm2 must not live inside nimdeploy's cgroup:** start it with its own unit
  (`pm2-deploy.service` from [setup-root.sh](../install/service-account.md), or
  `pm2 startup`). See [pm2](pm2.md).
- **SSR build:** the script expects `.output/server/index.mjs` (Nitro's
  default `node-server` preset).

## Rollback by hand

```bash
cd /var/www/frontend/agency
ls releases/                                  # newest last
ln -sfn releases/<previous> current.next && mv -T current.next current
pm2 reload agency-frontend
```

Then revert the commit in git, or the next push deploys the broken code again.
