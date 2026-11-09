package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestKernelPUTPersistsConfigurationWithWarnings(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.txt")
	s, err := state.LoadOrCreate(path)
	require.NoError(t, err)

	server := Server{state: s}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/1.0/system/kernel", strings.NewReader(`{"config":{"pci":{"passthrough":[{"vendor_id":"10de","product_id":"1e04","pci_address":"0000:aa:00.0"},{"vendor_id":"1234","product_id":"abcd"}]}}}`))
	reply := httptest.NewRecorder()
	server.putSystemKernel(reply, request, func(_ context.Context, config api.SystemKernelConfig) error {
		require.Len(t, config.PCI.Passthrough, 2)

		return errors.Join(errors.New("stale PCI address"), errors.New("SR-IOV unavailable"))
	})
	require.Equal(t, http.StatusOK, reply.Code)

	body := struct {
		Metadata map[string]string `json:"metadata"`
	}{}
	require.NoError(t, json.Unmarshal(reply.Body.Bytes(), &body))
	require.Contains(t, body.Metadata["warning"], "stale PCI address")
	require.Contains(t, body.Metadata["warning"], "SR-IOV unavailable")

	loaded, err := state.LoadOrCreate(path)
	require.NoError(t, err)
	require.Equal(t, s.System.Kernel.Config, loaded.System.Kernel.Config)
	require.Len(t, loaded.System.Kernel.Config.PCI.Passthrough, 2)
}
