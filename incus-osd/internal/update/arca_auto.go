package update

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// The automatic updater's limits.
const (
	defaultBakeTime = 15 * time.Minute
	maxStrikes      = 3
	maxStarts       = 3
	deferGrace      = 2 * time.Hour
	tickEvery       = time.Minute
)

// DefaultRestartWindow is the restart window used when none is configured: 03:00 to 05:00 local time.
var DefaultRestartWindow = api.SystemUpdateMaintenanceWindow{StartHour: 3, EndHour: 5}

// machine is what the automatic updater reads from and does to the node; tests replace it.
type machine interface {
	probe
	bootLoader
}

// probe reads the node and its instances.
type probe interface {
	now() time.Time
	bootID() (string, error)
	// busy returns why a restart should wait, in plain words, or "".
	busy(ctx context.Context) string
	// health returns the checks that fail, in plain words.
	health(ctx context.Context, h *api.SystemUpdateHealth) []string
	runningInstances(ctx context.Context) ([]string, error)
	startInstances(ctx context.Context, instances []string)
	addresses(ctx context.Context) []string
	save() error
}

// bootLoader changes boot entries and restarts the node.
type bootLoader interface {
	markGood(version string) error
	markBad(version string) error
	// bootInto makes the next boot start version, which must be on the other slot.
	bootInto(ctx context.Context, version string) error
	reboot(ctx context.Context) error
}

// auto drives the OS update life cycle: restart in the window, health bake and rollback.
type auto struct {
	s *state.State
	m machine

	waitLogged string
}

// RestartWindow returns the configured restart window, or the default one.
func RestartWindow(c api.SystemUpdateConfig) api.SystemUpdateMaintenanceWindow {
	if c.RestartWindow != nil {
		return *c.RestartWindow
	}

	return DefaultRestartWindow
}

// BakeTime returns how long a new OS version is checked before it is marked good.
func BakeTime(c api.SystemUpdateConfig) time.Duration {
	d, err := time.ParseDuration(c.BakeTime)
	if err != nil || d < time.Minute {
		return defaultBakeTime
	}

	return d
}

// NextWindowStart returns when the restart window next opens; now itself when it is open.
func NextWindowStart(w api.SystemUpdateMaintenanceWindow, now time.Time) time.Time {
	return now.Truncate(time.Minute).Add(w.TimeUntilActiveReference(now))
}

// windowEnd returns the first minute after the window that is open at now.
func windowEnd(w api.SystemUpdateMaintenanceWindow, now time.Time) time.Time {
	t := now.Truncate(time.Minute)

	for range 8 * 24 * 60 {
		if !w.IsActive(t) {
			break
		}

		t = t.Add(time.Minute)
	}

	return t
}

// WindowText returns the window as "03:00-05:00".
func WindowText(w api.SystemUpdateMaintenanceWindow) string {
	return clock(w.StartHour, w.StartMinute) + "-" + clock(w.EndHour, w.EndMinute)
}

func clock(h int, m int) string {
	return twoDigits(h) + ":" + twoDigits(m)
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}

	return strconv.Itoa(n)
}

func stamp(t time.Time) string {
	return t.Format(time.RFC3339)
}

func parseStamp(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, v)

	return t, err == nil
}

// IsFailedVersion reports whether an OS version failed before and must not be applied automatically.
func IsFailedVersion(s *state.State, version string) bool {
	_, ok := s.UpdateHealth.FailedVersions[version]

	return ok
}

// FailedVersions returns the failed OS versions, sorted.
func FailedVersions(s *state.State) []string {
	return slices.Sorted(maps.Keys(s.UpdateHealth.FailedVersions))
}

// armBake records an OS version that was just installed and waits for its restart.
func armBake(s *state.State, version string, bootID string) {
	h := &s.UpdateHealth
	h.Bake = &api.SystemUpdateBake{Version: version, Previous: s.OS.RunningRelease, StagedBootID: bootID}
	h.PendingVersion = version
	h.LastResult = "Version " + version + " is installed and waits for its restart"
	h.LastReason = ""
}

// reconcileBoot updates the record once per daemon start, before the next boot is chosen.
// It returns a reason when this boot must go back to the previous version at once.
func (a *auto) reconcileBoot(ctx context.Context) string {
	h := &a.s.UpdateHealth
	running := a.s.OS.RunningRelease
	h.CurrentVersion = running

	id, idErr := a.m.bootID()

	b := h.Bake
	if b != nil && !b.Rollback && running == b.Version {
		if b.Started == "" {
			b.Started = stamp(a.m.now())
			h.PendingVersion = ""
			h.PreviousVersion = b.Previous
			h.LastResult = "Version " + running + " started; checking its health"
			slog.InfoContext(ctx, "Update health check started", "version", running, "previous", b.Previous)
		}

		if idErr == nil {
			if b.StartsBootID == id {
				b.Starts++
			} else {
				b.Starts, b.StartsBootID = 1, id
			}
		}

		if b.Starts > maxStarts {
			return "incus-osd did not finish starting after " + strconv.Itoa(maxStarts) + " attempts"
		}
	}

	if b != nil && !b.Rollback && running != b.Version && idErr == nil && b.StagedBootID != "" && b.StagedBootID != id {
		// The machine restarted after the version was installed, but isn't running it.
		reason := "version " + b.Version + " did not start; the machine went back to " + running
		if b.Started != "" {
			reason = "version " + b.Version + " stopped during its health check; the machine went back to " + running
		}

		a.failed(b.Version, reason)

		h.Bake = nil
		h.PendingVersion = ""
		h.LastResult = "Rolled back to " + running
		h.LastReason = reason
		a.s.OS.NextRelease = running

		slog.WarnContext(ctx, "Update rolled back by the boot loader", "version", b.Version, "reason", reason)
	}

	if b != nil && b.Rollback {
		if running == b.Version {
			if b.Started == "" {
				b.Started = stamp(a.m.now())
			}
		} else {
			a.needsOwner(ctx, "the rollback to "+b.Version+" did not start; the machine runs "+running)
		}
	}

	// Leave a rolled back or failed next release, so the machine doesn't run as a "backup" boot.
	if a.s.OS.NextRelease != running && (IsFailedVersion(a.s, a.s.OS.NextRelease) || (h.Bake != nil && h.Bake.Rollback)) {
		a.s.OS.NextRelease = running
	}

	if h.Bake == nil && IsFailedVersion(a.s, running) && h.NeedsOwner == "" {
		return "version " + running + " failed before: " + h.FailedVersions[running]
	}

	err := a.m.save()
	if err != nil {
		slog.WarnContext(ctx, "Update record could not be saved", "error", err)
	}

	return ""
}

// ready runs once the system is ready: it blesses a boot that isn't being checked.
func (a *auto) ready(ctx context.Context) {
	h := &a.s.UpdateHealth
	running := a.s.OS.RunningRelease

	if h.Bake == nil || h.Bake.Version != running || h.Bake.Rollback {
		err := a.m.markGood(running)
		if err != nil {
			slog.DebugContext(ctx, "Boot entry not blessed", "version", running, "error", err)
		}
	}

	if h.Bake == nil && len(h.Instances) > 0 {
		a.m.startInstances(ctx, h.Instances)
	}
}

// tick runs one round: the health bake, then a pending restart.
func (a *auto) tick(ctx context.Context) {
	h := &a.s.UpdateHealth
	h.CurrentVersion = a.s.OS.RunningRelease

	if h.Bake != nil && h.Bake.Started != "" && h.Bake.Version == a.s.OS.RunningRelease {
		a.bake(ctx)

		return
	}

	a.restart(ctx)
}

// bake runs one health check of the running version.
func (a *auto) bake(ctx context.Context) {
	h := &a.s.UpdateHealth
	b := h.Bake
	now := a.m.now()

	if len(h.Instances) > 0 {
		a.m.startInstances(ctx, h.Instances)
	}

	fails := a.m.health(ctx, h)
	if len(fails) > 0 {
		b.Strikes++
		b.LastFailure = strings.Join(fails, "; ")
		slog.WarnContext(ctx, "Update health check failed", "version", b.Version, "strike", b.Strikes, "reason", b.LastFailure)
	} else {
		b.Passes++
		b.LastFailure = ""
	}

	started, ok := parseStamp(b.Started)
	over := !ok || now.Sub(started) >= BakeTime(a.s.System.Update.Config)

	switch {
	case b.Strikes >= maxStrikes:
		a.bakeFailed(ctx, strconv.Itoa(maxStrikes)+" health checks failed, last: "+b.LastFailure)
	case over && b.Passes == 0:
		a.bakeFailed(ctx, "no health check passed within the bake: "+b.LastFailure)
	case over && len(fails) == 0:
		a.bakePassed(ctx)
	default:
		err := a.m.save()
		if err != nil {
			slog.WarnContext(ctx, "Update record could not be saved", "error", err)
		}
	}
}

func (a *auto) bakePassed(ctx context.Context) {
	h := &a.s.UpdateHealth
	b := h.Bake

	if b.Rollback {
		h.LastResult = "Rolled back to " + b.Version + "; it is healthy"
		slog.InfoContext(ctx, "Rollback health check passed", "version", b.Version)
	} else {
		err := a.m.markGood(b.Version)
		if err != nil {
			slog.WarnContext(ctx, "Boot entry could not be blessed", "version", b.Version, "error", err)
		}

		h.PreviousVersion = b.Previous
		h.LastResult = "Version " + b.Version + " passed its health check and is marked good"
		h.LastReason = ""

		slog.InfoContext(ctx, "Update health check passed; version marked good", "version", b.Version)
	}

	h.GoodVersion = b.Version
	h.Bake = nil
	h.Instances, h.Addresses = nil, nil

	err := a.m.save()
	if err != nil {
		slog.WarnContext(ctx, "Update record could not be saved", "error", err)
	}
}

func (a *auto) bakeFailed(ctx context.Context, reason string) {
	b := a.s.UpdateHealth.Bake
	if b.Rollback {
		a.needsOwner(ctx, "after going back to "+b.Version+", "+reason)

		return
	}

	a.rollback(ctx, b.Version, b.Previous, reason)
}

// rollback marks version failed and restarts once into previous.
func (a *auto) rollback(ctx context.Context, version string, previous string, reason string) {
	h := &a.s.UpdateHealth

	a.failed(version, reason)
	h.LastReason = reason

	if previous == "" || previous == version || IsFailedVersion(a.s, previous) {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and there is no good version to go back to")

		return
	}

	id, err := a.m.bootID()
	if err != nil {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and this boot could not be identified")

		return
	}

	if h.RollbackBootID == id {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and a rollback was already tried in this boot")

		return
	}

	h.RollbackBootID = id
	h.Bake = &api.SystemUpdateBake{Version: previous, Previous: version, Rollback: true}
	h.LastResult = "Version " + version + " failed; going back to " + previous

	err = a.m.save()
	if err != nil {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and the rollback could not be recorded")

		return
	}

	slog.ErrorContext(ctx, "Update failed its health check; rolling back", "version", version, "previous", previous, "reason", reason)

	err = a.m.markBad(version)
	if err != nil {
		slog.WarnContext(ctx, "Boot entry could not be marked bad", "version", version, "error", err)
	}

	err = a.m.bootInto(ctx, previous)
	if err != nil {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and "+previous+" could not be selected for the next boot: "+err.Error())

		return
	}

	err = a.m.reboot(ctx)
	if err != nil {
		a.needsOwner(ctx, "version "+version+" failed ("+reason+") and the restart into "+previous+" failed: "+err.Error())
	}
}

func (a *auto) failed(version string, reason string) {
	h := &a.s.UpdateHealth
	if h.FailedVersions == nil {
		h.FailedVersions = map[string]string{}
	}

	h.FailedVersions[version] = reason
}

// needsOwner stops automatic recovery and pauses automatic updates until the owner acts.
func (a *auto) needsOwner(ctx context.Context, reason string) {
	pauseForOwner(ctx, a.s, func(*state.State) error { return a.m.save() }, reason)
}

// pendingVersion returns the installed OS version that waits for its restart, or "".
func pendingVersion(s *state.State) string {
	b := s.UpdateHealth.Bake
	if b == nil || b.Rollback || b.Started != "" || b.Version == s.OS.RunningRelease || IsFailedVersion(s, b.Version) {
		return ""
	}

	return b.Version
}

// restart restarts into a pending OS version inside the restart window, unless the machine is busy.
func (a *auto) restart(ctx context.Context) {
	h := &a.s.UpdateHealth
	cfg := a.s.System.Update.Config
	now := a.m.now()

	pending := pendingVersion(a.s)
	h.PendingVersion = pending

	if pending == "" {
		h.Waiting, h.DeferUntil = "", ""

		return
	}

	if cfg.AutoUpdatesOff || h.NeedsOwner != "" {
		h.Waiting = "Update " + pending + " is installed; restart the machine to use it"

		return
	}

	w := RestartWindow(cfg)
	until, deferred := parseStamp(h.DeferUntil)

	if !w.IsActive(now) && (!deferred || now.After(until)) {
		h.DeferUntil = ""
		h.Waiting = "Update ready; restarts in the window at " + clock(w.StartHour, w.StartMinute)

		return
	}

	reason := a.m.busy(ctx)
	if reason != "" {
		if !deferred {
			h.DeferUntil = stamp(windowEnd(w, now).Add(deferGrace))
		}

		h.Waiting = "Update ready; the restart waits: " + reason
		if a.waitLogged != reason {
			a.waitLogged = reason
			slog.InfoContext(ctx, "Update restart deferred", "version", pending, "reason", reason)
		}

		return
	}

	instances, err := a.m.runningInstances(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Running instances could not be listed before the update restart", "error", err)
	}

	h.Instances = instances
	h.Addresses = a.m.addresses(ctx)
	h.Waiting, h.DeferUntil = "", ""
	h.LastResult = "Restarting into version " + pending

	err = a.m.save()
	if err != nil {
		slog.WarnContext(ctx, "Update record could not be saved; not restarting", "error", err)

		return
	}

	slog.InfoContext(ctx, "Restarting into the update", "version", pending, "instances", len(instances))

	err = a.m.reboot(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Update restart failed", "error", err)
	}
}
