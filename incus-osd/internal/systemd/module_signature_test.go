package systemd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/smallstep/pkcs7"
	"github.com/stretchr/testify/require"
)

func TestKernelModuleSignatureUsesOSCertificate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture OS"}, NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	payload := []byte("module fixture bytes")
	signed, err := pkcs7.NewSignedData(payload)
	require.NoError(t, err)
	signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	require.NoError(t, signed.AddSigner(cert, key, pkcs7.SignerInfoConfig{}))
	signed.Detach()
	signature, err := signed.Finish()
	require.NoError(t, err)

	header := make([]byte, 12)
	header[2] = 2

	length := len(signature)
	for i := range 4 {
		header[11-i] = byte(length & 0xff)
		length >>= 8
	}

	require.Zero(t, length)

	body := slices.Concat(payload, signature, header)
	body = append(body, []byte("~Module signature appended~\n")...)
	module := filepath.Join(root, "nvidia.ko")
	require.NoError(t, os.WriteFile(module, body, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "os.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, VerifyKernelModule(module, root))

	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	wrongDER, err := x509.CreateCertificate(rand.Reader, template, template, &wrongKey.PublicKey, wrongKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "os.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wrongDER}), 0o600))
	require.Error(t, VerifyKernelModule(module, root))
	require.NoError(t, os.WriteFile(filepath.Join(root, "os.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	body[0] ^= 1
	require.NoError(t, os.WriteFile(module, body, 0o600))
	require.Error(t, VerifyKernelModule(module, root))
	require.NoError(t, os.WriteFile(module, payload, 0o600))
	require.Error(t, VerifyKernelModule(module, root))
}
