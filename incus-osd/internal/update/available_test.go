package update

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// stubUpdate is an update the stub provider offers; it is never downloaded.
type stubUpdate struct {
	version string
}

func (u stubUpdate) Version() string { return u.version }

func (u stubUpdate) IsNewerThan(other string) bool {
	return providers.DatetimeComparison(u.version, other)
}

func (stubUpdate) Download(context.Context, string, func(float64)) error {
	return errors.New("stub updates are not downloaded")
}

func (stubUpdate) DownloadImage(context.Context, string, string, func(float64)) (string, error) {
	return "", errors.New("stub updates are not downloaded")
}

func (stubUpdate) GetFilename() string { return "" }

// stubProvider offers fixed versions, or fails with err.
type stubProvider struct {
	providers.Provider

	os, secureBoot string
	err            error
}

func (*stubProvider) Type() string { return "images" }

func (p *stubProvider) GetOSUpdate(context.Context) (providers.OSUpdate, error) {
	if p.err != nil {
		return nil, p.err
	}

	if p.os == "" {
		return nil, providers.ErrNoUpdateAvailable
	}

	return stubUpdate{p.os}, nil
}

func (p *stubProvider) GetSecureBootCertUpdate(context.Context) (providers.SecureBootCertUpdate, error) {
	if p.secureBoot == "" {
		return nil, providers.ErrNoUpdateAvailable
	}

	return stubUpdate{p.secureBoot}, nil
}

func (*stubProvider) GetApplicationUpdate(context.Context, string) (providers.ApplicationUpdate, error) {
	return nil, providers.ErrNoUpdateAvailable
}

func runningState(release string) *state.State {
	s := &state.State{}
	s.OS.Name = "Arca OS"
	s.OS.RunningRelease = release

	return s
}

func checkOSOn(t *testing.T, s *state.State, p providers.Provider) error {
	t.Helper()

	return checkOSWith(withCheckCycle(t.Context()), s, p, nil, false)
}

func TestOSUpdateNeededRecordsTheChannelVersion(t *testing.T) {
	t.Parallel()

	// A newer version is available and is applied.
	s := runningState("202611010000")

	needed, skip, err := osUpdateNeeded(t.Context(), s, stubUpdate{"202611150123"})
	require.NoError(t, err)
	require.False(t, skip)
	require.True(t, needed)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.True(t, s.System.Update.State.UpdateAvailable)

	// A version that failed before is held, so it isn't available.
	s = runningState("202611010000")
	s.UpdateHealth.FailedVersions = map[string]string{"202611150123": "3 health checks failed"}

	needed, skip, err = osUpdateNeeded(t.Context(), s, stubUpdate{"202611150123"})
	require.NoError(t, err)
	require.True(t, skip)
	require.False(t, needed)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.False(t, s.System.Update.State.UpdateAvailable)

	// A locally newer version is not an update.
	s = runningState("202611200000")
	s.System.Update.State.UpdateAvailable = true

	_, _, err = osUpdateNeeded(t.Context(), s, stubUpdate{"202611150123"})
	require.Error(t, err)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.False(t, s.System.Update.State.UpdateAvailable)
}

func TestCheckOSAtTheChannelVersion(t *testing.T) {
	t.Parallel()

	s := runningState("202611150123")
	s.System.Update.State.UpdateAvailable = true

	require.NoError(t, checkOSOn(t, s, &stubProvider{os: "202611150123"}))
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.False(t, s.System.Update.State.UpdateAvailable)
}

func TestCheckOSFailureKeepsTheAvailableVersion(t *testing.T) {
	t.Parallel()

	s := runningState("202611010000")
	s.System.Update.State.AvailableVersion = "202611150123"
	s.System.Update.State.UpdateAvailable = true

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	require.Error(t, checkOSOn(t, s, &stubProvider{err: refused}))
	require.Equal(t, ErrorKindNetwork, s.System.Update.State.LastErrorKind)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.True(t, s.System.Update.State.UpdateAvailable)

	// An untrusted server changes neither either.
	f := newTrustFixture(t)
	f.server.serve(f.foreign)

	require.Error(t, checkOSOn(t, s, f.provider))
	require.Equal(t, ErrorKindTrust, s.System.Update.State.LastErrorKind)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.True(t, s.System.Update.State.UpdateAvailable)
}

func TestCheckOSOfAnEmptyChannel(t *testing.T) {
	t.Parallel()

	s := runningState("202611010000")
	s.System.Update.State.AvailableVersion = "202611150123"
	s.System.Update.State.UpdateAvailable = true

	require.NoError(t, checkOSOn(t, s, &stubProvider{}))
	require.Empty(t, s.System.Update.State.AvailableVersion)
	require.False(t, s.System.Update.State.UpdateAvailable)
}

func TestOtherChecksLeaveTheAvailableVersion(t *testing.T) {
	t.Parallel()

	p := &stubProvider{os: "202611150123", secureBoot: "202611200000"}

	osStep := func(ctx context.Context, s *state.State, _ bool, force bool) error {
		return checkOSWith(ctx, s, p, nil, force)
	}

	secureBootStep := func(ctx context.Context, s *state.State, _ bool, force bool) error {
		return checkSecureBootWith(ctx, s, p, nil, force)
	}

	applicationsStep := func(ctx context.Context, s *state.State, _ bool, force bool) error {
		return checkApplicationsWith(ctx, s, p, nil, []string{"incus"}, force)
	}

	// The OS step goes first, so the Secure Boot and application steps run after it.
	s := runningState("202611150123")
	s.SecureBoot.Version = "202611200000"

	require.NoError(t, fullCheck(t.Context(), s, false, osStep, secureBootStep, applicationsStep))
	require.Empty(t, s.System.Update.State.LastErrorKind)
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.False(t, s.System.Update.State.UpdateAvailable)

	// Without an OS step, a behind node stays behind.
	s = runningState("202611010000")
	s.SecureBoot.Version = "202611200000"
	s.System.Update.State.AvailableVersion = "202611150123"
	s.System.Update.State.UpdateAvailable = true

	require.NoError(t, fullCheck(t.Context(), s, false, secureBootStep, applicationsStep))
	require.Equal(t, "202611150123", s.System.Update.State.AvailableVersion)
	require.True(t, s.System.Update.State.UpdateAvailable)
}
