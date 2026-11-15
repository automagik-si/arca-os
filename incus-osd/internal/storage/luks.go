package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// luksKeysDir, luksMapperDir and runCryptsetup let tests stand in for the key store and cryptsetup.
var (
	luksKeysDir   = "/var/lib/incus-os/keys"
	luksMapperDir = "/dev/mapper"
	runCryptsetup = func(ctx context.Context, args ...string) error {
		_, err := subprocess.RunCommandContext(ctx, "cryptsetup", args...)

		return err
	}
)

type cryptsetupLuksDumpPartialParse struct {
	Tokens map[string]struct {
		Type     string `json:"type"`
		TPM2PCRS []int  `json:"tpm2-pcrs"` //nolint:tagliatelle
	} `json:"tokens"`
}

// EncryptDrive wipes and formats a drive as a LUKS device.
func EncryptDrive(ctx context.Context, devPath string, secure bool) error {
	if !strings.HasPrefix(devPath, "/dev/disk/by-id/") {
		return errors.New("invalid disk id")
	}

	devName := filepath.Base(devPath)
	keyfilePath := "/var/lib/incus-os/keys/luks." + devName + ".key"

	// Wipe the drive.
	err := WipeDrive(ctx, devPath, secure)
	if err != nil {
		return err
	}

	// Generate a random encryption key.
	err = util.GenerateEncryptionKeyFile(keyfilePath)
	if err != nil {
		return err
	}

	// Format the drive.
	_, err = subprocess.RunCommandContext(ctx, "cryptsetup", "luksFormat", "-q", devPath, keyfilePath)
	if err != nil {
		return err
	}

	// Unlock the drive.
	return unlockDrive(ctx, devPath)
}

// ImportEncryptedDrive decrypts a drive using a user provided key.
func ImportEncryptedDrive(ctx context.Context, devPath string, key string) error {
	if !strings.HasPrefix(devPath, "/dev/disk/by-id/") {
		return errors.New("invalid disk id")
	}

	devName := filepath.Base(devPath)
	keyfilePath := filepath.Join(luksKeysDir, "luks."+devName+".key")

	// Decode encryption key into raw bytes.
	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return err
	}

	if len(rawKey) != 32 {
		return fmt.Errorf("expected a 32 byte raw encryption key, got %d bytes", len(rawKey))
	}

	// The stored key is only replaced by one proven to open the drive, so a failure here never loses it.
	tmp, err := os.CreateTemp(luksKeysDir, "import-*.tmp")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck

	_, err = tmp.Write(rawKey)
	if err != nil {
		_ = tmp.Close()

		return err
	}

	err = tmp.Close()
	if err != nil {
		return err
	}

	// Test the key against a keyslot, even if the drive is already unlocked.
	err = runCryptsetup(ctx, "open", "--test-passphrase", "--type=luks", "--key-file", tmp.Name(), devPath)
	if err != nil {
		return fmt.Errorf("the key does not open %s: %w", devPath, err)
	}

	// Unlock the drive.
	err = openDrive(ctx, devPath, tmp.Name())
	if err != nil {
		return err
	}

	return os.Rename(tmp.Name(), keyfilePath)
}

// DecryptDrives decrypts all LUKS encrypted drives on the system where a key is available.
func DecryptDrives(ctx context.Context) error {
	// Look for keys.
	entries, err := os.ReadDir("/var/lib/incus-os/keys/")
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entryName := entry.Name()

		if !strings.HasPrefix(entryName, "luks.") || !strings.HasSuffix(entryName, ".key") {
			continue
		}

		devID := strings.TrimSuffix(strings.TrimPrefix(entryName, "luks."), ".key")
		devPath := "/dev/disk/by-id/" + devID

		_, err := os.Stat(devPath)
		if err != nil {
			slog.Warn("Couldn't find encrypted drive", "id", devPath, "err", err)

			continue
		}

		err = unlockDrive(ctx, devPath)
		if err != nil {
			slog.Warn("Couldn't unlock encrypted drive", "id", devPath, "err", err)

			continue
		}
	}

	return nil
}

// GetDriveKeys returns a map of device IDs to their base64 encoded keys.
func GetDriveKeys() (map[string]string, error) {
	keys := map[string]string{}

	// Look for keys.
	entries, err := os.ReadDir("/var/lib/incus-os/keys/")
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		entryName := entry.Name()

		if !strings.HasPrefix(entryName, "luks.") || !strings.HasSuffix(entryName, ".key") {
			continue
		}

		devID := strings.TrimSuffix(strings.TrimPrefix(entryName, "luks."), ".key")

		devKey, err := os.ReadFile("/var/lib/incus-os/keys/" + entryName)
		if err != nil {
			return nil, err
		}

		keys[devID] = base64.StdEncoding.EncodeToString(devKey)
	}

	return keys, nil
}

// LUKSBoundToPCR determines if the given LUKS volume is bound to the specified PCR.
func LUKSBoundToPCR(ctx context.Context, devPath string, pcrIndex int) (bool, error) {
	output, err := subprocess.RunCommandContext(ctx, "cryptsetup", "luksDump", "--dump-json-metadata", devPath)
	if err != nil {
		return false, err
	}

	state := cryptsetupLuksDumpPartialParse{}

	err = json.Unmarshal([]byte(output), &state)
	if err != nil {
		return false, err
	}

	for _, token := range state.Tokens {
		if token.Type != "systemd-tpm2" {
			continue
		}

		if slices.Contains(token.TPM2PCRS, pcrIndex) {
			return true, nil
		}
	}

	return false, nil
}

func unlockDrive(ctx context.Context, devPath string) error {
	return openDrive(ctx, devPath, filepath.Join(luksKeysDir, "luks."+filepath.Base(devPath)+".key"))
}

// openDrive opens devPath with keyfile, unless it is already open.
func openDrive(ctx context.Context, devPath string, keyfile string) error {
	devName := filepath.Base(devPath)

	_, err := os.Stat(filepath.Join(luksMapperDir, "luks-"+devName))
	if err == nil {
		// Already unlocked.
		return nil
	}

	return runCryptsetup(ctx, "open", "--type=luks", "-d", keyfile, devPath, "luks-"+devName)
}

func lockDrive(ctx context.Context, devPath string) error {
	devName := filepath.Base(devPath)

	_, err := os.Stat("/dev/mapper/luks-" + devName)
	if err != nil {
		return err
	}

	_, err = subprocess.RunCommandContext(ctx, "cryptsetup", "close", "/dev/mapper/luks-"+devName)
	if err != nil {
		return err
	}

	return nil
}

// luksKeyName returns the key name for the first by-id path with a key file, keys are named after the path used to encrypt.
func luksKeyName(paths ...string) string {
	for _, path := range paths {
		if path == "" {
			continue
		}

		name := filepath.Base(path)

		_, err := os.Stat("/var/lib/incus-os/keys/luks." + name + ".key")
		if err == nil {
			return name
		}
	}

	return ""
}
