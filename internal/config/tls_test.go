package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validPin is a recognisable 64-char hex SHA-256 of an all-zero
// 32-byte input. decodePin accepts it as a syntactically-valid pin.
const validPin = "0000000000000000000000000000000000000000000000000000000000000000"

func TestBuildTLSConfig_BothEmpty(t *testing.T) {
	cfg, err := BuildTLSConfig(ServerConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config when both fields empty, got %+v", cfg)
	}
}

func TestBuildTLSConfig_CAOnly_LoadsPEM(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, mustSelfSignedCAPEM(t), 0o600); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}

	cfg, err := BuildTLSConfig(ServerConfig{CACert: caPath})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.RootCAs == nil {
		t.Error("expected RootCAs to be set from PEM file")
	}
	if cfg.VerifyConnection != nil {
		t.Error("expected no VerifyConnection when no pin configured")
	}
	if cfg.MinVersion == 0 {
		t.Error("expected MinVersion to be set to TLS 1.2 or higher")
	}
}

func TestBuildTLSConfig_CAFileMissing(t *testing.T) {
	_, err := BuildTLSConfig(ServerConfig{CACert: "/nonexistent/ca.pem"})
	if err == nil {
		t.Fatal("expected error for missing CA file, got nil")
	}
	if !strings.Contains(err.Error(), "ca_cert") {
		t.Errorf("error should mention ca_cert: %v", err)
	}
}

func TestBuildTLSConfig_CAPEMIsGarbage(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, []byte("this is not a PEM file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := BuildTLSConfig(ServerConfig{CACert: caPath})
	if err == nil {
		t.Fatal("expected error for unparseable PEM, got nil")
	}
	if !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("error should mention 'no certificates': %v", err)
	}
}

func TestBuildTLSConfig_PinOnly_SetsVerifyConnection(t *testing.T) {
	cfg, err := BuildTLSConfig(ServerConfig{PinnedCertSHA256: validPin})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if cfg.VerifyConnection == nil {
		t.Fatal("expected VerifyConnection to be set when pin configured")
	}
	// Pin-only path leaves RootCAs nil — gorilla/TLS will then
	// fall back to the OS pool, which is the documented behavior
	// for "pin a public-CA cert without bringing your own CA".
	if cfg.RootCAs != nil {
		t.Errorf("expected RootCAs nil for pin-only, got %+v", cfg.RootCAs)
	}
}

func TestBuildTLSConfig_PinInvalidHex(t *testing.T) {
	cases := []string{
		"zzzz",                  // non-hex chars
		"abc",                   // too short
		validPin + "00",         // too long
		strings.Repeat("g", 64), // right length, not hex
	}
	for _, pin := range cases {
		t.Run(pin, func(t *testing.T) {
			_, err := BuildTLSConfig(ServerConfig{PinnedCertSHA256: pin})
			if err == nil {
				t.Errorf("expected error for malformed pin %q, got nil", pin)
			}
		})
	}
}

func TestBuildTLSConfig_BothSet(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, mustSelfSignedCAPEM(t), 0o600); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}

	cfg, err := BuildTLSConfig(ServerConfig{
		CACert:           caPath,
		PinnedCertSHA256: validPin,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("expected RootCAs to be set")
	}
	if cfg.VerifyConnection == nil {
		t.Error("expected VerifyConnection to be set")
	}
}

// mustSelfSignedCAPEM returns a self-signed CA cert as PEM bytes.
// The cert is only used to test that the loader finds certificates
// in the file — we don't care about key strength, validity dates,
// or whether any actual TLS handshake would succeed against it.
func mustSelfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
