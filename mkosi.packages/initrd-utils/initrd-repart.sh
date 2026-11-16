#!/bin/sh
set -eu

# A label-fallback ESP may belong to an installed disk when booting from CDROM.
efivars=${ARCA_BOOT_EFIVARS:-/sys/firmware/efi/efivars}
variable=$efivars/LoaderDevicePartUUID-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f

# efivarfs has four attribute bytes, then a NUL-terminated UTF-16LE UUID.
uuid=$(od -An -v -j4 -tu1 "$variable" 2>/dev/null | awk '
{
	for (i = 1; i <= NF; i++) {
		n++
		if (n % 2 == 0 || n == 73) {
			if ($i != 0) bad = 1
		} else {
			value = value sprintf("%c", $i)
		}
	}
}
END {
	if (n != 74 || bad) exit 1
	print tolower(value)
}')
printf '%s\n' "$uuid" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'

# Keep root creation on the loader's disk even when another disk has the same /usr GUID.
BOOT_ESP=$(findmnt --noheadings --output SOURCE --mountpoint /boot)
BOOT_UUID=$(lsblk --noheadings --nodeps --raw --output PARTUUID "$BOOT_ESP" | tr '[:upper:]' '[:lower:]')
[ "$BOOT_UUID" = "$uuid" ]
BOOT_DISK=$(lsblk --noheadings --nodeps --paths --raw --output PKNAME "$BOOT_ESP")
[ -n "$BOOT_DISK" ]

exec systemd-repart --dry-run=no "$BOOT_DISK"
