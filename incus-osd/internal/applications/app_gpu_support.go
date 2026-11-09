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

	return nil
}

func gpuSupportModules(blacklist []string) []string {
	modules := []string{"amdgpu", "i915", "intel_vpu", "xe"}

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
