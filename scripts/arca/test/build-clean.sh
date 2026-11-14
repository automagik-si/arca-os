#!/usr/bin/env bash
# Tests the builder cleanup without a builder: clean-builder.sh on a temp work
# dir and artifacts dir, and build-image.sh with incus and in-builder.sh stubbed.
# fstrim is stubbed on PATH, so no real filesystem is ever trimmed.
#
#   scripts/arca/test/build-clean.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
check() { # <case> <description> <command...>
	local c=$1 d=$2
	shift 2
	if "$@"; then printf 'PASS %s: %s\n' "$c" "$d"; else printf 'FAIL %s: %s\n' "$c" "$d"; fails=$((fails + 1)); fi
}
# run <command...>: runs it, keeps its exit code in $rc and its stdout in $out.
run() {
	set +e
	out=$("$@" 2>"$t/stderr")
	rc=$?
	set -e
}
has() { grep -qxF -- "$1" <<<"$out"; }
lacks() { ! grep -qF -- "$1" <<<"$out"; }
tree() { (cd "$t/srv" && find . | LC_ALL=C sort && find . -type l -printf '%p -> %l\n' | LC_ALL=C sort); }
same() { [ "$(tree)" = "$1" ]; }
left() { [ "$(cd "$A" && LC_ALL=C ls -1)" = "$1" ]; }
trimmed() { [ -s "$t/fstrim.log" ] && [ "$(cat "$t/fstrim.log")" = '-v /' ]; }
untrimmed() { [ ! -e "$t/fstrim.log" ]; }

mkdir -p "$t/bin"
cat >"$t/bin/fstrim" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$T/fstrim.log"
echo "/: 1 GiB (1073741824 bytes) trimmed on /dev/sda2"
EOF
chmod +x "$t/bin/fstrim"
export T=$t PATH=$t/bin:$PATH
W=$t/srv/work A=$t/srv/artifacts/os
export ARCA_WORK_DIR=$W ARCA_OS_ARTIFACTS=$A

# setup: two work trees, four versions, latest at the newest, and files that are not versions.
setup() {
	rm -rf "$t/srv" "$t/outside" "$t/fstrim.log"
	mkdir -p "$W/wt-a/.cache" "$W/other" "$A" "$t/outside"
	echo x >"$W/wt-a/file"
	echo x >"$W/wt-a/.cache/pkg"
	echo x >"$t/outside/keep"
	for v in 202611100001 202611100002 202611100003 202611100004; do
		mkdir "$A/$v"
		echo "$v" >"$A/$v/IncusOS_$v.raw"
	done
	mkdir "$A/202611100005.tmp"
	echo x >"$A/notes"
	ln -s 202611100004 "$A/latest"
}
cb=$here/clean-builder.sh

setup
before=$(tree)
run "$cb" --work-tree wt-a --dry-run
check dry-run "exit 0" [ "$rc" -eq 0 ]
check dry-run "plans the work tree" has "work-tree: would remove $W/wt-a"
check dry-run "keeps the newest two, latest named" has 'artifact: keep 202611100004 (latest)'
check dry-run "keeps the second newest" has 'artifact: keep 202611100003'
check dry-run "plans the two older versions" has 'artifact: would remove 202611100002'
check dry-run "plans the oldest" has 'artifact: would remove 202611100001'
check dry-run "plans no trim" has 'fstrim: would trim /'
check dry-run "changes nothing" same "$before"
check dry-run "never runs fstrim" untrimmed

run "$cb" --work-tree wt-a
check default "exit 0" [ "$rc" -eq 0 ]
check default "removes the work tree and its cache" [ ! -e "$W/wt-a" ]
check default "leaves the other work tree" [ -d "$W/other" ]
check default "keeps the newest two and what is not a version" left $'202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'
check default "names each removal" has 'artifact: remove 202611100001'
check default "names each removal" has 'artifact: remove 202611100002'
check default "latest still points at the newest" [ "$(readlink "$A/latest")" = 202611100004 ]
check default "trims /" trimmed
check default "reports the trimmed bytes" has 'trimmed: 1073741824 bytes'
check default "reports df before and after" grep -Eq '^df-before: [0-9]+ bytes available on /$' <<<"$out"
check default "reports df after" grep -Eq '^df-after: [0-9]+ bytes available on /$' <<<"$out"
check default "reports the bytes freed" grep -Eq '^freed: -?[0-9]+ bytes$' <<<"$out"

after=$(tree)
run "$cb" --work-tree wt-a
check idempotent "exit 0" [ "$rc" -eq 0 ]
check idempotent "the work tree is absent" has "work-tree: absent $W/wt-a"
check idempotent "removes nothing" lacks 'remove'
check idempotent "the tree is unchanged" same "$after"

setup
ln -sfn 202611100001 "$A/latest"
run "$cb"
check latest-old "exit 0" [ "$rc" -eq 0 ]
check latest-old "keeps what latest points at" has 'artifact: keep 202611100001 (latest, older than the newest 2)'
check latest-old "warns about it" grep -q 'latest points at 202611100001' "$t/stderr"
check latest-old "removes only the other old one" left $'202611100001\n202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'
check latest-old "no --work-tree leaves work trees alone" [ -d "$W/wt-a" ]

setup
run "$cb" --keep-artifacts 1 --keep-version 202611100003
check keep-version "exit 0" [ "$rc" -eq 0 ]
check keep-version "keeps the version just built" has 'artifact: keep 202611100003 (just built, older than the newest 1)'
check keep-version "keeps the newest one" left $'202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'

setup
run "$cb" --keep-artifacts 3
check retention "keeps the newest three" left $'202611100002\n202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'

setup
run "$cb" --no-prune
check no-prune "exit 0" [ "$rc" -eq 0 ]
check no-prune "keeps every version" left $'202611100001\n202611100002\n202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'
check no-prune "still trims" trimmed

setup
ln -s "$t/outside" "$W/escape"
ln -s other "$W/alias"
before=$(tree)
for bad in '' . .. ../outside 'a/b' 'a b' escape alias; do
	run "$cb" --work-tree "$bad"
	check refuse "work tree '$bad' is refused with exit 2" [ "$rc" -eq 2 ]
done
for bad in 0 -1 x 2x ''; do
	run "$cb" --keep-artifacts "$bad"
	check refuse "--keep-artifacts '$bad' is refused with exit 2" [ "$rc" -eq 2 ]
done
run "$cb" --keep-version 2026
check refuse "a short --keep-version is refused with exit 2" [ "$rc" -eq 2 ]
run "$cb" --bogus
check refuse "an unknown option is refused with exit 2" [ "$rc" -eq 2 ]
check refuse "a refusal changes nothing" same "$before"
check refuse "a refusal never trims" untrimmed
check refuse "the target outside is intact" [ -f "$t/outside/keep" ]

# build-image.sh: incus runs `bash -s` here; in-builder.sh is a stub that "builds" a new version.
cat >"$t/bin/incus" <<'EOF'
#!/usr/bin/env bash
case "$1" in
query)
	case "$2" in
	/1.0/instances/*) echo '{"status": "Running", "expanded_devices": {"root": {"pool": "pool0"}}}' ;;
	/1.0/storage-pools/pool0/resources)
		n=$(cat "$T/pool" 2>/dev/null || echo 0)
		echo $((n + 1)) >"$T/pool"
		echo "{\"space\": {\"total\": 1000000, \"used\": $((500000 - n * 1000))}}" ;;
	*) exit 1 ;;
	esac ;;
exec)
	while [ "$1" != -- ]; do shift; done
	shift
	printf '%s\n' "$*" >>"$T/exec.log"
	exec "$@" ;;
*) exit 1 ;;
esac
EOF
cat >"$t/in-builder" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$T/in-builder.log"
[ "$ARCA_BUILDER_LOCK_HELD" = 1 ] || { echo "lock not held" >&2; exit 99; }
while [ "$2" != -- ]; do shift; done
mkdir -p "$ARCA_WORK_DIR/$(basename "$1")/.cache"
if [ "${BUILD_RC:-0}" -ne 0 ]; then exit "$BUILD_RC"; fi
mkdir "$ARCA_OS_ARTIFACTS/202611100009"
ln -sfn 202611100009 "$ARCA_OS_ARTIFACTS/latest"
echo "version: 202611100009"
EOF
chmod +x "$t/bin/incus" "$t/in-builder"
git init -q "$t/wt-a"
export ARCA_IN_BUILDER=$t/in-builder ARCA_BUILDER_LOCK=$t/builder.lock
bi=$here/build-image.sh
reset() { setup; rm -f "$t/pool" "$t/exec.log" "$t/in-builder.log"; }

reset
run "$bi" "$t/wt-a" --iso
check build-ok "exit 0" [ "$rc" -eq 0 ]
check build-ok "runs the build through in-builder.sh" [ "$(cat "$t/in-builder.log")" = "--project arcaos-dev --vm arcaos-builder $t/wt-a -- scripts/arca/build.sh --iso" ]
check build-ok "cleans in the builder over stdin" [ "$(cat "$t/exec.log")" = 'bash -s -- --work-tree wt-a --keep-artifacts 2 --keep-version 202611100009' ]
check build-ok "removes the work tree" [ ! -e "$W/wt-a" ]
check build-ok "keeps the version just built and the newest before it" left $'202611100004\n202611100005.tmp\n202611100009\nlatest\nnotes'
check build-ok "trims" trimmed
check build-ok "reports the bytes freed" has 'builder-freed: '"$(sed -n 's/^freed: //p' <<<"$out")"
check build-ok "reports the pool before" has 'pool-before: pool0 500000 bytes free'
check build-ok "reports the pool after" has 'pool-after: pool0 501000 bytes free (+1000 bytes)'

reset
run env BUILD_RC=3 "$bi" "$t/wt-a" --keep-artifacts 1
check build-failed "exits with the build's code" [ "$rc" -eq 3 ]
check build-failed "keeps the work tree" [ -d "$W/wt-a/.cache" ]
check build-failed "prunes no artifact" left $'202611100001\n202611100002\n202611100003\n202611100004\n202611100005.tmp\nlatest\nnotes'
check build-failed "still trims" trimmed
check build-failed "cleans with --no-prune only" [ "$(cat "$t/exec.log")" = 'bash -s -- --no-prune' ]

reset
run "$bi" "$t/wt-a" --keep-tree --keep-artifacts 3
check keep-tree "exit 0" [ "$rc" -eq 0 ]
check keep-tree "keeps the work tree" [ -d "$W/wt-a" ]
check keep-tree "keeps the newest three" left $'202611100003\n202611100004\n202611100005.tmp\n202611100009\nlatest\nnotes'

reset
mkdir -p "$W/wt-a"
before=$(tree)
run "$bi" "$t/wt-a" --dry-run
check build-dry-run "exit 0" [ "$rc" -eq 0 ]
check build-dry-run "does not build" [ ! -e "$t/in-builder.log" ]
check build-dry-run "prints the plan" has 'artifact: would remove 202611100001'
check build-dry-run "plans the work tree" has "work-tree: would remove $W/wt-a"
check build-dry-run "changes nothing" same "$before"
check build-dry-run "never trims" untrimmed

reset
run "$bi" "$t/wt-a" --keep-artifacts 0
check build-refuse "--keep-artifacts 0 is refused with exit 2" [ "$rc" -eq 2 ]
check build-refuse "nothing is built" [ ! -e "$t/in-builder.log" ]
run "$bi" --project default "$t/wt-a"
check build-refuse "a project outside arcaos-* is refused with exit 2" [ "$rc" -eq 2 ]

[ "$fails" -eq 0 ]
