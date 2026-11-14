#!/usr/bin/env bash
# Clean the builder after a build (inside the builder, as root).
#
#   clean-builder.sh [--work-tree <name>] [--keep-artifacts N] [--keep-version <version>]
#                    [--no-prune] [--dry-run]
#
# --work-tree removes /srv/work/<name>, an in-builder.sh checkout, with its
# .cache/. The name must match [A-Za-z0-9._-]+, not be . or .., and resolve to
# itself directly under the work dir (no symlink); anything else is refused.
# Unless --no-prune, it keeps the newest N (default 2) version directories
# (12-digit names, in lexical order) under /srv/artifacts/os and removes the
# others, except the one `latest` points at (kept with a warning when it is
# older) and --keep-version (the version just built). Other entries there are
# left alone. It prints every version kept or removed.
# It then runs `fstrim -v /` and prints the space available on / before and
# after, the bytes trimmed and the bytes freed. A second run removes nothing.
# --dry-run prints the plan and changes nothing; it does not trim.
# ARCA_WORK_DIR and ARCA_OS_ARTIFACTS override the two directories (tests).
#
# Exit codes: 0, 1 when a removal or fstrim fails, 2 when refused (usage, a bad name or path).
set -euo pipefail

WORK=${ARCA_WORK_DIR:-/srv/work}
ART=${ARCA_OS_ARTIFACTS:-/srv/artifacts/os}
keep=2
tree=''
just=''
prune=1
dry=0

# It is often run as `bash -s`, so the usage is not read from $0.
usage() {
	cat >&2 <<'EOF'
usage: clean-builder.sh [--work-tree <name>] [--keep-artifacts N] [--keep-version <version>]
                        [--no-prune] [--dry-run]
EOF
	exit 2
}
warn() { printf 'clean-builder: warning: %s\n' "$*" >&2; }
die() { printf 'clean-builder: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'clean-builder: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--work-tree) [ $# -ge 2 ] || usage; tree=$2; [ -n "$tree" ] || refuse "empty work tree name"; shift 2 ;;
	--keep-artifacts) [ $# -ge 2 ] || usage; keep=$2; shift 2 ;;
	--keep-version) [ $# -ge 2 ] || usage; just=$2; shift 2 ;;
	--no-prune) prune=0; shift ;;
	--dry-run) dry=1; shift ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done
[[ "$keep" =~ ^[1-9][0-9]*$ ]] || refuse "--keep-artifacts must be a whole number of at least 1, not '$keep'"
if [ -n "$just" ] && ! [[ "$just" =~ ^[0-9]{12}$ ]]; then
	refuse "--keep-version must be a 12-digit version, not '$just'"
fi

wpath=''
if [ -n "$tree" ]; then
	[[ "$tree" =~ ^[A-Za-z0-9._-]+$ ]] || refuse "bad work tree name '$tree'"
	if [ "$tree" = . ] || [ "$tree" = .. ]; then refuse "bad work tree name '$tree'"; fi
	wroot=$(realpath -m -- "$WORK")
	wpath=$wroot/$tree
	[ "$(realpath -m -- "$wpath")" = "$wpath" ] || refuse "$wpath does not resolve to itself under $wroot"
fi

avail() { df -B1 --output=avail / | tail -n 1 | tr -d ' '; }
before=$(avail)
[ "$dry" -eq 0 ] || echo "dry-run: nothing is changed"
echo "df-before: $before bytes available on /"

if [ -n "$wpath" ]; then
	if [ ! -e "$wpath" ]; then
		echo "work-tree: absent $wpath"
	elif [ "$dry" -eq 1 ]; then
		echo "work-tree: would remove $wpath"
	else
		echo "work-tree: remove $wpath"
		rm -rf -- "$wpath" || die "could not remove $wpath"
	fi
fi

if [ "$prune" -eq 1 ] && [ -d "$ART" ]; then
	latest=''
	if [ -L "$ART/latest" ]; then
		latest=$(realpath -e -- "$ART/latest" 2>/dev/null) || warn "latest is dangling"
	fi
	versions=()
	while IFS= read -r v; do
		if [ -d "$ART/$v" ] && [ ! -L "$ART/$v" ]; then versions+=("$v"); fi
	done < <(cd "$ART" && find . -mindepth 1 -maxdepth 1 -printf '%f\n' | grep -Ex '[0-9]{12}' | LC_ALL=C sort -r)
	n=0
	for v in "${versions[@]}"; do
		n=$((n + 1))
		note=''
		if [ -n "$latest" ] && [ "$(realpath -e -- "$ART/$v")" = "$latest" ]; then note=latest; fi
		if [ "$v" = "$just" ]; then note=${note:+$note, }'just built'; fi
		if [ "$n" -le "$keep" ]; then
			echo "artifact: keep $v${note:+ ($note)}"
		elif [ -n "$note" ]; then
			echo "artifact: keep $v ($note, older than the newest $keep)"
			if [ "$v" != "$just" ]; then warn "latest points at $v, older than the newest $keep; it is kept"; fi
		elif [ "$dry" -eq 1 ]; then
			echo "artifact: would remove $v"
		else
			echo "artifact: remove $v"
			rm -rf -- "${ART:?}/$v" || die "could not remove $ART/$v"
		fi
	done
fi

if [ "$dry" -eq 1 ]; then
	echo "fstrim: would trim /"
	exit 0
fi
trim=$(fstrim -v /) || die "fstrim -v / failed"
echo "fstrim: $trim"
bytes=$(sed -nE 's/.*\(([0-9]+) bytes\).*/\1/p' <<<"$trim")
echo "trimmed: ${bytes:-0} bytes"
after=$(avail)
echo "df-after: $after bytes available on /"
echo "freed: $((after - before)) bytes"
