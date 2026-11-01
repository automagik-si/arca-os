#!/usr/bin/env bash
# Boot an OS image in a fresh test VM and check it, following upstream's E2E
# job (.github/workflows/tests.yml, "Start the VM" to "Load the extensions").
#
#   vm-test.sh --project arcaos-* --vm <name> (--from-builder <vm> | --artifacts <dir>)
#              --seed-apps <a,b,...> --provider debug|images [--server-url <url>]
#              --checks stock|none [--stop-builder] [--keep]
#
# Steps:
#   1. Refuse to start the test VM when MemAvailable is below 12 GiB (checked
#      after --stop-builder has stopped the builder).
#   2. Write applications.json and provider.json into a seed tar, dd it into a
#      copy of the image .raw at seek=4196352 bs=512, and convert that copy to
#      qcow2 (in the builder with --from-builder, locally with --artifacts).
#   3. Pull the qcow2, the seeded applications' sysexts and the build's
#      test/metadata.tar.xz (build.sh copies it beside the image).
#   4. incus image import.
#   5. Create the VM: security.secureboot=false, 4 vCPU, 8 GiB, 50 GiB root, a
#      vtpm device and systemd.credential.fully-enable-incus-agent=true.
#   6. Start it and poll `incus exec <vm> -- true` for up to 300 s.
#   7. With provider debug, push RELEASE and each seeded application's .raw
#      into /root/updates/.
#   8. POST /1.0/system/update/:check, then poll /1.0/applications until every
#      seeded application is listed, for up to 10 min.
#   9. Run the checks, printing "PASS <check>" or "FAIL <check>" for each.
# The VM is stopped at the end unless --keep is given.
#
# Check sets:
#   stock  boot, app-listed (each seeded app), secureboot, os-id (ID in
#          /usr/lib/os-release equals the build's ImageId, read from its
#          <ImageId>_<version>.efi name), mode (mode=dev)
#   none   no checks
#
# The project must start with "arcaos-" and have its own images and profiles
# (features.images=true, features.profiles=true), and --vm must not name the
# builder (arcaos-builder, or the --from-builder name).
#
# Exit codes: 0 all PASS, 1 a FAIL or an error, 2 refused (usage, project),
# 3 refused for lack of memory.
set -euo pipefail

BUILDER_NAME=arcaos-builder
MIN_MEM_GIB=12
VM_CPUS=4
VM_MEMORY=8GiB
VM_ROOT=50GiB
SEED_OFFSET=4196352
BUILDER_ARTIFACTS=/srv/artifacts/os/latest
# Large files are staged on disk, not under /tmp, which may be a tmpfs.
STAGE_BASE=${ARCA_VM_TEST_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/arca-os/vm-test}
OSD_SOCK=/run/incus-os/unix.socket

project="" vm="" builder="" artifacts="" seed_apps="" provider="" server_url="" checks=""
stop_builder=0 keep=0

usage() { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'vm-test: %s\n' "$*" >&2; }
die() { printf 'vm-test: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'vm-test: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	--from-builder) [ $# -ge 2 ] || usage; builder=$2; shift 2 ;;
	--artifacts) [ $# -ge 2 ] || usage; artifacts=$2; shift 2 ;;
	--seed-apps) [ $# -ge 2 ] || usage; seed_apps=$2; shift 2 ;;
	--provider) [ $# -ge 2 ] || usage; provider=$2; shift 2 ;;
	--server-url) [ $# -ge 2 ] || usage; server_url=$2; shift 2 ;;
	--checks) [ $# -ge 2 ] || usage; checks=$2; shift 2 ;;
	--stop-builder) stop_builder=1; shift ;;
	--keep) keep=1; shift ;;
	-h | --help) usage ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

case "$project" in
arcaos-*) ;;
*) refuse "project '$project' does not start with arcaos-" ;;
esac
name_re='[a-z0-9][a-z0-9-]*'
printf '%s' "$vm" | grep -Eqx "$name_re" || refuse "bad or missing --vm"
if [ -n "$builder" ] && [ -n "$artifacts" ] || [ -z "$builder$artifacts" ]; then
	refuse "give exactly one of --from-builder and --artifacts"
fi
if [ -n "$builder" ]; then
	printf '%s' "$builder" | grep -Eqx "$name_re" || refuse "bad --from-builder name"
fi
if [ "$vm" = "$BUILDER_NAME" ] || [ "$vm" = "$builder" ]; then
	refuse "--vm must not be the builder"
fi
[ "$stop_builder" -eq 0 ] || [ -n "$builder" ] || refuse "--stop-builder needs --from-builder"
printf '%s' "$seed_apps" | grep -Eqx '[a-z0-9.-]+(,[a-z0-9.-]+)*' || refuse "bad or missing --seed-apps"
case "$provider" in
debug) ;;
images) [ -n "$server_url" ] || refuse "--provider images needs --server-url" ;;
*) refuse "--provider must be debug or images" ;;
esac
case "$checks" in
stock | none) ;;
*) refuse "--checks must be stock or none" ;;
esac

P=(--project "$project")
# A project without its own images and profiles would import into, and
# delete from, the default project's images.
incus project show "$project" >/dev/null 2>&1 || refuse "no project $project"
for f in features.images features.profiles; do
	v=$(incus project get "$project" "$f" 2>/dev/null || true)
	[ "$v" = true ] || refuse "project $project has $f=${v:-unset}; it must have its own images and profiles"
done
IFS=, read -r -a apps <<<"$seed_apps"
apps_json=$(jq -cn --arg a "$seed_apps" '{applications: ($a | split(",") | map({name: .}))}')
if [ "$provider" = debug ]; then
	provider_json='{"name":"debug"}'
else
	provider_json=$(jq -cn --arg u "$server_url" '{name: "images", config: {server_url: $u}}')
fi

mem_avail_gib() { awk '/^MemAvailable:/ { printf "%d", $2 / 1048576 }' /proc/meminfo; }
mem_guard() {
	local avail
	avail=$(mem_avail_gib)
	if [ "$avail" -lt "$MIN_MEM_GIB" ]; then
		printf 'vm-test: refused: MemAvailable is %s GiB, below %s GiB; not starting %s\n' "$avail" "$MIN_MEM_GIB" "$vm" >&2
		exit 3
	fi
	log "MemAvailable ${avail} GiB (floor ${MIN_MEM_GIB} GiB)"
}
instance_state() { incus list "${P[@]}" -f csv -c ns | awk -F, -v n="$1" '$1 == n { print $2 }'; }
vexec() { incus exec "${P[@]}" "$vm" -- "$@"; }
osd() { vexec curl -sf --max-time 60 --unix-socket "$OSD_SOCK" "$@"; }

# Without --stop-builder nothing frees memory later, so check up front too.
[ "$stop_builder" -eq 1 ] || mem_guard

mkdir -p "$STAGE_BASE"
work=$(mktemp -d "$STAGE_BASE/$vm.XXXXXX")
started=0
# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
cleanup() {
	rm -rf "$work"
	if [ "$started" -eq 1 ] && [ "$keep" -eq 0 ]; then
		log "stopping $vm"
		incus stop "${P[@]}" "$vm" --timeout 120 >/dev/null 2>&1 || incus stop "${P[@]}" "$vm" --force >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

# Step 2: seed and convert. The same script runs in the builder or locally.
# shellcheck disable=SC2016
PREP='
set -euo pipefail
src=$(readlink -f "$1") out=$2 apps=$3 provider=$4 offset=$5
cd "$src"
shopt -s nullglob
efis=(*_*.efi)
[ ${#efis[@]} -gt 0 ] || { echo "vm-test: no *_*.efi in $src" >&2; exit 1; }
efi=$(printf "%s\n" "${efis[@]}" | sort | tail -1)
os=${efi%_*}
ver=${efi##*_}
ver=${ver%.efi}
raw="${os}_${ver}.raw"
[ -f "$raw" ] || { echo "vm-test: no $raw in $src" >&2; exit 1; }
[ -f metadata.tar.xz ] || { echo "vm-test: no metadata.tar.xz in $src" >&2; exit 1; }
rm -rf "$out"
mkdir -p "$out"
cp metadata.tar.xz "$out/"
printf "%s\n" "$apps" > "$out/applications.json"
printf "%s\n" "$provider" > "$out/provider.json"
tar -C "$out" -cf "$out/seed.tar" applications.json provider.json
cp --sparse=always "$raw" "$out/os.raw"
dd if="$out/seed.tar" of="$out/os.raw" seek="$offset" bs=512 conv=notrunc status=none
qemu-img convert -f raw -O qcow2 "$out/os.raw" "$out/os.qcow2"
rm -f "$out/os.raw"
printf "%s\n" "$ver" > "$out/RELEASE"
printf "%s\n" "$os" > "$out/OSNAME"
printf "%s\n" "$src" > "$out/SRC"
'

if [ -n "$builder" ]; then
	[ "$(instance_state "$builder")" = RUNNING ] || die "builder $builder is not running in $project"
	bout=/srv/artifacts/vm-test/$vm
	log "seeding and converting the image in $builder"
	incus exec "${P[@]}" "$builder" -- bash -c "$PREP" vm-test-prep \
		"$BUILDER_ARTIFACTS" "$bout" "$apps_json" "$provider_json" "$SEED_OFFSET"
	log "pulling the qcow2 and sysexts from $builder"
	for f in os.qcow2 metadata.tar.xz RELEASE OSNAME SRC; do
		incus file pull -q "${P[@]}" "$builder$bout/$f" "$work/"
	done
	bsrc=$(cat "$work/SRC")
	if [ "$provider" = debug ]; then
		for a in "${apps[@]}"; do
			incus file pull -q "${P[@]}" "$builder$bsrc/$a.raw" "$work/" || die "no $a.raw in $builder:$bsrc"
		done
	fi
	incus exec "${P[@]}" "$builder" -- rm -rf "$bout"
	if [ "$stop_builder" -eq 1 ]; then
		log "stopping builder $builder"
		incus stop "${P[@]}" "$builder" --timeout 120 || incus stop "${P[@]}" "$builder" --force
	fi
else
	[ -d "$artifacts" ] || die "no artifacts directory $artifacts"
	log "seeding and converting the image from $artifacts"
	bash -c "$PREP" vm-test-prep "$artifacts" "$work/prep" "$apps_json" "$provider_json" "$SEED_OFFSET"
	mv "$work"/prep/{os.qcow2,metadata.tar.xz,RELEASE,OSNAME,SRC} "$work/"
	if [ "$provider" = debug ]; then
		for a in "${apps[@]}"; do
			[ -f "$artifacts/$a.raw" ] || die "no $a.raw in $artifacts"
			cp --sparse=always "$artifacts/$a.raw" "$work/"
		done
	fi
fi
release=$(cat "$work/RELEASE")
image_id=$(cat "$work/OSNAME")
[ -n "$image_id" ] || die "no ImageId in the artifacts"
log "release $release, image $image_id, apps ${apps[*]}, provider $provider"

# Step 1: the memory guard, after the builder has stopped.
mem_guard

# Steps 4 and 5: a fresh image and a fresh VM.
alias=$vm
if incus image info "${P[@]}" "$alias" >/dev/null 2>&1; then
	incus image delete "${P[@]}" "$alias"
fi
if [ -n "$(instance_state "$vm")" ]; then
	log "deleting the previous $vm"
	incus delete "${P[@]}" --force "$vm"
fi
log "importing the image"
incus image import -q "${P[@]}" "$work/metadata.tar.xz" "$work/os.qcow2" --alias "$alias"
rm -f "$work/os.qcow2"
log "creating $vm"
incus create -q --vm "${P[@]}" "$alias" "$vm" \
	-c security.secureboot=false -c limits.cpu="$VM_CPUS" -c limits.memory="$VM_MEMORY" -d root,size="$VM_ROOT"
incus config device add -q "${P[@]}" "$vm" vtpm tpm
incus config set "${P[@]}" "$vm" systemd.credential.fully-enable-incus-agent=true
incus image delete "${P[@]}" "$alias"

fails=0
result() { # <PASS|FAIL> <check> [detail]
	printf '%s %s%s\n' "$1" "$2" "${3:+ $3}"
	[ "$1" = PASS ] || fails=$((fails + 1))
}

# Step 6: boot.
log "starting $vm"
incus start "${P[@]}" "$vm"
started=1
booted=0
t0=$(date +%s)
while [ $(($(date +%s) - t0)) -lt 300 ]; do
	if vexec true >/dev/null 2>&1; then
		booted=1
		break
	fi
	sleep 5
done
if [ "$booted" -eq 0 ]; then
	[ "$checks" = none ] || result FAIL boot "no incus agent after 300 s"
	exit 1
fi
boot_s=$(($(date +%s) - t0))
log "agent up after ${boot_s} s"

# Step 7: the debug provider reads /root/updates/.
if [ "$provider" = debug ]; then
	vexec mkdir -p /root/updates
	incus file push -q "${P[@]}" "$work/RELEASE" "$vm/root/updates/RELEASE"
	for a in "${apps[@]}"; do
		log "pushing $a.raw"
		incus file push -q "${P[@]}" "$work/$a.raw" "$vm/root/updates/$a.raw"
	done
fi

# Step 8: ask for an update check (retrying until the API answers), then wait
# for every seeded application.
t0=$(date +%s)
posted=0
listed=""
missing="${apps[*]}"
while [ $(($(date +%s) - t0)) -lt 600 ]; do
	if [ "$posted" -eq 0 ] && osd -X POST http://localhost/1.0/system/update/:check >/dev/null 2>&1; then
		posted=1
		log "update check requested"
	fi
	if [ "$posted" -eq 1 ]; then
		listed=$(osd http://localhost/1.0/applications 2>/dev/null | jq -r '.metadata[]? | sub(".*/"; "")' 2>/dev/null || true)
		missing=""
		for a in "${apps[@]}"; do
			printf '%s\n' "$listed" | grep -qx "$a" || missing="$missing $a"
		done
		[ -n "$missing" ] || break
	fi
	sleep 10
done
if [ -z "$missing" ]; then
	log "all applications listed after $(($(date +%s) - t0)) s"
else
	log "still missing after 600 s:$missing"
fi

# Step 9: the checks.
if [ "$checks" = stock ]; then
	result PASS boot "agent after ${boot_s} s"
	for a in "${apps[@]}"; do
		if printf '%s\n' "$listed" | grep -qx "$a"; then
			result PASS app-listed "$a"
		else
			result FAIL app-listed "$a"
		fi
	done
	sb=$(vexec bootctl status 2>/dev/null | sed -n 's/^ *Secure Boot: //p' | head -1 || true)
	case "$sb" in
	enabled*) result PASS secureboot "$sb" ;;
	*) result FAIL secureboot "${sb:-unknown}" ;;
	esac
	# shellcheck disable=SC2016  # expanded by the shell in the VM
	os_id=$(vexec sh -c '. /usr/lib/os-release && printf %s "$ID"' 2>/dev/null || true)
	if [ "$os_id" = "$image_id" ]; then
		result PASS os-id "$os_id"
	else
		result FAIL os-id "got '${os_id}', want '${image_id}'"
	fi
	mode=$(vexec journalctl -u incus-osd -b0 --no-pager 2>/dev/null | grep -Eo '(^|[[:space:]])mode=[a-z]+' | tail -1 | sed 's/.*mode=//' || true)
	if [ "$mode" = dev ]; then
		result PASS mode "$mode"
	else
		result FAIL mode "got '${mode:-none}', want 'dev'"
	fi
fi

if [ "$fails" -gt 0 ]; then
	log "$fails check(s) failed; last incus-osd lines:"
	vexec journalctl -u incus-osd -b0 --no-pager 2>/dev/null | tail -40 >&2 || true
	exit 1
fi
exit 0
