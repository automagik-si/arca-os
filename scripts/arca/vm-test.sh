#!/usr/bin/env bash
# Boot an OS image in a fresh test VM and check it, following upstream's E2E
# job (.github/workflows/tests.yml, "Start the VM" to "Load the extensions").
#
#   vm-test.sh --project arcaos-* --vm <name>
#              (--from-builder <vm> | --artifacts <dir>)
#              --seed-apps <a,b,...> --provider debug|images [--server-url <url>]
#              --checks stock|tracer|none [--stop-builder] [--keep]
#   vm-test.sh --project arcaos-* --vm <name> --from-builder <vm>
#              --seed-apps <a,b,...> --provider images --server-url <url>
#              --checks wifi [--stop-builder] [--keep]
#   vm-test.sh --project arcaos-* --vm <name> --install-media <dir>
#              --checks stock|tracer|none [--keep]
#
# Steps:
#   1. Refuse to start the test VM when MemAvailable is below 12 GiB (checked
#      after --stop-builder has stopped the builder).
#   2. Write applications.json and provider.json into a seed tar, dd it into a
#      copy of the image .raw at seek=4196352 bs=512, and convert that copy to
#      qcow2 (in the builder with --from-builder, locally with --artifacts).
#   3. Pull the qcow2, the seeded applications' sysexts, the build's
#      test/metadata.tar.xz (build.sh copies it beside the image), and the
#      base.manifest, kernel.BUILDINFO and wifi-firmware.txt when the build has
#      them.
#   4. incus image import.
#   5. Create the VM: security.secureboot=false, 4 vCPU, 8 GiB, 50 GiB root, a
#      vtpm device and systemd.credential.fully-enable-incus-agent=true.
#   6. Start it and poll `incus exec <vm> -- true` for up to 300 s.
#   7. With provider debug, push RELEASE and each seeded application's .raw
#      into /root/updates/.
#   8. POST /1.0/system/update/:check (not with --install-media), then poll
#      /1.0/applications until every seeded application is listed, for up to
#      10 min. When arca is one of them, then poll `systemctl is-active
#      arca.service` for up to 10 min.
#   9. Run the checks, printing "PASS <check>" or "FAIL <check>" for each.
# The VM is stopped at the end unless --keep is given.
#
# --install-media <dir> takes a directory from tracer-installer.sh on this
# host instead: one *-installer.img, seed/, updates/, and the manifest and
# BUILDINFO files beside them. Its seed names the applications and the
# provider (images, with a server_url of http://<address>:<port>), so
# --seed-apps, --provider and --server-url are refused with it. The address
# must be this host's address on the network of the project's default eth0,
# which is the one the VM reaches, and the port (1024 or above) must be free
# on every address. Steps 2-7 become: create an empty VM as in step 5, attach
# a copy of the installer as USB boot media, start it, and wait up to 30 min
# for "was successfully installed" in incus-osd's journal; then stop it,
# detach the media, serve updates/ on that address and port with a throwaway
# python3 http.server, and boot the installed system. Its first boot installs
# the applications from that server by itself, so step 8 asks for no update
# check. The server is stopped on exit, and on any exit before the media is
# detached the VM is stopped and the media detached.
#
# Check sets:
#   stock   boot, app-listed (each seeded app), secureboot, os-id (ID in
#           /usr/lib/os-release equals the build's ImageId, read from its
#           <ImageId>_<version>.efi name), mode (mode=dev)
#   tracer  stock, plus kernel (uname -r has -zabbly-arca and equals the
#           krel in kernel.BUILDINFO), binder-fs, binder-major, binderfs-mount
#           (binder-control in a binderfs mounted on /tmp/b), arca-active
#           (active, NRestarts=0 after 120 s), arca-call (a viewer token's
#           `arca call watch_report` exits 0 and names the project default),
#           zfs (modprobe zfs, with lockdown integrity or confidentiality),
#           kernel-pkg (base.manifest has linux-image-<krel> at the BUILDINFO
#           version and no Zabbly linux-image-*-zabbly+)
#   wifi    boot, then scripts/arca/test/wifi/run.sh (its cases: first-boot-gate,
#           rank, rank-6ghz, sae, secret, regdb, firmware against the build's
#           wifi-firmware.txt, debian-units).
#           The VM has no NIC (eth0 is masked with a device of type none)
#           and gets systemd.credential.arca-wifi-hwsim=1, so it has simulated
#           radios at boot and reaches the update server only over Wi-Fi. Steps 7
#           and 8 are skipped: its first boot installs the applications once it
#           has joined. The update tree is the builder's
#           /srv/artifacts/installer/wifi-test/updates (from tracer-installer.sh;
#           refused with exit 2 when missing), served in the VM at --server-url.
#           It needs --from-builder and --provider images, and refuses
#           --install-media. run.sh's progress and diagnostics come to stdout
#           with its results; a secret-shaped line among them is withheld and
#           fails wifi-output.
#   none    no checks
# With --install-media, every set but none also checks installed,
# media-removed (no install device left on the VM), apps-from-server (the
# server answered the VM's GET of index.sjson and of each application's
# .raw.gz with 200) and updates-off (check frequency never, no automatic
# reboot, provider images at the seed's server_url).
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
WIFI_UPDATES=/srv/artifacts/installer/wifi-test/updates
# Large files are staged on disk, not under /tmp, which may be a tmpfs.
STAGE_BASE=${ARCA_VM_TEST_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/arca-os/vm-test}
OSD_SOCK=/run/incus-os/unix.socket
ARCA=/usr/local/bin/arca
ARCA_CONFIG=/usr/lib/arca/node.json

project="" vm="" builder="" artifacts="" media="" seed_apps="" provider="" server_url="" checks=""
stop_builder=0 keep=0
# With --install-media: where the update server listens, its pid, and whether
# the VM still has the install media attached.
serve_addr="" serve_port="" serve_pid="" install_attached=0

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
log() { printf 'vm-test: %s\n' "$*" >&2; }
die() { printf 'vm-test: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'vm-test: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--project) [ $# -ge 2 ] || usage; project=$2; shift 2 ;;
	--vm) [ $# -ge 2 ] || usage; vm=$2; shift 2 ;;
	--from-builder) [ $# -ge 2 ] || usage; builder=$2; shift 2 ;;
	--artifacts) [ $# -ge 2 ] || usage; artifacts=$2; shift 2 ;;
	--install-media) [ $# -ge 2 ] || usage; media=$2; shift 2 ;;
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
sources=0
for s in "$builder" "$artifacts" "$media"; do
	[ -z "$s" ] || sources=$((sources + 1))
done
[ "$sources" -eq 1 ] || refuse "give exactly one of --from-builder, --artifacts and --install-media"
if [ -n "$builder" ]; then
	printf '%s' "$builder" | grep -Eqx "$name_re" || refuse "bad --from-builder name"
fi
if [ "$vm" = "$BUILDER_NAME" ] || [ "$vm" = "$builder" ]; then
	refuse "--vm must not be the builder"
fi
[ "$stop_builder" -eq 0 ] || [ -n "$builder" ] || refuse "--stop-builder needs --from-builder"
if [ -n "$media" ]; then
	[ -z "$seed_apps$provider$server_url" ] || refuse "--install-media takes its applications and provider from its seed"
	[ -d "$media" ] || refuse "no install media directory $media"
	media=$(cd "$media" && pwd)
	[ -f "$media/seed/applications.json" ] || refuse "no seed/applications.json in $media"
	seed_apps=$(jq -r '[.applications[].name] | join(",")' "$media/seed/applications.json")
	provider=$(jq -r '.name // empty' "$media/seed/provider.json" 2>/dev/null || true)
	[ "$provider" = images ] || refuse "the install media's provider is '${provider:-none}', not images"
	server_url=$(jq -r '.config.server_url // empty' "$media/seed/provider.json")
	[ -f "$media/updates/index.sjson" ] || refuse "no updates/index.sjson in $media"
	# The VM fetches from the seed's server_url, so that is where this host
	# serves updates/: an IPv4 address and a port, and no path.
	serve_addr=$(printf '%s' "$server_url" | sed -n 's|^http://\([0-9][0-9.]*\):\([0-9]\{1,5\}\)$|\1|p')
	serve_port=$(printf '%s' "$server_url" | sed -n 's|^http://\([0-9][0-9.]*\):\([0-9]\{1,5\}\)$|\2|p')
	if [ -z "$serve_addr" ] || [ -z "$serve_port" ]; then
		refuse "the seed's server_url '${server_url:-none}' is not http://<IPv4 address>:<port>"
	fi
	if [ "$serve_port" -lt 1024 ] || [ "$serve_port" -gt 65535 ]; then
		refuse "the seed's server_url port $serve_port is not between 1024 and 65535"
	fi
fi
printf '%s' "$seed_apps" | grep -Eqx '[a-z0-9.-]+(,[a-z0-9.-]+)*' || refuse "bad or missing --seed-apps"
case "$provider" in
debug) ;;
images) [ -n "$server_url" ] || refuse "--provider images needs --server-url" ;;
*) refuse "--provider must be debug or images" ;;
esac
case "$checks" in
stock | tracer | wifi | none) ;;
*) refuse "--checks must be stock, tracer, wifi or none" ;;
esac
if [ "$checks" = wifi ]; then
	[ -z "$media" ] || refuse "--checks wifi refuses --install-media"
	[ -n "$builder" ] || refuse "--checks wifi needs --from-builder"
	[ "$provider" = images ] || refuse "--checks wifi needs --provider images --server-url <url>"
fi

P=(--project "$project")
# A project without its own images and profiles would import into, and
# delete from, the default project's images.
incus project show "$project" >/dev/null 2>&1 || refuse "no project $project"
for f in features.images features.profiles; do
	v=$(incus project get "$project" "$f" 2>/dev/null || true)
	[ "$v" = true ] || refuse "project $project has $f=${v:-unset}; it must have its own images and profiles"
done
if [ -n "$media" ]; then
	# Listen only on this host's address on the VM's network, on a port that
	# nothing on this host listens on, whatever the address.
	vm_net=$(incus profile device get "${P[@]}" default eth0 network 2>/dev/null || true)
	[ -n "$vm_net" ] || refuse "the default profile of $project has no eth0 network"
	net_addr=$(incus network get "${P[@]}" "$vm_net" ipv4.address 2>/dev/null || true)
	if [ "${net_addr%/*}" != "$serve_addr" ]; then
		refuse "the seed's server_url address $serve_addr is not this host's address on $vm_net (${net_addr:-none})"
	fi
	# shellcheck disable=SC2016  # an awk program
	if ss -ltnH | awk -v p=":$serve_port" 'substr($4, length($4) - length(p) + 1) == p { f = 1 } END { exit !f }'; then
		refuse "port $serve_port is already in use on this host"
	fi
fi
IFS=, read -r -a apps <<<"$seed_apps"
apps_json=$(jq -cn --arg a "$seed_apps" '{applications: ($a | split(",") | map({name: .}))}')
if [ "$provider" = debug ]; then
	provider_json='{"name":"debug"}'
else
	provider_json=$(jq -cn --arg u "$server_url" '{name: "images", config: {server_url: $u}}')
fi
has_arca=0
for a in "${apps[@]}"; do
	[ "$a" != arca ] || has_arca=1
done

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
# wait_agent <seconds>: 0 once `incus exec` answers.
wait_agent() {
	local t0
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt "$1" ]; do
		if vexec true >/dev/null 2>&1; then return 0; fi
		sleep 5
	done
	return 1
}
osd_journal() { vexec journalctl -u incus-osd -b0 --no-pager 2>/dev/null || true; }
# install_done: the installer said it is done, in incus-osd's journal (through
# the agent) or on the console.
install_done() {
	osd_journal | grep -qF 'was successfully installed' && return 0
	incus console "${P[@]}" "$vm" --show-log 2>/dev/null | grep -qF 'was successfully installed'
}
# serve_updates: serve $media/updates at the seed's server_url with python3's
# http.server, in isolated mode and from $work, logging each request to
# $work/http.log. Its readiness probe is a HEAD, so every GET in the log is
# the VM's.
serve_updates() {
	log "serving $media/updates at $server_url"
	(cd "$work" && exec python3 -I -u -m http.server --bind "$serve_addr" --directory "$media/updates" "$serve_port") \
		>"$work/http.log" 2>&1 &
	serve_pid=$!
	for _ in $(seq 1 20); do
		if curl -sfI --max-time 2 "$server_url/index.sjson" >/dev/null 2>&1; then return 0; fi
		kill -0 "$serve_pid" 2>/dev/null || break
		sleep 0.5
	done
	tail -5 "$work/http.log" >&2 || true
	die "the update server does not answer at $server_url"
}
# got_get <path>: 0 when the server answered a GET of <path> from another
# address than its own with 200.
got_get() {
	# shellcheck disable=SC2016  # an awk program
	awk -v p="\"GET $1 HTTP/" -v a="$serve_addr" '$1 != a && index($0, p) && index($0, "\" 200 ") { f = 1 } END { exit !f }' "$work/http.log"
}

# Without --stop-builder nothing frees memory later, so check up front too.
[ "$stop_builder" -eq 1 ] || mem_guard

mkdir -p "$STAGE_BASE"
work=$(mktemp -d "$STAGE_BASE/$vm.XXXXXX")
started=0
# shellcheck disable=SC2317,SC2329  # called from the EXIT trap
cleanup() {
	if [ -n "$serve_pid" ]; then
		kill "$serve_pid" 2>/dev/null || true
		wait "$serve_pid" 2>/dev/null || true
		log "stopped the update server"
	fi
	if [ "$started" -eq 1 ] && [ "$keep" -eq 0 ]; then
		log "stopping $vm"
		incus stop "${P[@]}" "$vm" --timeout 120 >/dev/null 2>&1 || incus stop "${P[@]}" "$vm" --force >/dev/null 2>&1 || true
	fi
	# The install media is still attached when the install did not finish. A
	# kept VM still runs from its copy in $work; otherwise the media is
	# detached, so the VM never points at a deleted file.
	if [ "$install_attached" -eq 1 ]; then
		if [ "$started" -eq 1 ] && [ "$keep" -eq 1 ]; then
			log "$vm is kept with its install media, $work/install.img"
			return 0
		fi
		incus config device remove -q "${P[@]}" "$vm" install >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
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
for f in base.manifest kernel.BUILDINFO wifi-firmware.txt; do
	if [ -f "$f" ]; then cp "$f" "$out/"; fi
done
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
	if [ "$checks" = wifi ] && ! incus exec "${P[@]}" "$builder" -- test -f "$WIFI_UPDATES/index.sjson"; then
		refuse "no $WIFI_UPDATES/index.sjson in $builder (run tracer-installer.sh --out ${WIFI_UPDATES%/updates} there)"
	fi
	bout=/srv/artifacts/vm-test/$vm
	log "seeding and converting the image in $builder"
	incus exec "${P[@]}" "$builder" -- bash -c "$PREP" vm-test-prep \
		"$BUILDER_ARTIFACTS" "$bout" "$apps_json" "$provider_json" "$SEED_OFFSET"
	log "pulling the qcow2 and sysexts from $builder"
	for f in os.qcow2 metadata.tar.xz RELEASE OSNAME SRC; do
		incus file pull -q "${P[@]}" "$builder$bout/$f" "$work/"
	done
	for f in base.manifest kernel.BUILDINFO wifi-firmware.txt; do
		incus file pull -q "${P[@]}" "$builder$bout/$f" "$work/" 2>/dev/null || true
	done
	bsrc=$(cat "$work/SRC")
	if [ "$provider" = debug ]; then
		for a in "${apps[@]}"; do
			incus file pull -q "${P[@]}" "$builder$bsrc/$a.raw" "$work/" || die "no $a.raw in $builder:$bsrc"
		done
	fi
	if [ "$checks" = wifi ]; then
		log "pulling the update tree and the debug sysext from $builder"
		incus file pull -q -r "${P[@]}" "$builder$WIFI_UPDATES" "$work/"
		incus file pull -q "${P[@]}" "$builder$bsrc/debug.raw" "$work/" || die "no debug.raw in $builder:$bsrc"
	fi
	incus exec "${P[@]}" "$builder" -- rm -rf "$bout"
	if [ "$stop_builder" -eq 1 ]; then
		log "stopping builder $builder"
		incus stop "${P[@]}" "$builder" --timeout 120 || incus stop "${P[@]}" "$builder" --force
	fi
elif [ -n "$artifacts" ]; then
	[ -d "$artifacts" ] || die "no artifacts directory $artifacts"
	log "seeding and converting the image from $artifacts"
	bash -c "$PREP" vm-test-prep "$artifacts" "$work/prep" "$apps_json" "$provider_json" "$SEED_OFFSET"
	mv "$work"/prep/{os.qcow2,metadata.tar.xz,RELEASE,OSNAME,SRC} "$work/"
	for f in base.manifest kernel.BUILDINFO wifi-firmware.txt; do
		if [ -f "$work/prep/$f" ]; then mv "$work/prep/$f" "$work/"; fi
	done
	if [ "$provider" = debug ]; then
		for a in "${apps[@]}"; do
			[ -f "$artifacts/$a.raw" ] || die "no $a.raw in $artifacts"
			cp --sparse=always "$artifacts/$a.raw" "$work/"
		done
	fi
else
	shopt -s nullglob
	imgs=("$media"/*-installer.img)
	shopt -u nullglob
	[ ${#imgs[@]} -eq 1 ] || die "want exactly one *-installer.img in $media"
	for f in RELEASE OSNAME; do
		[ -f "$media/$f" ] || die "no $f in $media"
		cp "$media/$f" "$work/"
	done
	for f in base.manifest kernel.BUILDINFO wifi-firmware.txt; do
		if [ -f "$media/$f" ]; then cp "$media/$f" "$work/"; fi
	done
	[ -f "$media/updates/$(cat "$work/RELEASE")/update.sjson" ] || die "updates/ in $media has no release $(cat "$work/RELEASE")"
	# The VM writes to its media; the files in $media stay as they are.
	log "copying the install media"
	cp --sparse=always "${imgs[0]}" "$work/install.img" || die "could not copy ${imgs[0]}"
	chmod 0755 "$work"
	chmod 0644 "$work/install.img"
fi
release=$(cat "$work/RELEASE")
image_id=$(cat "$work/OSNAME")
[ -n "$image_id" ] || die "no ImageId in the artifacts"
log "release $release, image $image_id, apps ${apps[*]}, provider $provider"

# Step 1: the memory guard, after the builder has stopped.
mem_guard

# Steps 4 and 5: a fresh image (or empty disk) and a fresh VM.
alias=$vm
if incus image info "${P[@]}" "$alias" >/dev/null 2>&1; then
	incus image delete "${P[@]}" "$alias"
fi
if [ -n "$(instance_state "$vm")" ]; then
	log "deleting the previous $vm"
	incus delete "${P[@]}" --force "$vm"
fi
vm_config=(-c security.secureboot=false -c limits.cpu="$VM_CPUS" -c limits.memory="$VM_MEMORY" -d "root,size=$VM_ROOT")
if [ -z "$media" ]; then
	log "importing the image"
	incus image import -q "${P[@]}" "$work/metadata.tar.xz" "$work/os.qcow2" --alias "$alias"
	rm -f "$work/os.qcow2"
	log "creating $vm"
	incus create -q --vm "${P[@]}" "$alias" "$vm" "${vm_config[@]}"
else
	log "creating an empty $vm"
	incus init -q --empty --vm "${P[@]}" "$vm" "${vm_config[@]}"
fi
incus config device add -q "${P[@]}" "$vm" vtpm tpm
incus config set "${P[@]}" "$vm" systemd.credential.fully-enable-incus-agent=true
if [ "$checks" = wifi ]; then
	incus config device add -q "${P[@]}" "$vm" eth0 none
	incus config set "${P[@]}" "$vm" systemd.credential.arca-wifi-hwsim=1
fi
if [ -z "$media" ]; then
	incus image delete "${P[@]}" "$alias"
fi

fails=0
result() { # <PASS|FAIL> <check> [detail]
	printf '%s %s%s\n' "$1" "$2" "${3:+ $3}"
	[ "$1" = PASS ] || fails=$((fails + 1))
}

installed=""
if [ -n "$media" ]; then
	# The install, from USB boot media, onto the empty root disk.
	incus config device add -q "${P[@]}" "$vm" install disk source="$work/install.img" io.bus=usb boot.priority=10 readonly=false \
		|| die "could not attach the install media to $vm"
	install_attached=1
	log "starting $vm from the installer"
	started=1
	incus start "${P[@]}" "$vm" || die "could not start $vm from the installer"
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt 1800 ]; do
		if install_done; then
			installed="after $(($(date +%s) - t0)) s"
			break
		fi
		sleep 15
	done
	if [ -z "$installed" ]; then
		[ "$checks" = none ] || result FAIL installed "no 'was successfully installed' after 1800 s"
		osd_journal | tail -40 >&2
		# The cleanup stops the VM and detaches the install media.
		exit 1
	fi
	log "installed $installed; removing the install media"
	incus stop "${P[@]}" "$vm" --force || die "could not stop $vm"
	incus config device remove -q "${P[@]}" "$vm" install || die "could not detach the install media from $vm"
	install_attached=0
	# The installed system's first boot fetches its applications from here.
	serve_updates
fi

# Step 6: boot.
log "starting $vm"
incus start "${P[@]}" "$vm"
started=1
t0=$(date +%s)
if ! wait_agent 300; then
	[ "$checks" = none ] || result FAIL boot "no incus agent after 300 s"
	exit 1
fi
boot_s=$(($(date +%s) - t0))
log "agent up after ${boot_s} s"

# Step 7: the debug provider reads /root/updates/ (the installed system of
# --install-media fetches its applications from the update server instead).
if [ "$provider" = debug ]; then
	vexec mkdir -p /root/updates
	incus file push -q "${P[@]}" "$work/RELEASE" "$vm/root/updates/RELEASE"
	for a in "${apps[@]}"; do
		log "pushing $a.raw"
		incus file push -q "${P[@]}" "$work/$a.raw" "$vm/root/updates/$a.raw"
	done
fi

# Step 8: ask for an update check (retrying until the API answers), then wait
# for every seeded application. An installed system is not asked: its first
# boot must install them by itself.
t0=$(date +%s)
posted=0
listed=""
missing="${apps[*]}"
step8=1
[ "$checks" != wifi ] || step8=0
while [ "$step8" -eq 1 ] && [ $(($(date +%s) - t0)) -lt 600 ]; do
	if [ "$posted" -eq 0 ] && [ -z "$media" ] && osd -X POST http://localhost/1.0/system/update/:check >/dev/null 2>&1; then
		posted=1
		log "update check requested"
	fi
	if [ "$posted" -eq 1 ] || [ -n "$media" ]; then
		listed=$(osd http://localhost/1.0/applications 2>/dev/null | jq -r '.metadata[]? | sub(".*/"; "")' 2>/dev/null || true)
		missing=""
		for a in "${apps[@]}"; do
			printf '%s\n' "$listed" | grep -qx "$a" || missing="$missing $a"
		done
		[ -n "$missing" ] || break
	fi
	sleep 10
done
if [ "$step8" -eq 0 ]; then
	:
elif [ -z "$missing" ]; then
	log "all applications listed after $(($(date +%s) - t0)) s"
else
	log "still missing after 600 s:$missing"
fi
arca_state=""
arca_seen=0
if [ "$has_arca" -eq 1 ] && [ "$step8" -eq 1 ]; then
	t0=$(date +%s)
	while [ $(($(date +%s) - t0)) -lt 600 ]; do
		arca_state=$(vexec systemctl is-active arca.service 2>/dev/null || true)
		if [ "$arca_state" = active ]; then
			arca_seen=$(date +%s)
			break
		fi
		sleep 10
	done
	log "arca.service: ${arca_state:-unknown} after $(($(date +%s) - t0)) s"
fi

# Step 9: the checks.
stock_checks() {
	local a sb os_id mode
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
	mode=$(osd_journal | grep -Eo '(^|[[:space:]])mode=[a-z]+' | tail -1 | sed 's/.*mode=//' || true)
	if [ "$mode" = dev ]; then
		result PASS mode "$mode"
	else
		result FAIL mode "got '${mode:-none}', want 'dev'"
	fi
}

# The kernel release may end in "+": apt-get reads a trailing "+" as "install
# this", and a regular expression reads it as a repeat. Every comparison with
# it below is therefore a fixed-string one (shell =, grep -F, jq ==).
tracer_checks() {
	local bi krel want_krel want_ver want_meta major ld signer wait_s nr st out names call_rc got zabbly meta
	bi=$work/kernel.BUILDINFO
	want_krel=$(sed -n 's/^krel: //p' "$bi" 2>/dev/null || true)
	want_ver=$(sed -n 's/^version: //p' "$bi" 2>/dev/null || true)
	want_meta=$(sed -n 's/^meta-version: //p' "$bi" 2>/dev/null || true)

	krel=$(vexec uname -r 2>/dev/null || true)
	if [ -z "$want_krel" ]; then
		result FAIL kernel "running '${krel:-unknown}', and the build has no kernel.BUILDINFO"
	elif [ "$krel" = "$want_krel" ] && printf '%s' "$krel" | grep -qF -- -zabbly-arca; then
		result PASS kernel "$krel"
	else
		result FAIL kernel "running '${krel:-unknown}', built '$want_krel'"
	fi

	if vexec grep -qw binder /proc/filesystems; then
		result PASS binder-fs "binder in /proc/filesystems"
	else
		result FAIL binder-fs "no binder in /proc/filesystems"
	fi

	# shellcheck disable=SC2016  # an awk program
	major=$(vexec awk '$2 == "binder" { print $1; exit }' /proc/devices 2>/dev/null || true)
	if printf '%s' "$major" | grep -Eqx '[0-9]+'; then
		result PASS binder-major "binder is major $major in /proc/devices"
	else
		result FAIL binder-major "no binder line in /proc/devices"
	fi

	# shellcheck disable=SC2016  # expanded by the shell in the VM
	if vexec sh -euc 'mkdir -p /tmp/b; mount -t binder binder /tmp/b; rc=0; test -c /tmp/b/binder-control || rc=1; umount /tmp/b; rmdir /tmp/b; exit $rc' 2>/dev/null; then
		result PASS binderfs-mount "/tmp/b/binder-control, then unmounted"
	else
		result FAIL binderfs-mount "no /tmp/b/binder-control in a binderfs mount"
	fi

	if [ "$arca_seen" -eq 0 ]; then
		result FAIL arca-active "arca.service is '${arca_state:-unknown}' after 600 s"
	else
		wait_s=$((120 - ($(date +%s) - arca_seen)))
		if [ "$wait_s" -gt 0 ]; then sleep "$wait_s"; fi
		st=$(vexec systemctl is-active arca.service 2>/dev/null || true)
		nr=$(vexec systemctl show -p NRestarts --value arca.service 2>/dev/null || true)
		if [ "$st" = active ] && [ "$nr" = 0 ]; then
			result PASS arca-active "active, NRestarts=0 after 120 s"
		else
			result FAIL arca-active "is '$st', NRestarts=${nr:-unknown} after 120 s"
		fi
	fi

	# A throwaway viewer token in the VM; only its id and scope are printed.
	# The call must exit 0 as well as name the project.
	names="" call_rc=""
	vexec rm -f /root/tracer.secret
	if vexec "$ARCA" token mint --scope viewer --id tracer --config "$ARCA_CONFIG" --out /root/tracer.secret >/dev/null 2>&1 \
		&& vexec systemctl restart arca.service; then
		for _ in $(seq 1 30); do
			if vexec test -S /run/arca/arca.sock 2>/dev/null; then break; fi
			sleep 2
		done
		if out=$(vexec "$ARCA" call watch_report --config "$ARCA_CONFIG" --token-file /root/tracer.secret --args '{}' 2>/dev/null); then
			call_rc=0
		else
			call_rc=$?
		fi
		names=$(printf '%s' "$out" | jq -r '.projects[]?.project' 2>/dev/null || true)
	fi
	vexec rm -f /root/tracer.secret
	if [ -z "$call_rc" ]; then
		result FAIL arca-call "could not mint a viewer token or restart arca.service"
	elif [ "$call_rc" != 0 ]; then
		result FAIL arca-call "arca call watch_report exited $call_rc"
	elif printf '%s\n' "$names" | grep -qxF default; then
		result PASS arca-call "exit 0, watch_report projects: $(printf '%s\n' "$names" | paste -sd, -)"
	else
		result FAIL arca-call "watch_report exited 0 but did not name the project default"
	fi

	# zfs is signed for this kernel, so it must load with lockdown on.
	ld=$(vexec cat /sys/kernel/security/lockdown 2>/dev/null | sed -n 's/.*\[\([a-z]*\)\].*/\1/p' || true)
	if vexec modprobe zfs 2>/dev/null && vexec test -d /sys/module/zfs; then
		signer=$(vexec modinfo -F signer zfs 2>/dev/null || true)
		case "$ld" in
		integrity | confidentiality) result PASS zfs "loaded, lockdown=$ld, signer '${signer:-unknown}'" ;;
		*) result FAIL zfs "loaded, but lockdown is '${ld:-unknown}', not integrity or confidentiality" ;;
		esac
	else
		result FAIL zfs "modprobe zfs failed (lockdown=${ld:-unknown})"
	fi

	if [ ! -f "$work/base.manifest" ] || [ -z "$want_krel" ]; then
		result FAIL kernel-pkg "no base.manifest or kernel.BUILDINFO in the build"
	else
		got=$(jq -r --arg n "linux-image-$want_krel" '.packages[] | select(.name == $n) | .version' "$work/base.manifest")
		zabbly=$(jq -r '.packages[] | .name | select(startswith("linux-image-") and contains("-zabbly") and (contains("-zabbly-arca") | not) and . != "linux-image-zabbly")' "$work/base.manifest")
		meta=$(jq -r '.packages[] | select(.name == "linux-image-zabbly") | .version' "$work/base.manifest")
		if [ "$got" = "$want_ver" ] && [ -z "$zabbly" ] && [ "$meta" = "$want_meta" ]; then
			result PASS kernel-pkg "linux-image-$want_krel $got, linux-image-zabbly $meta, no Zabbly kernel"
		else
			result FAIL kernel-pkg "linux-image-$want_krel '${got:-absent}' (want '$want_ver'), linux-image-zabbly '${meta:-absent}' (want '$want_meta'), Zabbly: '$(printf '%s' "$zabbly" | paste -sd, -)'"
		fi
	fi
}

install_checks() {
	local devs a f gets client upd prov freq reboot name url
	result PASS installed "$installed"
	devs=$(incus config device list "${P[@]}" "$vm" 2>/dev/null || true)
	if [ "$install_attached" -eq 0 ] && ! printf '%s\n' "$devs" | grep -qx install; then
		result PASS media-removed "devices: $(printf '%s\n' "$devs" | paste -sd, -)"
	else
		result FAIL media-removed "the install device is still attached"
	fi
	# Every application's file, by its path in the published index.
	gets=""
	got_get /index.sjson || gets="$gets /index.sjson"
	for a in "${apps[@]}"; do
		f=$(jq -r --arg n "$a.raw.gz" '[.updates[0].files[] | select((.filename | split("/") | last) == $n) | .filename] | first // empty' "$media/updates/index.json")
		if [ -z "$f" ] || ! got_get "/$release/$f"; then gets="$gets /$release/${f:-$a.raw.gz}"; fi
	done
	# shellcheck disable=SC2016  # an awk program
	client=$(awk -v a="$serve_addr" '$1 != a && index($0, "\"GET ") { print $1; exit }' "$work/http.log" 2>/dev/null || true)
	if [ -z "$gets" ]; then
		result PASS apps-from-server "${client:-the VM} got /index.sjson and $(printf '%s.raw.gz ' "${apps[@]}")with 200 from $server_url"
	else
		result FAIL apps-from-server "no GET answered with 200 from the VM for:$gets"
	fi
	upd=$(osd http://localhost/1.0/system/update 2>/dev/null || true)
	prov=$(osd http://localhost/1.0/system/provider 2>/dev/null || true)
	freq=$(printf '%s' "$upd" | jq -r '.metadata.config.check_frequency // empty' 2>/dev/null || true)
	reboot=$(printf '%s' "$upd" | jq -r '.metadata.config.auto_reboot' 2>/dev/null || true)
	name=$(printf '%s' "$prov" | jq -r '.metadata.config.name // empty' 2>/dev/null || true)
	url=$(printf '%s' "$prov" | jq -r '.metadata.config.config.server_url // empty' 2>/dev/null || true)
	if [ "$freq" = never ] && [ "$reboot" = false ] && [ "$name" = images ] && [ "$url" = "$server_url" ]; then
		result PASS updates-off "check_frequency never, auto_reboot false, provider images at the seed's server_url"
	else
		result FAIL updates-off "check_frequency '${freq:-unknown}', auto_reboot '${reboot:-unknown}', provider '${name:-unknown}' at '${url:-unknown}'"
	fi
}

# withhold_secrets <flag file>: copies lines from stdin, but replaces a
# secret-shaped one (a passphrase field, a psk= or sae_password= line, a private
# key) with a note and creates the flag file.
withhold_secrets() {
	local line
	while IFS= read -r line || [ -n "$line" ]; do
		if printf '%s\n' "$line" | grep -Eiq '"passphrase" *: *"[^"]|psk=|sae_password=|PRIVATE KEY'; then
			: >"$1"
			printf 'vm-test: a secret-shaped line was withheld\n'
		else
			printf '%s\n' "$line"
		fi
	done
}

wifi_checks() {
	local rc=0 leak="$work/wifi-secret-shaped"
	result PASS boot "agent after ${boot_s} s"
	rm -f "$leak"
	# Both streams, progress and diagnostics included, go through one check and on to stdout.
	"$(dirname "$0")/test/wifi/run.sh" --project "$project" --vm "$vm" --server-url "$server_url" \
		--updates "$work/updates" --debug-sysext "$work/debug.raw" \
		--firmware "$work/wifi-firmware.txt" 2>&1 | withhold_secrets "$leak" || rc=$?
	[ "$rc" -eq 0 ] || result FAIL wifi "run.sh exited $rc"
	[ ! -e "$leak" ] || result FAIL wifi-output "run.sh printed a secret-shaped line (withheld)"
}

if [ "$checks" = wifi ]; then
	wifi_checks
elif [ "$checks" != none ]; then
	stock_checks
	if [ "$checks" = tracer ]; then tracer_checks; fi
	if [ -n "$media" ]; then install_checks; fi
fi

if [ "$fails" -gt 0 ]; then
	log "$fails check(s) failed; last incus-osd lines:"
	osd_journal | tail -40 >&2
	if [ -n "$serve_pid" ]; then
		log "last update server lines:"
		tail -20 "$work/http.log" >&2 || true
	fi
	exit 1
fi
exit 0
