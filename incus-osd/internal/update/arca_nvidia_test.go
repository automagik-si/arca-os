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
		calls   int
	}{
		{"the driver is missing", []string{"gpu-support", "nvidia"}, nil, []string{"gpu-support", "nvidia"}, 1},
		{"nothing is missing", nil, nil, nil, 0},
		{"the check fails", []string{"nvidia"}, errors.New("no state"), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var installed []string

			calls := 0

			missing := func(context.Context, *state.State) ([]string, error) { return tc.missing, tc.err }
			install := func(_ context.Context, _ *state.State, names []string) error {
				calls++

				installed = append(installed, names...)

				return nil
			}

			installNVIDIAAfterReady(t.Context(), &state.State{}, 0, missing, install)
			require.Equal(t, tc.want, installed)
			require.Equal(t, tc.calls, calls)
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
