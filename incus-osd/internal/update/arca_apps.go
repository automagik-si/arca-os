package update

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// checkedApplications are the applications whose update is checked and rolled back here. The NVIDIA
// applications keep their own boot-time checks and recovery.
var checkedApplications = []string{"incus", "incus-lts-7.0", "arca"}

// appOps is what an application check uses; tests replace it.
type appOps struct {
	load    func(ctx context.Context, s *state.State, name string) (applications.Application, error)
	refresh func(ctx context.Context, s *state.State) error
	healthy func(ctx context.Context, app applications.Application) string
	save    func(s *state.State) error
	wait    time.Duration
	every   time.Duration
}

func liveAppOps() appOps {
	return appOps{
		load:    applications.Load,
		refresh: applications.RefreshExtensions,
		healthy: applicationHealth,
		save:    func(s *state.State) error { return s.SaveDurable() },
		wait:    time.Minute,
		every:   5 * time.Second,
	}
}

// rememberPreviousApplication records the version an application update replaced.
func rememberPreviousApplication(s *state.State, name string, previous string, version string) {
	if previous == "" || previous == version {
		return
	}

	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	h := &s.UpdateHealth
	if h.PreviousApplications == nil {
		h.PreviousApplications = map[string]string{}
	}

	h.PreviousApplications[name] = previous
}

// applicationHealth returns why an application isn't healthy, or "".
func applicationHealth(ctx context.Context, app applications.Application) string {
	if !app.IsRunning(ctx) {
		return "the " + app.Name() + " application is not running"
	}

	if app.Name() != "arca" {
		fails := incusHealth(ctx, nil)
		if len(fails) > 0 {
			return fails[0]
		}
	}

	return ""
}

// waitHealthy checks app until it is healthy or ops.wait has passed, and returns the last reason.
func waitHealthy(ctx context.Context, ops appOps, app applications.Application) string {
	deadline := time.Now().Add(ops.wait)

	for {
		reason := ops.healthy(ctx, app)
		if reason == "" || !time.Now().Before(deadline) {
			return reason
		}

		select {
		case <-ctx.Done():
			return reason
		case <-time.After(ops.every):
		}
	}
}

// verifyApplication checks an application right after its update. When it isn't healthy, the
// version is marked failed and the application goes back to the version it had before; when that
// doesn't help either, automatic updates stop and the owner is asked.
func verifyApplication(ctx context.Context, s *state.State, ops appOps, name string, version string) {
	if !slices.Contains(checkedApplications, name) {
		return
	}

	app, err := ops.load(ctx, s, name)
	if err != nil {
		return
	}

	// The record is locked only while it changes; the waits and the switch run unlocked.
	reason := waitHealthy(ctx, ops, app)
	if reason == "" {
		withRecord(s, ops.save, func(h *api.SystemUpdateHealth) { h.LastResult = "Application " + name + " updated to " + version })

		return
	}

	previous := ""

	withRecord(s, ops.save, func(h *api.SystemUpdateHealth) {
		h.FailedApplications = addFailed(h.FailedApplications, name+":"+version, reason)
		h.LastReason = reason
		previous = h.PreviousApplications[name]
	})

	slog.ErrorContext(ctx, "Application update failed its health check; going back", "app", name, "version", version, "reason", reason)

	if previous == "" || previous == version {
		lockedPause(ctx, s, ops.save, "application "+name+" version "+version+" failed ("+reason+") and there is no previous version to go back to")

		return
	}

	err = app.SwitchVersion(previous)
	if err == nil {
		err = ops.refresh(ctx, s)
	}

	if err == nil {
		err = app.Restart(ctx)
	}

	if err != nil {
		lockedPause(ctx, s, ops.save, "application "+name+" version "+version+" failed ("+reason+") and going back to "+previous+" failed: "+err.Error())

		return
	}

	after := waitHealthy(ctx, ops, app)
	if after != "" {
		lockedPause(ctx, s, ops.save, "application "+name+" went back to "+previous+" after "+version+" failed, and it is still not healthy: "+after)

		return
	}

	withRecord(s, ops.save, func(h *api.SystemUpdateHealth) {
		h.LastResult = "Application " + name + " version " + version + " failed; went back to " + previous
	})

	slog.WarnContext(ctx, "Application went back to its previous version", "app", name, "version", previous)
}

// withRecord changes the update record with it locked, then saves it.
func withRecord(s *state.State, save func(*state.State) error, change func(h *api.SystemUpdateHealth)) {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	change(&s.UpdateHealth)

	err := save(s)
	if err != nil {
		slog.Warn("Update record could not be saved", "error", err)
	}
}

// lockedPause is pauseForOwner for a caller that doesn't hold the update record.
func lockedPause(ctx context.Context, s *state.State, save func(*state.State) error, reason string) {
	s.LockUpdateHealth()
	defer s.UnlockUpdateHealth()

	pauseForOwner(ctx, s, save, reason)
}

// pauseForOwner stops automatic recovery and pauses automatic updates until the owner acts.
func pauseForOwner(ctx context.Context, s *state.State, save func(*state.State) error, reason string) {
	h := &s.UpdateHealth
	h.NeedsOwner = reason
	h.Bake = nil
	h.NVIDIAWasActive = false
	h.LastResult = "Needs you: " + reason
	s.System.Update.Config.AutoUpdatesOff = true

	slog.ErrorContext(ctx, "Automatic updates paused; needs the owner", "reason", reason)

	err := save(s)
	if err != nil {
		slog.WarnContext(ctx, "Update record could not be saved", "error", err)
	}
}
