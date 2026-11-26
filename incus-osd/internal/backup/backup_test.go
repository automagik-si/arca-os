package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/storage"
)

// A data mount holds the owner's data: an OS backup must succeed beside it and a restore must leave it alone.
//
//nolint:paralleltest // Swaps the package's paths and hooks.
func TestBackupAndRestoreLeaveDataMountsAlone(t *testing.T) {
	root := t.TempDir()

	oldDir, oldProcess, oldReboot := osDir, processState, reboot

	t.Cleanup(func() { osDir, processState, reboot = oldDir, oldProcess, oldReboot })

	osDir = filepath.Join(root, "var/lib/incus-os")
	processState = func(context.Context, *state.State, []string) error { return nil }
	reboot = func(context.Context) error { return nil }

	require.NoError(t, os.MkdirAll(filepath.Join(osDir, "keys"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(osDir, "state.txt"), []byte("state"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(osDir, "keys", "luks.nvme-EXAMPLE.key"), []byte("key"), 0o600))

	// A fixture directory stands in for a mounted data mount, laid out where the real one goes.
	dataMount := filepath.Join(root, storage.DataMountRoot, "data")
	ownerFiles := []string{"owner-photos/a.jpg", "incus-pool/containers/c1/file"}

	for _, name := range ownerFiles {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dataMount, name)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dataMount, name), []byte(name), 0o600))
	}

	archive, err := GetOSBackup()
	require.NoError(t, err)

	require.NoError(t, ApplyOSBackup(t.Context(), &state.State{}, bytes.NewReader(archive), nil))

	for _, name := range ownerFiles {
		content, err := os.ReadFile(filepath.Join(dataMount, name))
		require.NoError(t, err, name)
		require.Equal(t, name, string(content))
	}

	content, err := os.ReadFile(filepath.Join(osDir, "keys", "luks.nvme-EXAMPLE.key"))
	require.NoError(t, err)
	require.Equal(t, "key", string(content))
}

// A restore's uninstall of the NVIDIA driver is not the owner's removal; the backup's own choice is kept.
func TestRestoreKeepsTheBackupsNVIDIARemoval(t *testing.T) {
	t.Parallel()

	for _, backupRemoved := range []bool{false, true} {
		machine := &state.State{}
		machine.Applications.NVIDIA.State.Removed = !backupRemoved

		backup := &state.State{}
		backup.Applications.NVIDIA.State.Removed = backupRemoved
		backup.System.Update.Config.CheckFrequency = "12h"

		copyRestoredState(machine, backup)

		require.Equal(t, backupRemoved, machine.Applications.NVIDIA.State.Removed)
		require.Equal(t, "12h", machine.System.Update.Config.CheckFrequency)
	}
}
