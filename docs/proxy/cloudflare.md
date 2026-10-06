# Cloudflare

## Cloudflare Tunnel

Recommended when you can: no open ports on the server. `cloudflared` connects
from localhost, which is trusted by default:

```yaml
# /etc/cloudflared/config.yml
tunnel: <tunnel-id>
credentials-file: /etc/cloudflared/<tunnel-id>.json
ingress:
  - hostname: deploy.example.com
    path: ^/hooks/
    service: http://127.0.0.1:9000        # or unix:/run/nimdeploy/nimdeploy.sock
  - service: http_status:404
```

```toml
[server]
client_ip_header = "CF-Connecting-IP"
```

## Proxied DNS (orange cloud)

Cloudflare → nginx/Caddy → nimdeploy. The proxy appends Cloudflare's edge IP
to `X-Forwarded-For`, so trust Cloudflare's ranges too:

```toml
[server]
trusted_proxies = ["127.0.0.0/8", "::1", "cloudflare"]
client_ip_header = "CF-Connecting-IP"
```

`"cloudflare"` expands to Cloudflare's published IP ranges, built into the
binary. If they change (<https://www.cloudflare.com/ips/>), list the new ranges
explicitly until the next release.

Use SSL mode **Full (strict)** with an Origin certificate on the server, and
let the origin accept only Cloudflare's IPs (or use Authenticated Origin
Pulls); otherwise anyone reaching the origin directly can fake
`CF-Connecting-IP`. That only affects the logged IP: the webhook signature is
checked either way.

## Settings that break deliveries

Look for `403` or an HTML challenge in GitHub → Settings → Webhooks → Recent
Deliveries.

- **WAF, Super Bot Fight Mode, "I'm Under Attack", rate limiting:** the git
  host can't solve challenges. Add a WAF custom rule with the expression
  `starts_with(http.request.uri.path, "/hooks/")` and action **Skip**,
  selecting the features to skip.
- **Bot Fight Mode** (free plan) cannot be skipped per path: if it blocks the
  deliveries, turn it off for the zone or serve the hooks from another zone.
- **Cloudflare Access:** add a self-hosted application for
  `deploy.example.com/hooks/` with a **Bypass** policy for *Everyone*, or the
  git host gets the login page.

Payloads are well below Cloudflare's 100 MB request limit, and POST requests
are never cached.
