package config

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// BuildTLSConfig constructs a *tls.Config that honours the [server]
// section of the config: an optional PEM-encoded CA bundle
// (ca_cert) and an optional hex-encoded SHA-256 pin of the leaf
// certificate (pinned_cert_sha256). Both empty returns nil, which
// callers pass through to wire.DialOptions.TLSConfig; the dialer
// then falls back to the OS CA bundle with no pin, which is the
// right default for public CA-signed servers.
//
// The pin is enforced via tls.Config.VerifyConnection, which runs
// AFTER the standard chain verification (so RootCAs must trust the
// chain). When the operator configures only a pin (no ca_cert), we
// leave RootCAs nil: Go's TLS stack falls back to the system pool
// at handshake time, so chain verification can succeed against a
// public-CA leaf. The pin then short-circuits the dial if the leaf
// hash doesn't match. This matches the PROTOCOL.md §7 contract:
// pinning is supported, but always layered on top of a trusted
// chain.
//
// A malformed pin (non-hex, wrong length) is a configuration error
// and returns an error rather than falling through to "trust
// everything" — the operator who set it likely fat-fingered the
// value and silently accepting would defeat the purpose.
func BuildTLSConfig(server ServerConfig) (*tls.Config, error) {
	caPath := server.CACert
	pinHex := server.PinnedCertSHA256

	if caPath == "" && pinHex == "" {
		// Empty config — defer to the dialer's default (OS CA
		// bundle, no pin). wire.DialOptions treats a nil
		// TLSConfig as "use the default dialer".
		return nil, nil
	}

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, fmt.Errorf("config: ca_cert %q: %w", caPath, err)
		}
		cfg.RootCAs = pool
	}

	if pinHex != "" {
		pin, err := decodePin(pinHex)
		if err != nil {
			return nil, fmt.Errorf("config: pinned_cert_sha256: %w", err)
		}
		// VerifyConnection receives the verified chain. We only
		// inspect the leaf (chain[0]). The standard chain check
		// has already run, so by the time we get here, the leaf
		// is signed by a CA in RootCAs (or the OS bundle when
		// the operator pinned without a custom CA).
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("tls: peer presented no certificates")
			}
			leaf := cs.PeerCertificates[0]
			sum := sha256.Sum256(leaf.Raw)
			if !pin.matches(sum[:]) {
				return fmt.Errorf("tls: peer certificate sha256 %x does not match pinned %s",
					sum[:], pinHex)
			}
			return nil
		}
	}

	return cfg, nil
}

// loadCAPool reads a PEM file containing one or more certificates
// and returns a *x509.CertPool containing them. The file may be a
// single cert, a CA bundle, or a chain — AppendCertsFromPEM
// accepts all three.
func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no certificates found in PEM file")
	}
	return pool, nil
}

// pin is a parsed, length-validated SHA-256 fingerprint. Storing
// the bytes (not the hex string) keeps the hot path's compare out
// of hex.DecodeString and the equality check in constant time via
// subtle, which we approximate with bytewise compare on equal-
// length slices. A real constant-time compare is overkill for
// cert pinning — the attacker doesn't learn the pin through
// timing — but bytewise compare is the idiomatic Go shape and
// avoids the "subtle.ConstantTimeCompare returns 0 vs 1" footgun.
type pin struct {
	sum [sha256.Size]byte
}

func (p pin) matches(got []byte) bool {
	if len(got) != sha256.Size {
		return false
	}
	for i := 0; i < sha256.Size; i++ {
		if p.sum[i] != got[i] {
			return false
		}
	}
	return true
}

// decodePin parses a hex-encoded SHA-256 fingerprint. Lowercase
// and uppercase hex are both accepted. The result is a fixed-size
// 32-byte array, which lets matches() do a length-checked, byte-
// per-byte compare without further allocation.
func decodePin(s string) (pin, error) {
	var p pin
	raw, err := hex.DecodeString(s)
	if err != nil {
		return p, fmt.Errorf("not valid hex: %w", err)
	}
	if len(raw) != sha256.Size {
		return p, fmt.Errorf("expected %d hex chars (sha256), got %d", sha256.Size*2, len(raw))
	}
	copy(p.sum[:], raw)
	return p, nil
}
