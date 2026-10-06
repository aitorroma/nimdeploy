# Install

nimdeploy is one static binary for Linux (amd64 and arm64) plus a systemd unit.
Pick the setup that matches who manages the server:

| You are… | Use | Root needed |
|---|---|---|
| a normal user deploying your own apps | [Without root](user.md) | no |
| an admin who wants a system service run by a service account, operated by a named person without root | [Service account + operator](service-account.md) | once, by the admin |
| root on your own server | [As root](root.md) | yes |

## One-line installer

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh          # as a normal user
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sudo sh     # as root
```

The script ([`get.sh`](https://github.com/aitorroma/nimdeploy/blob/main/get.sh)
in the repository) downloads the latest release for your architecture from
GitHub, **verifies it against the release's `checksums.txt`** and hands over
to the real installer: `nimdeploy install` for a normal user, the release's
`install.sh` for root. Arguments after `sh -s --` are passed through:

```bash
curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh -s -- \
  --repo acme/shop --dir /srv/shop --command ./deploy.sh
```

Pin a version with `NIMDEPLOY_VERSION=v0.3.2`. Prefer to read it first?

```bash
curl -fsSLO https://nimdeploy.nimbox360.com/install.sh
less install.sh
sh install.sh
```

## Manual download

Every [release](https://github.com/aitorroma/nimdeploy/releases) has:

| File | Contents |
|---|---|
| `nimdeploy_linux_amd64`, `nimdeploy_linux_arm64` | the bare binary |
| `nimdeploy_<version>_linux_<arch>.tar.gz` | binary, `install.sh`, example config, systemd units, deploy scripts, README |
| `checksums.txt` | SHA-256 of all of the above |

```bash
VERSION=v0.3.2 ARCH=amd64
curl -fsSLO https://github.com/aitorroma/nimdeploy/releases/download/$VERSION/nimdeploy_${VERSION}_linux_${ARCH}.tar.gz
curl -fsSLO https://github.com/aitorroma/nimdeploy/releases/download/$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar xzf nimdeploy_${VERSION}_linux_${ARCH}.tar.gz && cd nimdeploy_${VERSION}_linux_${ARCH}
./install.sh            # or: sudo ./install.sh
```

## From source

Go 1.26 or newer:

```bash
git clone https://github.com/aitorroma/nimdeploy && cd nimdeploy
make build              # ./nimdeploy, version from git describe
make install            # build + sudo ./install.sh
```

## Requirements

- Linux with systemd (tested on Amazon Linux 2023, Debian, Ubuntu, Arch).
- A reverse proxy with HTTPS in front (nginx, Caddy, Traefik, Cloudflare
  Tunnel…). nimdeploy speaks plain HTTP on localhost.
- Whatever your deploy script needs (git, node, php…), in the `PATH` of the
  user nimdeploy runs as.
