#!/usr/bin/env bash
# Manage the Arca OS builder VM in its own Incus project.
#
#   dev-vm.sh create --pool <pool> --network <bridge> [--project arcaos-*] [--vm <name>]
#   dev-vm.sh status  [--project arcaos-*] [--vm <name>]
#   dev-vm.sh stop    [--project arcaos-*] [--vm <name>]
#   dev-vm.sh destroy [--project arcaos-*]
#
# It only ever writes inside the named project, which must start with
# "arcaos-" (anything else exits 2 before any action). The pool and the bridge
# are arguments, because they differ per host.
#
# Exit codes: 0 ok, 1 failure, 2 refused (bad project or usage).
set -euo pipefail

PROJECT=arcaos-dev
VM=arcaos-builder
POOL=""
NETWORK=""
CPUS=8
MEMORY=16GiB
ROOT_SIZE=200GiB
IMAGE=images:debian/13
# Free host memory needed before the builder starts: its 16 GiB plus 4 GiB of
# headroom, the same margin vm-test.sh keeps for an 8 GiB test VM.
MIN_MEM_GIB=20

HERE=$(cd "$(dirname "$0")" && pwd)

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'dev-vm: %s\n' "$*" >&2; }
die() { printf 'dev-vm: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'dev-vm: refused: %s\n' "$*" >&2; exit 2; }

[ $# -ge 1 ] || usage
cmd=$1
shift
while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; PROJECT=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; VM=$2; shift 2 ;;
	--pool) [ $# -ge 2 ] || usage; POOL=$2; shift 2 ;;
	--network) [ $# -ge 2 ] || usage; NETWORK=$2; shift 2 ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

case "$PROJECT" in
arcaos-*) ;;
*) refuse "project '$PROJECT' does not start with arcaos-" ;;
esac
printf '%s' "$PROJECT" | grep -Eqx 'arcaos-[a-z0-9][a-z0-9-]*' || refuse "bad project name '$PROJECT'"
printf '%s' "$VM" | grep -Eqx '[a-z0-9][a-z0-9-]*' || refuse "bad instance name '$VM'"
case "$cmd" in
create | status | stop | destroy) ;;
*) refuse "unknown command '$cmd'" ;;
esac

P=(--project "$PROJECT")

project_exists() { incus project list -f csv -c n | sed 's/ (current)$//' | grep -qx "$PROJECT"; }
vm_state() { incus list "${P[@]}" -f csv -c ns | awk -F, -v n="$VM" '$1 == n { print $2 }'; }
mem_avail_gib() { awk '/^MemAvailable:/ { printf "%d", $2 / 1048576 }' /proc/meminfo; }

wait_agent() {
	local i
	for i in $(seq 1 60); do
		if incus exec "${P[@]}" "$VM" -- true >/dev/null 2>&1; then
			return 0
		fi
		sleep 5
	done
	die "$VM: no incus agent after 300 s"
}

do_create() {
	if [ -z "$POOL" ] || [ -z "$NETWORK" ]; then refuse "create needs --pool and --network"; fi
	incus storage show "$POOL" >/dev/null || die "no storage pool '$POOL'"
	incus network show "$NETWORK" >/dev/null || die "no network '$NETWORK'"

	if project_exists; then
		log "project $PROJECT exists"
	else
		log "creating project $PROJECT"
		incus project create "$PROJECT" \
			-c features.images=true -c features.profiles=true \
			-c features.storage.volumes=true -c features.networks=false
	fi

	local devs
	devs=$(incus profile device list default "${P[@]}")
	if ! printf '%s\n' "$devs" | grep -qx root; then
		log "profile default: root disk on pool $POOL"
		incus profile device add default root disk path=/ pool="$POOL" "${P[@]}"
	fi
	if ! printf '%s\n' "$devs" | grep -qx eth0; then
		log "profile default: eth0 on $NETWORK"
		incus profile device add default eth0 nic name=eth0 network="$NETWORK" "${P[@]}"
	fi

	local state
	state=$(vm_state)
	if [ -z "$state" ]; then
		[ "$(mem_avail_gib)" -ge "$MIN_MEM_GIB" ] \
			|| die "MemAvailable is $(mem_avail_gib) GiB, below ${MIN_MEM_GIB} GiB; not starting $VM"
		log "launching $VM ($IMAGE, ${CPUS} vCPU, $MEMORY, root $ROOT_SIZE)"
		incus launch "$IMAGE" "$VM" --vm "${P[@]}" \
			-c limits.cpu="$CPUS" -c limits.memory="$MEMORY" -d root,size="$ROOT_SIZE"
	elif [ "$state" != RUNNING ]; then
		[ "$(mem_avail_gib)" -ge "$MIN_MEM_GIB" ] \
			|| die "MemAvailable is $(mem_avail_gib) GiB, below ${MIN_MEM_GIB} GiB; not starting $VM"
		log "starting $VM"
		incus start "${P[@]}" "$VM"
	fi
	wait_agent

	log "waiting for the network in $VM"
	local i
	for i in $(seq 1 60); do
		if incus exec "${P[@]}" "$VM" -- getent hosts deb.debian.org >/dev/null 2>&1; then
			break
		fi
		[ "$i" -lt 60 ] || die "$VM: no DNS after 300 s"
		sleep 5
	done

	log "provisioning $VM"
	incus file push "${P[@]}" "$HERE/provision-builder.sh" "$VM/root/provision-builder.sh"
	incus exec "${P[@]}" "$VM" -- bash /root/provision-builder.sh
}

do_status() {
	if ! project_exists; then
		echo "project: $PROJECT absent"
		return 0
	fi
	echo "project: $PROJECT"
	incus list "${P[@]}" -f csv -c nstl | sed 's/^/instance: /'
	local state
	state=$(vm_state)
	echo "builder: $VM ${state:-absent}"
	if [ "$state" = RUNNING ]; then
		incus exec "${P[@]}" "$VM" -- sh -c 'df -h --output=used,avail,target /srv | tail -1' | sed 's/^/disk: /'
	fi
	echo "host-mem-available-gib: $(mem_avail_gib)"
}

do_stop() {
	project_exists || die "no project $PROJECT"
	local state
	state=$(vm_state)
	[ -n "$state" ] || die "no instance $VM in $PROJECT"
	if [ "$state" = RUNNING ]; then
		log "stopping $VM"
		incus stop "${P[@]}" "$VM" --timeout 120 || incus stop "${P[@]}" "$VM" --force
	fi
	log "$VM stopped"
}

do_destroy() {
	if ! project_exists; then
		log "no project $PROJECT; nothing to do"
		return 0
	fi
	local name fp
	for name in $(incus list "${P[@]}" -f csv -c n); do
		log "deleting instance $name"
		incus delete "${P[@]}" --force "$name"
	done
	for fp in $(incus image list "${P[@]}" -f csv -c F); do
		log "deleting image $fp"
		incus image delete "${P[@]}" "$fp"
	done
	for name in $(incus profile list "${P[@]}" -f csv -c n); do
		[ "$name" = default ] && continue
		log "deleting profile $name"
		incus profile delete "${P[@]}" "$name"
	done
	log "deleting project $PROJECT"
	incus project delete "$PROJECT"
}

"do_$cmd"
