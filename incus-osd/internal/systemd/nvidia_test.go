package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
			m := nvidiaExtensions{links: filepath.Join(root, "extensions"), images: filepath.Join(root, "images"), reason: filepath.Join(root, "reason")}
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
			// ExecStartPre uses this exact guard, with no dependency on daemon state or an update server.
			m.check(t.Context())
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
