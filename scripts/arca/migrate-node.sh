#!/usr/bin/env bash
# Migrate a node onto a persistent-CA build with no reinstall. A node whose
# embedded update CA and Secure Boot signing certificate no longer exist can
# verify no signed update, so the build is written directly: a privileged
# container on the node's own Incus (which this host's client certificate
# administers) enters the node's namespaces and runs arca-node-migrate there,
# which writes the build and re-seals the TPM for the new UKI exactly as
# incus-osd's own update apply does. The node then reboots into the new build
# and updates itself normally from then on.
#
#   migrate-node.sh --remote <incus-remote> --version <12-digit>
#                   --tree <update-tree> --server-url <http://addr:port>
#                   [--channel stable] [--check-frequency 1h]
#                   [--helper <path>] [--image images:debian/13]
#                   [--container arca-migrator] [--wait <seconds>]
#                   [--check] [--keep-container] [--no-reboot]
#
# --remote      the node's Incus remote; its /os API is reached through it.
# --version     the build to install, as published in --tree.
# --tree        the update tree publish-update.sh wrote (the served
#               directory); the build's files are read from <version>/ and
#               checked against its update.json.
# --server-url  the update server the node checks once migrated.
# --helper      the static arca-node-migrate binary (default
#               $ARCA_NODE_MIGRATE or ~/.cache/arca-os/arca-node-migrate);
#               build it with:
#                 cd incus-osd && CGO_ENABLED=0 go build -trimpath \
#                   -o arca-node-migrate ./internal/arca/nodemigrate
# --check       only the read-only preflight (node state, tree, helper).
# --wait        how long to wait for the node after the reboot (default 900).
#
# Steps: preflight (Secure Boot off, TPM ok, the running version older than
# --version, the tree has the build and every installed application, all
# checksums match); launch the container (privileged, unconfined, the node's
# /proc at /hostproc); push the helper and the build; run the helper in the
# node's namespaces; delete the container; point the provider at
# --server-url; reboot; wait for the node to answer again on --version; set
# the update policy (channel, check frequency, automatic reboot); report.
#
# If the node instead asks for its recovery passphrase on that boot, the
# re-seal did not take: type it once at the node, then
#   incus query -X POST <remote>:/os/1.0/system/security/:tpm-rebind
# (see scripts/arca/README.md). The container is deleted on every exit unless
# --keep-container is given. Exit codes: 0 done, 1 failed, 2 refused.
set -euo pipefail

remote='' version='' tree='' server_url=''
channel=stable check_frequency=1h
helper=${ARCA_NODE_MIGRATE:-$HOME/.cache/arca-os/arca-node-migrate}
image=images:debian/13 container=arca-migrator wait_s=900
check_only=false keep_container=false do_reboot=true
arch=x86_64

log() { printf 'migrate-node: %s\n' "$*" >&2; }
die() { printf 'migrate-node: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'migrate-node: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--remote | --version | --tree | --server-url | --channel | --check-frequency | --helper | --image | --container | --wait)
		[ $# -ge 2 ] || refuse "need a value for $1"
		case "$1" in
		--remote) remote=$2 ;;
		--version) version=$2 ;;
		--tree) tree=$2 ;;
		--server-url) server_url=$2 ;;
		--channel) channel=$2 ;;
		--check-frequency) check_frequency=$2 ;;
		--helper) helper=$2 ;;
		--image) image=$2 ;;
		--container) container=$2 ;;
		--wait) wait_s=$2 ;;
		esac
		shift 2 ;;
	--check) check_only=true; shift ;;
	--keep-container) keep_container=true; shift ;;
	--no-reboot) do_reboot=false; shift ;;
	-h | --help) sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

for v in remote version tree server_url; do
	[ -n "${!v}" ] || refuse "--${v//_/-} is required"
done
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || refuse "version '$version' is not 12 digits"
printf '%s' "$server_url" | grep -Eqx 'https?://[^/[:space:]]+' || refuse "--server-url '$server_url' is not http(s)://<host>[:<port>]"
printf '%s' "$container" | grep -Eqx '[a-z][a-z0-9-]{0,62}' || refuse "bad --container name"
printf '%s' "$wait_s" | grep -Eqx '[0-9]+' || refuse "--wait takes seconds"
case "$check_frequency" in never) refuse "--check-frequency never would leave the node without updates" ;; esac
command -v incus >/dev/null || die "incus is not on PATH"
command -v jq >/dev/null || die "jq is not on PATH"

osq() { incus query "$remote:/os/1.0$1" "${@:2}"; }

# --- Preflight (read-only) ---------------------------------------------------
log "preflight on $remote"
root=$(osq "") || die "the node's /os API does not answer through $remote"
running=$(jq -r '.environment.os_version // empty' <<<"$root")
[ -n "$running" ] || die "the node did not report its running version"
# Only the non-secret fields of the security state are read.
sec=$(osq /system/security | jq -c '{sb: .state.secure_boot_enabled, tpm: .state.tpm_status, volumes: [.state.encrypted_volumes[]? | "\(.volume)=\(.state)"]}') \
	|| die "could not read the node's security state"
apps=$(osq /applications | jq -r '.[]? | sub("^.*/"; "")' | sort | paste -sd, -)
log "node: running $running; secure boot $(jq -r .sb <<<"$sec"); tpm $(jq -r .tpm <<<"$sec"); volumes $(jq -r '.volumes | join(" ")' <<<"$sec"); applications ${apps:-none}"
[ "$(jq -r .sb <<<"$sec")" = false ] || refuse "secure boot is enabled on the node; this migration is for the Secure-Boot-disabled case"
[ "$(jq -r .tpm <<<"$sec")" = ok ] || refuse "the node's TPM status is not ok"
[ "$running" \< "$version" ] || refuse "the node runs $running, not older than $version"
[ -n "$apps" ] || die "the node reports no installed applications"

# The build's files in the tree, checked against its update.json.
upd=$tree/$version/update.json
[ -f "$upd" ] || die "no $upd (publish the build with publish-update.sh first)"
files=$(jq -r --arg a "$arch" '.files[] | select(.architecture == $a) | "\(.sha256) \(.filename)"' "$upd")
[ -n "$files" ] || die "no $arch files for $version in $upd"
need=()
while read -r sum name; do
	case "$name" in
	*.efi.gz | *.usr-*.raw.gz) need+=("$name") ;;
	*.raw.gz)
		app=$(basename "$name" .raw.gz)
		case ",$apps," in *",$app,"*) need+=("$name") ;; esac ;;
	*) continue ;;
	esac
	[ -f "$tree/$version/$name" ] || die "missing $tree/$version/$name"
	got=$(sha256sum "$tree/$version/$name" | cut -d' ' -f1)
	[ "$got" = "$sum" ] || die "checksum mismatch for $name"
done <<<"$files"
for app in ${apps//,/ }; do
	printf '%s\n' "${need[@]}" | grep -Eq "(^|/)$app\.raw\.gz\$" || die "the node runs $app but the tree has no $app for $version"
done
[ "$(printf '%s\n' "${need[@]}" | grep -c '\.usr-')" -eq 3 ] || die "the tree does not hold the three usr images for $version"
log "tree: ${#need[@]} files for $version verified against update.json"
[ -x "$helper" ] || die "no helper at $helper (see --help)"
file -b "$helper" 2>/dev/null | grep -q 'statically linked' || die "$helper is not a static binary"
incus info "$remote:$container" >/dev/null 2>&1 && refuse "$remote:$container already exists"

if $check_only; then
	echo "preflight: ok (running $running -> $version; applications $apps)"
	exit 0
fi

# --- Container ---------------------------------------------------------------
cleanup() {
	if ! $keep_container; then
		incus delete -f "$remote:$container" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

log "launching $container on $remote (privileged, unconfined, with the node's /proc)"
incus init -q "$image" "$remote:$container" -c security.privileged=true \
	-c raw.lxc="$(printf 'lxc.apparmor.profile=unconfined\nlxc.cgroup2.devices.allow=a\n')"
incus config device add -q "$remote:$container" hostproc disk source=/proc path=/hostproc
incus start "$remote:$container"
for _ in $(seq 60); do
	incus exec "$remote:$container" -- true 2>/dev/null && break
	sleep 2
done
incus exec "$remote:$container" -- test -e /hostproc/1/ns/mnt || die "the container does not see the node's /proc"

log "pushing the helper and ${#need[@]} build files"
incus exec "$remote:$container" -- mkdir -p /root/build
incus file push -q --mode 0755 "$helper" "$remote:$container/root/arca-node-migrate"
for name in "${need[@]}"; do
	incus file push -q "$tree/$version/$name" "$remote:$container/root/build/$(basename "$name")"
done
sums=$(for name in "${need[@]}"; do grep " $name\$" <<<"$files" | sed "s| .*/| |"; done)
# shellcheck disable=SC2016 # expanded by the container's shell
incus exec "$remote:$container" -- sh -euc 'cd /root/build && printf "%s\n" "$1" | sed "s/ /  /" | sha256sum -c --quiet - && gunzip -f ./*.gz' sh "$sums" \
	|| die "the pushed files did not verify in the container"

pid=$(incus query "$remote:/1.0/instances/$container/state" | jq -r '.pid')
printf '%s' "$pid" | grep -Eqx '[0-9]+' || die "could not read the container's process id"

# --- Migration ---------------------------------------------------------------
# Enter the node's namespaces and run the helper there. The node's PID
# namespace is an ancestor of the container's, so it cannot be joined (and the
# helper does not need it); every other namespace can. The environment is
# cleared so the node's own tools do not inherit the container's "container=lxc"
# marker, which would make systemd skip the boot-loader EFI writes. The helper
# and its build are reached through /proc/<container-pid>/root in the node's
# mount namespace.
log "running arca-node-migrate in the node's namespaces"
ns=/hostproc/1/ns
incus exec "$remote:$container" -- \
	env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	nsenter --mount=$ns/mnt --uts=$ns/uts --ipc=$ns/ipc --net=$ns/net --cgroup=$ns/cgroup -- \
	"/proc/$pid/root/root/arca-node-migrate" --version "$version" --source "/proc/$pid/root/root/build" \
	|| die "arca-node-migrate failed; the node keeps running $running (nothing to undo)"

cleanup
trap - EXIT
$keep_container || log "deleted $container"

# Point the provider at the new server, keeping its other settings.
log "pointing the node's provider at $server_url"
provider=$(osq /system/provider | jq -c --arg u "$server_url" '{config: (.config | .config.server_url = $u)}')
osq /system/provider -X PUT -d "$provider" >/dev/null || die "could not set the provider; set server_url to $server_url"

if ! $do_reboot; then
	echo "migrated: $running -> $version (not rebooted)"
	echo "next: incus query -X POST $remote:/os/1.0/system/:reboot -d '{\"next_boot_version\":\"newest\"}', then set the update policy"
	exit 0
fi

log "rebooting the node into the new build"
# The reboot handler decodes a body into SystemNextBootVersion, so an object is
# required; "newest" selects the just-installed UKI (it also re-sets the one-shot
# from the node itself, where bootctl can write EFI variables).
osq /system/:reboot -X POST -d '{"next_boot_version":"newest"}' >/dev/null \
	|| die "could not reboot the node through its /os API"

# Wait for the node to answer on the new version (it answers on the old one
# for a few seconds while shutting down).
t0=$(date +%s) now=''
while [ $(($(date +%s) - t0)) -lt "$wait_s" ]; do
	sleep 10
	now=$(osq "" 2>/dev/null | jq -r '.environment.os_version // empty' || true)
	[ "$now" = "$version" ] && break
done
if [ "$now" != "$version" ]; then
	echo "migrate-node: the node did not answer on $version within $wait_s s after the reboot (last seen: ${now:-no answer})." >&2
	echo "If it shows a recovery passphrase prompt, type it once, then:" >&2
	echo "  incus query -X POST $remote:/os/1.0/system/security/:tpm-rebind" >&2
	exit 1
fi
log "the node is back on $now after $(($(date +%s) - t0)) s"

policy=$(osq /system/update | jq -c --arg c "$channel" --arg f "$check_frequency" \
	'{config: (.config | .channel = $c | .check_frequency = $f | .auto_reboot = true)}')
osq /system/update -X PUT -d "$policy" >/dev/null || die "could not set the update policy"

sec=$(osq /system/security | jq -c '{sb: .state.secure_boot_enabled, tpm: .state.tpm_status, volumes: [.state.encrypted_volumes[]? | "\(.volume)=\(.state)"]}')
echo "migrated-version: $now"
echo "tpm: $(jq -r .tpm <<<"$sec"); volumes: $(jq -r '.volumes | join(" ")' <<<"$sec")"
echo "applications: $(osq /applications | jq -r '.[]? | sub("^.*/"; "")' | sort | paste -sd, -)"
echo "provider: $(osq /system/provider | jq -r '.config.config.server_url')"
echo "update: $(osq /system/update | jq -c '.config | {channel, check_frequency, auto_reboot}')"
