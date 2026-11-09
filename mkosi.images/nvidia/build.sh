#!/bin/sh -eux

python3 /work/src/mkosi.images/nvidia/install.py /buildroot "${DESTDIR}"
KERNEL="$(ls /buildroot/usr/lib/modules/)"
SOURCE=/usr/src/nvidia
cp "/buildroot/boot/config-${KERNEL}" "/buildroot/usr/src/linux-headers-${KERNEL}/.config"
mkosi-chroot make -C "${SOURCE}" -j"$(nproc)" modules \
    "KERNEL_UNAME=${KERNEL}" "SYSSRC=/usr/src/linux-headers-${KERNEL}"
mkdir -p "${DESTDIR}/usr/lib/modules/${KERNEL}/nvidia/"
for mod in nvidia nvidia-modeset nvidia-drm nvidia-uvm nvidia-peermem; do
    test -z "$(modinfo -F signer "/buildroot${SOURCE}/kernel-open/${mod}.ko")"
    "/buildroot/usr/src/linux-headers-${KERNEL}/scripts/sign-file" \
        sha256 /work/src/mkosi.key /work/src/mkosi.crt \
        "/buildroot${SOURCE}/kernel-open/${mod}.ko" \
        "${DESTDIR}/usr/lib/modules/${KERNEL}/nvidia/${mod}.ko"
done
printf '%s\n' "${KERNEL}" > "${DESTDIR}/usr/share/nvidia/kernel"
modinfo -F firmware "${DESTDIR}/usr/lib/modules/${KERNEL}/nvidia/nvidia.ko" \
    > "${DESTDIR}/usr/share/nvidia/firmware.txt"
