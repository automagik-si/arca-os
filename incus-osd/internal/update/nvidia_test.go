package update

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

type failedNVIDIAPreparation struct{ applications.Application }

func (failedNVIDIAPreparation) PrepareUpdate(_ context.Context) error {
	return errors.New("temporary NVIDIA metadata failure")
}

func TestNVIDIAPreparationFailureStillRefreshesIncus(t *testing.T) {
	t.Parallel()

	s := &state.State{}
	updated := map[string]string{"nvidia": "002", "incus": "002"}
	refreshed := false
	loaded := map[string]bool{}
	err := refreshApplicationUpdates(t.Context(), s, updated, func(ctx context.Context, _ *state.State, name string) (applications.Application, error) {
		loaded[name] = true
		if name == "nvidia" {
			return failedNVIDIAPreparation{}, nil
		}

		return applications.Load(ctx, s, name)
	}, func(_ context.Context, actual *state.State) error {
		refreshed = true

		require.True(t, loaded["incus"])
		require.Contains(t, actual.Applications.NVIDIA.State.Reason, "current driver is unchanged")

		return nil
	})
	require.NoError(t, err)
	require.True(t, refreshed)
}
