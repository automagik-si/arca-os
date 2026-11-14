#!/usr/bin/env bash
# Build the OS image in the builder, then leave the builder clean.
#
#   build-image.sh [--project arcaos-*] [--vm <name>] <worktree> [build.sh args]
#                  [--keep-tree] [--keep-artifacts N] [--dry-run]
#
# It runs `in-builder.sh <worktree> -- scripts/arca/build.sh <args>`, then
# clean-builder.sh inside the builder, sent on stdin to `bash -s` so nothing
# is left there. After a successful build the cleanup removes the work tree
# /srv/work/<worktree name> (unless --keep-tree), keeps the newest N (default
# 2) versions under /srv/artifacts/os and never the version just built nor
# autopublish's builds (they carry autopublish.json), and trims /. After a failed build it keeps the work tree for debugging, prunes
# no artifact, still trims, and exits with the build's code.
# It prints the bytes freed in the builder and the free space of the builder's
# storage pool before and after.
# --dry-run builds nothing: it prints the build it would run and the cleanup
# plan against the builder as it is, and changes nothing (a stopped builder is
# not started).
# The whole run holds the builder lock that in-builder.sh takes (see there),
# so a cleanup never races another build or a VM test.
# ARCA_IN_BUILDER overrides the in-builder.sh it runs (tests).
#
# Exit codes: the build's on a failed build; otherwise 0, 1 when the cleanup
# fails, or 2 when refused (usage, bad project, bad retention).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
PROJECT=arcaos-dev
VM=arcaos-builder
VM_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
IN_BUILDER=${ARCA_IN_BUILDER:-$here/in-builder.sh}
keep=2
keep_tree=0
dry=0

usage() { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'build-image: %s\n' "$*" >&2; }
die() { printf 'build-image: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'build-image: refused: %s\n' "$*" >&2; exit 2; }

wt=''
bargs=()
while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; PROJECT=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; VM=$2; shift 2 ;;
	--keep-tree) keep_tree=1; shift ;;
	--keep-artifacts) [ $# -ge 2 ] || usage; keep=$2; shift 2 ;;
	--dry-run) dry=1; shift ;;
	-h | --help) usage ;;
	*)
		if [ -n "$wt" ]; then
			bargs+=("$1")
		elif [ "${1#-}" != "$1" ]; then
			refuse "unknown option before the worktree: $1"
		else
			wt=$1
		fi
		shift ;;
	esac
done
[ -n "$wt" ] || usage
case "$PROJECT" in
arcaos-*) ;;
*) refuse "project '$PROJECT' does not start with arcaos-" ;;
esac
[[ "$keep" =~ ^[1-9][0-9]{0,3}$ ]] || refuse "--keep-artifacts must be a whole number from 1 to 9999, not '$keep'"
command -v jq >/dev/null || die "jq is not on PATH"

# The builder lock, as in-builder.sh takes it; in-builder.sh then does not take it again.
if [ "${ARCA_BUILDER_LOCK_HELD:-}" != 1 ]; then
	exec 9>>"${ARCA_BUILDER_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock}"
	if ! flock -n 9; then
		log "waiting for the builder lock"
		flock 9
	fi
	export ARCA_BUILDER_LOCK_HELD=1
fi

top=$(git -C "$wt" rev-parse --show-toplevel 2>/dev/null) || refuse "$wt is not a git worktree"
name=$(basename "$top")

P=(--project "$PROJECT")
instance() { incus query "/1.0/instances/$VM?project=$PROJECT"; }
pool=$(instance | jq -r '.expanded_devices.root.pool // empty') || die "no instance $VM in project $PROJECT"
[ -n "$pool" ] || die "$VM has no root disk pool"
pool_free() { incus query "/1.0/storage-pools/$pool/resources" | jq -r '.space.total - .space.used'; }
running() { [ "$(instance | jq -r .status)" = Running ]; }
clean() { incus exec -T "${P[@]}" "$VM" --env "PATH=$VM_PATH" -- bash -s -- "$@" <"$here/clean-builder.sh"; }

pool_before=$(pool_free)
echo "pool-before: $pool $pool_before bytes free"

if [ "$dry" -eq 1 ]; then
	echo "dry-run: would run: in-builder.sh $wt -- scripts/arca/build.sh${bargs[*]:+ ${bargs[*]}}"
	running || die "$VM is not running; a dry run does not start it"
	args=(--keep-artifacts "$keep" --dry-run)
	if [ "$keep_tree" -eq 0 ]; then args=(--work-tree "$name" "${args[@]}"); fi
	clean "${args[@]}"
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
set +e
"$IN_BUILDER" "${P[@]}" --vm "$VM" "$wt" -- scripts/arca/build.sh "${bargs[@]}" | tee "$tmp/build.out"
rc=${PIPESTATUS[0]}
set -e

if [ "$rc" -eq 0 ]; then
	version=$(sed -n 's/^version: //p' "$tmp/build.out" | tail -n 1)
	args=(--keep-artifacts "$keep")
	if [[ "$version" =~ ^[0-9]{12}$ ]]; then
		args+=(--keep-version "$version")
	else
		log "the build printed no version; pruning no artifact"
		args=(--no-prune)
	fi
	if [ "$keep_tree" -eq 0 ]; then args=(--work-tree "$name" "${args[@]}"); fi
	log "build ok (version ${version:-unknown}); cleaning $VM"
else
	log "build failed (exit $rc); keeping the work tree and every artifact, trimming only"
	args=(--no-prune)
fi

crc=0
if running; then
	set +e
	clean "${args[@]}" | tee "$tmp/clean.out"
	crc=${PIPESTATUS[0]}
	set -e
	echo "builder-freed: $(sed -n 's/^freed: //p' "$tmp/clean.out")"
else
	log "$VM is not running; no cleanup"
	[ "$rc" -ne 0 ] || crc=1
fi
pool_after=$(pool_free)
echo "pool-after: $pool $pool_after bytes free ($(printf '%+d' $((pool_after - pool_before))) bytes)"

if [ "$rc" -ne 0 ]; then exit "$rc"; fi
[ "$crc" -eq 0 ] || die "the build succeeded (version ${version:-unknown}) but the cleanup failed (exit $crc)"
