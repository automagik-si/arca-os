package kernel

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/units"

	"github.com/lxc/incus-os/incus-osd/api"
)

// ValidateKernelConfiguration checks input without applying any host changes.
func ValidateKernelConfiguration(config api.SystemKernelConfig) error {
	name := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	address := regexp.MustCompile(`^[[:xdigit:]]{4}:[[:xdigit:]]{2}:[[:xdigit:]]{2}\.[0-7]$`)
	id := regexp.MustCompile(`^[[:xdigit:]]{4}$`)

	for _, module := range config.BlacklistModules {
		if !name.MatchString(module) {
			return fmt.Errorf("invalid module name %q", module)
		}
	}

	if config.Memory != nil {
		if config.Memory.PersistentHugepages < 0 {
			return errors.New("persistent hugepages count cannot be negative")
		}

		err := validateSize(config.Memory.ZramSwapSize)
		if err != nil {
			return fmt.Errorf("invalid zram swap size: %w", err)
		}
	}

	if config.ZFS != nil {
		err := validateSize(config.ZFS.ARCMaxSize)
		if err != nil {
			return fmt.Errorf("invalid ZFS ARC max size: %w", err)
		}
	}

	if config.Network != nil {
		c := config.Network
		if c.BufferSize < 0 || c.NetdevMaxBacklog < 0 {
			return errors.New("network buffer size and backlog cannot be negative")
		}

		for _, value := range []string{c.QueuingDiscipline, c.TCPCongestionAlgorithm} {
			if value != "" && !name.MatchString(value) {
				return errors.New("invalid network algorithm name")
			}
		}

		available, err := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control")
		if err == nil && c.TCPCongestionAlgorithm != "" && !slices.Contains(strings.Fields(string(available)), c.TCPCongestionAlgorithm) {
			return errors.New("unsupported TCP congestion algorithm")
		}
	}

	if config.CPU != nil && config.CPU.ScalingGovernor != "" {
		governor := config.CPU.ScalingGovernor
		if !name.MatchString(governor) {
			return errors.New("invalid CPU scaling governor")
		}

		available, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors")
		if err == nil && !slices.Contains(strings.Fields(string(available)), governor) {
			return errors.New("unsupported CPU scaling governor")
		}
	}

	if config.PCI != nil {
		for _, c := range config.PCI.Passthrough {
			if !id.MatchString(c.VendorID) || !id.MatchString(c.ProductID) {
				return errors.New("invalid PCI vendor or product ID")
			}

			if c.PCIAddress != "" && !address.MatchString(c.PCIAddress) {
				return errors.New("invalid PCI passthrough address")
			}
		}

		for _, c := range config.PCI.SRIOV {
			if !address.MatchString(c.PCIAddress) || c.VFCount < 0 {
				return errors.New("invalid SR-IOV address or virtual function count")
			}
		}
	}

	for _, c := range config.Console {
		if !name.MatchString(c.Device) || c.BaudRate < 0 {
			return errors.New("invalid console device or baud rate")
		}
	}

	return nil
}

func validateSize(value string) error {
	if value == "" {
		return nil
	}

	size, err := units.ParseByteSizeString(value)
	if err != nil {
		return err
	}

	if size < 0 {
		return errors.New("size cannot be negative")
	}

	return nil
}
