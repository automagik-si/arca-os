package applications

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestNVIDIABootFallbackRebootIgnoresHungUnload(t *testing.T) {
	t.Parallel()

	for _, previous := range []string{"none", "unusable", "good"} {
		t.Run(previous, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			n.state.OS.SystemIsReady = false

			s := &n.state.Applications.NVIDIA.State
			failed := api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "001"}
			other := api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}

			if previous != "none" {
				s.Previous = &other
			}

			if previous == "unusable" {
				s.BadBuilds = map[string]string{n.buildKey(other): "failed before"}
			}

			release := make(chan struct{})

			t.Cleanup(func() { close(release) })

			var unloadAttempted atomic.Bool

			rebooted := false
			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				switch name {
				case "/sbin/insmod":
					return "", errors.New("No such device")
				case "/sbin/rmmod":
					// A module unload stuck in the kernel after a GPU hang never returns.
					unloadAttempted.Store(true)
					<-release
				case "systemctl":
					saved, err := state.LoadOrCreate(n.path("/state.txt"))
					require.NoError(t, err)
					require.Equal(t, "nouveau", saved.Applications.NVIDIA.State.ActiveDriver)
					require.Equal(t, "boot-one", saved.Applications.NVIDIA.State.FallbackBootID)
					require.NotEmpty(t, saved.Applications.NVIDIA.State.BadBuilds[n.buildKey(failed)])

					if previous == "good" {
						// The next boot selects the previous build; nothing is restored live.
						require.Equal(t, other.Branch, saved.Applications.NVIDIA.State.RequestedBranch)
						require.Equal(t, other.Version, saved.Applications.NVIDIA.State.Version)
						require.True(t, saved.Applications.NVIDIA.State.NeedsReboot)
					}

					for _, address := range []string{"0000:01:00.0", "0000:02:00.0"} {
						override, err := os.ReadFile(n.path("/sys/bus/pci/devices/" + address + "/driver_override"))
						require.NoError(t, err)
						require.Equal(t, "nouveau", string(override))
					}

					rebooted = true
				default:
				}

				return original(ctx, name, args...)
			}
			require.NoError(t, n.prepare(t.Context(), true))
			require.True(t, readNVIDIABoot(t, n).Failure)

			done := make(chan error, 1)

			go func() { done <- n.Start(t.Context()) }()

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("a hung module unload blocked the boot fallback")
			}

			require.True(t, rebooted)
			require.False(t, unloadAttempted.Load())
			require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))
			require.Equal(t, "nouveau", s.ActiveDriver)
			require.Equal(t, failed, n.activeBuild())
			require.NotContains(t, strings.Join(*commands, "\n"), "systemd-sysext")
		})
	}
}

func TestNVIDIABootFallbackWithoutRebootBindsNouveau(t *testing.T) {
	t.Parallel()

	for _, probe := range []string{"works", "fails"} {
		t.Run(probe, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			n.state.OS.SystemIsReady = false
			n.bootRecovery = true

			s := &n.state.Applications.NVIDIA.State
			s.FallbackBootID = "boot-one"
			address := "0000:01:00.0"

			if probe == "fails" {
				require.NoError(t, os.Mkdir(n.path("/sys/bus/pci/drivers_probe"), 0o700))
			}

			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "/sbin/modprobe" && args[0] == "nouveau" {
					override, err := os.ReadFile(n.path("/sys/bus/pci/devices/" + address + "/driver_override"))
					require.NoError(t, err)
					require.Equal(t, "nouveau", string(override))

					if probe == "works" {
						require.NoFileExists(t, n.path("/sys/bus/pci/drivers_probe"))
					}
				}

				return original(ctx, name, args...)
			}

			require.NoError(t, n.fallback(t.Context(), nvidiaBoot{Version: s.DriverVersion, Cards: []string{address}, Attempted: true}, "NVIDIA health check failed"))

			ran := strings.Join(*commands, "\n")
			require.Contains(t, ran, "/sbin/rmmod nvidia")
			require.Contains(t, ran, "/sbin/modprobe nouveau")
			require.NotContains(t, ran, "reboot")
			require.Equal(t, "nouveau", s.ActiveDriver)

			if probe == "works" {
				probed, err := os.ReadFile(n.path("/sys/bus/pci/drivers_probe"))
				require.NoError(t, err)
				require.Equal(t, address, string(probed))
				require.Equal(t, "NVIDIA health check failed", s.Reason)
			} else {
				require.Equal(t, "NVIDIA health check failed; "+errNVIDIANouveau.Error(), s.Reason)
			}

			saved, err := state.LoadOrCreate(n.path("/state.txt"))
			require.NoError(t, err)
			require.Equal(t, s.Reason, saved.Applications.NVIDIA.State.Reason)
		})
	}
}

func TestNVIDIABootFallbackQuarantinesBuildOrVersion(t *testing.T) {
	t.Parallel()

	for _, previous := range []string{"good-same-driver", "none", "also-bad"} {
		t.Run(previous, func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			n.state.OS.SystemIsReady = false

			s := &n.state.Applications.NVIDIA.State
			failed := api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "001"}
			// Both fixture packs carry the same NVIDIA driver version: 002 is a rebuilt pack.
			rebuilt := api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}

			switch previous {
			case "good-same-driver":
				s.Previous = &rebuilt
			case "also-bad":
				s.Previous = &rebuilt
				s.BadBuilds = map[string]string{n.buildKey(rebuilt): "failed before"}
			default:
			}

			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "/usr/bin/nvidia-smi" && n.activeBuild() == failed {
					return "", errors.New("GSP failed")
				}

				return original(ctx, name, args...)
			}
			require.NoError(t, n.prepare(t.Context(), true))

			driver := readNVIDIABoot(t, n).Version
			require.NotEmpty(t, driver)
			require.NoError(t, n.Start(t.Context()))
			require.NotEmpty(t, s.BadBuilds[n.buildKey(failed)])

			require.Equal(t, "nouveau", s.ActiveDriver)
			require.Equal(t, 1, strings.Count(strings.Join(*commands, "\n"), "systemctl --no-block reboot"))

			if previous == "good-same-driver" {
				// The rebuilt pack of the same driver version stays eligible for the next boot.
				require.NotContains(t, s.BadVersions, n.attemptKey(driver))
				require.NotContains(t, s.BadBuilds, n.buildKey(rebuilt))
				require.Equal(t, rebuilt.Branch, s.RequestedBranch)
				require.Equal(t, rebuilt.Version, s.Version)
				require.Contains(t, s.Reason, "reboot to restore the previous driver")

				return
			}

			require.NotEmpty(t, s.BadVersions[n.attemptKey(driver)])
		})
	}
}

func TestNVIDIAUnsupportedCardOnNouveauDoesNotDefer(t *testing.T) {
	t.Parallel()

	ada, pascal := "0000:01:00.0", "0000:02:00.0"

	for _, onNouveau := range []string{pascal, ada} {
		t.Run(map[string]string{pascal: "pre-turing-on-nouveau", ada: "ada-on-nouveau"}[onNouveau], func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			s := &n.state.Applications.NVIDIA.State
			s.RequestedBranch, s.Version = nvidiaLTS, "002"

			writeGPUFixtureFile(t, n.sysroot, "sys/bus/pci/devices/"+ada+"/device", "0x2805")
			writeGPUFixtureFile(t, n.sysroot, "sys/bus/pci/devices/"+pascal+"/device", "0x1b80")
			writeGPUFixtureFile(t, n.sysroot, "usr/share/nvidia/supported-gpus", "2805\n")

			nouveau := n.path("/sys/bus/pci/devices/" + onNouveau + "/driver")
			require.NoError(t, os.Symlink(n.path("/sys/bus/pci/drivers/nouveau"), nouveau))

			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				out, err := original(ctx, name, args...)

				// Unloading NVIDIA leaves the card that Nouveau drives where it is.
				_, statErr := os.Lstat(nouveau)
				if name == "/sbin/rmmod" && errors.Is(statErr, os.ErrNotExist) {
					require.NoError(t, os.Symlink(n.path("/sys/bus/pci/drivers/nouveau"), nouveau))
				}

				return out, err
			}

			require.NoError(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))

			if onNouveau == ada {
				require.Equal(t, errNVIDIAOtherDriver.Error(), s.Reason)
				require.True(t, s.NeedsReboot)
				require.Equal(t, nvidiaStable, s.ActiveBranch)
				require.NotContains(t, strings.Join(*commands, "\n"), "/sbin/rmmod")

				return
			}

			require.False(t, s.NeedsReboot)
			require.Empty(t, s.Reason)
			require.Equal(t, api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}, n.activeBuild())
			require.Equal(t, "nvidia", s.ActiveDriver)

			override, err := os.ReadFile(n.path("/sys/bus/pci/devices/" + pascal + "/driver_override"))
			require.NoError(t, err)
			require.NotEqual(t, "nvidia", string(override))

			driver, err := os.Readlink(nouveau)
			require.NoError(t, err)
			require.Equal(t, n.path("/sys/bus/pci/drivers/nouveau"), driver)
		})
	}
}

func TestNVIDIAInterruptedAttemptQuarantinesBuildOrVersion(t *testing.T) {
	t.Parallel()

	for _, previous := range []string{"good-same-driver", "none", "also-bad", "same-build"} {
		for _, marker := range []string{"attempt", "health"} {
			t.Run(previous+"/"+marker, func(t *testing.T) {
				t.Parallel()
				n, _, _ := switchFixture(t, nvidiaStable)
				s := &n.state.Applications.NVIDIA.State
				driver := s.DriverVersion
				failed := api.ApplicationNVIDIABuild{Branch: nvidiaStable, Version: "001"}
				// Both fixture packs carry the same NVIDIA driver version: 002 is a rebuilt pack.
				rebuilt := api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}

				switch previous {
				case "good-same-driver":
					s.Previous = &rebuilt
				case "also-bad":
					s.Previous = &rebuilt
					s.BadBuilds = map[string]string{n.buildKey(rebuilt): "failed before"}
				case "same-build":
					s.Previous = &failed
				default:
				}

				if marker == "attempt" {
					s.PendingAttempt, s.AttemptBootID = n.attemptKey(driver), "boot-zero"
				} else {
					s.PendingHealth, s.HealthBootID = n.attemptKey(driver), "boot-zero"
				}

				require.Empty(t, n.checkPreviousAttempt())
				require.Empty(t, s.PendingAttempt)
				require.Empty(t, s.PendingHealth)

				saved, err := state.LoadOrCreate(n.path("/state.txt"))
				require.NoError(t, err)

				if previous == "good-same-driver" {
					require.NotContains(t, saved.Applications.NVIDIA.State.BadVersions, n.attemptKey(driver))
					require.NotEmpty(t, saved.Applications.NVIDIA.State.BadBuilds[n.buildKey(failed)])
					require.NotContains(t, saved.Applications.NVIDIA.State.BadBuilds, n.buildKey(rebuilt))

					return
				}

				require.NotEmpty(t, saved.Applications.NVIDIA.State.BadVersions[n.attemptKey(driver)])
			})
		}
	}
}
