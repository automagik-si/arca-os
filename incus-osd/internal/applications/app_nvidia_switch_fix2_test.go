package applications

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func idleNVIDIAModules(t *testing.T, n *nvidia) {
	t.Helper()

	for module, refs := range map[string]string{"nvidia": "3", "nvidia_modeset": "1", "nvidia_uvm": "0", "nvidia_drm": "0"} {
		writeGPUFixtureFile(t, n.sysroot, "sys/module/"+module+"/refcnt", refs)
		require.NoError(t, os.MkdirAll(n.path("/sys/module/"+module+"/holders"), 0o700))
	}

	for module, holders := range map[string][]string{"nvidia": {"nvidia_drm", "nvidia_modeset", "nvidia_uvm"}, "nvidia_modeset": {"nvidia_drm"}} {
		for _, holder := range holders {
			require.NoError(t, os.Symlink("../../"+holder, n.path("/sys/module/"+module+"/holders/"+holder)))
		}
	}
}

func TestNVIDIAModuleHoldersDistinguishIdleFromUsers(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"idle", "uvm-user", "core-user", "foreign-holder", "unreadable-holders", "missing-holders", "malformed-refcount", "malformed-holder"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			idleNVIDIAModules(t, n)

			switch mode {
			case "uvm-user":
				writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia_uvm/refcnt", "1")
			case "core-user":
				writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia/refcnt", "4")
			case "foreign-holder":
				require.NoError(t, os.Symlink("../../foreign", n.path("/sys/module/nvidia_uvm/holders/foreign")))
			case "unreadable-holders":
				require.NoError(t, os.Remove(n.path("/sys/module/nvidia_uvm/holders")))
				writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia_uvm/holders", "not a directory")
			case "missing-holders":
				require.NoError(t, os.Remove(n.path("/sys/module/nvidia_uvm/holders")))
			case "malformed-refcount":
				writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia_uvm/refcnt", "unknown")
			case "malformed-holder":
				writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia_uvm/holders/nvidia_drm", "not a symlink")
			default:
			}

			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))

			if mode == "idle" {
				require.False(t, n.state.Applications.NVIDIA.State.NeedsReboot)
				require.Equal(t, nvidiaLTS, n.state.Applications.NVIDIA.State.ActiveBranch)
				require.Contains(t, strings.Join(*commands, "\n"), "/sbin/rmmod")
			} else {
				require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
				require.Empty(t, *commands)
			}
		})
	}
}

func TestNVIDIAUserArrivingAfterDetachRestoresActiveLink(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	original := n.refreshExtensions
	calls := 0
	n.refreshExtensions = func(ctx context.Context) error {
		calls++
		err := original(ctx)

		if calls == 1 {
			require.NoError(t, os.MkdirAll(n.path("/proc/100/fd"), 0o700))
			require.NoError(t, os.Symlink("/dev/nvidia0", n.path("/proc/100/fd/7")))
		}

		return err
	}
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
	target, err := os.Readlink(n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia.raw")))
	require.NoError(t, err)
	require.Contains(t, target, "001/nvidia.raw")
	require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/rmmod")
	require.Greater(t, calls, 1)
}

func TestNVIDIARepairRefreshHasSixtySeconds(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	n.refreshExtensions = func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Greater(t, time.Until(deadline), 55*time.Second)
		require.LessOrEqual(t, time.Until(deadline), 60*time.Second)

		return nil
	}
	result := errors.New("transient")
	n.refreshOnError(t.Context(), &result)
	require.Error(t, result)
}

func TestNVIDIAUpdateClearsQuarantineBeforeSelection(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	driver := n.state.Applications.NVIDIA.State.DriverVersion
	s := &n.state.Applications.NVIDIA.State
	n.SetVersions("002", []string{"001", "002"})

	s.DriverVersion = "different-active-driver"
	key := n.attemptKey(driver)
	s.BadVersions = map[string]string{key: "failed health", "other:driver": "keep"}
	s.BadBuilds = map[string]string{n.buildKey(api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "002"}): "failed health"}
	s.PendingHealth, s.HealthBootID = key, "previous-boot"
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "systemd-dissect" {
			require.Contains(t, strings.Join(args, " "), "002/nvidia.raw")

			return driver, nil
		}

		return original(ctx, name, args...)
	}
	require.NoError(t, n.PrepareUpdate(t.Context()))
	saved, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.NotContains(t, saved.Applications.NVIDIA.State.BadVersions, key)
	require.Contains(t, saved.Applications.NVIDIA.State.BadVersions, "other:driver")
	require.Empty(t, saved.Applications.NVIDIA.State.BadBuilds)
	require.Empty(t, saved.Applications.NVIDIA.State.PendingHealth)

	n.findExtension = func(context.Context, string, string, bool) (string, []string, string) {
		require.NotContains(t, s.BadVersions, key)

		return "002", []string{"001", "002"}, ""
	}
	require.NoError(t, n.reconcile(t.Context()))
	require.True(t, s.NeedsReboot)
}

func TestNVIDIASwitchQuarantinesOnlyBindOrHealth(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"verification", "canceled-refresh", "transient-refresh", "canceled-health", "transient-load", "bind", "health"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			s := &n.state.Applications.NVIDIA.State
			s.RequestedBranch, s.Version = nvidiaLTS, "002"

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if phase == "verification" {
				n.verifyImage = func(context.Context, string) error { return errors.New("invalid signature") }
			}

			refresh := n.refreshExtensions
			calls := 0
			n.refreshExtensions = func(ctx context.Context) error {
				calls++
				if calls == 1 && (phase == "canceled-refresh" || phase == "transient-refresh") {
					if phase == "canceled-refresh" {
						cancel()

						return ctx.Err()
					}

					return errors.New("temporary refresh failure")
				}

				return refresh(ctx)
			}
			original := n.runCommand
			failed := false
			bindLoads := 0
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if !failed && name == "/usr/bin/nvidia-smi" {
					if phase == "canceled-health" {
						failed = true

						cancel()

						return "", ctx.Err()
					}

					if phase == "health" {
						failed = true

						return "", errors.New("failed health")
					}
				}

				if phase == "bind" && name == "/sbin/insmod" {
					if strings.HasSuffix(args[0], "/nvidia.ko") {
						bindLoads++
					}

					if bindLoads == 1 {
						return "", nil
					}
				}

				if !failed && name == "/sbin/insmod" {
					if phase == "transient-load" {
						failed = true

						return "", errors.New("temporary load failure")
					}
				}

				return original(ctx, name, args...)
			}
			err := n.switchBuild(ctx, api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"})
			require.NoError(t, err)

			quarantine := phase == "bind" || phase == "health"
			require.Equal(t, quarantine, len(s.BadBuilds) > 0)
			require.Empty(t, s.BadVersions)

			if phase == "verification" {
				require.Empty(t, *commands)
				require.Nil(t, s.Previous)
			}

			if !quarantine {
				require.True(t, s.NeedsReboot)
			}

			require.NotContains(t, strings.Join(*commands, "\n"), "reboot")
		})
	}
}

func TestNVIDIADaemonRestartCannotRepeatBootHealthOrReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, n.Start(t.Context()))

	*commands = nil
	n.state.OS.SystemIsReady = false
	n.runCommand = func(context.Context, string, ...string) (string, error) {
		t.Fatal("daemon restart must not probe or reboot")

		return "", nil
	}
	require.NoError(t, n.Start(t.Context()))
	require.False(t, n.bootRecovery)
	require.True(t, n.bootStarted())
}

func TestNVIDIAQuarantineReasonSurvivesMissingMetadata(t *testing.T) {
	t.Parallel()
	n, _, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, os.Remove(n.path("/usr/share/nvidia/version")))
	writeGPUFixtureFile(t, n.sysroot, systemd.NVIDIAExtensionReasonPath, systemd.NVIDIAQuarantinedReason)
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, n.Start(t.Context()))
	require.Equal(t, systemd.NVIDIAQuarantinedReason, n.state.Applications.NVIDIA.State.Reason)
	require.Error(t, n.SetFriendlyVersion(t.Context()))
	require.Equal(t, systemd.NVIDIAQuarantinedReason, n.state.Applications.NVIDIA.State.Reason)
}

func TestNVIDIAControlledBootErrorsDoNotQuarantine(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"load", "health"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if phase == "load" && name == "/sbin/insmod" {
					return "", errors.New("temporary load failure")
				}

				if phase == "health" && name == "/usr/bin/nvidia-smi" {
					return "", context.Canceled
				}

				return original(ctx, name, args...)
			}
			require.NoError(t, n.prepare(t.Context(), true))
			require.NoError(t, n.Start(t.Context()))
			require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
			require.Empty(t, n.state.Applications.NVIDIA.State.BadBuilds)
			require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
			require.Empty(t, n.state.Applications.NVIDIA.State.PendingAttempt)
			require.Empty(t, n.state.Applications.NVIDIA.State.PendingHealth)
			require.NotContains(t, strings.Join(*commands, "\n"), "reboot")
			writeGPUFixtureFile(t, n.sysroot, "proc/sys/kernel/random/boot_id", "next-boot")
			require.Empty(t, n.checkPreviousAttempt())
			require.Empty(t, n.state.Applications.NVIDIA.State.BadVersions)
		})
	}
}

func TestNVIDIAUpdateDefersDuringDaemonRestart(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	n.state.OS.SystemIsReady = false
	_, err := claimNVIDIABoot(n.path(nvidiaStartedFile))
	require.NoError(t, err)
	require.True(t, n.bootStarted())
	n.SetVersions("002", []string{"001", "002"})
	n.findExtension = func(context.Context, string, string, bool) (string, []string, string) {
		return "002", []string{"001", "002"}, ""
	}
	require.NoError(t, n.reconcile(t.Context()))
	require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
	require.Equal(t, "001", n.state.Applications.NVIDIA.State.ActiveExtensionVersion)
	require.Empty(t, *commands)
}
