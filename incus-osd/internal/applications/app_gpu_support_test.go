package applications

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestGPUSupportStart(t *testing.T) {
	t.Parallel()

	firmware := []string{
		"nvidia/620.70.06/gsp_tu10x.bin",
		"nvidia/620.70.06/gsp_ga10x.bin",
		"nvidia/620.70.06/ucodes_tu10x.bin",
		"nvidia/620.70.06/ucodes_ga10x.bin",
	}

	tests := []struct {
		name        string
		device      string
		driver      string
		files       []string
		blacklist   []string
		modinfoFail bool
		loadFail    bool
		mixed       bool
		wantNVIDIA  bool
	}{
		{name: "missing firmware keeps nouveau", device: "0x1e04"},
		{name: "GSP alone is insufficient", device: "0x1e04", files: firmware[:2]},
		{name: "all matching firmware enables Turing", device: "0x1e04", files: firmware, wantNVIDIA: true},
		{name: "Pascal stays on nouveau", device: "0x1b80", files: firmware},
		{name: "mixed generations keep separate drivers", device: "0x1e04", files: firmware, mixed: true, wantNVIDIA: true},
		{name: "vfio keeps its device", device: "0x1e04", driver: "vfio-pci", files: firmware},
		{name: "blacklisted nvidia stays dormant", device: "0x1e04", files: firmware, blacklist: []string{"nvidia"}},
		{name: "blacklisted nouveau is not reloaded", device: "0x1e04", files: firmware, blacklist: []string{"nouveau"}},
		{name: "blacklist normalizes hyphens", device: "0x1e04", blacklist: []string{"intel-vpu"}},
		{name: "unavailable module keeps nouveau", device: "0x1e04", files: firmware, modinfoFail: true},
		{name: "other firmware release is insufficient", device: "0x1e04", files: []string{"nvidia/619.00.00/gsp_tu10x.bin"}},
		{name: "failed load restores nouveau", device: "0x1e04", files: firmware, loadFail: true, wantNVIDIA: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()

			writeGPUFixtureFile(t, root, "usr/share/nvidia-open-kernel-modules/supported-gpus", "1e04\n2206\n")

			for _, file := range tt.files {
				writeGPUFixtureFile(t, root, filepath.Join("usr/lib/firmware", file), "firmware")
			}

			for _, module := range []string{"nouveau", "intel_vpu"} {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "sys/module", module), 0o755))
				writeGPUFixtureFile(t, root, filepath.Join("sys/bus/pci/drivers", module, "unbind"), "")
			}

			writeGPUFixtureFile(t, root, "sys/bus/pci/drivers/nouveau/bind", "")
			writeGPUFixtureFile(t, root, "sys/bus/pci/drivers/nvidia/bind", "")

			driver := tt.driver
			if driver == "" {
				driver = "nouveau"
			}

			addGPUFixtureDevice(t, root, "0000:01:00.0", tt.device, driver)

			if tt.mixed {
				addGPUFixtureDevice(t, root, "0000:02:00.0", "0x1b80", "nouveau")
			}

			s := &state.State{}
			s.System.Kernel.Config.BlacklistModules = tt.blacklist

			var loaded []string

			g := &gpuSupport{common: common{state: s}, sysroot: root}
			g.runCommand = func(_ context.Context, command string, args ...string) (string, error) {
				if command == "/sbin/modinfo" {
					require.Equal(t, []string{"-F", "firmware", "nvidia"}, args)

					if tt.modinfoFail {
						return "", errors.New("module not found")
					}

					return strings.Join(firmware, "\n"), nil
				}

				if command == "/sbin/modprobe" {
					require.Contains(t, args, "--use-blacklist")
					module := args[len(args)-1]

					loaded = append(loaded, module)
					if module == "nvidia" {
						require.Equal(t, []string{"--use-blacklist", "--config", filepath.Join(root, "/etc/modprobe.d"), "nvidia"}, args)

						if tt.loadFail {
							return "", errors.New("module initialization failed")
						}

						link := filepath.Join(root, "sys/bus/pci/devices/0000:01:00.0/driver")
						require.NoError(t, os.Remove(link))
						require.NoError(t, os.Symlink(filepath.Join(root, "sys/bus/pci/drivers/nvidia"), link))
					}
				}

				return "", nil
			}

			require.NoError(t, g.Start(t.Context()))

			if tt.wantNVIDIA {
				require.Contains(t, loaded, "nvidia")
				require.Contains(t, loaded, "vfio-pci")
				require.Less(t, slices.Index(loaded, "vfio-pci"), slices.Index(loaded, "nvidia"))

				unbind, err := os.ReadFile(filepath.Join(root, "sys/bus/pci/drivers/nouveau/unbind"))
				require.NoError(t, err)
				require.Equal(t, "0000:01:00.0", string(unbind))
			} else {
				require.NotContains(t, loaded, "nvidia")
			}

			for _, module := range gpuSupportModules(tt.blacklist) {
				if module == "nouveau" || module == "intel_vpu" {
					require.Contains(t, loaded, module)
				}
			}

			for _, module := range tt.blacklist {
				require.NotContains(t, loaded, strings.ReplaceAll(module, "-", "_"))
			}

			if tt.mixed {
				legacy, err := os.Readlink(filepath.Join(root, "sys/bus/pci/devices/0000:02:00.0/driver"))
				require.NoError(t, err)
				require.Equal(t, "nouveau", filepath.Base(legacy))
			}

			if tt.loadFail {
				bound, err := os.ReadFile(filepath.Join(root, "sys/bus/pci/drivers/nouveau/bind"))
				require.NoError(t, err)
				require.Equal(t, "0000:01:00.0", string(bound))
			}
		})
	}
}

func writeGPUFixtureFile(t *testing.T, root string, path string, value string) {
	t.Helper()

	path = filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
}

func addGPUFixtureDevice(t *testing.T, root string, address string, id string, driver string) {
	t.Helper()

	path := filepath.Join("sys/bus/pci/devices", address)
	writeGPUFixtureFile(t, root, filepath.Join(path, "vendor"), "0x10de\n")
	writeGPUFixtureFile(t, root, filepath.Join(path, "class"), "0x030000\n")
	writeGPUFixtureFile(t, root, filepath.Join(path, "device"), id+"\n")
	require.NoError(t, os.Symlink(filepath.Join(root, "sys/bus/pci/drivers", driver), filepath.Join(root, path, "driver")))
}
