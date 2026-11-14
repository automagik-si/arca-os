#!/usr/bin/env bash
# Tests the initrd TPM diagnosis (mkosi.packages/initrd-utils/initrd-tpm-diagnosis.sh)
# against a freshly built initrd-utils, with a fake ESP, fake efivars and fake PCR
# files; mountpoint, mount and umount are stubbed. Needs go; runs shellcheck on the
# script when it is installed.
#
#   scripts/arca/test/tpm-diagnosis.sh
#
# It prints "PASS <case>" or "FAIL <case>" per case and exits 1 on any FAIL.
set -euo pipefail

top=$(cd "$(dirname "$0")/../../.." && pwd)
script=$top/mkosi.packages/initrd-utils/initrd-tpm-diagnosis.sh
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fails=0
check() { # <case> <description> <command...>
	local c=$1 d=$2
	shift 2
	if "$@"; then printf 'PASS %s: %s\n' "$c" "$d"; else printf 'FAIL %s: %s\n' "$c" "$d"; fails=$((fails + 1)); fi
}

(cd "$top/incus-osd" && go build -o "$t/initrd-utils" ./cmd/initrd-utils)

# mountpoint says /boot is mounted while $T/boot-mounted exists; mount copies the fake ESP and logs its arguments.
mkdir -p "$t/bin"
cat >"$t/bin/mountpoint" <<'EOF'
#!/bin/sh
[ -e "$T/boot-mounted" ]
EOF
cat >"$t/bin/mount" <<'EOF'
#!/bin/sh
echo "mount $*" >>"$T/mount.log"
for target; do :; done
eval "source=\${$(($# - 1))}"
cp -r "$source"/. "$target"/
EOF
cat >"$t/bin/umount" <<'EOF'
#!/bin/sh
echo "umount $*" >>"$T/mount.log"
rm -rf "${1:?}"/*
EOF
chmod +x "$t"/bin/*

A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
C=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc

# efivar <0|1> <file>: a one-byte efivarfs file, after its four attribute bytes.
efivar() {
	printf '\x06\x00\x00\x00' >"$2"
	if [ "$1" = 1 ]; then printf '\x01'; else printf '\x00'; fi >>"$2"
}

# luks2 <keyslots JSON>: the root volume, a LUKS2 header with these keyslots.
luks2() {
	{
		printf 'LUKS\xba\xbe\x00\x02\x00\x00\x00\x00\x00\x00\x40\x00'
		head -c 4080 /dev/zero
		printf '{"keyslots":%s,"tokens":{}}' "$1"
	} >"$t/root-luks"
	truncate -s 16384 "$t/root-luks"
}

# setup <secure boot 0|1> <setup mode 0|1> <pcr4> <pcr7> <pcr12>: the current state; the record is sealed
# with Secure Boot off at A/B/C. The ESP is the device to mount unless $T/boot-mounted exists.
setup() {
	rm -rf "${t:?}/esp" "${t:?}/boot" "${t:?}/efivars" "${t:?}/pcr" "${t:?}/mnt" "$t/root-luks" "$t/mount.log" "$t/boot-mounted" "$t/tty1" "$t/ttyS0"
	mkdir -p "$t/esp" "$t/boot" "$t/efivars" "$t/pcr"
	cat >"$t/esp/arca-tpm-seal.json" <<EOF
{"version": 1, "uki_version": "202611141118", "secure_boot_enabled": false, "setup_mode": false,
 "pcr4": "$A", "pcr7": "$B", "pcr12": "$C", "sealed_at": "2026-11-14T11:00:00Z"}
EOF
	efivar "$1" "$t/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"
	efivar "$2" "$t/efivars/SetupMode-8be4df61-93ca-11d2-aa0d-00e098032b8c"
	echo "${3^^}" >"$t/pcr/4"
	echo "${4^^}" >"$t/pcr/7"
	echo "${5^^}" >"$t/pcr/12"
	: >"$t/tty1"
	: >"$t/ttyS0"
	luks2 '{"1":{},"2":{},"3":{}}'
}

run() {
	env T="$t" PATH="$t/bin:$PATH" TTYS="$t/tty1 $t/ttyS0" BOOT_DIR="$t/boot" ESP_DEVICE="$t/esp" MOUNT_DIR="$t/mnt" \
		EFIVARS_DIR="$t/efivars" PCR_DIR="$t/pcr" ROOT_LUKS="$t/root-luks" INITRD_UTILS="${INITRD_UTILS:-$t/initrd-utils}" sh "$script"
}

quiet() { run && [ ! -s "$t/tty1" ] && [ ! -s "$t/ttyS0" ]; }

# says <text...>: the script exits 0 and both consoles show every text, in yellow.
says() {
	run || return 1
	local tty want
	for tty in "$t/tty1" "$t/ttyS0"; do
		grep -q $'\033\\[0;33m' "$tty" || return 1
		for want; do grep -qF -- "$want" "$tty" || return 1; done
	done
}

lines() { [ "$(wc -l <"$t/tty1")" -eq "$1" ]; }
mounted_ro() { grep -qx "mount -o ro $t/esp $t/mnt" "$t/mount.log" && grep -qx "umount $t/mnt" "$t/mount.log" && [ ! -e "$t/mnt" ]; }

if command -v shellcheck >/dev/null; then
	check shellcheck "the script is shellcheck clean" shellcheck "$script"
fi

setup 0 0 "$A" "$B" "$C"
check equal "a healthy boot prints nothing" quiet
check equal "the ESP was mounted read-only and released" mounted_ro

setup 0 0 "$A" "$B" "$A"
check pcr12 "boot parameters changed, with the way back" says \
	"the boot parameters changed" "PCR12 sealed cccccccccccccccc..., now aaaaaaaaaaaaaaaa..." \
	"Way back: remove the extra kernel command line" "8-word recovery passphrase" "tpm-rebind"
check pcr12 "five lines" lines 5

setup 0 0 "$C" "$B" "$C"
check pcr4 "a different OS image, with the way back" says \
	"a different OS image" "(version 202611141118)" "Way back: pick the sealed version in the boot menu" "8-word recovery passphrase"

setup 0 0 "$A" "$C" "$C"
check pcr7 "Secure Boot keys changed, with the way back" says \
	"the Secure Boot keys changed" "Way back: restore the firmware's Secure Boot keys" "8-word recovery passphrase"

setup 1 0 "$A" "$C" "$C"
check sb-state "Secure Boot state changed, with the way back" says \
	"the Secure Boot state changed" "Sealed with Secure Boot off, now Secure Boot on." \
	"Way back: set the firmware to Secure Boot off with the same keys" "8-word recovery passphrase"

setup 0 0 "$A" "$B" "$C"
rm "$t/esp/arca-tpm-seal.json"
check missing "an old disk gets the generic line" says "type the 8-word recovery passphrase" "tpm-rebind"
check missing "one line" lines 1
luks2 '{"0":{}}'
: >"$t/tty1"
: >"$t/ttyS0"
check fresh "a volume created on this boot stays quiet" quiet

setup 0 0 "$A" "$B" "$A"
touch "$t/boot-mounted"
cp "$t/esp/arca-tpm-seal.json" "$t/boot/"
check mounted "an ESP already at /boot is read in place" says "the boot parameters changed"
check mounted "and not mounted again" test ! -e "$t/mount.log"

setup 0 0 "$A" "$B" "$A"
rm -rf "$t/pcr"
check no-tpm "no TPM: nothing to say" quiet

setup 0 0 "$A" "$B" "$A"
rm -rf "$t/esp"
check no-esp "no ESP: nothing to say" quiet

setup 0 0 "$A" "$B" "$A"
printf '#!/bin/sh\nsleep 30\n' >"$t/hang"
chmod +x "$t/hang"
INITRD_UTILS=$t/hang
start=$(date +%s)
check hang "a hanging diagnosis times out quietly" quiet
check hang "within ten seconds" test $(($(date +%s) - start)) -lt 10
unset INITRD_UTILS

setup 0 0 "$A" "$B" "$A"
check bad-tty "a console that can't be opened doesn't stop the others" \
	env T="$t" PATH="$t/bin:$PATH" TTYS="$t/missing/tty1 $t/ttyS0" BOOT_DIR="$t/boot" ESP_DEVICE="$t/esp" MOUNT_DIR="$t/mnt" \
	EFIVARS_DIR="$t/efivars" PCR_DIR="$t/pcr" ROOT_LUKS="$t/root-luks" INITRD_UTILS="$t/initrd-utils" sh "$script"
check bad-tty "the serial console still shows it" grep -qF "the boot parameters changed" "$t/ttyS0"

[ "$fails" -eq 0 ]
