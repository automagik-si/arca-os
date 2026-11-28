package update

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/systemd"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// The update that crashed: the UKI of 202611272126 had its signature stripped after the build.
func strippedUKIError() error {
	return fmt.Errorf("applying: %w", &systemd.UKIError{
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
	require.Contains(t, st.LastError, "the UKI of OS version 202611272126 cannot be verified")
	require.Contains(t, st.LastError, "signature table points past the end of the file")
}

func TestUnverifiedUKIIsASignatureError(t *testing.T) {
	t.Parallel()

	s := runningState("202611172310")

	recordCheck(t.Context(), s, &systemd.UKIError{Version: "202611272126", Err: errors.New("binary has no valid signatures")})

	require.Equal(t, ErrorKindSignature, s.System.Update.State.LastErrorKind)
}

// A rejected UKI is not downloaded and failed again on the next check: the version is held until a
// newer one comes or the owner asks to try it again.
func TestRejectedUKIIsNotRetried(t *testing.T) {
	t.Parallel()

	s := runningState("202611172310")

	require.True(t, rejectOSUpdate(t.Context(), s, "202611272126", strippedUKIError()))
	require.True(t, IsFailedVersion(s, "202611272126"))
	require.Contains(t, s.UpdateHealth.FailedVersions["202611272126"], "the UKI of OS version 202611272126 cannot be verified")
	require.Equal(t, "202611172310", s.OS.RunningRelease)
	require.Empty(t, s.OS.NextRelease)
	require.False(t, s.System.Update.State.NeedsReboot)

	// stubUpdate fails any download, so a check that tried to apply the version again would fail.
	require.NoError(t, checkOSOn(t, s, &stubProvider{os: "202611272126"}))
	require.True(t, strings.HasPrefix(s.System.Update.State.Status, "Version 202611272126 failed before (the UKI of OS version 202611272126"),
		s.System.Update.State.Status)

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
