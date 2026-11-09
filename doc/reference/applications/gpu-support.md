# GPU support

The GPU support (`gpu-support`) application includes the required firmware files for GPUs from one of:

* `amd`
* `nvidia`
* `intel`

```{note}
`gpu-support` only includes firmware files - it does not include any drivers (Kernel modules are always part of the IncusOS base image, never leaf images like `gpu-support`).
```

The base image includes NVIDIA's open GPU kernel modules (`nvidia`, `nvidia-modeset`, `nvidia-drm`, `nvidia-uvm`, and `nvidia-peermem`), built for the image kernel and signed with the image's Secure Boot key. Nouveau is blacklisted, and `gpu-support` prefers the NVIDIA modules when available.

This includes only the open kernel modules. NVIDIA userspace libraries (including `libcuda`), `nvidia-smi`, matching GSP firmware, and `nvidia-container-cli` are not supplied by this support. The modules alone do not provide working CUDA or GPU acceleration; matching firmware and userspace are still required.

Use the system kernel [`blacklist_modules`](../system/kernel.md) setting to prevent `gpu-support` from reloading a driver, for example `nvidia` for PCI passthrough. A reboot may be required to unload drivers that are already in use.
