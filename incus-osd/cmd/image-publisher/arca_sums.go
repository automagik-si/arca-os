package main

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiupdate "github.com/lxc/incus-os/incus-osd/api/images"
)

// buildSumsName is the build's record of its release files, which publish-build.sh ships in the
// archive: the SHA-256 of each file before it was compressed.
const buildSumsName = "SHA256SUMS"

// checkBuildSums refuses a release file that is not what the build made, such as a UKI rewritten in
// place after the build. An archive without the build's sums, as upstream's make targets write it,
// is not checked here.
func checkBuildSums(zr *zip.Reader, targetPath string, files []apiupdate.UpdateFile) error {
	sums, err := readBuildSums(zr)
	if err != nil || sums == nil {
		return err
	}

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f.Filename), ".gz")

		want, listed := sums[name]
		if !listed {
			return fmt.Errorf("refusing to publish %s: the build's %s does not list it", f.Filename, buildSumsName)
		}

		got, err := uncompressedSHA256(filepath.Join(targetPath, f.Filename))
		if err != nil {
			return fmt.Errorf("refusing to publish %s: %w", f.Filename, err)
		}

		if got != want {
			return fmt.Errorf("refusing to publish %s: it changed after the build (sha256 %s, the build recorded %s)", f.Filename, got, want)
		}
	}

	return nil
}

// readBuildSums returns the archive's build sums by file name, or nil when it has none.
func readBuildSums(zr *zip.Reader) (map[string]string, error) {
	for _, f := range zr.File {
		if f.Name == buildSumsName {
			return parseBuildSums(f)
		}
	}

	return nil, nil //nolint:nilnil // no sums is not an error
}

func parseBuildSums(f *zip.File) (map[string]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}

	defer rc.Close()

	sums := map[string]string{}

	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("the build's %s has a bad line: %q", buildSumsName, sc.Text())
		}

		sums[strings.TrimPrefix(parts[1], "*")] = parts[0]
	}

	err = sc.Err()
	if err != nil {
		return nil, err
	}

	return sums, nil
}

// uncompressedSHA256 is the SHA-256 of a published file as the build made it.
func uncompressedSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		return "", err
	}

	defer f.Close()

	var r io.Reader = f

	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return "", errors.New("it cannot be decompressed: " + err.Error())
		}

		defer zr.Close()

		r = zr
	}

	h := sha256.New()

	_, err = io.Copy(h, r)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
