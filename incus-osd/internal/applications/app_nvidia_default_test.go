package applications

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
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
	names, err := installApplications(t.Context(), &state.State{}, card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaStable)
	require.NotContains(t, names, nvidiaLTS)

	// A machine without one does not.
	names, err = installApplications(t.Context(), &state.State{}, noCard)
	require.NoError(t, err)
	require.NotContains(t, names, nvidiaStable)
	require.NotContains(t, names, nvidiaLTS)

	// The owner's removal is kept: the driver is not installed again by itself.
	removed := &state.State{}
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
	lts := &state.State{}
	lts.Applications.NVIDIA.State.Initialized = true
	lts.Applications.NVIDIA.State.Version = "202611010000"
	lts.Applications.NVIDIA.State.RequestedBranch = nvidiaLTS

	names, err = installApplications(t.Context(), lts, card)
	require.NoError(t, err)
	require.Contains(t, names, nvidiaLTS)
	require.NotContains(t, names, nvidiaStable)
}
