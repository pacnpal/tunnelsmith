package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSelfSignedCert generates a fresh ECDSA-P256 self-signed cert
// valid for "127.0.0.1" and writes the PEM-encoded cert + key into
// the given directory. Returns the two paths plus the cert pool a
// test client should trust. Cert validity is one hour — well beyond
// any test runtime, well below "forever" so a leaked test bundle
// stops working quickly.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tunnelsmith-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert to pool failed")
	}
	return certPath, keyPath, pool
}

func quietControlLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServerWithOpts boots a control.Server bound to a random port
// and waits for Ready. Tests use this to exercise the listener wiring
// without spinning up the full cmd/tunnelsmith stack.
func startServerWithOpts(t *testing.T, opts ServerOptions) (*Server, chan error) {
	t.Helper()
	srv := NewServer("127.0.0.1:0", &fakeBackend{poolIDs: []string{}}, nil, opts, quietControlLogger())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()
	select {
	case <-srv.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("server did not bind in time")
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		select {
		case err := <-serveErr:
			if err != nil {
				t.Errorf("Serve returned unexpected error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after Shutdown")
		}
	})
	return srv, serveErr
}

// TestServeTLSAcceptsHTTPSClient is the headline check: when
// TLSCertFile/TLSKeyFile are set, an HTTPS client that trusts the
// cert can reach /healthz. Proves the cert/key pair is loaded and
// the listener handshakes TLS.
func TestServeTLSAcceptsHTTPSClient(t *testing.T) {
	certPath, keyPath, pool := writeSelfSignedCert(t, t.TempDir())
	srv, _ := startServerWithOpts(t, ServerOptions{
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})

	if !srv.TLSEnabled() {
		t.Fatal("expected TLSEnabled() = true")
	}

	addr := srv.Addr().String()
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
	if resp.TLS == nil {
		t.Fatal("expected non-nil resp.TLS (request didn't go over TLS?)")
	}
}

// TestServeTLSRejectsPlaintextClient locks the contract that a TLS
// listener doesn't tunnel a plain HTTP request through to the
// scoreboard. The Go standard library detects the malformed handshake
// and replies with HTTP 400 plus a "Client sent an HTTP request to
// an HTTPS server" body — a better failure mode than a silent TCP
// reset because the operator gets a self-describing error in their
// client. We assert that exact contract: a transport-level error
// here would be a regression to silent rejection, which the test
// should surface, not accept.
func TestServeTLSRejectsPlaintextClient(t *testing.T) {
	certPath, keyPath, _ := writeSelfSignedCert(t, t.TempDir())
	srv, _ := startServerWithOpts(t, ServerOptions{
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	})

	addr := srv.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("plaintext request to TLS listener: expected stdlib 400 reply, got transport error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plaintext request to TLS listener: status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !containsAny(string(body), []string{"HTTP request", "HTTPS server"}) {
		t.Fatalf("plaintext request to TLS listener: body = %q, want HTTPS-mismatch message", body)
	}
	if resp.TLS != nil {
		t.Fatal("plaintext request unexpectedly carried a TLS connection state")
	}
}

// TestServePlaintextWhenTLSUnset preserves the pre-1.2 wire shape:
// with both cert/key empty the listener serves plain HTTP. Catches
// any regression that would silently force TLS on operators who
// haven't configured it.
func TestServePlaintextWhenTLSUnset(t *testing.T) {
	srv, _ := startServerWithOpts(t, ServerOptions{})
	if srv.TLSEnabled() {
		t.Fatal("TLSEnabled() must be false with empty cert/key")
	}
	addr := srv.Addr().String()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS != nil {
		t.Fatal("plaintext listener returned a TLS connection state")
	}
}

// TestServeHalfConfiguredTLSFails pins the defense-in-depth check
// inside Serve: a ServerOptions with cert-without-key (or vice
// versa) returns an explicit error rather than silently falling
// back to plaintext on a listener the operator intended to be TLS.
// config.Validate already enforces this at config-load for
// cmd/tunnelsmith, but NewServer is callable from tests and any
// future internal package; the Serve-side guard makes the contract
// the same regardless of entry point.
func TestServeHalfConfiguredTLSFails(t *testing.T) {
	cases := []struct {
		name string
		cert string
		key  string
	}{
		{"cert without key", "/etc/cert.pem", ""},
		{"key without cert", "", "/etc/key.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer("127.0.0.1:0", &fakeBackend{}, nil, ServerOptions{
				TLSCertFile: tc.cert,
				TLSKeyFile:  tc.key,
			}, quietControlLogger())
			serveErr := make(chan error, 1)
			go func() { serveErr <- srv.Serve(context.Background()) }()
			select {
			case err := <-serveErr:
				if err == nil {
					t.Fatal("expected Serve to reject half-configured TLS, got nil")
				}
				if !containsAny(err.Error(), []string{"tls_cert_file", "tls_key_file"}) {
					t.Fatalf("err = %v, want one mentioning the cert/key pair", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Serve did not return for half-configured TLS")
			}
		})
	}
}

// TestServeTLSBadCertFails surfaces the failure mode where the
// operator points cert/key at a path that doesn't exist (or contains
// garbage). ServeTLS returns the error from the goroutine; Serve
// should propagate it rather than swallow it.
func TestServeTLSBadCertFails(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "missing.pem")
	keyPath := filepath.Join(dir, "missing-key.pem")
	srv := NewServer("127.0.0.1:0", &fakeBackend{}, nil, ServerOptions{
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	}, quietControlLogger())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(context.Background()) }()
	<-srv.Ready()
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	select {
	case err := <-serveErr:
		if err == nil {
			t.Fatal("expected Serve to return an error for missing cert/key")
		}
		if !errorMentionsMissingCert(err) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return for missing cert/key in time")
	}
}

func errorMentionsMissingCert(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg, []string{"no such file", "not found", "missing"})
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
