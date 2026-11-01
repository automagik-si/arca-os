#!/usr/bin/env bash
# Build the OS image in the builder (as root, from the top of a checkout).
#
#   build.sh [--local-kernel <dir>] [--iso]
#
# It removes mkosi.output/arca* first, because upstream's build target cleans
# only base*, debug* and incus*, so a stale arca sysext would otherwise ship.
# It then runs scripts/arca/build-arca.sh, which puts the arca binary into the
# arca sysext's tree. With --local-kernel, <dir> is a kernel build directory
# (debs/ and BUILDINFO, as kernel/build-kernel.sh writes it): its debs are
# copied into mkosi.packages/, mkosi's local repository, for the build only,
# and removed again on exit. It then runs `make build` (or `make build-iso`);
# upstream's inject-system-certs target makes test certificates when there is
# no mkosi.crt. The outputs (the image .raw, the .efi, the usr images, every
# sysext .raw, the manifests and the .iso if built) are copied to
# /srv/artifacts/os/<version>/, with this tree's test/metadata.tar.xz for
# vm-test.sh, the kernel's BUILDINFO as kernel.BUILDINFO with --local-kernel,
# and /srv/artifacts/os/latest points there.
# The last line printed is "version: <version>".
set -euo pipefail

ARTIFACTS=/srv/artifacts/os
target=build
kdir=''

usage() { sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'build: %s\n' "$*" >&2; }
die() { printf 'build: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--iso) target=build-iso; shift ;;
	--local-kernel)
		[ $# -ge 2 ] || usage
		kdir=$(readlink -f -- "$2") || die "no kernel directory $2"
		shift 2 ;;
	-h | --help) usage ;;
	*) printf 'build: unknown argument: %s\n' "$1" >&2; exit 2 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (inside the builder)"
cd "$(dirname "$0")/../.."
if [ ! -f mkosi.conf ] || [ ! -f Makefile ]; then die "not at the top of an Arca OS checkout"; fi
command -v mkosi >/dev/null || die "mkosi is not on PATH"
osname=$(sed -n 's/^ImageId=//p' mkosi.conf)
[ -n "$osname" ] || die "no ImageId in mkosi.conf"

shopt -s nullglob
kdebs=()
if [ -n "$kdir" ]; then
	[ -f "$kdir/BUILDINFO" ] || die "no BUILDINFO in $kdir"
	kdebs=("$kdir"/debs/*.deb)
	[ ${#kdebs[@]} -gt 0 ] || die "no debs in $kdir/debs"
	for d in "${kdebs[@]}"; do
		[ ! -e "mkosi.packages/$(basename "$d")" ] || die "mkosi.packages/$(basename "$d") already exists"
	done
fi
# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
remove_kernel_debs() {
	local d
	for d in "${kdebs[@]}"; do
		rm -f -- "mkosi.packages/$(basename "$d")"
	done
}
trap remove_kernel_debs EXIT

start=$(date +%s)
log "removing stale arca outputs"
rm -rf mkosi.output/arca*

log "building the arca binary"
scripts/arca/build-arca.sh

if [ -n "$kdir" ]; then
	log "local kernel: $(sed -n 's/^krel: //p' "$kdir/BUILDINFO") $(sed -n 's/^version: //p' "$kdir/BUILDINFO") (${#kdebs[@]} debs)"
	cp -- "${kdebs[@]}" mkosi.packages/
fi

log "make $target (ImageId $osname)"
make "$target"

efis=(mkosi.output/"${osname}"_*.efi)
[ ${#efis[@]} -gt 0 ] || die "no ${osname}_*.efi in mkosi.output"
version=$(printf '%s\n' "${efis[@]}" | sed -e 's/.*_//' -e 's/\.efi$//' | sort -n | tail -1)
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || die "unexpected version '$version'"

raw="mkosi.output/${osname}_${version}.raw"
[ -f "$raw" ] || die "no $raw"
files=("$raw" "mkosi.output/${osname}_${version}.efi")
usr=(mkosi.output/"${osname}_${version}".usr-*)
[ ${#usr[@]} -gt 0 ] || die "no usr images for $version"
files+=("${usr[@]}")
for f in mkosi.output/*.raw; do
	case "$(basename "$f")" in "${osname}"_*) continue ;; esac
	files+=("$f")
done
files+=(mkosi.output/*.manifest*)
files+=(test/metadata.tar.xz)
if [ "$target" = build-iso ]; then
	iso="mkosi.output/${osname}_${version}.iso"
	[ -f "$iso" ] || die "no $iso"
	files+=("$iso")
fi

dest="$ARTIFACTS/$version"
log "copying ${#files[@]} files to $dest"
rm -rf "$dest.tmp"
install -d -m 0755 "$dest.tmp"
cp --sparse=always -- "${files[@]}" "$dest.tmp/"
if [ -n "$kdir" ]; then
	cp -- "$kdir/BUILDINFO" "$dest.tmp/kernel.BUILDINFO"
fi
rm -rf "$dest"
mv "$dest.tmp" "$dest"
ln -sfn "$version" "$ARTIFACTS/latest"

end=$(date +%s)
echo "build-seconds: $((end - start))"
echo "image: $(du -h --apparent-size "$raw" | cut -f1) apparent, $(du -h "$raw" | cut -f1) allocated ($(basename "$raw"))"
echo "artifacts: $dest ($(du -sh "$dest" | cut -f1))"
echo "version: $version"
