package applications

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestNVIDIAPrunesOnlyUnneededReleases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := state.LoadOrCreate(filepath.Join(root, "state.txt"))
	require.NoError(t, err)

	images := filepath.Join(root, "images")

	versions := []string{"001", "002", "003", "004", "005", "006"}
	for _, version := range versions {
		directory := filepath.Join(images, version)
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.raw"), []byte(version), 0o600))
	}

	require.NoError(t, os.WriteFile(filepath.Join(images, "001", "incus.raw"), []byte("Incus"), 0o600))

	s.Applications.NVIDIA.State.ActiveExtensionVersion = "003"
	s.Applications.NVIDIA.State.Version = "005"
	s.Applications.NVIDIA.State.AvailableVersions = versions
	require.NoError(t, pruneNVIDIAExtensions(s, "002", images))
	require.Equal(t, []string{"002", "003", "005"}, s.Applications.NVIDIA.State.AvailableVersions)

	for _, version := range versions {
		_, err := os.Stat(filepath.Join(images, version, "nvidia.raw"))
		if version == "002" || version == "003" || version == "005" {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, os.ErrNotExist)
		}
	}

	_, err = os.Stat(filepath.Join(images, "001", "incus.raw"))
	require.NoError(t, err)

	s.Applications.NVIDIA.State.ActiveExtensionVersion = "005"
	require.NoError(t, pruneNVIDIAExtensions(s, "003", images))
	require.Equal(t, []string{"003", "005"}, s.Applications.NVIDIA.State.AvailableVersions)

	_, err = os.Stat(filepath.Join(images, "002"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNVIDIAPreparedBuildRunsOnFirstBoot(t *testing.T) {
	t.Parallel()
	n, _, version := newNVIDIAFixture(t, nvidiaBootTest{})
	n.appState.Version = "002"
	n.state.Applications.NVIDIA.State.ActiveExtensionVersion = "002"
	directory := n.path("/var/lib/extensions")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.Symlink("/var/lib/incus-os-extensions/002/nvidia.raw", filepath.Join(directory, "nvidia.raw")))
	require.NoError(t, n.prepare(t.Context(), true))
	require.Equal(t, "002", readNVIDIABoot(t, n).ExtensionVersion)
	persisted, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.Empty(t, persisted.Applications.NVIDIA.State.PendingAttempt)
	require.Empty(t, persisted.Applications.NVIDIA.State.AttemptBootID)
	require.Empty(t, persisted.Applications.NVIDIA.State.PendingHealth)

	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/nvidia-smi" {
			saved, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.Empty(t, saved.Applications.NVIDIA.State.PendingAttempt)
			require.Equal(t, "test-release:"+version, saved.Applications.NVIDIA.State.PendingHealth)
			require.Equal(t, "boot-one", saved.Applications.NVIDIA.State.HealthBootID)
		}

		return original(ctx, name, args...)
	}
	require.NoError(t, n.Start(t.Context()))
	require.Equal(t, "nvidia", n.state.Applications.NVIDIA.State.ActiveDriver)
	require.False(t, n.state.System.Update.State.NeedsReboot)
	require.Empty(t, n.state.Applications.NVIDIA.State.PendingHealth)
}

func TestNVIDIALateSelectionRequiresReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), false))
	n.state.Applications.NVIDIA.State.ActiveExtensionVersion = "002"
	n.appState.Version = "002"
	require.True(t, flagNVIDIAReboot(n.state, readNVIDIABoot(t, n)))
	require.NoError(t, n.Update(t.Context()))
	require.NoError(t, n.Start(t.Context()))
	require.True(t, n.state.System.Update.State.NeedsReboot)
	require.Equal(t, "reboot to enable NVIDIA", n.state.Applications.NVIDIA.State.Reason)
	require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
	require.NotContains(t, *commands, "systemctl --no-block reboot")
}

func TestNVIDIABindingCompleteBeforeHealthStarts(t *testing.T) {
	t.Parallel()
	n, _, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))
	require.Empty(t, n.state.Applications.NVIDIA.State.PendingAttempt)
	require.Empty(t, n.state.Applications.NVIDIA.State.PendingHealth)
	writeGPUFixtureFile(t, n.sysroot, "proc/sys/kernel/random/boot_id", "boot-two")
	require.Empty(t, n.checkPreviousAttempt(t.Context()))
	require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
	require.Empty(t, n.state.Applications.NVIDIA.State.PendingHealth)
}

func TestNVIDIAInterruptedHealthIsQuarantined(t *testing.T) {
	t.Parallel()
	n, _, version := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/nvidia-smi" {
			panic("interrupted health probe")
		}

		return original(ctx, name, args...)
	}

	require.Panics(t, func() { _ = n.Start(t.Context()) })

	persisted, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.Empty(t, persisted.Applications.NVIDIA.State.PendingAttempt)
	require.Equal(t, "test-release:"+version, persisted.Applications.NVIDIA.State.PendingHealth)
	writeGPUFixtureFile(t, n.sysroot, "proc/sys/kernel/random/boot_id", "boot-two")
	require.Empty(t, n.checkPreviousAttempt(t.Context()))
	require.Contains(t, n.state.Applications.NVIDIA.State.BadVersions["test-release:"+version], "previous health check")
}

func TestNVIDIAHealthSaveFailureDoesNotProbeOrReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))
	n.state.UnrecognizedFields = []string{"future.field"}
	require.ErrorContains(t, n.Start(t.Context()), "recorded safely")
	require.NotContains(t, strings.Join(*commands, "\n"), "/usr/bin/nvidia-smi")
	require.NotContains(t, *commands, "systemctl --no-block reboot")
	require.Empty(t, n.state.Applications.NVIDIA.State.PendingHealth)
}
