package applications_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

func TestArcaIsSupported(t *testing.T) {
	t.Parallel()

	require.Contains(t, applications.Supported, "arca")
}

func TestArcaLoad(t *testing.T) {
	t.Parallel()

	app, err := applications.Load(context.Background(), &state.State{}, "arca")
	require.NoError(t, err)

	require.Equal(t, "arca", app.Name())
	require.False(t, app.IsPrimary())
	require.Equal(t, []string{"incus OR incus-lts-7.0"}, app.GetDependencies())

	// arca uses the Incus socket, so it starts after the primary application.
	require.Positive(t, app.StartupWeight())

	// Nothing is installed in an empty state.
	require.False(t, app.IsInstalled())
	require.Empty(t, app.Version())
}

func TestArcaStateField(t *testing.T) {
	t.Parallel()

	s := &state.State{}

	app, err := applications.Load(context.Background(), s, "arca")
	require.NoError(t, err)

	// The application reads and writes its own state field, not another application's.
	app.SetVersions("202611010000", []string{"202611010000"})
	require.Equal(t, "202611010000", s.Applications.Arca.State.Version)
	require.Empty(t, s.Applications.Debug.State.Version)
	require.Empty(t, s.Applications.GPUSupport.State.Version)

	got, err := app.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, s.Applications.Arca, got)
}
