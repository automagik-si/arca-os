package update

import (
	"context"
	"errors"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// restartAfterApply reports whether an OS update restarts the machine at once. Recovery media and a
// fresh install with no primary application restart at once. Otherwise, with automatic updates on,
// the restart waits for the restart window; with them off, auto_reboot decides as before.
func restartAfterApply(ctx context.Context, s *state.State) bool {
	if s.System.TemporaryProvider != nil {
		return true
	}

	if !s.OS.SystemIsReady {
		_, err := applications.GetPrimary(ctx, s, false)
		if errors.Is(err, applications.ErrNoPrimary) {
			return true
		}
	}

	if s.System.Update.Config.AutoUpdatesOff {
		return s.System.Update.Config.AutoReboot || !s.OS.SystemIsReady
	}

	return false
}

// osUpdateHold returns why an offered OS version must not be applied now, or "". A version that
// failed its health check is never applied again automatically; a newer one is. While the running
// version is being checked, the other slot keeps the version to go back to.
func osUpdateHold(s *state.State, version string) string {
	if IsFailedVersion(s, version) {
		return "Version " + version + " failed before (" + s.UpdateHealth.FailedVersions[version] + "); waiting for a newer version"
	}

	b := s.UpdateHealth.Bake
	if b != nil && b.Started != "" && b.Version == s.OS.RunningRelease && version != s.OS.RunningRelease {
		return "Version " + version + " waits until the health check of " + b.Version + " ends"
	}

	return ""
}
