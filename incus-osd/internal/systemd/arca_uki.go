package systemd

import (
	"crypto/x509"
	"errors"
	"os"

	"github.com/foxboron/go-uefi/authenticode"
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

func verifyUKI(path string, _ string, cert *x509.Certificate) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	a, err := authenticode.Parse(f)
	if err != nil {
		return err
	}

	_, err = a.Verify(cert)

	return err
}
