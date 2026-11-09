package update

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/applications"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// fakeApp is an application whose health follows its version.
type fakeApp struct {
	applications.Application

	name     string
	version  string
	bad      map[string]bool
	restarts int
}

func (f *fakeApp) Name() string { return f.name }

func (f *fakeApp) SwitchVersion(v string) error {
	f.version = v

	return nil
}

func (f *fakeApp) Restart(_ context.Context) error {
	f.restarts++

	return nil
}

func appTest(t *testing.T, app *fakeApp) (*state.State, appOps, *int) {
	t.Helper()

	s, err := state.LoadOrCreate(filepath.Join(t.TempDir(), "state.txt"))
	require.NoError(t, err)

	refreshes := 0
	ops := appOps{
		load: func(_ context.Context, _ *state.State, _ string) (applications.Application, error) { return app, nil },
		refresh: func(_ context.Context, _ *state.State) error {
			refreshes++

			return nil
		},
		healthy: func(_ context.Context, a applications.Application) string {
			if app.bad[app.version] {
				return "the " + a.Name() + " application is not running"
			}

			return ""
		},
		save: func(s *state.State) error { return s.SaveDurable() },
	}

	return s, ops, &refreshes
}

func TestApplicationUpdateHealthy(t *testing.T) {
	t.Parallel()

	app := &fakeApp{name: "arca", version: "202611020000"}
	s, ops, _ := appTest(t, app)
	rememberPreviousApplication(s, "arca", "202611010000", "202611020000")

	verifyApplication(context.Background(), s, ops, "arca", "202611020000")
	require.Empty(t, s.UpdateHealth.FailedApplications)
	require.Equal(t, "Application arca updated to 202611020000", s.UpdateHealth.LastResult)
	require.Equal(t, 0, app.restarts)
}

func TestApplicationUpdateRollsBack(t *testing.T) {
	t.Parallel()

	app := &fakeApp{name: "incus", version: "202611020000", bad: map[string]bool{"202611020000": true}}
	s, ops, refreshes := appTest(t, app)
	rememberPreviousApplication(s, "incus", "202611010000", "202611020000")

	verifyApplication(context.Background(), s, ops, "incus", "202611020000")
	require.Equal(t, "202611010000", app.version)
	require.Equal(t, 1, *refreshes)
	require.Equal(t, 1, app.restarts)
	require.Equal(t, "the incus application is not running", s.UpdateHealth.FailedApplications["incus:202611020000"])
	require.Equal(t, "Application incus version 202611020000 failed; went back to 202611010000", s.UpdateHealth.LastResult)
	require.Empty(t, s.UpdateHealth.NeedsOwner)
}

func TestApplicationRollbackThatDoesNotHelpNeedsOwner(t *testing.T) {
	t.Parallel()

	app := &fakeApp{name: "incus", version: "202611020000", bad: map[string]bool{"202611020000": true, "202611010000": true}}
	s, ops, _ := appTest(t, app)
	rememberPreviousApplication(s, "incus", "202611010000", "202611020000")

	verifyApplication(context.Background(), s, ops, "incus", "202611020000")
	require.Equal(t, "application incus went back to 202611010000 after 202611020000 failed, and it is still not healthy: the incus application is not running", s.UpdateHealth.NeedsOwner)
	require.True(t, s.System.Update.Config.AutoUpdatesOff)
}

func TestApplicationWithoutPreviousNeedsOwner(t *testing.T) {
	t.Parallel()

	app := &fakeApp{name: "arca", version: "202611020000", bad: map[string]bool{"202611020000": true}}
	s, ops, _ := appTest(t, app)

	verifyApplication(context.Background(), s, ops, "arca", "202611020000")
	require.Contains(t, s.UpdateHealth.NeedsOwner, "there is no previous version to go back to")
}

func TestNVIDIAKeepsItsOwnRecovery(t *testing.T) {
	t.Parallel()

	app := &fakeApp{name: "nvidia", version: "202611020000", bad: map[string]bool{"202611020000": true}}
	s, ops, refreshes := appTest(t, app)
	rememberPreviousApplication(s, "nvidia", "202611010000", "202611020000")

	verifyApplication(context.Background(), s, ops, "nvidia", "202611020000")
	require.Equal(t, "202611020000", app.version)
	require.Equal(t, 0, *refreshes)
	require.Empty(t, s.UpdateHealth.FailedApplications)
}
