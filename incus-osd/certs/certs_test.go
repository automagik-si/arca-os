package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
)

// issue returns a new certificate for pub, self-signed when parent is nil.
func issue(t *testing.T, name string, pub crypto.PublicKey, parent *x509.Certificate, parentKey crypto.Signer, isCA bool) *x509.Certificate {
	t.Helper()

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}

	if isCA {
		template.KeyUsage = x509.KeyUsageCertSign
	}

	if parent == nil {
		parent = template
	}

	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, parentKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return cert
}

func pemFile(der []byte) *fstest.MapFile {
	return &fstest.MapFile{Data: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// testInventory returns the files of a complete, valid set of certificates.
func testInventory(t *testing.T) fstest.MapFS {
	t.Helper()

	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	secureBootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	updateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	pkKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	root := issue(t, "Root", rootKey.Public(), nil, rootKey, true)
	secureBoot := issue(t, "Secure Boot", secureBootKey.Public(), root, rootKey, true)
	update := issue(t, "Update", updateKey.Public(), root, rootKey, true)
	pk := issue(t, "Secure Boot PK", pkKey.Public(), secureBoot, secureBootKey, false)

	return fstest.MapFS{
		"files/production-cert-subject-key-ids.txt": &fstest.MapFile{},
		"files/root-E1.crt":                         pemFile(root.Raw),
		"files/secureboot-E1.crt":                   pemFile(secureBoot.Raw),
		"files/update-E1.crt":                       pemFile(update.Raw),
		"files/secureboot-PK-R1.crt":                pemFile(pk.Raw),
	}
}

func TestInventoryFailedLoadIsNotCached(t *testing.T) {
	t.Parallel()

	files := testInventory(t)
	files["files/update-E1.crt"] = pemFile([]byte("not a certificate"))

	inv := &inventory{files: files}

	// A first load that fails, then a second call.
	for range 2 {
		got, err := inv.get()
		require.Error(t, err)
		require.Equal(t, Certificates{}, got)
	}
}

func TestInventoryLoadedOnceIsCached(t *testing.T) {
	t.Parallel()

	inv := &inventory{files: testInventory(t)}

	first, err := inv.get()
	require.NoError(t, err)
	require.NotNil(t, first.RootCACertificate)
	require.NotNil(t, first.SecureBootCACertificate)
	require.NotNil(t, first.UpdateCACertificate)
	require.NotNil(t, first.SecureBootCertificates.PK)
	require.Same(t, first.UpdateCACertificate, first.HotfixCACertificate)
	require.Same(t, first.UpdateCACertificate, first.SupportCACertificate)

	// The cached call no longer reads any file.
	inv.files = fstest.MapFS{}

	second, err := inv.get()
	require.NoError(t, err)
	require.Equal(t, first, second)

	// Changing a returned list doesn't change the cached one.
	first.ProductionCertSubjectKeyIDs[0] = "changed"

	third, err := inv.get()
	require.NoError(t, err)
	require.Equal(t, second, third)
}
