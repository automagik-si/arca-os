package console

import (
	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// Seal is the TPM re-seal after this boot's recovery unlock: its status and, when it
// failed, the reason. Both are empty when the boot unlocked with the TPM.
type Seal struct {
	Status, Reason string
}

// liveSeal reads the re-seal from the security state, only when this boot used the recovery passphrase.
func liveSeal(s *state.State) Seal {
	sec := s.System.Security.State
	if !sec.RecoveryUnlocked() {
		return Seal{}
	}

	return Seal{Status: sec.ResealStatus, Reason: sec.ResealReason}
}

// readSeal reads the re-seal into the snapshot.
func (c *console) readSeal() {
	if c.src.seal == nil {
		return
	}

	seal := c.src.seal()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.snap.Seal = seal
}

// sealAlert is the alert line's tag and text for seal, the owner's full sentence when it
// fits in n cells and a shorter one otherwise, or "" when there is nothing to say.
func sealAlert(seal Seal, n int) (string, string) {
	tag, mark, short := tagOK, "✓ ", "Unlocked with the recovery passphrase; the TPM is re-sealed to this system"

	switch seal.Status {
	case api.ResealStatusResealed:
	case api.ResealStatusFailed:
		tag, mark = tagBad, "✗ "
		short = "TPM not re-sealed (passphrase asked again): " + seal.Reason
	default:
		return "", ""
	}

	full := mark + api.ResealSentence(seal.Status, seal.Reason)
	if width(full) <= n {
		return tag, full
	}

	return tag, mark + short
}
