#!/usr/bin/env bash
# Pin an installer image to one disk, by its /dev/disk/by-id name.
#
#   set-target.sh <by-id name> [image dir]
#
# It rewrites seed/install.json's target.id, preserving the other install
# options and target selectors, packs the seed files into seed/seed.tar
# (install, applications, provider and update, plus incus and network when
# present), writes the seed into the image at sector 4196352, reads it back,
# and rewrites and checks SHA256SUMS. The image directory defaults to the
# current directory.
#
# A name with whitespace or '/' is refused, and so is any name containing
# ADATA, Samsung, 990, Expansion, Seagate or Flash_Disk (case-insensitive):
# those are the old RAID pair, the Samsung 990 Pro, the Seagate Expansion
# recovery disk and the 29 GB flash stick, none of which this image may ever
# install onto. A name that matches no disk makes the installer refuse, so the
# placeholder in a fresh image is safe to boot, but never intended to install.
set -euo pipefail

SEED_OFFSET=4196352
REFUSE='ADATA|Samsung|990|Expansion|Seagate|Flash_Disk'

usage() { sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
die() { printf 'set-target: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'set-target: refused: %s\n' "$*" >&2; exit 2; }

case "${1:-}" in
-h | --help) usage ;;
esac
[ $# -ge 1 ] && [ $# -le 2 ] || usage

name=$1
dir=${2:-.}
[ -n "$name" ] || refuse "empty target name"
case "$name" in
*[[:space:]/]*) refuse "target '$name' has whitespace or a slash; want a bare /dev/disk/by-id name" ;;
esac
if printf '%s' "$name" | grep -Eiq "$REFUSE"; then
	refuse "target '$name' matches the never-install list ($REFUSE)"
fi

[ -d "$dir" ] || die "no image directory: $dir"
[ -f "$dir/seed/install.json" ] || die "no $dir/seed/install.json (wrong image directory?)"
shopt -s nullglob
imgs=("$dir"/*-installer.img)
shopt -u nullglob
[ ${#imgs[@]} -eq 1 ] || die "expected one *-installer.img in $dir, found ${#imgs[@]}"
img=${imgs[0]}
[ -f "$img" ] || die "not a regular file: $img"

for c in jq tar sha256sum cmp dd; do
	command -v "$c" >/dev/null || die "missing tool: $c"
done

files=(install.json applications.json provider.json update.json)
for f in "${files[@]}"; do
	[ -f "$dir/seed/$f" ] || die "no $dir/seed/$f"
done
if [ -f "$dir/seed/incus.json" ]; then files+=(incus.json); fi
if [ -f "$dir/seed/network.json" ]; then files+=(network.json); fi

# The seed starts at the start of partition 2 and must fit in it. The layout
# gives the partition 100 MiB; without sgdisk, keep to 16 MiB of that.
if command -v sgdisk >/dev/null 2>&1; then
	p2_start=$(sgdisk -i 2 "$img" | sed -n 's/^First sector: \([0-9]*\).*/\1/p')
	p2_end=$(sgdisk -i 2 "$img" | sed -n 's/^Last sector: \([0-9]*\).*/\1/p')
	case "$p2_start" in '' | *[!0-9]*) die "cannot read the start of partition 2 from $img" ;; esac
	case "$p2_end" in '' | *[!0-9]*) die "cannot read the end of partition 2 from $img" ;; esac
	[ "$p2_start" = "$SEED_OFFSET" ] || die "partition 2 starts at sector $p2_start, not $SEED_OFFSET"
	max_seed=$(((p2_end - p2_start + 1) * 512))
else
	max_seed=$((16 * 1024 * 1024))
fi

tmp=$(mktemp -d "$dir/seed/.set-target.XXXXXX") || die "cannot make a staging directory in $dir/seed"
trap 'rm -rf "$tmp"' EXIT
jq --arg t "$name" '.target.id = $t' "$dir/seed/install.json" > "$tmp/install.json"
for f in "${files[@]}"; do
	if [ "$f" != install.json ]; then cp -p "$dir/seed/$f" "$tmp/$f"; fi
done
tar -C "$tmp" --owner=0 --group=0 -cf "$tmp/seed.tar" "${files[@]}"
seed_bytes=$(wc -c < "$tmp/seed.tar" | tr -d '[:space:]')
[ "$seed_bytes" -le "$max_seed" ] || die "the seed ($seed_bytes bytes) does not fit in partition 2"
[ $((seed_bytes % 512)) -eq 0 ] || die "the seed ($seed_bytes bytes) is not whole 512-byte sectors"

mv -f "$tmp/install.json" "$dir/seed/install.json"
mv -f "$tmp/seed.tar" "$dir/seed/seed.tar"
dd if="$dir/seed/seed.tar" of="$img" seek="$SEED_OFFSET" bs=512 conv=notrunc status=none
dd if="$img" skip="$SEED_OFFSET" bs=512 count=$((seed_bytes / 512)) status=none \
	| cmp -s - "$dir/seed/seed.tar" || die "the seed does not read back from $img"

sha=$(sha256sum "$img" | cut -d' ' -f1)
base=$(basename "$img")
sums=$dir/SHA256SUMS
[ -f "$sums" ] || die "no $sums"
if ! awk -v f="$base" -v s="$sha" '
	{ n = $2; sub(/^\*/, "", n) }
	n == f { $0 = s "  " f; found = 1 }
	{ print }
	END { exit !found }
' "$sums" > "$sums.new"; then
	rm -f "$sums.new"
	die "no $base line in $sums"
fi
mv -f "$sums.new" "$sums"
(cd "$dir" && sha256sum -c --quiet SHA256SUMS) || die "SHA256SUMS does not check after the rewrite"

echo "target:    $name"
echo "seed:      ${files[*]} ($seed_bytes bytes)"
echo "installer: $img $(wc -c < "$img" | tr -d '[:space:]') $sha"
echo "checked:   seed reads back, SHA256SUMS checks"
