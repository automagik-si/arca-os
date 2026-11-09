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
#   resume   the newest autopublished release is still in testing with no
#            canary verdict (an earlier canary could not run): run its canary
#   publish  the branch moved since the newest autopublished release: build
#            it in the builder (build.sh), publish it into channel testing
#            (publish-update.sh) with notes.json, and run the canary
#   nothing  otherwise
# The canary (canary.sh) installs the current stable release in a capped VM,
# points it at testing and waits for it to update itself and pass. Passed: the
# same signed release is promoted to stable (release-tree.sh promote; nothing
# is rebuilt). Failed: canary.json records "failed" and the plain reason, and
# the release leaves testing; it is never promoted. Inconclusive (the canary
# could not run): it stays in testing and the next run retries, up to 3 runs,
# then it is marked failed. After a verdict, the tree keeps the newest 3
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
# CANARY_TIMEOUT, CANARY_TRIES, CANARY_BAKE_JQ, STATE_DIR. Flags win.
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
nodes='' apps=incus,arca keep_stable=3
build_args='--local-kernel /srv/artifacts/kernel/latest'
canary_timeout=2700 canary_tries=3 canary_bake_jq=''
state_dir=${XDG_STATE_HOME:-$HOME/.local/state}/arca-os/autopublish
config=${XDG_CONFIG_HOME:-$HOME/.config}/arca-os/autopublish.conf
dry_run=0
run_lock=$cache/arca-autopublish.lock
builder_lock=${ARCA_BUILDER_LOCK:-$cache/arcaos-builder.lock}
inbuilder_lock=${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock
cargo_lock=$cache/arca-cargo.lock
NVIDIA_NOTE='NVIDIA drivers (nvidia, nvidia-lts) are not included: the update publisher does not publish them yet.'

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
		BUILDER) builder=$val ;; CANARY_VM) canary_vm=$val ;; NODES) nodes=$val ;; APPS) apps=$val ;;
		KEEP_STABLE) keep_stable=$val ;; BUILD_ARGS) build_args=$val ;; CANARY_TIMEOUT) canary_timeout=$val ;;
		CANARY_TRIES) canary_tries=$val ;; CANARY_BAKE_JQ) canary_bake_jq=$val ;; STATE_DIR) state_dir=$val ;;
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
printf '%s' "$canary_tries" | grep -Eqx '[1-9][0-9]*' || refuse "CANARY_TRIES must be a positive number"
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

# notes_json <version>: the release notes for the target commit.
notes_json() {
	local range subjects
	if [ -n "$last_commit" ] && git -C "$git_dir" merge-base --is-ancestor "$last_commit" "$target" 2>/dev/null; then
		range=("$last_commit..$target")
	else
		range=(-n 20 "$target")
	fi
	subjects=$(git -C "$git_dir" log --first-parent --format=%s "${range[@]}" 2>/dev/null | head -50 || true)
	jq -n --arg v "$1" --arg c "$target" --arg p "$last_commit" --arg b "$branch" \
		--arg t "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg s "$subjects" --arg nv "$NVIDIA_NOTE" \
		'{version: $v, commit: $c, previous_commit: $p, branch: $b, built_at: $t,
		  changes: ($s | split("\n") | map(select(length > 0))), not_included: [$nv]}'
}

# ---- The plan ---------------------------------------------------------------
plan=nothing
why=''
if [ -n "$last_auto" ] && [ "$(verdict_of "$last_auto")" = pending ] && in_ch testing | grep -qx "$last_auto"; then
	plan=resume
	why="$last_auto is in testing with no canary verdict"
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
		say "build: in-builder.sh --project $project --vm $builder $checkout -- scripts/arca/build.sh $build_args"
		say "publish: publish-update.sh --version <new> --channel testing --apps $apps --serve-dir $serve_dir --builder-tree $builder_tree"
		say "notes: release-tree.sh notes --version <new> (signed notes.json), with:"
		notes_json '<new>' | sed 's/^/       /'
		;;
	resume) say "no build: $last_auto is already published in testing" ;;
	esac
	if [ "$plan" != nothing ]; then
		v=${last_auto:-<new>}
		[ "$plan" = resume ] || v='<new>'
		say "canary: canary.sh --project $project --vm $canary_vm --from-builder $builder --stop-builder --base ${prev_stable:-<none: inconclusive>} --target $v --channel testing --server-url http://<${project} bridge address>:<free port> --timeout $canary_timeout${canary_bake_jq:+ --bake-jq \"$canary_bake_jq\"}"
		say "passed: release-tree.sh promote --version $v --base ${prev_stable:-none} --to stable (the same signed files)"
		say "failed: release-tree.sh fail --version $v --reason '<reason>' (out of testing, never promoted)"
		say "inconclusive: stays in testing; retried by the next run, failed after $canary_tries tries"
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
tree_op() { in_builder "$tools_wt" -- scripts/arca/release-tree.sh --tree "$builder_tree" "$@"; }
# mirror: the builder's tree onto the served one, pruned versions included.
mirror() {
	local kept v
	[ "$(instance_state "$builder")" = RUNNING ] || in_builder "$tools_wt" -- true
	incus exec --project "$project" "$builder" -- tar -C "$builder_tree" -cf - . </dev/null | tar -C "$serve_dir" -xf -
	kept=$(incus exec --project "$project" "$builder" -- ls -1 "$builder_tree" </dev/null)
	for v in "$serve_dir"/*/; do
		v=$(basename "$v")
		printf '%s' "$v" | grep -Eqx '[0-9]{12}' || continue
		printf '%s\n' "$kept" | grep -qx "$v" || { rm -rf "${serve_dir:?}/$v"; log "pruned $v from $serve_dir"; }
	done
	[ "$(incus exec --project "$project" "$builder" -- sha256sum "$builder_tree/index.sjson" </dev/null | cut -d' ' -f1)" = \
		"$(sha256sum "$serve_dir/index.sjson" | cut -d' ' -f1)" ] || die "the served index.sjson differs from the builder's"
}

version=''
if [ "$plan" = publish ]; then
	log "building ${target:0:12} in $builder"
	# shellcheck disable=SC2086  # BUILD_ARGS is a word list
	in_builder "$checkout" -- scripts/arca/build.sh $build_args || { summary error '' "the build failed"; die "the build failed; see $run_log"; }
	version=$(sed -n 's/^version: \([0-9]\{12\}\)$/\1/p' "$run_log" | tail -1)
	[ -n "$version" ] || { summary error '' "the build printed no version"; die "the build printed no version; see $run_log"; }
	log "built $version; publishing it into testing"
	"$PUBLISH_UPDATE" --version "$version" --serve-dir "$serve_dir" --channel testing --apps "$apps" \
		--project "$project" --builder "$builder" --builder-tree "$builder_tree" --checkout "$tools_wt" >>"$run_log" 2>&1 \
		|| { summary error "$version" "publishing failed"; die "publishing $version failed; see $run_log"; }
	notes_json "$version" >"$state_dir/notes-$version.json"
	incus file push -q -p --project "$project" "$state_dir/notes-$version.json" "$builder/srv/artifacts/autopublish/notes-$version.json" </dev/null
	tree_op notes --version "$version" --file "/srv/artifacts/autopublish/notes-$version.json" \
		|| { summary error "$version" "writing notes failed"; die "writing the notes failed; see $run_log"; }
	mirror
else
	version=$last_auto
fi

# The canary, served from this host on the canary's network.
verdict='' reason=''
if [ -z "$prev_stable" ]; then
	verdict=inconclusive reason="there is no stable release to install the canary from"
else
	net=$(incus profile device get --project "$project" default eth0 network </dev/null)
	addr=$(incus network get --project "$project" "$net" ipv4.address </dev/null)
	addr=${addr%/*}
	port=$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
	(cd "$state_dir" && exec python3 -I -m http.server --bind "$addr" --directory "$serve_dir" "$port") 6>&- 7>&- 8>&- >>"$run_log" 2>&1 &
	http_pid=$!
	[ "$(instance_state "$builder")" = RUNNING ] || in_builder "$tools_wt" -- true
	log "canary: $prev_stable -> $version on $canary_vm, served at http://$addr:$port"
	set +e
	"$CANARY" --project "$project" --vm "$canary_vm" --from-builder "$builder" --stop-builder \
		--base "$prev_stable" --target "$version" --server-url "http://$addr:$port" --channel testing \
		--apps "$apps" --timeout "$canary_timeout" ${canary_bake_jq:+--bake-jq "$canary_bake_jq"} >"$state_dir/canary-$version.out" 2>>"$run_log"
	rc=$?
	set -e
	kill "$http_pid" 2>/dev/null || true
	http_pid=''
	cat "$state_dir/canary-$version.out" >>"$run_log"
	reason=$(sed -n 's/^reason: //p' "$state_dir/canary-$version.out" | tail -1)
	case "$rc" in
	0) verdict=passed ;;
	1) verdict=failed ;;
	*) verdict=inconclusive reason=${reason:-canary.sh exited $rc} ;;
	esac
fi
log "canary verdict for $version: $verdict${reason:+ ($reason)}"

tries_file=$state_dir/canary-tries-$version
if [ "$verdict" = inconclusive ]; then
	tries=$(($(cat "$tries_file" 2>/dev/null || echo 0) + 1))
	echo "$tries" >"$tries_file"
	if [ "$tries" -ge "$canary_tries" ]; then
		verdict=failed reason="the canary could not run in $tries tries; last: $reason"
	fi
fi

case "$verdict" in
passed)
	tree_op promote --version "$version" --base "$prev_stable" --to stable --from testing \
		|| { summary error "$version" "promotion failed"; die "promoting $version failed; see $run_log"; }
	;;
failed)
	tree_op fail --version "$version" --base "${prev_stable:-none}" --reason "$reason" --from testing \
		|| { summary error "$version" "marking it failed failed"; die "marking $version failed did not work; see $run_log"; }
	;;
inconclusive)
	mirror
	summary inconclusive "$version" "$reason"
	log "inconclusive ($tries of $canary_tries tries): $reason"
	exit 3
	;;
esac
rm -f "$tries_file"

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
