package util

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
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/smallstep/pkcs7"
	"github.com/stretchr/testify/require"
)

type smimeIdentity struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// newSMIMEIdentity returns a new key and its certificate, self-signed when parent is nil.
func newSMIMEIdentity(t *testing.T, name string, serial int64, parent *smimeIdentity, isCA bool) *smimeIdentity {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

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

	issuer := &smimeIdentity{cert: template, key: key}
	if parent != nil {
		issuer = parent
	}

	der, err := x509.CreateCertificate(rand.Reader, template, issuer.cert, key.Public(), issuer.key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &smimeIdentity{cert: cert, key: key}
}

func certPEM(certs ...*smimeIdentity) []byte {
	var buf bytes.Buffer

	for _, c := range certs {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
	}

	return buf.Bytes()
}

// signSMIME signs content the way the image publisher does.
func signSMIME(t *testing.T, content string, signers []*smimeIdentity, chain []*smimeIdentity) []byte {
	t.Helper()

	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	out := filepath.Join(dir, "out")
	chainFile := filepath.Join(dir, "chain.crt")

	require.NoError(t, os.WriteFile(in, []byte(content), 0o600))
	require.NoError(t, os.WriteFile(chainFile, certPEM(chain...), 0o600))

	args := []string{"smime", "-text", "-sign", "-in", in, "-out", out}
	if len(chain) > 0 {
		args = append(args, "-certfile", chainFile)
	}

	for i, signer := range signers {
		certFile := filepath.Join(dir, "signer"+string(rune('0'+i))+".crt")
		keyFile := filepath.Join(dir, "signer"+string(rune('0'+i))+".key")

		der, err := x509.MarshalECPrivateKey(signer.key)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(certFile, certPEM(signer), 0o600))
		require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600))

		args = append(args, "-signer", certFile, "-inkey", keyFile)
	}

	_, err := subprocess.RunCommandContext(t.Context(), "openssl", args...)
	require.NoError(t, err)

	message, err := os.ReadFile(out)
	require.NoError(t, err)

	return message
}

func TestVerifySMIMESignerIssuer(t *testing.T) {
	t.Parallel()

	_, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl isn't available")
	}

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	otherCA := newSMIMEIdentity(t, "Other CA", 3, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, expectedCA, false)
	secondLeaf := newSMIMEIdentity(t, "Second leaf", 2, expectedCA, false)
	otherLeaf := newSMIMEIdentity(t, "Other leaf", 1, otherCA, false)
	rootLeaf := newSMIMEIdentity(t, "Root leaf", 4, root, false)
	unrelatedRoot := newSMIMEIdentity(t, "Unrelated root", 1, nil, true)
	unrelatedLeaf := newSMIMEIdentity(t, "Unrelated leaf", 1, unrelatedRoot, false)

	const notIssued = "was not signed by one of the following expected intermediate CAs"

	tests := []struct {
		name    string
		signers []*smimeIdentity
		chain   []*smimeIdentity
		wantErr string
	}{
		{name: "signer issued by the expected CA", signers: []*smimeIdentity{leaf}, chain: []*smimeIdentity{expectedCA}},
		{name: "expected CA present, signer issued by another CA", signers: []*smimeIdentity{otherLeaf}, chain: []*smimeIdentity{otherCA, expectedCA}, wantErr: notIssued},
		{name: "expected CA present, signer issued by the root", signers: []*smimeIdentity{rootLeaf}, chain: []*smimeIdentity{expectedCA}, wantErr: notIssued},
		{name: "expected CA absent, signer issued by another CA", signers: []*smimeIdentity{otherLeaf}, chain: []*smimeIdentity{otherCA}, wantErr: notIssued},
		{name: "signer is the expected CA", signers: []*smimeIdentity{expectedCA}, wantErr: notIssued},
		{name: "two signers", signers: []*smimeIdentity{leaf, secondLeaf}, chain: []*smimeIdentity{expectedCA}, wantErr: "must have exactly one signer"},
		{name: "signer not under the root", signers: []*smimeIdentity{unrelatedLeaf}, chain: []*smimeIdentity{expectedCA}, wantErr: "missing or unverifiable CA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			message := signSMIME(t, "signed content", tt.signers, tt.chain)

			verified, err := verifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, "signed content", strings.TrimSpace(verified.String()))
		})
	}
}

func TestSMIMESigner(t *testing.T) {
	t.Parallel()

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	ca := newSMIMEIdentity(t, "CA", 2, root, true)
	sameNameCA := newSMIMEIdentity(t, "CA", 3, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, ca, false)
	sameSerialLeaf := newSMIMEIdentity(t, "Leaf", 1, sameNameCA, false)

	parse := func(extra ...*smimeIdentity) *pkcs7.PKCS7 {
		signed, err := pkcs7.NewSignedData([]byte("signed content"))
		require.NoError(t, err)

		signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
		require.NoError(t, signed.AddSigner(leaf.cert, leaf.key, pkcs7.SignerInfoConfig{}))

		for _, c := range extra {
			signed.AddCertificate(c.cert)
		}

		der, err := signed.Finish()
		require.NoError(t, err)

		parsed, err := pkcs7.Parse(der)
		require.NoError(t, err)

		return parsed
	}

	// A single signer, with other certificates alongside.
	signer, err := smimeSigner(parse(ca))
	require.NoError(t, err)
	require.True(t, signer.Equal(leaf.cert))

	// A second certificate with the signer's issuer name and serial number.
	_, err = smimeSigner(parse(sameSerialLeaf))
	require.ErrorContains(t, err, "more than one certificate for its signer")

	// No signer at all.
	der, err := pkcs7.DegenerateCertificate(leaf.cert.Raw)
	require.NoError(t, err)

	unsigned, err := pkcs7.Parse(der)
	require.NoError(t, err)

	_, err = smimeSigner(unsigned)
	require.ErrorContains(t, err, "must have exactly one signer")
}
