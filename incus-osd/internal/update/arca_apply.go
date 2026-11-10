package update

import (
	"context"
	"errors"
	"log/slog"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// automaticUpdatesOff reports whether the owner turned automatic updates off; only a manual check runs then.
func automaticUpdatesOff(ctx context.Context, s *state.State) bool {
	if !s.System.Update.Config.AutoUpdatesOff {
		return false
	}

	s.System.Update.State.Status = "Automatic updates are off"
	slog.DebugContext(ctx, s.System.Update.State.Status)

	return true
}

// installedVersion returns an application's installed version, or "".
func installedVersion(ctx context.Context, s *state.State, name string) string {
	app, err := applications.Load(ctx, s, name)
	if err != nil || !app.IsInstalled() {
		return ""
	}

	return app.Version()
}

// osUpdateHeld reports whether an offered OS version must not be applied now, and says why.
func osUpdateHeld(ctx context.Context, s *state.State, version string) bool {
	s.LockUpdateHealth()
	hold := osUpdateHold(s, version)
	s.UnlockUpdateHealth()

	if hold == "" {
		return false
	}

	s.System.Update.State.Status = hold
	slog.WarnContext(ctx, hold, "version", version)

	return true
}

// applicationHeld reports whether an offered application version failed before and is skipped.
func applicationHeld(ctx context.Context, s *state.State, name string, version string, installed string) bool {
	s.LockUpdateHealth()
	reason, failed := s.UpdateHealth.FailedApplications[name+":"+version]
	s.UnlockUpdateHealth()

	if !failed || version == installed {
		return false
	}

	slog.WarnContext(ctx, "Skipping an application version that failed before", "app", name, "version", version, "reason", reason)

	return true
}

// armOSBake arms the health check of an OS version that was just installed. When the boot it was
// installed from can't be identified, that counts as a failed check.
func armOSBake(ctx context.Context, s *state.State, version string) {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	id, err := (&liveMachine{s: s}).bootID()
	armBake(s, version, id)

	if err != nil {
		s.UpdateHealth.Bake.Strikes = 1
		s.UpdateHealth.Bake.LastFailure = "the boot the update was installed from could not be identified"
		slog.WarnContext(ctx, "Update health check failed", "version", version, "strike", 1, "reason", s.UpdateHealth.Bake.LastFailure)
	}

	err = s.SaveDurable()
	if err != nil {
		slog.WarnContext(ctx, "Update record could not be saved", "error", err)
	}
}

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
