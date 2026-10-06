#!/bin/sh
# One-line installer for nimdeploy, published as https://nimdeploy.nimbox360.com/install.sh
#
#   curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh
#   curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sh -s -- --repo acme/shop --dir /srv/shop --command ./deploy.sh
#   curl -fsSL https://nimdeploy.nimbox360.com/install.sh | sudo sh
#
# Downloads the release for this machine from GitHub, checks it against the
# release's checksums.txt and hands over to the real installer:
#   - normal user: "nimdeploy install [ARGS]" (no root; ~/.local/bin, systemd user service)
#   - root:        "install.sh [ARGS]" from the release archive (system service)
# NIMDEPLOY_VERSION=v0.3.2 picks a release (default: the latest).
set -eu

REPO="aitorroma/nimdeploy"
VERSION="${NIMDEPLOY_VERSION:-latest}"

say() { printf 'nimdeploy: %s\n' "$*" >&2; }
die() {
	say "ERROR: $*"
	exit 1
}

[ "$(uname -s)" = Linux ] || die "only Linux is supported (this is $(uname -s))"
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "no build for $(uname -m); build from source: https://github.com/$REPO" ;;
esac
command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
	SHA="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	SHA="shasum -a 256"
else
	die "sha256sum or shasum is required to verify the download"
fi

if [ "$VERSION" = latest ]; then
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$VERSION" ] || die "could not find the latest release (GitHub API rate limit?); set NIMDEPLOY_VERSION=vX.Y.Z"
fi
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM
cd "$TMP"

fetch() {
	curl -fsSL --retry 3 -o "$1" "$BASE/$1" || die "download failed: $BASE/$1"
}
verify() {
	want=$(awk -v f="./$1" '$2 == f || $2 == substr(f, 3) { print $1; exit }' checksums.txt)
	[ -n "$want" ] || die "$1 is not listed in checksums.txt"
	got=$($SHA "$1" | awk '{ print $1 }')
	[ "$got" = "$want" ] || die "checksum mismatch for $1; refusing to install"
}

say "installing $VERSION for linux/$ARCH"
fetch checksums.txt
if [ "$(id -u)" -eq 0 ]; then
	ARCHIVE="nimdeploy_${VERSION}_linux_${ARCH}.tar.gz"
	fetch "$ARCHIVE"
	verify "$ARCHIVE"
	tar -xzf "$ARCHIVE"
	cd "nimdeploy_${VERSION}_linux_${ARCH}"
	# Read the installer's prompts, if any, from the terminal, not from the pipe.
	if [ -t 1 ] && [ -r /dev/tty ]; then
		exec ./install.sh "$@" </dev/tty
	fi
	exec ./install.sh "$@"
fi

BIN="nimdeploy_linux_${ARCH}"
fetch "$BIN"
verify "$BIN"
mv "$BIN" nimdeploy
chmod +x nimdeploy
# "nimdeploy install" copies itself to ~/.local/bin, so the temporary copy can go.
./nimdeploy install "$@"
