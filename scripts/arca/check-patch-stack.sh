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
# <path> <class>", then "total <n>/200", where n sums added and deleted lines
# over the non-owned paths. Binary paths count as zero lines.
#
# Exit codes: 0 when nothing is REFUSED and n <= 200; 1 otherwise; 2 on usage
# or an unknown base ref.
set -euo pipefail

CAP=200
ALLOWLIST=scripts/arca/patch-allowlist.txt

usage() {
	echo "usage: check-patch-stack.sh <base-ref>" >&2
	exit 2
}

# owned <path>: returns 0 when the path is fork-owned.
owned() {
	case "$1" in
	scripts/arca/* | kernel/* | mkosi.images/arca/* | incus-osd/internal/arca/* | NOTICE) return 0 ;;
	incus-osd/api/images/update_file_component_arca_test.go) return 0 ;;
	incus-osd/api/system_network_arca_wifi.go | incus-osd/internal/tui/arca_overlay.go | incus-osd/internal/tui/arca_overlay_test.go) return 0 ;;
	esac
	[[ $1 =~ ^\.github/workflows/arca-[^/]*\.yml$ ]] && return 0
	[[ $1 =~ ^mkosi\.sandbox/etc/apt/[^/]+/arca[^/]*$ ]] && return 0
	[[ $1 =~ ^incus-osd/internal/applications/app_arca[^/]*\.go$ ]] && return 0
	[[ $1 =~ ^mkosi\.images/(base|debug)/(.*/)?arca-[^/]*$ ]] && return 0
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
