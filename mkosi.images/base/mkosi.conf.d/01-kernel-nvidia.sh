#!/bin/sh -eux

KERNEL="$(ls /buildroot/usr/lib/modules/)"
VERSION=620.70.06
SHA256=bc9fa156dcdc5e0ed19c3c9ea70cfe2b8c9a0fdc415426af1ba5c3a1f9a7238d
SOURCE="/buildroot/usr/src/nvidia-${VERSION}"

curl --proto =https --proto-redir =https --fail --location --retry 3 \
    "https://github.com/NVIDIA/open-gpu-kernel-modules/archive/refs/tags/${VERSION}.tar.gz" \
    --output /tmp/nvidia.tar.gz
echo "${SHA256}  /tmp/nvidia.tar.gz" | sha256sum --check
mkdir -p "${SOURCE}"
tar -xzf /tmp/nvidia.tar.gz --strip-components=1 -C "${SOURCE}"
rm /tmp/nvidia.tar.gz

cp "/buildroot/boot/config-${KERNEL}" "/buildroot/usr/src/linux-headers-${KERNEL}/.config"
cat > "${SOURCE}/dkms.conf" <<EOF
PACKAGE_NAME="nvidia"
PACKAGE_VERSION="${VERSION}"
MAKE[0]="'make' -j$(nproc) modules KERNEL_UNAME=\${kernelver} SYSSRC=/usr/src/linux-headers-${KERNEL}"
AUTOINSTALL="no"
EOF

index=0
for mod in nvidia nvidia-modeset nvidia-drm nvidia-uvm nvidia-peermem; do
    cat >> "${SOURCE}/dkms.conf" <<EOF
BUILT_MODULE_NAME[${index}]="${mod}"
BUILT_MODULE_LOCATION[${index}]="kernel-open"
DEST_MODULE_LOCATION[${index}]="/updates/dkms"
EOF
    index=$((index + 1))
done

mkosi-chroot dkms build -m nvidia -v "${VERSION}" -k "${KERNEL}" \
    --kernelsourcedir "/usr/src/linux-headers-${KERNEL}"

mkdir -p "${DESTDIR}/usr/lib/modules/${KERNEL}/updates/dkms/"
for mod in nvidia nvidia-modeset nvidia-drm nvidia-uvm nvidia-peermem; do
    "/buildroot/usr/src/linux-headers-${KERNEL}/scripts/sign-file" \
        sha256 /work/src/mkosi.key /work/src/mkosi.crt \
        "/buildroot/var/lib/dkms/nvidia/${VERSION}/${KERNEL}/$(uname -m)/module/${mod}.ko" \
        "${DESTDIR}/usr/lib/modules/${KERNEL}/updates/dkms/${mod}.ko"
done

mkdir -p "${DESTDIR}/usr/share/licenses/nvidia-open-kernel-modules/"
cp "${SOURCE}/COPYING" "${DESTDIR}/usr/share/licenses/nvidia-open-kernel-modules/"
mkdir -p "${DESTDIR}/usr/share/nvidia-open-kernel-modules/"
awk -F '|' '/^\|/ { split($3, ids, " "); if (ids[1] ~ /^[[:xdigit:]]{4}$/) print tolower(ids[1]) }' \
    "${SOURCE}/README.md" | sort -u > "${DESTDIR}/usr/share/nvidia-open-kernel-modules/supported-gpus"
test -s "${DESTDIR}/usr/share/nvidia-open-kernel-modules/supported-gpus"
mkdir -p "${DESTDIR}/usr/lib/modprobe.d/"
cat > "${DESTDIR}/usr/lib/modprobe.d/nvidia.conf" <<EOF
blacklist nvidia
blacklist nvidia_drm
blacklist nvidia_modeset
blacklist nvidia_uvm
softdep nvidia pre: vfio-pci
EOF
