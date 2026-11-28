#!/usr/bin/env bash
# The build's record of its release files, so nothing changed after the build is published.
#
#   release-sums.sh write <dir>
#   release-sums.sh check <dir> <file>[=<name>]...
#
# write records the SHA-256 of every file in <dir> in <dir>/SHA256SUMS, by name, sorted, and
# makes the folder and its files read-only (mode 555 and 444). Root ignores those modes, so the
# sums are the check; the modes stop anyone else, and say the release is not to be edited.
# check compares each file with the sum the build recorded for <name> (default: its own name),
# prints one line per file that differs or that the build did not list, and exits 1 if any did,
# or when <dir> has no SHA256SUMS. It exits 2 on bad usage.
set -euo pipefail

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

case "${1:-}" in
write)
	[ $# -eq 2 ] && [ -d "$2" ] || usage
	dir=$2
	rm -f "$dir/SHA256SUMS"
	(cd "$dir" && find . -maxdepth 1 -type f ! -name 'SHA256SUMS*' -printf '%f\n' | LC_ALL=C sort | xargs -r -d '\n' sha256sum --) >"$dir/SHA256SUMS.tmp"
	mv "$dir/SHA256SUMS.tmp" "$dir/SHA256SUMS"
	find "$dir" -maxdepth 1 -type f -exec chmod 0444 {} +
	chmod 0555 "$dir"
	;;
check)
	[ $# -ge 3 ] && [ -d "$2" ] || usage
	dir=$2
	shift 2
	sums=$dir/SHA256SUMS
	if [ ! -f "$sums" ]; then
		echo "no SHA256SUMS in $dir: it was built before the sums; build it again"
		exit 1
	fi
	bad=0
	for arg in "$@"; do
		file=${arg%%=*} name=$(basename "$file")
		[ "$arg" = "$file" ] || name=${arg#*=}
		want=$(awk -v n="$name" '{ f = substr($0, 67); sub(/^\*/, "", f) } f == n { print $1; exit }' "$sums")
		if [ -z "$want" ]; then
			echo "not in the build's SHA256SUMS: $name"
			bad=1
			continue
		fi
		got=$(sha256sum -- "$file" | cut -d' ' -f1)
		if [ "$got" != "$want" ]; then
			echo "changed after the build: $name (sha256 $got, the build recorded $want)"
			bad=1
		fi
	done
	exit "$bad"
	;;
*) usage ;;
esac
