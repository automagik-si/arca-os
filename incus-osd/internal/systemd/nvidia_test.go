package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestNVIDIAKernelCompatibility(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateNVIDIAKernel("test-kernel\n", "test-kernel\n"))
	require.ErrorContains(t, validateNVIDIAKernel("previous-kernel\n", "test-kernel\n"), NVIDIAMissingReason)
	require.Error(t, validateNVIDIAKernel("", "test-kernel"))
}

func TestNVIDIARefreshDoesNotBlockIncus(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		kernel    string
		preferred string
		pinned    bool
		haveNext  bool
		want      string
	}{
		{"application arrives before its kernel", "old-kernel", "002", false, true, "001"},
		{"kernel newer than application", "new-kernel", "001", false, false, ""},
		{"OS updated after failed driver download", "new-kernel", "002", false, false, ""},
		{"pinned application and kernel bump", "new-kernel", "001", true, true, ""},
		{"matching pending image becomes usable", "new-kernel", "002", false, true, "002"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			m := nvidiaExtensions{links: filepath.Join(root, "extensions"), images: filepath.Join(root, "images"), reason: filepath.Join(root, "reason"), preferred: tt.preferred, pinned: tt.pinned}
			require.NoError(t, os.MkdirAll(m.links, 0o700))

			for _, version := range []string{"001", "002"} {
				if version == "002" && !tt.haveNext {
					continue
				}

				path := filepath.Join(m.images, version)
				require.NoError(t, os.MkdirAll(path, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(path, "nvidia.raw"), []byte(version), 0o600))
			}

			incus := filepath.Join(root, "incus.raw")
			require.NoError(t, os.WriteFile(incus, []byte("signed Incus fixture"), 0o600))
			require.NoError(t, os.Symlink(incus, filepath.Join(m.links, "incus.raw")))
			link := filepath.Join(m.links, "nvidia.raw")
			require.NoError(t, os.Symlink(filepath.Join(m.images, "001", "nvidia.raw"), link))
			m.validate = func(_ context.Context, path string) error {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}

				kernel := "old-kernel"
				if string(body) == "002" {
					kernel = "new-kernel"
				}

				return validateNVIDIAKernel(kernel, tt.kernel)
			}
			// Assert selection before the daemon refresh, so prepare can use NVIDIA on this first boot.
			m.check(t.Context())

			if tt.want != "" {
				target, err := os.Readlink(link)
				require.NoError(t, err)
				require.Equal(t, filepath.Join(m.images, tt.want, "nvidia.raw"), target)
				require.NoError(t, m.validate(t.Context(), link))
			}

			selected, versions, reason := m.selectImage(t.Context(), tt.preferred, tt.pinned)
			require.Equal(t, tt.want, selected)
			require.Contains(t, versions, "001")

			if tt.want == "" {
				require.Equal(t, NVIDIAMissingReason, reason)
			}

			merged := false
			err := refreshWithGPUCheck(t.Context(), m, func(ctx context.Context, name string, args ...string) (string, error) {
				require.Equal(t, "systemd-sysext", name)
				require.Equal(t, []string{"refresh"}, args)

				target, err := os.Readlink(filepath.Join(m.links, "incus.raw"))
				require.NoError(t, err)
				require.Equal(t, incus, target)

				if tt.want == "" {
					_, err = os.Lstat(link)
					require.ErrorIs(t, err, os.ErrNotExist)
				} else {
					require.NoError(t, m.validate(ctx, link))
				}

				merged = true

				return "", nil
			}, func(context.Context) error { return nil })
			require.NoError(t, err)
			require.True(t, merged)

			_, err = os.Stat(filepath.Join(m.images, "001", "nvidia.raw"))
			require.NoError(t, err)
		})
	}
}

func TestNVIDIAExecStartPreExcludesUnverifiableImage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := nvidiaExtensions{links: root, reason: filepath.Join(root, "reason"), validate: func(context.Context, string) error { return errors.New("bad signature") }}
	require.NoError(t, os.WriteFile(filepath.Join(root, "incus.raw"), []byte("Incus"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia.raw"), []byte("invalid"), 0o600))
	m.check(t.Context())

	_, err := os.Stat(filepath.Join(root, "nvidia.raw"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(root, "nvidia.raw.disabled"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "incus.raw"))
	require.NoError(t, err)
}

func TestNVIDIAExecStartPreSelectsNewestVerifiedBuild(t *testing.T) {
	t.Parallel()

	for _, pinned := range []bool{false, true} {
		t.Run(strconv.FormatBool(pinned), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()

			m := nvidiaExtensions{links: filepath.Join(root, "links"), images: filepath.Join(root, "images"), reason: filepath.Join(root, "reason"), preferred: "002", pinned: pinned}
			for _, version := range []string{"002", "003", "004"} {
				directory := filepath.Join(m.images, version)
				require.NoError(t, os.MkdirAll(directory, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.raw"), []byte(version), 0o600))
			}

			m.validate = func(_ context.Context, path string) error {
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}

				if string(body) == "004" {
					return errors.New("wrong signing key")
				}

				return nil
			}
			m.check(t.Context())
			target, err := os.Readlink(filepath.Join(m.links, "nvidia.raw"))
			require.NoError(t, err)

			want := "003"
			if pinned {
				want = "002"
			}

			require.Equal(t, filepath.Join(m.images, want, "nvidia.raw"), target)
		})
	}
}

func TestNVIDIABranchSelectionAtBootAndBusyRefresh(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := nvidiaExtensions{links: filepath.Join(root, "links"), images: filepath.Join(root, "images"), reason: filepath.Join(root, "reason"), preferred: "002", branch: "nvidia-lts", pinned: true}
	require.NoError(t, os.MkdirAll(m.links, 0o700))

	for _, branch := range []string{"nvidia", "nvidia-lts"} {
		directory := filepath.Join(m.images, "002")
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, branch+".raw"), []byte(branch), 0o600))
	}

	old := filepath.Join(m.images, "002", "nvidia.raw")
	require.NoError(t, os.Symlink(old, filepath.Join(m.links, "nvidia.raw")))
	m.validate = func(context.Context, string) error { return nil }
	m.skipSelection = true
	require.NoError(t, refreshWithGPUCheck(t.Context(), m, func(context.Context, string, ...string) (string, error) { return "", nil }, func(context.Context) error { return nil }))
	target, err := os.Readlink(filepath.Join(m.links, "nvidia.raw"))
	require.NoError(t, err)
	require.Equal(t, old, target)

	_, err = os.Stat(filepath.Join(m.links, "nvidia-lts.raw"))
	require.ErrorIs(t, err, os.ErrNotExist)

	m.skipSelection = false
	m.check(t.Context())
	target, err = os.Readlink(filepath.Join(m.links, "nvidia-lts.raw"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.images, "002", "nvidia-lts.raw"), target)
	_, err = os.Stat(filepath.Join(m.links, "nvidia.raw"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNVIDIASelectionSkipsQuarantinedBuildsAndDrivers(t *testing.T) {
	t.Parallel()

	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()

			m := nvidiaExtensions{images: filepath.Join(root, "images"), links: filepath.Join(root, "links"), release: "release", branch: "nvidia", inspectOnly: true}
			for _, version := range []string{"001", "002"} {
				directory := filepath.Join(m.images, version)
				require.NoError(t, os.MkdirAll(directory, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.raw"), []byte(version), 0o600))
			}

			m.validate = func(context.Context, string) error { return nil }
			if legacy {
				m.badVersions = map[string]string{"release:bad-driver": "failed health"}
				m.driverVersion = func(_ context.Context, image string) (string, error) {
					if filepath.Base(filepath.Dir(image)) == "002" {
						return "bad-driver", nil
					}

					return "good-driver", nil
				}
			} else {
				m.badBuilds = map[string]string{"release:nvidia:002": "failed health"}
			}

			selected, _, reason := m.selectImage(t.Context(), "002", false)
			require.Equal(t, "001", selected)
			require.Empty(t, reason)
		})
	}
}

func TestNVIDIABootGuardFallbackKeepsGoodBuild(t *testing.T) {
	t.Parallel()

	for _, interrupted := range []bool{false, true} {
		t.Run(strconv.FormatBool(interrupted), func(t *testing.T) {
			t.Parallel()

			s := &state.State{}
			s.OS.RunningRelease = "release"
			app := &s.Applications.NVIDIA
			good := api.ApplicationNVIDIABuild{Branch: "nvidia", Version: "001"}
			app.State.Previous = &good
			app.State.RequestedBranch, app.State.Version = "nvidia-lts", "002"

			app.State.ActiveBranch, app.State.ActiveExtensionVersion = "nvidia-lts", "002"
			if interrupted {
				app.State.PendingHealth = "release:bad-driver"
			} else {
				app.State.BadBuilds = map[string]string{"release:nvidia-lts:002": "failed health"}
			}

			m := configureNVIDIAExtensions(nvidiaExtensions{}, s)
			require.Equal(t, good.Branch, m.branch)
			require.Equal(t, good.Version, m.preferred)
			require.True(t, m.lockedChoice)
			require.True(t, m.pinned)
			require.Equal(t, &good, app.State.Previous)
		})
	}
}

func TestNVIDIAQuarantineHasAnExplicitReason(t *testing.T) {
	t.Parallel()

	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			m := nvidiaExtensions{links: filepath.Join(root, "links"), images: filepath.Join(root, "images"), reason: filepath.Join(root, "reason"), release: "release", branch: "nvidia", preferred: "002", pinned: true}
			image := filepath.Join(m.images, "002", "nvidia.raw")
			require.NoError(t, os.MkdirAll(filepath.Dir(image), 0o700))
			require.NoError(t, os.WriteFile(image, []byte("signed pack"), 0o600))
			require.NoError(t, os.MkdirAll(m.links, 0o700))
			require.NoError(t, os.Symlink(image, filepath.Join(m.links, "nvidia.raw")))

			m.validate = func(context.Context, string) error { return nil }
			if legacy {
				m.badVersions = map[string]string{"release:driver": "failed health"}
				m.driverVersion = func(context.Context, string) (string, error) { return "driver", nil }
			} else {
				m.badBuilds = map[string]string{"release:nvidia:002": "failed health"}
			}

			m.check(t.Context())
			selected, _, reason := m.selectImage(t.Context(), "002", true)
			require.Empty(t, selected)
			require.Equal(t, NVIDIAQuarantinedReason, reason)
			body, err := os.ReadFile(m.reason)
			require.NoError(t, err)
			require.Equal(t, NVIDIAQuarantinedReason, string(body))

			_, err = os.Stat(image)
			require.NoError(t, err)
		})
	}
}
