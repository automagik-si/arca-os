package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeCryptsetup accepts only goodKey, records each call and maps the drive on open.
type fakeCryptsetup struct {
	t         *testing.T
	goodKey   []byte
	keyfile   string
	oldKey    []byte
	calls     [][]string
	mapperDir string
}

func (f *fakeCryptsetup) run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)

	keyfile := args[slices.IndexFunc(args, func(a string) bool { return a == "-d" || a == "--key-file" })+1]

	// The stored key must not have changed while the new one is checked.
	stored, err := os.ReadFile(f.keyfile)
	require.NoError(f.t, err)
	require.Equal(f.t, f.oldKey, stored)

	given, err := os.ReadFile(keyfile)
	require.NoError(f.t, err)

	if string(given) != string(f.goodKey) {
		return errors.New("no key available with this passphrase")
	}

	if !slices.Contains(args, "--test-passphrase") {
		return os.WriteFile(filepath.Join(f.mapperDir, args[len(args)-1]), nil, 0o600)
	}

	return nil
}

// setupImport points the key store and cryptsetup at fixtures holding oldKey as the drive's stored key.
func setupImport(t *testing.T, goodKey []byte, unlocked bool) (*fakeCryptsetup, string) {
	t.Helper()

	oldDirs, oldMapper, oldRun := luksKeysDir, luksMapperDir, runCryptsetup

	t.Cleanup(func() { luksKeysDir, luksMapperDir, runCryptsetup = oldDirs, oldMapper, oldRun })

	luksKeysDir = t.TempDir()
	luksMapperDir = t.TempDir()

	f := &fakeCryptsetup{t: t, goodKey: goodKey, oldKey: []byte("old key, known to be good....32b"), mapperDir: luksMapperDir}
	f.keyfile = filepath.Join(luksKeysDir, "luks.nvme-EXAMPLE.key")
	require.NoError(t, os.WriteFile(f.keyfile, f.oldKey, 0o600))

	if unlocked {
		require.NoError(t, os.WriteFile(filepath.Join(luksMapperDir, "luks-nvme-EXAMPLE"), nil, 0o600))
	}

	runCryptsetup = f.run

	return f, f.keyfile
}

func requireOnlyKeyFile(t *testing.T, keyfile string, content []byte) {
	t.Helper()

	stored, err := os.ReadFile(keyfile)
	require.NoError(t, err)
	require.Equal(t, content, stored)

	entries, err := os.ReadDir(filepath.Dir(keyfile))
	require.NoError(t, err)
	require.Len(t, entries, 1, "a temporary key file was left behind")
}

//nolint:paralleltest // Swaps the key store and cryptsetup.
func TestImportEncryptedDriveWrongKeyLocked(t *testing.T) {
	f, keyfile := setupImport(t, []byte("old key, known to be good....32b"), false)

	err := ImportEncryptedDrive(t.Context(), "/dev/disk/by-id/nvme-EXAMPLE", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.Error(t, err)

	requireOnlyKeyFile(t, keyfile, f.oldKey)
	require.NoFileExists(t, filepath.Join(luksMapperDir, "luks-nvme-EXAMPLE"))
}

//nolint:paralleltest // Swaps the key store and cryptsetup.
func TestImportEncryptedDriveWrongKeyUnlocked(t *testing.T) {
	f, keyfile := setupImport(t, []byte("old key, known to be good....32b"), true)

	err := ImportEncryptedDrive(t.Context(), "/dev/disk/by-id/nvme-EXAMPLE", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.Error(t, err)

	requireOnlyKeyFile(t, keyfile, f.oldKey)
}

//nolint:paralleltest // Swaps the key store and cryptsetup.
func TestImportEncryptedDriveGoodKey(t *testing.T) {
	newKey := []byte("a new key that opens slot 1...32")
	f, keyfile := setupImport(t, newKey, false)

	require.NoError(t, ImportEncryptedDrive(t.Context(), "/dev/disk/by-id/nvme-EXAMPLE", base64.StdEncoding.EncodeToString(newKey)))

	// Tested first, then opened, and only then put in place of the old key.
	require.Equal(t, "--test-passphrase", f.calls[0][1])
	require.Equal(t, "luks-nvme-EXAMPLE", f.calls[1][len(f.calls[1])-1])
	require.Len(t, f.calls, 2)

	requireOnlyKeyFile(t, keyfile, newKey)

	info, err := os.Stat(keyfile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.FileExists(t, filepath.Join(luksMapperDir, "luks-nvme-EXAMPLE"))
}
