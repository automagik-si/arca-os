package update

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// Summary is what the console and the API show about automatic updates.
type Summary struct {
	Automatic  bool
	Window     string
	NextWindow time.Time
	Current    string
	Previous   string
	Good       string
	Pending    string
	Failed     []string // "<version>: <reason>", OS versions then applications
	LastCheck  time.Time
	LastResult string
	LastReason string
	NeedsOwner string

	// Status is one line for the screen.
	Status string
}

// Summarize returns the automatic updater's state as of now.
func Summarize(s *state.State, now time.Time) Summary {
	h := s.UpdateHealth
	cfg := s.System.Update.Config
	w := RestartWindow(cfg)

	sum := Summary{
		Automatic:  !cfg.AutoUpdatesOff,
		Window:     WindowText(w),
		NextWindow: NextWindowStart(w, now),
		Current:    s.OS.RunningRelease,
		Previous:   h.PreviousVersion,
		Good:       h.GoodVersion,
		Pending:    pendingVersion(s),
		LastCheck:  s.System.Update.State.LastCheck,
		LastResult: h.LastResult,
		LastReason: h.LastReason,
		NeedsOwner: h.NeedsOwner,
	}

	for _, v := range FailedVersions(s) {
		sum.Failed = append(sum.Failed, v+": "+h.FailedVersions[v])
	}

	apps := make([]string, 0, len(h.FailedApplications))
	for k, reason := range h.FailedApplications {
		apps = append(apps, strings.Replace(k, ":", " ", 1)+": "+reason)
	}

	slices.Sort(apps)
	sum.Failed = append(sum.Failed, apps...)

	b := h.Bake

	switch {
	case h.NeedsOwner != "":
		sum.Status = "Needs you: " + h.NeedsOwner
	case b != nil && b.Started != "" && b.Version == s.OS.RunningRelease && b.Rollback:
		sum.Status = "Went back to " + b.Version + "; checking its health"
	case b != nil && b.Started != "" && b.Version == s.OS.RunningRelease:
		sum.Status = "Checking version " + b.Version + " before marking it good"
		if b.LastFailure != "" {
			sum.Status += " (last check: " + b.LastFailure + ")"
		}
	case sum.Pending != "" && h.Waiting != "":
		sum.Status = h.Waiting
	case sum.Pending != "" && sum.Automatic:
		sum.Status = "Update ready; restarts in the window at " + clock(w.StartHour, w.StartMinute)
	case sum.Pending != "":
		sum.Status = "Update " + sum.Pending + " is installed; restart the machine to use it"
	case !sum.Automatic:
		sum.Status = "Automatic updates are off"
	case h.LastResult != "":
		sum.Status = h.LastResult
	default:
		sum.Status = "Up to date"
	}

	return sum
}

// Health returns the record the API shows, with the values worked out now.
func Health(s *state.State, now time.Time) *api.SystemUpdateHealth {
	h := s.UpdateHealth
	h.CurrentVersion = s.OS.RunningRelease
	h.PendingVersion = pendingVersion(s)
	h.NextWindow = stamp(NextWindowStart(RestartWindow(s.System.Update.Config), now))

	if h.Bake != nil {
		b := *h.Bake
		h.Bake = &b
	}

	return &h
}

// SetAutomatic turns automatic updates on or off. Turning them on clears a "Needs you" pause.
func SetAutomatic(ctx context.Context, s *state.State, on bool) error {
	s.System.Update.Config.AutoUpdatesOff = !on
	if on {
		s.UpdateHealth.NeedsOwner = ""
	}

	slog.InfoContext(ctx, "Automatic updates switched by the owner", "on", on)

	return s.SaveDurable()
}

// ShiftRestartWindow moves the restart window by the given number of hours.
func ShiftRestartWindow(ctx context.Context, s *state.State, hours int) error {
	w := RestartWindow(s.System.Update.Config)
	w.StartHour = ((w.StartHour+hours)%24 + 24) % 24
	w.EndHour = ((w.EndHour+hours)%24 + 24) % 24
	s.System.Update.Config.RestartWindow = &w

	slog.InfoContext(ctx, "Restart window changed by the owner", "window", WindowText(w))

	return s.SaveDurable()
}

// RetryVersion lets a failed OS or application version be applied again, then checks for updates.
// An application version is given as "<application>:<version>".
func RetryVersion(ctx context.Context, s *state.State, version string) error {
	h := &s.UpdateHealth

	_, isOS := h.FailedVersions[version]
	_, isApp := h.FailedApplications[version]

	if !isOS && !isApp {
		return errors.New("version " + version + " is not marked failed")
	}

	if isOS {
		delete(h.FailedVersions, version)

		err := util.ResetUKITries(version)
		if err != nil {
			slog.DebugContext(ctx, "No local boot entry to reset", "version", version, "error", err)
		}
	} else {
		delete(h.FailedApplications, version)
	}

	h.LastResult = "The owner asked to try " + version + " again"

	slog.InfoContext(ctx, "Failed version released by the owner", "version", version)

	err := s.SaveDurable()
	if err != nil {
		return err
	}

	CheckNow(s)

	return nil
}

// CheckNow asks the daemon for an update check, without waiting for it.
func CheckNow(s *state.State) {
	if s.TriggerUpdate == nil {
		return
	}

	select {
	case s.TriggerUpdate <- true:
	default:
	}
}
