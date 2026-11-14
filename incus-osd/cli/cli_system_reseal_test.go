package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
)

func recoveryVolumes(st string) []api.SystemSecurityEncryptedVolume {
	return []api.SystemSecurityEncryptedVolume{{Volume: "root", State: st}, {Volume: "swap", State: st}}
}

func TestResealFieldsExposed(t *testing.T) {
	t.Parallel()

	sec := api.SystemSecurity{State: api.SystemSecurityState{
		EncryptedVolumes:   recoveryVolumes(api.EncryptedVolumeUnlockedRecovery),
		LastRecoveryUnlock: "2026-11-14T12:30:00Z",
		ResealStatus:       api.ResealStatusFailed,
		ResealReason:       "command failed: exit status 1",
	}}

	data, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"last_recovery_unlock":"2026-11-14T12:30:00Z"`,
		`"reseal_status":"reseal failed"`,
		`"reseal_reason":"command failed: exit status 1"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("no %s in %s", want, data)
		}
	}

	// A node that never needed the passphrase shows none of them.
	data, err = json.Marshal(api.SystemSecurity{})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "recovery_unlock") || strings.Contains(string(data), "reseal") {
		t.Fatalf("empty re-seal fields are shown: %s", data)
	}
}

func TestResealNote(t *testing.T) {
	t.Parallel()

	st := api.SystemSecurityState{
		EncryptedVolumes:   recoveryVolumes(api.EncryptedVolumeUnlockedRecovery),
		LastRecoveryUnlock: "2026-11-14T12:30:00Z",
		ResealStatus:       api.ResealStatusResealed,
	}

	got := resealNote(st)
	if got != "\nRecovery unlock (2026-11-14T12:30:00Z): The disk was unlocked with the recovery passphrase; the TPM has been re-sealed to this system's current state.\n" {
		t.Fatalf("re-sealed note: %q", got)
	}

	st.ResealStatus, st.ResealReason = api.ResealStatusFailed, "command failed: exit status 1"

	got = resealNote(st)
	if !strings.Contains(got, "the TPM could not be re-sealed: command failed: exit status 1; the passphrase will be asked again next boot.") {
		t.Fatalf("failed note: %q", got)
	}

	// A TPM boot says nothing, whatever an earlier recovery boot recorded.
	st.EncryptedVolumes = recoveryVolumes(api.EncryptedVolumeUnlockedTPM)
	if got := resealNote(st); got != "" {
		t.Fatalf("TPM boot note: %q", got)
	}
}
