#!/usr/bin/env bash
# Publish a finished build into an update tree, signed with the persistent test
# CA, in the layout incus-osd's images provider reads. Runs in the builder, as
# root, from the top of a checkout (it builds the small Go tools and reads the
# persistent CA at $ARCA_TEST_CA_DIR, default /srv/arca-test-ca).
#
#   publish-build.sh --version <ver> --out <tree> [--channel stable]
#                    [--apps incus,arca] [--with-image]
#
# It reads the build at /srv/artifacts/os/<ver>/ (as build.sh wrote it) and
# publishes, into <tree> (adding to any releases already there):
#   index.json / index.sjson, and <ver>/update.json / update.sjson with
#   <ver>/<arch>/ holding the UKI (.efi.gz), the usr, usr-verity and
#   usr-verity-sig images, and <app>.raw.gz for each --apps application.
# The .sjson files are signed with the persistent update key, chained to the
# update CA every build embeds, so a node installed from any build with that CA
# verifies them over plain http. The usr-verity image carries its own
# build-time signature (the persistent Secure Boot signing key), which the node
# verifies against its embedded db, so the OS update applies. A normal update
# downloads only the UKI and usr images; --with-image also publishes the full
# OS raw (.img.gz), used only for re-imaging.
#
# It reads the tree back as the node will: both signatures against the update
# CA, exactly one release for <ver> in the channel, and each file's sha256 and
# size, and that every --apps application is in it. It fails if anything does
# not match.
set -euo pipefail

CA_DIR=${ARCA_TEST_CA_DIR:-/srv/arca-test-ca}
OS_ARTIFACTS=/srv/artifacts/os
channel=stable
version='' out='' with_image=false
apps_csv=incus,arca

log() { printf 'publish-build: %s\n' "$*" >&2; }
die() { printf 'publish-build: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'publish-build: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || refuse "need a value for $1"; version=$2; shift 2 ;;
	--out) [ $# -ge 2 ] || refuse "need a value for $1"; out=$2; shift 2 ;;
	--channel) [ $# -ge 2 ] || refuse "need a value for $1"; channel=$2; shift 2 ;;
	--apps) [ $# -ge 2 ] || refuse "need a value for $1"; apps_csv=$2; shift 2 ;;
	--with-image) with_image=true; shift ;;
	-h | --help) sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

[ -n "$version" ] || refuse "--version <ver> is required"
[ -n "$out" ] || refuse "--out <tree> is required"
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || refuse "version '$version' is not 12 digits"
[ "$(id -u)" -eq 0 ] || die "run as root (inside the builder)"
cd "$(dirname "$0")/../.."
{ [ -f mkosi.conf ] && [ -f Makefile ]; } || die "not at the top of an Arca OS checkout"
for c in jq openssl pigz zip sha256sum; do command -v "$c" >/dev/null || die "missing tool: $c"; done
osname=$(sed -n 's/^ImageId=//p' mkosi.conf)
[ -n "$osname" ] || die "no ImageId in mkosi.conf"
# image-publisher derives the release name and architecture from the zip's
# name and places each file under its own <arch>/ directory in the tree.
case "$(uname -m)" in
x86_64) zip_arch=amd64 ;;
aarch64) zip_arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

src=$OS_ARTIFACTS/$version
[ -d "$src" ] || die "no build at $src (run build.sh first)"
# The signing cert chains to the update CA; both from the persistent CA.
sig_key=$CA_DIR/certs/update-E1.key
sig_cert=$CA_DIR/certs/update-E1.crt
sig_chain=$CA_DIR/files/update-E1.crt
for f in "$sig_key" "$sig_cert" "$sig_chain"; do
	[ -f "$f" ] || die "persistent CA file missing: $f"
done

IFS=, read -r -a apps <<<"$apps_csv"
[ "${#apps[@]}" -gt 0 ] || die "no applications given"

# Build the publisher and manifest tools (build.sh builds them, but a stand-alone
# publish runs in a freshly cleaned checkout).
log "building image-publisher"
(cd incus-osd && go build ./cmd/image-publisher) || die "could not build image-publisher"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/upload"

# OS files the node's sysupdate consumes: the UKI and the three usr images.
efi=$src/${osname}_${version}.efi
[ -f "$efi" ] || die "no UKI $efi"
cp "$efi" "$tmp/upload/"
found_usr=0
for u in "$src/${osname}_${version}".usr-*.raw; do
	[ -e "$u" ] || continue
	cp "$u" "$tmp/upload/"
	found_usr=$((found_usr + 1))
done
[ "$found_usr" -ge 3 ] || die "expected usr, usr-verity and usr-verity-sig images in $src (found $found_usr)"
if $with_image; then
	raw=$src/${osname}_${version}.raw
	[ -f "$raw" ] || die "no OS raw $raw"
	cp "$raw" "$tmp/upload/${osname}_${version}.img"
fi
# Applications.
for a in "${apps[@]}"; do
	[ -f "$src/$a.raw" ] || die "no application image $src/$a.raw"
	cp "$src/$a.raw" "$tmp/upload/"
done

log "compressing $(find "$tmp/upload" -type f | wc -l) files"
(cd "$tmp/upload" && for f in *; do pigz -1 "$f"; done)
zipf=$tmp/${osname}-${version}-${zip_arch}.zip
zip -q -0 -j "$zipf" "$tmp"/upload/*.gz

install -d -m 0755 "$out"
log "publishing $version into $out (channel $channel), signed with the persistent update key"
SIG_KEY=$sig_key SIG_CERTIFICATE=$sig_cert SIG_CHAIN=$sig_chain UPDATE_CHANNEL=$channel \
	incus-osd/image-publisher sync "$out" "$zipf" >&2 || die "image-publisher sync failed"

# Read the tree back as the node will.
for s in index "$version/update"; do
	{ [ -f "$out/$s.json" ] && [ -f "$out/$s.sjson" ]; } || die "image-publisher wrote no $s.sjson"
	openssl smime -verify -text -partial_chain -CAfile "$sig_chain" \
		-in "$out/$s.sjson" -out "$tmp/verified.json" 2>/dev/null || die "$s.sjson does not verify against the update CA"
	[ "$(jq -S . "$tmp/verified.json")" = "$(jq -S . "$out/$s.json")" ] || die "$s.sjson does not carry $s.json"
done
idx=$out/index.json
jq -e --arg v "$version" '[.updates[] | select(.version==$v)] | length == 1' "$idx" >/dev/null \
	|| die "index.json does not list exactly one update for $version"
jq -e --arg v "$version" --arg c "$channel" '.updates[] | select(.version==$v) | .channels | index($c) != null' "$idx" >/dev/null \
	|| die "index.json does not put $version in channel $channel"
# Every published file's sha256 and size must match the index.
jq -r --arg v "$version" '.updates[] | select(.version==$v) | .files[] | .filename+" "+(.sha256)+" "+(.size|tostring)' "$idx" |
	while read -r fn sha sz; do
		p=$out/$version/$fn
		[ -f "$p" ] || die "index lists $fn but it is not in the tree"
		[ "$(sha256sum "$p" | cut -d' ' -f1)" = "$sha" ] || die "$fn sha256 mismatch"
		[ "$(stat -c %s "$p")" = "$sz" ] || die "$fn size mismatch"
	done
# The OS update must carry the UKI and the three usr files.
for t in update-efi update-usr update-usr-verity update-usr-verity-signature; do
	jq -e --arg v "$version" --arg t "$t" '.updates[] | select(.version==$v) | any(.files[]; .type==$t)' "$idx" >/dev/null \
		|| die "the published update is missing a file of type $t"
done
# Every --apps application must be in the update, or no node ever gets it.
for a in "${apps[@]}"; do
	jq -e --arg v "$version" --arg f "$a.raw.gz" '.updates[] | select(.version==$v) | any(.files[]; .type=="application" and (.filename | split("/") | last) == $f)' "$idx" >/dev/null \
		|| die "the published update is missing the application $a (image-publisher did not publish $a.raw.gz)"
done

nfiles=$(jq -r --arg v "$version" '.updates[] | select(.version==$v) | .files | length' "$idx")
log "published $version: $nfiles files in channel $channel at $out"
echo "published-version: $version"
echo "published-files: $nfiles"
echo "published-channel: $channel"
