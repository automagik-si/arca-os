package util

import (
	"crypto/x509"
	"errors"
	"io"

	"github.com/foxboron/go-uefi/authenticode"
)

// ErrAuthenticodeUnreadable is a PE image whose Authenticode data cannot be read.
var ErrAuthenticodeUnreadable = errors.New("the PE image cannot be read")

var authenticodeParse = authenticode.Parse

// ParseAuthenticode reads a PE image's Authenticode data.
func ParseAuthenticode(r io.ReaderAt) (*authenticode.PECOFFBinary, error) {
	return authenticodeParse(r)
}

// VerifyAuthenticode verifies a parsed PE image against a certificate.
func VerifyAuthenticode(a *authenticode.PECOFFBinary, cert *x509.Certificate) (bool, error) {
	return a.Verify(cert)
}
