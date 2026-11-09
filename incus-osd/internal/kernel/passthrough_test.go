package kernel

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestStalePassthroughDoesNotLoseDeviceIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	config := []api.SystemKernelConfigPCIPassthrough{
		{VendorID: "10de", ProductID: "1e04", PCIAddress: "0000:AA:00.0"},
		{VendorID: "1234", ProductID: "ABCD"},
	}
	err := updatePCIPassthroughsAt(t.Context(), config, root, func(context.Context, string, ...string) (string, error) { return "", nil })
	require.ErrorContains(t, err, "0000:aa:00.0")
	body, err := os.ReadFile(filepath.Join(root, "etc/modprobe.d/99-local-device-passthrough.conf"))
	require.NoError(t, err)
	require.Equal(t, "options vfio-pci ids=1234:abcd\n", string(body))
}
