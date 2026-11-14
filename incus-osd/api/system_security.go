package api

// TPMStatus defines a custom type for reporting the system's TPM status.
type TPMStatus string

// Define constants used to report the state of the system's TPM.
const (
	TPMStatusOK          TPMStatus = "ok"
	TPMStatusPCRMismatch TPMStatus = "pcr mismatch"
	TPMStatusSWTPM       TPMStatus = "swtpm"
)

// SystemSecurityState holds information about the current security state.
type SystemSecurityState struct {
	EncryptedVolumes                []SystemSecurityEncryptedVolume       `incusos:"-"                               json:"encrypted_volumes"                  yaml:"encrypted_volumes"`
	EncryptionRecoveryKeysRetrieved bool                                  `json:"encryption_recovery_keys_retrieved" yaml:"encryption_recovery_keys_retrieved"`
	DriveRecoveryKeys               map[string]string                     `incusos:"-"                               json:"drive_recovery_keys,omitempty"      yaml:"drive_recovery_keys,omitempty"`
	PoolRecoveryKeys                map[string]string                     `incusos:"-"                               json:"pool_recovery_keys"                 yaml:"pool_recovery_keys"`
	SecureBootCertificates          []SystemSecuritySecureBootCertificate `incusos:"-"                               json:"secure_boot_certificates"           yaml:"secure_boot_certificates"`
	SecureBootEnabled               bool                                  `incusos:"-"                               json:"secure_boot_enabled"                yaml:"secure_boot_enabled"`
	SystemStateIsTrusted            bool                                  `incusos:"-"                               json:"system_state_is_trusted"            yaml:"system_state_is_trusted"`
	SystemStateStatus               string                                `incusos:"-"                               json:"system_state_status"                yaml:"system_state_status"`
	TPMStatus                       TPMStatus                             `incusos:"-"                               json:"tpm_status"                         yaml:"tpm_status"`
	TPMPublicKey                    string                                `incusos:"-"                               json:"tpm_public_key,omitempty"           yaml:"tpm_public_key,omitempty"`

	// LastRecoveryUnlock is when the disk was last unlocked with a recovery passphrase instead of the TPM (RFC3339).
	LastRecoveryUnlock string `json:"last_recovery_unlock,omitempty" yaml:"last_recovery_unlock,omitempty"`

	// ResealStatus is what the last recovery unlock did to the TPM seal: "re-sealed", "reseal failed", or empty.
	ResealStatus string `json:"reseal_status,omitempty" yaml:"reseal_status,omitempty"`

	// ResealReason is why the re-seal failed, in one short line.
	ResealReason string `json:"reseal_reason,omitempty" yaml:"reseal_reason,omitempty"`
}

// The states of an encrypted volume that tell how it was unlocked on this boot.
const (
	EncryptedVolumeUnlockedTPM      = "unlocked (TPM)"
	EncryptedVolumeUnlockedRecovery = "unlocked (recovery passphrase)"
)

// The results of the TPM re-seal after a recovery unlock.
const (
	ResealStatusResealed = "re-sealed"
	ResealStatusFailed   = "reseal failed"
)

// RecoveryUnlocked reports whether this boot's root volume was unlocked with a recovery passphrase
// and no volume was unlocked by the TPM.
func (s SystemSecurityState) RecoveryUnlocked() bool {
	root := false

	for _, v := range s.EncryptedVolumes {
		if v.State == EncryptedVolumeUnlockedTPM {
			return false
		}

		if v.Volume == "root" {
			root = v.State == EncryptedVolumeUnlockedRecovery
		}
	}

	return root
}

// ResealSummary is the owner's sentence about this boot's recovery unlock and the TPM re-seal,
// or "" when the boot did not use the recovery passphrase.
func (s SystemSecurityState) ResealSummary() string {
	if !s.RecoveryUnlocked() {
		return ""
	}

	return ResealSentence(s.ResealStatus, s.ResealReason)
}

// ResealSentence is the owner's sentence for a re-seal status and reason, or "" for no status.
func ResealSentence(status string, reason string) string {
	switch status {
	case ResealStatusResealed:
		return "The disk was unlocked with the recovery passphrase; the TPM has been re-sealed to this system's current state"
	case ResealStatusFailed:
		return "The disk was unlocked with the recovery passphrase; the TPM could not be re-sealed: " + reason +
			"; the passphrase will be asked again next boot"
	default:
		return ""
	}
}

// SystemSecurityConfig holds additional security configuration settings.
type SystemSecurityConfig struct {
	CustomCACerts          []string `json:"custom_ca_certs,omitempty" yaml:"custom_ca_certs,omitempty"`
	EncryptionRecoveryKeys []string `json:"encryption_recovery_keys"  yaml:"encryption_recovery_keys"`
}

// SystemSecurity defines a struct to hold information about the system's security state.
//
// swagger:model
type SystemSecurity struct {
	Config SystemSecurityConfig `json:"config" yaml:"config"`

	State SystemSecurityState `json:"state" yaml:"state"`
}

// SystemSecuritySecureBootCertificate defines a struct that holds information about Secure Boot keys present on the host.
type SystemSecuritySecureBootCertificate struct {
	Type        string `json:"type"        yaml:"type"`
	Fingerprint string `json:"fingerprint" yaml:"fingerprint"`
	Subject     string `json:"subject"     yaml:"subject"`
	Issuer      string `json:"issuer"      yaml:"issuer"`
}

// SystemSecurityEncryptedVolume defines a struct that holds basic information about an encrypted volume.
type SystemSecurityEncryptedVolume struct {
	Volume string `json:"volume" yaml:"volume"`
	State  string `json:"state"  yaml:"state"`
}
