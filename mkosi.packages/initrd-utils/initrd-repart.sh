#!/bin/sh
set -eu

# Keep root creation on the boot disk even when another disk has the same /usr partition GUID.
BOOT_ESP=$(findmnt --noheadings --output SOURCE --mountpoint /boot)
BOOT_DISK=$(lsblk --noheadings --nodeps --paths --raw --output PKNAME "$BOOT_ESP")
[ -n "$BOOT_DISK" ]

exec systemd-repart --dry-run=no "$BOOT_DISK"
