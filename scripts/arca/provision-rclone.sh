#!/usr/bin/env bash
# Install the builder's pinned rclone; --check only checks it, without downloads.
set -euo pipefail

version=1.76.0
# https://downloads.rclone.org/v1.76.0/SHA256SUMS (linux-amd64 zip).
sha256=b00f00b3266c3764413afe599d3ec316f6f43b3e0ea8a0bc1d186d2133e46df7
archive=rclone-v$version-linux-amd64.zip
log() { printf 'provision-rclone: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }
check() {
	local found
	found=$(rclone version 2>/dev/null | sed -n '1p') || found=missing
	if [ "$found" != "rclone v$version" ] || ! rclone serve s3 --help >/dev/null 2>&1; then
		log "need rclone v$version with serve s3; run scripts/arca/provision-rclone.sh as root during builder provisioning"
		return 1
	fi
}

case "${1:-}" in
--check) [ "$#" -eq 1 ] || die "usage: provision-rclone.sh [--check]"; check; exit ;;
'') [ "$#" -eq 0 ] || die "usage: provision-rclone.sh [--check]" ;;
*) die "usage: provision-rclone.sh [--check]" ;;
esac
[ "$(id -u)" -eq 0 ] || die "run as root"
if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
	die "needs linux-amd64"
fi
if [ -x /usr/local/bin/rclone ] && [ "$(/usr/local/bin/rclone version | sed -n '1p')" = "rclone v$version" ]; then
	log "rclone v$version already installed"
else
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	log "installing rclone v$version (sha256 pinned)"
	curl -fsSL "https://downloads.rclone.org/v$version/$archive" -o "$tmp/$archive"
	printf '%s  %s\n' "$sha256" "$tmp/$archive" | sha256sum -c --quiet - || die "archive sha256 mismatch"
	python3 -m zipfile -e "$tmp/$archive" "$tmp"
	install -m 0755 "$tmp/rclone-v$version-linux-amd64/rclone" /usr/local/bin/rclone
fi
hash -r
check
