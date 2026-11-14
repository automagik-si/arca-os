package util

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	mrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	return newSMIMEIdentityWithSubject(t, name, nil, serial, parent, isCA)
}

// newSMIMEIdentityWithSubject is newSMIMEIdentity with an optional DER-encoded subject.
func newSMIMEIdentityWithSubject(t *testing.T, name string, rawSubject []byte, serial int64, parent *smimeIdentity, isCA bool) *smimeIdentity {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		RawSubject:            rawSubject,
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

func requireOpenSSL(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl isn't available")
	}
}

func TestVerifySMIMESignerIssuer(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

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

			verified, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, "signed content", strings.TrimSpace(verified.String()))
		})
	}
}

func TestVerifySMIMETrustsOnlyProvidedRoot(t *testing.T) { //nolint:paralleltest // sets SSL_CERT_DIR
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	storeRoot := newSMIMEIdentity(t, "Store root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, storeRoot, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, expectedCA, false)

	// A default certificate directory holding a root other than the provided one.
	storeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(storeDir, "store-root.crt"), certPEM(storeRoot), 0o600))

	_, err := subprocess.RunCommandContext(t.Context(), "openssl", "rehash", storeDir)
	require.NoError(t, err)

	t.Setenv("SSL_CERT_DIR", storeDir)

	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})

	_, err = VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)
	require.ErrorContains(t, err, "missing or unverifiable CA")

	// The same message verifies when its root is the provided one.
	_, err = VerifySMIMEWithRoot(t.Context(), storeRoot.cert, []*x509.Certificate{expectedCA.cert}, message)
	require.NoError(t, err)
}

// utf8CommonName returns the DER encoding of a subject whose common name is a UTF8String.
func utf8CommonName(t *testing.T, name string) []byte {
	t.Helper()

	value, err := asn1.MarshalWithParams(name, "utf8")
	require.NoError(t, err)

	der, err := asn1.Marshal(pkix.RDNSequence{{{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: asn1.RawValue{FullBytes: value}}}})
	require.NoError(t, err)

	return der
}

// smimeMessage wraps DER-encoded signed data as an S/MIME message.
func smimeMessage(der []byte) []byte {
	message := "MIME-Version: 1.0\n" +
		"Content-Type: application/x-pkcs7-mime; smime-type=signed-data; name=\"smime.p7m\"\n" +
		"Content-Transfer-Encoding: base64\n\n"

	lines := []string{}
	for line := range slices.Chunk([]byte(base64.StdEncoding.EncodeToString(der)), 64) {
		lines = append(lines, string(line))
	}

	return []byte(message + strings.Join(lines, "\n") + "\n")
}

func TestVerifySMIMEChecksVerifiedSigner(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, expectedCA, false)

	// A second CA whose name differs from the expected CA's only in its string encoding.
	otherCA := newSMIMEIdentityWithSubject(t, "Expected CA", utf8CommonName(t, "Expected CA"), 3, root, true)
	otherLeaf := newSMIMEIdentity(t, "Leaf", 1, otherCA, false)

	require.NotEqual(t, expectedCA.cert.RawSubject, otherCA.cert.RawSubject)
	require.Equal(t, expectedCA.cert.Subject.String(), otherCA.cert.Subject.String())

	// Both leaves are in the message and the signature verifies with the second one.
	signed, err := pkcs7.NewSignedData([]byte("Content-Type: text/plain\r\n\r\nsigned content\r\n"))
	require.NoError(t, err)

	signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	signed.AddCertificate(otherLeaf.cert)
	signed.AddCertificate(otherCA.cert)
	require.NoError(t, signed.AddSigner(leaf.cert, otherLeaf.key, pkcs7.SignerInfoConfig{}))

	der, err := signed.Finish()
	require.NoError(t, err)

	_, err = VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, smimeMessage(der))
	require.ErrorContains(t, err, "was not signed by one of the following expected intermediate CAs")
}

func TestSMIMEVerifyArgs(t *testing.T) {
	t.Parallel()

	args := smimeVerifyArgs("root.crt", "signer.crt")

	// Only the provided root CA is trusted.
	require.Contains(t, args, "-no-CApath")
	require.Contains(t, args, "-no-CAstore")
	require.NotContains(t, args, "-CApath")
	require.NotContains(t, args, "-CAstore")
}

func TestSMIMESigner(t *testing.T) {
	t.Parallel()

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	ca := newSMIMEIdentity(t, "CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, ca, false)
	secondLeaf := newSMIMEIdentity(t, "Second leaf", 2, ca, false)

	signed, err := pkcs7.NewSignedData([]byte("signed content"))
	require.NoError(t, err)

	signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	require.NoError(t, signed.AddSigner(leaf.cert, leaf.key, pkcs7.SignerInfoConfig{}))

	der, err := signed.Finish()
	require.NoError(t, err)

	oneSigner, err := pkcs7.Parse(der)
	require.NoError(t, err)

	// A single signer and a single verified certificate.
	signer, err := smimeSigner(oneSigner, certPEM(leaf))
	require.NoError(t, err)
	require.True(t, signer.Equal(leaf.cert))

	// More than one verified certificate.
	_, err = smimeSigner(oneSigner, certPEM(leaf, secondLeaf))
	require.ErrorContains(t, err, "must have exactly one signer")

	// No verified certificate.
	_, err = smimeSigner(oneSigner, nil)
	require.Error(t, err)

	// No signer at all.
	der, err = pkcs7.DegenerateCertificate(leaf.cert.Raw)
	require.NoError(t, err)

	unsigned, err := pkcs7.Parse(der)
	require.NoError(t, err)

	_, err = smimeSigner(unsigned, certPEM(leaf))
	require.ErrorContains(t, err, "must have exactly one signer")
}

func TestVerifySMIMETrustError(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 1, expectedCA, false)
	foreignRoot := newSMIMEIdentity(t, "Foreign root", 1, nil, true)
	foreignCA := newSMIMEIdentity(t, "Foreign CA", 2, foreignRoot, true)
	foreignLeaf := newSMIMEIdentity(t, "Foreign leaf", 1, foreignCA, false)

	// A message from a foreign CA names both sides of the mismatch.
	for _, chain := range [][]*smimeIdentity{{foreignCA}, {foreignCA, foreignRoot}} {
		message := signSMIME(t, "signed content", []*smimeIdentity{foreignLeaf}, chain)

		_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)

		var trustErr *TrustError

		require.ErrorAs(t, err, &trustErr)
		require.Equal(t, "unable to verify S/MIME message due to its use of a missing or unverifiable CA", trustErr.Error())
		require.Equal(t, CertificateFingerprint(root.cert), trustErr.RootFingerprint)
		require.Equal(t, CertificateFingerprint(expectedCA.cert), trustErr.ExpectedFingerprint)
		require.Equal(t, CertificateFingerprint(foreignCA.cert), trustErr.PresentedFingerprint)
		require.Equal(t, "CN=Foreign CA", trustErr.PresentedSubject)
		require.NotEqual(t, trustErr.ExpectedFingerprint, trustErr.PresentedFingerprint)
	}

	// A tampered message is a bad signature, not a trust failure.
	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})
	tampered := bytes.Replace(message, []byte("signed content"), []byte("signed contenT"), 1)
	require.NotEqual(t, message, tampered)

	_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, tampered)

	var sigErr *SignatureError

	require.ErrorAs(t, err, &sigErr)
	require.NotErrorAs(t, err, new(*TrustError))

	// Something that isn't S/MIME at all.
	_, err = VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, []byte("not a message\n"))
	require.ErrorIs(t, err, ErrSMIMEUnreadable)
	require.NotErrorAs(t, err, new(*TrustError))

	// The untampered message still verifies.
	_, err = VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)
	require.NoError(t, err)
}

func TestVerifySMIMEWrongIntermediateIsTrust(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	otherCA := newSMIMEIdentity(t, "Other CA", 3, root, true)
	otherLeaf := newSMIMEIdentity(t, "Other leaf", 1, otherCA, false)

	message := signSMIME(t, "signed content", []*smimeIdentity{otherLeaf}, []*smimeIdentity{otherCA})

	_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)

	var trustErr *TrustError

	require.ErrorAs(t, err, &trustErr)
	require.Equal(t, CertificateFingerprint(expectedCA.cert), trustErr.ExpectedFingerprint)
	require.Equal(t, CertificateFingerprint(otherCA.cert), trustErr.PresentedFingerprint)
	require.Equal(t, "CN=Other CA", trustErr.PresentedSubject)
}

func TestCertificateFingerprint(t *testing.T) {
	t.Parallel()

	cert := newSMIMEIdentity(t, "Root", 1, nil, true).cert
	sum := sha256.Sum256(cert.Raw)

	fp := CertificateFingerprint(cert)
	require.Regexp(t, `^([0-9A-F]{2}:){31}[0-9A-F]{2}$`, fp)
	require.Equal(t, strings.ToUpper(hex.EncodeToString(sum[:])), strings.ReplaceAll(fp, ":", ""))
	require.Empty(t, CertificateFingerprint(nil))
}

// timedIdentity returns a leaf issued by parent that is valid only between notBefore and notAfter.
func timedIdentity(t *testing.T, name string, parent *smimeIdentity, notBefore time.Time, notAfter time.Time) *smimeIdentity {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, parent.cert, key.Public(), parent.key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return &smimeIdentity{cert: cert, key: key}
}

// requireClean fails when err's text carries openssl's command line or a temporary path.
func requireClean(t *testing.T, err error) {
	t.Helper()

	require.NotContains(t, err.Error(), "Failed to run")
	require.NotContains(t, err.Error(), "-CAfile")
	require.NotContains(t, err.Error(), os.TempDir())
	require.NotContains(t, err.Error(), "\n")
}

func TestVerifySMIMECertificateValidity(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	expired := timedIdentity(t, "Expired leaf", expectedCA, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	future := timedIdentity(t, "Future leaf", expectedCA, time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour))

	for _, signer := range []*smimeIdentity{expired, future} {
		message := signSMIME(t, "signed content", []*smimeIdentity{signer}, []*smimeIdentity{expectedCA})

		_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)

		var certErr *CertificateError

		require.ErrorAs(t, err, &certErr, signer.cert.Subject.CommonName)
		require.NotErrorAs(t, err, new(*SignatureError))
		require.NotErrorAs(t, err, new(*TrustError))
		requireClean(t, err)
	}
}

func TestVerifySMIMECancelled(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 3, expectedCA, false)
	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := VerifySMIMEWithRoot(ctx, root.cert, []*x509.Certificate{expectedCA.cert}, message)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorAs(t, err, new(*SignatureError))
	require.NotErrorAs(t, err, new(*TrustError))
}

func TestVerifySMIMEWithoutOpenSSL(t *testing.T) { //nolint:paralleltest // sets PATH
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 3, expectedCA, false)
	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})

	t.Setenv("PATH", t.TempDir())

	_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)
	require.ErrorIs(t, err, exec.ErrNotFound)
	require.NotErrorAs(t, err, new(*SignatureError))
	require.NotErrorAs(t, err, new(*TrustError))
	requireClean(t, err)
}

func TestVerifySMIMEErrorsAreShort(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 3, expectedCA, false)
	foreignCA := newSMIMEIdentity(t, "Foreign CA", 2, newSMIMEIdentity(t, "Foreign root", 1, nil, true), true)
	foreignLeaf := newSMIMEIdentity(t, "Foreign leaf", 3, foreignCA, false)

	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})

	for _, m := range [][]byte{
		bytes.Replace(message, []byte("signed content"), []byte("signed contenT"), 1),
		[]byte("not a message\n"),
		signSMIME(t, "signed content", []*smimeIdentity{foreignLeaf}, []*smimeIdentity{foreignCA}),
	} {
		_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, m)
		require.Error(t, err)
		requireClean(t, err)
	}
}

func TestVerifySMIMEForgedIssuerName(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	foreignRoot := newSMIMEIdentity(t, "Foreign root", 1, nil, true)
	fakeCA := newSMIMEIdentity(t, "Expected CA", 2, foreignRoot, true)
	fakeLeaf := newSMIMEIdentity(t, "Fake leaf", 1, fakeCA, false)

	require.Equal(t, expectedCA.cert.RawSubject, fakeCA.cert.RawSubject)

	// The message carries the genuine CA next to the one that really issued the signer.
	for _, chain := range [][]*smimeIdentity{{expectedCA, fakeCA}, {fakeCA, expectedCA}} {
		message := signSMIME(t, "signed content", []*smimeIdentity{fakeLeaf}, chain)

		_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, message)

		var trustErr *TrustError

		require.ErrorAs(t, err, &trustErr)
		require.Equal(t, CertificateFingerprint(fakeCA.cert), trustErr.PresentedFingerprint)
		require.NotEqual(t, trustErr.ExpectedFingerprint, trustErr.PresentedFingerprint)
	}
}

func TestVerifySMIMEDamagedIsNotTrust(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	expectedCA := newSMIMEIdentity(t, "Expected CA", 2, root, true)
	leaf := newSMIMEIdentity(t, "Leaf", 3, expectedCA, false)
	message := signSMIME(t, `{"updates":[]}`, []*smimeIdentity{leaf}, []*smimeIdentity{expectedCA})

	const base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

	// Damage a correctly signed message in a few random places, reproducibly.
	r := mrand.New(mrand.NewSource(1)) //nolint:gosec // reproducible damage, not secrets
	damagedChains := 0

	for range 300 {
		m := slices.Clone(message)
		for range 1 + r.Intn(3) {
			m[r.Intn(len(m))] = base64Chars[r.Intn(len(base64Chars))]
		}

		_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{expectedCA.cert}, m)
		if err == nil {
			continue
		}

		require.NotErrorAs(t, err, new(*TrustError), "damaged message reported as untrusted: %v", err)

		if errors.Is(err, ErrSMIMEDamaged) {
			damagedChains++
		}
	}

	// Some of the damage hit the certificates, which openssl reports like an unknown issuer.
	require.Positive(t, damagedChains)
}

func TestVerifySMIMEUpdateCAOutsideRoot(t *testing.T) {
	t.Parallel()
	requireOpenSSL(t)

	root := newSMIMEIdentity(t, "Root", 1, nil, true)
	otherRoot := newSMIMEIdentity(t, "Other root", 1, nil, true)
	configuredCA := newSMIMEIdentity(t, "Configured CA", 2, otherRoot, true)
	leaf := newSMIMEIdentity(t, "Leaf", 3, configuredCA, false)

	// The configured update CA itself isn't under the trusted root.
	message := signSMIME(t, "signed content", []*smimeIdentity{leaf}, []*smimeIdentity{configuredCA})

	_, err := VerifySMIMEWithRoot(t.Context(), root.cert, []*x509.Certificate{configuredCA.cert}, message)

	var trustErr *TrustError

	require.ErrorAs(t, err, &trustErr)
	require.Equal(t, CertificateFingerprint(configuredCA.cert), trustErr.PresentedFingerprint)
}
