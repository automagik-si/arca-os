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
# It also copies the base image's Wi-Fi firmware list (usr/share/arca-wifi/
# firmware.txt in the base tree) there as wifi-firmware.txt and prints its
# first line, "wifi-firmware: <files> files, <bytes> bytes raw". It reads the
# usr erofs filesystem's block count and block size from its superblock (the
# usr image file is always the partition's full size), prints
# "usr: <MiB> MiB used of <MiB> MiB" and the usr image and UKI file sizes, and
# exits 1 before copying anything when usr uses more than 900 MiB.
# The last line printed is "version: <version>".
set -euo pipefail

ARTIFACTS=/srv/artifacts/os
USR_MAX_MIB=900
target=build
kdir=''

usage() { sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
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

# Embed the persistent test CA. This copies the one, stable CA into the
# checkout before `make`, so upstream's generate-test-certs never regenerates
# one, and every build embeds the same update CA and Secure Boot signing key
# that nodes already installed from an earlier build trust. It refuses the
# build when the CA set is missing rather than silently making a throwaway one.
log "injecting the persistent test CA"
scripts/arca/inject-test-ca.sh

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

fwlist=mkosi.output/base/usr/share/arca-wifi/firmware.txt
[ -s "$fwlist" ] || die "no $fwlist (the base image's Wi-Fi firmware list)"
head -n 1 "$fwlist"
# The usr partition is a fixed size, so its image file says nothing about use.
# erofs keeps its superblock at byte 1024: blkszbits (u8) at +12, blocks
# (le32) at +36.
usrfs=''
for f in "${usr[@]}"; do case "$f" in *-verity*) ;; *) usrfs=$f ;; esac; done
[ -n "$usrfs" ] || die "no usr filesystem image for $version"
sb() { od -An --endian=little -t "$1" -j "$2" -N "$3" "$usrfs" 2>/dev/null | tr -d ' \n' || true; }
# The superblock is 128 bytes; a block is 512 bytes to 64 KiB.
[ "$(stat -c %s "$usrfs")" -ge 1152 ] || die "$usrfs is too short for an erofs superblock"
[ "$(sb x4 1024 4)" = e0f5e1e2 ] || die "$usrfs has no erofs superblock"
usr_blocks=$(sb u4 1060 4) usr_blkszbits=$(sb u1 1036 1)
if ! [[ "$usr_blocks" =~ ^[0-9]+$ && "$usr_blkszbits" =~ ^[0-9]+$ ]] || [ "$usr_blkszbits" -lt 9 ] || [ "$usr_blkszbits" -gt 16 ]; then
	die "$usrfs: short or bad erofs superblock (blocks '$usr_blocks', blkszbits '$usr_blkszbits')"
fi
usr_bytes=$(( usr_blocks << usr_blkszbits ))
usr_size=$(stat -c %s "$usrfs")
awk -v u="$usr_bytes" -v s="$usr_size" 'BEGIN { printf "usr: %.1f MiB used of %d MiB\n", u / 1048576, s / 1048576 }'
uki="mkosi.output/${osname}_${version}.efi"
echo "usr-image: $(basename "$usrfs") $usr_size bytes, $usr_bytes bytes used"
echo "uki: $(basename "$uki") $(stat -c %s "$uki") bytes"
[ "$usr_bytes" -le $((USR_MAX_MIB * 1048576)) ] || die "usr uses more than $USR_MAX_MIB MiB"
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
cp -- "$fwlist" "$dest.tmp/wifi-firmware.txt"
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
