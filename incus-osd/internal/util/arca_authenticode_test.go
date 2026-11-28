package util

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/foxboron/go-uefi/authenticode"
)

// tinyPE returns a minimal PE32+ image: headers and one 0x200-byte .text section.
func tinyPE() []byte {
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
	binary.LittleEndian.PutUint16(b[p:], 0x8664)  // Machine: amd64.
	binary.LittleEndian.PutUint16(b[p+2:], 1)     // NumberOfSections.
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

func testSigner(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
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

// signedPE returns tinyPE signed by a fresh certificate, and that certificate.
func signedPE(t *testing.T) ([]byte, *x509.Certificate) {
	t.Helper()

	key, cert := testSigner(t)

	a, err := authenticode.Parse(bytes.NewReader(tinyPE()))
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.Sign(key, cert)
	if err != nil {
		t.Fatal(err)
	}

	return a.Bytes(), cert
}

// certTable returns the certificate table's file offset and size.
func certTable(b []byte) (uint32, uint32) {
	o := int(binary.LittleEndian.Uint32(b[0x3c:])) + 4 + 20

	return binary.LittleEndian.Uint32(b[o+144:]), binary.LittleEndian.Uint32(b[o+148:])
}

// strippedPE is a signed image whose signature data is gone but whose header still points at it, as GNU
// objcopy leaves a signed UKI it rewrites in place.
func strippedPE(t *testing.T) []byte {
	t.Helper()

	b, _ := signedPE(t)

	off, size := certTable(b)
	if size == 0 || int(off)+int(size) != len(b) {
		t.Fatalf("the signed test image has no trailing certificate table (offset %d, size %d, file %d)", off, size, len(b))
	}

	return b[:off]
}

func TestParseAuthenticodeSigned(t *testing.T) {
	b, cert := signedPE(t)

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
	a, err := ParseAuthenticode(bytes.NewReader(tinyPE()))
	if err != nil {
		t.Fatalf("an unsigned image does not parse: %v", err)
	}

	_, cert := testSigner(t)

	ok, err := VerifyAuthenticode(a, cert)
	if err == nil || ok {
		t.Fatalf("an unsigned image verifies: %v %v", ok, err)
	}
}

// The upstream parser panics on the stripped image; this is the crash the update hit.
func TestUpstreamParsePanicsOnStrippedImage(t *testing.T) {
	b := strippedPE(t)

	defer func() {
		if recover() == nil {
			t.Fatal("authenticode.Parse no longer panics on a dangling certificate table; ParseAuthenticode's guard may be dropped")
		}
	}()

	_, _ = authenticode.Parse(bytes.NewReader(b))
}

func TestParseAuthenticodeStrippedImage(t *testing.T) {
	b := strippedPE(t)

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
	b, _ := signedPE(t)

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

	_, err := ParseAuthenticode(bytes.NewReader(tinyPE()))
	if !errors.Is(err, ErrAuthenticodeUnreadable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a parser panic is not an error: %v", err)
	}
}

func TestVerifyAuthenticodeRecoversPanic(t *testing.T) {
	_, cert := testSigner(t)

	ok, err := VerifyAuthenticode(&authenticode.PECOFFBinary{}, cert)
	if ok || err == nil {
		t.Fatalf("verifying an empty binary: %v %v", ok, err)
	}
}
