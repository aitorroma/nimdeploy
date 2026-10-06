# HestiaCP

The root installer publishes the hooks on an existing HestiaCP web domain
without touching its templates:

```bash
sudo ./install.sh hestia deploy.example.com            # owner found automatically
sudo ./install.sh hestia deploy.example.com admin --api
sudo ./install.sh hestia-remove deploy.example.com
```

It writes the output of `nimdeploy nginx` to
`/home/<user>/conf/web/<domain>/nginx.conf_nimdeploy` and
`nginx.ssl.conf_nimdeploy`, which HestiaCP's templates include inside the
domain's `server` block and keep when the domain is rebuilt. Then it runs
`nginx -t` (reverting on error), reloads nginx, prints the payload URLs and
sends a test POST that must get `401` from nimdeploy. It warns if the domain
has no SSL or uses a custom template without the `nginx.conf_*` include.
`install.sh uninstall` removes the files from every domain.

Works with nginx alone or nginx in front of Apache. The domain can be a site
that already exists (only the exact hook paths are taken) or a dedicated one
like `deploy.example.com`.

Run nimdeploy as the HestiaCP user that owns the sites you deploy
(`sudo SERVICE_USER=admin ./install.sh`): their code lives in
`/home/<user>/web/<domain>/`, which is the `working_directory` to use. Run
`./install.sh hestia` again after adding or renaming hook paths.
