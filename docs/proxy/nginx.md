# nginx

Inside the site's `server { }` block:

```nginx
location ^~ /hooks/ {
    proxy_pass http://127.0.0.1:9000;      # or http://unix:/run/nimdeploy/nimdeploy.sock;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 25m;
}
```

Or let nimdeploy write it, with one exact location per configured hook:

```bash
sudo nimdeploy nginx > /etc/nginx/snippets/nimdeploy.conf    # then include it in server {}
sudo nginx -t && sudo systemctl reload nginx
```

!!! tip "Why `=` and `^~`"
    Exact (`=`) and `^~` locations win over the regex locations most sites
    have (`\.php$`, static files), so the hooks are never handled by PHP or by
    a static-file rule. Use them for every app path when several apps share a
    domain, and avoid regex locations there.

## A dedicated domain

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name deploy.example.com;
    ssl_certificate     /etc/letsencrypt/live/deploy.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/deploy.example.com/privkey.pem;

    location ^~ /hooks/ {
        proxy_pass http://127.0.0.1:9000;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP $remote_addr;
        client_max_body_size 25m;
    }
    location / { return 404; }
}
```

## Under a subpath, stripping the prefix

Note the trailing `/` in both lines:

```nginx
location /nimdeploy/ {
    proxy_pass http://127.0.0.1:9000/;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    client_max_body_size 25m;
}
```

The payload URL is then `https://example.com/nimdeploy/hooks/agency`.

## Real-world example: several apps on one domain

From a production-like stage server: a corporate Nuxt site at `/`, a Laravel
API and admin at `/api` and `/admin`, and a second product under `/agency`
with its own Nuxt frontend and a Laravel backend on PHP-FPM, all deployed by
nimdeploy.

```text
/agency/api, /agency/admin      -> PHP-FPM pool "laravel" (Laravel under /agency)
/agency/storage/, /agency/css/  -> static files of that Laravel
/hooks, /hooks/                 -> 127.0.0.1:9000 (nimdeploy)
/agency, /agency/               -> 127.0.0.1:3001 (Nuxt, app.baseURL=/agency/)
/api, /admin                    -> 127.0.0.1:8000 (corporate backend)
/                               -> 127.0.0.1:3000 (corporate Nuxt)
```

nginx always picks the longest matching prefix, so the order in the file
doesn't matter. Each path is declared twice, exact and with a trailing slash
(`location = /agency/api` and `location ^~ /agency/api/`). Two details that
bite:

- `add_header` in a `location` stops that location from inheriting the
  server-level headers: keep security headers at `server` level only.
- `$connection_upgrade` (for WebSocket) needs a `map` in the `http` context,
  declared once, e.g. in `conf.d/00-connection-upgrade.conf`:

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
```

The Laravel-under-a-prefix part is explained in
[Laravel → PHP-FPM](../guides/laravel.md#nginx).
