package update

import (
	"context"
	"errors"
	"fmt"
	"testing"

	incusapi "github.com/lxc/incus/v7/shared/api"
	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestCrashRebootsDuringBakeRollBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	for i := 1; i <= 10; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	// The first unexpected restart is a failed check, and the bake clock starts again.
	m.clock = at(14, 16)

	require.Empty(t, crashBoot(a, m, "202611020000", "boot-3"))

	b := a.s.UpdateHealth.Bake
	require.Equal(t, 1, b.Unplanned)
	require.Equal(t, 1, b.Strikes)
	require.Equal(t, 2, b.Boots)
	require.Equal(t, stamp(at(14, 16)), b.BootStarted)

	m.clock = at(14, 20)

	a.tick(ctx)
	require.Empty(t, m.good)

	// The second one rolls back from the boot record, before the version goes on starting.
	m.clock = at(14, 21)

	reason := crashBoot(a, m, "202611020000", "boot-4")
	require.Equal(t, "the machine restarted unexpectedly 2 times during the health check", reason)
	require.Empty(t, m.good)

	// The rollback restart is a planned one, though it runs before the shutdown sequence exists.
	a.rollback(ctx, "202611020000", "202611010000", reason)
	require.Equal(t, 1, m.reboots)
	require.Empty(t, crashBoot(a, m, "202611010000", "boot-5"))
	require.False(t, a.s.UpdateHealth.UnplannedBoot)
}

func TestPlannedRestartRestartsBakeClock(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	for i := 1; i <= 10; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	// A clean restart is no failed check, but the version must pass a whole bake in the new boot.
	require.Empty(t, bootNew(a, m, "202611020000", "boot-3"))
	require.Zero(t, a.s.UpdateHealth.Bake.Strikes)

	m.clock = at(14, 16)

	a.tick(ctx)
	require.NotNil(t, a.s.UpdateHealth.Bake)

	m.clock = at(14, 26)

	a.tick(ctx)
	require.Nil(t, a.s.UpdateHealth.Bake)
	require.Equal(t, []string{"202611020000"}, m.good)
}

func TestRestartLoopRollsBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)

	// Clean restarts that never let a bake finish end too.
	for i := 2; i <= 6; i++ {
		require.Empty(t, bootNew(a, m, "202611020000", "boot-"+itoa2(i)))
	}

	require.Equal(t, "the machine restarted 5 times before the health check could pass", bootNew(a, m, "202611020000", "boot-07"))
}

func TestNoBlessingAfterUnplannedBoot(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))
	require.Empty(t, crashBoot(a, m, "202611020000", "boot-3"))

	a.ready(ctx)

	for i := 1; i <= 16; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	// The version passed, but the boot that followed a crash leaves its entry unblessed.
	require.Nil(t, a.s.UpdateHealth.Bake)
	require.Equal(t, "202611020000", a.s.UpdateHealth.GoodVersion)
	require.Empty(t, m.good)

	// A crash again: still not blessed. A boot after a clean stop blesses it.
	require.Empty(t, crashBoot(a, m, "202611020000", "boot-4"))
	a.ready(ctx)
	require.Empty(t, m.good)

	require.Empty(t, bootNew(a, m, "202611020000", "boot-5"))
	a.ready(ctx)
	require.Equal(t, []string{"202611020000"}, m.good)
}

func TestNotReadyInTimeRollsBack(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	a.notReady(ctx)
	a.notReady(ctx)
	require.Equal(t, 2, a.s.UpdateHealth.Bake.Strikes)
	require.Equal(t, 0, m.reboots)

	a.notReady(ctx)
	require.Equal(t, 1, m.reboots)
	require.Equal(t, []string{"202611010000"}, m.bootedInto)
	require.Contains(t, a.s.UpdateHealth.FailedVersions["202611020000"], "did not get ready within 10m0s")
}

func TestUnidentifiedBootIsAStrike(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	m.bootIDErr = errors.New("no boot id")

	require.Empty(t, a.reconcileBoot(context.Background()))
	require.Equal(t, 1, a.s.UpdateHealth.Bake.Strikes)
	require.Equal(t, "this boot could not be identified", a.s.UpdateHealth.Bake.LastFailure)
}

func TestFailedListIsCapped(t *testing.T) {
	t.Parallel()

	failed := map[string]string{}
	for i := 10; i < 40; i++ {
		failed = addFailed(failed, "2026110100"+itoa2(i), "broken")
	}

	require.Len(t, failed, maxFailed)
	require.NotContains(t, failed, "202611010010")
	require.Contains(t, failed, "202611010039")

	apps := addFailed(nil, "incus:202611010000", "broken")
	require.Len(t, apps, 1)
}

func itoa2(i int) string {
	return fmt.Sprintf("%02d", i)
}

func TestRollbackKeepsApplicationVersions(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()
	m.apps = map[string]string{"incus": "202611020000"}

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	m.fails = []string{"the incus application is not running"}

	for i := 1; i <= 3; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 1, m.reboots)
	require.Equal(t, map[string]string{"incus": "202611020000"}, a.s.UpdateHealth.KeptApplications)

	// A newer OS version installed later lets the applications follow it again.
	armBake(a.s, "202611030000", "boot-3")
	require.Nil(t, a.s.UpdateHealth.KeptApplications)
}

func TestRestartRecordsNVIDIA(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	a.s.System.Update.Config.RestartWindow = &api.SystemUpdateMaintenanceWindow{StartHour: 13, EndHour: 15}
	m.nvidia = true

	a.tick(context.Background())
	require.Equal(t, 1, m.reboots)
	require.True(t, a.s.UpdateHealth.NVIDIAWasActive)
}

func TestNVIDIAHealthComparesWithBefore(t *testing.T) {
	t.Parallel()

	fellBack := api.ApplicationNVIDIAState{ActiveDriver: "nouveau", Reason: "the driver did not load"}

	// All GPUs on passthrough: the driver never ran, so its absence is no failure.
	require.Empty(t, nvidiaHealth(false, false, fellBack))
	require.Empty(t, nvidiaHealth(true, true, api.ApplicationNVIDIAState{ActiveDriver: "nvidia"}))
	require.Equal(t, []string{"the NVIDIA driver no longer runs: the driver did not load"}, nvidiaHealth(true, false, fellBack))

	fellBack.NeedsReboot = true
	require.Empty(t, nvidiaHealth(true, false, fellBack))
}

func TestInstanceChecks(t *testing.T) {
	t.Parallel()

	all := []incusapi.Instance{
		{Name: "web", Project: "default", StatusCode: incusapi.Running},
		{Name: "db", Project: "default", StatusCode: incusapi.Stopped},
		{Name: "build", Project: "ci", StatusCode: incusapi.Running, InstancePut: incusapi.InstancePut{Ephemeral: true}},
	}

	// Ephemeral instances go away by design; they aren't brought back.
	require.Equal(t, []string{"default/web"}, recordedInstances(all))

	// A deleted instance isn't a failure; a stopped one is.
	require.Equal(t, []string{"instance default/db is not running"}, instanceFailures([]string{"default/web", "default/db", "default/gone"}, all))
}

// fakeSwitch records what switchBoot does.
type fakeSwitch struct {
	calls      []string
	keyChanged bool
	keyErr     error
	rekeyErr   error
	rebindErr  error
	nextErr    error
}

func (f *fakeSwitch) switcher(sbDisabled bool) bootSwitch {
	return bootSwitch{
		version: "202611010000", target: "old.efi", current: "new.efi", sbDisabled: sbDisabled,
		keyChanged: func(uki string) (bool, error) {
			f.calls = append(f.calls, "key "+uki)

			return f.keyChanged, f.keyErr
		},
		rekey: func(_ context.Context, uki string) error {
			f.calls = append(f.calls, "rekey "+uki)
			if uki == "old.efi" {
				return f.rekeyErr
			}

			return nil
		},
		rebindPCR4: func(_ context.Context, uki string) error {
			f.calls = append(f.calls, "pcr4 "+uki)
			if uki == "old.efi" {
				return f.rebindErr
			}

			return nil
		},
		nextVersion: func(_ context.Context, v string) error {
			f.calls = append(f.calls, "next "+v)

			return f.nextErr
		},
		nextCurrent: func(_ context.Context) error {
			f.calls = append(f.calls, "next current")

			return nil
		},
	}
}

func TestSwitchBoot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Same key with Secure Boot on: only the next boot changes.
	f := &fakeSwitch{}
	require.NoError(t, switchBoot(ctx, f.switcher(false)))
	require.Equal(t, []string{"key old.efi", "next 202611010000"}, f.calls)

	// Another signing key: the TPM is enrolled for the target before the reboot.
	f = &fakeSwitch{keyChanged: true}
	require.NoError(t, switchBoot(ctx, f.switcher(false)))
	require.Equal(t, []string{"key old.efi", "next 202611010000", "rekey old.efi"}, f.calls)

	// Enrolling fails: bound back, next boot back on the running version, and an error (no reboot).
	f = &fakeSwitch{keyChanged: true, rekeyErr: errors.New("not in db")}
	require.ErrorContains(t, switchBoot(ctx, f.switcher(false)), "not in db")
	require.Equal(t, []string{"key old.efi", "next 202611010000", "rekey old.efi", "rekey new.efi", "next current"}, f.calls)

	// The key can't be checked: nothing changes.
	f = &fakeSwitch{keyErr: errors.New("no key")}
	require.ErrorContains(t, switchBoot(ctx, f.switcher(false)), "could not be checked")
	require.Equal(t, []string{"key old.efi"}, f.calls)

	// Secure Boot off: PCR4 is bound to the target, and back when that fails.
	f = &fakeSwitch{}
	require.NoError(t, switchBoot(ctx, f.switcher(true)))
	require.Equal(t, []string{"key old.efi", "next 202611010000", "pcr4 old.efi"}, f.calls)

	f = &fakeSwitch{rebindErr: errors.New("tpm")}
	require.Error(t, switchBoot(ctx, f.switcher(true)))
	require.Equal(t, []string{"key old.efi", "next 202611010000", "pcr4 old.efi", "pcr4 new.efi", "next current"}, f.calls)

	// The next boot can't be set: the disks stay as they are.
	f = &fakeSwitch{nextErr: errors.New("efi")}
	require.Error(t, switchBoot(ctx, f.switcher(true)))
	require.Equal(t, []string{"key old.efi", "next 202611010000"}, f.calls)
}

func TestRollbackStopsWhenBootSwitchFails(t *testing.T) {
	t.Parallel()

	a, m := newAuto(t)
	ctx := context.Background()
	m.bootErr = errors.New("the disks could not be bound to 202611010000: not in db")

	require.Empty(t, bootNew(a, m, "202611020000", "boot-2"))

	m.fails = []string{"the Incus API does not answer"}

	for i := 1; i <= 3; i++ {
		m.clock = at(14, i)

		a.tick(ctx)
	}

	require.Equal(t, 0, m.reboots)
	require.Contains(t, a.s.UpdateHealth.NeedsOwner, "not in db")
	require.True(t, a.s.System.Update.Config.AutoUpdatesOff)
}
