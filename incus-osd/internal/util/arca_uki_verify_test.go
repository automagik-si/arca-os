package util

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/arca/petest"
)

func writeUKI(t *testing.T, b []byte) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "IncusOS_202611272126.efi")
	require.NoError(t, os.WriteFile(p, b, 0o600))

	return p
}

func TestVerifyUKISigned(t *testing.T) {
	t.Parallel()

	b, cert := petest.Signed(t)

	require.NoError(t, VerifyUKIFile(writeUKI(t, b), "202611272126", cert))
}

// The update that crashed: a UKI whose signature objcopy stripped is a UKIError naming the version.
func TestVerifyUKIStripped(t *testing.T) {
	t.Parallel()

	b, cert := petest.Stripped(t)

	err := VerifyUKIFile(writeUKI(t, b), "202611272126", cert)
	require.Error(t, err)
	require.True(t, IsUKIRejected(err), "not a rejected UKI: %v", err)
	require.ErrorIs(t, err, ErrAuthenticodeUnreadable)
	require.Contains(t, err.Error(), "202611272126")
	require.Contains(t, err.Error(), "signature table points past the end of the file")
}

func TestVerifyUKIUnsigned(t *testing.T) {
	t.Parallel()

	_, cert := petest.Signer(t)

	err := VerifyUKIFile(writeUKI(t, petest.Unsigned()), "202611272126", cert)
	require.True(t, IsUKIRejected(err), "not a rejected UKI: %v", err)
	require.Contains(t, err.Error(), "202611272126")
}

func TestVerifyUKIOtherCertificate(t *testing.T) {
	t.Parallel()

	b, _ := petest.Signed(t)
	_, other := petest.Signer(t)

	err := VerifyUKIFile(writeUKI(t, b), "202611272126", other)
	require.True(t, IsUKIRejected(err), "not a rejected UKI: %v", err)
}

// A UKI that is not there is a local problem, not a rejected release.
func TestVerifyUKIMissing(t *testing.T) {
	t.Parallel()

	_, cert := petest.Signer(t)

	err := VerifyUKIFile(filepath.Join(t.TempDir(), "none.efi"), "202611272126", cert)
	require.Error(t, err)
	require.False(t, IsUKIRejected(err))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUKIErrorWording(t *testing.T) {
	t.Parallel()

	err := &UKIError{Version: "202611272126", Err: errors.New("why")}
	require.True(t, strings.HasPrefix(err.Error(), "the UKI of OS version 202611272126 cannot be verified"), err.Error())
}
