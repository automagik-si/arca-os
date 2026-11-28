package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	apiupdate "github.com/lxc/incus-os/incus-osd/api/images"
	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// checkReleaseUKI refuses a release whose UKI a node would refuse to install: one that cannot be
// read, or that isn't signed by the certificate the usr verity signature names. A node only finds
// out after downloading the release, and an older node may crash on a damaged UKI.
func checkReleaseUKI(targetPath string, files []apiupdate.UpdateFile) error {
	var uki, sig string

	for _, f := range files {
		switch f.Type { //nolint:exhaustive
		case apiupdate.UpdateFileTypeUpdateEFI:
			uki = f.Filename
		case apiupdate.UpdateFileTypeUpdateUsrVeritySignature:
			sig = f.Filename
		}
	}

	if uki == "" {
		return nil
	}

	err := checkUKIFile(filepath.Join(targetPath, uki), sig, targetPath)
	if err != nil {
		return fmt.Errorf("refusing to publish %s: %w", uki, err)
	}

	return nil
}

func checkUKIFile(ukiPath string, sig string, targetPath string) error {
	if sig == "" {
		return errors.New("the release has no usr verity signature to check the UKI against")
	}

	fingerprint, err := veritySignatureFingerprint(filepath.Join(targetPath, sig))
	if err != nil {
		return err
	}

	// The UKI is published compressed; check it as a node installs it.
	tmp, err := os.CreateTemp(filepath.Dir(ukiPath), ".uki-check-*")
	if err != nil {
		return err
	}

	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	err = gunzipTo(tmp, ukiPath)
	if err != nil {
		return err
	}

	return util.CheckSignedUKI(tmp, fingerprint)
}

func gunzipTo(w io.Writer, path string) error {
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the UKI cannot be decompressed: %w", err)
	}

	for {
		_, err := io.CopyN(w, zr, 4*1024*1024)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("the UKI cannot be decompressed: %w", err)
		}
	}
}

// veritySignatureFingerprint returns the certificate fingerprint a compressed usr verity signature names.
func veritySignatureFingerprint(path string) (string, error) {
	var buf bytes.Buffer

	err := gunzipTo(&buf, path)
	if err != nil {
		return "", fmt.Errorf("the usr verity signature cannot be read: %w", err)
	}

	var meta struct {
		CertificateFingerprint string `json:"certificateFingerprint"`
	}

	err = json.Unmarshal(bytes.Trim(buf.Bytes(), "\x00"), &meta)
	if err != nil {
		return "", fmt.Errorf("the usr verity signature cannot be read: %w", err)
	}

	if meta.CertificateFingerprint == "" {
		return "", errors.New("the usr verity signature names no certificate")
	}

	return meta.CertificateFingerprint, nil
}
