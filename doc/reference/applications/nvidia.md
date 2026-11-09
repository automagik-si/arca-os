# NVIDIA

The `nvidia` application supplies the curated NVIDIA open driver for supported Turing and newer cards. Install it with `incus admin os application add nvidia`, then reboot. Installation also adds `gpu-support` for Nouveau fallback firmware. AMD, Intel and older NVIDIA cards continue to use their in-kernel drivers and the `gpu-support` firmware application.

The application contains modules built for this OS kernel and signed once with the image key, matching GSP and ucode firmware, CUDA and NVML libraries, `nvidia-smi`, `libnvidia-container`, and the NVIDIA Container Toolkit. Incus supplies its LXC NVIDIA hook; this application supplies the `nvidia-container-cli` that hook invokes for `nvidia.runtime=true`. Real GPU and container workload validation is required before production use.

## Driver choice

The application configuration has `driver: nvidia` (the installed default) or `driver: off`. `off` disables the NVIDIA application driver and uses Nouveau. The `cards` map overrides individual PCI addresses with `nvidia`, `vfio-pci`, or `off`; a card set to `off` stays unbound. For example:

```yaml
config:
  driver: nvidia
  pin_current_versions: false
  cards:
    "0000:01:00.0": nvidia
    "0000:02:00.0": vfio-pci
```

Choice changes, application updates and removal require a reboot; the API reports `needs_reboot`. No live driver switching is attempted. Existing system PCI passthrough configuration takes precedence, including vendor/device assignments. Address-specific passthrough uses `driver_override` instead of registering a device ID, so an identical second card remains independently assignable.

The base image suppresses Nouveau and NVIDIA autoload. The boot preparation service generates modprobe configuration, sets per-address overrides, and explicitly loads drivers after the extensions merge. Nouveau remains available for unsupported cards and fallback. The NVIDIA DRM module is loaded with `modeset=1 fbdev=1` to preserve a GPU console. `gpu-support` does not unbind or reload either NVIDIA driver.

## Health and recovery

After boot, `nvidia-smi` must initialise every PCI address assigned to NVIDIA. A failed bind or health check records the reason and quarantines that driver version for this OS release in `state.bad_versions`. The host automatically reboots to release DRM consoles safely and selects Nouveau on the next boot. An installation with no GPU reports `no NVIDIA GPU` and stays on Nouveau without a reboot loop. `state.reason` is available to management clients. A quarantined driver is not retried automatically on the same OS release.

The app checks its installed version metadata against `modinfo -F version` and requires every firmware file reported by the built module. Both boot-time and daemon-triggered extension refreshes verify the signed image and compare its declared kernel to the running kernel before merging it. A mismatched image is refused.

Modules are loaded by full path in dependency order, with base-kernel dependencies loaded by modprobe. This avoids replacing the base image's `modules.dep` with an extension-specific index. NVIDIA variant names are selected at application load and share one implementation and state slot; an LTS variant is reserved for a later phase.

## Sources and licences

`mkosi.images/nvidia/version` is the single driver version source. NVIDIA's official redistributable archives contain the complete open module source, firmware and userspace for that version. SHA256 pins in `artifacts.json` cover those archives and the official NVIDIA repository packages for the toolkit; no generated GitHub tag archive or executable installer is used. The modules are built directly without DKMS or a temporary signing key.

The open modules are distributed under their permissive MIT or BSD licence option; the NVIDIA userspace and firmware have NVIDIA's redistribution licence. The Container Toolkit and the pinned `libnvidia-container` packages are Apache-2.0; bundled BSD and MIT components have their own notices. The application ships the NVIDIA licence and open module COPYING under `/usr/share/licenses/nvidia`, and the container packages' copyright and licence notices under `/usr/share/doc/`. Review those files when redistributing an image.
