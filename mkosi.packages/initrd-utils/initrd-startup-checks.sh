#!/bin/sh

# shellcheck disable=SC2028

duplicatePartitions=$(/usr/bin/initrd-utils check-partition-uuids)
if [ -n "$duplicatePartitions" ]; then
    for TTY in $TTYS; do
        printf '%s\n' "$duplicatePartitions" > "$TTY" || true
    done
fi

# If a physical TPM is present, verify that the PE binaries involved in boot (systemd-boot, UKI) match the TPM event log and are properly signed by a trusted certificate.
if [ ! -d /boot/swtpm ] && [ -e /sys/class/tpm/tpm0/tpm_version_major ]; then
    peBinaryStatus=$(/usr/bin/initrd-utils validate-pe-binaries 2>&1)
    if [ "$peBinaryStatus" != "" ]; then
        for TTY in $TTYS; do
            echo "\033[31m$NAME failed to verify PE binaries: $peBinaryStatus\033[0m" > "$TTY" || true
        done
        sleep 3600
    fi
fi
