#!/bin/sh

# Before the passphrase prompt, say in plain words why the TPM will not unlock the root volume and the way back.
# It only reads: the ESP is mounted read-only when needed. It prints nothing when the seal record matches.
# BOOT_DIR, ESP_DEVICE, MOUNT_DIR, EFIVARS_DIR, PCR_DIR, ROOT_LUKS and INITRD_UTILS can be overridden for testing.

BOOT_DIR="${BOOT_DIR:-/boot}"
ESP_DEVICE="${ESP_DEVICE:-/dev/disk/by-partlabel/esp}"
MOUNT_DIR="${MOUNT_DIR:-/run/arca-tpm-diagnosis}"
EFIVARS_DIR="${EFIVARS_DIR:-/sys/firmware/efi/efivars}"
PCR_DIR="${PCR_DIR:-/sys/class/tpm/tpm0/pcr-sha256}"
ROOT_LUKS="${ROOT_LUKS:-/dev/gpt-auto-root-luks}"
INITRD_UTILS="${INITRD_UTILS:-/usr/bin/initrd-utils}"

# Without a TPM there is nothing to diagnose.
if [ ! -d "${PCR_DIR}" ]; then
    exit 0
fi

# Use the ESP at /boot, or mount it read-only by its partition label.
ESP_DIR="${BOOT_DIR}"
if ! mountpoint -q "${BOOT_DIR}"; then
    ESP_DIR=""
    if [ -e "${ESP_DEVICE}" ] && mkdir -p "${MOUNT_DIR}"; then
        if timeout 5 mount -o ro "${ESP_DEVICE}" "${MOUNT_DIR}"; then
            ESP_DIR="${MOUNT_DIR}"
            trap 'umount "${MOUNT_DIR}" 2> /dev/null; rmdir "${MOUNT_DIR}" 2> /dev/null' EXIT
        else
            rmdir "${MOUNT_DIR}" 2> /dev/null
        fi
    fi
fi

# Without the ESP, the record can't be read; stay quiet rather than guess.
if [ -z "${ESP_DIR}" ]; then
    exit 0
fi

MESSAGE=$(timeout 5 "${INITRD_UTILS}" diagnose-tpm "${ESP_DIR}/arca-tpm-seal.json" "${EFIVARS_DIR}" "${PCR_DIR}" "${ROOT_LUKS}" 2> /dev/null)
if [ -z "${MESSAGE}" ]; then
    exit 0
fi

for TTY in $TTYS; do
    printf '%s\n' "${MESSAGE}" | while IFS= read -r LINE; do
        printf '\033[0;33m%s\033[0m\n' "${LINE}"
    done 2> /dev/null > "$TTY" || true
done

exit 0
