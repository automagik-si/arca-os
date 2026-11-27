package applications

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func TestNVIDIACardPresent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, vendor, class string
		want                bool
	}{
		{"an NVIDIA VGA card", "0x10de", "0x030000", true},
		{"an NVIDIA 3D controller", "0x10de", "0x030200", true},
		{"an NVIDIA audio function", "0x10de", "0x040300", false},
		{"an AMD VGA card", "0x1002", "0x030000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeGPUFixtureFile(t, root, "sys/bus/pci/devices/0000:2b:00.0/vendor", tc.vendor)
			writeGPUFixtureFile(t, root, "sys/bus/pci/devices/0000:2b:00.0/class", tc.class)
			writeGPUFixtureFile(t, root, "sys/bus/pci/devices/0000:2b:00.0/device", "0x2805")

			require.Equal(t, tc.want, nvidiaCardPresent(root))
		})
	}

	// A machine whose PCI devices cannot be listed has no card to install a driver for.
	require.False(t, nvidiaCardPresent(t.TempDir()))
}

func TestNVIDIAComesWithTheSystem(t *testing.T) {
	t.Parallel()

	card := func() bool { return true }
	noCard := func() bool { return false }

	// A machine with an NVIDIA card installs the curated driver without being asked.
	names, err := installApplications(t.Context(), readyState(), card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaStable)
	require.NotContains(t, names, nvidiaLTS)

	// A new install gets it at first start, before the system is ready.
	names, err = installApplications(t.Context(), &state.State{}, card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaStable)

	// A machine without one does not.
	names, err = installApplications(t.Context(), readyState(), noCard)
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)
	require.NotContains(t, names, nvidiaLTS)

	// The owner's removal is kept: the driver is not installed again by itself.
	removed := readyState()
	removed.Applications.NVIDIA.State.Removed = true

	names, err = installApplications(t.Context(), removed, card)
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)
	require.NotContains(t, names, nvidiaLTS)

	// Adding the application again ends the removal.
	app, err := Load(t.Context(), removed, nvidiaStable)
	require.NoError(t, err)
	app.SetVersions("202611100018", nil)
	require.False(t, removed.Applications.NVIDIA.State.Removed)

	// A machine already on the other branch stays on it.
	lts := readyState()
	lts.Applications.NVIDIA.State.Initialized = true
	lts.Applications.NVIDIA.State.Version = "202611010000"
	lts.Applications.NVIDIA.State.RequestedBranch = nvidiaLTS

	names, err = installApplications(t.Context(), lts, card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaLTS)
	require.NotContains(t, names, nvidiaStable)
}

func TestNVIDIARemovalIsRemembered(t *testing.T) {
	t.Parallel()

	for _, name := range []string{nvidiaStable, nvidiaLTS} {
		s := &state.State{}
		s.Applications.NVIDIA.State.Initialized = true
		s.Applications.NVIDIA.State.Version = "202611010000"

		// Uninstalling, as a restore does, is not the owner's removal.
		require.NoError(t, removeFromState(s, name))
		require.Empty(t, s.Applications.NVIDIA.State.Version)
		require.False(t, s.Applications.NVIDIA.State.Removed)

		// The owner's remove marks first, and the uninstall keeps the mark for its single save.
		owner := &state.State{}
		owner.Applications.NVIDIA.State.Version = "202611010000"

		require.True(t, MarkOwnerRemoval(owner, name))
		require.NoError(t, removeFromState(owner, name))
		require.Empty(t, owner.Applications.NVIDIA.State.Version)
		require.True(t, owner.Applications.NVIDIA.State.Removed)
	}

	// Removing another application leaves the driver's choice alone.
	s := &state.State{}
	require.False(t, MarkOwnerRemoval(s, "debug"))
	require.False(t, s.Applications.NVIDIA.State.Removed)
}

// The full application check asks the machine's real PCI devices whether it has an NVIDIA card.
//
//nolint:paralleltest // Points the package's card detection at fixtures.
func TestNVIDIACardDecidesTheInstall(t *testing.T) {
	oldSysroot := nvidiaCardSysroot

	t.Cleanup(func() { nvidiaCardSysroot = oldSysroot })

	card := t.TempDir()
	writeGPUFixtureFile(t, card, "sys/bus/pci/devices/0000:2b:00.0/vendor", "0x10de")
	writeGPUFixtureFile(t, card, "sys/bus/pci/devices/0000:2b:00.0/class", "0x030000")
	writeGPUFixtureFile(t, card, "sys/bus/pci/devices/0000:2b:00.0/device", "0x2805")

	nvidiaCardSysroot = card

	names, err := GetInstallApplications(t.Context(), readyState())
	require.NoError(t, err)
	require.Contains(t, names, nvidiaStable)

	nvidiaCardSysroot = t.TempDir()

	names, err = GetInstallApplications(t.Context(), readyState())
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)
}

func readyState() *state.State {
	s := &state.State{}
	s.OS.SystemIsReady = true

	return s
}

// An installed system gets the driver only once it is ready, so the start-up check never downloads it.
//
//nolint:paralleltest // Points the package's extension paths at fixtures.
func TestNVIDIAWaitsForReadyOnAnInstalledSystem(t *testing.T) {
	oldLinks, oldImages := systemd.SystemExtensionsPath, systemd.LocalExtensionsPath

	t.Cleanup(func() { systemd.SystemExtensionsPath, systemd.LocalExtensionsPath = oldLinks, oldImages })

	systemd.SystemExtensionsPath = t.TempDir()
	systemd.LocalExtensionsPath = t.TempDir()

	image := filepath.Join(systemd.LocalExtensionsPath, "202611010000", "incus.raw")
	require.NoError(t, os.MkdirAll(filepath.Dir(image), 0o700))
	require.NoError(t, os.WriteFile(image, nil, 0o600))
	require.NoError(t, os.Symlink(image, filepath.Join(systemd.SystemExtensionsPath, "incus.raw")))

	card := func() bool { return true }

	upgraded := &state.State{}
	upgraded.Applications.Incus.State.Version = "202611010000"

	names, err := installApplications(t.Context(), upgraded, card)
	require.NoError(t, err)
	require.Contains(t, names, "incus")
	require.NotContains(t, names, nvidiaStable)

	upgraded.OS.SystemIsReady = true

	names, err = installApplications(t.Context(), upgraded, card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaStable)
}

// The automatic add waits while an OS update is checked or the updater waits for the owner, even when nothing counts as installed.
//
//nolint:paralleltest // Points the package's extension paths at fixtures.
func TestNVIDIAWaitsForTheUpdater(t *testing.T) {
	oldLinks, oldImages := systemd.SystemExtensionsPath, systemd.LocalExtensionsPath

	t.Cleanup(func() { systemd.SystemExtensionsPath, systemd.LocalExtensionsPath = oldLinks, oldImages })

	card := func() bool { return true }

	// An upgraded machine whose primary is recorded but has no extension link.
	noLink := func() *state.State {
		s := &state.State{}
		s.Applications.Incus.State.Version = "202611010000"
		s.Applications.Incus.State.Initialized = true

		return s
	}

	// An interrupted Incus replacement: the state is cleared, but it was initialised.
	cleared := func() *state.State {
		s := &state.State{}
		s.Applications.Incus.State.Initialized = true

		return s
	}

	// The link is there but its versioned image is gone.
	noImage := func() *state.State {
		s := &state.State{}
		s.Applications.Incus.State.Version = "202611010000"

		return s
	}

	for _, tc := range []struct {
		broken func() *state.State
		link   bool
	}{{noLink, false}, {cleared, false}, {noImage, true}} {
		broken := tc.broken

		systemd.SystemExtensionsPath = t.TempDir()
		systemd.LocalExtensionsPath = t.TempDir()

		if tc.link {
			require.NoError(t, os.Symlink(filepath.Join(systemd.LocalExtensionsPath, "202611010000", "incus.raw"), filepath.Join(systemd.SystemExtensionsPath, "incus.raw")))
		}

		// Without a bake these look like a new install.
		names, err := installApplications(t.Context(), broken(), card)
		require.NoError(t, err)
		require.Contains(t, names, nvidiaStable)

		baking := broken()
		baking.UpdateHealth.Bake = &api.SystemUpdateBake{Version: "202611260000"}

		names, err = installApplications(t.Context(), baking, card)
		require.NoError(t, err)
		require.NotContains(t, names, nvidiaStable)
	}

	// A ready system waits too, while a bake is armed or the updater waits for the owner.
	baking := readyState()
	baking.UpdateHealth.Bake = &api.SystemUpdateBake{Version: "202611260000"}

	names, err := installApplications(t.Context(), baking, card)
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)

	paused := readyState()
	paused.UpdateHealth.NeedsOwner = "the rollback did not start"

	names, err = installApplications(t.Context(), paused, card)
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)
}
