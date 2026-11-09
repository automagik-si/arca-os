package applications

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func switchFixture(t *testing.T, branch string) (*nvidia, *[]string, *bool) {
	t.Helper()
	n, commands, version := newNVIDIAFixture(t, nvidiaBootTest{})
	n.appConfig = &n.state.Applications.NVIDIA.Config.ApplicationConfig
	n.appState.Initialized = true
	n.state.OS.SystemIsReady = true
	s := &n.state.Applications.NVIDIA.State
	s.RequestedBranch, s.ActiveBranch, s.ActiveExtensionVersion, s.Version = branch, branch, "001", "001"
	n.variant = branch

	for _, b := range []string{nvidiaStable, nvidiaLTS} {
		for _, v := range []string{"001", "002"} {
			writeGPUFixtureFile(t, n.sysroot, filepath.Join(systemd.LocalExtensionsPath, v, b+".raw"), "signed pack")
		}
	}

	writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia/refcnt", "0")
	n.verifyImage = func(_ context.Context, image string) error {
		_, err := os.Stat(image)

		return err
	}
	original := n.runCommand
	fail := false
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/sbin/rmmod" && args[0] == "nvidia" {
			for _, address := range []string{"0000:01:00.0", "0000:02:00.0"} {
				_ = os.Remove(n.path("/sys/bus/pci/devices/" + address + "/driver"))
			}
		}

		if name == "/usr/bin/nvidia-smi" && fail {
			fail = false

			*commands = append(*commands, name+" failed")

			return "", errors.New("health failed")
		}

		return original(ctx, name, args...)
	}
	s.DriverVersion = version

	return n, commands, &fail
}

func TestNVIDIASwitchLiveAndOfflineRollback(t *testing.T) {
	t.Parallel()

	for _, source := range []string{nvidiaStable, nvidiaLTS} {
		for _, failHealth := range []bool{false, true} {
			t.Run(source+" failure="+strconv.FormatBool(failHealth), func(t *testing.T) {
				t.Parallel()
				n, commands, fail := switchFixture(t, source)

				target := nvidiaStable
				if source == nvidiaStable {
					target = nvidiaLTS
				}

				n.SetVersions("002", []string{"001", "002"})
				n.state.Applications.NVIDIA.Config.PinCurrentVersions = true
				n.state.Applications.NVIDIA.State.RequestedBranch = target
				*fail = failHealth

				require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: target, Version: "002"}))
				s := n.state.Applications.NVIDIA.State
				require.Equal(t, &api.ApplicationNVIDIABuild{Branch: source, Version: "001"}, s.Previous)
				require.True(t, n.state.Applications.NVIDIA.Config.PinCurrentVersions)
				require.False(t, s.NeedsReboot)
				require.Equal(t, target, s.RequestedBranch)
				require.Equal(t, "002", s.Version)

				active := target
				if failHealth {
					active = source

					require.Contains(t, s.Reason, "restored the previous driver from disk")
					require.Len(t, s.BadBuilds, 1)
				}

				require.Equal(t, active, s.ActiveBranch)

				sequence := strings.Join(*commands, "\n")
				require.Less(t, strings.Index(sequence, "systemd-sysext unmerge"), strings.Index(sequence, "/sbin/rmmod nvidia"))
				require.Less(t, strings.Index(sequence, "/sbin/rmmod nvidia"), strings.Index(sequence, "systemd-sysext refresh"))
				require.Less(t, strings.Index(sequence, "systemd-sysext refresh"), strings.Index(sequence, "/sbin/insmod"))
				require.NotContains(t, sequence, "curl")
				require.NotContains(t, sequence, "reboot")
				require.NoError(t, pruneNVIDIAExtensions(n.state, "", n.path(systemd.LocalExtensionsPath)))
				_, err := os.Stat(n.path(filepath.Join(systemd.LocalExtensionsPath, "001", source+".raw")))
				require.NoError(t, err)
			})
		}
	}
}

func TestNVIDIABusySwitchWaitsForBoot(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	require.NoError(t, os.MkdirAll(n.path("/proc/123/fd"), 0o700))
	require.NoError(t, os.Symlink("/dev/nvidia0", n.path("/proc/123/fd/7")))
	s := &n.state.Applications.NVIDIA.State
	s.RequestedBranch, s.Version = nvidiaLTS, "002"
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Equal(t, "001", s.ActiveExtensionVersion)
	require.True(t, s.NeedsReboot)
	require.Contains(t, s.Reason, "in use")
	require.Empty(t, *commands)

	saved, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.Equal(t, nvidiaLTS, saved.Applications.NVIDIA.State.RequestedBranch)
	require.NoError(t, os.RemoveAll(n.path("/proc/123")))
	// Simulate boot after the early selector links the persisted branch and processes have exited.
	require.NoError(t, os.MkdirAll(n.path(systemd.SystemExtensionsPath), 0o700))
	require.NoError(t, os.Symlink(n.path(filepath.Join(systemd.LocalExtensionsPath, "002", "nvidia-lts.raw")), n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia-lts.raw"))))
	n.state.OS.SystemIsReady = false
	n.variant = nvidiaLTS
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, n.Start(t.Context()))
	require.Equal(t, nvidiaLTS, s.ActiveBranch)
	require.False(t, s.NeedsReboot)
}

func TestNVIDIAVersionSwitchRetainsPrevious(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	n.appState.AvailableVersions = []string{"001", "002"}
	require.NoError(t, n.SwitchVersion("002"))
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: n.Version()}))
	require.Equal(t, "001", n.state.Applications.NVIDIA.State.Previous.Version)
	require.Equal(t, "002", n.state.Applications.NVIDIA.State.ActiveExtensionVersion)
}

func TestNVIDIANoSelectionDoesNotPrune(t *testing.T) {
	t.Parallel()

	for _, active := range []string{"", "001"} {
		n, _, _ := switchFixture(t, nvidiaStable)
		n.state.Applications.NVIDIA.State.ActiveExtensionVersion = active
		n.state.Applications.NVIDIA.State.Reason = systemd.NVIDIAMissingReason
		require.NoError(t, pruneNVIDIAExtensions(n.state, "", n.path(systemd.LocalExtensionsPath)))
		_, err := os.Stat(n.path(filepath.Join(systemd.LocalExtensionsPath, "002", "nvidia.raw")))
		require.NoError(t, err)
	}
}

func TestNVIDIABranchesShareOneRequestedSlot(t *testing.T) {
	t.Parallel()

	n, _, _ := switchFixture(t, nvidiaStable)
	for _, branch := range []string{nvidiaLTS, nvidiaStable} {
		app, err := Load(t.Context(), n.state, branch)
		require.NoError(t, err)
		app.SetVersions("001", []string{"001"})
		require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: branch, Version: "001"}))

		for _, name := range []string{nvidiaStable, nvidiaLTS} {
			candidate, err := Load(t.Context(), n.state, name)
			require.NoError(t, err)
			require.Equal(t, name == branch, candidate.IsInstalled())
		}

		require.Equal(t, "001", n.state.Applications.NVIDIA.State.Previous.Version)
	}
}

func TestNVIDIAModuleBusyDoesNotQuarantineRequestedBuild(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/sbin/rmmod" {
			return "", errors.New("module is in use")
		}

		return original(ctx, name, args...)
	}
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	s := n.state.Applications.NVIDIA.State
	require.True(t, s.NeedsReboot)
	require.Empty(t, s.BadBuilds)
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Contains(t, s.Reason, "modules are still in use")
}

func TestNVIDIAFailedLiveRollbackRequestsOneDurableReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/nvidia-smi" {
			return "", errors.New("health unavailable")
		}

		if name == "systemctl" {
			saved, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.Equal(t, "boot-one", saved.Applications.NVIDIA.State.FallbackBootID)
			require.NotEmpty(t, saved.Applications.NVIDIA.State.BadBuilds)
		}

		return original(ctx, name, args...)
	}
	target := api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}
	require.NoError(t, n.switchBuild(t.Context(), target))
	require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
	require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))
	require.NoError(t, n.deferRollback(t.Context(), "health unavailable"))
	require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))
}
