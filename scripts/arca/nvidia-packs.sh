#!/usr/bin/env bash
# Report which NVIDIA driver packs a build can publish. Runs in the builder
# (through in-builder.sh), and needs objcopy and systemd-dissect.
#
#   nvidia-packs.sh <build-dir>
#
# The build's kernel is the .uname section of its UKI (<build-dir>/*.efi). For
# nvidia and nvidia-lts it prints one line:
#   <app> ok <kernel>              <app>.raw is built for the build's kernel
#   <app> missing                  the build made no <app>.raw
#   <app> kernel <pack> <build>    <app>.raw is built for another kernel
#   <app> error <reason>           <app>.raw could not be read, or not within
#                                  $NVIDIA_PACKS_TIMEOUT seconds (120)
# and then "gpu-support ok" or "gpu-support missing".
#
# Exit codes: 0 when it printed every line; 1 when the build's kernel cannot be
# read; 2 on usage.
set -euo pipefail

die() { printf 'nvidia-packs: %s\n' "$*" >&2; exit 1; }
[ $# -eq 1 ] || { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
dir=$1
[ -d "$dir" ] || die "no build directory $dir"

efis=("$dir"/*.efi)
{ [ "${#efis[@]}" -eq 1 ] && [ -f "${efis[0]}" ]; } || die "expected one UKI in $dir"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
objcopy -O binary --only-section=.uname "${efis[0]}" "$tmp" 2>/dev/null || die "cannot read the kernel release from ${efis[0]}"
build=$(tr -d '\0' <"$tmp" | tr -d '[:space:]')
[ -n "$build" ] || die "${efis[0]} has no kernel release"

for a in nvidia nvidia-lts; do
	raw=$dir/$a.raw
	if [ ! -e "$raw" ]; then
		echo "$a missing"
		continue
	fi
	# The builder's kernel lacks the pack signing key, and a signed verity setup then hangs; the node checks the signature.
	if ! pack=$(SYSTEMD_DISSECT_VERITY_SIGNATURE=0 timeout "${NVIDIA_PACKS_TIMEOUT:-120}" systemd-dissect --copy-from "$raw" /usr/share/nvidia/kernel 2>/dev/null); then
		echo "$a error its kernel stamp cannot be read"
		continue
	fi
	pack=$(printf '%s' "$pack" | tr -d '[:space:]')
	if [ "$pack" = "$build" ]; then
		echo "$a ok $build"
	else
		echo "$a kernel ${pack:-none} $build"
	fi
done
if [ -e "$dir/gpu-support.raw" ]; then echo "gpu-support ok"; else echo "gpu-support missing"; fi
