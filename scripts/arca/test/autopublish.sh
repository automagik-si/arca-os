#!/usr/bin/env bash
# Tests autopublish.sh's decisions without a builder, VM or network: the
# builder, publish-update.sh, canary.sh and incus are stubs over a fake
# builder filesystem, and git is real. It checks that a failed canary never
# promotes (also not on later runs), a passed canary promotes the same build,
# an inconclusive canary turns into a failure after CANARY_TRIES runs, a canary
# that dies without a verdict or a failure the tree could not record is never
# promoted later, a failed build or publish is removed and backed off, low
# space refuses a build, builds never move the latest link, a busy builder
# skips the run, a dry run calls nothing, and the NVIDIA drivers the build
# made are published unless APPS is set.
#
#   scripts/arca/test/autopublish.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
ok() { printf 'PASS %s\n' "$1"; }
bad() { printf 'FAIL %s: %s\n' "$1" "$2"; fails=$((fails + 1)); }
check() { # <case> <description> <command...>
	local c=$1 d=$2
	shift 2
	if "$@"; then ok "$c: $d"; else bad "$c" "$d"; fi
}

mkdir -p "$t/bin" "$t/builder/srv/artifacts/updates" "$t/serve" "$t/cache" "$t/run" "$t/state" "$t/config/arca-os"
echo STOPPED >"$t/builder.state"
: >"$t/calls"

# reindex <tree>: index.json from every <ver>/update.json, newest first.
cat >"$t/bin/reindex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
tree=$1
find "$tree" -mindepth 2 -maxdepth 2 -name update.json -print0 | xargs -0 -r cat |
	jq -s '{format: "1.0", updates: (sort_by(.version) | reverse | map(. + {url: ("/" + .version)}))}' >"$tree/index.json"
cp "$tree/index.json" "$tree/index.sjson"
EOF
# set_channels <tree> <ver> <jq filter on .channels>
cat >"$t/bin/set-channels" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
f=$1/$2/update.json
jq ".channels |= ($3)" "$f" >"$f.new" && mv "$f.new" "$f"
reindex "$1"
EOF

# The fake incus: the builder's paths live under $T/builder.
cat >"$t/bin/incus" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
B=$T/builder
args=()
for a in "$@"; do case "$a" in --project | -q | -p) ;; arcaos-dev) ;; *) args+=("$a") ;; esac; done
set -- "${args[@]}"
case "$1" in
list) echo "arcaos-builder,$(cat "$T/builder.state")" ;;
config)
	case "$*" in
	*limits.cpu*) echo 4 ;; *limits.read*) echo 150MB ;; *limits.write*) echo 80MB ;; *) exit 1 ;;
	esac ;;
storage) # storage info <pool> --bytes
	printf 'info:\n  total space: "%s"\n  space used: "%s"\n' $((1000 * 1073741824)) $(((1000 - $(cat "$T/pool-free-gib")) * 1073741824)) ;;
stop) echo "incus stop $2" >>"$T/calls"; echo STOPPED >"$T/builder.state" ;;
delete) echo "incus delete $*" >>"$T/calls" ;;
file) # file push <src> <vm>/<path>
	dst=$B/${4#*/}; mkdir -p "$(dirname "$dst")"; cp "$3" "$dst" ;;
exec) # exec <vm> -- <cmd> ...
	shift 3
	case "$1" in
	tar) tar -C "$B$3" -cf - . ;;
	ls) ls -1 "$B$3" ;;
	sha256sum) sha256sum "$B$2" ;;
	df) printf 'Avail\n%s\n' $(($(cat "$T/builder-free-gib") * 1073741824)) ;;
	test) test -f "$B$3" ;;
	rm) echo "builder rm $3" >>"$T/calls"; rm -rf "${B:?}$3" ;;
	sh) [ -d "$B/srv/artifacts/os/$(cat "$T/next-version")" ] && echo '{}' >"$B/srv/artifacts/os/$(cat "$T/next-version")/autopublish.json" ;;
	*) exit 1 ;;
	esac ;;
profile) case "$*" in *pool*) echo crucial ;; *) echo incusbr0 ;; esac ;;
network) echo 127.0.0.1/24 ;;
*) exit 1 ;;
esac
EOF

# The fake in-builder.sh: builds and the tree commands act on the fake builder.
cat >"$t/bin/in-builder" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
B=$T/builder
while [ "$1" != -- ]; do shift; done
shift
echo RUNNING >"$T/builder.state"
echo "in-builder $*" >>"$T/calls"
case "$1" in
true) ;;
scripts/arca/build.sh)
	v=$(cat "$T/next-version")
	[ ! -e "$T/build-broken" ] || exit 1
	mkdir -p "$B/srv/artifacts/os/$v"
	[ ! -e "$T/build-nvidia" ] || touch "$B/srv/artifacts/os/$v/nvidia.raw" "$B/srv/artifacts/os/$v/nvidia-lts.raw"
	echo "version: $v" ;;
scripts/arca/release-tree.sh)
	tree=$B$3 cmd=$4
	shift 4
	declare -A o=()
	while [ $# -gt 0 ]; do o[$1]=$2; shift 2; done
	v=${o[--version]:-}
	case "$cmd" in
	notes) cp "$B${o[--file]}" "$tree/$v/notes.json" ;;
	promote)
		[ ! -e "$T/record-broken" ] || exit 1
		if [ -f "$tree/$v/canary.json" ] && [ "$(jq -r .state "$tree/$v/canary.json")" = failed ]; then exit 2; fi
		jq -n --arg v "$v" '{version: $v, state: "passed", reason: ""}' >"$tree/$v/canary.json"
		set-channels "$tree" "$v" '. + ["stable"] | unique' ;;
	fail)
		# Like the real one: a reason is required, and recording can fail.
		[ -n "${o[--reason]:-}" ] || exit 2
		[ ! -e "$T/record-broken" ] || exit 1
		jq -n --arg v "$v" --arg r "${o[--reason]}" '{version: $v, state: "failed", reason: $r}' >"$tree/$v/canary.json"
		set-channels "$tree" "$v" 'map(select(. != "testing"))' ;;
	prune)
		for p in ${o[--remove]//,/ }; do rm -rf "${tree:?}/$p"; done
		reindex "$tree" ;;
	esac ;;
*) exit 1 ;;
esac
EOF

# The fake publish-update.sh: adds the release to the builder tree in testing, then copies the tree.
cat >"$t/bin/publish-update" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
declare -A o=()
while [ $# -gt 0 ]; do o[$1]=$2; shift 2; done
v=${o[--version]} tree=$T/builder${o[--builder-tree]}
echo "publish-update $v ${o[--channel]} ${o[--apps]}" >>"$T/calls"
mkdir -p "$tree/$v"
jq -n --arg v "$v" --arg c "${o[--channel]}" '{format: "1.0", version: $v, channels: [$c], severity: "none", files: []}' >"$tree/$v/update.json"
reindex "$tree"
tar -C "$tree" -cf - . | tar -C "${o[--serve-dir]}" -xf -
[ ! -e "$T/publish-broken" ] || exit 1
EOF

# The fake canary.sh: its verdict comes from $T/canary-mode.
cat >"$t/bin/canary" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "canary $*" >>"$T/calls"
case "$(cat "$T/canary-mode")" in
pass) echo "PASS version"; echo "result: passed"; exit 0 ;;
fail) echo "FAIL arca arca.service is not active after 600 s"; echo "result: failed"; echo "reason: arca.service is not active after 600 s"; exit 1 ;;
fail-noreason) echo "result: failed"; exit 1 ;;
die-target) echo "phase: target"; exit 1 ;;
die-baseline) exit 1 ;;
*) echo "result: inconclusive"; echo "reason: the base canary did not boot"; exit 3 ;;
esac
EOF
chmod +x "$t"/bin/*

# A stable release to start from, in the builder and served.
mkdir -p "$t/builder/srv/artifacts/updates/202611010000"
jq -n '{format: "1.0", version: "202611010000", channels: ["stable"], severity: "none", files: []}' \
	>"$t/builder/srv/artifacts/updates/202611010000/update.json"
PATH=$t/bin:$PATH reindex "$t/builder/srv/artifacts/updates"
tar -C "$t/builder/srv/artifacts/updates" -cf - . | tar -C "$t/serve" -xf -

# The publish branch, and the build checkout cloned from it.
git init -q -b main "$t/origin"
git -C "$t/origin" -c user.name=t -c user.email=t@t commit -q --allow-empty -m "first change"
git clone -q "$t/origin" "$t/checkout"
# The script runs from a throwaway tools repository, so its tools worktree is made there.
mkdir -p "$t/tools/scripts/arca"
cp "$here/autopublish.sh" "$t/tools/scripts/arca/"
git init -q "$t/tools"
git -C "$t/tools" add -A
git -C "$t/tools" -c user.name=t -c user.email=t@t commit -q -m tools
ap=$t/tools/scripts/arca/autopublish.sh
commit() { git -C "$t/origin" -c user.name=t -c user.email=t@t commit -q --allow-empty -m "$1"; }

export T=$t PATH=$t/bin:$PATH XDG_CACHE_HOME=$t/cache XDG_RUNTIME_DIR=$t/run XDG_STATE_HOME=$t/state XDG_CONFIG_HOME=$t/config
export ARCA_BUILDER_LOCK=$t/cache/arcaos-builder.lock
export ARCA_AUTOPUBLISH_IN_BUILDER=$t/bin/in-builder ARCA_AUTOPUBLISH_PUBLISH_UPDATE=$t/bin/publish-update ARCA_AUTOPUBLISH_CANARY=$t/bin/canary
printf 'CANARY_TRIES=2\n' >"$t/config/arca-os/autopublish.conf"
echo 500 >"$t/pool-free-gib"
echo 100 >"$t/builder-free-gib"
run() { # <canary mode> <next version>: runs autopublish, sets rc and out
	echo "$1" >"$t/canary-mode"
	echo "$2" >"$t/next-version"
	: >"$t/calls"
	set +e
	out=$("$ap" --serve-dir "$t/serve" --checkout "$t/checkout" --remote origin --branch main </dev/null 2>&1)
	rc=$?
	set -e
}
idx=$t/serve/index.json
channels() { jq -r --arg v "$1" '.updates[] | select(.version == $v) | .channels | join(",")' "$idx"; }
called() { grep -q -- "$1" "$t/calls"; }
not_called() { ! grep -q -- "$1" "$t/calls"; }
jqe() { jq -e "$@" >/dev/null; }
state() { jq -r .state "$t/serve/$1/canary.json" 2>/dev/null || echo none; }

# 1. A failed canary: marked failed, out of testing, never in stable.
commit "merge: a change that breaks arca"
run fail 202611100001
check failed-canary "exits 1" [ "$rc" -eq 1 ]
check failed-canary "built once" [ "$(grep -c 'build.sh' "$t/calls")" -eq 1 ]
check failed-canary "published into testing" called "publish-update 202611100001 testing"
check failed-canary "release-tree fail was called" called "release-tree.sh --tree /srv/artifacts/updates fail --version 202611100001"
check failed-canary "release-tree promote was never called" not_called "promote"
check failed-canary "not in stable or testing" [ -z "$(channels 202611100001)" ]
check failed-canary "canary.json says failed" [ "$(state 202611100001)" = failed ]
check failed-canary "the reason is recorded" grep -q "arca.service is not active" "$t/serve/202611100001/canary.json"
check failed-canary "stable is unchanged" [ "$(jq -r '[.updates[] | select(.channels | index("stable"))][0].version' "$idx")" = 202611010000 ]
check failed-canary "the builder is stopped" [ "$(cat "$t/builder.state")" = STOPPED ]
check failed-canary "notes.json lists the merge subject" jqe '.changes | index("merge: a change that breaks arca")' "$t/serve/202611100001/notes.json"
check failed-canary "the build never moves the latest link" called "build.sh --no-latest"
check failed-canary "the verdict is saved before the tree changes" [ "$(jq -r .state "$t/state/arca-os/autopublish/verdicts/202611100001.json")" = failed ]
check failed-canary "published without NVIDIA when the build made none" called "publish-update 202611100001 testing incus,arca$"
check failed-canary "notes.json says NVIDIA is not included" jqe '.not_included[0] | test("NVIDIA drivers \\(nvidia, nvidia-lts\\)")' "$t/serve/202611100001/notes.json"

# 2. The next run, with the canary now passing and no new commit: nothing.
run pass 202611100002
check failed-stays "exits 0" [ "$rc" -eq 0 ]
check failed-stays "nothing is built, tested or promoted" [ ! -s "$t/calls" ]
check failed-stays "the failed release is still not in stable" [ -z "$(channels 202611100001)" ]

# 3. A new commit whose canary passes: the same build is promoted.
commit "merge: fix arca"
run pass 202611100003
check passed-canary "exits 0" [ "$rc" -eq 0 ]
check passed-canary "built once" [ "$(grep -c 'build.sh' "$t/calls")" -eq 1 ]
check passed-canary "canary from the stable release" called "canary .*--base 202611010000 --target 202611100003"
check passed-canary "promote of the built version" called "release-tree.sh --tree /srv/artifacts/updates promote --version 202611100003"
check passed-canary "in stable and testing" [ "$(channels 202611100003)" = stable,testing ]
check passed-canary "the failed release is still not in stable" [ -z "$(channels 202611100001)" ]
check passed-canary "tree commands ran from the tools worktree" called "in-builder scripts/arca/release-tree.sh"
check passed-canary "the tools worktree is at the tools commit" [ "$(git -C "$t/cache/arca-os/autopublish/arca-tools" rev-parse HEAD)" = "$(git -C "$t/tools" rev-parse HEAD)" ]
check passed-canary "notes cover only the new commit" [ "$(jq -c .changes "$t/serve/202611100003/notes.json")" = '["merge: fix arca"]' ]

# 4. Inconclusive twice (CANARY_TRIES=2): retried, then failed, never promoted.
commit "merge: a change the canary cannot try"
run inconclusive 202611100004
check inconclusive "exits 3" [ "$rc" -eq 3 ]
check inconclusive "stays in testing" [ "$(channels 202611100004)" = testing ]
check inconclusive "with no verdict" [ "$(state 202611100004)" = none ]
run inconclusive 202611100005
check inconclusive "the retry does not rebuild" not_called "build.sh"
check inconclusive "the retry runs the canary again" called "canary .*--target 202611100004"
check inconclusive "the second try marks it failed" [ "$(state 202611100004)" = failed ]
check inconclusive "and exits 1" [ "$rc" -eq 1 ]
check inconclusive "never promoted" not_called "promote"

# 5. Retention: the newest 3 stable releases are kept.
for v in 202611100006 202611100007 202611100008; do
	commit "merge: release $v"
	run pass "$v"
done
check retention "4 stable releases made, 3 kept" [ "$(jq '[.updates[] | select(.channels | index("stable"))] | length' "$idx")" -eq 3 ]
check retention "the oldest stable is pruned from the served tree" [ ! -d "$t/serve/202611010000" ]
check retention "older failed releases are pruned" [ ! -d "$t/serve/202611100001" ]

# 6. A busy builder: the run is skipped and touches nothing.
commit "merge: while the builder is busy"
flock "$ARCA_BUILDER_LOCK" sleep 5 &
sleep 1
run pass 202611100009
check busy-builder "exits 0" [ "$rc" -eq 0 ]
check busy-builder "says skipped" grep -q skipped <<<"$out"
check busy-builder "calls nothing" [ ! -s "$t/calls" ]
wait

# 7. A dry run prints the plan and calls nothing.
run pass 202611100009
: >"$t/calls"
out=$("$ap" --dry-run --serve-dir "$t/serve" --checkout "$t/checkout" --remote origin --branch main </dev/null 2>&1)
check dry-run "plans nothing once the branch is published" grep -q '^plan: nothing' <<<"$out"
commit "merge: after the dry run"
out=$("$ap" --dry-run --serve-dir "$t/serve" --checkout "$t/checkout" --remote origin --branch main </dev/null 2>&1)
check dry-run "plans a publish of the new commit" grep -q '^plan: publish' <<<"$out"
check dry-run "calls nothing" [ ! -s "$t/calls" ]

# 8. The canary dies without a verdict after switching to testing: failed, never promoted later.
run die-target 202611100010
check died-on-target "exits 1" [ "$rc" -eq 1 ]
check died-on-target "recorded failed" [ "$(state 202611100010)" = failed ]
check died-on-target "with a plain reason" grep -q "stopped without a verdict" "$t/serve/202611100010/canary.json"
run pass 202611100099
check died-on-target "a later run with a passing canary does nothing" [ ! -s "$t/calls" ]
check died-on-target "never promoted" [ -z "$(channels 202611100010)" ]

# 9. The canary dies before switching: counted as inconclusive, then failed, never promoted.
commit "merge: a canary that dies early"
run die-baseline 202611100011
check died-early "first death is inconclusive" [ "$rc" -eq 3 ]
run die-baseline 202611100099
check died-early "the second death (CANARY_TRIES=2) fails it" [ "$(state 202611100011)" = failed ]
check died-early "and exits 1" [ "$rc" -eq 1 ]
run pass 202611100099
check died-early "never promoted later" [ -z "$(channels 202611100011)" ]

# 10. A failed verdict with no reason still gets recorded, with a default reason.
commit "merge: a canary that fails silently"
run fail-noreason 202611100012
check no-reason "exits 1" [ "$rc" -eq 1 ]
check no-reason "recorded failed" [ "$(state 202611100012)" = failed ]
check no-reason "with a default reason" grep -q "without giving a reason" "$t/serve/202611100012/canary.json"

# 11. Recording the failure in the tree fails: the saved verdict is applied by the next run, never promoted.
commit "merge: a failure the tree cannot record"
touch "$t/record-broken"
run fail 202611100013
rm -f "$t/record-broken"
check record-broken "exits 1" [ "$rc" -eq 1 ]
check record-broken "the verdict was saved" [ "$(jq -r .state "$t/state/arca-os/autopublish/verdicts/202611100013.json")" = failed ]
run pass 202611100099
check record-broken "the next run applies the saved failure" [ "$(state 202611100013)" = failed ]
check record-broken "without running the canary again" not_called "canary "
check record-broken "without building" not_called "build.sh"
check record-broken "never promoted" [ -z "$(channels 202611100013)" ]

# 12. A failing build backs off, stops after BUILD_TRIES (3), and a new commit resets it.
commit "merge: a change that does not build"
bfile() { echo "$t/state/arca-os/autopublish/build-failures/$(git -C "$t/origin" rev-parse HEAD)"; }
touch "$t/build-broken"
run pass 202611100014
check build-fails "exits 1" [ "$rc" -eq 1 ]
check build-fails "counted once" [ "$(cut -d' ' -f1 "$(bfile)")" = 1 ]
run pass 202611100014
check build-fails "the next run backs off" grep -q "backing off" <<<"$out"
check build-fails "and builds nothing" not_called "build.sh"
echo "1 0" >"$(bfile)"
run pass 202611100014
echo "2 0" >"$(bfile)"
run pass 202611100014
check build-fails "counted three times" [ "$(cut -d' ' -f1 "$(bfile)")" = 3 ]
echo "3 0" >"$(bfile)"
run pass 202611100014
check build-fails "then waits for a new commit" grep -q "waiting for a new commit" <<<"$out"
check build-fails "without building" not_called "build.sh"
rm -f "$t/build-broken"
commit "merge: fix the build"
run pass 202611100015
check build-fails "a new commit builds and is promoted" [ "$(channels 202611100015)" = stable,testing ]
check build-fails "the failure count is cleared" [ -z "$(ls "$t/state/arca-os/autopublish/build-failures")" ]

# 13. Publishing fails after the build: the build is removed and marked failed in the tree.
commit "merge: a release that does not publish"
touch "$t/publish-broken"
run pass 202611100016
rm -f "$t/publish-broken"
check publish-fails "exits 1" [ "$rc" -eq 1 ]
check publish-fails "the build directory is deleted" [ ! -d "$t/builder/srv/artifacts/os/202611100016" ]
check publish-fails "it is marked failed in the tree" called "fail --version 202611100016 --base none --reason publishing did not finish"
check publish-fails "it is not left in testing" [ -z "$(channels 202611100016)" ]
check publish-fails "never promoted" not_called "promote"

# 14. Too little space refuses the build.
commit "merge: while the disk is full"
echo 10 >"$t/builder-free-gib"
run pass 202611100017
echo 100 >"$t/builder-free-gib"
check low-space "builder: exits 1" [ "$rc" -eq 1 ]
check low-space "builder: says why" grep -q "refusing to build: builder 10 GiB free" <<<"$out"
check low-space "builder: builds nothing" not_called "build.sh"
echo 20 >"$t/pool-free-gib"
run pass 202611100017
echo 500 >"$t/pool-free-gib"
check low-space "pool: refuses" grep -q "pool crucial 20 GiB free" <<<"$out"
check low-space "pool: builds nothing" not_called "build.sh"

# 15. The build made the NVIDIA drivers: they are published, the notes drop the line, the canary skips them.
commit "merge: with NVIDIA drivers"
touch "$t/build-nvidia"
run pass 202611100018
check nvidia "published with both drivers" called "publish-update 202611100018 testing incus,arca,nvidia,nvidia-lts$"
check nvidia "notes.json has no not_included" jqe 'has("not_included") | not' "$t/serve/202611100018/notes.json"
check nvidia "the canary installs only incus,arca" called "canary .*--target 202611100018 .*--apps incus,arca "
check nvidia "promoted" [ "$(channels 202611100018)" = stable,testing ]

# 16. APPS in the config names the applications exactly, NVIDIA built or not.
commit "merge: with APPS set"
cp "$t/config/arca-os/autopublish.conf" "$t/conf.saved"
printf 'APPS=incus,arca\n' >>"$t/config/arca-os/autopublish.conf"
run pass 202611100019
cp "$t/conf.saved" "$t/config/arca-os/autopublish.conf"
rm -f "$t/build-nvidia"
check apps-config "publishes only APPS" called "publish-update 202611100019 testing incus,arca$"
check apps-config "notes.json says NVIDIA is not included" jqe '.not_included[0] | test("NVIDIA")' "$t/serve/202611100019/notes.json"

[ "$fails" -eq 0 ] || exit 1
