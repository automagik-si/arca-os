package util

import (
	"crypto/x509"
	"errors"
	"os"
)

// UKIError is an OS update whose UKI cannot be read or verified.
type UKIError struct {
	Version string
	Err     error
}

func (e *UKIError) Error() string {
	return "the UKI of OS version " + e.Version + " cannot be verified: " + e.Err.Error()
}

func (e *UKIError) Unwrap() error {
	return e.Err
}

// IsUKIRejected reports whether err is an OS update whose UKI cannot be read or verified.
func IsUKIRejected(err error) bool {
	var u *UKIError

	return errors.As(err, &u)
}

// VerifyUKIFile checks that the UKI at path is signed by cert. A UKI that cannot be read or verified is a
// UKIError; the parser of a damaged image returns an error instead of panicking.
func VerifyUKIFile(path string, version string, cert *x509.Certificate) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	a, err := ParseAuthenticode(f)
	if err != nil {
		return &UKIError{Version: version, Err: err}
	}

	ok, err := VerifyAuthenticode(a, cert)
	if err != nil {
		return &UKIError{Version: version, Err: err}
	}

	if !ok {
		return &UKIError{Version: version, Err: errors.New("it is not signed by the trusted certificate")}
	}

	return nil
}
