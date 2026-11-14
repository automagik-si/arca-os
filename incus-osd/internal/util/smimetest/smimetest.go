// Package smimetest signs S/MIME messages with throwaway certificates for tests.
package smimetest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"
)

// Identity is a key and its certificate.
type Identity struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// RequireOpenSSL skips the test when openssl isn't available.
func RequireOpenSSL(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl isn't available")
	}
}

// NewIdentity returns a new key and its certificate, self-signed when parent is nil.
func NewIdentity(t *testing.T, name string, serial int64, parent *Identity, isCA bool) *Identity {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}

	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}

	issuer := &Identity{Cert: template, Key: key}
	if parent != nil {
		issuer = parent
	}

	der, err := x509.CreateCertificate(rand.Reader, template, issuer.Cert, key.Public(), issuer.Key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	return &Identity{Cert: cert, Key: key}
}

// PEM returns the identities' certificates, PEM-encoded.
func PEM(ids ...*Identity) []byte {
	var buf bytes.Buffer

	for _, id := range ids {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: id.Cert.Raw})
	}

	return buf.Bytes()
}

// Sign signs content the way the image publisher does, adding chain to the message.
func Sign(t *testing.T, content string, signer *Identity, chain ...*Identity) []byte {
	t.Helper()

	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	out := filepath.Join(dir, "out")
	certFile := filepath.Join(dir, "signer.crt")
	keyFile := filepath.Join(dir, "signer.key")
	chainFile := filepath.Join(dir, "chain.crt")

	der, err := x509.MarshalECPrivateKey(signer.Key)
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		in:        []byte(content),
		certFile:  PEM(signer),
		keyFile:   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
		chainFile: PEM(chain...),
	} {
		err := os.WriteFile(name, data, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	args := []string{"smime", "-text", "-sign", "-in", in, "-out", out, "-signer", certFile, "-inkey", keyFile}
	if len(chain) > 0 {
		args = append(args, "-certfile", chainFile)
	}

	_, err = subprocess.RunCommandContext(t.Context(), "openssl", args...)
	if err != nil {
		t.Fatal(err)
	}

	message, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	return message
}
