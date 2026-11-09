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

// fakeBootctl puts a bootctl on PATH that lists entries and records the one-shot entry.
func fakeBootctl(t *testing.T, entries string) string {
	t.Helper()

	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\nlist) printf '%s' '" + entries + "' ;;\nset-oneshot) printf '%s' \"$2\" > " + dir + "/oneshot ;;\n*) exit 1 ;;\nesac\n"

	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootctl"), []byte(script), 0o700)) //nolint:gosec // the fake bootctl must be executable
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	return filepath.Join(dir, "oneshot")
}

func TestNextBootSkipsFailedVersions(t *testing.T) { //nolint:paralleltest // sets PATH and the failed versions
	oneshot := fakeBootctl(t, `[{"type":"type2","id":"IncusOS_202611010000.efi@main"},{"type":"type2","id":"IncusOS_202611020000+2-1.efi@main"}]`)
	ctx := t.Context()

	defer SetFailedBootVersions(nil)

	next := func() string {
		body, err := os.ReadFile(oneshot)
		require.NoError(t, err)

		return string(body)
	}

	// The newest version, by its listed ID with the boot counter.
	require.NoError(t, SetNextBootID(ctx, BootIDNewest))
	require.Equal(t, "IncusOS_202611020000+2-1.efi@main", next())

	// A failed version is skipped by every next-boot choice, a reboot or power-off included.
	SetFailedBootVersions([]string{"202611020000"})
	require.NoError(t, SetNextBootID(ctx, BootIDNewest))
	require.Equal(t, "IncusOS_202611010000.efi@main", next())

	// With every version failed, the plain choice stands.
	SetFailedBootVersions([]string{"202611010000", "202611020000"})
	require.NoError(t, SetNextBootID(ctx, BootIDNewest))
	require.Equal(t, "IncusOS_202611020000+2-1.efi@main", next())

	require.NoError(t, SetNextBootVersion(ctx, "202611010000"))
	require.Equal(t, "IncusOS_202611010000.efi@main", next())
	require.Error(t, SetNextBootVersion(ctx, "202611030000"))
}
