#!/bin/sh
# Install a specific CLI release. Run on Linux; no root or Go runtime required.
set -eu
version=${1:?Usage: sh scripts/install.sh v0.1.0}
case "$version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo 'Expected a CLI release tag such as v0.1.0' >&2; exit 1 ;;
esac
case "$version" in *[!a-zA-Z0-9._-]*) echo 'Invalid release tag' >&2; exit 1 ;; esac
[ "$(uname -s)" = Linux ] || { echo 'Linux is required' >&2; exit 1; }
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo 'Supported architectures: amd64, arm64' >&2; exit 1 ;;
esac
umask 077
task_tmp=$(mktemp -d)
trap 'rm -rf "$task_tmp"' EXIT HUP INT TERM
asset=new-api-gateway-linux-$arch.tar.gz
base=https://github.com/KaranocaVe/new-api-dev-gateway/releases/download/$version
curl --fail --location --proto '=https' --tlsv1.2 "$base/$asset" -o "$task_tmp/$asset"
curl --fail --location --proto '=https' --tlsv1.2 "$base/SHA256SUMS" -o "$task_tmp/SHA256SUMS"
(cd "$task_tmp" && sha256sum --check --ignore-missing SHA256SUMS)
tar -xzf "$task_tmp/$asset" -C "$task_tmp" ./new-api-gateway
install_dir=${GATEWAY_INSTALL_DIR:-"$HOME/.local/bin"}
mkdir -p "$install_dir"
# Rename a complete new file so upgrades cannot truncate a running executable.
install -m 755 "$task_tmp/new-api-gateway" "$install_dir/.new-api-gateway.install"
mv -f "$install_dir/.new-api-gateway.install" "$install_dir/new-api-gateway"
printf 'Installed %s to %s/new-api-gateway\n' "$version" "$install_dir"
