#!/usr/bin/env bash
# Write a test-signed build as a USB installer that never updates itself (in
# the builder, as root, from the top of the checkout build.sh has just built,
# before anything cleans it).
#
#   tracer-installer.sh [--client-cert <file>|-] [--force-install] [--out <dir>]
#
# It writes, in <dir> (default /srv/artifacts/installer/<version>, with
# /srv/artifacts/installer/latest pointing there):
#   <ImageId>_<version>-installer.img
#       The built .raw with an install seed at seek=4196352 bs=512:
#       install (force_install only with --force-install; no forced reboot,
#       so it waits for the stick to be removed), applications incus and arca,
#       provider debug, update (check_frequency never, auto_reboot false), and
#       with --client-cert, incus (apply_defaults, that certificate trusted as
#       a client). It installs onto the one disk it finds, and refuses when it
#       finds none or several.
#   <ImageId>_<version>-apps.img
#       A GPT disk image with one FAT partition, named and labelled
#       RESCUE_DATA, holding update/: this build's update.sjson, signed with
#       the same test update key the image trusts, and the incus and arca
#       sysexts. incus-osd applies it at boot (its recovery mode), which is how
#       the installed system gets its applications with no update server.
#   seed/ (the seed files and seed.tar), RELEASE, OSNAME, base.manifest,
#   kernel.BUILDINFO (when build.sh had --local-kernel) and SHA256SUMS.
# It signs with this checkout's test certificates only (certs/, made by the
# build). The target's firmware must be in Secure Boot Setup Mode: the first
# boot enrolls this build's test keys.
# The last lines printed are "installer: <file> <bytes> <sha256>" and
# "apps: <file> <bytes> <sha256>".
set -euo pipefail

OUT_BASE=/srv/artifacts/installer
OS_ARTIFACTS=/srv/artifacts/os
SEED_OFFSET=4196352
APPS=(incus arca)

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'tracer-installer: %s\n' "$*" >&2; }
die() { printf 'tracer-installer: %s\n' "$*" >&2; exit 1; }

cert_src='' out='' force_install=false
while [ $# -gt 0 ]; do
	case "$1" in
	--client-cert) [ $# -ge 2 ] || usage; cert_src=$2; shift 2 ;;
	--force-install) force_install=true; shift ;;
	--out) [ $# -ge 2 ] || usage; out=$2; shift 2 ;;
	-h | --help) usage ;;
	*) printf 'tracer-installer: unknown argument: %s\n' "$1" >&2; exit 2 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (inside the builder)"
cd "$(dirname "$0")/../.."
if [ ! -f mkosi.conf ] || [ ! -f Makefile ]; then die "not at the top of an Arca OS checkout"; fi
for c in make jq openssl sgdisk mformat mcopy sha256sum tar; do
	command -v "$c" >/dev/null || die "missing tool: $c"
done
osname=$(sed -n 's/^ImageId=//p' mkosi.conf)
[ -n "$osname" ] || die "no ImageId in mkosi.conf"

shopt -s nullglob
efis=(mkosi.output/"${osname}"_*.efi)
shopt -u nullglob
[ ${#efis[@]} -gt 0 ] || die "no ${osname}_*.efi in mkosi.output (run build.sh first)"
version=$(printf '%s\n' "${efis[@]}" | sed -e 's/.*_//' -e 's/\.efi$//' | sort -n | tail -1)
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || die "unexpected version '$version'"
raw=mkosi.output/${osname}_${version}.raw
[ -f "$raw" ] || die "no $raw"
for a in "${APPS[@]}"; do
	[ -f "mkosi.output/$a.raw" ] || die "no mkosi.output/$a.raw"
done
for f in certs/update-E1.key certs/update-E1.crt incus-osd/certs/files/update-E1.crt incus-osd/image-publisher; do
	[ -e "$f" ] || die "no $f in this checkout (the build makes it)"
done
out=${out:-$OUT_BASE/$version}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cert=''
if [ -n "$cert_src" ]; then
	if [ "$cert_src" = - ]; then cat > "$tmp/client.crt"; else cp -- "$cert_src" "$tmp/client.crt"; fi
	! grep -q 'PRIVATE KEY' "$tmp/client.crt" || die "refused: the client certificate file holds a private key"
	openssl x509 -noout -in "$tmp/client.crt" 2>/dev/null || die "the client certificate is not a PEM certificate"
	cert=$tmp/client.crt
fi

start=$(date +%s)
stage=$out.partial
rm -rf "$stage"
install -d -m 0755 "$stage" "$stage/seed"

# --- The signed update, as upstream's publish-local-update makes it ---------
ud=local-image-server/$version
rm -rf "$ud"
log "signing the $version update with this build's test update key"
make publish-local-update RELEASE="$version" >&2
if [ ! -f "$ud/update.json" ] || [ ! -f "$ud/update.sjson" ]; then
	die "publish-local-update made no $ud/update.sjson"
fi
# incus-osd checks it against the Update CA it embeds; so does this.
openssl smime -verify -text -partial_chain -CAfile incus-osd/certs/files/update-E1.crt \
	-in "$ud/update.sjson" -out "$tmp/verified.json" 2>/dev/null || die "$ud/update.sjson does not verify against the Update CA"
[ "$(jq -S . "$tmp/verified.json")" = "$(jq -S . "$ud/update.json")" ] || die "update.sjson does not carry update.json"
[ "$(jq -r .version "$ud/update.json")" = "$version" ] || die "update.json is not for $version"

# --- The apps image: one FAT partition named and labelled RESCUE_DATA ------
mkdir -p "$tmp/rescue/update"
cp "$ud/update.sjson" "$tmp/rescue/update/"
: > "$tmp/files"
for a in "${APPS[@]}"; do
	f=$(jq -r --arg n "$a.raw.gz" '[.files[] | select((.filename | split("/") | last) == $n) | .filename] | if length == 1 then .[0] else empty end' "$ud/update.json")
	[ -n "$f" ] || die "update.json does not list exactly one $a.raw.gz"
	mkdir -p "$tmp/rescue/update/$(dirname "$f")"
	cp "$ud/$f" "$tmp/rescue/update/$f"
	printf '%s\n' "$f" >> "$tmp/files"
done
apps_img=$stage/${osname}_${version}-apps.img
bytes=$(du -sb "$tmp/rescue" | cut -f1)
# The files, 5% for the FAT and its tables, and 32 MiB of slack.
mib=$((bytes * 105 / 100 / 1048576 + 32))
truncate -s $(((mib + 2) * 1048576)) "$apps_img"
sgdisk -n "1:2048:+${mib}M" -t 1:0700 -c 1:RESCUE_DATA "$apps_img" >/dev/null 2>&1
sgdisk -i 1 "$apps_img" 2>/dev/null | grep -qx "Partition name: 'RESCUE_DATA'" || die "sgdisk wrote no RESCUE_DATA partition to $apps_img"
fat=()
[ "$mib" -lt 512 ] || fat=(-F)
export MTOOLS_SKIP_CHECK=1
mformat -i "$apps_img@@1048576" -T $((mib * 2048)) -h 64 -s 32 "${fat[@]}" -v RESCUE_DATA ::
mcopy -s -i "$apps_img@@1048576" "$tmp/rescue/update" ::/
# Read the partition and every file back, and check them against update.json.
[ "$(sgdisk -i 1 "$apps_img" 2>/dev/null | sed -n 's/^First sector: \([0-9]*\).*/\1/p')" = 2048 ] \
	|| die "the RESCUE_DATA partition of $apps_img does not start at sector 2048"
mcopy -n -i "$apps_img@@1048576" ::/update/update.sjson "$tmp/back.sjson"
cmp -s "$tmp/back.sjson" "$ud/update.sjson" || die "update.sjson reads back changed from $apps_img"
while IFS= read -r f; do
	mcopy -n -i "$apps_img@@1048576" "::/update/$f" "$tmp/back"
	want=$(jq -r --arg f "$f" '.files[] | select(.filename == $f) | .sha256' "$ud/update.json")
	[ "$(sha256sum "$tmp/back" | cut -d' ' -f1)" = "$want" ] || die "$f reads back with the wrong sha256 from $apps_img"
	rm -f "$tmp/back"
done < "$tmp/files"
rm -rf "$tmp/rescue" "$ud"

# --- The installer: the built image with an install seed --------------------
inst_img=$stage/${osname}_${version}-installer.img
seed=$stage/seed
jq -n --argjson f "$force_install" '{version: "1", force_install: $f, force_reboot: false}' > "$seed/install.json"
jq -n '{version: "1", applications: ($ARGS.positional | map({name: .}))}' --args "${APPS[@]}" > "$seed/applications.json"
jq -n '{version: "1", name: "debug"}' > "$seed/provider.json"
jq -n '{version: "1", auto_reboot: false, channel: "stable", check_frequency: "never"}' > "$seed/update.json"
seed_files=(install.json applications.json provider.json update.json)
if [ -n "$cert" ]; then
	jq -n --rawfile c "$cert" \
		'{version: "1", apply_defaults: true, preseed: {certificates: [{name: "installer-client", type: "client", certificate: $c}]}}' \
		> "$seed/incus.json"
	seed_files+=(incus.json)
fi
tar -C "$seed" --owner=0 --group=0 -cf "$seed/seed.tar" "${seed_files[@]}"
cp --sparse=always "$raw" "$inst_img"
# The seed goes at the start of partition 2, and must fit in it.
p2_start=$(sgdisk -i 2 "$inst_img" | sed -n 's/^First sector: \([0-9]*\).*/\1/p')
p2_end=$(sgdisk -i 2 "$inst_img" | sed -n 's/^Last sector: \([0-9]*\).*/\1/p')
[ "$p2_start" = "$SEED_OFFSET" ] || die "partition 2 starts at sector ${p2_start:-?}, not $SEED_OFFSET"
seed_bytes=$(stat -c %s "$seed/seed.tar")
[ "$seed_bytes" -le $(((p2_end - p2_start + 1) * 512)) ] || die "the seed ($seed_bytes bytes) does not fit in partition 2"
dd if="$seed/seed.tar" of="$inst_img" seek="$SEED_OFFSET" bs=512 conv=notrunc status=none
dd if="$inst_img" skip="$SEED_OFFSET" bs=512 count=$(((seed_bytes + 511) / 512)) status=none | head -c "$seed_bytes" | cmp -s - "$seed/seed.tar" \
	|| die "the seed reads back changed from $inst_img"

# --- Metadata --------------------------------------------------------------
printf '%s\n' "$version" > "$stage/RELEASE"
printf '%s\n' "$osname" > "$stage/OSNAME"
cp mkosi.output/base.manifest "$stage/"
if [ -f "$OS_ARTIFACTS/$version/kernel.BUILDINFO" ]; then
	cp "$OS_ARTIFACTS/$version/kernel.BUILDINFO" "$stage/"
fi
(cd "$stage" && sha256sum -- *.img > SHA256SUMS)
rm -rf "$out"
mv -T "$stage" "$out"
if [ "$(dirname "$out")" = "$OUT_BASE" ]; then
	ln -sfn "$(basename "$out")" "$OUT_BASE/latest"
fi

echo "seed: ${seed_files[*]} (force_install $force_install)"
echo "build-seconds: $(($(date +%s) - start))"
for f in "$(basename "$inst_img")" "$(basename "$apps_img")"; do
	kind=installer
	case "$f" in *-apps.img) kind=apps ;; esac
	echo "$kind: $out/$f $(stat -c %s "$out/$f") $(sed -n "s/  $f\$//p" "$out/SHA256SUMS")"
done
