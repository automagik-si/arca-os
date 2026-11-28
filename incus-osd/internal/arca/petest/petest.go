// Package petest builds small PE images for tests: unsigned, signed, and signed with the
// signature stripped the way GNU objcopy leaves an image it rewrites in place.
package petest

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/foxboron/go-uefi/authenticode"
)

// Unsigned returns a minimal PE32+ image: headers and one 0x200-byte .text section.
func Unsigned() []byte {
	const (
		lfanew      = 0x40
		optSize     = 240
		fileAlign   = 0x200
		headersSize = 0x200
	)

	b := make([]byte, headersSize+fileAlign)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], lfanew)

	p := lfanew
	copy(b[p:], "PE\x00\x00")
	p += 4

	// COFF header.
	binary.LittleEndian.PutUint16(b[p:], 0x8664) // Machine: amd64.
	binary.LittleEndian.PutUint16(b[p+2:], 1)    // NumberOfSections.
	binary.LittleEndian.PutUint16(b[p+16:], optSize)
	binary.LittleEndian.PutUint16(b[p+18:], 0x22) // Executable, large address aware.
	p += 20

	// Optional header (PE32+).
	o := p
	binary.LittleEndian.PutUint16(b[o:], 0x20b)
	binary.LittleEndian.PutUint32(b[o+16:], 0x1000)      // AddressOfEntryPoint.
	binary.LittleEndian.PutUint64(b[o+24:], 0x140000000) // ImageBase.
	binary.LittleEndian.PutUint32(b[o+32:], 0x1000)      // SectionAlignment.
	binary.LittleEndian.PutUint32(b[o+36:], fileAlign)
	binary.LittleEndian.PutUint32(b[o+56:], 0x2000) // SizeOfImage.
	binary.LittleEndian.PutUint32(b[o+60:], headersSize)
	binary.LittleEndian.PutUint16(b[o+68:], 10)  // Subsystem: EFI application.
	binary.LittleEndian.PutUint32(b[o+108:], 16) // NumberOfRvaAndSizes.
	p += optSize

	// Section header.
	copy(b[p:], ".text")
	binary.LittleEndian.PutUint32(b[p+8:], 0x10)         // VirtualSize.
	binary.LittleEndian.PutUint32(b[p+12:], 0x1000)      // VirtualAddress.
	binary.LittleEndian.PutUint32(b[p+16:], fileAlign)   // SizeOfRawData.
	binary.LittleEndian.PutUint32(b[p+20:], headersSize) // PointerToRawData.
	binary.LittleEndian.PutUint32(b[p+36:], 0x60000020)  // Code, execute, read.

	copy(b[headersSize:], "\xc3 arca test")

	return b
}

// Signer returns a fresh key and its self-signed certificate.
func Signer(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "arca test db"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return key, cert
}

// Signed returns Unsigned signed by a fresh certificate, and that certificate.
func Signed(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()

	key, cert := Signer(t)

	a, err := authenticode.Parse(bytes.NewReader(Unsigned()))
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Sign(key, cert)
	if err != nil {
		t.Fatal(err)
	}

	return a.Bytes(), cert
}

// CertificateTable returns the image's certificate table file offset and size.
func CertificateTable(b []byte) (uint32, uint32) {
	o := int(binary.LittleEndian.Uint32(b[0x3c:])) + 4 + 20

	return binary.LittleEndian.Uint32(b[o+144:]), binary.LittleEndian.Uint32(b[o+148:])
}

// Stripped returns a signed image whose signature data is gone but whose header still points at
// it, as GNU objcopy leaves a signed UKI it rewrites in place; and the certificate that signed it.
func Stripped(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()

	b, cert := Signed(t)

	off, size := CertificateTable(b)
	if size == 0 || int(off)+int(size) != len(b) {
		t.Fatalf("the signed test image has no trailing certificate table (offset %d, size %d, file %d)", off, size, len(b))
	}

	return b[:off], cert
}
