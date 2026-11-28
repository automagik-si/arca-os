package update

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// The update that crashed: the UKI of 202611272126 had its signature stripped after the build.
func strippedUKIError() error {
	return fmt.Errorf("applying: %w", &util.UKIError{
		Version: "202611272126",
		Err:     fmt.Errorf("%w: its signature table points past the end of the file", util.ErrAuthenticodeUnreadable),
	})
}

func TestRejectedUKIIsTheLastError(t *testing.T) {
	t.Parallel()

	s := runningState("202611172310")

	recordCheck(t.Context(), s, strippedUKIError())

	st := s.System.Update.State
	require.Equal(t, ErrorKindParse, st.LastErrorKind)
	require.Equal(t, refusedWords, st.LastError)
	require.Contains(t, st.LastErrorDetail, "the UKI of OS version 202611272126 cannot be verified")
	require.Contains(t, st.LastErrorDetail, "signature table points past the end of the file")
}

// What the owner reads about the update that was refused.
const refusedWords = "The update 202611272126 was refused: its boot image is damaged or was changed after it was built. " +
	"The server keeps running the version it has."

func heldState(t *testing.T) *state.State {
	t.Helper()

	s, err := state.LoadOrCreate(filepath.Join(t.TempDir(), "state.txt"))
	require.NoError(t, err)

	s.OS.Name = "Arca OS"
	s.OS.RunningRelease = "202611172310"

	require.True(t, rejectOSUpdate(t.Context(), s, "202611272126", strippedUKIError()))

	return s
}

func requireRefusalShown(t *testing.T, s *state.State) {
	t.Helper()

	st := s.System.Update.State
	require.Equal(t, refusedWords, st.LastError)
	require.Equal(t, ErrorKindParse, st.LastErrorKind)
	require.Contains(t, st.LastErrorDetail, "signature table points past the end of the file")
}

func requireNoError(t *testing.T, s *state.State) {
	t.Helper()

	st := s.System.Update.State
	require.Empty(t, st.LastError)
	require.Empty(t, st.LastErrorKind)
	require.Empty(t, st.LastErrorDetail)
}

// While the refused version is held, every later check keeps showing why, in last_error and status:
// the next check found nothing wrong only because it skipped that version.
func TestRefusedUpdateStaysShownWhileHeld(t *testing.T) {
	t.Parallel()

	s := heldState(t)

	// Secure Boot and application checks run first in a cycle and succeed.
	recordCheck(t.Context(), s, nil)
	requireRefusalShown(t, s)

	// The OS check skips the held version.
	require.NoError(t, checkOSOn(t, s, &stubProvider{os: "202611272126"}))
	requireRefusalShown(t, s)
	require.Equal(t, refusedWords, s.System.Update.State.Status)
	require.Equal(t, "Version 202611272126 was not installed", s.UpdateHealth.LastResult)
	require.Equal(t, refusedWords, s.UpdateHealth.LastReason)
	require.Contains(t, s.UpdateHealth.FailedVersions["202611272126"], "the UKI of OS version 202611272126 cannot be verified")

	// Another failure is shown instead while it lasts, and the refusal again after it.
	recordCheck(t.Context(), s, errors.New("dial tcp: connection refused"))
	require.Contains(t, s.System.Update.State.LastError, "connection refused")
	require.Empty(t, s.System.Update.State.LastErrorDetail)
	recordCheck(t.Context(), s, nil)
	requireRefusalShown(t, s)
}

// The refusal is no longer shown once the owner asks to try the version again.
func TestRefusedUpdateClearsOnRelease(t *testing.T) {
	t.Parallel()

	s := heldState(t)

	require.NoError(t, releaseVersion(t.Context(), s, "202611272126"))
	recordCheck(t.Context(), s, nil)
	requireNoError(t, s)
}

// The refusal is no longer shown once a newer version is installed.
func TestRefusedUpdateClearsWhenANewerOneApplies(t *testing.T) {
	t.Parallel()

	s := heldState(t)

	recordOSApplied(s, "202611280623")
	recordCheck(t.Context(), s, nil)
	requireNoError(t, s)

	// And it stays cleared once that version runs.
	s.OS.RunningRelease, s.OS.NextRelease = "202611280623", ""
	recordCheck(t.Context(), s, nil)
	requireNoError(t, s)
}

func TestUnverifiedUKIIsASignatureError(t *testing.T) {
	t.Parallel()

	s := runningState("202611172310")

	recordCheck(t.Context(), s, &util.UKIError{Version: "202611272126", Err: errors.New("binary has no valid signatures")})

	require.Equal(t, ErrorKindSignature, s.System.Update.State.LastErrorKind)
}

// A rejected UKI is not downloaded and failed again on the next check: the version is held until a
// newer one comes or the owner asks to try it again.
func TestRejectedUKIIsNotRetried(t *testing.T) {
	t.Parallel()

	s, err := state.LoadOrCreate(filepath.Join(t.TempDir(), "state.txt"))
	require.NoError(t, err)

	s.OS.Name = "Arca OS"
	s.OS.RunningRelease = "202611172310"

	require.True(t, rejectOSUpdate(t.Context(), s, "202611272126", strippedUKIError()))
	require.True(t, IsFailedVersion(s, "202611272126"))
	require.Contains(t, s.UpdateHealth.FailedVersions["202611272126"], "the UKI of OS version 202611272126 cannot be verified")
	require.Equal(t, "202611172310", s.OS.RunningRelease)
	require.Empty(t, s.OS.NextRelease)
	require.False(t, s.System.Update.State.NeedsReboot)

	// stubUpdate fails any download, so a check that tried to apply the version again would fail.
	require.NoError(t, checkOSOn(t, s, &stubProvider{os: "202611272126"}))
	require.False(t, s.System.Update.State.UpdateAvailable)
	require.Empty(t, s.OS.NextRelease)
	require.Equal(t, refusedWords, osUpdateHold(s, "202611272126"))

	// A newer version is not held.
	require.Empty(t, osUpdateHold(s, "202611290000"))

	// The owner can ask for it again.
	require.NoError(t, releaseVersion(t.Context(), s, "202611272126"))
	require.False(t, IsFailedVersion(s, "202611272126"))
}

func TestOtherApplyErrorsAreNotRejections(t *testing.T) {
	t.Parallel()

	s := runningState("202611172310")

	require.False(t, rejectOSUpdate(t.Context(), s, "202611272126", errors.New("systemd-sysupdate failed")))
	require.False(t, IsFailedVersion(s, "202611272126"))
}
