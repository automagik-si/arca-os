#!/usr/bin/env bash
# Migrate a node onto a persistent-CA build with no reinstall. For a node whose
# embedded update CA and Secure Boot signing certificate no longer exist, no
# signed update can be verified, so the OS is written directly: a privileged
# container on the node's own Incus (which we admin), given the node's disk and
# TPM, runs arca-node-migrate in the node's context to write the new build and
# re-seal the TPM for the new UKI. The node then reboots into the new build and
# updates itself normally from then on.
#
#   migrate-node.sh --remote <incus-remote> --os-api <https://host:8443/os/1.0>
#                   --version <12-digit> --build-dir <dir>
#                   --server-url <http://addr:port> [--channel stable]
#                   [--check-frequency 1h] [--project default]
#                   [--image images:debian/13] [--container arca-migrator]
#                   [--keep-container] [--no-reboot]
#
# --remote      the node's Incus remote (this host's client cert is trusted).
# --os-api      the node's /os API base, reached with the same client cert.
# --version     the build to install (as published under --build-dir).
# --build-dir   a directory holding the build's UKI and usr images for
#               <version> (as build.sh wrote them under /srv/artifacts/os, or
#               the serving tree's <version>/<arch> unpacked).
# --server-url  the update server the node checks after migrating.
#
# Preferred path (zero keyboard): the container re-seals the TPM before reboot,
# so the next boot unlocks automatically. If re-sealing cannot run in the
# container, boot once with the recovery passphrase, then POST
# /os/1.0/system/security/:tpm-rebind; see scripts/arca/README.md.
#
# It stops after reboot-and-reconfigure and prints what it did. It never writes
# outside the named project and never touches a node other than --remote.
set -euo pipefail

remote='' os_api='' version='' build_dir='' server_url=''
channel=stable check_frequency=1h project=default
image=images:debian/13 container=arca-migrator
keep_container=false do_reboot=true

log() { printf 'migrate-node: %s\n' "$*" >&2; }
die() { printf 'migrate-node: %s\n' "$*" >&2; exit 1; }
refuse() { printf 'migrate-node: refused: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--remote) remote=$2; shift 2 ;;
	--os-api) os_api=$2; shift 2 ;;
	--version) version=$2; shift 2 ;;
	--build-dir) build_dir=$2; shift 2 ;;
	--server-url) server_url=$2; shift 2 ;;
	--channel) channel=$2; shift 2 ;;
	--check-frequency) check_frequency=$2; shift 2 ;;
	--project) project=$2; shift 2 ;;
	--image) image=$2; shift 2 ;;
	--container) container=$2; shift 2 ;;
	--keep-container) keep_container=true; shift ;;
	--no-reboot) do_reboot=false; shift ;;
	-h | --help) sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
	*) refuse "unknown argument: $1" ;;
	esac
done

for v in remote os_api version build_dir server_url; do
	[ -n "${!v}" ] || refuse "--${v//_/-} is required"
done
printf '%s' "$version" | grep -Eqx '[0-9]{12}' || refuse "version '$version' is not 12 digits"
[ -d "$build_dir" ] || die "no build directory $build_dir"
command -v incus >/dev/null || die "incus is not on PATH"
command -v curl >/dev/null || die "curl is not on PATH"

helper=$(dirname "$0")/../../.arca-node-migrate
[ -x "$helper" ] || die "helper binary not found at $helper; build it first:
  in-builder.sh <checkout> -- sh -euc 'cd incus-osd && CGO_ENABLED=0 go build -o /srv/artifacts/arca-node-migrate ./internal/arca/nodemigrate'
then copy it beside this script as .arca-node-migrate"

inc() { incus "$@"; }

# Launch a privileged container on the node, with its disk and TPM, and bind
# the node's state (recovery keys) and ESP so the helper can write and re-seal.
log "launching $container on $remote (privileged)"
inc launch "$image" "$remote:$container" --project "$project" -c security.privileged=true >/dev/null
cleanup() { $keep_container || inc delete -f "$remote:$container" --project "$project" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# The devices and binds the helper needs in the node's context. The exact
# source paths are the node's; adjust per node layout.
inc config device add "$remote:$container" tpm unix-char source=/dev/tpmrm0 --project "$project" >/dev/null
inc config device add "$remote:$container" bootdisk disk source=/dev/disk/by-partlabel path=/dev/disk/by-partlabel recursive=true --project "$project" >/dev/null 2>&1 || \
	log "note: could not add by-partlabel passthrough; the rehearsal determines the working device recipe"
inc config device add "$remote:$container" state disk source=/var/lib/incus-os path=/var/lib/incus-os --project "$project" >/dev/null 2>&1 || true
inc config device add "$remote:$container" esp disk source=/boot path=/boot --project "$project" >/dev/null 2>&1 || true

# Wait for the container, install the tools the helper shells out to.
for _ in $(seq 60); do inc exec "$remote:$container" --project "$project" -- true 2>/dev/null && break; sleep 2; done
inc exec "$remote:$container" --project "$project" -- sh -c 'command -v systemd-cryptenroll >/dev/null || (apt-get update -qq && apt-get install -y -qq systemd cryptsetup-bin)' >/dev/null 2>&1 || true

# Push the helper and the build's files into the container.
log "pushing the helper and build $version"
inc file push "$helper" "$remote:$container/root/arca-node-migrate" --project "$project" --mode 0755 >/dev/null
inc exec "$remote:$container" --project "$project" -- mkdir -p /root/build
for f in "$build_dir"/*_"$version".efi "$build_dir"/*_"$version".usr-*.raw; do
	[ -e "$f" ] || continue
	inc file push "$f" "$remote:$container/root/build/$(basename "$f")" --project "$project" >/dev/null
done

# Run the migration in the node's context.
log "running the migration (writes the build and re-seals the TPM)"
inc exec "$remote:$container" --project "$project" -- /root/arca-node-migrate --version "$version" --source /root/build

cleanup
trap - EXIT

# Point the node at the update server and set an automatic check frequency.
log "setting the node's update policy through the /os API"
curl -sf -X PUT "$os_api/system/provider" \
	--data "$(printf '{"config":{"server_url":"%s"}}' "$server_url")" >/dev/null || log "note: set provider manually if this failed"
curl -sf -X PUT "$os_api/system/update" \
	--data "$(printf '{"config":{"channel":"%s","check_frequency":"%s","auto_reboot":true}}' "$channel" "$check_frequency")" >/dev/null \
	|| log "note: set update policy manually if this failed"

if $do_reboot; then
	log "rebooting the node into the new build"
	curl -sf -X POST "$os_api/system/:reboot" >/dev/null 2>&1 || log "reboot the node manually (incus/OS API)"
fi

echo "migrated-version: $version"
echo "server-url: $server_url"
echo "check-frequency: $check_frequency"
echo "note: if the node asks for a recovery passphrase on first boot, re-sealing did not apply;"
echo "      boot once with it, then POST $os_api/system/security/:tpm-rebind"
