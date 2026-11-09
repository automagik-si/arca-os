package applications

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
)

type gpuSupport struct {
	common

	sysroot    string
	runCommand func(context.Context, string, ...string) (string, error)
}

func (g *gpuSupport) Get(_ context.Context) (any, error) {
	return g.state.Applications.GPUSupport, nil
}

// IsInstalled reports whether the application has been installed.
func (g *gpuSupport) IsInstalled() bool {
	return isInstalled(g.Name(), g.appState.Version)
}

func (*gpuSupport) IsRunning(_ context.Context) bool {
	return true
}

func (*gpuSupport) Name() string {
	return "gpu-support"
}

// SetFriendlyVersion records the friendly version.
func (g *gpuSupport) SetFriendlyVersion(_ context.Context) error {
	version, err := os.ReadFile("/usr/lib/firmware/linux-firmware-gpu-version")
	if err != nil {
		return err
	}

	g.appState.FriendlyVersion = string(version) + " [" + g.appState.Version + "]"

	return nil
}

func (g *gpuSupport) Start(ctx context.Context) error {
	modules := gpuSupportModules(g.state.System.Kernel.Config.BlacklistModules)

	// Reload the modules if loaded.
	for _, module := range modules {
		// Check if loaded.
		_, err := os.Stat(filepath.Join(g.sysroot, "/sys/module", module))
		if err != nil {
			continue
		}

		// Unbind the devices.
		entries, err := os.ReadDir(filepath.Join(g.sysroot, "/sys/bus/pci/drivers", module))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("Failed to check kernel module", "module", module)

			continue
		}

		for _, entry := range entries {
			address := filepath.Base(entry.Name())

			if !strings.Contains(address, ":") || !strings.Contains(address, ".") {
				continue
			}

			err := os.WriteFile(filepath.Join(g.sysroot, "/sys/bus/pci/drivers", module, "unbind"), []byte(address), 0o600)
			if err != nil {
				slog.Warn("Failed to unbind device", "module", module, "address", address)

				continue
			}
		}

		// Unload the module.
		_, err = g.command(ctx, "/sbin/rmmod", module)
		if err != nil {
			slog.Warn("Failed to unload kernel module", "module", module)

			continue
		}

		// Load the module back.
		_, err = g.command(ctx, "/sbin/modprobe", "--use-blacklist", module)
		if err != nil {
			slog.Warn("Failed to load kernel module", "module", module)

			continue
		}
	}

	g.startNVIDIA(ctx)

	return nil
}

func gpuSupportModules(blacklist []string) []string {
	modules := []string{"amdgpu", "i915", "intel_vpu", "nouveau", "xe"}

	return slices.DeleteFunc(modules, func(module string) bool {
		return gpuModuleBlacklisted(module, blacklist)
	})
}

func gpuModuleBlacklisted(module string, blacklist []string) bool {
	return slices.ContainsFunc(blacklist, func(blocked string) bool {
		return strings.ReplaceAll(blocked, "-", "_") == module
	})
}

func (g *gpuSupport) command(ctx context.Context, command string, args ...string) (string, error) {
	if g.runCommand != nil {
		return g.runCommand(ctx, command, args...)
	}

	return subprocess.RunCommandContext(ctx, command, args...)
}

func (g *gpuSupport) nvidiaFirmwareReady(ctx context.Context) bool {
	firmware, err := g.command(ctx, "/sbin/modinfo", "-F", "firmware", "nvidia")
	if err != nil || strings.TrimSpace(firmware) == "" {
		return false
	}

	for name := range strings.FieldsSeq(firmware) {
		parts := strings.Split(name, "/")
		if len(parts) != 3 || parts[0] != "nvidia" || parts[1] != "620.70.06" || parts[2] == "" {
			return false
		}

		info, err := os.Stat(filepath.Join(g.sysroot, "/usr/lib/firmware", name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}

	return true
}

func (g *gpuSupport) startNVIDIA(ctx context.Context) {
	blacklist := g.state.System.Kernel.Config.BlacklistModules
	if gpuModuleBlacklisted("nvidia", blacklist) || !g.nvidiaFirmwareReady(ctx) {
		return
	}

	// The pinned source's compatibility table lists only Turing and newer GPUs.
	supported, err := os.ReadFile(filepath.Join(g.sysroot, "/usr/share/nvidia-open-kernel-modules/supported-gpus"))
	if err != nil {
		return
	}

	err = os.MkdirAll(filepath.Join(g.sysroot, "/etc/modprobe.d"), 0o755)
	if err != nil {
		return
	}

	// Load passthrough support before releasing any GPU from its current driver.
	_, err = g.command(ctx, "/sbin/modprobe", "--use-blacklist", "vfio-pci")
	if err != nil {
		return
	}

	devices, err := os.ReadDir(filepath.Join(g.sysroot, "/sys/bus/pci/devices"))
	if err != nil {
		return
	}

	for _, device := range devices {
		path := filepath.Join(g.sysroot, "/sys/bus/pci/devices", device.Name())
		vendor, vendorErr := os.ReadFile(filepath.Join(path, "vendor"))
		class, classErr := os.ReadFile(filepath.Join(path, "class"))

		id, idErr := os.ReadFile(filepath.Join(path, "device"))
		if vendorErr != nil || classErr != nil || idErr != nil || strings.TrimSpace(string(vendor)) != "0x10de" {
			continue
		}

		if !slices.Contains([]string{"0x030000", "0x030200"}, strings.TrimSpace(string(class))) {
			continue
		}

		if !slices.Contains(strings.Fields(string(supported)), strings.TrimPrefix(strings.TrimSpace(string(id)), "0x")) {
			continue
		}

		driver, err := os.Readlink(filepath.Join(path, "driver"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}

		driver = filepath.Base(driver)
		if driver != "nouveau" && driver != "." {
			continue
		}

		if gpuModuleBlacklisted(driver, blacklist) {
			continue
		}

		g.bindNVIDIA(ctx, device.Name(), driver == "nouveau")
	}
}

func (g *gpuSupport) bindNVIDIA(ctx context.Context, address string, nouveauBound bool) {
	drivers := filepath.Join(g.sysroot, "/sys/bus/pci/drivers")
	if nouveauBound {
		err := os.WriteFile(filepath.Join(drivers, "nouveau/unbind"), []byte(address), 0o600)
		if err != nil {
			slog.Warn("Failed to unbind GPU", "address", address, "error", err)

			return
		}
	}

	// Use operator configuration without the factory NVIDIA autoload blacklist.
	_, err := g.command(ctx, "/sbin/modprobe", "--use-blacklist", "--config", filepath.Join(g.sysroot, "/etc/modprobe.d"), "nvidia")
	if err == nil {
		driver, _ := os.Readlink(filepath.Join(g.sysroot, "/sys/bus/pci/devices", address, "driver"))
		if filepath.Base(driver) == "nvidia" {
			return
		}

		err = os.WriteFile(filepath.Join(drivers, "nvidia/bind"), []byte(address), 0o600)
		if err == nil {
			return
		}
	}

	slog.Warn("Failed to activate NVIDIA GPU", "address", address, "error", err)

	if nouveauBound {
		err = os.WriteFile(filepath.Join(drivers, "nouveau/bind"), []byte(address), 0o600)
		if err != nil {
			slog.Warn("Failed to restore Nouveau GPU", "address", address, "error", err)
		}
	}
}
