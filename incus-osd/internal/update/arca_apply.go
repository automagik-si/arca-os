package update

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
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
	r := s.UpdateHealth.Refused
	if r != nil && r.Version == version && IsFailedVersion(s, version) {
		return r.Message
	}

	if IsFailedVersion(s, version) {
		return "Version " + version + " failed before (" + s.UpdateHealth.FailedVersions[version] + "); waiting for a newer version"
	}

	b := s.UpdateHealth.Bake
	if b != nil && b.Started != "" && b.Version == s.OS.RunningRelease && version != s.OS.RunningRelease {
		return "Version " + version + " waits until the health check of " + b.Version + " ends"
	}

	return ""
}

// rejectOSUpdate records an OS version whose UKI cannot be verified, and reports whether err is one.
// Nothing of it was installed. The version is held like one that failed its health check, so later
// checks neither download nor apply it again; a newer version is applied, and the owner can retry it.
func rejectOSUpdate(ctx context.Context, s *state.State, version string, err error) bool {
	uki, ok := errors.AsType[*util.UKIError](err)
	if !ok {
		return false
	}

	detail := shortError(errors.New(uki.Detail()))

	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	h := &s.UpdateHealth
	h.FailedVersions = addFailed(h.FailedVersions, version, detail)
	util.SetFailedBootVersions(FailedVersions(s))

	h.LastResult = "Version " + version + " was not installed"
	h.LastReason = uki.Error()
	h.Refused = &api.SystemUpdateRefusal{Version: version, Kind: errorKind(uki), Message: uki.Error(), Detail: detail}

	slog.ErrorContext(ctx, "OS update rejected, nothing was installed", "version", version, "reason", detail)

	saveErr := s.SaveDurable()
	if saveErr != nil {
		slog.WarnContext(ctx, "Update record could not be saved", "error", saveErr)
	}

	return true
}

// heldRefusal is the refused OS version to keep showing as the last error: the owner has not asked to
// try it again, and no version as new as it is installed. Versions are timestamps of one length.
func heldRefusal(s *state.State) *api.SystemUpdateRefusal {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	r := s.UpdateHealth.Refused
	if r == nil || !IsFailedVersion(s, r.Version) || s.OS.RunningRelease >= r.Version || s.OS.NextRelease >= r.Version {
		return nil
	}

	return r
}

// checkedStatus is the status after an OS check that installed nothing.
func checkedStatus(s *state.State) string {
	r := heldRefusal(s)
	if r != nil {
		return r.Message
	}

	return "OS update check completed"
}

// dropRejectedDownload removes the files of an OS update that was rejected; a retry downloads them again.
func dropRejectedDownload(ctx context.Context) {
	err := os.RemoveAll(systemd.SystemUpdatesPath)
	if err != nil {
		slog.WarnContext(ctx, "The rejected update's files could not be removed", "error", err)
	}
}
