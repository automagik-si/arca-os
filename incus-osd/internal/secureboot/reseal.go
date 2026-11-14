package secureboot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// resealMarker exists once this boot's re-seal ran; /run is empty again on the next boot.
var resealMarker = "/run/incus-os/tpm-resealed"

// What the re-seal calls; tests replace them.
var (
	resealSecureBootEnabled = Enabled
	resealUKIVersions       = util.GetUKIVersions
	resealPCR4Binding       = UpdatePCR4Binding
	resealPCR7Binding       = UpdatePCR7Binding
	resealNow               = time.Now
)

// resealReasonMax is the longest reseal_reason kept in the security state.
const resealReasonMax = 160

// ResealAfterRecoveryUnlock re-seals the TPM to this boot's state when the volumes were unlocked
// with the recovery passphrase, once per boot, and records the result in the security state.
// A failure is recorded and logged, never returned: the node keeps running. It reports whether
// a re-seal was attempted.
func ResealAfterRecoveryUnlock(ctx context.Context, s *state.State) bool {
	sec := &s.System.Security.State

	if !sec.RecoveryUnlocked() {
		return false
	}

	_, err := os.Stat(resealMarker)
	if err == nil {
		return false
	}

	// Mark the boot first, so a crash during the re-seal does not repeat it.
	err = os.MkdirAll(filepath.Dir(resealMarker), 0o700)
	if err == nil {
		err = os.WriteFile(resealMarker, nil, 0o600)
	}

	if err != nil {
		slog.WarnContext(ctx, "TPM re-seal could not be marked for this boot", "err", err)
	}

	sec.LastRecoveryUnlock = resealNow().UTC().Format(time.RFC3339)
	sec.ResealStatus = ""
	sec.ResealReason = ""

	slog.InfoContext(ctx, "The disk was unlocked with the recovery passphrase, re-sealing the TPM to this system's current state")

	err = reseal(ctx, s)
	if err != nil {
		sec.ResealStatus = api.ResealStatusFailed
		sec.ResealReason = resealReason(err)
		slog.WarnContext(ctx, "TPM re-seal failed, the recovery passphrase will be asked again next boot", "err", err)
	} else {
		sec.ResealStatus = api.ResealStatusResealed

		slog.InfoContext(ctx, "TPM re-sealed, the next boot unlocks with the TPM")
	}

	err = s.Save()
	if err != nil {
		slog.WarnContext(ctx, "TPM re-seal result could not be saved", "err", err)
	}

	return true
}

// reseal binds the TPM token to what boots next: the current PCR7 with Secure Boot enabled,
// and otherwise the PCR4 of the UKI that boots next, with PCR7 and PCR12.
func reseal(ctx context.Context, s *state.State) error {
	sbEnabled, err := resealSecureBootEnabled()
	if err != nil {
		return err
	}

	if sbEnabled {
		return resealPCR7Binding(ctx)
	}

	ukis, err := resealUKIVersions()
	if err != nil {
		return err
	}

	// Booted from the backup image, the newest one boots next.
	uki := ukis.CurrentFilepath
	if s.OS.RunningFromBackup() && ukis.OtherFilepath != "" {
		uki = ukis.OtherFilepath
	}

	return resealPCR4Binding(ctx, uki)
}

// resealReason is err as one short line, without the command line of a failed command.
func resealReason(err error) string {
	msg := err.Error()

	runErr, ok := errors.AsType[subprocess.RunError](err)
	if ok {
		msg = strings.Replace(msg, runErr.Error(), "command failed: "+runErr.Unwrap().Error(), 1)
	}

	msg, _, _ = strings.Cut(msg, "\n")

	if len(msg) > resealReasonMax {
		msg = strings.ToValidUTF8(msg[:resealReasonMax-len("…")], "") + "…"
	}

	return msg
}
