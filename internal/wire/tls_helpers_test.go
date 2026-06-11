package wire

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// mustGenRSAKey returns a fresh 2048-bit RSA key or fails the test.
// 2048 bits is the smallest size Go's default TLS config still
// accepts; tests don't need a stronger key because they're not
// proving crypto agility, just exercising the wire code paths.
func mustGenRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// mustCertTemplate returns a minimal x509.Certificate template
// suitable for self-signed test certs. Validity is one hour
// around now so time-skewed CI doesn't reject the cert.
func mustCertTemplate(commonName string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
}

// mustCreateCert signs a cert with the given key. The parent
// template being the same as the cert template yields a
// self-signed cert, which is what tests need for "private CA"
// scenarios.
func mustCreateCert(t *testing.T, tmpl, parent *x509.Certificate, pub interface{}, priv interface{}) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return der
}
