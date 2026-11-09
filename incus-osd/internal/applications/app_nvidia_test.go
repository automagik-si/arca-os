package applications

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

type nvidiaBootTest struct {
	name              string
	choice            string
	kernel            string
	cardUse           string
	bound             string
	passthrough       bool
	bindFail          bool
	healthFail        bool
	noGPU             bool
	firmwareFail      bool
	versionFail       bool
	changeBeforeStart bool
	partialHealth     bool
	badVersion        bool
	wantCards         int
	wantReason        string
}

func TestNVIDIABootAndHealth(t *testing.T) {
	t.Parallel()

	tests := []nvidiaBootTest{
		{name: "two identical cards including compute class", wantCards: 2},
		{name: "new choice waits for reboot", changeBeforeStart: true, wantCards: 2},
		{name: "one identical card assigned to vfio", cardUse: "vfio-pci", wantCards: 1},
		{name: "one identical card disabled", cardUse: "off", wantCards: 1},
		{name: "configured passthrough skipped", passthrough: true, wantCards: 1},
		{name: "bound vfio skipped", bound: "vfio-pci", wantCards: 1},
		{name: "never unbind nouveau at runtime", bound: "nouveau", wantCards: 1},
		{name: "explicit off leaves drivers alone", choice: "off"},
		{name: "missing firmware", firmwareFail: true, wantReason: "firmware is missing"},
		{name: "mismatched module version", versionFail: true, wantReason: "does not match its userspace"},
		{name: "only one GPU initialises", partialHealth: true, wantCards: 2, wantReason: "health check failed for 0000:02:00.0"},
		{name: "kernel mismatch", kernel: "different-kernel", wantReason: "the NVIDIA driver for this Arca OS version is not installed yet"},
		{name: "bind fails after module load", bindFail: true, wantCards: 2, wantReason: "failed to bind"},
		{name: "health failure reverts", healthFail: true, wantCards: 2, wantReason: "health check"},
		{name: "no GPU falls back cleanly", noGPU: true, wantReason: "no NVIDIA GPU"},
		{name: "failed driver stays quarantined", badVersion: true, wantReason: "previous failure"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			n, commands, version := newNVIDIAFixture(t, tt)
			root := n.sysroot
			s := n.state

			require.NoError(t, n.prepare(t.Context(), true))

			body, err := os.ReadFile(filepath.Join(root, nvidiaBootFile))
			require.NoError(t, err)

			boot := nvidiaBoot{}
			require.NoError(t, json.Unmarshal(body, &boot))
			require.Len(t, boot.Cards, tt.wantCards)

			if tt.changeBeforeStart {
				s.Applications.NVIDIA.Config.Driver = "off"
			}

			require.NoError(t, n.Start(t.Context()))

			if tt.wantReason != "" {
				require.Contains(t, s.Applications.NVIDIA.State.Reason, tt.wantReason)

				if tt.bindFail || tt.healthFail || tt.partialHealth || tt.badVersion {
					require.Contains(t, s.Applications.NVIDIA.State.BadVersions, "test-release:"+version)
				} else {
					require.Empty(t, s.Applications.NVIDIA.State.BadVersions)
				}

				if tt.wantCards > 0 {
					require.Contains(t, *commands, "systemctl --no-block reboot")
				}
			} else if tt.wantCards > 0 {
				require.Equal(t, "nvidia", s.Applications.NVIDIA.State.ActiveDriver)
			}

			if tt.changeBeforeStart {
				require.True(t, s.System.Update.State.NeedsReboot)
			}

			for _, driver := range []string{"nouveau", "vfio-pci"} {
				for _, action := range []string{"bind", "unbind"} {
					value, err := os.ReadFile(filepath.Join(root, "sys/bus/pci/drivers", driver, action))
					require.NoError(t, err)
					require.Equal(t, "untouched", string(value))
				}
			}

			if tt.wantCards == 0 {
				require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")
				require.NotContains(t, *commands, "/sbin/modprobe vfio-pci")
			}
		})
	}
}

func nvidiaTestCommand(t *testing.T, root string, tt nvidiaBootTest, firmware []byte, version string, addresses []string, commands *[]string) func(context.Context, string, ...string) (string, error) {
	t.Helper()

	return func(_ context.Context, command string, args ...string) (string, error) {
		*commands = append(*commands, command+" "+strings.Join(args, " "))
		switch command {
		case "uname":
			if tt.kernel != "" {
				return tt.kernel, nil
			}

			return "test-kernel", nil
		case "/sbin/modinfo":
			switch args[1] {
			case "version":
				if tt.versionFail {
					return "wrong-version", nil
				}

				return version, nil
			case "firmware":
				return string(firmware), nil
			case "depends":
				return "drm", nil
			default:
				t.Fatalf("unexpected modinfo field: %v", args)
			}
		case "/sbin/insmod":
			if !tt.bindFail && strings.HasSuffix(args[0], "/nvidia.ko") {
				for _, address := range addresses {
					path := filepath.Join(root, "sys/bus/pci/devices", address)
					use, err := os.ReadFile(filepath.Join(path, "driver_override"))
					require.NoError(t, err)

					if string(use) == "nvidia" {
						require.NoError(t, os.Symlink(filepath.Join(root, "sys/bus/pci/drivers/nvidia"), filepath.Join(path, "driver")))
					}
				}
			}
		case "systemd-dissect":
			// The fixture packs ship the same metadata as the merged driver.
			if len(args) == 3 && args[0] == "--copy-from" && strings.HasPrefix(args[2], "/usr/share/nvidia/") {
				body, err := os.ReadFile(filepath.Join(root, args[2]))

				return string(body), err
			}
		case "/usr/bin/nvidia-smi":
			if tt.healthFail {
				return "", errors.New("GSP failed")
			}

			if tt.partialHealth {
				return "00000000:01:00.0\n", nil
			}

			return "00000000:01:00.0\n00000000:02:00.0\n", nil
		default:
			// The remaining commands only change kernel state in a real boot.
		}

		return "", nil
	}
}

func newNVIDIAFixture(t *testing.T, tt nvidiaBootTest) (*nvidia, *[]string, string) {
	t.Helper()
	root := t.TempDir()
	writeGPUFixtureFile(t, root, "proc/sys/kernel/random/boot_id", "boot-one")
	s, err := state.LoadOrCreate(filepath.Join(root, "state.txt"))
	require.NoError(t, err)

	s.OS.RunningRelease = "test-release"

	s.Applications.NVIDIA.Config.Driver = "nvidia"
	if tt.choice != "" {
		s.Applications.NVIDIA.Config.Driver = tt.choice
	}

	firmware, err := os.ReadFile("testdata/nvidia-firmware.txt")
	require.NoError(t, err)

	version := strings.Split(strings.Fields(string(firmware))[0], "/")[1]
	writeGPUFixtureFile(t, root, "usr/share/nvidia/version", version)
	writeGPUFixtureFile(t, root, "usr/share/nvidia/kernel", "test-kernel")
	writeGPUFixtureFile(t, root, "usr/share/nvidia/supported-gpus", "1e04\n")

	for name := range strings.FieldsSeq(string(firmware)) {
		writeGPUFixtureFile(t, root, "usr/lib/firmware/"+name, "firmware")
	}

	if tt.firmwareFail {
		require.NoError(t, os.Remove(filepath.Join(root, "usr/lib/firmware", strings.Fields(string(firmware))[0])))
	}

	if tt.badVersion {
		s.Applications.NVIDIA.State.BadVersions = map[string]string{"test-release:" + version: "previous failure"}
	}

	for _, driver := range []string{"nvidia", "nouveau", "vfio-pci"} {
		for _, action := range []string{"bind", "unbind"} {
			writeGPUFixtureFile(t, root, "sys/bus/pci/drivers/"+driver+"/"+action, "untouched")
		}
	}

	addresses := []string{"0000:01:00.0", "0000:02:00.0"}

	require.NoError(t, os.MkdirAll(filepath.Join(root, "sys/bus/pci/devices"), 0o755))

	if !tt.noGPU {
		for i, address := range addresses {
			base := "sys/bus/pci/devices/" + address
			writeGPUFixtureFile(t, root, base+"/vendor", "0x10de")
			writeGPUFixtureFile(t, root, base+"/device", "0x1e04")

			class := "0x030000"
			if i == 1 {
				class = "0x030200"
			}

			writeGPUFixtureFile(t, root, base+"/class", class)
			writeGPUFixtureFile(t, root, base+"/driver_override", "untouched")
		}
	}

	if tt.cardUse != "" {
		s.Applications.NVIDIA.Config.Cards = map[string]string{addresses[1]: tt.cardUse}
	}

	if tt.passthrough {
		s.System.Kernel.Config.PCI = &api.SystemKernelConfigPCI{Passthrough: []api.SystemKernelConfigPCIPassthrough{{VendorID: "10de", ProductID: "1e04", PCIAddress: addresses[1]}}}
	}

	if tt.bound != "" {
		require.NoError(t, os.Symlink(filepath.Join(root, "sys/bus/pci/drivers", tt.bound), filepath.Join(root, "sys/bus/pci/devices", addresses[1], "driver")))
	}

	var commands []string

	n := &nvidia{common: common{state: s, appState: &s.Applications.NVIDIA.State.ApplicationState}, variant: nvidiaStable, sysroot: root}
	n.verifyModule = func(string) error { return nil }
	n.runCommand = nvidiaTestCommand(t, root, tt, firmware, version, addresses, &commands)

	return n, &commands, version
}
