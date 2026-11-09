package applications

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestGPUSupportPreservesGPUOwnership(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, module := range []string{"nouveau", "nvidia", "vfio-pci"} {
		writeGPUFixtureFile(t, root, "sys/bus/pci/drivers/"+module+"/unbind", "untouched")
		writeGPUFixtureFile(t, root, "sys/bus/pci/drivers/"+module+"/bind", "untouched")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "sys/module", module), 0o755))
	}

	g := &gpuSupport{common: common{state: &state.State{}}, sysroot: root}
	g.runCommand = func(_ context.Context, command string, args ...string) (string, error) {
		t.Fatalf("unexpected GPU ownership change: %s %v", command, args)

		return "", nil
	}
	require.NoError(t, g.Start(t.Context()))

	for _, module := range []string{"nouveau", "nvidia", "vfio-pci"} {
		for _, action := range []string{"bind", "unbind"} {
			value, err := os.ReadFile(filepath.Join(root, "sys/bus/pci/drivers", module, action))
			require.NoError(t, err)
			require.Equal(t, "untouched", string(value))
		}
	}
}

func writeGPUFixtureFile(t *testing.T, root string, path string, value string) {
	t.Helper()

	path = filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
}
