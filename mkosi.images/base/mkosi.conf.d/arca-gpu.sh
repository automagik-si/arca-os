#!/bin/sh
# Base image build script: the Intel i915 GPU firmware.
#
# The i915 driver is built as a module and loads its GuC, HuC and DMC firmware
# at runtime; without it in the image the GPU is left wedged at boot. This
# installs every firmware file the kernel's i915 module declares
# (modinfo -F firmware i915), from the linux-firmware checkout the base image
# already uses for its other firmware, into /usr/lib/firmware, keeping the
# "i915/" path each name carries.
#
# It fails the build when
#   1. a declared firmware file is not in the checkout;
#   2. no listed module is found, or a module declares no firmware;
#   3. a required early-boot file (adlp_guc_70.bin, adlp_dmc.bin, tgl_huc.bin,
#      the ones this machine's GPU needs) is not installed;
#   4. a declared name is an absolute path or has a ".." component.
# A modinfo that fails stops it too. It writes the installed list, headed by
# the line "gpu-firmware: <files> files, <bytes> bytes raw" that it also
# prints, to /usr/share/arca-gpu/firmware.txt.
#
# The environment can point it elsewhere (a fixture test does):
# ARCA_GPU_FW_SRC (the checkout), DESTDIR (the tree it installs into),
# ARCA_GPU_MODULES (the kernel's module directory), ARCA_GPU_MODINFO (the
# modinfo command) and ARCA_GPU_MODULES_LIST (the modules to read, default
# "i915").
set -eu

die() { echo "arca-gpu: $*" >&2; exit 1; }
PATH=$PATH:/usr/sbin:/sbin
src=${ARCA_GPU_FW_SRC:-/work/src/app-build/linux-firmware}
modinfo=${ARCA_GPU_MODINFO:-modinfo}
modules_list=${ARCA_GPU_MODULES_LIST:-i915}
moddir=${ARCA_GPU_MODULES:-}
if [ -z "$moddir" ]; then
	set -- /buildroot/usr/lib/modules/*
	if [ $# -ne 1 ] || [ ! -d "$1" ]; then echo "arca-gpu: want one kernel in /buildroot/usr/lib/modules" >&2; exit 1; fi
	moddir=$1
fi
dest=${DESTDIR:?}/usr/lib/firmware
out=$DESTDIR/usr/share/arca-gpu/firmware.txt
[ -f "$src/WHENCE" ] || die "no WHENCE in $src (not the firmware checkout)"
[ -d "$moddir/kernel" ] || die "no kernel/ in $moddir"

# These are the files this machine's Alder Lake GPU asks for at boot; a build
# that somehow dropped them must fail rather than ship a wedged GPU.
required="i915/adlp_guc_70.bin i915/adlp_dmc.bin i915/tgl_huc.bin"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Rule 2: every listed module must exist and declare firmware.
: > "$tmp/declared"
for mod in $modules_list; do
	ko=$(find "$moddir/kernel" -name "$mod.ko" -o -name "$mod.ko.*" 2>/dev/null | head -1)
	[ -n "$ko" ] || die "no module $mod under $moddir/kernel"
	"$modinfo" -F firmware "$ko" >> "$tmp/declared" || die "modinfo -F firmware $mod failed"
done
sort -u "$tmp/declared" > "$tmp/uniq"
[ -s "$tmp/uniq" ] || die "$modules_list declares no firmware"

# Install each declared file, collecting errors (rules 1 and 4).
files=0 bytes=0
: > "$tmp/list"
: > "$tmp/errors"
while IFS= read -r f; do
	case "$f" in
	/* | *../* | */.. | ..) echo "4. unsafe path $f" >> "$tmp/errors"; continue ;;
	esac
	if [ -f "$src/$f" ]; then
		mkdir -p "$dest/$(dirname "$f")"
		cp -L "$src/$f" "$dest/$f"
		chmod 0644 "$dest/$f"
		files=$((files + 1))
		bytes=$((bytes + $(wc -c < "$dest/$f")))
		printf 'f %s\n' "$f" >> "$tmp/list"
	else
		echo "1. declared $f is not in the firmware checkout" >> "$tmp/errors"
	fi
done < "$tmp/uniq"

# Rule 3: the required early-boot files must be installed.
for r in $required; do
	[ -f "$dest/$r" ] || echo "3. required $r not installed" >> "$tmp/errors"
done
[ "$files" -gt 0 ] || echo "2. no firmware installed" >> "$tmp/errors"

if [ -s "$tmp/errors" ]; then
	sed 's/^/arca-gpu: incomplete: /' "$tmp/errors" >&2
	exit 1
fi

line="gpu-firmware: $files files, $bytes bytes raw"
mkdir -p "$(dirname "$out")"
{ echo "$line"; cat "$tmp/list"; } > "$out"
chmod 0644 "$out"
echo "$line"
