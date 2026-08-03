// Package wire — E2E encryption primitives.
//
// Implements the PAKE-style handshake from docs/e2e-spec.md:
// X25519 ECDH → HKDF with pairing token → HMAC mutual auth → AES-256-GCM.
package wire

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// e2eKeySize is AES-256.
const e2eKeySize = 32

// e2eNonceSize is 12 bytes (AES-GCM standard).
const e2eNonceSize = 12

// e2eTagSize is GCM's 16-byte authentication tag.
const e2eTagSize = 16

// e2eSaltSize is the random salt we put in hello_ack.
const e2eSaltSize = 32

// e2eInfoHandshake is the HKDF info string for the handshake key.
const e2eInfoHandshake = "hermes-node-e2e-v1"

// e2eInfoSession is the HKDF info string for the session key.
const e2eInfoSession = "hermes-node-session-v1"

// e2eProofClient is the HMAC message for the client proof.
const e2eProofClient = "client-auth"

// e2eProofServer is the HMAC message for the server proof.
const e2eProofServer = "server-auth"

// ErrE2EDecryptFailed is returned when GCM tag verification fails.
var ErrE2EDecryptFailed = errors.New("e2e: decrypt failed — wrong key or tampered data")

// ErrE2EProofMismatch is returned when the HMAC proof doesn't verify.
var ErrE2EProofMismatch = errors.New("e2e: proof mismatch — token differs or MITM")

// --- X25519 key generation & ECDH -------------------------------------------

// E2EKeyPair is an ephemeral X25519 keypair.
type E2EKeyPair struct {
	Public  []byte // 32 bytes
	Private []byte // 32 bytes
}

// GenerateE2EKeyPair creates a fresh X25519 keypair using crypto/rand.
func GenerateE2EKeyPair() (*E2EKeyPair, error) {
	priv := make([]byte, curve25519.ScalarSize)
	if _, err := io.ReadFull(rand.Reader, priv); err != nil {
		return nil, fmt.Errorf("e2e: gen keypair: %w", err)
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("e2e: compute public: %w", err)
	}
	return &E2EKeyPair{Public: pub, Private: priv}, nil
}

// ECDH performs X25519 scalar multiplication: our_priv × peer_pub.
func ECDH(ourPriv, peerPub []byte) ([]byte, error) {
	shared, err := curve25519.X25519(ourPriv, peerPub)
	if err != nil {
		return nil, fmt.Errorf("e2e: ecdh: %w", err)
	}
	return shared, nil
}

// --- HKDF key derivation ----------------------------------------------------

// deriveHandshakeKey produces the handshake key from the ECDH shared secret
// and the pairing token. This key is used ONLY for the HMAC proof exchange.
func deriveHandshakeKey(ecdhShared, salt, token []byte) ([]byte, error) {
	info := append([]byte(e2eInfoHandshake), token...)
	r := hkdf.New(sha256.New, ecdhShared, salt, info)
	key := make([]byte, e2eKeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("e2e: hkdf handshake: %w", err)
	}
	return key, nil
}

// DeriveSessionKey produces the session key from the handshake key.
// The session key is used for AES-256-GCM encryption of operational messages.
func DeriveSessionKey(handshakeKey []byte, sessionID string) ([]byte, error) {
	r := hkdf.New(sha256.New, handshakeKey, []byte(sessionID),
		[]byte(e2eInfoSession))
	key := make([]byte, e2eKeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("e2e: hkdf session: %w", err)
	}
	return key, nil
}

// --- HMAC mutual authentication ---------------------------------------------

// ComputeClientProof returns HMAC-SHA256(handshakeKey, "client-auth").
func ComputeClientProof(handshakeKey []byte) []byte {
	mac := hmac.New(sha256.New, handshakeKey)
	mac.Write([]byte(e2eProofClient))
	return mac.Sum(nil)
}

// ComputeServerProof returns HMAC-SHA256(handshakeKey, "server-auth").
func ComputeServerProof(handshakeKey []byte) []byte {
	mac := hmac.New(sha256.New, handshakeKey)
	mac.Write([]byte(e2eProofServer))
	return mac.Sum(nil)
}

// VerifyServerProof checks that the server's proof matches our computed one.
func VerifyServerProof(handshakeKey, proof []byte) error {
	expected := ComputeServerProof(handshakeKey)
	if !hmac.Equal(expected, proof) {
		return ErrE2EProofMismatch
	}
	return nil
}

// --- AES-256-GCM encryption -------------------------------------------------

// EncryptE2E encrypts plaintext with AES-256-GCM.
// Returns IV || ciphertext || tag.
// The IV is 12 random bytes prepended to the output.
func EncryptE2E(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("e2e: aes cipher: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("e2e: gcm: %w", err)
	}
	nonce := make([]byte, e2eNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("e2e: nonce: %w", err)
	}
	// Seal appends the tag to ciphertext. Prepend the nonce.
	out := aesgcm.Seal(nonce, nonce, plaintext, nil)
	return out, nil
}

// DecryptE2E decrypts IV || ciphertext || tag with AES-256-GCM.
func DecryptE2E(key, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < e2eNonceSize+e2eTagSize {
		return nil, fmt.Errorf("%w: ciphertext too short (%d bytes)", ErrE2EDecryptFailed, len(ciphertext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("e2e: aes cipher: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("e2e: gcm: %w", err)
	}
	nonce, ct := ciphertext[:e2eNonceSize], ciphertext[e2eNonceSize:]
	plain, err := aesgcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrE2EDecryptFailed, err)
	}
	return plain, nil
}

// --- Encoding helpers -------------------------------------------------------

// EncodeBase64 returns the base64url (no padding) encoding of raw bytes.
// Used for ECDH public keys, salt, and proof values in JSON.
func EncodeBase64(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeBase64 decodes a base64url string (with or without padding).
func DecodeBase64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// --- Salt generation --------------------------------------------------------

// GenerateSalt returns e2eSaltSize (32) random bytes.
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, e2eSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("e2e: salt: %w", err)
	}
	return salt, nil
}
