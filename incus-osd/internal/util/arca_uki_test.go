package util

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func ukiNames(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(UKIDir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	slices.Sort(names)

	return names
}

func TestUKIBootCounting(t *testing.T) { //nolint:paralleltest // points UKIDir at a temporary directory
	UKIDir = t.TempDir()

	for _, name := range []string{"IncusOS_202611010000.efi", "IncusOS_202611020000+2-1.efi"} {
		require.NoError(t, os.WriteFile(filepath.Join(UKIDir, name), nil, 0o600))
	}

	require.Equal(t, "202611020000", UKIFileVersion("IncusOS_202611020000+2-1.efi"))
	require.Equal(t, "202611010000", UKIFileVersion("IncusOS_202611010000.efi"))
	require.Equal(t, "202611010000", UKIFileVersion("IncusOS_202611010000+3.efi"))
	require.Empty(t, UKIFileVersion("IncusOS_2026.efi"))

	require.NoError(t, MarkUKIBad("202611020000"))
	require.Equal(t, []string{"IncusOS_202611010000.efi", "IncusOS_202611020000+0-1.efi"}, ukiNames(t))

	require.NoError(t, ResetUKITries("202611020000"))
	require.Equal(t, []string{"IncusOS_202611010000.efi", "IncusOS_202611020000+3-0.efi"}, ukiNames(t))

	require.NoError(t, MarkUKIGood("202611020000"))
	require.NoError(t, MarkUKIGood("202611010000"))
	require.Equal(t, []string{"IncusOS_202611010000.efi", "IncusOS_202611020000.efi"}, ukiNames(t))

	require.Error(t, MarkUKIGood("202611030000"))
	require.Equal(t, "IncusOS_202611020000.efi@main", ukiCounterRegex.ReplaceAllString("IncusOS_202611020000+3-0.efi@main", ".efi"))
}
