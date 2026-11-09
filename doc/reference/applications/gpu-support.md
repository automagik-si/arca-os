# GPU support

The GPU support (`gpu-support`) application supplies firmware for AMD and Intel GPUs, and firmware used by Nouveau on NVIDIA GPUs.

```{note}
`gpu-support` includes firmware files, not kernel drivers. Kernel modules are part of the IncusOS base image.
```

The base image includes NVIDIA's open GPU kernel modules (`nvidia`, `nvidia-modeset`, `nvidia-drm`, `nvidia-uvm`, and `nvidia-peermem`), built for the image kernel and signed with the image's Secure Boot key. These modules stay dormant today: their matching NVIDIA GSP and ucode firmware is not shipped. Nouveau remains the default driver, including on pre-Turing cards.

`gpu-support` only activates `nvidia` for a device listed in the pinned release's Turing-or-newer compatibility table, and only when every firmware file reported by `modinfo -F firmware nvidia` exists under `/usr/lib/firmware/nvidia/620.70.06/`. Older cards keep Nouveau, including in a host with both generations. Devices already bound to `vfio-pci` or another driver are left alone. `gpu-support` loads `vfio-pci` before considering NVIDIA activation, giving PCI passthrough priority. The image also supplies `softdep nvidia pre: vfio-pci` for manual module loading.

Automatic loading of `nvidia`, `nvidia_drm`, `nvidia_modeset`, and `nvidia_uvm` is disabled by the image's `/usr/lib/modprobe.d/nvidia.conf`. Guarded activation uses `modprobe --use-blacklist --config /etc/modprobe.d nvidia`: it bypasses the factory autoload blacklist while retaining the operator's modprobe configuration. Use the system kernel [`blacklist_modules`](../system/kernel.md) setting to prevent activation or reloading of a driver.

NVIDIA userspace libraries (including `libcuda`), `nvidia-smi`, matching GSP/ucode firmware, and `nvidia-container-cli` are not included in this phase. The open modules alone do not provide working CUDA or GPU acceleration. A reboot may be required when changing driver or passthrough configuration.
