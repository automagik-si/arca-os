package systemd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/muesli/crunchy"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/passphrase"
	"github.com/lxc/incus-os/incus-osd/internal/secureboot"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// keysDir holds each LUKS volume's own recovery passphrase; tests replace it.
var keysDir = "/var/lib/incus-os/keys"

// luksVolumePartitions returns the LUKS volumes by name; tests replace it.
var luksVolumePartitions = util.GetLUKSVolumePartitions

// luks2KeySlots is the number of keyslots a LUKS2 header holds.
const luks2KeySlots = 32

// GenerateRecoveryKeys utilizes systemd-cryptenroll to generate recovery keys for the
// root and swap LUKS volumes. The logic depends on an existing tpm2-backed key being
// enrolled and accessible, which should be the case on first boot.
//
// A unique recovery passphrase will be generated and saved to disk for each LUKS volume,
// which IncusOS will then use when performing future cryptsetup actions.
//
// If no encryption recovery keys currently exist in state, a second common recovery
// passphrase will also be generated and reported via the security API. Each LUKS volume
// will enroll all encryption recovery keys from state in addition to its own passphrase.
//
// This setup is required so IncusOS can always have a known-good recovery key even if
// the TPM is not in a state that can be used to unlock the LUKS volumes. Every passphrase
// is eight words (see the passphrase package) enrolled as a traditional "password" slot,
// so it can be typed at the console when the TPM refuses to unlock a volume.
func GenerateRecoveryKeys(ctx context.Context, s *state.State) error {
	// Get the underlying LUKS partitions.
	luksVolumes, err := luksVolumePartitions(ctx)
	if err != nil {
		return err
	}

	// On first boot, generate a common recovery passphrase that will be used for both volumes and reported via the security API.
	if len(s.System.Security.Config.EncryptionRecoveryKeys) == 0 {
		commonRecoveryPassword, err := passphrase.Generate()
		if err != nil {
			return err
		}

		// Save the new recovery passphrase to state.
		s.System.Security.Config.EncryptionRecoveryKeys = append(s.System.Security.Config.EncryptionRecoveryKeys, commonRecoveryPassword)
		s.System.Security.State.EncryptionRecoveryKeysRetrieved = false
	}

	// Generate and save unique recovery passphrases for each volume.
	for volumeName, volumeDev := range luksVolumes {
		// Wipe any existing recovery or password keys.
		_, err := subprocess.RunCommandContext(ctx, "systemd-cryptenroll", "--unlock-tpm2-device=auto", "--wipe-slot=recovery,password", volumeDev)
		if err != nil {
			return err
		}

		recoveryPassword, err := passphrase.Generate()
		if err != nil {
			return err
		}

		// Enroll it as a password; systemd-cryptenroll reads it from the NEWPASSWORD environment variable.
		_, _, err = subprocess.RunCommandSplit(ctx, append(os.Environ(), "NEWPASSWORD="+recoveryPassword), nil, "systemd-cryptenroll", "--unlock-tpm2-device=auto", "--password", volumeDev)
		if err != nil {
			return err
		}

		// Write the recovery passphrase to disk.
		f, err := os.Create(filepath.Join(keysDir, "recovery."+volumeName+".key"))
		if err != nil {
			return err
		}
		defer f.Close() //nolint:revive

		_, err = f.WriteString(recoveryPassword)
		if err != nil {
			return err
		}

		// Secure permissions on the file.
		err = f.Chmod(0o400)
		if err != nil {
			return err
		}
	}

	// Finally, enroll each encryption recovery key from state for each volume.
	for _, volumeDev := range luksVolumes {
		for _, key := range s.System.Security.Config.EncryptionRecoveryKeys {
			// Need to provide the recovery key to systemd-cryptenroll via the NEWPASSWORD environment variable.
			_, _, err := subprocess.RunCommandSplit(ctx, append(os.Environ(), "NEWPASSWORD="+key), nil, "systemd-cryptenroll", "--unlock-tpm2-device=auto", "--password", volumeDev)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// AddEncryptionKey utilizes systemd-cryptenroll to add a user-specified key for the
// root and swap LUKS volumes.
func AddEncryptionKey(ctx context.Context, s *state.State, key string, skipKeyValidation bool) error {
	if slices.Contains(s.System.Security.Config.EncryptionRecoveryKeys, key) {
		return errors.New("provided encryption key is already enrolled")
	}

	if !skipKeyValidation {
		validator := crunchy.NewValidatorWithOpts(crunchy.Options{
			MinLength:         15,
			MustContainDigit:  false, // systemd-cryptenroll generates recovery passphrases that don't contain any digits.
			MustContainSymbol: true,
			CheckHIBP:         false,
		})

		err := validator.Check(key)
		if err != nil {
			return err
		}
	}

	// Get the underlying LUKS partitions.
	luksVolumes, err := luksVolumePartitions(ctx)
	if err != nil {
		return err
	}

	// Add the new encryption password. Need to pass to systemd-cryptenroll via NEWPASSWORD environment variable.
	for name, volume := range luksVolumes {
		_, _, err := subprocess.RunCommandSplit(ctx, append(os.Environ(), "NEWPASSWORD="+key), nil, "systemd-cryptenroll", "--unlock-key-file="+filepath.Join(keysDir, "recovery."+name+".key"), "--password", volume)
		if err != nil {
			return err
		}
	}

	s.System.Security.Config.EncryptionRecoveryKeys = append(s.System.Security.Config.EncryptionRecoveryKeys, key)

	return nil
}

// DeleteEncryptionKey utilizes systemd-cryptenroll to remove a user-specified key from the
// root and swap LUKS volumes. Due to systemd-cryptenroll only being able to wipe slots by
// index or type, we must first remove all password slots, then re-add any remaining keys.
func DeleteEncryptionKey(ctx context.Context, s *state.State, key string) error {
	if !slices.Contains(s.System.Security.Config.EncryptionRecoveryKeys, key) {
		return errors.New("provided encryption key is not enrolled")
	}

	if len(s.System.Security.Config.EncryptionRecoveryKeys) == 1 {
		return errors.New("cannot remove only existing recovery key")
	}

	// Get the underlying LUKS partitions.
	luksVolumes, err := luksVolumePartitions(ctx)
	if err != nil {
		return err
	}

	// First, remove every key from state.
	for name, volume := range luksVolumes {
		err := WipeAllRecoveryKeys(ctx, name, volume, s.System.Security.Config.EncryptionRecoveryKeys)
		if err != nil {
			return err
		}
	}

	existingKeys := s.System.Security.Config.EncryptionRecoveryKeys
	s.System.Security.Config.EncryptionRecoveryKeys = []string{}

	// Re-add remaining keys. They were checked when added, and a generated passphrase has no symbol.
	for _, existingKey := range existingKeys {
		if existingKey == key {
			continue
		}

		err := AddEncryptionKey(ctx, s, existingKey, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// WipeAllRecoveryKeys removes the given encryption recovery keys from the provided volume.
// The volume's own passphrase is a password slot too, so the keys are removed one by one; a
// volume whose key file still holds a recovery-type key has every password slot wiped instead.
func WipeAllRecoveryKeys(ctx context.Context, name string, volume string, keys []string) error {
	keyFile := filepath.Join(keysDir, "recovery."+name+".key")

	own, err := os.ReadFile(keyFile) //nolint:gosec
	if err != nil {
		return err
	}

	if !passphrase.Valid(string(own)) {
		_, err := subprocess.RunCommandContext(ctx, "systemd-cryptenroll", "--unlock-key-file="+keyFile, "--wipe-slot=password", volume)

		return err
	}

	for _, key := range keys {
		if key == string(own) {
			continue
		}

		// Each call removes one slot, so repeat until exit code 2 says no slot holds the key.
		for range luks2KeySlots {
			err := subprocess.RunCommandWithFds(ctx, strings.NewReader(key), nil, "cryptsetup", "luksRemoveKey", "--batch-mode", "--key-file=-", volume)
			if err == nil {
				continue
			}

			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
				break
			}

			return err
		}
	}

	return nil
}

// ListEncryptedVolumes returns a list of each encrypted volume and its status.
func ListEncryptedVolumes(ctx context.Context) ([]api.SystemSecurityEncryptedVolume, error) {
	ret := []api.SystemSecurityEncryptedVolume{}

	// Get the LUKS partitions.
	luksVolumes, err := util.GetLUKSVolumePartitions(ctx)
	if err != nil {
		return ret, err
	}

	// Check if the TPM was able to unlock things automatically in the initrd.
	tpmStateIsGood, err := util.TPMAutoUnlocked()
	if err != nil {
		return ret, err
	}

	for volumeName := range luksVolumes {
		// First, check if the volume is mapped, and therefore unlocked.
		_, err := subprocess.RunCommandContext(ctx, "dmsetup", "info", volumeName)
		if err != nil {
			ret = append(ret, api.SystemSecurityEncryptedVolume{
				Volume: volumeName,
				State:  "locked",
			})

			continue
		}

		// Second, test if we can auto-unlock with the current TPM state.
		if !tpmStateIsGood {
			tpmStatus, err := secureboot.TPMStatus()
			if err != nil {
				return ret, err
			}

			// Do we have a PCR mismatch on the TPM? If so, assume we can unlock with the TPM upon reboot.
			if tpmStatus == api.TPMStatusPCRMismatch {
				ret = append(ret, api.SystemSecurityEncryptedVolume{
					Volume: volumeName,
					State:  "unlocked (TPM; PCR update pending)",
				})
			} else {
				ret = append(ret, api.SystemSecurityEncryptedVolume{
					Volume: volumeName,
					State:  api.EncryptedVolumeUnlockedRecovery,
				})
			}

			continue
		}

		// The volume auto-unlocked using the TPM.
		ret = append(ret, api.SystemSecurityEncryptedVolume{
			Volume: volumeName,
			State:  api.EncryptedVolumeUnlockedTPM,
		})
	}

	return ret, nil
}
