#!/bin/sh
# Points an AUR jobtail-bin checkout at a new release, without makepkg
# (the release runner is Ubuntu): rewrites pkgver, resets pkgrel, and swaps
# the per-arch checksums in both PKGBUILD and .SRCINFO.
#
#   packaging/aur/bump.sh <dir> <version> <sha256-linux-amd64> <sha256-linux-arm64>
set -eu

dir="$1"; new="${2#v}"; sha_amd64="$3"; sha_arm64="$4"
cd "$dir"

old="$(sed -n 's/^pkgver=//p' PKGBUILD)"
old_amd64="$(sed -n "s/^sha256sums_x86_64=('\(.*\)')/\1/p" PKGBUILD)"
old_arm64="$(sed -n "s/^sha256sums_aarch64=('\(.*\)')/\1/p" PKGBUILD)"
[ -n "$old" ] && [ -n "$old_amd64" ] && [ -n "$old_arm64" ] || { echo "bump.sh: can't parse PKGBUILD" >&2; exit 1; }

for f in PKGBUILD .SRCINFO; do
  sed -i \
    -e "s/${old}/${new}/g" \
    -e "s/${old_amd64}/${sha_amd64}/" \
    -e "s/${old_arm64}/${sha_arm64}/" \
    -e 's/^pkgrel=.*/pkgrel=1/' \
    -e 's/^\(\tpkgrel = \).*/\11/' \
    "$f"
done
echo "jobtail-bin ${old} -> ${new}"
