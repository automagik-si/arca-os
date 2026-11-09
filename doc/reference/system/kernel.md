# Kernel

IncusOS exposes a limited set of configuration knobs for adjusting kernel-level settings. A system reboot may be required for changes to fully take effect.

`PUT /1.0/system/kernel` saves the requested configuration even when some live changes fail. In that case it returns HTTP 200 with the combined apply errors in `metadata.warning`; the remaining kernel settings are still applied. A persistence failure returns an error.

## Configuration options

Configuration fields are defined in the [`SystemKernelConfig` struct](https://github.com/lxc/incus-os/blob/main/incus-osd/api/system_kernel.go).

The following configuration options can be set:

* `blacklist_modules`: A list of one or more kernel modules to blacklist. Typically useful when passing through PCI devices to virtual machines.

* `cpu`: Change CPU-specific settings.
   * `scaling_governor`: Optional; the CPU frequency scaling governor to use, must be one of the governors supported by the kernel.
   * `disable_thermal_protection`: Optional; when true, the kernel stops acting on thermal zone trip points and thermal management is left to the firmware. Useful on boards such as the Raspberry Pi 4 where the firmware already throttles the processor but the critical trip point it exposes to the kernel triggers a shutdown.

* `memory`: Change `sysctl` values or other settings that impact the system's memory configuration
   * `persistent_hugepages`: Optional; number of persistent hugepages to allocate.
   * `zram_swap_size`: Optional; a human-readable string such as "4GiB" that defines the size of the zram-backed swap device to create. A value of "0" will disable any existing zram-backed swap that may exist. IncusOS will attempt to immediately apply this change without requiring a reboot.

* `network`: Change `sysctl` values that impact the system's network configuration.
   * `buffer_size`: Optional; configure the maximum buffer size used when setting the `net.ipv4.tcp_rmem`, `net.ipv4.tcp_wmem`, `net.core.rmem_max`, and `net.core.wmem_max` `sysctl` fields.

   * `queuing_discipline`: Optional; configure the value of the `net.core.default_qdisc` `sysctl` field.

   * `tcp_congestion_algorithm`: Optional; configure the TCP congestion algorithm used by the system, defaults to `bbr`.

* `pci`: Change PCI device configuration.
   * `passthrough`: Configure one or more PCI devices for pass-through to a virtual machine:
      * `vendor_id`: The PCI vendor ID
      * `product_id`: The PCI product ID
      * `pci_address`: Optional; if specified the system will attempt to unbind the given PCI device from its existing driver and configure it for passing though to a virtual machine without requiring a reboot.

Invalid kernel configuration returns HTTP 400 before any changes are applied or saved. Valid configuration is persisted even if applying it live fails; those apply errors are returned as warnings.
