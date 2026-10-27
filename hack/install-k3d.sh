#!/usr/bin/env bash
# Installs a pinned k3d release into $1 (default: /usr/local/bin), verifying
# its checksum against the release's checksums.txt.
set -euo pipefail

K3D_VERSION="${K3D_VERSION:-v5.9.0}"
DEST="${1:-/usr/local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
asset="k3d-${os}-${arch}"
base="https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

want="$(awk -v f="_dist/$asset" '$2 == f || $2 == substr(f, 7) { print $1 }' "$tmp/checksums.txt")"
got="$( (sha256sum "$tmp/$asset" 2>/dev/null || shasum -a 256 "$tmp/$asset") | awk '{ print $1 }')"
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "checksum mismatch for $asset: want '$want', got '$got'" >&2
  exit 1
fi

install -m 0755 "$tmp/$asset" "$DEST/k3d"
"$DEST/k3d" version
