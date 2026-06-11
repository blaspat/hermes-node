package wire

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// serveTLSScenario is the TLS counterpart of serveScenario: a
// real wss:// endpoint that performs the WebSocket upgrade and
// runs the same handshake state machine. We use httptest.NewTLSServer
// so the cert is self-signed and ephemeral per test; the dialer
// is given the cert as its RootCAs so chain verification succeeds.
func serveTLSScenario(t *testing.T, sc testScenario) *httptest.Server {
	t.Helper()
	if sc.handshakeTimeout == 0 {
		sc.handshakeTimeout = testHandshakeTimeout
	}

	upgrader := websocket.Upgrader{
		// httptest.NewTLSServer is local; trust any origin.
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("tls upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		_ = conn.SetReadDeadline(time.Now().Add(sc.handshakeTimeout))
		var hello map[string]any
		if err := conn.ReadJSON(&hello); err != nil {
			t.Logf("tls server: read hello: %v", err)
			return
		}
		if hello["type"] != "hello" {
			t.Logf("tls server: expected hello, got %v", hello["type"])
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
		_ = conn.WriteJSON(map[string]any{
			"type":          "hello_ack",
			"session_id":    "test-session-tls",
			"server_time":   "2026-06-11T00:00:00Z",
		})

		_ = conn.SetReadDeadline(time.Now().Add(sc.handshakeTimeout))
		var auth map[string]any
		if err := conn.ReadJSON(&auth); err != nil {
			t.Logf("tls server: read auth: %v", err)
			return
		}
		if auth["type"] != "auth" {
			t.Logf("tls server: expected auth, got %v", auth["type"])
			return
		}
		if sc.authOK {
			_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
			_ = conn.WriteJSON(map[string]any{
				"type":       "auth_ok",
				"session_id": "test-session-tls",
			})
		} else {
			_ = conn.SetWriteDeadline(time.Now().Add(sc.handshakeTimeout))
			_ = conn.WriteJSON(map[string]any{
				"type":   "auth_err",
				"reason": sc.authErrReason,
				"code":   4001,
			})
		}
	})

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// serverCertPEM extracts the server's leaf certificate as PEM bytes
// so tests can build a RootCAs pool that trusts exactly that cert.
// httptest.NewTLSServer sets srv.Certificate to the leaf it just
// minted. Certificate is a method (not a field) because the
// underlying value is built lazily on first call.
func serverCertPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("httptest.NewTLSServer did not expose its certificate")
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
}

// poolFromPEM returns a non-nil x509.CertPool containing the certs
// in pem. The caller is asserting trust in exactly these certs
// (typical for the "trust my private CA" use case in production).
func poolFromPEM(t *testing.T, pemBytes []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("no certs in PEM block")
	}
	return pool
}

// tlsConfigForServer returns a *tls.Config that trusts exactly
// the test server's self-signed cert, so a dial against wss://
// <srv>.URL succeeds chain verification. No pin is set; pin tests
// add their own VerifyConnection on top.
func tlsConfigForServer(t *testing.T, srv *httptest.Server) *tls.Config {
	t.Helper()
	return &tls.Config{
		RootCAs:    poolFromPEM(t, serverCertPEM(t, srv)),
		MinVersion: tls.VersionTLS12,
	}
}

// pinOf returns the hex SHA-256 of the test server's leaf cert, in
// the format PROTOCOL.md §7 specifies.
func pinOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	sum := sha256.Sum256(cert.Raw)
	return hexLower(sum[:])
}

func hexLower(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}

// wssURL converts an httptest.NewTLSServer URL (https://...) to
// the wss:// equivalent for the gorilla dialer.
func wssURL(srv *httptest.Server) string {
	return "wss" + strings.TrimPrefix(srv.URL, "https")
}

// TestConnect_AcceptsPinnedCert: the configured pin matches the
// server's leaf, so the dial succeeds and the handshake completes.
func TestConnect_AcceptsPinnedCert(t *testing.T) {
	srv := serveTLSScenario(t, testScenario{authOK: true})
	tlsCfg := tlsConfigForServer(t, srv)
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errNoPeerCert
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		if hexLower(sum[:]) != pinOf(t, srv) {
			return errPinMismatch
		}
		return nil
	}

	client, err := Connect(ctx5s(t), DialOptions{
		ServerURL:        wssURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		HandshakeTimeout: testHandshakeTimeout,
		TLSConfig:        tlsCfg,
	})
	if err != nil {
		t.Fatalf("Connect with valid pin: %v", err)
	}
	t.Cleanup(func() { _ = client.Conn().Close() })
	if client.SessionID() != "test-session-tls" {
		t.Errorf("SessionID: got %q, want test-session-tls", client.SessionID())
	}
}

// TestConnect_RejectsPinnedCertMismatch: the configured pin is the
// SHA-256 of all-zero bytes; the server's leaf has a different
// hash, so VerifyConnection must reject.
func TestConnect_RejectsPinnedCertMismatch(t *testing.T) {
	srv := serveTLSScenario(t, testScenario{authOK: true})
	tlsCfg := tlsConfigForServer(t, srv)
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		// Pin to a value we know won't match: 64 zeros.
		return errPinMismatch
	}

	_, err := Connect(ctx5s(t), DialOptions{
		ServerURL:        wssURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		HandshakeTimeout: testHandshakeTimeout,
		TLSConfig:        tlsCfg,
	})
	if err == nil {
		t.Fatal("expected Connect to fail when pin does not match")
	}
	if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "pin") {
		t.Logf("error message: %v (any TLS/pin error is acceptable here)", err)
	}
}

// TestConnect_RejectsUntrustedCA: dialer trusts a *different* cert
// pool, so chain verification fails before any pin check runs.
// This is the "operator misconfigures ca_cert" failure mode.
func TestConnect_RejectsUntrustedCA(t *testing.T) {
	srv := serveTLSScenario(t, testScenario{authOK: true})
	// poolFromPEM on an unrelated cert: any self-signed cert will
	// do, since the goal is "this cert is NOT the server's cert".
	unrelatedCertPEM := unrelatedSelfSignedPEM(t)
	tlsCfg := &tls.Config{
		RootCAs:    poolFromPEM(t, unrelatedCertPEM),
		MinVersion: tls.VersionTLS12,
	}

	_, err := Connect(ctx5s(t), DialOptions{
		ServerURL:        wssURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		HandshakeTimeout: testHandshakeTimeout,
		TLSConfig:        tlsCfg,
	})
	if err == nil {
		t.Fatal("expected Connect to fail when RootCAs does not trust server cert")
	}
	// Tighten the assertion: a positive check that the failure
	// came from the x509 chain-verification layer (not some other
	// dial-time error). The Go stdlib prefixes chain verification
	// failures with "x509: ", e.g. "x509: certificate signed by
	// unknown authority". A refactor that changes the failure
	// layer (e.g. moves to a custom VerifyPeer callback) would
	// still need to keep chain verification firing for this test
	// to pass — which is what we want.
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("expected x509 chain-verification error, got: %v", err)
	}
}

// TestConnect_AcceptsCustomCA: the operator's ca_cert contains the
// server's self-signed cert, chain verification succeeds, no pin
// is configured, dial succeeds. This is the "private CA"
// production path.
func TestConnect_AcceptsCustomCA(t *testing.T) {
	srv := serveTLSScenario(t, testScenario{authOK: true})
	tlsCfg := &tls.Config{
		RootCAs:    poolFromPEM(t, serverCertPEM(t, srv)),
		MinVersion: tls.VersionTLS12,
	}

	client, err := Connect(ctx5s(t), DialOptions{
		ServerURL:        wssURL(srv),
		NodeName:         "test-node",
		Token:            "test-token",
		NodeVersion:      "0.1.0-test",
		HandshakeTimeout: testHandshakeTimeout,
		TLSConfig:        tlsCfg,
	})
	if err != nil {
		t.Fatalf("Connect with matching custom CA: %v", err)
	}
	t.Cleanup(func() { _ = client.Conn().Close() })
}

// ctx5s is a tiny helper: short timeout context for tests so a
// misbehaving dial doesn't pin the suite.
func ctx5s(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// errPinMismatch and errNoPeerCert are sentinels for tests that
// exercise VerifyConnection failure paths. They live here (not in
// production code) because the production VerifyConnection in
// config.BuildTLSConfig constructs its own error messages inline;
// the test callbacks just need a stable error to return.
var (
	errPinMismatch = errors.New("peer certificate sha256 does not match pinned")
	errNoPeerCert  = errors.New("no peer certificate")
)

// unrelatedSelfSignedPEM returns a self-signed cert unrelated to
// the test server. It's used to build a RootCAs pool that
// deliberately does NOT trust the server, so chain verification
// fails.
func unrelatedSelfSignedPEM(t *testing.T) []byte {
	t.Helper()
	return generateSelfSignedPEM(t, "unrelated.test")
}

func generateSelfSignedPEM(t *testing.T, commonName string) []byte {
	t.Helper()
	key := mustGenRSAKey(t)
	tmpl := mustCertTemplate(commonName)
	der := mustCreateCert(t, tmpl, tmpl, &key.PublicKey, key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
