package applications

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/systemd"
)

func TestNVIDIASwitchErrorsKeepOtherExtensionsMerged(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"remove-link", "first-refresh", "unload", "mkdir", "remove-staging", "symlink", "rename", "second-refresh", "ldconfig", "canceled-refresh"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			n, _, _ := switchFixture(t, nvidiaStable)
			merged := true
			failed := false
			refreshes := 0
			injected := errors.New("injected " + phase)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			fail := func(step string) error {
				if phase == step && !failed {
					failed = true

					return injected
				}

				return nil
			}
			files := n.files()
			n.extensionFiles = &nvidiaExtensionFiles{
				mkdirAll: func(path string, mode os.FileMode) error {
					require.True(t, merged)

					err := fail("mkdir")
					if err != nil {
						return err
					}

					return files.mkdirAll(path, mode)
				},
				remove: func(path string) error {
					require.True(t, merged)

					step := ""
					if strings.HasSuffix(path, "nvidia-lts.raw") {
						step = "remove-link"
					}

					if strings.HasSuffix(path, ".new") {
						step = "remove-staging"
					}

					err := fail(step)
					if err != nil {
						return err
					}

					return files.remove(path)
				},
				symlink: func(old string, path string) error {
					require.True(t, merged)

					err := fail("symlink")
					if err != nil {
						return err
					}

					return files.symlink(old, path)
				},
				rename: func(old string, path string) error {
					require.True(t, merged)

					err := fail("rename")
					if err != nil {
						return err
					}

					return files.rename(old, path)
				},
			}
			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				require.True(t, merged)

				step := ""
				if name == "/sbin/rmmod" {
					step = "unload"
				}

				if name == "/sbin/ldconfig" {
					step = "ldconfig"
				}

				err := fail(step)
				if err != nil {
					return "", err
				}

				return original(ctx, name, args...)
			}
			refresh := n.refreshExtensions
			n.refreshExtensions = func(refreshCtx context.Context) error {
				refreshes++

				step := ""
				if refreshes == 1 {
					step = "first-refresh"
				}

				if refreshes == 2 {
					step = "second-refresh"
				}

				if phase == "canceled-refresh" && !failed {
					failed, merged = true, false

					cancel()

					return ctx.Err()
				}

				err := fail(step)
				if err != nil {
					merged = false

					return err
				}

				require.NoError(t, refreshCtx.Err())

				merged = true

				return refresh(refreshCtx)
			}
			err := n.activateBuild(ctx, api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"})
			require.Error(t, err)
			require.True(t, failed)
			require.True(t, merged)

			for _, other := range []string{"incus", "gpu-support"} {
				target, err := os.Readlink(n.path(filepath.Join(systemd.SystemExtensionsPath, other+".raw")))
				require.NoError(t, err)
				require.Contains(t, target, "001/"+other+".raw")
			}
		})
	}
}

func TestNVIDIASwitchSaveErrorsKeepOtherExtensionsMerged(t *testing.T) {
	t.Parallel()

	for _, healthFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-health", true: "before-rollback"}[healthFailure], func(t *testing.T) {
			t.Parallel()
			n, commands, _ := switchFixture(t, nvidiaStable)
			original := n.runCommand
			n.runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
				if name == "/usr/bin/nvidia-smi" {
					n.state.UnrecognizedFields = []string{"future.field"}

					if healthFailure {
						return "", errors.New("health failed")
					}
				}

				return original(ctx, name, args...)
			}
			require.Error(t, n.switchBuild(t.Context(), api.ApplicationNVIDIABuild{Branch: nvidiaLTS, Version: "002"}))
			require.Equal(t, "systemd-sysext refresh", (*commands)[len(*commands)-1])
			require.NotContains(t, strings.Join(*commands, "\n"), "unmerge")

			for _, other := range []string{"incus", "gpu-support"} {
				_, err := os.Stat(n.path(filepath.Join(systemd.SystemExtensionsPath, other+".raw")))
				require.NoError(t, err)
			}
		})
	}
}
