package applications

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func readNVIDIABoot(t *testing.T, n *nvidia) nvidiaBoot {
	t.Helper()

	body, err := os.ReadFile(n.path(nvidiaBootFile))
	require.NoError(t, err)

	boot := nvidiaBoot{}
	require.NoError(t, json.Unmarshal(body, &boot))

	return boot
}

func TestNVIDIANormalFallbackDoesNotBlockVersion(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"no GPUs", "vfio-pci", "off", "blacklist", "signature"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{noGPU: mode == "no GPUs"})
			switch mode {
			case "vfio-pci", "off":
				n.state.Applications.NVIDIA.Config.Cards = map[string]string{"0000:01:00.0": mode, "0000:02:00.0": mode}
			case "blacklist":
				n.state.System.Kernel.Config.BlacklistModules = []string{"nvidia"}
			case "signature":
				n.verifyModule = func(string) error { return errors.New("wrong OS key") }
			default:
			}

			require.NoError(t, n.prepare(t.Context(), true))
			require.NoError(t, n.Start(t.Context()))
			require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
			require.Empty(t, n.state.Applications.NVIDIA.State.PendingAttempt)
			require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")
			require.NotContains(t, *commands, "systemctl --no-block reboot")
			require.NotEmpty(t, readNVIDIABoot(t, n).Reason)
		})
	}
}

func TestNVIDIAPanicBeforeAttemptCannotPoisonVersion(t *testing.T) {
	t.Parallel()
	n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	n.verifyModule = func(string) error { panic("preflight interrupted") }

	require.Panics(t, func() { _ = n.prepare(t.Context(), true) })
	require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")
	require.Contains(t, *commands, "/sbin/modprobe nouveau")

	persisted, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.Empty(t, persisted.Applications.NVIDIA.State.PendingAttempt)
	require.Empty(t, persisted.Applications.NVIDIA.State.BadVersions)
	require.Equal(t, "NVIDIA startup was interrupted", readNVIDIABoot(t, n).Reason)
}

func TestNVIDIAAttemptSurvivesInterruptedLoad(t *testing.T) {
	t.Parallel()
	n, _, version := newNVIDIAFixture(t, nvidiaBootTest{})
	original := n.runCommand
	verified := 0
	n.verifyModule = func(string) error {
		verified++

		return nil
	}
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/sbin/insmod" {
			require.Equal(t, 5, verified)

			persisted, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.Equal(t, "test-release:"+version, persisted.Applications.NVIDIA.State.PendingAttempt)
			panic("interrupted module startup")
		}

		return original(ctx, name, args...)
	}

	require.Panics(t, func() { _ = n.prepare(t.Context(), true) })

	loaded, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)

	n.state = loaded
	n.appState = &loaded.Applications.NVIDIA.State.ApplicationState
	writeGPUFixtureFile(t, n.sysroot, "proc/sys/kernel/random/boot_id", "boot-two")
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		require.NotEqual(t, "/sbin/insmod", name)

		return original(ctx, name, args...)
	}
	require.NoError(t, n.prepare(t.Context(), true))
	require.Contains(t, loaded.Applications.NVIDIA.State.BadVersions["test-release:"+version], "previous startup")
	require.Empty(t, loaded.Applications.NVIDIA.State.PendingAttempt)
	require.Contains(t, readNVIDIABoot(t, n).Reason, "previous startup")
}

func TestNVIDIAWillNotLoadOrRebootWithoutDurableState(t *testing.T) {
	t.Parallel()
	t.Run("save refused before insmod", func(t *testing.T) {
		t.Parallel()
		n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
		n.state.UnrecognizedFields = []string{"future.field"}
		require.NoError(t, n.prepare(t.Context(), true))
		require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")
		require.Contains(t, readNVIDIABoot(t, n).Reason, "recorded safely")
		require.Empty(t, n.state.Applications.NVIDIA.State.PendingAttempt)

		persisted, err := state.LoadOrCreate(n.path("/state.txt"))
		require.NoError(t, err)
		require.Empty(t, persisted.Applications.NVIDIA.State.PendingAttempt)
	})
	t.Run("failed quarantine save prevents reboot", func(t *testing.T) {
		t.Parallel()
		n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{healthFail: true})
		require.NoError(t, n.prepare(t.Context(), true))
		original := n.runCommand
		n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
			if name == "/usr/bin/nvidia-smi" {
				n.state.UnrecognizedFields = []string{"future.field"}
			}

			return original(ctx, name, args...)
		}
		require.ErrorContains(t, n.Start(t.Context()), "could not be saved")
		require.NotContains(t, *commands, "systemctl --no-block reboot")
		require.Empty(t, n.state.Applications.NVIDIA.State.FallbackBootID)
	})
	t.Run("repeated health failures reboot once", func(t *testing.T) {
		t.Parallel()
		n, commands, version := newNVIDIAFixture(t, nvidiaBootTest{healthFail: true})
		require.NoError(t, n.prepare(t.Context(), true))

		for range 3 {
			require.NoError(t, n.Start(t.Context()))
		}

		require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))

		loaded, err := state.LoadOrCreate(n.path("/state.txt"))
		require.NoError(t, err)
		require.NotEmpty(t, loaded.Applications.NVIDIA.State.BadVersions["test-release:"+version])
		require.Equal(t, "boot-one", loaded.Applications.NVIDIA.State.FallbackBootID)
	})
}

func TestNVIDIAUnrelatedErrorsStillPrepareFallback(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"ldconfig", "vfio-pci", "drivers_probe", "driver_override", "PCI read"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{cardUse: "vfio-pci"})
			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if (operation == "ldconfig" && name == "/sbin/ldconfig") || (operation == "vfio-pci" && name == "/sbin/modprobe" && args[0] == "vfio-pci") {
					*commands = append(*commands, name+" "+strings.Join(args, " "))

					return "", errors.New("synthetic failure")
				}

				return original(ctx, name, args...)
			}

			switch operation {
			case "drivers_probe":
				require.NoError(t, os.Mkdir(n.path("/sys/bus/pci/drivers_probe"), 0o700))
			case "driver_override":
				path := n.path("/sys/bus/pci/devices/0000:02:00.0/driver_override")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Mkdir(path, 0o700))
			case "PCI read":
				require.NoError(t, os.Remove(n.path("/sys/bus/pci/devices/0000:02:00.0/vendor")))
			default:
			}

			require.NoError(t, n.prepare(t.Context(), true))
			boot := readNVIDIABoot(t, n)
			require.NotEmpty(t, boot.Reason)
			require.NotContains(t, boot.Reason, "synthetic failure")
			require.Contains(t, *commands, "/sbin/modprobe nouveau")
			require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")

			use, err := os.ReadFile(n.path("/sys/bus/pci/devices/0000:01:00.0/driver_override"))
			require.NoError(t, err)
			require.Equal(t, "nouveau", string(use))
			require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
		})
	}
}

func TestNVIDIAResetAndAddressNormalization(t *testing.T) {
	t.Parallel()
	n, _, version := newNVIDIAFixture(t, nvidiaBootTest{})
	n.state.Applications.NVIDIA.State.BadVersions = map[string]string{"test-release:" + version: "failed", "older:version": "failed"}
	n.state.Applications.NVIDIA.State.DriverVersion = version
	n.appState.Version = "next-build"
	n.state.Applications.NVIDIA.State.ActiveExtensionVersion = "current-build"
	require.NoError(t, n.Update(t.Context()))
	require.Contains(t, n.state.Applications.NVIDIA.State.BadVersions, "test-release:"+version)
	n.state.Applications.NVIDIA.State.ActiveExtensionVersion = "next-build"
	require.NoError(t, n.Update(t.Context()))
	require.NotContains(t, n.state.Applications.NVIDIA.State.BadVersions, "test-release:"+version)
	require.Contains(t, n.state.Applications.NVIDIA.State.BadVersions, "older:version")

	req := &api.ApplicationNVIDIA{Config: api.ApplicationNVIDIAConfig{Driver: "off", ResetBadVersions: true, Cards: map[string]string{"0000:AA:00.0": "off"}}}
	require.NoError(t, n.UpdateConfig(t.Context(), req))
	require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
	require.Contains(t, n.state.Applications.NVIDIA.Config.Cards, "0000:aa:00.0")
	require.False(t, n.state.Applications.NVIDIA.Config.ResetBadVersions)
	require.Equal(t, "0000:aa:00.0", normaliseNVIDIAAddress("00000000:AA:00.0"))
}
