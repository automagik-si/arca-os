package util

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/foxboron/go-uefi/authenticode"

	"github.com/lxc/incus-os/incus-osd/internal/arca/petest"
)

func TestParseAuthenticodeSigned(t *testing.T) {
	b, cert := petest.Signed(t)

	a, err := ParseAuthenticode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("a signed image does not parse: %v", err)
	}

	ok, err := VerifyAuthenticode(a, cert)
	if err != nil || !ok {
		t.Fatalf("a signed image does not verify: %v %v", ok, err)
	}
}

func TestParseAuthenticodeUnsigned(t *testing.T) {
	a, err := ParseAuthenticode(bytes.NewReader(petest.Unsigned()))
	if err != nil {
		t.Fatalf("an unsigned image does not parse: %v", err)
	}

	_, cert := petest.Signer(t)

	ok, err := VerifyAuthenticode(a, cert)
	if err == nil || ok {
		t.Fatalf("an unsigned image verifies: %v %v", ok, err)
	}
}

// The upstream parser panics on the stripped image; this is the crash the update hit.
func TestUpstreamParsePanicsOnStrippedImage(t *testing.T) {
	b, _ := petest.Stripped(t)

	defer func() {
		if recover() == nil {
			t.Fatal("authenticode.Parse no longer panics on a dangling certificate table; ParseAuthenticode's guard may be dropped")
		}
	}()

	_, _ = authenticode.Parse(bytes.NewReader(b))
}

func TestParseAuthenticodeStrippedImage(t *testing.T) {
	b, _ := petest.Stripped(t)

	_, err := ParseAuthenticode(bytes.NewReader(b))
	if err == nil {
		t.Fatal("an image whose certificate table points past its end parses")
	}

	if !errors.Is(err, ErrAuthenticodeUnreadable) {
		t.Fatalf("the error is not ErrAuthenticodeUnreadable: %v", err)
	}

	if !strings.Contains(err.Error(), "signature table points past the end of the file") {
		t.Fatalf("the error does not say why: %v", err)
	}
}

func TestParseAuthenticodeTruncatedImage(t *testing.T) {
	b, _ := petest.Signed(t)

	_, err := ParseAuthenticode(bytes.NewReader(b[:0x100]))
	if !errors.Is(err, ErrAuthenticodeUnreadable) {
		t.Fatalf("a truncated image: %v", err)
	}
}

func TestParseAuthenticodeRecoversPanic(t *testing.T) {
	orig := authenticodeParse

	t.Cleanup(func() { authenticodeParse = orig })

	authenticodeParse = func(io.ReaderAt) (*authenticode.PECOFFBinary, error) {
		panic("boom")
	}

	_, err := ParseAuthenticode(bytes.NewReader(petest.Unsigned()))
	if !errors.Is(err, ErrAuthenticodeUnreadable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a parser panic is not an error: %v", err)
	}
}

func TestVerifyAuthenticodeRecoversPanic(t *testing.T) {
	_, cert := petest.Signer(t)

	ok, err := VerifyAuthenticode(&authenticode.PECOFFBinary{}, cert)
	if ok || err == nil {
		t.Fatalf("verifying an empty binary: %v %v", ok, err)
	}
}

func fingerprint(b []byte) string {
	fp := sha256.Sum256(b)

	return hex.EncodeToString(fp[:])
}

func TestCheckSignedUKI(t *testing.T) {
	b, cert := petest.Signed(t)

	require := func(err error, want string) {
		t.Helper()

		if want == "" && err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("want an error saying %q, got %v", want, err)
		}
	}

	// Signed by the certificate the usr image names.
	require(CheckSignedUKI(bytes.NewReader(b), fingerprint(cert.Raw)), "")

	// Signed, but by another certificate.
	_, other := petest.Signer(t)
	require(CheckSignedUKI(bytes.NewReader(b), fingerprint(other.Raw)), "is not signed by the certificate "+fingerprint(other.Raw)[:16])

	// Not signed.
	require(CheckSignedUKI(bytes.NewReader(petest.Unsigned()), fingerprint(cert.Raw)), "has no signature")

	// The signature stripped, as the published 202611272126 UKI was.
	stripped, strippedCert := petest.Stripped(t)
	require(CheckSignedUKI(bytes.NewReader(stripped), fingerprint(strippedCert.Raw)), "signature table points past the end of the file")
}
