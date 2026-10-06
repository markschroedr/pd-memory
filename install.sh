#!/bin/sh
set -eu
repo=markschroedr/pd-memory
case "$(uname -s)" in Darwin) os=darwin;; Linux) os=linux;; *) echo 'Download the Windows binary from GitHub Releases.' >&2; exit 1;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=amd64;; *) echo 'Unsupported architecture.' >&2; exit 1;; esac
version=${PD_MEMORY_VERSION:-latest}
if [ "$version" = latest ]; then base="https://github.com/$repo/releases/latest/download"; else base="https://github.com/$repo/releases/download/$version"; fi
asset="pd-memory_${os}_${arch}"
dir=${PD_MEMORY_INSTALL_DIR:-"$HOME/.local/bin"}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
curl -fLsS "$base/$asset" -o "$tmp/$asset"
curl -fLsS "$base/checksums.txt" -o "$tmp/checksums.txt"
expected=$(awk -v file="$asset" '$2 == file {print $1}' "$tmp/checksums.txt")
if [ -z "$expected" ]; then echo 'Release checksum missing.' >&2; exit 1; fi
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$asset" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}'); fi
if [ "$actual" != "$expected" ]; then echo 'Release checksum mismatch.' >&2; exit 1; fi
mkdir -p "$dir"
install -m 755 "$tmp/$asset" "$dir/pd-memory"
echo "Installed $dir/pd-memory"
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to PATH before running pd-memory init.";; esac
