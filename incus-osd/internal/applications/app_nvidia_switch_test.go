package applications

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func switchFixture(t *testing.T, branch string) (*nvidia, *[]string, *bool) {
	t.Helper()
	n, commands, version := newNVIDIAFixture(t, nvidiaBootTest{})
	n.appConfig = &n.state.Applications.NVIDIA.Config.ApplicationConfig
	n.appState.Initialized = true
	n.state.OS.SystemIsReady = true
	s := &n.state.Applications.NVIDIA.State
	s.RequestedBranch, s.ActiveBranch, s.ActiveExtensionVersion, s.Version = branch, branch, "001", "001"
	n.variant = branch

	for _, b := range []string{nvidiaStable, nvidiaLTS} {
		for _, v := range []string{"001", "002"} {
			writeGPUFixtureFile(t, n.sysroot, filepath.Join(systemd.LocalExtensionsPath, v, b+".raw"), "signed pack")
		}
	}

	writeGPUFixtureFile(t, n.sysroot, "sys/module/nvidia/refcnt", "0")
	require.NoError(t, os.MkdirAll(n.path("/sys/module/nvidia/holders"), 0o700))
	require.NoError(t, os.MkdirAll(n.path(systemd.SystemExtensionsPath), 0o700))
	require.NoError(t, os.Symlink(n.path(filepath.Join(systemd.LocalExtensionsPath, "001", branch+".raw")), n.path(filepath.Join(systemd.SystemExtensionsPath, branch+".raw"))))

	for _, other := range []string{"incus", "gpu-support"} {
		image := filepath.Join(systemd.LocalExtensionsPath, "001", other+".raw")
		writeGPUFixtureFile(t, n.sysroot, image, other)
		require.NoError(t, os.Symlink(n.path(image), n.path(filepath.Join(systemd.SystemExtensionsPath, other+".raw"))))
	}

	n.refreshExtensions = func(ctx context.Context) error {
		_, err := n.command(ctx, "systemd-sysext", "refresh")

		return err
	}
	n.verifyImage = func(_ context.Context, image string) error {
		_, err := os.Stat(image)

		return err
	}
	original := n.runCommand
	fail := false
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		for _, other := range []string{"incus", "gpu-support"} {
			data, err := os.ReadFile(n.path(filepath.Join(systemd.SystemExtensionsPath, other+".raw")))
			require.NoError(t, err)
			require.Equal(t, other, string(data))
		}

		if name == "systemd-sysext" {
			require.Equal(t, []string{"refresh"}, args)

			linked := 0

			for _, candidate := range []string{nvidiaStable, nvidiaLTS} {
				_, err := os.Lstat(n.path(filepath.Join(systemd.SystemExtensionsPath, candidate+".raw")))
				if err == nil {
					linked++
				}
			}

			require.LessOrEqual(t, linked, 1)
		}

		if name == "/sbin/rmmod" && args[0] == "nvidia" {
			for _, address := range []string{"0000:01:00.0", "0000:02:00.0"} {
				_ = os.Remove(n.path("/sys/bus/pci/devices/" + address + "/driver"))
			}
		}

		if name == "/usr/bin/nvidia-smi" && fail {
			fail = false

			*commands = append(*commands, name+" failed")

			return "", errors.New("health failed")
		}

		return original(ctx, name, args...)
	}
	s.DriverVersion = version

	return n, commands, &fail
}

func TestNVIDIASwitchLiveAndOfflineRollback(t *testing.T) {
	t.Parallel()

	for _, source := range []string{nvidiaStable, nvidiaLTS} {
		for _, failHealth := range []bool{false, true} {
			t.Run(source+" failure="+strconv.FormatBool(failHealth), func(t *testing.T) {
				t.Parallel()
				n, commands, fail := switchFixture(t, source)

				target := nvidiaStable
				if source == nvidiaStable {
					target = nvidiaLTS
				}

				n.SetVersions("002", []string{"001", "002"})
				n.state.Applications.NVIDIA.Config.PinCurrentVersions = true
				n.state.Applications.NVIDIA.State.RequestedBranch = target
				*fail = failHealth

				require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: target, Version: "002"}))
				s := n.state.Applications.NVIDIA.State
				require.Equal(t, &api.ApplicationNVIDIABuild{Branch: source, Version: "001"}, s.Previous)
				require.True(t, n.state.Applications.NVIDIA.Config.PinCurrentVersions)
				require.False(t, s.NeedsReboot)
				require.Equal(t, target, s.RequestedBranch)
				require.Equal(t, "002", s.Version)

				active := target
				if failHealth {
					active = source

					require.Contains(t, s.Reason, "restored the previous driver from disk")
					require.Len(t, s.BadBuilds, 1)
				}

				require.Equal(t, active, s.ActiveBranch)

				sequence := strings.Join(*commands, "\n")
				unload := strings.Index(sequence, "/sbin/rmmod nvidia")
				merged := unload + strings.Index(sequence[unload:], "systemd-sysext refresh")
				require.NotContains(t, sequence, "systemd-sysext unmerge")
				require.Less(t, strings.Index(sequence, "systemd-sysext refresh"), unload)
				require.Less(t, unload, merged)
				require.Less(t, merged, strings.Index(sequence, "/sbin/insmod"))
				require.NotContains(t, sequence, "curl")
				require.NotContains(t, sequence, "reboot")
				require.NoError(t, pruneNVIDIAExtensions(n.state, "", n.path(systemd.LocalExtensionsPath)))
				_, err := os.Stat(n.path(filepath.Join(systemd.LocalExtensionsPath, "001", source+".raw")))
				require.NoError(t, err)
			})
		}
	}
}

func TestNVIDIABusySwitchWaitsForBoot(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	require.NoError(t, os.MkdirAll(n.path("/proc/123/fd"), 0o700))
	require.NoError(t, os.Symlink("/dev/nvidia0", n.path("/proc/123/fd/7")))
	s := &n.state.Applications.NVIDIA.State
	s.RequestedBranch, s.Version = nvidiaLTS, "002"
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Equal(t, "001", s.ActiveExtensionVersion)
	require.True(t, s.NeedsReboot)
	require.Contains(t, s.Reason, "in use")
	require.Empty(t, *commands)

	saved, err := state.LoadOrCreate(n.path("/state.txt"))
	require.NoError(t, err)
	require.Equal(t, nvidiaLTS, saved.Applications.NVIDIA.State.RequestedBranch)
	require.NoError(t, os.RemoveAll(n.path("/proc/123")))
	// Simulate boot after the early selector links the persisted branch and processes have exited.
	require.NoError(t, os.Remove(n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia.raw"))))
	require.NoError(t, os.MkdirAll(n.path(systemd.SystemExtensionsPath), 0o700))
	require.NoError(t, os.Symlink(n.path(filepath.Join(systemd.LocalExtensionsPath, "002", "nvidia-lts.raw")), n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia-lts.raw"))))
	n.state.OS.SystemIsReady = false
	n.variant = nvidiaLTS
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, n.Start(t.Context()))
	require.Equal(t, nvidiaLTS, s.ActiveBranch)
	require.False(t, s.NeedsReboot)
}

func TestNVIDIAVersionSwitchRetainsPrevious(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	n.appState.AvailableVersions = []string{"001", "002"}
	require.NoError(t, n.SwitchVersion("002"))
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: n.Version()}))
	require.Equal(t, "001", n.state.Applications.NVIDIA.State.Previous.Version)
	require.Equal(t, "002", n.state.Applications.NVIDIA.State.ActiveExtensionVersion)
}

func TestNVIDIANoSelectionDoesNotPrune(t *testing.T) {
	t.Parallel()

	for _, active := range []string{"", "001"} {
		n, _, _ := switchFixture(t, nvidiaStable)
		n.state.Applications.NVIDIA.State.ActiveExtensionVersion = active
		n.state.Applications.NVIDIA.State.Reason = systemd.NVIDIAMissingReason
		require.NoError(t, pruneNVIDIAExtensions(n.state, "", n.path(systemd.LocalExtensionsPath)))
		_, err := os.Stat(n.path(filepath.Join(systemd.LocalExtensionsPath, "002", "nvidia.raw")))
		require.NoError(t, err)
	}
}

func TestNVIDIABranchesShareOneRequestedSlot(t *testing.T) {
	t.Parallel()

	n, _, _ := switchFixture(t, nvidiaStable)
	for _, branch := range []string{nvidiaLTS, nvidiaStable} {
		app, err := Load(t.Context(), n.state, branch)
		require.NoError(t, err)
		app.SetVersions("001", []string{"001"})
		require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: branch, Version: "001"}))

		for _, name := range []string{nvidiaStable, nvidiaLTS} {
			candidate, err := Load(t.Context(), n.state, name)
			require.NoError(t, err)
			require.Equal(t, name == branch, candidate.IsInstalled())
		}

		require.Equal(t, "001", n.state.Applications.NVIDIA.State.Previous.Version)
	}
}

func TestNVIDIAModuleBusyDoesNotQuarantineRequestedBuild(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/sbin/rmmod" {
			return "", errors.New("module is in use")
		}

		return original(ctx, name, args...)
	}
	require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
	s := n.state.Applications.NVIDIA.State
	require.True(t, s.NeedsReboot)
	require.Empty(t, s.BadBuilds)
	require.Equal(t, nvidiaStable, s.ActiveBranch)
	require.Contains(t, s.Reason, "modules are still in use")
}

func TestNVIDIARuntimeFailedHealthNeverReboots(t *testing.T) {
	t.Parallel()

	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)

			s := &n.state.Applications.NVIDIA.State
			if !previous {
				s.ActiveExtensionVersion = ""
			}

			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "/usr/bin/nvidia-smi" {
					return "", errors.New("health unavailable")
				}

				return original(ctx, name, args...)
			}
			s.RequestedBranch, s.Version = nvidiaLTS, "002"
			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
			require.True(t, s.NeedsReboot)
			require.Contains(t, s.Reason, "restart to")
			reason := s.Reason

			require.NoError(t, os.Remove(n.path("/usr/share/nvidia/version")))
			require.Error(t, n.SetFriendlyVersion(t.Context()))
			require.Equal(t, reason, s.Reason)
			require.NotContains(t, strings.Join(*commands, "\n"), "reboot")
			require.Empty(t, s.PendingAttempt)
			require.Empty(t, s.PendingHealth)

			saved, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.True(t, saved.Applications.NVIDIA.State.NeedsReboot)

			if previous {
				require.Equal(t, nvidiaStable, saved.Applications.NVIDIA.State.RequestedBranch)
				require.Equal(t, "001", saved.Applications.NVIDIA.State.Version)
			}
		})
	}
}

func TestNVIDIABootFailureStillBoundsAutomaticReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := switchFixture(t, nvidiaStable)
	n.state.OS.SystemIsReady = false
	n.state.Applications.NVIDIA.State.Previous = &api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}
	original := n.runCommand
	n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "/usr/bin/nvidia-smi" {
			return "", errors.New("health unavailable")
		}

		if name == "systemctl" {
			saved, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.Equal(t, "boot-one", saved.Applications.NVIDIA.State.FallbackBootID)
			require.NotEmpty(t, saved.Applications.NVIDIA.State.BadBuilds)
		}

		return original(ctx, name, args...)
	}
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, n.Start(t.Context()))
	require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))
	require.NoError(t, n.deferRollback(t.Context(), "health unavailable", true))
	require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))
}

func TestNVIDIAUpdateDefersWhileExplicitRequestSwitchesLive(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"background", nvidiaLTS, "incus"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			s := &n.state.Applications.NVIDIA.State
			s.RequestedBranch, s.Version = nvidiaLTS, "002"
			n.findExtension = func(context.Context, string, string, bool) (string, []string, string) {
				return "002", []string{"001", "002"}, ""
			}

			ctx := t.Context()
			if source != "background" {
				ctx = WithNVIDIALiveSwitch(ctx, source)
			}

			require.NoError(t, n.reconcile(ctx))

			if source == nvidiaLTS {
				require.Equal(t, nvidiaLTS, s.ActiveBranch)
				require.False(t, s.NeedsReboot)
				require.NotEmpty(t, *commands)
			} else {
				require.Equal(t, nvidiaStable, s.ActiveBranch)
				require.True(t, s.NeedsReboot)
				require.Contains(t, s.Reason, "restart to apply")
				require.Empty(t, *commands)
			}
		})
	}
}

func TestNVIDIAModuleReferencesDeferBeforeAnyChanges(t *testing.T) {
	t.Parallel()

	for _, module := range nvidiaModules {
		t.Run(module, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			writeGPUFixtureFile(t, n.sysroot, "sys/module/"+module+"/refcnt", "1")
			require.NoError(t, os.MkdirAll(n.path("/sys/module/"+module+"/holders"), 0o700))
			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
			require.True(t, n.state.Applications.NVIDIA.State.NeedsReboot)
			require.Empty(t, *commands)

			target, err := os.Readlink(n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia.raw")))
			require.NoError(t, err)
			require.Contains(t, target, "001/nvidia.raw")
		})
	}
}

func TestNVIDIABootFallbackPreservesGoodPrevious(t *testing.T) {
	t.Parallel()
	n, _, _ := switchFixture(t, nvidiaStable)
	s := &n.state.Applications.NVIDIA.State
	good := api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "001"}
	failed := api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}
	s.Previous = &good
	s.ActiveBranch, s.ActiveExtensionVersion, s.RequestedBranch, s.Version = failed.Branch, failed.Version, failed.Branch, failed.Version
	s.BadBuilds = map[string]string{n.buildKey(failed): "failed check"}
	require.NoError(t, n.prepare(t.Context(), true))
	require.Equal(t, &good, s.Previous)
	require.Equal(t, good, n.activeBuild())
}

func TestNVIDIACheckBuildSkipsQuarantine(t *testing.T) {
	t.Parallel()

	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)

			s := &n.state.Applications.NVIDIA.State
			if legacy {
				s.BadVersions = map[string]string{n.attemptKey(s.DriverVersion): "failed check"}
			} else {
				s.BadBuilds = map[string]string{n.buildKey(n.activeBuild()): "failed check"}
			}

			require.ErrorContains(t, n.checkBuild(t.Context()), "failed check")
			require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/insmod")
			require.NotContains(t, strings.Join(*commands, "\n"), "/usr/bin/nvidia-smi")
		})
	}
}
