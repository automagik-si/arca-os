#!/usr/bin/env bash
# Builds and publishes the images the update VM proof (updates-vm.sh) uses.
#
#   scripts/arca/test/updates-build.sh <evidence-dir> <snapshot-dir> <name>=<worktree|version>...
#
# Each <name> is built from <worktree>, or reuses the builder's /srv/artifacts/os/<version> when
# given a 12-digit version, and is published, in order, into one private tree. The tree after each
# publish is copied to <snapshot-dir>/t1, t2, ... The VM image (<snapshot-dir>/vm) is made from the
# first one, with a seed that installs incus, arca and debug and points the images provider at the
# host's proof server, PROOF_ADDR:PROOF_PORT (required; e.g. PROOF_ADDR=192.0.2.1 PROOF_PORT=8095,
# the host's address on the VMs' bridge). version-<name>.txt and
# the logs go to <evidence-dir>. Takes the builder lock unless ARCA_BUILDER_LOCK_HELD=1; restores
# the artifacts' latest link, removes the tree and stops the builder at the end.
set -uo pipefail

[ $# -ge 3 ] || {
	echo "usage: $0 <evidence-dir> <snapshot-dir> <name>=<worktree|version>..." >&2
	exit 2
}
EV=$1 SNAP=$2
shift 2
WT=$(cd "$(dirname "$0")/../../.." && pwd)
P=(--project "${ARCA_PROJECT:-arcaos-dev}")
B=arcaos-builder
TREE=/srv/artifacts/proof-updates
IB=("$WT/scripts/arca/in-builder.sh")
SERVER=http://${PROOF_ADDR:?set PROOF_ADDR to the host address the VM reaches}:${PROOF_PORT:?set PROOF_PORT}

mkdir -p "$EV" "$SNAP"
log() { printf '%s updates-build: %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$EV/updates-build.log" >&2; }
bx() { incus exec "${P[@]}" "$B" -- "$@" </dev/null; }

if [ "${ARCA_BUILDER_LOCK_HELD:-}" != 1 ]; then
	exec 9>>"${ARCA_BUILDER_LOCK:-${XDG_RUNTIME_DIR:-/tmp}/arcaos-builder.lock}"
	if ! flock -n 9; then
		log "waiting for the builder lock"
		flock 9
	fi
	export ARCA_BUILDER_LOCK_HELD=1
fi

incus start "${P[@]}" "$B" </dev/null >/dev/null 2>&1
for _ in $(seq 60); do
	bx true 2>/dev/null && break
	sleep 3
done
latest=$(bx readlink /srv/artifacts/os/latest)
log "artifacts latest is $latest"
finish() {
	bx ln -sfn "$latest" /srv/artifacts/os/latest
	bx rm -rf "$TREE" /srv/artifacts/proof-vm
	incus stop "${P[@]}" "$B" </dev/null
	log "latest restored to $latest, tree removed, builder stopped"
}
trap finish EXIT
bx rm -rf "$TREE"

# build <name> <worktree|version>: writes version-<name>.txt.
build() {
	if [[ $2 =~ ^[0-9]{12}$ ]]; then
		bx test -d "/srv/artifacts/os/$2" || {
			log "no build $2 on the builder"
			return 1
		}
		log "$1 reuses build $2"
		echo "$2" >"$EV/version-$1.txt"
		return 0
	fi
	log "building $1 from $2"
	"${IB[@]}" "$2" -- scripts/arca/build.sh --local-kernel /srv/artifacts/kernel/latest --no-latest >"$EV/build-$1.log" 2>&1 </dev/null
	local rc=$? v
	v=$(sed -n 's/^version: //p' "$EV/build-$1.log" | tail -1)
	log "build $1 exit $rc version ${v:-none}"
	[ "$rc" -eq 0 ] && [ -n "$v" ] || return 1
	echo "$v" >"$EV/version-$1.txt"
}

# publish <name> <snapshot>
publish() {
	local v rc
	v=$(cat "$EV/version-$1.txt")
	"${IB[@]}" "$WT" -- scripts/arca/publish-build.sh --version "$v" --out "$TREE" --channel stable --apps incus,arca,debug \
		>"$EV/publish-$1.log" 2>&1 </dev/null
	rc=$?
	log "publish $1 ($v) exit $rc"
	[ "$rc" -eq 0 ] || return 1
	rm -rf "${SNAP:?}/$2"
	mkdir -p "$SNAP/$2"
	bx tar -C "$TREE" -cf - . | tar -C "$SNAP/$2" -xf -
}

n=0
first=
for spec in "$@"; do
	n=$((n + 1))
	name=${spec%%=*}
	build "$name" "${spec#*=}" && publish "$name" "t$n" || exit 1
	[ -n "$first" ] || first=$(cat "$EV/version-$name.txt")
done

# The VM's disk: the first image with the seed.
apps='{"applications":[{"name":"incus"},{"name":"arca"},{"name":"debug"}]}'
provider='{"name":"images","config":{"server_url":"'$SERVER'"}}'
# shellcheck disable=SC2016
bx bash -c '
set -euo pipefail
src=/srv/artifacts/os/$1 out=/srv/artifacts/proof-vm
rm -rf "$out"; mkdir -p "$out"; cd "$src"
efi=$(ls *_"$1".efi); os=${efi%_*}
cp metadata.tar.xz "$out/"
printf "%s\n" "$2" > "$out/applications.json"
printf "%s\n" "$3" > "$out/provider.json"
tar -C "$out" -cf "$out/seed.tar" applications.json provider.json
cp --sparse=always "${os}_$1.raw" "$out/os.raw"
dd if="$out/seed.tar" of="$out/os.raw" seek=4196352 bs=512 conv=notrunc status=none
qemu-img convert -f raw -O qcow2 "$out/os.raw" "$out/os.qcow2"
rm -f "$out/os.raw"
' prep "$first" "$apps" "$provider" >"$EV/prep-vm.log" 2>&1 || {
	log "VM image could not be made"
	exit 1
}
mkdir -p "$SNAP/vm"
for f in os.qcow2 metadata.tar.xz; do
	incus file pull -q "${P[@]}" "$B/srv/artifacts/proof-vm/$f" "$SNAP/vm/" </dev/null || exit 1
done
log "VM image from $first in $SNAP/vm"
