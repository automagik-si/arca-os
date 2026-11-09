#!/usr/bin/env bash
# Checks the fork's changes against the upstream patch-stack rules.
#
#   check-patch-stack.sh <base-ref>
#
# Every path that changed between <base-ref> and HEAD is classed as one of:
#   owned    fork-owned (the Arca-owned paths below); not counted
#   allowed  upstream, and listed in scripts/arca/patch-allowlist.txt
#   REFUSED  upstream, and not listed
# The allowlist is read as committed at HEAD, so a working-tree edit cannot
# admit a path. A commit in the range can still admit one, so every path the
# range adds to the allowlist is printed as "admitted <path>" for review. It prints one row per non-owned path, "<added> <deleted>
# <path> <class>", then "total <n>/<cap>", where n sums added and deleted lines
# over the non-owned paths. Binary paths count as zero lines.
#
# Exit codes: 0 when nothing is REFUSED and n <= the cap (1300); 1 otherwise; 2 on usage
# or an unknown base ref.
set -euo pipefail

# Arca + NVIDIA + update-health edits to upstream files; was 900 before automatic updates.
CAP=1300
ALLOWLIST=scripts/arca/patch-allowlist.txt

usage() {
	echo "usage: check-patch-stack.sh <base-ref>" >&2
	exit 2
}

# owned <path>: returns 0 when the path is fork-owned.
owned() {
	case "$1" in
	scripts/arca/* | kernel/* | mkosi.images/arca/* | incus-osd/internal/arca/* | NOTICE) return 0 ;;
	doc/reference/arca-*.md) return 0 ;;
	incus-osd/api/images/update_file_component_arca_test.go) return 0 ;;
	incus-osd/api/system_network_arca_wifi.go | incus-osd/internal/tui/arca_overlay.go | incus-osd/internal/tui/arca_overlay_test.go) return 0 ;;
	incus-osd/internal/tui/arca_console.go | incus-osd/internal/tui/arca_console_test.go) return 0 ;;
	incus-osd/api/system_update_arca.go) return 0 ;;
	esac
	[[ $1 =~ ^\.github/workflows/arca-[^/]*\.yml$ ]] && return 0
	[[ $1 =~ ^mkosi\.sandbox/etc/apt/[^/]+/arca[^/]*$ ]] && return 0
	[[ $1 =~ ^incus-osd/internal/applications/app_arca[^/]*\.go$ ]] && return 0
	[[ $1 =~ ^incus-osd/internal/(update|util|rest)/arca_[^/]*\.go$ ]] && return 0
	[[ $1 =~ ^doc/reference/system/arca-[^/]*\.md$ ]] && return 0
	[[ $1 =~ ^mkosi\.images/(base|debug)/(.*/)?arca-[^/]*$ ]] && return 0
	nvidia_owned "$1" && return 0
	return 1
}

# nvidia_owned <path>: returns 0 for the NVIDIA driver line's own files.
nvidia_owned() {
	case "$1" in
	doc/reference/applications/nvidia.md | incus-osd/api/application_nvidia.go) return 0 ;;
	incus-osd/internal/applications/nvidia_sysext_test.go | incus-osd/internal/applications/testdata/nvidia-firmware.txt) return 0 ;;
	incus-osd/internal/applications/app_gpu_support_test.go | incus-osd/internal/update/nvidia_test.go) return 0 ;;
	incus-osd/internal/kernel/validate.go | incus-osd/internal/kernel/passthrough_test.go) return 0 ;;
	incus-osd/internal/rest/api_system_kernel_test.go | incus-osd/internal/state/persistence_test.go) return 0 ;;
	incus-osd/internal/systemd/nvidia.go | incus-osd/internal/systemd/nvidia_test.go) return 0 ;;
	incus-osd/internal/systemd/module_signature.go | incus-osd/internal/systemd/module_signature_test.go) return 0 ;;
	mkosi.images/nvidia/* | mkosi.images/nvidia-lts/*) return 0 ;;
	mkosi.images/base/mkosi.extra/usr/lib/modprobe.d/50-gpu-boot.conf) return 0 ;;
	mkosi.images/base/mkosi.extra/usr/lib/systemd/system/nvidia-prepare.service) return 0 ;;
	mkosi.images/base/mkosi.extra/usr/lib/systemd/system/sysinit.target.wants/nvidia-prepare.service) return 0 ;;
	mkosi.images/base/mkosi.extra/usr/lib/systemd/system/systemd-sysext.service.d/nvidia.conf) return 0 ;;
	esac
	[[ $1 =~ ^incus-osd/internal/applications/app_nvidia[^/]*\.go$ ]] && return 0
	return 1
}

[ $# -eq 1 ] || usage
base=$1
git rev-parse --verify --quiet "$base^{commit}" >/dev/null || {
	echo "check-patch-stack: unknown base ref" >&2
	exit 2
}

allow=$(git show "HEAD:$ALLOWLIST" 2>/dev/null | sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -v '^$' || true)
numstat=$(git -c core.quotePath=false diff --no-renames --numstat "$base..HEAD")

total=0
refused=0
while IFS=$'\t' read -r added deleted path; do
	[ -n "$path" ] || continue
	owned "$path" && continue
	if [ "$added" = - ]; then added=0; fi
	if [ "$deleted" = - ]; then deleted=0; fi
	cls=allowed
	if ! grep -Fxq -- "$path" <<<"$allow"; then
		cls=REFUSED
		refused=$((refused + 1))
	fi
	total=$((total + added + deleted))
	printf '%s %s %s %s\n' "$added" "$deleted" "$path" "$cls"
done <<<"$numstat"

git diff -U0 "$base..HEAD" -- "$ALLOWLIST" | sed -n 's/^+\([^+]\)/\1/p' |
	sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -v '^$' |
	while IFS= read -r path; do printf 'admitted %s\n' "$path"; done || true
printf 'total %s/%s\n' "$total" "$CAP"
if [ "$refused" -eq 0 ] && [ "$total" -le "$CAP" ]; then
	exit 0
fi
exit 1
