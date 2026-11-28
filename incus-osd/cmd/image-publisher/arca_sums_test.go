package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus-os/incus-osd/internal/arca/petest"
)

const (
	sumsUKI = "IncusOS_202611272126.efi"
	sumsSig = "IncusOS_202611272126.usr-x86-64-verity-sig.de3a3a5441b349e1ae83f6aadb04adf5.raw"
)

func sha(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

// summedZip writes a release archive of the given uncompressed files, each gzipped as publish-build.sh
// ships it, and a SHA256SUMS of the files as the build recorded them.
func summedZip(t *testing.T, files map[string][]byte, built map[string][]byte) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "IncusOS-202611272126-x86_64.zip")

	f, err := os.Create(p)
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	for name, body := range files {
		w, err := zw.Create(name + ".gz")
		require.NoError(t, err)

		_, err = w.Write(gz(t, body))
		require.NoError(t, err)
	}

	var sums strings.Builder
	for name, body := range built {
		_, _ = sums.WriteString(sha(body) + "  " + name + "\n")
	}

	w, err := zw.Create("SHA256SUMS")
	require.NoError(t, err)

	_, err = w.Write([]byte(sums.String()))
	require.NoError(t, err)

	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	return p
}

func signedRelease(t *testing.T) ([]byte, []byte) {
	t.Helper()

	b, cert := petest.Signed(t)
	sig := []byte(`{"rootHash":"00","certificateFingerprint":"` + certFingerprint(cert.Raw) + `","signature":"AA=="}`)

	return b, append(sig, make([]byte, 4096)...)
}

func TestDownloadImageChecksTheBuildSums(t *testing.T) {
	t.Parallel()

	uki, sig := signedRelease(t)
	files := map[string][]byte{sumsUKI: uki, sumsSig: sig}

	got, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64", &url.URL{Path: summedZip(t, files, files)}, t.TempDir())
	require.NoError(t, err)
	require.Len(t, got, 2)
}

// The incident: the UKI was rewritten in place between the build and the publish.
func TestDownloadImageRefusesAFileChangedAfterTheBuild(t *testing.T) {
	t.Parallel()

	uki, sig := signedRelease(t)
	changed := append([]byte{}, uki...)
	changed[len(changed)-1] ^= 0xff

	_, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64",
		&url.URL{Path: summedZip(t, map[string][]byte{sumsUKI: changed, sumsSig: sig}, map[string][]byte{sumsUKI: uki, sumsSig: sig})}, t.TempDir())
	require.ErrorContains(t, err, "refusing to publish x86_64/"+sumsUKI+".gz: it changed after the build (sha256 "+sha(changed)+
		", the build recorded "+sha(uki)+")")
}

func TestDownloadImageRefusesAFileTheBuildDidNotList(t *testing.T) {
	t.Parallel()

	uki, sig := signedRelease(t)

	_, err := (&cmdSync{}).downloadImage(t.Context(), "x86_64",
		&url.URL{Path: summedZip(t, map[string][]byte{sumsUKI: uki, sumsSig: sig}, map[string][]byte{sumsUKI: uki})}, t.TempDir())
	require.ErrorContains(t, err, "refusing to publish x86_64/"+sumsSig+".gz: the build's SHA256SUMS does not list it")
}
