package update

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

var errNoSlot = errors.New("version is not on the other slot")

// fakeMachine records what the automatic updater does.
type fakeMachine struct {
	s    *state.State
	path string

	clock      time.Time
	boot       string
	busyReason string
	fails      []string
	instances  []string
	addrs      []string

	started    [][]string
	good, bad  []string
	bootedInto []string
	reboots    int
	bootErr    error
}

func (f *fakeMachine) now() time.Time { return f.clock }

func (f *fakeMachine) bootID() (string, error) { return f.boot, nil }

func (f *fakeMachine) busy(_ context.Context) string { return f.busyReason }

func (f *fakeMachine) health(_ context.Context, _ *api.SystemUpdateHealth) []string { return f.fails }

func (f *fakeMachine) addresses(_ context.Context) []string { return f.addrs }

func (f *fakeMachine) markGood(v string) error { f.good = append(f.good, v); return nil } //nolint:nlreturn // one-line fake

func (f *fakeMachine) markBad(v string) error { f.bad = append(f.bad, v); return nil } //nolint:nlreturn // one-line fake

func (f *fakeMachine) reboot(_ context.Context) error { f.reboots++; return nil } //nolint:nlreturn // one-line fake

func (f *fakeMachine) save() error { return f.s.SaveDurable() }

func (f *fakeMachine) runningInstances(_ context.Context) ([]string, error) {
	return f.instances, nil
}

func (f *fakeMachine) startInstances(_ context.Context, instances []string) {
	f.started = append(f.started, instances)
}

func (f *fakeMachine) bootInto(_ context.Context, v string) error {
	if f.bootErr != nil {
		return f.bootErr
	}

	f.bootedInto = append(f.bootedInto, v)

	return nil
}

// at returns 2026-11-09 (a Monday) at hh:mm; the updater keeps the clock's own zone.
func at(hh int, mm int) time.Time {
	return time.Date(2026, 11, 9, hh, mm, 0, 0, time.UTC)
}

// newAuto returns an updater on a node running "previous" with "version" installed and waiting.
func newAuto(t *testing.T) (*auto, *fakeMachine) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.txt")

	s, err := state.LoadOrCreate(path)
	require.NoError(t, err)

	s.OS.RunningRelease = "202611010000"
	s.OS.NextRelease = "202611020000"
	s.OS.SystemIsReady = true
	armBake(s, "202611020000", "boot-1")

	m := &fakeMachine{s: s, path: path, clock: at(14, 0), boot: "boot-1", instances: []string{"default/web"}, addrs: []string{"192.0.2.10"}}

	return &auto{s: s, m: m}, m
}

// bootInto simulates the machine coming up on version in a new boot.
func bootNew(a *auto, m *fakeMachine, version string, boot string) string {
	m.boot = boot
	a.s.OS.RunningRelease = version

	return a.reconcileBoot(context.Background())
}

func TestRestartWaitsForWindow(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	a.tick(ctx)
	require.Equal(t, 0, m.reboots)
	require.Equal(t, "Update ready; restarts in the window at 03:00", a.s.UpdateHealth.Waiting)
	require.Equal(t, "Update ready; restarts in the window at 03:00", Summarize(a.s, m.clock).Status)

	m.clock = at(3, 30).Add(24 * time.Hour)

	a.tick(ctx)
	require.Equal(t, 1, m.reboots)
	require.Equal(t, []string{"default/web"}, a.s.UpdateHealth.Instances)
	require.Equal(t, []string{"192.0.2.10"}, a.s.UpdateHealth.Addresses)

	// The record that drives the next boot is on disk before the restart.
	saved, err := state.LoadOrCreate(m.path)
	require.NoError(t, err)
	require.Equal(t, []string{"default/web"}, saved.UpdateHealth.Instances)
	require.Equal(t, "202611020000", saved.UpdateHealth.Bake.Version)
}

func TestRestartWindowIsConfigurable(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.System.Update.Config.RestartWindow = &api.SystemUpdateMaintenanceWindow{StartHour: 13, EndHour: 15}

	a.tick(context.Background())
	require.Equal(t, 1, m.reboots)
}

func TestRestartDefersWhileBusy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("busy then idle within two hours", func(t *testing.T) {
		t.Parallel()

		a, m := newAuto(t)
		m.busyReason = "Incus is busy (Migrating instance)"
		m.clock = at(4, 30)

		a.tick(ctx)
		require.Equal(t, 0, m.reboots)
		require.Equal(t, "Update ready; the restart waits: Incus is busy (Migrating instance)", Summarize(a.s, m.clock).Status)

		until, ok := parseStamp(a.s.UpdateHealth.DeferUntil)
		require.True(t, ok)
		require.Equal(t, at(7, 1), until)

		m.clock = at(6, 0)

		a.tick(ctx)
		require.Equal(t, 0, m.reboots)

		m.busyReason = ""
		m.clock = at(6, 30)

		a.tick(ctx)
		require.Equal(t, 1, m.reboots)
	})

	t.Run("still busy two hours after the window", func(t *testing.T) {
		t.Parallel()

		a, m := newAuto(t)
		m.busyReason = "the NVIDIA driver is being changed"
		m.clock = at(3, 0)

		a.tick(ctx)

		m.busyReason = ""
		m.clock = at(7, 30)

		a.tick(ctx)
		require.Equal(t, 0, m.reboots)
		require.Equal(t, "Update ready; restarts in the window at 03:00", a.s.UpdateHealth.Waiting)

		m.clock = at(3, 0).Add(24 * time.Hour)

		a.tick(ctx)
		require.Equal(t, 1, m.reboots)
	})
}

func TestSwitchOffNeverRestarts(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.System.Update.Config.AutoUpdatesOff = true
	m.clock = at(3, 30)

	a.tick(context.Background())
	require.Equal(t, 0, m.reboots)
	require.Equal(t, "Update 202611020000 is installed; restart the machine to use it", Summarize(a.s, m.clock).Status)

	// The scheduled check doesn't run either; only a manual one does.
	err := CheckRespectMaintenanceWindows(context.Background(), a.s)
	require.NoError(t, err)
	require.Equal(t, "Automatic updates are off", a.s.System.Update.State.Status)
}

func TestRestartAfterApply(t *testing.T) {
	t.Parallel()

	s := &state.State{}
	s.OS.SystemIsReady = true
	s.System.Update.Config.AutoReboot = true
	require.False(t, restartAfterApply(context.Background(), s), "with automatic updates on, the restart waits for the window")

	s.System.Update.Config.AutoUpdatesOff = true
	require.True(t, restartAfterApply(context.Background(), s), "with them off, auto_reboot decides")

	s.System.Update.Config.AutoReboot = false
	require.False(t, restartAfterApply(context.Background(), s))

	s.System.TemporaryProvider = &api.SystemProvider{}
	require.True(t, restartAfterApply(context.Background(), s), "recovery media restarts at once")
}

func TestBakePasses(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()
	a.s.UpdateHealth.Instances = []string{"default/web"}

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))
	require.NotEmpty(t, a.s.UpdateHealth.Bake.Started)
	require.Equal(t, "202611010000", a.s.UpdateHealth.PreviousVersion)

	a.ready(ctx)
	require.Empty(t, m.good, "the version is not blessed before its bake")

	for i := 1; i <= 15; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 0, m.reboots)
	require.Nil(t, a.s.UpdateHealth.Bake)
	require.Equal(t, "202611020000", a.s.UpdateHealth.GoodVersion)
	require.Equal(t, []string{"202611020000"}, m.good)
	require.Equal(t, "Version 202611020000 passed its health check and is marked good", a.s.UpdateHealth.LastResult)
	require.NotEmpty(t, m.started, "instances running before the restart are started again")
	require.Nil(t, a.s.UpdateHealth.Instances)
}

func TestBakeConfigurableTime(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.System.Update.Config.BakeTime = "5m"
	bootNew(a, m, "202611020000", "boot-2")

	for i := 1; i <= 5; i++ {
		m.clock = at(14, i)

		a.tick(context.Background())
	}

	require.Equal(t, "202611020000", a.s.UpdateHealth.GoodVersion)
}

func TestBakeThreeStrikesRollsBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	bootNew(a, m, "202611020000", "boot-2")

	m.fails = []string{"the Incus API does not answer"}
	for i := 1; i <= 2; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
		require.Equal(t, 0, m.reboots)
	}

	m.clock = at(14, 3)

	a.tick(ctx)

	h := a.s.UpdateHealth

	require.Equal(t, 1, m.reboots)
	require.Equal(t, []string{"202611020000"}, m.bad)
	require.Equal(t, []string{"202611010000"}, m.bootedInto)
	require.Equal(t, "3 health checks failed, last: the Incus API does not answer", h.FailedVersions["202611020000"])
	require.True(t, h.Bake.Rollback)
	require.Equal(t, "boot-2", h.RollbackBootID)

	// The next boot runs the previous version, which checks healthy.
	m.fails = nil
	require.Empty(t, bootNew(a, m, "202611010000", "boot-3"))
	require.Equal(t, "202611010000", a.s.OS.NextRelease, "the rolled back version is no longer the next release")

	for i := 4; i <= 20; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 1, m.reboots, "exactly one rollback reboot")
	require.Nil(t, a.s.UpdateHealth.Bake)
	require.Equal(t, "Rolled back to 202611010000; it is healthy", a.s.UpdateHealth.LastResult)
	require.Empty(t, a.s.UpdateHealth.NeedsOwner)

	// The failed version is never applied again automatically; a newer one is.
	require.NotEmpty(t, osUpdateHold(a.s, "202611020000"))
	require.Empty(t, osUpdateHold(a.s, "202611030000"))
}

func TestBakeNoPassRollsBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.System.Update.Config.BakeTime = "1m"
	bootNew(a, m, "202611020000", "boot-2")

	m.fails = []string{"instance default/web is not running"}
	m.clock = at(14, 1)

	a.tick(context.Background())

	require.Equal(t, 1, m.reboots)
	require.Equal(t, "no health check passed within the bake: instance default/web is not running", a.s.UpdateHealth.FailedVersions["202611020000"])
}

func TestOneRollbackPerBoot(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	bootNew(a, m, "202611020000", "boot-2")
	a.s.UpdateHealth.RollbackBootID = "boot-2"

	m.fails = []string{"the Incus API does not answer"}
	for i := 1; i <= 3; i++ {
		m.clock = at(14, i)

		a.tick(context.Background())
	}

	require.Equal(t, 0, m.reboots)
	require.Contains(t, a.s.UpdateHealth.NeedsOwner, "a rollback was already tried in this boot")
	require.True(t, a.s.System.Update.Config.AutoUpdatesOff)
}

func TestRollbackThatDoesNotHelpNeedsOwner(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	bootNew(a, m, "202611020000", "boot-2")

	m.fails = []string{"address 192.0.2.10 is missing"}
	for i := 1; i <= 3; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 1, m.reboots)

	bootNew(a, m, "202611010000", "boot-3")

	for i := 4; i <= 6; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 1, m.reboots, "no second rollback")
	require.Equal(t, "after going back to 202611010000, 3 health checks failed, last: address 192.0.2.10 is missing", a.s.UpdateHealth.NeedsOwner)
	require.True(t, a.s.System.Update.Config.AutoUpdatesOff, "automatic updates are paused")
	require.Equal(t, "Needs you: after going back to 202611010000, 3 health checks failed, last: address 192.0.2.10 is missing", Summarize(a.s, m.clock).Status)

	// Turning automatic updates back on is the owner's answer.
	require.NoError(t, SetAutomatic(ctx, a.s, true))
	require.Empty(t, a.s.UpdateHealth.NeedsOwner)
	require.False(t, a.s.System.Update.Config.AutoUpdatesOff)
}

func TestRollbackWithoutTargetNeedsOwner(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	bootNew(a, m, "202611020000", "boot-2")
	m.bootErr = errNoSlot

	m.fails = []string{"the arca application is not running"}
	for i := 1; i <= 3; i++ {
		m.clock = at(14, i)

		a.tick(context.Background())
	}

	require.Equal(t, 0, m.reboots)
	require.Contains(t, a.s.UpdateHealth.NeedsOwner, "could not be selected for the next boot")
}

func TestBootLoaderFallbackMarksFailed(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)

	// The machine restarted, but systemd-boot went back to the previous version.
	require.Empty(t, bootNew(a, m, "202611010000", "boot-2"))
	require.Equal(t, "version 202611020000 did not start; the machine went back to 202611010000", a.s.UpdateHealth.FailedVersions["202611020000"])
	require.Nil(t, a.s.UpdateHealth.Bake)
	require.Equal(t, "202611010000", a.s.OS.NextRelease)
	require.Equal(t, []string{"202611020000"}, NextBootSkips(a.s))
}

func TestPendingSurvivesSameBootRestart(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)

	// incus-osd restarts in the same boot, before the update restart: nothing failed.
	require.Empty(t, bootNew(a, m, "202611010000", "boot-1"))
	require.Empty(t, a.s.UpdateHealth.FailedVersions)
	require.Equal(t, "202611020000", pendingVersion(a.s))
}

func TestDaemonThatNeverStartsRollsBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)

	for range maxStarts {
		require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))
	}

	require.Equal(t, "incus-osd did not finish starting after 3 attempts", bootNew(a, m, "202611020000", "boot-2"))
}

func TestFailedVersionLeftAfterPowerCut(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.UpdateHealth.Bake = nil
	a.s.UpdateHealth.FailedVersions = map[string]string{"202611020000": "3 health checks failed"}

	require.Equal(t, "version 202611020000 failed before: 3 health checks failed", bootNew(a, m, "202611020000", "boot-2"))
}

func TestRetryReleasesFailedVersion(t *testing.T) {
	t.Parallel()

	a, _ := newAuto(t)
	ctx := context.Background()
	a.s.UpdateHealth.FailedVersions = map[string]string{"202611020000": "3 health checks failed"}
	a.s.UpdateHealth.FailedApplications = map[string]string{"arca:202611020000": "the arca application is not running"}

	sum := Summarize(a.s, at(14, 0))
	require.Equal(t, []string{"202611020000: 3 health checks failed", "arca 202611020000: the arca application is not running"}, sum.Failed)

	require.Error(t, RetryVersion(ctx, a.s, "202611090000"))
	require.NoError(t, RetryVersion(ctx, a.s, "202611020000"))
	require.Empty(t, osUpdateHold(a.s, "202611020000"))
	require.NoError(t, RetryVersion(ctx, a.s, "arca:202611020000"))
	require.Empty(t, a.s.UpdateHealth.FailedApplications)
}

func TestWindowHelpers(t *testing.T) {
	t.Parallel()

	w := DefaultRestartWindow
	require.Equal(t, "03:00-05:00", WindowText(w))
	require.Equal(t, at(3, 0).Add(24*time.Hour), NextWindowStart(w, at(14, 0)))
	require.Equal(t, at(4, 10), NextWindowStart(w, at(4, 10)))
	require.Equal(t, at(5, 1), windowEnd(w, at(4, 10)))

	night := api.SystemUpdateMaintenanceWindow{StartHour: 22, EndHour: 2}
	require.Equal(t, at(2, 1).Add(24*time.Hour), windowEnd(night, at(23, 0)))

	s, err := state.LoadOrCreate(filepath.Join(t.TempDir(), "state.txt"))
	require.NoError(t, err)
	require.NoError(t, ShiftRestartWindow(context.Background(), s, -4))
	require.Equal(t, "23:00-01:00", WindowText(RestartWindow(s.System.Update.Config)))

	require.Equal(t, defaultBakeTime, BakeTime(api.SystemUpdateConfig{}))
	require.Equal(t, 5*time.Minute, BakeTime(api.SystemUpdateConfig{BakeTime: "5m"}))
}

func TestHealthRecordPersists(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	bootNew(a, m, "202611020000", "boot-2")
	a.s.UpdateHealth.FailedVersions = map[string]string{"202611000000": "did not start"}
	a.s.UpdateHealth.Instances = []string{"default/web", "other/db"}
	require.NoError(t, a.s.SaveDurable())

	saved, err := state.LoadOrCreate(m.path)
	require.NoError(t, err)
	require.Empty(t, saved.UnrecognizedFields)
	require.Equal(t, a.s.UpdateHealth, saved.UpdateHealth)
	require.True(t, strings.HasPrefix(saved.UpdateHealth.Bake.Started, "2026-11-09T14:00:00"))
}
