#!/usr/bin/env bash
# Create the persistent Arca test CA once, in the builder, outside any checkout.
#
#   provision-test-ca.sh [--force]
#
# The build embeds a test CA (the update CA, the Secure Boot signing keys and
# so on). Upstream regenerates that CA on every build when ./certs/ is absent,
# and in-builder.sh's `git clean -ffdx` makes it absent every time, so each
# build would embed a different, unrecoverable CA. This generates ONE CA with
# upstream's own scripts/test generators and stores it at $ARCA_TEST_CA_DIR
# (default /srv/arca-test-ca), from where inject-test-ca.sh copies it into each
# checkout before the build. Keep a copy off the builder too (for example on
# the build host under ~/.cache/arca-os/test-ca, mode 0700); if both copies are
# lost, nodes already installed can no longer be sent a verifiable update.
#
# It runs from the top of a checkout, in the builder, as root. It REFUSES to
# overwrite an existing CA unless --force is given (replacing the CA orphans
# every node already installed from a build that embedded the old one).
set -euo pipefail

CA_DIR=${ARCA_TEST_CA_DIR:-/srv/arca-test-ca}
SB_KEY=secureboot-DB-1-R1

log() { printf 'provision-test-ca: %s\n' "$*" >&2; }
die() { printf 'provision-test-ca: %s\n' "$*" >&2; exit 1; }

force=false
case "${1:-}" in
--force) force=true ;;
"") ;;
-h | --help) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
*) die "unknown argument: $1" ;;
esac

cd "$(dirname "$0")/../.."
[ -f mkosi.conf ] && [ -f Makefile ] || die "not at the top of an Arca OS checkout"
for s in generate-test-certificates generate-secure-boot-vars switch-secure-boot-signing-key; do
	[ -x "scripts/test/$s.sh" ] || die "missing scripts/test/$s.sh"
done
command -v cert-to-efi-sig-list >/dev/null || die "efitools is not installed"

if [ -e "$CA_DIR" ] && ! $force; then
	die "$CA_DIR already exists; refusing to replace the CA (use --force only if no node depends on it)"
fi

# Generate into a clean temp checkout root so we never disturb the caller's
# certs/. The generators write ./certs and ./incus-osd/certs/files.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -a scripts "$work/scripts"
cp -a mkosi.conf Makefile "$work/" 2>/dev/null || true
install -d "$work/incus-osd/certs" "$work/mkosi.images/base/mkosi.extra/boot/EFI"
(
	cd "$work"
	./scripts/test/generate-test-certificates.sh >/dev/null
	./scripts/test/generate-secure-boot-vars.sh >/dev/null
	./scripts/test/switch-secure-boot-signing-key.sh 1 >/dev/null
)

fp() { openssl x509 -in "$1" -noout -fingerprint -sha256 | sed 's/.*=//; s/://g'; }

stage="$CA_DIR.tmp.$$"
rm -rf "$stage"
install -d -m 0700 "$stage" "$stage/files"
cp -a "$work/certs" "$stage/certs"
cp -a "$work/incus-osd/certs/files/." "$stage/files/"
{
	printf 'update_ca_sha256=%s\n' "$(fp "$stage/files/update-E1.crt")"
	printf 'secureboot_db1_sha256=%s\n' "$(fp "$stage/files/$SB_KEY.crt")"
} > "$stage/FINGERPRINTS"
chmod -R go-rwx "$stage"
rm -rf "$CA_DIR"
mv "$stage" "$CA_DIR"

log "created the persistent test CA at $CA_DIR"
cat "$CA_DIR/FINGERPRINTS"
log "copy $CA_DIR off the builder and keep it safe"
