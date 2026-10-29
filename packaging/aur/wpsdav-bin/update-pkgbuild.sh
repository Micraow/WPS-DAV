#!/usr/bin/env bash
# Update pkgver and sha256sums from a published GitHub release.
# Usage: ./update-pkgbuild.sh 0.1.0
set -euo pipefail

ver="${1:?usage: update-pkgbuild.sh <version-without-v>}"
cd "$(dirname "$0")"

url="https://github.com/Micraow/WPS-DAV/releases/download/v${ver}/checksums-sha256.txt"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

curl -fsSL "$url" -o "$tmp"
name="wpsdav_${ver}_linux_amd64.tar.gz"
sum="$(awk -v n="$name" '$2 == n { print $1 }' "$tmp")"
if [ -z "$sum" ]; then
  echo "checksum for ${name} not found in ${url}" >&2
  exit 1
fi

sed -i -E "s/^pkgver=.*/pkgver=${ver}/" PKGBUILD
sed -i -E "s/^sha256sums=\('.*'\)/sha256sums=('${sum}')/" PKGBUILD

echo "updated PKGBUILD to ${ver}"
echo "sha256: ${sum}"
echo "next: makepkg --printsrcinfo > .SRCINFO"
