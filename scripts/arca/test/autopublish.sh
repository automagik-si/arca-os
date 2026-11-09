#!/usr/bin/env bash
# Tests autopublish.sh's decisions without a builder, VM or network: the
# builder, publish-update.sh, canary.sh and incus are stubs over a fake
# builder filesystem, and git is real. It checks that a failed canary never
# promotes (also not on later runs), a passed canary promotes the same build,
# an inconclusive canary turns into a failure after CANARY_TRIES runs, a busy
# builder skips the run, and a dry run calls nothing.
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
	*) exit 1 ;;
	esac ;;
profile) echo incusbr0 ;;
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
	mkdir -p "$B/srv/artifacts/os/$v"
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
		if [ -f "$tree/$v/canary.json" ] && [ "$(jq -r .state "$tree/$v/canary.json")" = failed ]; then exit 2; fi
		jq -n --arg v "$v" '{version: $v, state: "passed", reason: ""}' >"$tree/$v/canary.json"
		set-channels "$tree" "$v" '. + ["stable"] | unique' ;;
	fail)
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
echo "publish-update $v ${o[--channel]}" >>"$T/calls"
mkdir -p "$tree/$v"
jq -n --arg v "$v" --arg c "${o[--channel]}" '{format: "1.0", version: $v, channels: [$c], severity: "none", files: []}' >"$tree/$v/update.json"
reindex "$tree"
tar -C "$tree" -cf - . | tar -C "${o[--serve-dir]}" -xf -
EOF

# The fake canary.sh: its verdict comes from $T/canary-mode.
cat >"$t/bin/canary" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "canary $*" >>"$T/calls"
case "$(cat "$T/canary-mode")" in
pass) echo "PASS version"; echo "result: passed"; exit 0 ;;
fail) echo "FAIL arca arca.service is not active after 600 s"; echo "result: failed"; echo "reason: arca.service is not active after 600 s"; exit 1 ;;
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
check failed-canary "notes.json says NVIDIA is not included" jqe '.not_included[0] | test("NVIDIA")' "$t/serve/202611100001/notes.json"

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

[ "$fails" -eq 0 ] || exit 1
