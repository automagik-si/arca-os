#!/bin/sh
# Exercise the ESP generator with an isolated efivarfs fixture, without /sys writes.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
generator=$here/../../../mkosi.images/base/mkosi.extra/usr/lib/systemd/system-generators/arca-boot-esp-generator
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/efivars"
variable=$tmp/efivars/LoaderDevicePartUUID-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f
uuid=01234567-89ab-cdef-0123-456789abcdef
failed=0

write_variable() {
	printf '\006\000\000\000' > "$variable"
	printf '%s\000' "$1" | iconv -f ASCII -t UTF-16LE >> "$variable"
}

check() {
	name=$1 what=$2
	rm -rf "$tmp/normal" "$tmp/early" "$tmp/late"
	ARCA_BOOT_EFIVARS=$tmp/efivars dash "$generator" "$tmp/normal" "$tmp/early" "$tmp/late"
	device=$(systemd-escape --path "$what")
	fsck=systemd-fsck@$device.service
	if [ ! -e "$tmp/normal/boot.mount" ] && [ ! -e "$tmp/late/boot.mount" ] &&
		grep -Fxq "What=$what" "$tmp/early/boot.mount" &&
		grep -Fxq "Requires=$fsck" "$tmp/early/boot.mount" &&
		grep -Fxq "After=$fsck" "$tmp/early/boot.mount" &&
		grep -Fxq "After=blockdev@$device.target" "$tmp/early/boot.mount" &&
		grep -Fxq "BindsTo=$device.device" "$tmp/early/$fsck" &&
		grep -Fxq "After=$device.device local-fs-pre.target" "$tmp/early/$fsck" &&
		grep -Fxq "ExecStart=-/usr/lib/systemd/systemd-fsck $what" "$tmp/early/$fsck"; then
		echo "PASS $name"
	else
		echo "FAIL $name"
		failed=1
	fi
	if [ "$what" != /dev/disk/by-partlabel/esp ] && grep -rq 'partlabel' "$tmp/early"; then
		echo "FAIL $name retains a dependency on the other ESP"
		failed=1
	fi
}

check missing-variable /dev/disk/by-partlabel/esp
write_variable "$(printf '%s' "$uuid" | tr 'a-f' 'A-F')"
check uppercase-utf16 /dev/disk/by-partuuid/$uuid
write_variable "$uuid"
check device-not-yet-present /dev/disk/by-partuuid/$uuid
write_variable not-a-uuid
check invalid-uuid /dev/disk/by-partlabel/esp
write_variable "$uuid"
printf '\001' >> "$variable"
check truncated-utf16 /dev/disk/by-partlabel/esp
write_variable "$uuid"
printf '\001' | dd of="$variable" bs=1 seek=5 conv=notrunc 2>/dev/null
check non-ascii-utf16 /dev/disk/by-partlabel/esp
rm "$variable"
mkdir "$variable"
check unreadable-variable /dev/disk/by-partlabel/esp

exit "$failed"
