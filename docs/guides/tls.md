# TLS and client certificates

Behind a [reverse proxy](../proxy/index.md), the proxy handles HTTPS and
nimdeploy listens on `127.0.0.1` or a unix socket: that stays the simplest
setup. Native TLS is for when there is no proxy, or when the traffic between
hosts must be encrypted end to end. Mutual TLS (client certificates) is for
knowing **which machine** is calling, on top of the webhook's signature.

## HTTPS

```toml
[server]
listen = "0.0.0.0:9443"
tls_cert_file = "/etc/nimdeploy/tls/server.pem"   # with the intermediates
tls_key_file = "/etc/nimdeploy/tls/server.key"
tls_min_version = "1.2"                           # or "1.3"
```

- The files are **re-read when they change** (checked every few seconds).
  Renewals (certbot, cert-manager, an internal CA) need no restart. If the new
  files are broken, nimdeploy keeps the previous certificate and logs it.
- Changing the file paths themselves needs a restart.
- HTTP/2 is offered; the ciphers are Go's defaults (no weak suites).

## Client certificates (mTLS)

```toml
[server]
tls_cert_file = "/etc/nimdeploy/tls/server.pem"
tls_key_file = "/etc/nimdeploy/tls/server.key"
tls_client_ca_file = "/etc/nimdeploy/tls/clients-ca.pem"
tls_client_auth = "optional"          # or "require"
api_client_names = ["ops-laptop", "ci.example.com"]

[deploy.infra]
client_names = ["awx.example.com"]    # only AWX may call this webhook
```

| | |
|---|---|
| `tls_client_auth = "optional"` | a certificate is verified when the client sends one; without it the connection works. The checks below decide per route |
| `tls_client_auth = "require"` | no valid client certificate, no connection, for every route including `/healthz` |
| `client_names` (deploy) | its webhook only accepts a verified certificate whose CN or a SAN (DNS, email, URI) is one of these. Others get `403`. The signature or token is still checked |
| `api_client_names` (server) | the same for `/status`, `/history`, `/deploy`, `/rollback`, `/metrics` and `/debug/pprof`, on top of the token |

Certificates from another CA are refused during the handshake.

```bash
# from AWX/Ansible, with its certificate
curl --cert awx.pem --key awx.key --cacert server-ca.pem \
  -H "X-Signature: sha256=..." -d @payload.json https://deploy.example.com:9443/hooks/infra
```

The [Ansible collection](ansible.md#the-ansible-collection) takes
`client_cert`, `client_key` and `ca_path`.

## Hub and agents

The hub can serve HTTPS itself and require each agent to present a
certificate **with its own name**:

```bash
nimdeploy hub serve \
  -tls-cert /tls/hub.pem -tls-key /tls/hub.key \
  -tls-client-ca /tls/agents-ca.pem -agent-certs require
# or NIMDEPLOY_HUB_TLS_CERT, NIMDEPLOY_HUB_TLS_KEY, NIMDEPLOY_HUB_TLS_CLIENT_CA, NIMDEPLOY_HUB_AGENT_CERTS
```

- `-agent-certs require`: events are accepted only with a certificate whose
  CN or SAN is the agent's name (`X-Nimdeploy-Agent`), besides its token and
  signature. A stolen token alone is not enough.
- `-agent-certs optional` (default): a certificate is checked when sent.
- The dashboard and `/healthz` don't need a client certificate.

On each agent:

```toml
[hub]
url = "https://hub.example.com:9443"
agent = "web-1"
token_env = "NIMDEPLOY_HUB_TOKEN"
ca_file = "/etc/nimdeploy/tls/hub-ca.pem"   # the hub's CA, if it is private
cert_file = "/etc/nimdeploy/tls/web-1.pem"  # CN=web-1
key_file = "/etc/nimdeploy/tls/web-1.key"
```

The agent re-reads its certificate when it changes too.

With an Ingress or load balancer in front of the hub, mTLS needs TLS
passthrough (the hub must see the client certificate), or terminate it at the
hub with a `LoadBalancer`/`NodePort` Service. See the [Helm values](hub.md#run-the-hub-on-kubernetes-helm).

## A small CA

For a private CA without extra software, `openssl` is enough:

```bash
openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout ca.key -out ca.pem -days 3650 -subj "/CN=nimdeploy clients"

# a client certificate: CN is the name client_names/agents check
openssl req -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout web-1.key -out web-1.csr -subj "/CN=web-1"
openssl x509 -req -in web-1.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -days 365 -out web-1.pem -extfile <(printf "extendedKeyUsage=clientAuth\nsubjectAltName=DNS:web-1")
```

Keep `ca.key` off the servers. Monitor expiry with the certificate files'
dates or your usual tooling; nimdeploy picks up the renewed files on its own.
