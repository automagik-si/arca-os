#!/usr/bin/env bash
# Tests set-target.sh without a real installer image: a sparse 2.2 GiB file
# with a partition 2 starting at sector 4196352 stands in for one.
#
#   scripts/arca/test/set-target.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
set_target=$here/set-target.sh
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
check() { # <case> <description> <command...>
	local c=$1 d=$2
	shift 2
	if "$@"; then printf 'PASS %s: %s\n' "$c" "$d"; else printf 'FAIL %s: %s\n' "$c" "$d"; fails=$((fails + 1)); fi
}

command -v sgdisk >/dev/null 2>&1 || {
	printf 'FAIL setup: sgdisk is needed to make the fake image\n'
	exit 1
}

make_image() { # <dir> [optional seed files...]
	local d=$1 img
	shift
	mkdir -p "$d/seed"
	img=$d/IncusOS_202611100434-installer.img
	truncate -s 2200M "$img"
	sgdisk -n 2:4196352:4391151 -t 2:8300 -c 2:seed-data "$img" >/dev/null 2>&1
	printf '%s\n' '{"version":"1","force_install":true,"force_reboot":false,"target":{"bus":"NVME"}}' >"$d/seed/install.json"
	printf '%s\n' '{"version":"1","applications":[{"name":"incus"},{"name":"arca"}]}' >"$d/seed/applications.json"
	printf '%s\n' '{"version":"1","name":"images","config":{"server_url":"http://192.0.2.1:8080"}}' >"$d/seed/provider.json"
	printf '%s\n' '{"version":"1","channel":"stable","check_frequency":"never","auto_reboot":false}' >"$d/seed/update.json"
	local f
	for f in "$@"; do printf '{"version":"1","name":"%s"}\n' "$f" >"$d/seed/$f.json"; done
	(cd "$d" && sha256sum -- *-installer.img >SHA256SUMS)
}

refused() { # <name> <dir>
	local out rc
	if out=$("$set_target" "$1" "$2" 2>&1); then rc=0; else rc=$?; fi
	[ "$rc" -eq 2 ] && printf '%s' "$out" | grep -q 'refused:'
}

install_target_is() { # <install.json> <target>
	# shellcheck disable=SC2016 # jq program
	jq -e --arg t "$2" '.target.id == $t' "$1" >/dev/null
}
install_keeps() { jq -e '.version == "1" and .force_install == true and .force_reboot == false' "$1" >/dev/null; }
target_bus_kept() { jq -e '.target.bus == "NVME"' "$1" >/dev/null; }
tar_has() { tar -tf "$1/seed/seed.tar" | grep -qx "$2"; }
tar_lacks() { ! tar -tf "$1/seed/seed.tar" | grep -qx "$2"; }
tar_install_matches() { tar -xOf "$1/seed/seed.tar" install.json | cmp -s - "$1/seed/install.json"; }
seed_reads_back() { # <image dir> <image>
	local size
	size=$(wc -c <"$1/seed/seed.tar" | tr -d '[:space:]')
	dd if="$2" skip=4196352 bs=512 count=$((size / 512)) status=none | cmp -s - "$1/seed/seed.tar"
}
sums_check() { (cd "$1" && sha256sum -c --quiet SHA256SUMS); }
tar_base_only() {
	[ "$(tar -tf "$1/seed/seed.tar" | LC_ALL=C sort | tr '\n' ' ')" = "applications.json install.json provider.json update.json " ]
}

usage_ok() {
	local out rc
	if out=$("$set_target" "$@" 2>&1); then rc=0; else rc=$?; fi
	[ "$rc" -eq 2 ] && printf '%s\n' "$out" | grep -q 'set-target.sh <by-id name> \[image dir\]'
}

case_refused_usage() { usage_ok; }
case_refused_three_args() { usage_ok good-name "$imgdir" extra; }
case_help() { usage_ok --help; }

imgdir=$t/image
make_image "$imgdir" incus network
img=$imgdir/IncusOS_202611100434-installer.img
cp "$imgdir/seed/install.json" "$t/install.json.before"
cp "$imgdir/SHA256SUMS" "$t/SHA256SUMS.before"
old_sha=$(sha256sum "$img" | cut -d' ' -f1)
good=nvme-eui.0025385b71b4a123

check usage "no arguments prints the usage, exit 2" case_refused_usage
check usage-extra "three arguments prints the usage, exit 2" case_refused_three_args
check help "--help prints the usage, exit 2" case_help

for n in ADATA_SU800 samsung_990_PRO_2TB nvme-990 Expansion_Desk Generic_Flash_Disk; do
	check "refuse-$n" "refuses '$n' by its never-install name" refused "$n" "$imgdir"
done
check refuse-space "refuses a name with a space" refused 'disk with space' "$imgdir"
check refuse-tab "refuses a name with a tab" refused $'disk\twith\ttab' "$imgdir"
check refuse-slash "refuses a name with a slash" refused 'usb-fake/part1' "$imgdir"
check refuse-empty "refuses an empty name" refused '' "$imgdir"
check unchanged "refusals changed neither install.json nor SHA256SUMS" cmp -s "$imgdir/seed/install.json" "$t/install.json.before"
check unchanged-sums "refusals changed no checksum" cmp -s "$imgdir/SHA256SUMS" "$t/SHA256SUMS.before"

if ! "$set_target" "$good" "$imgdir" >"$t/out" 2>"$t/err"; then
	printf 'FAIL happy-path: set-target exited nonzero\n'
	cat "$t/err"
	exit 1
fi
check output "prints the target and that it checked the seed and sums" grep -q "target:    $good" "$t/out"
check output-seed "names every packed seed file" grep -q 'seed:      install.json applications.json provider.json update.json incus.json network.json' "$t/out"
check target-id "install.json has the new target.id" install_target_is "$imgdir/seed/install.json" "$good"
check preserve "install.json keeps version, force_install and force_reboot" install_keeps "$imgdir/seed/install.json"
check preserve-target "install.json keeps the other target selectors" target_bus_kept "$imgdir/seed/install.json"
check tar-network "seed.tar packs network.json when present" tar_has "$imgdir" network.json
check tar-incus "seed.tar packs incus.json when present" tar_has "$imgdir" incus.json
check tar-install "seed.tar carries the rewritten install.json" tar_install_matches "$imgdir"
check read-back "the seed reads back from the image at sector 4196352" seed_reads_back "$imgdir" "$img"
now_sha=$(sha256sum "$img" | cut -d' ' -f1)
check image-changed "the image checksum changed after the rewrite" test "$now_sha" != "$old_sha"
check sums-updated "SHA256SUMS has the new image checksum" grep -qx "$now_sha  $(basename "$img")" "$imgdir/SHA256SUMS"
check sums-check "SHA256SUMS verifies" sums_check "$imgdir"
check no-staging "no staging directory is left in seed/" test -z "$(find "$imgdir/seed" -maxdepth 1 -name '.set-target.*' -print -quit)"

nodir=$t/no-optional
make_image "$nodir"
if ! "$set_target" "$good" "$nodir" >"$t/out2" 2>&1; then
	printf 'FAIL happy-path-no-optional: set-target exited nonzero\n'
	cat "$t/out2"
	exit 1
fi
check tar-no-network "a seed without network.json packs no network.json" tar_lacks "$nodir" network.json
check tar-no-incus "a seed without incus.json packs no incus.json" tar_lacks "$nodir" incus.json
check tar-base "a seed without optional files packs the four base files" tar_base_only "$nodir"

if [ "$fails" -ne 0 ]; then
	printf '%d case(s) failed\n' "$fails"
	exit 1
fi
