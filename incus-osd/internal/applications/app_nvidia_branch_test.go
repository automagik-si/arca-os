package applications

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// bootRedownloads mirrors startApplications in cmd/incus-osd/main.go: it fetches initialized applications that are not installed.
func bootRedownloads(t *testing.T, s *state.State) []string {
	t.Helper()

	names := []string{}

	for _, name := range Supported {
		app, err := Load(t.Context(), s, name)
		require.NoError(t, err)

		if !app.IsInitialized() || app.IsInstalled() {
			continue
		}

		names = append(names, name)
	}

	return names
}

func TestNVIDIABranchStaysAtBoot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ requested, running, other string }{
		{"", nvidiaStable, nvidiaLTS},
		{nvidiaLTS, nvidiaLTS, nvidiaStable},
	} {
		t.Run(tc.running, func(t *testing.T) {
			t.Parallel()

			s := &state.State{}
			s.Applications.NVIDIA.State.Initialized = true
			s.Applications.NVIDIA.State.Version = "202611010000"
			s.Applications.NVIDIA.State.RequestedBranch = tc.requested

			// With both packs on the server, a boot fetches neither branch again.
			require.Empty(t, bootRedownloads(t, s))

			// The update check downloads only the running branch.
			names, err := GetInstallApplications(t.Context(), s)
			require.NoError(t, err)
			require.Contains(t, names, tc.running)
			require.NotContains(t, names, tc.other)
			require.Equal(t, tc.requested, s.Applications.NVIDIA.State.RequestedBranch)

			running, err := Load(t.Context(), s, tc.running)
			require.NoError(t, err)
			require.True(t, running.IsInitialized())

			other, err := Load(t.Context(), s, tc.other)
			require.NoError(t, err)
			require.False(t, other.IsInitialized())

			// An explicit switch moves the initialized branch, so the update reloads it instead of starting over.
			other.SetVersions("202611100018", nil)
			require.True(t, other.IsInitialized())
			require.False(t, running.IsInitialized())
			require.Empty(t, bootRedownloads(t, s))
		})
	}
}
