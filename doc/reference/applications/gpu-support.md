# GPU support

The GPU support (`gpu-support`) application supplies firmware for AMD and Intel GPUs, and firmware used by Nouveau on NVIDIA GPUs. Its kernel drivers are part of the base image.

NVIDIA's open kernel modules, matching firmware and CUDA userspace are supplied separately by the [NVIDIA application](nvidia.md). The base image contains no NVIDIA open module copy. Driver ownership is selected at boot by PCI address; `gpu-support` leaves Nouveau, NVIDIA and VFIO bindings alone. Use the system kernel [`blacklist_modules`](../system/kernel.md) setting to prevent reloading a driver.
