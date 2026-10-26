package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// writeSelfSigned writes a fresh self-signed certificate for 127.0.0.1 and
// its key, and returns a pool that trusts only that certificate.
func writeSelfSigned(t *testing.T, certFile, keyFile string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "schmeide-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

// startQueueOverTLS starts a queue that requires TLS with the given pair and
// returns its address.
func startQueueOverTLS(t *testing.T, certFile, keyFile string) (*Queue, string) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	q, err := StartQueue(t.Context(), QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: testRunnerTokens, TLSCertFile: certFile, TLSKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q, addr
}

func connectTLS(addr string, roots *x509.CertPool) (*nats.Conn, error) {
	return nats.Connect("tls://"+addr, nats.UserInfo("octocat", "octocat-token"), nats.Secure(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}), nats.NoReconnect())
}

func tlsPair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func TestQueueServesTLS(t *testing.T) {
	certFile, keyFile := tlsPair(t)
	roots := writeSelfSigned(t, certFile, keyFile)
	q, addr := startQueueOverTLS(t, certFile, keyFile)

	nc, err := connectTLS(addr, roots)
	if err != nil {
		t.Fatalf("trusted TLS client: %v", err)
	}
	nc.Close()
	if _, err := nats.Connect("nats://"+addr, nats.UserInfo("octocat", "octocat-token"), nats.NoReconnect()); err == nil {
		t.Error("a plain TCP client connected to a TLS-only NATS server")
	}
	if !q.Conn().IsConnected() {
		t.Error("the in-process connection is not connected")
	}
}

func TestQueueServesARotatedCertificate(t *testing.T) {
	certFile, keyFile := tlsPair(t)
	oldRoots := writeSelfSigned(t, certFile, keyFile)
	_, addr := startQueueOverTLS(t, certFile, keyFile)

	newRoots := writeSelfSigned(t, certFile, keyFile)
	nc, err := connectTLS(addr, newRoots)
	if err != nil {
		t.Fatalf("client trusting the renewed certificate: %v", err)
	}
	nc.Close()
	if nc, err := connectTLS(addr, oldRoots); err == nil {
		nc.Close()
		t.Error("a client trusting only the old certificate connected after rotation")
	}
}

func TestQueueKeepsTheLastGoodCertificate(t *testing.T) {
	certFile, keyFile := tlsPair(t)
	roots := writeSelfSigned(t, certFile, keyFile)
	_, addr := startQueueOverTLS(t, certFile, keyFile)

	// A half-written renewal.
	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\ntrunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	nc, err := connectTLS(addr, roots)
	if err != nil {
		t.Fatalf("the last good certificate was not served: %v", err)
	}
	nc.Close()
}

func TestNATSTLSConfigRejectsBadFiles(t *testing.T) {
	certFile, keyFile := tlsPair(t)
	if _, err := natsTLSConfig(certFile, keyFile); err == nil || !strings.Contains(err.Error(), "--nats-tls-cert-file") {
		t.Errorf("missing files: err = %v, want an error naming the flags", err)
	}
	if err := os.WriteFile(certFile, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StartQueue(t.Context(), QueueOptions{Addr: "127.0.0.1:0", DataDir: t.TempDir(), TLSCertFile: certFile, TLSKeyFile: keyFile}); err == nil {
		t.Error("StartQueue accepted an unreadable certificate")
	}
}

func TestParseControlNeedsBothTLSFiles(t *testing.T) {
	for _, s := range controlSettings {
		t.Setenv(s.env(), "")
	}
	base := []string{"--webhook-secret", "s", "--github-app-id", "1", "--allowed-orgs", "acme", "--runner-tokens", "octocat:t", "--github-app-private-key-file", "/k.pem"}
	for _, extra := range [][]string{{"--nats-tls-cert-file", "/c.pem"}, {"--nats-tls-key-file", "/k.pem"}} {
		_, err := parseControl(append(append([]string{}, base...), extra...))
		for _, want := range []string{"--nats-tls-cert-file", "--nats-tls-key-file", "SCHMEIDE_NATS_TLS_CERT_FILE", "SCHMEIDE_NATS_TLS_KEY_FILE"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%v: err = %v, want it to name %s", extra, err, want)
			}
		}
	}
	got, err := parseControl(append(append([]string{}, base...), "--nats-tls-cert-file", "/c.pem", "--nats-tls-key-file", "/k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if got.NATSTLSCertFile != "/c.pem" || got.NATSTLSKeyFile != "/k.pem" {
		t.Errorf("config = %+v, want both TLS files", got)
	}
}

func TestListensBeyondLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:4222": false,
		"[::1]:4222":     false,
		"localhost:4222": false,
		"":               false,
		"0.0.0.0:4222":   true,
		":4222":          true,
		"[::]:4222":      true,
		"10.0.0.5:4222":  true,
		"nats.example:1": true,
	} {
		if got := listensBeyondLoopback(addr); got != want {
			t.Errorf("listensBeyondLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestCheckNATSTransport(t *testing.T) {
	for _, tt := range []struct {
		name      string
		addr      string
		tls       bool
		plaintext bool
		refused   bool
	}{
		{name: "loopback without TLS", addr: "127.0.0.1:4222"},
		{name: "every interface with TLS", addr: "0.0.0.0:4222", tls: true},
		{name: "every interface without TLS", addr: "0.0.0.0:4222", refused: true},
		{name: "a LAN address without TLS", addr: "10.0.0.5:4222", refused: true},
		{name: "every interface with plain text allowed", addr: "0.0.0.0:4222", plaintext: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkNATSTransport(tt.addr, tt.tls, tt.plaintext)
			if !tt.refused {
				if err != nil {
					t.Errorf("err = %v, want none", err)
				}
				return
			}
			if !errors.Is(err, ErrPlaintextNATS) {
				t.Fatalf("err = %v, want ErrPlaintextNATS", err)
			}
			for _, want := range []string{"--nats-tls-cert-file", "--nats-tls-key-file", "--allow-plaintext-nats", tt.addr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to name %s", err, want)
				}
			}
		})
	}
}

func TestParseControlAllowPlaintextNATS(t *testing.T) {
	for _, s := range controlSettings {
		t.Setenv(s.env(), "")
	}
	base := []string{"--webhook-secret", "s", "--github-app-id", "1", "--allowed-orgs", "acme", "--runner-tokens", "octocat:t", "--github-app-private-key-file", "/k.pem"}
	got, err := parseControl(base)
	if err != nil || got.AllowPlaintextNATS != "false" {
		t.Fatalf("default = %q, %v; want false", got.AllowPlaintextNATS, err)
	}
	got, err = parseControl(append(append([]string{}, base...), "--allow-plaintext-nats", "true"))
	if err != nil || got.AllowPlaintextNATS != "true" {
		t.Fatalf("--allow-plaintext-nats true = %q, %v", got.AllowPlaintextNATS, err)
	}
	if _, err := parseControl(append(append([]string{}, base...), "--allow-plaintext-nats", "maybe")); err == nil || !strings.Contains(err.Error(), "--allow-plaintext-nats") {
		t.Errorf("err = %v, want it to name --allow-plaintext-nats", err)
	}
}
