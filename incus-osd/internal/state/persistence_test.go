package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSaveDurableRefusesUnknownFields(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.txt")
	s, err := LoadOrCreate(path)
	require.NoError(t, err)
	original, err := os.ReadFile(path)
	require.NoError(t, err)

	s.UnrecognizedFields = []string{"future.field"}
	s.Applications.NVIDIA.State.PendingAttempt = "release:version"
	require.Error(t, s.SaveDurable())
	require.NoError(t, s.Save())

	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, actual)
}
