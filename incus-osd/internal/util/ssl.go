package util

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/smallstep/pkcs7"

	"github.com/lxc/incus-os/incus-osd/certs"
)

// UpdateSystemCustomCACerts regenerates /etc/ssl/certs/ca-certificates.crt with any
// specified custom CA certificates appended.
func UpdateSystemCustomCACerts(pemCerts []string) error {
	// If no custom CA certificates are configured, restore the default symlink.
	if len(pemCerts) == 0 {
		// Remove any existing locally-configured certificates.
		err := os.RemoveAll("/etc/ssl/certs/")
		if err != nil {
			return err
		}

		return os.Symlink("/usr/share/certs", "/etc/ssl/certs")
	}

	// Validate that each PEM-encoded block is a valid x509 certificate.
	for i, pemCert := range pemCerts {
		pemBlock, _ := pem.Decode([]byte(pemCert))
		if pemBlock == nil {
			return fmt.Errorf("unable to decode certificate %d", i)
		}

		if pemBlock.Type != "CERTIFICATE" {
			return fmt.Errorf("certificate %d isn't a PEM-encoded certificate", i)
		}

		_, err := x509.ParseCertificate(pemBlock.Bytes)
		if err != nil {
			return err
		}
	}

	// Remove any existing locally-configured certificates.
	err := os.RemoveAll("/etc/ssl/certs/")
	if err != nil {
		return err
	}

	// Create an empty directory.
	err = os.MkdirAll("/etc/ssl/certs/", 0o755)
	if err != nil {
		return err
	}

	// Create the new CA bundle.
	bundleCerts, err := os.Create("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		return err
	}
	defer bundleCerts.Close()

	// Get default CA certificates from the verity image.
	systemCerts, err := os.Open("/usr/share/certs/ca-certificates.crt")
	if err != nil {
		return err
	}
	defer systemCerts.Close()

	// Copy the default CA certificates. Do it in a single call, since the file is only ~220KB.
	_, err = io.Copy(bundleCerts, systemCerts)
	if err != nil {
		return err
	}

	// Append each custom CA certificate.
	for _, pemCert := range pemCerts {
		_, err := bundleCerts.WriteString(pemCert)
		if err != nil {
			return err
		}

		// Ensure we end with a newline, even if the PEM-encoded string didn't have one.
		if !strings.HasSuffix(pemCert, "\n") {
			_, err := bundleCerts.WriteString("\n")
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// TrustError is an S/MIME message whose signer doesn't chain to the trusted root and
// expected intermediate CA. Fingerprints are SHA-256, uppercase and colon-separated.
type TrustError struct {
	// RootFingerprint is the root CA this system trusts.
	RootFingerprint string

	// ExpectedFingerprint is the intermediate CA the signer must be issued by.
	ExpectedFingerprint string

	// PresentedFingerprint and PresentedSubject are the signer's issuer as the message
	// presents it; the fingerprint is empty when the message doesn't carry that certificate.
	PresentedFingerprint string
	PresentedSubject     string

	msg string
	err error
}

func (e *TrustError) Error() string {
	return e.msg
}

func (e *TrustError) Unwrap() error {
	return e.err
}

// SignatureError is an S/MIME message whose signature doesn't verify.
type SignatureError struct {
	err error
}

func (*SignatureError) Error() string {
	return "invalid S/MIME signature"
}

func (e *SignatureError) Unwrap() error {
	return e.err
}

// CertificateError is an S/MIME message whose signer chains to the root CA but whose
// certificates aren't valid, for example because one has expired.
type CertificateError struct {
	reason string
	err    error
}

func (e *CertificateError) Error() string {
	return "S/MIME signing certificate isn't valid: " + e.reason
}

func (e *CertificateError) Unwrap() error {
	return e.err
}

// ErrSMIMEUnreadable is returned for a message that isn't S/MIME at all.
var ErrSMIMEUnreadable = errors.New("unable to read S/MIME message")

// ErrSMIMEDamaged is returned for a message whose certificates are damaged.
var ErrSMIMEDamaged = errors.New("S/MIME message is damaged")

// smimeFormatError is a message that can't be read or is damaged; kind is the sentinel.
type smimeFormatError struct {
	kind error
	err  error
}

func (e *smimeFormatError) Error() string {
	return e.kind.Error()
}

func (e *smimeFormatError) Unwrap() []error {
	return []error{e.kind, e.err}
}

// opensslError is a failure to run openssl, which says nothing about the message.
type opensslError struct {
	err error
}

func (e *opensslError) Error() string {
	cause := e.err

	runErr, ok := errors.AsType[subprocess.RunError](e.err)
	if ok {
		cause = runErr.Unwrap()
	}

	return "unable to run openssl to verify S/MIME message: " + cause.Error()
}

func (e *opensslError) Unwrap() error {
	return e.err
}

// smimeTrustFailures are openssl's verify errors for a chain that doesn't reach the root CA.
var smimeTrustFailures = []string{
	"Verify error: unable to get local issuer certificate",
	"Verify error: unable to get issuer certificate",
	"Verify error: self-signed certificate in certificate chain",
	"Verify error: self signed certificate in certificate chain",
}

// CertificateFingerprint returns the certificate's SHA-256 fingerprint, uppercase and colon-separated.
func CertificateFingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}

	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, 0, len(sum))

	for _, b := range sum {
		parts = append(parts, fmt.Sprintf("%02X", b))
	}

	return strings.Join(parts, ":")
}

// VerifySMIME first verifies the provided S/MIME-signed message using the root CA as a trust
// anchor. Then, if validation succeeded, it ensures the message has exactly one signer whose
// certificate was issued by one of the expected intermediate CAs. `openssl smime ...` doesn't
// provide a way to do this, so we must perform a manual check ourselves. Upon success, the
// verified plain-text message is returned.
func VerifySMIME(ctx context.Context, expectedIntermediateCAs []*x509.Certificate, message []byte) (*bytes.Buffer, error) {
	// Load the embedded certificates.
	embeddedCerts, err := certs.GetEmbeddedCertificates()
	if err != nil {
		return nil, err
	}

	return VerifySMIMEWithRoot(ctx, embeddedCerts.RootCACertificate, expectedIntermediateCAs, message)
}

// VerifySMIMEWithRoot is VerifySMIME using the provided root CA as the trust anchor.
func VerifySMIMEWithRoot(ctx context.Context, rootCA *x509.Certificate, expectedIntermediateCAs []*x509.Certificate, message []byte) (*bytes.Buffer, error) {
	if len(expectedIntermediateCAs) == 0 {
		return nil, errors.New("at least one intermediate CA must be provided when performing S/MIME validation")
	}

	for _, ca := range expectedIntermediateCAs {
		if ca == nil {
			return nil, errors.New("provided intermediate CA for S/MIME validation cannot be nil")
		}
	}

	// Get the root CA.
	var rootCABuf bytes.Buffer

	err := pem.Encode(&rootCABuf, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: rootCA.Raw,
	})
	if err != nil {
		return nil, err
	}

	// Write the root CA to a temporary file.
	rootCAFile, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}

	defer func() { _ = os.Remove(rootCAFile.Name()) }()

	_, err = rootCAFile.Write(rootCABuf.Bytes())
	if err != nil {
		return nil, err
	}

	err = rootCAFile.Close()
	if err != nil {
		return nil, err
	}

	// Create a temporary file for the certificate the signature gets verified with.
	signerFile, err := os.CreateTemp("", "")
	if err != nil {
		return nil, err
	}

	defer func() { _ = os.Remove(signerFile.Name()) }()

	err = signerFile.Close()
	if err != nil {
		return nil, err
	}

	// Verify the signed message, trusting only the provided root CA.
	verified := bytes.NewBuffer(nil)

	err = subprocess.RunCommandWithFds(ctx, bytes.NewReader(message), verified, "openssl", smimeVerifyArgs(rootCAFile.Name(), signerFile.Name())...)
	if err != nil {
		return nil, smimeVerifyError(ctx, rootCA, expectedIntermediateCAs, message, err)
	}

	// Get the now-verified message's certificate chain.
	pkcs, err := smimePKCS7(ctx, message)
	if err != nil {
		return nil, err
	}

	// Get the certificate the signature was verified with.
	verifiedSigners, err := os.ReadFile(signerFile.Name())
	if err != nil {
		return nil, err
	}

	signer, err := smimeSigner(pkcs, verifiedSigners)
	if err != nil {
		return nil, err
	}

	// Ensure the signer's certificate was issued by one of the expected intermediate CAs.
	issuedByExpectedCA := slices.ContainsFunc(expectedIntermediateCAs, func(ca *x509.Certificate) bool {
		return bytes.Equal(signer.RawIssuer, ca.RawSubject) && signer.CheckSignatureFrom(ca) == nil
	})

	if !issuedByExpectedCA {
		expectedSubjects := []string{}
		for _, ca := range expectedIntermediateCAs {
			if !slices.Contains(expectedSubjects, ca.Subject.String()) {
				expectedSubjects = append(expectedSubjects, ca.Subject.String())
			}
		}

		trustErr := newTrustError(rootCA, expectedIntermediateCAs, pkcs.Certificates, signer)
		trustErr.msg = "S/MIME message contained a valid signature, but was not signed by one of the following expected intermediate CAs: '" + strings.Join(expectedSubjects, "', '") + "'"

		return nil, trustErr
	}

	return verified, nil
}

// smimeVerifyError tells a chain that doesn't reach the root CA apart from a bad signature,
// invalid certificates, a damaged message and a failure to run openssl.
func smimeVerifyError(ctx context.Context, rootCA *x509.Certificate, expectedIntermediateCAs []*x509.Certificate, message []byte, err error) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return ctxErr
	}

	runErr, ran := errors.AsType[subprocess.RunError](err)
	if !ran || runErr.StdErr() == nil {
		return &opensslError{err: err}
	}

	stderr := runErr.StdErr().String()

	if strings.Contains(stderr, "Error reading S/MIME message") {
		return &smimeFormatError{kind: ErrSMIMEUnreadable, err: err}
	}

	_, reason, isVerifyError := strings.Cut(stderr, "Verify error: ")
	if !isVerifyError {
		if strings.Contains(stderr, "signature failure") || strings.Contains(stderr, "digest failure") {
			return &SignatureError{err: err}
		}

		return &opensslError{err: err}
	}

	reason, _, _ = strings.Cut(reason, "\n")

	isTrust := slices.ContainsFunc(smimeTrustFailures, func(failure string) bool {
		return strings.Contains(stderr, failure)
	})
	if !isTrust {
		return &CertificateError{reason: reason, err: err}
	}

	// Read what the message presents, even though it doesn't verify.
	pkcs, parseErr := smimePKCS7(ctx, message)
	if parseErr != nil {
		return &smimeFormatError{kind: ErrSMIMEDamaged, err: err}
	}

	signer := pkcs.GetOnlySigner()
	trustErr := newTrustError(rootCA, expectedIntermediateCAs, pkcs.Certificates, signer)

	if !presentsOtherIssuer(rootCA, expectedIntermediateCAs, trustErr, signer) {
		return &smimeFormatError{kind: ErrSMIMEDamaged, err: err}
	}

	trustErr.msg = "unable to verify S/MIME message due to its use of a missing or unverifiable CA"
	trustErr.err = err

	return trustErr
}

// presentsOtherIssuer reports whether a chain that doesn't reach the root CA is a trust mismatch:
// the signer names or presents another issuer, or the expected CA itself isn't under the root.
// Otherwise the message's certificates were damaged.
func presentsOtherIssuer(rootCA *x509.Certificate, expectedIntermediateCAs []*x509.Certificate, trustErr *TrustError, signer *x509.Certificate) bool {
	if signer == nil {
		return false
	}

	for _, ca := range expectedIntermediateCAs {
		if bytes.Equal(signer.RawIssuer, ca.RawSubject) && signer.CheckSignatureFrom(ca) == nil {
			return !bytes.Equal(ca.RawIssuer, rootCA.RawSubject) || ca.CheckSignatureFrom(rootCA) != nil
		}
	}

	namesExpected := slices.ContainsFunc(expectedIntermediateCAs, func(ca *x509.Certificate) bool {
		return bytes.Equal(signer.RawIssuer, ca.RawSubject)
	})

	return !namesExpected || trustErr.PresentedFingerprint != ""
}

// newTrustError returns a trust error naming the signer's issuer among the presented certificates.
func newTrustError(rootCA *x509.Certificate, expectedIntermediateCAs []*x509.Certificate, presented []*x509.Certificate, signer *x509.Certificate) *TrustError {
	trustErr := &TrustError{
		RootFingerprint:     CertificateFingerprint(rootCA),
		ExpectedFingerprint: CertificateFingerprint(expectedIntermediateCAs[0]),
	}

	if signer == nil {
		return trustErr
	}

	trustErr.PresentedSubject = signer.Issuer.String()

	for _, cert := range presented {
		if bytes.Equal(cert.RawSubject, signer.RawIssuer) && signer.CheckSignatureFrom(cert) == nil {
			trustErr.PresentedFingerprint = CertificateFingerprint(cert)

			break
		}
	}

	return trustErr
}

// smimePKCS7 returns the message's PKCS #7 structure, without verifying it.
func smimePKCS7(ctx context.Context, message []byte) (*pkcs7.PKCS7, error) {
	certChain := bytes.NewBuffer(nil)

	err := subprocess.RunCommandWithFds(ctx, bytes.NewReader(message), certChain, "openssl", "smime", "-pk7out")
	if err != nil {
		return nil, err
	}

	// openssl seems to output PEM even if `-outform DER` was provided in the command
	// above. So, we need to take an extra step to decode the output before it can be
	// parsed as pkcs7.
	block, _ := pem.Decode(certChain.Bytes())
	if block == nil {
		return nil, errors.New("unable to parse PEM-encoded pkcs7 data")
	}

	return pkcs7.Parse(block.Bytes)
}

// smimeVerifyArgs returns the openssl arguments to verify a message trusting only the root CA
// in rootCAFile, writing the certificate the signature was verified with to signerFile.
func smimeVerifyArgs(rootCAFile string, signerFile string) []string {
	return []string{"smime", "-verify", "-text", "-no-CApath", "-no-CAstore", "-CAfile", rootCAFile, "-signer", signerFile}
}

// smimeSigner returns the certificate the message's signature was verified with, requiring
// exactly one signer. verifiedSigners holds the PEM-encoded certificate(s) written by openssl.
func smimeSigner(pkcs *pkcs7.PKCS7, verifiedSigners []byte) (*x509.Certificate, error) {
	if len(pkcs.Signers) != 1 {
		return nil, fmt.Errorf("S/MIME message must have exactly one signer, found %d", len(pkcs.Signers))
	}

	block, rest := pem.Decode(verifiedSigners)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("unable to decode the S/MIME signer's certificate")
	}

	// One certificate is written per signer.
	extra, _ := pem.Decode(rest)
	if extra != nil {
		return nil, errors.New("S/MIME message must have exactly one signer, found several")
	}

	return x509.ParseCertificate(block.Bytes)
}
