package applications

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func TestPickApplicationVersion(t *testing.T) { //nolint:paralleltest // points the extensions path at a temporary directory
	systemd.LocalExtensionsPath = t.TempDir()

	for _, v := range []string{"202611010000", "202611020000", "202611030000"} {
		require.NoError(t, os.MkdirAll(filepath.Join(systemd.LocalExtensionsPath, v), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(systemd.LocalExtensionsPath, v, "incus.raw"), nil, 0o600))
	}

	s := &state.State{}
	version := &availableApplicationVersions{appVersion: "202611020000", currentOSVersion: "202611010000", latestDiskVersion: "202611030000"}

	require.Equal(t, "202611030000", pickApplicationVersion(s, "incus", version, "202611030000"))

	// A failed version is never picked while another one is on disk.
	s.UpdateHealth.FailedApplications = map[string]string{"incus:202611030000": "the Incus API does not answer"}
	require.Equal(t, "202611020000", pickApplicationVersion(s, "incus", version, "202611030000"))

	// After an automatic OS rollback, the version that ran before it stays, rather than an older one.
	s.UpdateHealth.FailedApplications = nil
	s.UpdateHealth.KeptApplications = map[string]string{"incus": "202611020000"}
	require.Equal(t, "202611020000", pickApplicationVersion(s, "incus", version, "202611010000"))
	require.Equal(t, []string{"202611010000", "202611020000"}, keepApplicationVersions(s, "incus", []string{"202611010000"}))

	// A kept version that isn't on disk is left out.
	s.UpdateHealth.KeptApplications = map[string]string{"incus": "202611090000"}
	require.Equal(t, "202611010000", pickApplicationVersion(s, "incus", version, "202611010000"))
}
