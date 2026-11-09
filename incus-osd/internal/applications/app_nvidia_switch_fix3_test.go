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
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func TestNVIDIABootLoadAndOwnHealthTimeoutQuarantineAndRecover(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"insmod-no-device", "own-health-timeout"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			n.state.OS.SystemIsReady = false
			original := n.runCommand
			n.healthTimeout = 20 * time.Millisecond
			probeTimedOut := false
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if phase == "insmod-no-device" && name == "/sbin/insmod" {
					return "", errors.New("No such device")
				}

				if phase == "own-health-timeout" && name == "/usr/bin/nvidia-smi" {
					<-ctx.Done()
					probeTimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)

					return "", ctx.Err()
				}

				return original(ctx, name, args...)
			}
			require.NoError(t, n.prepare(t.Context(), true))

			driver := readNVIDIABoot(t, n).Version
			for range 3 {
				require.NoError(t, n.Start(t.Context()))
			}

			s := n.state.Applications.NVIDIA.State
			require.NotEmpty(t, s.BadBuilds[n.buildKey(api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "001"})])
			require.NotEmpty(t, s.BadVersions[n.attemptKey(driver)])
			require.Equal(t, "nouveau", s.ActiveDriver)
			require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))

			for _, address := range []string{"0000:01:00.0", "0000:02:00.0"} {
				override, err := os.ReadFile(n.path("/sys/bus/pci/devices/" + address + "/driver_override"))
				require.NoError(t, err)
				require.Equal(t, "nouveau", string(override))
			}

			if phase == "own-health-timeout" {
				require.True(t, probeTimedOut)
			}
		})
	}
}

func TestNVIDIABootMarkerFailureSkipsProbeAndReboot(t *testing.T) {
	t.Parallel()
	n, commands, _ := newNVIDIAFixture(t, nvidiaBootTest{})
	require.NoError(t, n.prepare(t.Context(), true))
	require.NoError(t, os.RemoveAll(n.path("/run/incus-os")))
	writeGPUFixtureFile(t, n.sysroot, "run/incus-os", "not a directory")

	*commands = nil

	require.NoError(t, n.Start(t.Context()))
	require.False(t, n.bootRecovery)
	require.Empty(t, *commands)
}

func TestNVIDIANouveauOwnershipDefersWithoutDetach(t *testing.T) {
	t.Parallel()

	for _, first := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch", true: "first-install"}[first], func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			s := &n.state.Applications.NVIDIA.State

			s.RequestedBranch, s.Version, s.ActiveDriver = nvidiaLTS, "002", "nouveau"
			if first {
				s.ActiveBranch, s.ActiveExtensionVersion = "", ""

				require.NoError(t, os.Remove(n.path(filepath.Join(systemd.SystemExtensionsPath, "nvidia.raw"))))
			}

			require.NoError(t, os.Symlink(n.path("/sys/bus/pci/drivers/nouveau"), n.path("/sys/bus/pci/devices/0000:01:00.0/driver")))
			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
			require.Equal(t, "the card is using another driver; restart to switch it to NVIDIA", s.Reason)
			require.True(t, s.NeedsReboot)
			require.Equal(t, nvidiaLTS, s.RequestedBranch)
			require.Equal(t, "002", s.Version)
			require.Empty(t, *commands)
			require.Empty(t, s.BadBuilds)
		})
	}
}

func TestNVIDIATransientFailureReloadsOldModulesOrReportsRestart(t *testing.T) {
	t.Parallel()

	for _, failReload := range []bool{false, true} {
		t.Run(map[bool]string{false: "reload-works", true: "reload-fails"}[failReload], func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			s := &n.state.Applications.NVIDIA.State
			s.ActiveDriver = "nvidia"
			loaded := true
			original := n.runCommand
			failedStep := false
			reloadAttempted := false
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "/sbin/rmmod" && args[0] == "nvidia" {
					loaded = false
				}

				if name == "/sbin/ldconfig" && !failedStep {
					failedStep = true

					return "", errors.New("temporary cache error")
				}

				if name == "/sbin/insmod" && strings.HasSuffix(args[0], "/nvidia.ko") {
					reloadAttempted = true

					require.NotEqual(t, "nvidia", s.ActiveDriver)

					if failReload {
						return "", errors.New("reload failed")
					}

					loaded = true
				}

				if name == "/usr/bin/nvidia-smi" {
					require.True(t, loaded)
				}

				return original(ctx, name, args...)
			}
			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
			require.True(t, reloadAttempted)
			require.True(t, s.NeedsReboot)

			if failReload {
				require.NotEqual(t, "nvidia", s.ActiveDriver)
				require.False(t, loaded)
				require.Contains(t, s.Reason, "could not be reloaded")
			} else {
				require.Equal(t, "nvidia", s.ActiveDriver)
				require.True(t, loaded)
			}

			require.Empty(t, s.BadBuilds)
			require.NotContains(t, strings.Join(*commands, "\n"), "reboot")
		})
	}
}
