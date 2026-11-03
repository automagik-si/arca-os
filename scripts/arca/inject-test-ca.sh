#!/usr/bin/env bash
# Inject the persistent Arca test CA into this checkout, so every build embeds
# the same certificates instead of the Makefile's generate-test-certs making a
# fresh, throwaway set each time.
#
#   inject-test-ca.sh [--check]
#
# Why this exists: upstream's `make` runs generate-test-certs whenever ./certs/
# and ./mkosi.crt are absent, and in-builder.sh runs `git clean -ffdx`, which
# removes both (they are gitignored). So without this, each build would embed a
# brand-new update CA and Secure Boot signing key, and a node installed from an
# earlier build could never verify a later build's signed index or usr-verity
# signature again. The CA is therefore kept ONCE, outside any checkout, and
# copied in before every build.
#
# The CA set lives at $ARCA_TEST_CA_DIR (default /srv/arca-test-ca), which is
# outside /srv/work/<checkout>, so in-builder.sh's clean never touches it. It
# holds certs/ (the repo's ./certs tree), files/ (incus-osd/certs/files) and a
# FINGERPRINTS file pinning the Update CA and the Secure Boot DB-1 signing
# certificate.
#
# This copies certs/ and files/ into the checkout, recreates the mkosi.crt and
# mkosi.key symlinks and the EFI mkosi.der (as
# scripts/test/switch-secure-boot-signing-key.sh 1 would), then verifies the
# two pinned fingerprints. It REFUSES (exit 1) when the CA set is missing or a
# fingerprint does not match, so a build can never silently fall back to a
# fresh, unrecoverable CA. --check only verifies and prints the fingerprints,
# writing nothing into the checkout.
set -euo pipefail

CA_DIR=${ARCA_TEST_CA_DIR:-/srv/arca-test-ca}
SB_KEY=secureboot-DB-1-R1

log() { printf 'inject-test-ca: %s\n' "$*" >&2; }
die() { printf 'inject-test-ca: %s\n' "$*" >&2; exit 1; }

check_only=false
case "${1:-}" in
--check) check_only=true ;;
"") ;;
-h | --help) sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
*) die "unknown argument: $1" ;;
esac

cd "$(dirname "$0")/../.."
[ -f mkosi.conf ] && [ -f Makefile ] || die "not at the top of an Arca OS checkout"
command -v openssl >/dev/null || die "openssl is not on PATH"

# The CA set must be present and complete, or we refuse rather than let the
# build generate a throwaway one.
[ -d "$CA_DIR" ] || die "no test CA at $CA_DIR (run provision-test-ca.sh once); refusing to build with a throwaway CA"
[ -d "$CA_DIR/certs" ] && [ -d "$CA_DIR/files" ] || die "$CA_DIR is incomplete (need certs/ and files/)"
[ -f "$CA_DIR/FINGERPRINTS" ] || die "$CA_DIR has no FINGERPRINTS file"
for f in files/update-E1.crt "files/$SB_KEY.crt" "certs/$SB_KEY.key"; do
	[ -e "$CA_DIR/$f" ] || die "$CA_DIR/$f is missing"
done

fp() { openssl x509 -in "$1" -noout -fingerprint -sha256 | sed 's/.*=//; s/://g'; }
want_update=$(sed -n 's/^update_ca_sha256=//p' "$CA_DIR/FINGERPRINTS")
want_db1=$(sed -n 's/^secureboot_db1_sha256=//p' "$CA_DIR/FINGERPRINTS")
[ -n "$want_update" ] && [ -n "$want_db1" ] || die "$CA_DIR/FINGERPRINTS is missing a pinned fingerprint"
got_update=$(fp "$CA_DIR/files/update-E1.crt")
got_db1=$(fp "$CA_DIR/files/$SB_KEY.crt")
[ "$got_update" = "$want_update" ] || die "Update CA fingerprint $got_update does not match the pinned $want_update"
[ "$got_db1" = "$want_db1" ] || die "Secure Boot signing fingerprint $got_db1 does not match the pinned $want_db1"

if $check_only; then
	echo "update-ca-sha256: $got_update"
	echo "secureboot-db1-sha256: $got_db1"
	echo "inject-test-ca: CA set at $CA_DIR verified"
	exit 0
fi

# Copy the CA into the checkout.
rm -rf certs incus-osd/certs/files
cp -a "$CA_DIR/certs" certs
install -d incus-osd/certs/files
cp -a "$CA_DIR/files/." incus-osd/certs/files/

# Recreate the mkosi signing symlinks and the EFI der, as
# scripts/test/switch-secure-boot-signing-key.sh 1 does. mkosi hard-codes the
# names mkosi.crt / mkosi.key.
rm -f mkosi.crt mkosi.key mkosi.images/base/mkosi.extra/boot/EFI/mkosi.der
ln -s "./incus-osd/certs/files/$SB_KEY.crt" mkosi.crt
ln -s "./certs/$SB_KEY.key" mkosi.key
mkdir -p mkosi.images/base/mkosi.extra/boot/EFI/
openssl x509 -in mkosi.crt -out mkosi.images/base/mkosi.extra/boot/EFI/mkosi.der -outform DER

log "injected the persistent test CA from $CA_DIR (update-ca $got_update)"
