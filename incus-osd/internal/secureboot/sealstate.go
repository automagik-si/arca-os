package secureboot

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// SealStateFile is where the ESP keeps the record of the state the TPM binding was sealed to.
const SealStateFile = "/boot/arca-tpm-seal.json"

// sealStateVersion is the version of the seal state record written by this code.
const sealStateVersion = 1

// SealStatePath is the seal state record written by RecordSealState; tests replace it.
var SealStatePath = SealStateFile

// SealStateBootMarker says this boot already sealed or checked the record, on a per-boot tmpfs; tests replace it.
var SealStateBootMarker = "/run/incus-os/tpm-seal-state.boot"

// The readers RecordSealState uses; tests replace them.
var (
	sealReadPCR         = ReadPCR
	sealReadEFIVariable = util.ReadEFIVariable
	sealNow             = time.Now
)

// SealState records the measurements the LUKS TPM binding was last sealed to.
// It holds no secret: only the Secure Boot state and public PCR values.
type SealState struct {
	Version           int    `json:"version"`
	UKIVersion        string `json:"uki_version"`
	SecureBootEnabled bool   `json:"secure_boot_enabled"`
	SetupMode         bool   `json:"setup_mode"`
	PCR4              string `json:"pcr4"`
	PCR7              string `json:"pcr7"`
	PCR12             string `json:"pcr12"`
	SealedAt          string `json:"sealed_at"`
}

// Measurements is the boot state compared against a seal state record.
// An empty PCR value means it could not be read and is not compared.
type Measurements struct {
	SecureBootEnabled bool
	SetupMode         bool
	PCR4              string
	PCR7              string
	PCR12             string
}

// RecordSealState writes the seal state record for the given UKI and the PCR values the
// binding was sealed to; a nil value is read from the TPM. Failures are only logged, as
// the record is informational and must never fail a seal.
func RecordSealState(ukiFile string, pcr4 []byte, pcr7 []byte, pcr12 []byte) {
	// The record now holds the next boot's values, which RepairSealState must never replace on this boot.
	err := markSealStateBoot()
	if err != nil {
		slog.Warn("Failed to mark the TPM seal state for this boot", "err", err)
	}

	err = writeSealState(SealStatePath, buildSealState(ukiFile, pcr4, pcr7, pcr12))
	if err != nil {
		slog.Warn("Failed to record the TPM seal state", "err", err)
	}
}

// sealedVolume is the volume the initrd explains, so a seal records its state once this one is bound.
const sealedVolume = "root"

// beginSeal marks this boot before a seal changes a TPM binding, so a restart midway can't repair the record.
func beginSeal() {
	err := markSealStateBoot()
	if err != nil {
		slog.Warn("Failed to mark the TPM seal state for this boot", "err", err)
	}
}

// markSealStateBoot records that this boot sealed or checked the seal state record.
func markSealStateBoot() error {
	err := os.MkdirAll(filepath.Dir(SealStateBootMarker), 0o700)
	if err != nil {
		return err
	}

	return os.WriteFile(SealStateBootMarker, nil, 0o600)
}

// RepairSealState rewrites a missing, unreadable or stale record from the current measurements,
// but only when the TPM unlocked the volumes on this boot, as these are then the sealed ones.
// It runs at most once per boot and never after a seal on this boot, whose record holds the
// next boot's values: a restarted incus-osd or a seal before startup leaves the record alone.
func RepairSealState(tpmAutoUnlocked bool) {
	if !tpmAutoUnlocked {
		return
	}

	_, err := os.Stat(SealStateBootMarker)
	if !errors.Is(err, os.ErrNotExist) {
		return
	}

	// Without the marker, a later start on this boot could undo a seal, so don't repair.
	err = markSealStateBoot()
	if err != nil {
		slog.Warn("Failed to mark the TPM seal state for this boot", "err", err)

		return
	}

	fresh := buildSealState("", nil, nil, nil)
	current := Measurements{
		SecureBootEnabled: fresh.SecureBootEnabled,
		SetupMode:         fresh.SetupMode,
		PCR4:              fresh.PCR4,
		PCR7:              fresh.PCR7,
		PCR12:             fresh.PCR12,
	}

	record, err := ReadSealState(SealStatePath)
	if err == nil && DiagnoseSeal(record, current) == SealMatch {
		return
	}

	err = writeSealState(SealStatePath, fresh)
	if err != nil {
		slog.Warn("Failed to repair the TPM seal state", "err", err)
	}
}

// buildSealState gathers the seal state record from the given values and the firmware.
func buildSealState(ukiFile string, pcr4 []byte, pcr7 []byte, pcr12 []byte) SealState {
	if ukiFile == "" {
		versions, err := util.GetUKIVersions()
		if err == nil {
			ukiFile = versions.CurrentFilepath
		}
	}

	return SealState{
		Version:           sealStateVersion,
		UKIVersion:        util.UKIFileVersion(filepath.Base(ukiFile)),
		SecureBootEnabled: sealEFIFlag("SecureBoot"),
		SetupMode:         sealEFIFlag("SetupMode"),
		PCR4:              sealPCR("4", pcr4),
		PCR7:              sealPCR("7", pcr7),
		PCR12:             sealPCR("12", pcr12),
		SealedAt:          sealNow().UTC().Format(time.RFC3339),
	}
}

// sealEFIFlag returns whether a one-byte EFI variable is set, as Enabled does.
func sealEFIFlag(name string) bool {
	value, err := sealReadEFIVariable(name)
	if err != nil {
		slog.Warn("Failed to read EFI variable for the TPM seal state", "name", name, "err", err)

		return false
	}

	return len(value) > 0 && value[0] == 1
}

// sealPCR returns the hex value of a PCR, reading it from the TPM when not given.
func sealPCR(index string, value []byte) string {
	if value == nil {
		var err error

		value, err = sealReadPCR(index)
		if err != nil {
			slog.Warn("Failed to read PCR for the TPM seal state", "pcr", index, "err", err)

			return ""
		}
	}

	return hex.EncodeToString(value)
}

// writeSealState atomically replaces the seal state record.
func writeSealState(path string, state SealState) error {
	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"

	err = os.WriteFile(tmp, append(content, '\n'), 0o644)
	if err != nil {
		return err
	}

	err = os.Rename(tmp, path)
	if err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return nil
}

// ReadSealState reads a seal state record.
func ReadSealState(path string) (*SealState, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	state := &SealState{}

	err = json.Unmarshal(content, state)
	if err != nil {
		return nil, err
	}

	if state.Version < 1 {
		return nil, errors.New("seal state record has no version")
	}

	// The ESP isn't authenticated and the record reaches the console, so only plain values pass.
	for _, value := range []string{state.PCR4, state.PCR7, state.PCR12} {
		if value != "" && !isPCRHex(value) {
			return nil, errors.New("seal state record has a malformed PCR value")
		}
	}

	if strings.Trim(state.UKIVersion, "0123456789") != "" || len(state.UKIVersion) > 32 {
		return nil, errors.New("seal state record has a malformed version")
	}

	return state, nil
}

// ReadMeasurements reads the current Secure Boot state and PCRs from an efivars and a PCR directory.
func ReadMeasurements(efivarsDir string, pcrsDir string) Measurements {
	return Measurements{
		SecureBootEnabled: efiFileFlag(filepath.Join(efivarsDir, "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c")),
		SetupMode:         efiFileFlag(filepath.Join(efivarsDir, "SetupMode-8be4df61-93ca-11d2-aa0d-00e098032b8c")),
		PCR4:              pcrFile(filepath.Join(pcrsDir, "4")),
		PCR7:              pcrFile(filepath.Join(pcrsDir, "7")),
		PCR12:             pcrFile(filepath.Join(pcrsDir, "12")),
	}
}

// efiFileFlag returns whether a one-byte efivarfs file, after its four attribute bytes, is set.
func efiFileFlag(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil || len(content) < 5 {
		return false
	}

	return content[4] == 1
}

// pcrFile returns a sysfs PCR value as lowercase hex, or an empty string when unreadable.
func pcrFile(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	value := strings.ToLower(strings.TrimSpace(string(content)))
	if !isPCRHex(value) {
		return ""
	}

	return value
}

// isPCRHex reports whether a value is a SHA-256 PCR in hex.
func isPCRHex(value string) bool {
	_, err := hex.DecodeString(value)

	return err == nil && len(value) == 64
}

// SealCause is why the boot's measurements no longer match the seal state record.
type SealCause int

const (
	// SealMatch means nothing measured changed, there is nothing to say.
	SealMatch SealCause = iota
	// SealRecordMissing means the disk has no seal state record.
	SealRecordMissing
	// SealSecureBootState means Secure Boot or Setup Mode was turned on or off.
	SealSecureBootState
	// SealSecureBootKeys means the Secure Boot keys changed with the same Secure Boot state (PCR7).
	SealSecureBootKeys
	// SealImage means a different boot loader or OS image was booted (PCR4).
	SealImage
	// SealBootParameters means the kernel command line, credentials or add-ons changed (PCR12).
	SealBootParameters
)

// DiagnoseSeal compares the boot's measurements with the seal state record, a nil record
// meaning there is none. PCR4 and PCR12 are only bound, so only compared, without Secure Boot.
func DiagnoseSeal(record *SealState, current Measurements) SealCause {
	if record == nil {
		return SealRecordMissing
	}

	if record.SecureBootEnabled != current.SecureBootEnabled || record.SetupMode != current.SetupMode {
		return SealSecureBootState
	}

	if pcrChanged(record.PCR7, current.PCR7) {
		return SealSecureBootKeys
	}

	if record.SecureBootEnabled {
		return SealMatch
	}

	if pcrChanged(record.PCR4, current.PCR4) {
		return SealImage
	}

	if pcrChanged(record.PCR12, current.PCR12) {
		return SealBootParameters
	}

	return SealMatch
}

// pcrChanged reports whether both values are known and differ.
func pcrChanged(recorded string, current string) bool {
	return recorded != "" && current != "" && !strings.EqualFold(recorded, current)
}

// passphraseLines are the way back that always works.
var passphraseLines = []string{
	"Or type the recovery passphrase (8 words) or older recovery key (dashes) below.",
	"That re-seals the TPM to this state: type it only if you made the change.",
}

// SealMessage returns the plain-words lines shown before the passphrase prompt, none on a match.
func SealMessage(cause SealCause, record *SealState, current Measurements) []string {
	lines := make([]string, 0, 3+len(passphraseLines))

	switch cause { //nolint:exhaustive
	case SealRecordMissing:
		return []string{"If asked, type the recovery passphrase (8 words) or older recovery key (dashes)"}
	case SealSecureBootState:
		sealed := secureBootState(record.SecureBootEnabled, record.SetupMode)
		lines = append(lines,
			"The TPM won't unlock the drive: Secure Boot changed since it was sealed.",
			"Sealed with "+sealed+", now "+secureBootState(current.SecureBootEnabled, current.SetupMode)+".",
			"Way back: set the firmware back to "+sealed+", then boot again.",
		)
	case SealSecureBootKeys:
		lines = append(lines,
			"The TPM won't unlock the drive: Secure Boot keys changed since it was sealed.",
			"PCR7 sealed "+shortPCR(record.PCR7)+", now "+shortPCR(current.PCR7),
			"Way back: restore the firmware's Secure Boot keys, then boot again.",
		)
	case SealImage:
		lines = append(lines,
			"The TPM won't unlock the drive: a different OS image was started.",
			"PCR4 sealed "+shortPCR(record.PCR4)+", now "+shortPCR(current.PCR4),
			"Way back: pick "+ukiVersionText(record)+" in the boot menu, then boot again.",
		)
	case SealBootParameters:
		lines = append(lines,
			"The TPM won't unlock the drive: boot parameters changed since it was sealed.",
			"PCR12 sealed "+shortPCR(record.PCR12)+", now "+shortPCR(current.PCR12),
			"Way back: drop the extra command line, credentials or add-ons, then boot again.",
		)
	default:
		// A match has nothing to say.
		return nil
	}

	return append(lines, passphraseLines...)
}

// secureBootState describes a Secure Boot state in words.
func secureBootState(enabled bool, setupMode bool) string {
	switch {
	case setupMode:
		return "Secure Boot in Setup Mode"
	case enabled:
		return "Secure Boot on"
	default:
		return "Secure Boot off"
	}
}

// shortPCR abbreviates a PCR value for the console.
func shortPCR(value string) string {
	if value == "" {
		return "unknown"
	}

	if len(value) > 16 {
		return value[:16] + "..."
	}

	return value
}

// ukiVersionText names the sealed OS version, when known.
func ukiVersionText(record *SealState) string {
	if record.UKIVersion == "" {
		return "the sealed version"
	}

	return "version " + record.UKIVersion
}
