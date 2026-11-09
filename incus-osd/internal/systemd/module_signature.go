package systemd

import (
	"bytes"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"

	"github.com/smallstep/pkcs7"
)

// VerifyKernelModule verifies the appended Linux module CMS signature against the OS certificates.
func VerifyKernelModule(module string, certificateDirectory string) error {
	data, err := os.ReadFile(module)
	if err != nil {
		return errors.New("the GPU module could not be read")
	}

	magic := []byte("~Module signature appended~\n")
	if !bytes.HasSuffix(data, magic) || len(data) < len(magic)+12 {
		return errors.New("the GPU module has no signature")
	}

	end := int64(len(data) - len(magic))
	header := data[end-12 : end]

	length := int64(binary.BigEndian.Uint32(header[8:]))
	if header[2] != 2 || length == 0 || length > end-12 {
		return errors.New("the GPU module signature is invalid")
	}

	signature, err := pkcs7.Parse(data[end-12-length : end-12])
	if err != nil {
		return errors.New("the GPU module signature is invalid")
	}

	signature.Content = data[:end-12-length]

	certificates, _ := filepath.Glob(filepath.Join(certificateDirectory, "*.crt"))
	for _, name := range certificates {
		body, err := os.ReadFile(name)
		if err != nil {
			continue
		}

		block, _ := pem.Decode(body)
		if block == nil {
			continue
		}

		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}

		signature.Certificates = []*x509.Certificate{certificate}

		err = signature.Verify()
		if err == nil {
			return nil
		}
	}

	return errors.New("the GPU module is not signed by this operating system")
}
