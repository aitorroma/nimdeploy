# Reverse proxy

nimdeploy speaks plain HTTP and is meant to sit behind nginx, Caddy, Traefik,
Cloudflare Tunnel or similar, which handles TLS. Keep `listen` local
(`127.0.0.1:9000` or a unix socket) and publish only what you need:

| Path | Publish? |
|---|---|
| `/hooks/...` | **yes**, the git host has to reach it. Protected by the signature |
| `/status`, `/history/...`, `/deploy/...` | only with `api_token_env` set, and only if you need them from outside the server |
| `/metrics` | only to your monitoring, and with `api_token_env` set |
| `/healthz` | for the proxy's health checks, if any |

## Things the proxy has to get right

- **Body size.** GitHub payloads go up to 25 MB; nginx rejects anything over
  1 MB by default: set `client_max_body_size 25m`.
- **Client IP.** Send `X-Forwarded-For` (or `X-Real-IP`). nimdeploy believes
  it only from `trusted_proxies` (default: loopback) or the unix socket, and
  reads it right to left, so entries added by the client are ignored. The IP
  shows up in the journal: `http POST /hooks/agency status=202 client=140.82.115.3 ...`.
- **Prefix.** If the proxy strips `/nimdeploy` nothing is needed; if it
  forwards `/nimdeploy/hooks/agency` as is, set `base_path = "/nimdeploy"`.
- **Timeouts.** None needed: webhooks are answered right away (`202`) and the
  deploy runs in the background.

## Generate the config

`nimdeploy nginx` prints the nginx blocks for your current config, one exact
`location` per hook path, with the right upstream, socket, prefix and body
size:

```bash
nimdeploy nginx              # hooks only
nimdeploy nginx -api         # also /status and /deploy
```

## Unix socket

```toml
[server]
listen = "unix:/run/nimdeploy/nimdeploy.sock"
socket_mode = "0660"
```

The root install's unit creates `/run/nimdeploy/`. The socket is `0666` by
default, the same exposure as a TCP port on localhost; with `0660` add the
proxy's user to the service user's group (`usermod -aG deploy nginx`). The
CLI uses the socket automatically, and requests through it are always
trusted for the client IP.

If `listen` is not local and no API token is configured, nimdeploy warns at
startup that `/status` is open.

## Guides

- [nginx](nginx.md)
- [HestiaCP](hestiacp.md)
- [Caddy](caddy.md)
- [Traefik](traefik.md)
- [Cloudflare](cloudflare.md) (proxied DNS and Tunnel)
