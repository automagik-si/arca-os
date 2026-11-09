#!/usr/bin/env bash
# Run a command inside the builder VM on a clean copy of a worktree.
#
#   in-builder.sh [--project arcaos-*] [--vm <name>] <worktree> -- <cmd...>
#
# It refuses a dirty worktree (any output from `git status --porcelain`). It
# ships HEAD with `git bundle`, checks it out at /srv/work/<worktree basename>
# in the VM (reset and cleaned with `git clean -ffdx`, keeping only the build
# cache .cache/), and runs the command there as root. It exits with the
# command's exit code. Build outputs persist under /srv/artifacts/ in the VM.
# The worktree's git user.name and user.email, when both are set, become the
# VM checkout's local identity (passed on stdin, never printed), so a build
# there can read them; otherwise the VM checkout has none.
# A stopped builder is started first, if the host has the memory for it.
# From the worktree check on, the run holds the builder lock, a flock on
# $ARCA_BUILDER_LOCK (default ${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock),
# so one builder or test VM run goes at a time; while another run holds it, it
# prints "waiting for the builder lock" and waits. A caller that holds the lock
# already sets ARCA_BUILDER_LOCK_HELD=1, and the run does not take it again.
#
# Exit codes: the command's, or 2 when refused (dirty tree, bad project, usage).
set -euo pipefail

PROJECT=arcaos-dev
VM=arcaos-builder
# The builder takes 16 GiB; start it only with 4 GiB of headroom on top.
MIN_MEM_GIB=20
# /root/go/bin holds the tools make static-analysis installs (swagger, golangci-lint).
VM_PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/root/go/bin

usage() { sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'in-builder: %s\n' "$*" >&2; }
die() { printf 'in-builder: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'in-builder: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; PROJECT=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; VM=$2; shift 2 ;;
	-h | --help) usage ;;
	--) usage ;;
	-*) refuse "unknown option: $1" ;;
	*) break ;;
	esac
done
if [ $# -lt 3 ] || [ "$2" != -- ]; then usage; fi
WT=$1
shift 2

case "$PROJECT" in
arcaos-*) ;;
*) refuse "project '$PROJECT' does not start with arcaos-" ;;
esac

# The builder lock (see the header), before anything reads the worktree or calls incus.
if [ "${ARCA_BUILDER_LOCK_HELD:-}" != 1 ]; then
	exec 9>>"${ARCA_BUILDER_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock}"
	if ! flock -n 9; then
		log "waiting for the builder lock"
		flock 9
	fi
	export ARCA_BUILDER_LOCK_HELD=1
fi

top=$(git -C "$WT" rev-parse --show-toplevel 2>/dev/null) || refuse "$WT is not a git worktree"
if [ -n "$(git -C "$top" status --porcelain)" ]; then
	refuse "worktree $top is dirty (commit or stash first)"
fi
name=$(basename "$top")
printf '%s' "$name" | grep -Eqx '[A-Za-z0-9._-]+' || refuse "bad worktree name '$name'"
sha=$(git -C "$top" rev-parse HEAD)

P=(--project "$PROJECT")

state=$(incus list "${P[@]}" -f csv -c ns | awk -F, -v n="$VM" '$1 == n { print $2 }')
[ -n "$state" ] || die "no instance $VM in project $PROJECT (run dev-vm.sh create)"
if [ "$state" != RUNNING ]; then
	avail=$(awk '/^MemAvailable:/ { printf "%d", $2 / 1048576 }' /proc/meminfo)
	[ "$avail" -ge "$MIN_MEM_GIB" ] || die "MemAvailable is $avail GiB, below $MIN_MEM_GIB GiB; not starting $VM"
	log "starting $VM"
	incus start "${P[@]}" "$VM"
fi
for i in $(seq 1 60); do
	incus exec "${P[@]}" "$VM" -- true >/dev/null 2>&1 && break
	[ "$i" -lt 60 ] || die "$VM: no incus agent after 300 s"
	sleep 5
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git -C "$top" bundle create -q "$tmp/head.bundle" HEAD
incus file push -q -p "${P[@]}" "$tmp/head.bundle" "$VM/srv/work/.bundles/$name.bundle"
rm -rf "$tmp"

gname=$(git -C "$top" config user.name 2>/dev/null || true)
gemail=$(git -C "$top" config user.email 2>/dev/null || true)
if [ -z "$gname" ] || [ -z "$gemail" ]; then gname='' gemail=''; fi

log "checking out $name at ${sha:0:12} in $VM:/srv/work/$name"
# shellcheck disable=SC2016  # expanded by the shell in the VM
printf '%s\n%s\n' "$gname" "$gemail" | incus exec "${P[@]}" "$VM" --env "PATH=$VM_PATH" -- sh -euc '
	name=$1 sha=$2
	IFS= read -r gname || true
	IFS= read -r gemail || true
	b=/srv/work/.bundles/$name.bundle
	d=/srv/work/$name
	install -d -m 0755 /srv/artifacts
	if [ ! -d "$d/.git" ]; then
		rm -rf "$d"
		git init -q "$d"
	fi
	cd "$d"
	git bundle verify -q "$b" >/dev/null
	git fetch -q --force "$b" "HEAD:refs/arca/in-builder"
	git checkout -q -f --detach "$sha"
	git reset -q --hard "$sha"
	git clean -q -ffdx -e /.cache/
	if [ -n "$gname" ] && [ -n "$gemail" ]; then
		git config user.name "$gname"
		git config user.email "$gemail"
	else
		git config --unset-all user.name || true
		git config --unset-all user.email || true
	fi
	rm -f "$b"
	test "$(git rev-parse HEAD)" = "$sha"
	test -z "$(git status --porcelain)"
' in-builder "$name" "$sha"

set +e
incus exec "${P[@]}" "$VM" --cwd "/srv/work/$name" --env HOME=/root --env "PATH=$VM_PATH" -- "$@"
rc=$?
set -e
exit "$rc"
