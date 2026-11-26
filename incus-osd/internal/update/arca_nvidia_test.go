package update

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestNVIDIAInstalledOnceAfterReady(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		missing []string
		err     error
		want    []string
	}{
		{"the driver is missing", []string{"gpu-support", "nvidia"}, nil, []string{"gpu-support", "nvidia"}},
		{"nothing is missing", nil, nil, nil},
		{"the check fails", nil, errors.New("no state"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var installed []string

			missing := func(context.Context, *state.State) ([]string, error) { return tc.missing, tc.err }
			install := func(_ context.Context, _ *state.State, names []string) error {
				installed = append(installed, names...)

				return nil
			}

			installNVIDIAAfterReady(t.Context(), &state.State{}, 0, missing, install)
			require.Equal(t, tc.want, installed)
		})
	}

	// A daemon that stops before the wait ends installs nothing.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	called := false
	missing := func(context.Context, *state.State) ([]string, error) {
		called = true

		return []string{"nvidia"}, nil
	}

	installNVIDIAAfterReady(ctx, &state.State{}, nvidiaAfterReadyWait, missing, func(context.Context, *state.State, []string) error { return nil })
	require.False(t, called)
}
