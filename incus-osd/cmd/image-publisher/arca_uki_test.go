package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/arca/petest"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := gzip.NewWriter(&buf)
	_, err := w.Write(b)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return buf.Bytes()
}

// releaseZip writes an OS release archive holding uki as its UKI and a usr verity signature naming
// the certificate whose SHA-256 is fingerprint, padded with zeroes as the partition image is.
func releaseZip(t *testing.T, uki []byte, fingerprint string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "IncusOS-202611272126-x86_64.zip")

	f, err := os.Create(p)
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	sig := []byte(`{"rootHash":"00","certificateFingerprint":"` + fingerprint + `","signature":"AA=="}`)
	sig = append(sig, make([]byte, 4096)...)

	for name, body := range map[string][]byte{
		"IncusOS_202611272126.efi.gz": gz(t, uki),
		"IncusOS_202611272126.usr-x86-64-verity-sig.de3a3a5441b349e1ae83f6aadb04adf5.raw.gz": gz(t, sig),
	} {
		w, err := zw.Create(name)
		require.NoError(t, err)

		_, err = w.Write(body)
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	return p
}

func certFingerprint(raw []byte) string {
	fp := sha256.Sum256(raw)

	return hex.EncodeToString(fp[:])
}

func TestDownloadImageSignedUKI(t *testing.T) {
	t.Parallel()

	b, cert := petest.Signed(t)

	files, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: releaseZip(t, b, certFingerprint(cert.Raw))}, t.TempDir())
	require.NoError(t, err)
	require.Len(t, files, 2)
}

// The release that crashed the node: its UKI's signature was stripped after the build.
func TestDownloadImageRefusesAStrippedUKI(t *testing.T) {
	t.Parallel()

	b, cert := petest.Stripped(t)

	_, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: releaseZip(t, b, certFingerprint(cert.Raw))}, t.TempDir())
	require.ErrorContains(t, err, "refusing to publish x86_64/IncusOS_202611272126.efi.gz")
	require.ErrorContains(t, err, "signature table points past the end of the file")
}

func TestDownloadImageRefusesAUKIOfAnotherCertificate(t *testing.T) {
	t.Parallel()

	b, _ := petest.Signed(t)
	_, other := petest.Signer(t)

	_, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: releaseZip(t, b, certFingerprint(other.Raw))}, t.TempDir())
	require.ErrorContains(t, err, "is not signed by the certificate "+certFingerprint(other.Raw))
}

func TestDownloadImageRefusesAUKIWithoutVeritySignature(t *testing.T) {
	t.Parallel()

	b, _ := petest.Signed(t)
	p := filepath.Join(t.TempDir(), "IncusOS-202611272126-x86_64.zip")

	f, err := os.Create(p)
	require.NoError(t, err)

	zw := zip.NewWriter(f)
	w, err := zw.Create("IncusOS_202611272126.efi.gz")
	require.NoError(t, err)

	_, err = w.Write(gz(t, b))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	_, err = (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: p}, t.TempDir())
	require.ErrorContains(t, err, "no usr verity signature")
}

// A refused release leaves no folder behind, so the next sync of a fixed build imports it.
func TestImportReleaseRefusedLeavesNoFolder(t *testing.T) {
	t.Parallel()

	b, cert := petest.Stripped(t)
	target := t.TempDir()

	rel := release{name: "202611272126", urls: map[string]*url.URL{"x86_64": {Path: releaseZip(t, b, certFingerprint(cert.Raw))}}}

	_, err := (&cmdSync{}).importRelease(t.Context(), target, rel, "stable")
	require.ErrorContains(t, err, "refusing to publish")
	require.NoDirExists(t, filepath.Join(target, "202611272126"))
}
