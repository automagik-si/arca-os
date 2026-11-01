#!/usr/bin/env bash
# Build the OS image in the builder (as root, from the top of a checkout).
#
#   build.sh [--iso]
#
# It removes mkosi.output/arca* first, because upstream's build target cleans
# only base*, debug* and incus*, so a stale arca sysext would otherwise ship.
# It then runs `make build` (or `make build-iso`); upstream's
# inject-system-certs target makes test certificates when there is no
# mkosi.crt. The outputs (the image .raw, the .efi, the usr images, every
# sysext .raw, the manifests and the .iso if built) are copied to
# /srv/artifacts/os/<version>/, and /srv/artifacts/os/latest points there.
# The last line printed is "version: <version>".
set -euo pipefail

ARTIFACTS=/srv/artifacts/os
target=build

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'build: %s\n' "$*" >&2; }
die() { printf 'build: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--iso) target=build-iso; shift ;;
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

start=$(date +%s)
log "removing stale arca outputs"
rm -rf mkosi.output/arca*

log "make $target (ImageId $osname)"
make "$target"

shopt -s nullglob
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
rm -rf "$dest"
mv "$dest.tmp" "$dest"
ln -sfn "$version" "$ARTIFACTS/latest"

end=$(date +%s)
echo "build-seconds: $((end - start))"
echo "image: $(du -h --apparent-size "$raw" | cut -f1) apparent, $(du -h "$raw" | cut -f1) allocated ($(basename "$raw"))"
echo "artifacts: $dest ($(du -sh "$dest" | cut -f1))"
echo "version: $version"
