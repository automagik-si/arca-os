package console

import (
	"strings"
	"testing"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

const sealReason = "command failed: exit status 1"

func sealSnapshot(seal Seal) Snapshot {
	snap := tracerSnapshot()
	snap.Seal = seal

	return snap
}

func TestSealGoldens(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		seal Seal
		w, h int
		want string
	}{
		{
			"reseal-ok-80x25.txt",
			Seal{Status: api.ResealStatusResealed},
			80, 25,
			"✓ Unlocked with the recovery passphrase; the TPM is re-sealed to this system",
		},
		{
			"reseal-failed-80x25.txt",
			Seal{Status: api.ResealStatusFailed, Reason: sealReason},
			80, 25,
			"✗ TPM not re-sealed (passphrase asked again): " + sealReason,
		},
		{
			"reseal-ok-120x36.txt",
			Seal{Status: api.ResealStatusResealed},
			120, 36,
			"✓ The disk was unlocked with the recovery passphrase; the TPM has been re-sealed to this system's current state",
		},
		{
			"reseal-failed-160x45.txt",
			Seal{Status: api.ResealStatusFailed, Reason: sealReason},
			160, 45,
			"✗ TPM not re-sealed (passphrase asked again): " + sealReason,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			screen := draw(t, sealSnapshot(tc.seal), tc.w, tc.h)

			if rowOf(screen, tc.want) < 0 {
				t.Fatalf("no %q on the screen:\n%s", tc.want, screenText(screen))
			}

			if rowOf(screen, "No alerts") >= 0 {
				t.Fatal("the alert line still says there is no alert")
			}

			golden(t, tc.name, screen)
		})
	}
}

func TestSealNothingOnTPMBoot(t *testing.T) {
	t.Parallel()

	s := &state.State{}
	s.System.Security.State.EncryptedVolumes = []api.SystemSecurityEncryptedVolume{
		{Volume: "root", State: api.EncryptedVolumeUnlockedTPM},
		{Volume: "swap", State: api.EncryptedVolumeUnlockedTPM},
	}

	// A re-seal result from an earlier recovery boot is not this boot's.
	s.System.Security.State.LastRecoveryUnlock = "2026-11-01T10:00:00Z"
	s.System.Security.State.ResealStatus = api.ResealStatusFailed
	s.System.Security.State.ResealReason = sealReason

	seal := liveSeal(s)
	if seal != (Seal{}) {
		t.Fatalf("a TPM boot reads a re-seal: %+v", seal)
	}

	screen := draw(t, sealSnapshot(seal), 80, 25)
	if rowOf(screen, "✓ No alerts") < 0 || rowOf(screen, "re-seal") >= 0 || rowOf(screen, "recovery passphrase") >= 0 {
		t.Fatalf("a TPM boot shows a re-seal line:\n%s", screenText(screen))
	}

	// The same state after a recovery unlock reads the result.
	s.System.Security.State.EncryptedVolumes[0].State = api.EncryptedVolumeUnlockedRecovery
	s.System.Security.State.EncryptedVolumes[1].State = api.EncryptedVolumeUnlockedRecovery

	seal = liveSeal(s)
	if seal != (Seal{Status: api.ResealStatusFailed, Reason: sealReason}) {
		t.Fatalf("a recovery boot reads %+v", seal)
	}
}

func TestSealAlertOrder(t *testing.T) {
	t.Parallel()

	// A failed re-seal comes before an update that needs the owner.
	snap := sealSnapshot(Seal{Status: api.ResealStatusFailed, Reason: sealReason})
	snap.Updates = needsYou()

	alertRow := chooseLayout(80, 25).Alert.Y

	line := screenLine(draw(t, snap, 80, 25), alertRow)
	if !strings.HasPrefix(line, " ✗ TPM not re-sealed") {
		t.Fatalf("alert line with a failed re-seal and an update: %q", line)
	}

	// An update that needs the owner comes before a successful re-seal.
	snap.Seal = Seal{Status: api.ResealStatusResealed}

	line = screenLine(draw(t, snap, 80, 25), alertRow)
	if !strings.HasPrefix(line, " ✗ Needs you") {
		t.Fatalf("alert line with a re-seal and an update: %q", line)
	}
}
