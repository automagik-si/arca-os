#!/usr/bin/env bash
# Check that an update lists every named application.
#
#   published-apps.sh <index.json> <version> <app>...
#
# Each <app> must have an "application" file named <app>.raw.gz in <version>'s
# update in <index.json>. It prints "missing <app>" for each one that does not.
#
# Exit codes: 0 when all are listed; 1 when any is missing; 2 on usage.
set -euo pipefail

[ $# -ge 3 ] || { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
idx=$1 version=$2
shift 2
rc=0
for a in "$@"; do
	jq -e --arg v "$version" --arg f "$a.raw.gz" \
		'.updates[] | select(.version == $v) | any(.files[]; .type == "application" and (.filename | split("/") | last) == $f)' \
		"$idx" >/dev/null || { echo "missing $a"; rc=1; }
done
exit "$rc"
