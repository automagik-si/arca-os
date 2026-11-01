#!/usr/bin/env bash
# Write a test-signed build as one USB installer image, plus the update tree
# its installed system fetches its applications from (in the builder, as root,
# from the top of the checkout build.sh has just built, before anything cleans
# it).
#
#   tracer-installer.sh --server-url <url> [--client-cert <file>|-]
#                       [--force-install] [--out <dir>]
#
# --server-url is required. It is the URL that will serve updates/ (below) as
# its root, such as http://<address>:<port>, and it goes into the image's seed.
# Plain http is enough: incus-osd checks the signed index, not the transport.
#
# It writes, in <dir> (default /srv/artifacts/installer/<version>, with
# /srv/artifacts/installer/latest pointing there):
#   <ImageId>_<version>-installer.img
#       The built .raw with an install seed at seek=4196352 bs=512:
#       install (force_install only with --force-install; no forced reboot,
#       so it waits for the stick to be removed), applications incus and arca,
#       provider images with that server_url, update (channel stable,
#       check_frequency never, auto_reboot false), and with --client-cert,
#       incus (apply_defaults, that certificate trusted as a client). It
#       installs onto the one disk it finds, and refuses when it finds none
#       or several.
#   updates/
#       A static tree in the layout incus-osd's images provider reads, made by
#       the build's own image-publisher: index.json, index.sjson, and
#       <version>/ with update.json, update.sjson and <arch>/incus.raw.gz and
#       <arch>/arca.raw.gz, in channel stable. The .sjson files are signed
#       with the same test update key the image trusts. The installed system
#       has no primary application on its first boot, so it checks for
#       updates once despite check_frequency never, and installs incus and
#       arca from the tree. The tree has no OS files, so the OS stays as is.
#   seed/ (the seed files and seed.tar), RELEASE, OSNAME, base.manifest,
#   kernel.BUILDINFO (when build.sh had --local-kernel) and SHA256SUMS (the
#   image and every file under updates/, by path relative to <dir>).
# It signs with this checkout's test certificates only (certs/, made by the
# build). The target's firmware must be in Secure Boot Setup Mode: the first
# boot enrolls this build's test keys.
# The last lines printed are "installer: <file> <bytes> <sha256>",
# "updates: <dir> <files> files <bytes> bytes" and "server-url: <url>".
set -euo pipefail

OUT_BASE=/srv/artifacts/installer
OS_ARTIFACTS=/srv/artifacts/os
SEED_OFFSET=4196352
APPS=(incus arca)
CHANNEL=stable

usage() { sed -n '2,41p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'tracer-installer: %s\n' "$*" >&2; }
die() { printf 'tracer-installer: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'tracer-installer: refused: %s\n' "$*" >&2; exit 2; }

cert_src='' out='' force_install=false server_url=''
while [ $# -gt 0 ]; do
	case "$1" in
	--server-url) [ $# -ge 2 ] || usage; server_url=$2; shift 2 ;;
	--client-cert) [ $# -ge 2 ] || usage; cert_src=$2; shift 2 ;;
	--force-install) force_install=true; shift ;;
	--out) [ $# -ge 2 ] || usage; out=$2; shift 2 ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

[ -n "$server_url" ] || refuse "--server-url <url> is required"
while [ "${server_url%/}" != "$server_url" ]; do server_url=${server_url%/}; done
# incus-osd appends /index.sjson and /<version>/<file> to it, so it takes no
# query, fragment or credentials.
printf '%s' "$server_url" | grep -Eqx 'https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~-]+)*' \
	|| refuse "bad --server-url '$server_url' (want http[s]://<host>[:<port>][/<path>])"

[ "$(id -u)" -eq 0 ] || die "run as root (inside the builder)"
cd "$(dirname "$0")/../.."
if [ ! -f mkosi.conf ] || [ ! -f Makefile ]; then die "not at the top of an Arca OS checkout"; fi
for c in jq openssl pigz zip sgdisk sha256sum tar; do
	command -v "$c" >/dev/null || die "missing tool: $c"
done
osname=$(sed -n 's/^ImageId=//p' mkosi.conf)
[ -n "$osname" ] || die "no ImageId in mkosi.conf"
# The archive is named with the Makefile's ARCH; the tree's files are under
# the architecture name incus-osd compares with its own.
case "$(uname -m)" in
x86_64) zip_arch=amd64 file_arch=x86_64 ;;
aarch64) zip_arch=arm64 file_arch=aarch64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

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

# --- The update tree, as image-publisher publishes a release ----------------
# The same steps as the Makefile's publish-local-update, for incus and arca
# only, into a tree of their own.
tree=$stage/updates
mkdir -p "$tmp/upload"
for a in "${APPS[@]}"; do
	pigz -1 -c "mkosi.output/$a.raw" > "$tmp/upload/$a.raw.gz"
done
zipf=$tmp/${osname}-${version}-${zip_arch}.zip
zip -q -0 -j "$zipf" "$tmp"/upload/*.raw.gz
rm -rf "$tmp/upload"
log "publishing ${APPS[*]} $version into updates/, signed with this build's test update key"
SIG_KEY=certs/update-E1.key SIG_CERTIFICATE=certs/update-E1.crt SIG_CHAIN=incus-osd/certs/files/update-E1.crt \
	UPDATE_CHANNEL=$CHANNEL incus-osd/image-publisher sync "$tree" "$zipf" >&2 || die "image-publisher sync failed"
rm -f "$zipf"

# Read the tree back as incus-osd will: both signatures against the Update CA
# it embeds, one update for this version in the channel, and exactly the
# applications, each matching its sysext.
for s in index "$version/update"; do
	[ -f "$tree/$s.json" ] && [ -f "$tree/$s.sjson" ] || die "image-publisher wrote no updates/$s.sjson"
	openssl smime -verify -text -partial_chain -CAfile incus-osd/certs/files/update-E1.crt \
		-in "$tree/$s.sjson" -out "$tmp/verified.json" 2>/dev/null || die "updates/$s.sjson does not verify against the Update CA"
	[ "$(jq -S . "$tmp/verified.json")" = "$(jq -S . "$tree/$s.json")" ] || die "updates/$s.sjson does not carry $s.json"
done
idx=$tree/index.json
[ "$(jq '.updates | length' "$idx")" = 1 ] || die "updates/index.json does not list exactly one update"
[ "$(jq -r '.updates[0].version' "$idx")" = "$version" ] || die "updates/index.json is not for $version"
jq -e --arg c "$CHANNEL" '.updates[0].channels | index($c) != null' "$idx" >/dev/null || die "updates/index.json does not put $version in channel $CHANNEL"
[ "$(jq -S '.updates[0] | del(.url)' "$idx")" = "$(jq -S . "$tree/$version/update.json")" ] \
	|| die "updates/index.json and updates/$version/update.json differ"
want=$(for a in "${APPS[@]}"; do printf '%s/%s.raw.gz\n' "$file_arch" "$a"; done | LC_ALL=C sort)
[ "$(jq -r '.updates[0].files[].filename' "$idx" | LC_ALL=C sort)" = "$want" ] \
	|| die "updates/index.json lists other files than ${APPS[*]}"
for a in "${APPS[@]}"; do
	f=$file_arch/$a.raw.gz
	entry=$(jq -c --arg f "$f" '.updates[0].files[] | select(.filename == $f)' "$idx")
	[ "$(printf '%s' "$entry" | jq -r '.type + " " + .architecture')" = "application $file_arch" ] \
		|| die "updates/index.json lists $f as '$(printf '%s' "$entry" | jq -r '.type + " " + .architecture')'"
	[ "$(printf '%s' "$entry" | jq -r .sha256)" = "$(sha256sum "$tree/$version/$f" | cut -d' ' -f1)" ] \
		|| die "updates/$version/$f does not have the sha256 the index lists"
	[ "$(printf '%s' "$entry" | jq -r .size)" = "$(stat -c %s "$tree/$version/$f")" ] \
		|| die "updates/$version/$f does not have the size the index lists"
	pigz -dc "$tree/$version/$f" | cmp -s - "mkosi.output/$a.raw" || die "updates/$version/$f does not unpack to mkosi.output/$a.raw"
done

# --- The installer: the built image with an install seed --------------------
inst_img=$stage/${osname}_${version}-installer.img
seed=$stage/seed
jq -n --argjson f "$force_install" '{version: "1", force_install: $f, force_reboot: false}' > "$seed/install.json"
jq -n '{version: "1", applications: ($ARGS.positional | map({name: .}))}' --args "${APPS[@]}" > "$seed/applications.json"
jq -n --arg u "$server_url" '{version: "1", name: "images", config: {server_url: $u}}' > "$seed/provider.json"
jq -n --arg c "$CHANNEL" '{version: "1", auto_reboot: false, channel: $c, check_frequency: "never"}' > "$seed/update.json"
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
(
	cd "$stage"
	sha256sum -- *.img
	find updates -type f | LC_ALL=C sort | xargs -d '\n' sha256sum --
) > "$tmp/SHA256SUMS" || die "could not hash the image and updates/"
mv "$tmp/SHA256SUMS" "$stage/SHA256SUMS"
(cd "$stage" && sha256sum -c --quiet SHA256SUMS) || die "SHA256SUMS does not check"
rm -rf "$out"
mv -T "$stage" "$out"
if [ "$(dirname "$out")" = "$OUT_BASE" ]; then
	ln -sfn "$(basename "$out")" "$OUT_BASE/latest"
fi

f=$(basename "$inst_img")
echo "seed: ${seed_files[*]} (force_install $force_install, provider images)"
echo "build-seconds: $(($(date +%s) - start))"
echo "installer: $out/$f $(stat -c %s "$out/$f") $(sed -n "s/  $f\$//p" "$out/SHA256SUMS")"
echo "updates: $out/updates $(find "$out/updates" -type f | wc -l) files $(du -sb "$out/updates" | cut -f1) bytes"
echo "server-url: $server_url"
