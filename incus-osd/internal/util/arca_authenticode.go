package util

import (
	"crypto/sha256"
	"crypto/x509"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/foxboron/go-uefi/authenticode"
	"github.com/foxboron/go-uefi/efi/signature"
)

// ErrAuthenticodeUnreadable is a PE image whose Authenticode data cannot be read.
var ErrAuthenticodeUnreadable = errors.New("the PE image cannot be read")

var authenticodeParse = authenticode.Parse

// ParseAuthenticode reads a PE image's Authenticode data like authenticode.Parse, but a damaged
// image is an error wrapping ErrAuthenticodeUnreadable, never a panic. GNU objcopy, for example,
// drops a signed image's signature when it rewrites the file in place, and leaves the header
// pointing at it; authenticode.Parse panics on that.
func ParseAuthenticode(r io.ReaderAt) (*authenticode.PECOFFBinary, error) {
	size, known := readerSize(r)
	if known {
		err := checkCertificateTable(r, size)
		if err != nil {
			return nil, err
		}
	}

	a, err := guarded(func() (*authenticode.PECOFFBinary, error) { return authenticodeParse(r) })
	if err != nil {
		if errors.Is(err, ErrAuthenticodeUnreadable) {
			return nil, err
		}

		return nil, fmt.Errorf("%w: %w", ErrAuthenticodeUnreadable, err)
	}

	return a, nil
}

// VerifyAuthenticode is a.Verify(cert), with a panic of the parser returned as an error.
func VerifyAuthenticode(a *authenticode.PECOFFBinary, cert *x509.Certificate) (bool, error) {
	return guarded(func() (bool, error) { return a.Verify(cert) })
}

// guarded runs f, and returns a panic of f as an error wrapping ErrAuthenticodeUnreadable.
func guarded[T any](f func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)

	func() {
		defer func() {
			p := recover()
			if p != nil {
				err = fmt.Errorf("%w: %v", ErrAuthenticodeUnreadable, p)
			}
		}()

		v, err = f()
	}()

	return v, err
}

func readerSize(r io.ReaderAt) (int64, bool) {
	switch v := r.(type) {
	case interface{ Size() int64 }:
		return v.Size(), true
	case interface{ Stat() (os.FileInfo, error) }:
		fi, err := v.Stat()
		if err != nil {
			return 0, false
		}

		return fi.Size(), true
	default:
		return 0, false
	}
}

// checkCertificateTable checks that the image's certificate table lies in the file, after its
// headers and sections, where authenticode.Parse expects it.
func checkCertificateTable(r io.ReaderAt, size int64) error {
	f, err := pe.NewFile(io.NewSectionReader(r, 0, size))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAuthenticodeUnreadable, err)
	}

	defer f.Close()

	var (
		dd      pe.DataDirectory
		headers int64
	)

	switch oh := f.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		dd, headers = oh.DataDirectory[4], int64(oh.SizeOfHeaders)
	case *pe.OptionalHeader64:
		dd, headers = oh.DataDirectory[4], int64(oh.SizeOfHeaders)
	default:
		return fmt.Errorf("%w: it has no optional header", ErrAuthenticodeUnreadable)
	}

	if dd.Size == 0 {
		return nil
	}

	start, end := int64(dd.VirtualAddress), int64(dd.VirtualAddress)+int64(dd.Size)
	if end > size {
		return fmt.Errorf("%w: its signature table points past the end of the file (%d bytes at offset %d; the file has %d bytes)",
			ErrAuthenticodeUnreadable, dd.Size, start, size)
	}

	// authenticode.Parse hashes the headers and every section, and expects the table in what follows.
	hashed := headers
	for _, s := range f.Sections {
		hashed += int64(s.Size)
	}

	if size-hashed < int64(dd.Size) {
		return fmt.Errorf("%w: its signature table (%d bytes at offset %d) overlaps its sections, which end at %d",
			ErrAuthenticodeUnreadable, dd.Size, start, hashed)
	}

	return nil
}

// CheckSignedUKI checks that a UKI is signed by the certificate whose SHA-256 is fingerprint, as an
// OS update's usr verity signature names it, and that the signature verifies. It is the check a
// node makes before it installs the UKI, done with the certificate the UKI carries.
func CheckSignedUKI(r io.ReaderAt, fingerprint string) error {
	a, err := ParseAuthenticode(r)
	if err != nil {
		return err
	}

	sigs, err := signatures(a)
	if err != nil {
		return err
	}

	if len(sigs) == 0 {
		return errors.New("the UKI has no signature")
	}

	for _, sig := range sigs {
		code, err := authenticode.ParseAuthenticode(sig.Certificate)
		if err != nil {
			return fmt.Errorf("%w: a signature cannot be read: %w", ErrAuthenticodeUnreadable, err)
		}

		for _, cert := range code.Pkcs.Certs {
			fp := sha256.Sum256(cert.Raw)
			if hex.EncodeToString(fp[:]) != fingerprint {
				continue
			}

			_, err := VerifyAuthenticode(a, cert)
			if err != nil {
				return fmt.Errorf("the UKI's signature by the certificate %s does not verify: %w", fingerprint, err)
			}

			return nil
		}
	}

	return errors.New("the UKI is not signed by the certificate " + fingerprint + " its usr image names")
}

func signatures(a *authenticode.PECOFFBinary) ([]*signature.WINCertificate, error) {
	sigs, err := guarded(a.Signatures)
	if err != nil {
		if errors.Is(err, ErrAuthenticodeUnreadable) {
			return nil, err
		}

		return nil, fmt.Errorf("%w: %w", ErrAuthenticodeUnreadable, err)
	}

	return sigs, nil
}
