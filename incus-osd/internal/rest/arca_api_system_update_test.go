package rest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/api"
)

func TestDecodeUpdateConfigKeepsUnsetFields(t *testing.T) {
	t.Parallel()

	current := api.SystemUpdateConfig{AutoUpdatesOff: true, BakeTime: "5m", RestartWindow: &api.SystemUpdateMaintenanceWindow{StartHour: 1, EndHour: 2}}

	// An older client that doesn't know the automatic updater's settings leaves them as they are.
	out := &api.SystemUpdate{}
	require.NoError(t, decodeUpdateConfig(strings.NewReader(`{"config":{"channel":"stable","check_frequency":"6h"}}`), out, current))
	require.True(t, out.Config.AutoUpdatesOff)
	require.Equal(t, "5m", out.Config.BakeTime)
	require.Equal(t, 1, out.Config.RestartWindow.StartHour)

	// Sent ones are taken as sent.
	out = &api.SystemUpdate{}
	require.NoError(t, decodeUpdateConfig(strings.NewReader(`{"config":{"check_frequency":"6h","auto_updates_off":false,"bake_time":""}}`), out, current))
	require.False(t, out.Config.AutoUpdatesOff)
	require.Empty(t, out.Config.BakeTime)

	require.Error(t, decodeUpdateConfig(strings.NewReader(`{`), out, current))
}
