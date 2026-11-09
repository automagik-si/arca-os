#!/usr/bin/env bash
# Prove a published release on a throwaway machine before any real one gets it.
#
#   canary.sh --project arcaos-* --vm <name> --from-builder <vm>
#             --base <ver> --target <ver> --server-url http://<addr>:<port>
#             [--channel testing] [--apps incus,arca] [--timeout <s>]
#             [--bake-jq <jq expression>] [--stop-builder] [--keep]
#
# The canary is installed from --base, the current stable release, exactly as
# a node running it: the build at /srv/artifacts/os/<base>/ in the builder,
# seeded with --apps, provider images at --server-url, channel stable and no
# automatic checks. It must come up on <base> with every application listed;
# otherwise the result is inconclusive, because the target was never tried.
# Then it is pointed at --channel with automatic reboots, as the owner would
# switch a node, and asked to check. It must download <target>, reboot into it
# on its own and pass:
#   version   incus-osd reports os_version <target>
#   apps      every application is listed at version <target>
#   incus     the Incus API answers on its unix socket
#   arca      arca.service is active with no restart after 60 s, and a viewer
#             token's `arca call watch_report` exits 0 and names project default
#   bake      with --bake-jq: the expression, run on GET /1.0/system/update,
#             yields "good" before the timeout ("failed" fails at once). This is
#             where the node's own health bake plugs in.
# The VM is capped like every proof VM: limits.cpu=2, and its root disk
# limits.read=150MB and limits.write=80MB. It is deleted on exit (unless
# --keep), with its image.
#
# The caller holds the builder lock (ARCA_BUILDER_LOCK_HELD=1); otherwise this
# takes it, as vm-test.sh does. The builder must be running; with
# --stop-builder it is stopped once the canary's disk is prepared.
#
# Output: one "PASS <check>" or "FAIL <check>" line per check, "phase: target"
# once the canary is switched to --channel, then "result:
# passed|failed|inconclusive" and, unless passed, "reason: <text>". Every exit
# after the usage checks carries a verdict: an unexpected error before the
# switch is inconclusive, and after it a failure, because the target was
# running.
# Exit codes: 0 passed, 1 failed, 2 refused (usage), 3 inconclusive.
set -euo pipefail

SEED_OFFSET=4196352
VM_CPUS=2
VM_MEMORY=8GiB
VM_ROOT=50GiB
DISK_READ=150MB
DISK_WRITE=80MB
MIN_MEM_GIB=12
OSD_SOCK=/run/incus-os/unix.socket
INCUS_SOCK=/var/lib/incus/unix.socket
ARCA=/usr/local/bin/arca
ARCA_CONFIG=/usr/lib/arca/node.json

project='' vm='' builder='' base='' target='' server_url='' bake_jq=''
channel=testing apps_csv=incus,arca timeout=2700 stop_builder=0 keep=0

usage() { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'canary: %s\n' "$*" >&2; }
refuse() { printf 'canary: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	--from-builder) [ $# -ge 2 ] || usage; builder=$2; shift 2 ;;
	--base) [ $# -ge 2 ] || usage; base=$2; shift 2 ;;
	--target) [ $# -ge 2 ] || usage; target=$2; shift 2 ;;
	--server-url) [ $# -ge 2 ] || usage; server_url=$2; shift 2 ;;
	--channel) [ $# -ge 2 ] || usage; channel=$2; shift 2 ;;
	--apps) [ $# -ge 2 ] || usage; apps_csv=$2; shift 2 ;;
	--timeout) [ $# -ge 2 ] || usage; timeout=$2; shift 2 ;;
	--bake-jq) [ $# -ge 2 ] || usage; bake_jq=$2; shift 2 ;;
	--stop-builder) stop_builder=1; shift ;;
	--keep) keep=1; shift ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

case "$project" in arcaos-*) ;; *) refuse "project '$project' does not start with arcaos-" ;; esac
name_re='[a-z0-9][a-z0-9-]*'
printf '%s' "$vm" | grep -Eqx "$name_re" || refuse "bad or missing --vm"
printf '%s' "$builder" | grep -Eqx "$name_re" || refuse "bad or missing --from-builder"
if [ "$vm" = "$builder" ] || [ "$vm" = arcaos-builder ]; then refuse "--vm must not be the builder"; fi
printf '%s' "$base" | grep -Eqx '[0-9]{12}' || refuse "--base <12 digits> is required"
printf '%s' "$target" | grep -Eqx '[0-9]{12}' || refuse "--target <12 digits> is required"
[ "$base" != "$target" ] || refuse "--base and --target are the same release"
printf '%s' "$server_url" | grep -Eqx 'https?://[^/[:space:]]+' || refuse "--server-url must be http(s)://<host>[:<port>]"
printf '%s' "$channel" | grep -Eqx '[a-z0-9-]+' || refuse "bad --channel"
[ "$channel" != stable ] || refuse "the canary channel must not be stable"
printf '%s' "$apps_csv" | grep -Eqx '[a-z0-9.-]+(,[a-z0-9.-]+)*' || refuse "bad --apps"
printf '%s' "$timeout" | grep -Eqx '[0-9]+' || refuse "bad --timeout"

if [ "${ARCA_BUILDER_LOCK_HELD:-}" != 1 ]; then
	exec 9>>"${ARCA_BUILDER_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock}"
	if ! flock -n 9; then
		log "waiting for the builder lock"
		flock 9
	fi
	export ARCA_BUILDER_LOCK_HELD=1
fi

P=(--project "$project")
IFS=, read -r -a apps <<<"$apps_csv"
results=()
fails=0
created=0
work=''
alias=canary-$vm
fp=''
phase=baseline verdict_done=0 err_line=''

result() { # <PASS|FAIL> <check> [detail]
	results+=("$1 $2${3:+ $3}")
	printf '%s %s%s\n' "$1" "$2" "${3:+ $3}"
	[ "$1" = PASS ] || fails=$((fails + 1))
}
# finish <passed|failed|inconclusive> [reason]: prints the verdict and exits.
finish() {
	verdict_done=1
	echo "result: $1"
	[ -z "${2:-}" ] || echo "reason: $2"
	case "$1" in passed) exit 0 ;; failed) exit 1 ;; *) exit 3 ;; esac
}
instance_state() { incus list "${P[@]}" -f csv -c ns </dev/null | awk -F, -v n="$1" '$1 == n { print $2 }'; }
vexec() { timeout 300 incus exec "${P[@]}" "$vm" -- "$@" </dev/null; }
osd() { vexec curl -sf --max-time 30 --unix-socket "$OSD_SOCK" "$@"; }
osd_env() { osd http://localhost/1.0 2>/dev/null | jq -r ".metadata.environment.$1 // empty" 2>/dev/null || true; }
app_version() { osd "http://localhost/1.0/applications/$1" 2>/dev/null | jq -r '.metadata.state.version // empty' 2>/dev/null || true; }
listed_apps() { osd http://localhost/1.0/applications 2>/dev/null | jq -r '.metadata[]? | sub(".*/"; "")' 2>/dev/null || true; }
journal_tail() { vexec journalctl -u incus-osd -b0 --no-pager 2>/dev/null | tail -"${1:-30}" || true; }

# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
cleanup() {
	local rc=$?
	if [ "$verdict_done" -eq 0 ]; then
		verdict_done=1
		echo "result: $([ "$phase" = target ] && echo failed || echo inconclusive)"
		echo "reason: the canary stopped unexpectedly (exit $rc${err_line:+, line $err_line}) while $([ "$phase" = target ] && echo "testing $target" || echo "preparing the $base baseline")"
		if [ "$phase" = target ]; then rc=1; else rc=3; fi
	fi
	if [ "$created" -eq 1 ] && [ "$keep" -eq 0 ]; then
		log "deleting $vm"
		incus delete "${P[@]}" --force "$vm" </dev/null >/dev/null 2>&1 || true
	fi
	incus image delete "${P[@]}" "$alias" </dev/null >/dev/null 2>&1 || true
	if [ -n "$fp" ]; then incus image delete "${P[@]}" "$fp" </dev/null >/dev/null 2>&1 || true; fi
	if [ -n "$work" ]; then rm -rf "$work"; fi
	if [ -n "$builder" ] && [ -n "${bout:-}" ]; then
		if [ "$(instance_state "$builder")" = RUNNING ]; then
			incus exec "${P[@]}" "$builder" -- rm -rf "$bout" </dev/null >/dev/null 2>&1 || true
		fi
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'err_line=$LINENO' ERR

[ "$(instance_state "$builder")" = RUNNING ] || finish inconclusive "the builder $builder is not running"
if [ -n "$(instance_state "$vm")" ]; then
	log "deleting a leftover $vm"
	incus delete "${P[@]}" --force "$vm" </dev/null
fi

# The base disk: the stable build, seeded as a node on channel stable.
apps_json=$(jq -cn --arg a "$apps_csv" '{applications: ($a | split(",") | map({name: .}))}')
provider_json=$(jq -cn --arg u "$server_url" '{name: "images", config: {server_url: $u}}')
update_json=$(jq -cn '{channel: "stable", check_frequency: "never", auto_reboot: false}')
bout=/srv/artifacts/canary/$vm
log "preparing the $base disk in $builder"
# shellcheck disable=SC2016  # expanded by bash in the builder
if ! incus exec "${P[@]}" "$builder" -- bash -euc '
	src=/srv/artifacts/os/$1 out=$2 offset=$6
	raw=$(find "$src" -maxdepth 1 -name "*_$1.raw" | head -1)
	[ -n "$raw" ] || { echo "no *_$1.raw in $src" >&2; exit 1; }
	[ -f "$src/metadata.tar.xz" ] || { echo "no metadata.tar.xz in $src" >&2; exit 1; }
	rm -rf "$out"
	mkdir -p "$out"
	cp "$src/metadata.tar.xz" "$out/"
	printf "%s\n" "$3" >"$out/applications.json"
	printf "%s\n" "$4" >"$out/provider.json"
	printf "%s\n" "$5" >"$out/update.json"
	tar -C "$out" -cf "$out/seed.tar" applications.json provider.json update.json
	cp --sparse=always "$raw" "$out/os.raw"
	dd if="$out/seed.tar" of="$out/os.raw" seek="$offset" bs=512 conv=notrunc status=none
	qemu-img convert -f raw -O qcow2 "$out/os.raw" "$out/os.qcow2"
	rm -f "$out/os.raw"
' canary-prep "$base" "$bout" "$apps_json" "$provider_json" "$update_json" "$SEED_OFFSET" </dev/null; then
	finish inconclusive "could not prepare a disk from the stable build $base in $builder"
fi
STAGE_BASE=${ARCA_VM_TEST_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/arca-os/vm-test}
mkdir -p "$STAGE_BASE"
work=$(mktemp -d "$STAGE_BASE/canary.XXXXXX")
for f in os.qcow2 metadata.tar.xz; do
	incus file pull -q "${P[@]}" "$builder$bout/$f" "$work/" </dev/null || finish inconclusive "could not pull $f from $builder"
done
incus exec "${P[@]}" "$builder" -- rm -rf "$bout" </dev/null
if [ "$stop_builder" -eq 1 ]; then
	log "stopping builder $builder"
	incus stop "${P[@]}" "$builder" --timeout 120 </dev/null || incus stop "${P[@]}" "$builder" --force </dev/null
fi

avail=$(awk '/^MemAvailable:/ { printf "%d", $2 / 1048576 }' /proc/meminfo)
[ "$avail" -ge "$MIN_MEM_GIB" ] || finish inconclusive "MemAvailable is $avail GiB, below $MIN_MEM_GIB GiB"

incus image delete "${P[@]}" "$alias" </dev/null >/dev/null 2>&1 || true
# A split image's fingerprint is the sha256 of its metadata and disk; with it, an
# import whose client timed out while the server finished is found and used, and
# never left behind.
fp=$(cat "$work/metadata.tar.xz" "$work/os.qcow2" | sha256sum | cut -d' ' -f1)
image_there() { incus image info "${P[@]}" "$fp" </dev/null >/dev/null 2>&1; }
if ! incus image import -q "${P[@]}" "$work/metadata.tar.xz" "$work/os.qcow2" --alias "$alias" </dev/null; then
	log "the import of the $base disk did not answer; waiting for the server to finish it"
	t0=$(date +%s)
	until image_there || [ $(($(date +%s) - t0)) -ge 600 ]; do sleep 10; done
	image_there || finish inconclusive "could not import the $base disk"
	incus image alias create "${P[@]}" "$alias" "$fp" </dev/null >/dev/null 2>&1 || true
fi
rm -f "$work/os.qcow2"
created=1
incus create -q --vm "${P[@]}" "$fp" "$vm" -c security.secureboot=false -c limits.cpu="$VM_CPUS" \
	-c limits.memory="$VM_MEMORY" -d "root,size=$VM_ROOT" -d "root,limits.read=$DISK_READ" -d "root,limits.write=$DISK_WRITE" </dev/null \
	|| finish inconclusive "could not create $vm"
incus image delete "${P[@]}" "$fp" </dev/null
incus config device add -q "${P[@]}" "$vm" vtpm tpm </dev/null
incus config set "${P[@]}" "$vm" systemd.credential.fully-enable-incus-agent=true </dev/null
log "starting $vm on $base (cpu $VM_CPUS, root read $DISK_READ / write $DISK_WRITE)"
incus start "${P[@]}" "$vm" </dev/null || finish inconclusive "could not start $vm"

# wait_for <seconds> <command...>: 0 once the command succeeds.
wait_for() {
	local limit=$1 t0
	shift
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt "$limit" ]; do
		if "$@"; then return 0; fi
		sleep 10
	done
	return 1
}
# shellcheck disable=SC2329  # called through wait_for
agent_up() { vexec true >/dev/null 2>&1; }
# shellcheck disable=SC2329  # called through wait_for
apps_listed() {
	local l a
	l=$(listed_apps)
	for a in "${apps[@]}"; do printf '%s\n' "$l" | grep -qx "$a" || return 1; done
}

# The baseline: the canary is a working stable node before it is pointed anywhere.
wait_for 300 agent_up || finish inconclusive "the $base canary did not boot (no agent after 300 s)"
wait_for 900 apps_listed || {
	journal_tail 30 >&2
	finish inconclusive "the $base canary did not install ${apps_csv} from $server_url within 900 s"
}
running=$(osd_env os_version)
[ "$running" = "$base" ] || finish inconclusive "the canary runs '${running:-unknown}', not the stable $base"
log "baseline: $base with $(listed_apps | paste -sd, -)"

# Point it at the canary channel, as an owner would, and let it update itself.
cfg=$(osd http://localhost/1.0/system/update | jq -c --arg c "$channel" \
	'.metadata.config | .channel = $c | .auto_reboot = true | .check_frequency = "5m"') \
	|| finish inconclusive "could not read the canary's update configuration"
# shellcheck disable=SC2016  # expanded by sh in the VM
vexec sh -c 'printf "%s" "$1" | curl -sf --max-time 30 --unix-socket "$2" -X PUT -H "Content-Type: application/json" --data-binary @- http://localhost/1.0/system/update >/dev/null' \
	canary "{\"config\":$cfg}" "$OSD_SOCK" || finish inconclusive "could not set the canary's channel to $channel"
osd -X POST http://localhost/1.0/system/update/:check >/dev/null || log "the first update check request failed; the 5m schedule retries"
phase=target
echo "phase: target"
log "pointed at channel $channel with automatic reboots; waiting up to ${timeout} s for $target"

# shellcheck disable=SC2329  # called through wait_for
on_target() { [ "$(osd_env os_version)" = "$target" ]; }
t0=$(date +%s)
if ! wait_for "$timeout" on_target; then
	got=$(osd_env os_version)
	status=$(osd http://localhost/1.0/system/update 2>/dev/null | jq -r '.metadata.state.status // empty' 2>/dev/null || true)
	journal_tail 40 >&2
	result FAIL version "still '${got:-unreachable}' after ${timeout} s (update status: '${status:-unknown}')"
	finish failed "the canary did not boot $target within ${timeout} s; it runs '${got:-nothing reachable}', update status '${status:-unknown}'"
fi
result PASS version "$target after $(($(date +%s) - t0)) s"

# The basic health checks on the new release, once incus-osd has swapped every
# application to it (that follows the OS boot by a few seconds to minutes).
# shellcheck disable=SC2329  # called through wait_for
apps_at_target() {
	local a
	[ "$(osd_env system_is_ready)" = true ] || return 1
	for a in "${apps[@]}"; do [ "$(app_version "$a")" = "$target" ] || return 1; done
}
wait_for 600 apps_at_target || true
for a in "${apps[@]}"; do
	v=$(app_version "$a")
	if [ "$v" = "$target" ]; then
		result PASS apps "$a $v"
	else
		result FAIL apps "$a is at '${v:-missing}', want $target"
	fi
done
# shellcheck disable=SC2329  # called through wait_for
incus_ok() { vexec curl -sf --max-time 20 --unix-socket "$INCUS_SOCK" http://localhost/1.0 2>/dev/null | jq -e '.metadata.api_status' >/dev/null 2>&1; }
if wait_for 300 incus_ok; then
	result PASS incus "the Incus API answers"
else
	result FAIL incus "no answer from $INCUS_SOCK after 300 s"
fi
# shellcheck disable=SC2329  # called through wait_for
arca_active() { [ "$(vexec systemctl is-active arca.service 2>/dev/null || true)" = active ]; }
if wait_for 600 arca_active; then
	sleep 60
	st=$(vexec systemctl is-active arca.service 2>/dev/null || true)
	nr=$(vexec systemctl show -p NRestarts --value arca.service 2>/dev/null || true)
	names='' call_rc=''
	vexec rm -f /root/canary.secret || true
	if vexec "$ARCA" token mint --scope viewer --id canary --config "$ARCA_CONFIG" --out /root/canary.secret >/dev/null 2>&1 \
		&& vexec systemctl restart arca.service; then
		wait_for 60 vexec test -S /run/arca/arca.sock || true
		if out=$(vexec "$ARCA" call watch_report --config "$ARCA_CONFIG" --token-file /root/canary.secret --args '{}' 2>/dev/null); then
			call_rc=0
		else
			call_rc=$?
		fi
		names=$(printf '%s' "$out" | jq -r '.projects[]?.project' 2>/dev/null || true)
	fi
	vexec rm -f /root/canary.secret || true
	if [ "$st" != active ] || [ "$nr" != 0 ]; then
		result FAIL arca "arca.service is '$st' with NRestarts=${nr:-unknown} after 60 s"
	elif [ "$call_rc" != 0 ]; then
		result FAIL arca "arca call watch_report exited '${call_rc:-not run}'"
	elif ! printf '%s\n' "$names" | grep -qxF default; then
		result FAIL arca "watch_report did not name the project default"
	else
		result PASS arca "active, NRestarts=0, watch_report answers"
	fi
else
	result FAIL arca "arca.service is not active after 600 s"
fi

if [ -n "$bake_jq" ]; then
	bake_state() { osd http://localhost/1.0/system/update 2>/dev/null | jq -r "$bake_jq" 2>/dev/null || true; }
	# shellcheck disable=SC2329  # called through wait_for
	bake_done() { case "$(bake_state)" in good | failed) return 0 ;; *) return 1 ;; esac; }
	wait_for "$timeout" bake_done || true
	b=$(bake_state)
	if [ "$b" = good ]; then
		result PASS bake "the node marked $target good"
	else
		result FAIL bake "the node's health bake says '${b:-nothing}'"
	fi
fi

if [ "$fails" -gt 0 ]; then
	journal_tail 30 >&2
	finish failed "$(printf '%s\n' "${results[@]}" | grep '^FAIL' | sed 's/^FAIL //' | paste -sd ';' - | sed 's/;/; /g')"
fi
finish passed
