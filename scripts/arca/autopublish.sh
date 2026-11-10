#!/usr/bin/env bash
# One automatic publishing run on the build host: merge -> build -> sign ->
# publish to "testing" -> canary -> promote to "stable", with no person in the
# loop, and nothing reaches a stable node before it worked on a canary.
#
#   autopublish.sh [--dry-run] [--config <file>] [--branch main] [--remote origin]
#                  [--checkout <dir>] [--serve-dir <dir>] [--builder-tree <dir>]
#                  [--project arcaos-dev] [--builder arcaos-builder]
#                  [--canary-vm arcaos-canary] [--nodes <remote,...>]
#
# A run does the first of these that applies:
#   record   the newest autopublished release has a verdict saved in the state
#            directory that the tree does not show yet (an earlier run stopped
#            after the canary): apply it, without testing again
#   resume   the newest autopublished release is still in testing with no
#            canary verdict (an earlier canary could not run): run its canary
#   publish  the branch moved since the newest autopublished release: build
#            it in the builder (build.sh --no-latest), publish it into channel
#            testing (publish-update.sh) with notes.json, and run the canary
#   nothing  otherwise, or the branch head failed to build or publish: after
#            a failure the same commit waits 30 min, then 60 min, and after
#            BUILD_TRIES (3) failures it waits for a new commit
# The canary (canary.sh) installs the current stable release in a capped VM,
# points it at testing and waits for it to update itself and pass. Passed: the
# same signed release is promoted to stable (release-tree.sh promote; nothing
# is rebuilt). Failed: canary.json records "failed" and the plain reason, and
# the release leaves testing; it is never promoted. Inconclusive (the canary
# could not run): it stays in testing and the next run retries; every canary
# attempt counts, and after CANARY_TRIES (3) it is marked failed. Each verdict
# is saved in the state directory before the tree is changed, and a version
# that ever failed is never promoted. A canary that stops without a verdict
# counts as inconclusive before it switched to testing, and as failed after.
# A build or publish that fails is removed again: its build directory is
# deleted and, when it reached the tree, it is marked failed there. A build is
# refused when the builder has less than MIN_BUILDER_FREE_GIB (30) GiB free or
# its storage pool less than MIN_POOL_FREE_GIB (50) GiB. After a verdict, the tree keeps the newest 3
# stable releases, the newest testing one, the newest autopublished one and
# every version a node in --nodes reports (running, next or alternate); the
# rest is pruned. The builder is stopped at the end of every run.
#
# Only one run goes at a time (a flock on ~/.cache/arca-autopublish.lock). A
# run never waits for the builder: when another build or proof VM holds the
# builder lock (~/.cache/arcaos-builder.lock, $ARCA_BUILDER_LOCK overrides it,
# and in-builder.sh's own default lock) or a cargo build holds
# ~/.cache/arca-cargo.lock, it logs "skipped" and exits 0.
#
# --dry-run reads the branch, the served tree and the nodes, prints the plan
# (with the notes it would publish and what retention would prune) and
# changes nothing: no lock, fetch, builder, VM or tree is touched.
#
# The config file (default ~/.config/arca-os/autopublish.conf) holds KEY=value
# lines, read as data: BRANCH, REMOTE, CHECKOUT, TOOLS_CHECKOUT, SERVE_DIR, BUILDER_TREE,
# PROJECT, BUILDER, CANARY_VM, NODES, APPS, KEEP_STABLE, BUILD_ARGS,
# BUILD_TRIES, MIN_BUILDER_FREE_GIB, MIN_POOL_FREE_GIB, CANARY_TIMEOUT,
# CANARY_TRIES, CANARY_BAKE_JQ, STATE_DIR. Flags win. Without APPS a release
# carries incus,arca and each NVIDIA driver (nvidia, nvidia-lts) the build made
# for its own kernel (nvidia-packs.sh), plus gpu-support with them; notes.json
# says why a driver is left out. The canary installs only incus,arca, as it has
# no GPU. A node in NODES whose
# update channel is empty takes the newest release in any channel, testing
# included; the run warns about it.
#
# Exit codes: 0 promoted, nothing to do, or skipped; 1 the canary failed or an
# error; 2 refused (usage, config, uncapped builder); 3 inconclusive.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
tools=$(cd "$here/../.." && pwd)
cache=${XDG_CACHE_HOME:-$HOME/.cache}

# Helpers; the test replaces them with stubs.
IN_BUILDER=${ARCA_AUTOPUBLISH_IN_BUILDER:-$here/in-builder.sh}
PUBLISH_UPDATE=${ARCA_AUTOPUBLISH_PUBLISH_UPDATE:-$here/publish-update.sh}
CANARY=${ARCA_AUTOPUBLISH_CANARY:-$here/canary.sh}

branch=main remote=origin
checkout=$cache/arca-os/autopublish/arca-autopublish
tools_wt=$cache/arca-os/autopublish/arca-tools
serve_dir=${ARCA_UPDATES_DIR:-$cache/arca-os/updates}
builder_tree=/srv/artifacts/updates
project=arcaos-dev builder=arcaos-builder canary_vm=arcaos-canary
nodes='' apps=incus,arca apps_given=0 keep_stable=3
build_args='--local-kernel /srv/artifacts/kernel/latest'
canary_timeout=2700 canary_tries=3 canary_bake_jq=''
build_tries=3 min_builder_free_gib=30 min_pool_free_gib=50
state_dir=${XDG_STATE_HOME:-$HOME/.local/state}/arca-os/autopublish
config=${XDG_CONFIG_HOME:-$HOME/.config}/arca-os/autopublish.conf
dry_run=0
run_lock=$cache/arca-autopublish.lock
builder_lock=${ARCA_BUILDER_LOCK:-$cache/arcaos-builder.lock}
inbuilder_lock=${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock
cargo_lock=$cache/arca-cargo.lock
NVIDIA_APPS='nvidia nvidia-lts'
declare -A nvidia_why=()

log() { printf 'autopublish: %s\n' "$*" >&2; }
die() { printf 'autopublish: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'autopublish: refused: %s\n' "$*" >&2; exit 2; }
usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

# The config file first, so flags win.
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
	if [ "${args[i]}" = --config ]; then
		[ $((i + 1)) -lt ${#args[@]} ] || usage
		config=${args[i + 1]}
		[ -f "$config" ] || refuse "no config file $config"
	fi
done
if [ -f "$config" ]; then
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in '' | '#'*) continue ;; esac
		key=${line%%=*} val=${line#*=}
		[ "$key" != "$line" ] || refuse "$config: not KEY=value: $line"
		val=${val#\"} val=${val%\"}
		case "$key" in
		BRANCH) branch=$val ;; REMOTE) remote=$val ;; CHECKOUT) checkout=$val ;; TOOLS_CHECKOUT) tools_wt=$val ;;
		SERVE_DIR) serve_dir=$val ;; BUILDER_TREE) builder_tree=$val ;; PROJECT) project=$val ;;
		BUILDER) builder=$val ;; CANARY_VM) canary_vm=$val ;; NODES) nodes=$val ;; APPS) apps=$val apps_given=1 ;;
		KEEP_STABLE) keep_stable=$val ;; BUILD_ARGS) build_args=$val ;; CANARY_TIMEOUT) canary_timeout=$val ;;
		CANARY_TRIES) canary_tries=$val ;; CANARY_BAKE_JQ) canary_bake_jq=$val ;; STATE_DIR) state_dir=$val ;;
		BUILD_TRIES) build_tries=$val ;; MIN_BUILDER_FREE_GIB) min_builder_free_gib=$val ;; MIN_POOL_FREE_GIB) min_pool_free_gib=$val ;;
		*) refuse "$config: unknown key $key" ;;
		esac
	done <"$config"
fi
while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) dry_run=1; shift ;;
	--config) shift 2 ;;
	--branch) [ $# -ge 2 ] || usage; branch=$2; shift 2 ;;
	--remote) [ $# -ge 2 ] || usage; remote=$2; shift 2 ;;
	--checkout) [ $# -ge 2 ] || usage; checkout=$2; shift 2 ;;
	--serve-dir) [ $# -ge 2 ] || usage; serve_dir=$2; shift 2 ;;
	--builder-tree) [ $# -ge 2 ] || usage; builder_tree=$2; shift 2 ;;
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--builder) [ $# -ge 2 ] || usage; builder=$2; shift 2 ;;
	--canary-vm) [ $# -ge 2 ] || usage; canary_vm=$2; shift 2 ;;
	--nodes) [ $# -ge 2 ] || usage; nodes=$2; shift 2 ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

case "$project" in arcaos-*) ;; *) refuse "project '$project' does not start with arcaos-" ;; esac
case "$builder_tree" in /srv/artifacts/*) ;; *) refuse "builder tree '$builder_tree' is not under /srv/artifacts/" ;; esac
printf '%s' "$branch" | grep -Eqx '[A-Za-z0-9._/-]+' || refuse "bad branch '$branch'"
printf '%s' "$keep_stable" | grep -Eqx '[1-9][0-9]*' || refuse "KEEP_STABLE must be a positive number"
for n in "$canary_tries" "$build_tries" "$min_builder_free_gib" "$min_pool_free_gib"; do
	printf '%s' "$n" | grep -Eqx '[0-9]+' || refuse "CANARY_TRIES, BUILD_TRIES and the free space limits must be numbers"
done
if [ "$canary_tries" -lt 1 ] || [ "$build_tries" -lt 1 ]; then refuse "CANARY_TRIES and BUILD_TRIES must be at least 1"; fi
[ -z "$nodes" ] || printf '%s' "$nodes" | grep -Eqx '[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*' || refuse "bad --nodes '$nodes'"
[ "$canary_vm" != "$builder" ] || refuse "the canary VM must not be the builder"
for c in git jq flock; do command -v "$c" >/dev/null || die "missing tool: $c"; done

# ---- Locks ------------------------------------------------------------------
# lock_free <file>: 0 when nobody holds a flock on it (it takes and drops one).
lock_free() { [ ! -e "$1" ] || flock -n "$1" true; }
if [ "$dry_run" -eq 0 ]; then
	mkdir -p "$cache" "$state_dir"
	exec 7>>"$run_lock"
	if ! flock -n 7; then
		log "skipped: another autopublish run holds $run_lock"
		exit 0
	fi
	exec 8>>"$builder_lock"
	if ! flock -n 8; then
		log "skipped: the builder is busy ($builder_lock is held by another build or proof VM)"
		exit 0
	fi
	if [ "$inbuilder_lock" != "$builder_lock" ]; then
		exec 6>>"$inbuilder_lock"
		if ! flock -n 6; then
			log "skipped: the builder is busy ($inbuilder_lock is held by in-builder.sh or vm-test.sh)"
			exit 0
		fi
	fi
	if ! lock_free "$cargo_lock"; then
		log "skipped: a cargo build holds $cargo_lock"
		exit 0
	fi
	export ARCA_BUILDER_LOCK=$builder_lock ARCA_BUILDER_LOCK_HELD=1
fi

# ---- Reading the state ------------------------------------------------------
idx=$serve_dir/index.json
[ -f "$idx" ] || refuse "no index.json in $serve_dir (publish a first release by hand with publish-update.sh)"
in_ch() { jq -r --arg c "$1" '.updates[] | select((.channels // []) | index($c) != null) | .version' "$idx"; }
all_versions() { jq -r '.updates[].version' "$idx"; }
notes_of() { [ -f "$serve_dir/$1/notes.json" ] && jq -r ".$2 // empty" "$serve_dir/$1/notes.json"; }
verdict_of() {
	if [ -f "$serve_dir/$1/canary.json" ]; then jq -r '.state // empty' "$serve_dir/$1/canary.json"; else echo pending; fi
}
prev_stable=$(in_ch stable | head -1)
last_auto='' last_commit=''
for v in $(all_versions); do
	if [ -f "$serve_dir/$v/notes.json" ]; then
		last_auto=$v
		last_commit=$(notes_of "$v" commit)
		break
	fi
done

# The verdict ledger: a verdict is saved here before the tree is changed, and a
# failure is never forgotten.
ledger=$state_dir/verdicts
saved_field() { if [ -f "$ledger/$1.json" ]; then jq -r ".$2 // empty" "$ledger/$1.json"; fi; }
ever_failed() { [ -f "$state_dir/failed-versions" ] && grep -qx "$1" "$state_dir/failed-versions"; }
# tree_agrees <version>: the tree already shows the saved verdict (or none is saved).
tree_agrees() {
	case "$(saved_field "$1" state)" in
	failed) [ "$(verdict_of "$1")" = failed ] && ! in_ch testing | grep -qx "$1" && ! in_ch stable | grep -qx "$1" ;;
	passed) [ "$(verdict_of "$1")" = passed ] && in_ch stable | grep -qx "$1" ;;
	*) return 0 ;;
	esac
}
# Build failures per commit: "<count> <epoch of the last failure>".
build_fail_file() { echo "$state_dir/build-failures/$1"; }
build_failures() { cut -d' ' -f1 "$(build_fail_file "$1")" 2>/dev/null || echo 0; }
build_failed_at() { cut -d' ' -f2 "$(build_fail_file "$1")" 2>/dev/null || echo 0; }

tools_sha=$(git -C "$tools" rev-parse HEAD)
# The branch head: the remote's, read without fetching on a dry run.
if [ "$dry_run" -eq 1 ]; then
	git_dir=$tools
	[ ! -d "$checkout" ] || git_dir=$checkout
	target=$(git -C "$git_dir" ls-remote "$remote" "refs/heads/$branch" | cut -f1)
	[ -n "$target" ] || die "no branch $branch on $remote"
else
	if [ ! -e "$checkout" ]; then
		log "creating the build checkout $checkout"
		mkdir -p "$(dirname "$checkout")"
		git -C "$tools" worktree add -q --detach "$checkout" HEAD
	fi
	[ -z "$(git -C "$checkout" status --porcelain)" ] || refuse "the build checkout $checkout is dirty"
	git -C "$checkout" fetch -q "$remote" "refs/heads/$branch" || die "could not fetch $branch from $remote"
	git -C "$checkout" checkout -q --detach FETCH_HEAD
	target=$(git -C "$checkout" rev-parse HEAD)
	git_dir=$checkout
	# The builder-side tools (publish-build.sh, release-tree.sh) come from this
	# script's own commit, in a clean worktree of their own.
	if [ ! -e "$tools_wt" ]; then
		log "creating the tools checkout $tools_wt"
		mkdir -p "$(dirname "$tools_wt")"
		git -C "$tools" worktree add -q --detach "$tools_wt" "$tools_sha"
	fi
	[ -z "$(git -C "$tools_wt" status --porcelain)" ] || refuse "the tools checkout $tools_wt is dirty"
	git -C "$tools_wt" checkout -q --detach "$tools_sha"
fi

# Versions the nodes report: what they run, will boot next, and can fall back to.
node_versions=''
node_report=''
for n in ${nodes//,/ }; do
	env_json=$(timeout 20 incus query "$n:/os/1.0" </dev/null 2>/dev/null || true)
	vs=$(printf '%s' "$env_json" | jq -r '.environment | [.os_version, .os_version_next, .os_version_alternate] | map(select(. != null and . != "")) | unique | join(" ")' 2>/dev/null || true)
	node_report="$node_report $n=${vs:-unknown}"
	node_versions="$node_versions $vs"
	ch=$(timeout 20 incus query "$n:/os/1.0/system/update" </dev/null 2>/dev/null | jq -r '.config.channel // empty' 2>/dev/null || true)
	if [ -n "$env_json" ] && [ -z "$ch" ]; then
		log "warning: node $n has no update channel set, so it takes the newest release in any channel, testing included"
		node_report="$node_report (no channel: takes testing too)"
	fi
done

# retention <newly stable or -> <protected...>: prints "keep <list>" and
# "prune <list>". The first argument counts as the newest stable release.
retention() {
	local keep v first=$1
	shift
	keep=$( {
		{ [ "$first" = - ] || echo "$first"; in_ch stable; } | head -"$keep_stable"
		in_ch testing | head -1
		[ -z "$last_auto" ] || echo "$last_auto"
		for v in "$@" $node_versions; do echo "$v"; done
	} | grep -Ex '[0-9]{12}|<new>' | sort -u)
	echo "keep $(printf '%s\n' "$keep" | sort -r | paste -sd' ' -)"
	echo "prune $(all_versions | grep -vxF "$keep" | paste -sd' ' - || true)"
}

# notes_json <version> <apps>: the release notes for the target commit; an
# NVIDIA driver not in <apps> is listed as not included, with nvidia_why[<app>].
notes_json() {
	local range subjects a nv=()
	for a in $NVIDIA_APPS; do
		case ",$2," in *",$a,"*) ;; *) nv+=("The NVIDIA driver $a is not included in this release${nvidia_why[$a]:+: ${nvidia_why[$a]}}.") ;; esac
	done
	if [ -n "$last_commit" ] && git -C "$git_dir" merge-base --is-ancestor "$last_commit" "$target" 2>/dev/null; then
		range=("$last_commit..$target")
	else
		range=(-n 20 "$target")
	fi
	subjects=$(git -C "$git_dir" log --first-parent --format=%s "${range[@]}" 2>/dev/null | head -50 || true)
	jq -n --arg v "$1" --arg c "$target" --arg p "$last_commit" --arg b "$branch" \
		--arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg s "$subjects" --arg nv "$(printf '%s\n' "${nv[@]}")" \
		'{version: $v, commit: $c, previous_commit: $p, branch: $b, built_at: $t,
		  changes: ($s | split("\n") | map(select(length > 0)))}
		 + ($nv | split("\n") | map(select(length > 0)) | if length > 0 then {not_included: .} else {} end)'
}

# ---- The plan ---------------------------------------------------------------
plan=nothing
why=''
now=$(date +%s)
bf=$(build_failures "$target")
backoff_until=$(($(build_failed_at "$target") + 1800 * (1 << (bf > 0 ? bf - 1 : 0))))
if [ -n "$last_auto" ] && ever_failed "$last_auto" && [ "$(verdict_of "$last_auto")" != failed ]; then
	plan=record
	why="$last_auto failed before; that verdict is applied again"
elif [ -n "$last_auto" ] && ! tree_agrees "$last_auto"; then
	plan=record
	why="$last_auto has a saved verdict ($(saved_field "$last_auto" state)) that the tree does not show yet"
elif [ -n "$last_auto" ] && [ "$(verdict_of "$last_auto")" = pending ] && in_ch testing | grep -qx "$last_auto"; then
	plan=resume
	why="$last_auto is in testing with no canary verdict"
elif [ "$target" != "$last_commit" ] && [ "$bf" -ge "$build_tries" ]; then
	why="$branch at ${target:0:12} failed to build or publish $bf times; waiting for a new commit"
elif [ "$target" != "$last_commit" ] && [ "$bf" -gt 0 ] && [ "$now" -lt "$backoff_until" ]; then
	why="$branch at ${target:0:12} failed to build or publish $bf time(s); backing off until $(date -d "@$backoff_until" '+%H:%M')"
elif [ "$target" != "$last_commit" ]; then
	plan=publish
	prev=none
	if [ -n "$last_auto" ]; then prev="$last_auto from ${last_commit:0:12}"; fi
	why="$branch moved to ${target:0:12} (last autopublished: $prev)"
else
	why="$branch at ${target:0:12} is already autopublished as $last_auto ($(verdict_of "$last_auto"))"
fi

if [ "$dry_run" -eq 1 ]; then
	lk() { if lock_free "$1"; then echo free; else echo held; fi; }
	echo "autopublish: dry run; nothing is fetched, built, published, started or changed"
	echo "branch: $remote/$branch at $target"
	echo "tools: $tools at ${tools_sha:0:12} (builder-side scripts run from a clean worktree at $tools_wt)"
	echo "tree: $serve_dir ($(all_versions | wc -l) releases)"
	echo "  stable: $(in_ch stable | paste -sd' ' -)"
	echo "  testing: $(in_ch testing | paste -sd' ' - | sed 's/^$/none/')"
	echo "  newest autopublished: ${last_auto:-none}${last_auto:+ (commit ${last_commit:0:12}, canary $(verdict_of "$last_auto"))}"
	echo "nodes:${node_report:- none configured}"
	echo "locks: run $(lk "$run_lock"), builder $(lk "$builder_lock") ($builder_lock), in-builder $(lk "$inbuilder_lock"), cargo $(lk "$cargo_lock")"
	echo "plan: $plan ($why)"
	step=0
	say() { step=$((step + 1)); echo "  $step. $*"; }
	case "$plan" in
	publish)
		say "check the builder caps (limits.cpu=4, root limits.read=150MB, limits.write=80MB)"
		say "check free space: builder >= ${min_builder_free_gib} GiB, its pool >= ${min_pool_free_gib} GiB"
		say "build: in-builder.sh --project $project --vm $builder $checkout -- scripts/arca/build.sh --no-latest $build_args"
		dry_apps=$apps
		[ "$apps_given" -eq 1 ] || dry_apps+=,nvidia,nvidia-lts,gpu-support
		say "publish: publish-update.sh --version <new> --channel testing --apps $dry_apps --serve-dir $serve_dir --builder-tree $builder_tree"
		[ "$apps_given" -eq 1 ] || say "  (each NVIDIA driver only if the build made it for its own kernel, and gpu-support only with one; nvidia-packs.sh decides)"
		say "notes: release-tree.sh notes --version <new> (signed notes.json), with:"
		notes_json '<new>' "$dry_apps" | sed 's/^/       /'
		;;
	resume) say "no build: $last_auto is already published in testing" ;;
	record) say "no build, no canary: apply the saved verdict to $last_auto: $(saved_field "$last_auto" state)${last_auto:+ }$(ever_failed "$last_auto" && echo '(it failed before, so failed)')" ;;
	esac
	if [ "$plan" = publish ] || [ "$plan" = resume ]; then
		v=${last_auto:-<new>}
		[ "$plan" = resume ] || v='<new>'
		say "canary: canary.sh --project $project --vm $canary_vm --from-builder $builder --stop-builder --base ${prev_stable:-<none: inconclusive>} --target $v --channel testing --server-url http://<${project} bridge address>:<free port> --timeout $canary_timeout${canary_bake_jq:+ --bake-jq \"$canary_bake_jq\"}"
		say "passed: release-tree.sh promote --version $v --base ${prev_stable:-none} --to stable (the same signed files)"
		say "failed: release-tree.sh fail --version $v --reason '<reason>' (out of testing, never promoted)"
		say "inconclusive: stays in testing; retried by the next run, failed after $canary_tries tries (every attempt counts)"
		say "retention after a pass: $(retention "$v" "$prev_stable" | paste -sd';' - | sed 's/;/; /')"
		say "retention after a failure: $(retention - "$prev_stable" "$v" | paste -sd';' - | sed 's/;/; /')"
		say "mirror $builder:$builder_tree to $serve_dir, then stop $builder"
	fi
	exit 0
fi

# ---- Running it ------------------------------------------------------------
run_id=$(date -u +%Y%m%dT%H%M%SZ)
run_log=$state_dir/run-$run_id.log
http_pid=''
used_builder=0
mkdir -p "$ledger" "$state_dir/build-failures"
summary() { # <result> <version> <reason>
	jq -n --arg r "$1" --arg v "$2" --arg why "$3" --arg plan "$plan" --arg c "$target" --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg l "$run_log" \
		'{finished_at: $t, plan: $plan, commit: $c, version: $v, result: $r, reason: $why, log: $l}' >"$state_dir/last-run.json"
	printf '%s\n' "$(jq -c . "$state_dir/last-run.json")" >>"$state_dir/history.jsonl"
}
instance_state() { incus list --project "$project" -f csv -c ns </dev/null | awk -F, -v n="$1" '$1 == n { print $2 }'; }
# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
cleanup() {
	if [ -n "$http_pid" ]; then kill "$http_pid" 2>/dev/null || true; fi
	if [ "$used_builder" -eq 1 ] && [ -n "$(instance_state "$canary_vm")" ]; then
		incus delete --project "$project" --force "$canary_vm" </dev/null >/dev/null 2>&1 || true
	fi
	if [ "$used_builder" -eq 1 ] && [ "$(instance_state "$builder")" = RUNNING ]; then
		log "stopping $builder"
		incus stop --project "$project" "$builder" --timeout 120 </dev/null || incus stop --project "$project" "$builder" --force </dev/null || true
	fi
}
trap cleanup EXIT
log "run $run_id: $plan ($why); log $run_log"
if [ "$plan" = nothing ]; then
	summary nothing "${last_auto:-}" "$why"
	exit 0
fi

# The builder must still carry its caps.
caps="$(incus config get --project "$project" "$builder" limits.cpu </dev/null) $(incus config device get --project "$project" "$builder" root limits.read </dev/null) $(incus config device get --project "$project" "$builder" root limits.write </dev/null)"
[ "$caps" = "4 150MB 80MB" ] || refuse "$builder is not capped as required (cpu, read, write: $caps)"

in_builder() { used_builder=1; "$IN_BUILDER" --project "$project" --vm "$builder" "$@" >>"$run_log" 2>&1; }
bexec() { incus exec --project "$project" "$builder" -- "$@" </dev/null; }
# builder_out <cmd...>: runs a tools-checkout command in the builder and prints its output.
builder_out() { used_builder=1; "$IN_BUILDER" --project "$project" --vm "$builder" "$tools_wt" -- "$@" 2>>"$run_log"; }
tree_op() { in_builder "$tools_wt" -- scripts/arca/release-tree.sh --tree "$builder_tree" "$@"; }
builder_up() { [ "$(instance_state "$builder")" = RUNNING ] || in_builder "$tools_wt" -- true; }
# mirror: the builder's tree onto the served one, pruned versions included.
mirror() {
	local kept v
	builder_up
	bexec tar -C "$builder_tree" -cf - . | tar -C "$serve_dir" -xf -
	kept=$(bexec ls -1 "$builder_tree")
	for v in "$serve_dir"/*/; do
		v=$(basename "$v")
		printf '%s' "$v" | grep -Eqx '[0-9]{12}' || continue
		printf '%s\n' "$kept" | grep -qx "$v" || { rm -rf "${serve_dir:?}/$v"; log "pruned $v from $serve_dir"; }
	done
	[ "$(bexec sha256sum "$builder_tree/index.sjson" | cut -d' ' -f1)" = \
		"$(sha256sum "$serve_dir/index.sjson" | cut -d' ' -f1)" ] || die "the served index.sjson differs from the builder's"
}
# save_verdict <version> <passed|failed> <reason> <base>: before the tree changes.
save_verdict() {
	jq -n --arg v "$1" --arg s "$2" --arg r "$3" --arg b "$4" --arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		'{version: $v, state: $s, reason: $r, base: $b, saved_at: $t}' >"$ledger/$1.json.new"
	mv "$ledger/$1.json.new" "$ledger/$1.json"
	if [ "$2" = failed ] && ! ever_failed "$1"; then echo "$1" >>"$state_dir/failed-versions"; fi
}
# apply_verdict <version>: the saved verdict, into the tree. A version that
# ever failed is marked failed, whatever was saved later.
apply_verdict() {
	local v=$1 st reason base
	st=$(saved_field "$v" state) reason=$(saved_field "$v" reason) base=$(saved_field "$v" base)
	if ever_failed "$v"; then
		if [ "$st" != failed ]; then st=failed reason="it failed an earlier canary"; fi
	fi
	case "$st" in
	passed) tree_op promote --version "$v" --base "${base:-none}" --to stable --from testing ;;
	failed) tree_op fail --version "$v" --base "${base:-none}" --reason "${reason:-the canary failed}" --from testing ;;
	*) die "no saved verdict for $v" ;;
	esac
}
# record_build_failure <reason>: counts a failed build or publish of the target.
record_build_failure() {
	local n
	n=$(($(build_failures "$target") + 1))
	echo "$n $(date +%s)" >"$(build_fail_file "$target")"
	summary error "${version:-}" "$1 (failure $n of $build_tries for ${target:0:12})"
	log "$1; failure $n of $build_tries for ${target:0:12}; see $run_log"
}
# abandon <reason>: a build that will not be tested is removed again.
abandon() {
	set +e
	if [ -n "$version" ]; then
		if bexec test -f "$builder_tree/$version/update.json" >/dev/null 2>&1; then
			tree_op fail --version "$version" --base none --reason "publishing did not finish: $1" --from testing
			mirror
		fi
		bexec rm -rf "/srv/artifacts/os/$version" >/dev/null 2>&1
		log "removed the build $version"
	fi
	set -e
	record_build_failure "$1"
	exit 1
}
# free_gib <pool>: the pool's free space in GiB.
pool_free_gib() {
	local info total used
	info=$(incus storage info "$1" --bytes </dev/null)
	total=$(printf '%s\n' "$info" | sed -n 's/^ *total space: "\{0,1\}\([0-9]*\)"\{0,1\}$/\1/p')
	used=$(printf '%s\n' "$info" | sed -n 's/^ *space used: "\{0,1\}\([0-9]*\)"\{0,1\}$/\1/p')
	[ -n "$total" ] && [ -n "$used" ] || { echo 0; return; }
	echo $(((total - used) / 1073741824))
}

version=''
if [ "$plan" = record ]; then
	version=$last_auto
	if ever_failed "$version" && [ "$(saved_field "$version" state)" != failed ]; then
		save_verdict "$version" failed "it failed an earlier canary" "$(saved_field "$version" base)"
	fi
	log "applying the saved verdict for $version: $(saved_field "$version" state)"
	apply_verdict "$version" || { summary error "$version" "applying the saved verdict failed"; die "applying the saved verdict for $version failed; see $run_log"; }
	verdict=$(saved_field "$version" state) reason=$(saved_field "$version" reason)
	prev_stable=$(saved_field "$version" base)
elif [ "$plan" = publish ]; then
	builder_up
	pool=$(incus config device get --project "$project" "$builder" root pool </dev/null 2>/dev/null || true)
	[ -n "$pool" ] || pool=$(incus profile device get --project "$project" default root pool </dev/null)
	bfree=$(($(bexec df -B1 --output=avail / | tail -1) / 1073741824))
	pfree=$(pool_free_gib "$pool")
	if [ "$bfree" -lt "$min_builder_free_gib" ] || [ "$pfree" -lt "$min_pool_free_gib" ]; then
		summary error '' "not enough space to build: builder ${bfree} GiB free (need $min_builder_free_gib), pool $pool ${pfree} GiB free (need $min_pool_free_gib)"
		die "refusing to build: builder ${bfree} GiB free (need $min_builder_free_gib), pool $pool ${pfree} GiB free (need $min_pool_free_gib)"
	fi
	log "building ${target:0:12} in $builder (builder ${bfree} GiB free, pool $pool ${pfree} GiB free)"
	# shellcheck disable=SC2086  # BUILD_ARGS is a word list
	in_builder "$checkout" -- scripts/arca/build.sh --no-latest $build_args || abandon "the build failed"
	version=$(sed -n 's/^version: \([0-9]\{12\}\)$/\1/p' "$run_log" | tail -1)
	[ -n "$version" ] || abandon "the build printed no version"
	# Marked at once, so retention can remove it even if this run stops here.
	bexec sh -c "printf '{}\n' >/srv/artifacts/os/$version/autopublish.json" || abandon "the build $version is not in /srv/artifacts/os"
	# Unless APPS names the applications, each NVIDIA driver built for this
	# build's kernel is published too, with gpu-support, its Nouveau fallback.
	pub_apps=$apps
	if [ "$apps_given" -eq 0 ]; then
		if ! packs=$(builder_out scripts/arca/nvidia-packs.sh "/srv/artifacts/os/$version"); then
			packs=''
			log "warning: the NVIDIA driver packs of $version could not be checked; publishing without them"
		fi
		for a in $NVIDIA_APPS; do
			read -r what one two < <(sed -n "s/^$a //p" <<<"$packs" | head -1) || what=''
			case "$what" in
			ok) pub_apps+=,$a ;;
			missing) nvidia_why[$a]="this build did not make it" ;;
			kernel) nvidia_why[$a]="it was built for kernel $one, not this release's $two" ;;
			error) nvidia_why[$a]="its pack could not be read in the builder" ;;
			*) nvidia_why[$a]="its pack could not be checked in the builder" ;;
			esac
			case "$what" in ok | missing) ;; *) log "warning: not publishing $a: ${nvidia_why[$a]}" ;; esac
		done
		case ",$pub_apps," in
		*,nvidia,* | *,nvidia-lts,*)
			if grep -qx 'gpu-support ok' <<<"$packs"; then
				case ",$pub_apps," in *,gpu-support,*) ;; *) pub_apps+=,gpu-support ;; esac
			else
				log "warning: $version has NVIDIA drivers but no gpu-support; nodes keep the gpu-support they have"
			fi ;;
		esac
	fi
	log "built $version; publishing it into testing with $pub_apps"
	"$PUBLISH_UPDATE" --version "$version" --serve-dir "$serve_dir" --channel testing --apps "$pub_apps" \
		--project "$project" --builder "$builder" --builder-tree "$builder_tree" --checkout "$tools_wt" >>"$run_log" 2>&1 \
		|| abandon "publishing $version failed"
	notes_json "$version" "$pub_apps" >"$state_dir/notes-$version.json"
	incus file push -q -p --project "$project" "$state_dir/notes-$version.json" "$builder/srv/artifacts/autopublish/notes-$version.json" </dev/null \
		|| abandon "copying the notes for $version failed"
	tree_op notes --version "$version" --file "/srv/artifacts/autopublish/notes-$version.json" || abandon "writing the notes for $version failed"
	mirror
	rm -f "$state_dir"/build-failures/*
else
	version=$last_auto
fi

if [ "$plan" != record ]; then
	# Every canary attempt counts, counted before it starts.
	tries_file=$state_dir/canary-tries-$version
	tries=$(($(cat "$tries_file" 2>/dev/null || echo 0) + 1))
	verdict='' reason=''
	if [ "$tries" -gt "$canary_tries" ]; then
		verdict=failed reason="the canary could not reach a verdict in $canary_tries tries"
	else
		echo "$tries" >"$tries_file"
	fi
	if [ -n "$verdict" ]; then
		:
	elif [ -z "$prev_stable" ]; then
		verdict=inconclusive reason="there is no stable release to install the canary from"
	else
		# The canary, served from this host on the canary's network.
		net=$(incus profile device get --project "$project" default eth0 network </dev/null)
		addr=$(incus network get --project "$project" "$net" ipv4.address </dev/null)
		addr=${addr%/*}
		port=$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
		(cd "$state_dir" && exec python3 -I -m http.server --bind "$addr" --directory "$serve_dir" "$port") 6>&- 7>&- 8>&- >>"$run_log" 2>&1 &
		http_pid=$!
		builder_up
		log "canary (try $tries of $canary_tries): $prev_stable -> $version on $canary_vm, served at http://$addr:$port"
		out=$state_dir/canary-$version.out
		set +e
		"$CANARY" --project "$project" --vm "$canary_vm" --from-builder "$builder" --stop-builder \
			--base "$prev_stable" --target "$version" --server-url "http://$addr:$port" --channel testing \
			--apps "$apps" --timeout "$canary_timeout" ${canary_bake_jq:+--bake-jq "$canary_bake_jq"} >"$out" 2>>"$run_log"
		rc=$?
		set -e
		kill "$http_pid" 2>/dev/null || true
		http_pid=''
		cat "$out" >>"$run_log"
		said=$(sed -n 's/^result: //p' "$out" | tail -1)
		reason=$(sed -n 's/^reason: //p' "$out" | tail -1)
		# A verdict counts only when the exit code and the result line agree.
		case "$rc:$said" in
		0:passed) verdict=passed ;;
		1:failed) verdict=failed reason=${reason:-the canary failed without giving a reason} ;;
		3:inconclusive) verdict=inconclusive reason=${reason:-the canary could not run} ;;
		*)
			if grep -qx 'phase: target' "$out"; then
				verdict=failed reason="the canary stopped without a verdict (exit $rc) while running $version"
			else
				verdict=inconclusive reason="the canary stopped without a verdict (exit $rc) before trying $version"
			fi
			;;
		esac
		if [ "$verdict" = passed ] && ever_failed "$version"; then
			verdict=failed reason="it failed an earlier canary"
		fi
	fi
	log "canary verdict for $version: $verdict${reason:+ ($reason)}"
	if [ "$verdict" = inconclusive ] && [ "$tries" -ge "$canary_tries" ]; then
		verdict=failed reason="the canary could not reach a verdict in $tries tries; last: $reason"
	fi

	case "$verdict" in
	passed | failed)
		save_verdict "$version" "$verdict" "$reason" "${prev_stable:-none}"
		apply_verdict "$version" || { summary error "$version" "recording the $verdict verdict in the tree failed"; die "recording $version as $verdict failed; the next run applies it again; see $run_log"; }
		rm -f "$tries_file"
		;;
	*)
		mirror
		summary inconclusive "$version" "$reason"
		log "inconclusive (try $tries of $canary_tries): $reason"
		exit 3
		;;
	esac
fi

# Retention, protecting the canary's two versions and what the nodes report.
mirror
prune=$(retention - "$prev_stable" "$version" | sed -n 's/^prune //p')
if [ -n "$prune" ]; then
	log "pruning: $prune"
	tree_op prune --remove "${prune// /,}" || log "pruning failed; see $run_log"
	mirror
fi

if [ "$verdict" = passed ]; then
	summary promoted "$version" "the canary passed; $version is in stable"
	log "promoted $version to stable"
	exit 0
fi
summary failed "$version" "$reason"
log "$version failed its canary and is not promoted: $reason"
exit 1
