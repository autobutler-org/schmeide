package control

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
)

// ErrPlaintextNATS is returned when NATS would listen beyond loopback in
// plain text without --allow-plaintext-nats.
var ErrPlaintextNATS = errors.New("NATS would listen beyond loopback without TLS, so steward tokens and every job's GitHub token would cross the network in plain text")

// checkNATSTransport refuses a NATS listener that other machines can reach
// unless it serves TLS or the operator allowed plain text.
func checkNATSTransport(addr string, tls, allowPlaintext bool) error {
	if tls || allowPlaintext || !listensBeyondLoopback(addr) {
		return nil
	}
	return fmt.Errorf("--nats-addr %s: %w; set --nats-tls-cert-file and --nats-tls-key-file, "+
		"listen on loopback (127.0.0.1:4222), or pass --allow-plaintext-nats true to accept the risk", addr, ErrPlaintextNATS)
}

// natsTLSConfig serves the certificate and key in certFile and keyFile. The
// pair is loaded once here, so a bad pair fails startup. After that, every
// handshake reads both files again, so a renewed certificate (from ACME, for
// example) is served without a restart. A read that fails, such as a renewal
// caught half-written, keeps serving the last pair that loaded.
func natsTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the NATS TLS certificate and key: %w; fix --nats-tls-cert-file and --nats-tls-key-file", err)
	}
	var last atomic.Pointer[tls.Certificate]
	last.Store(&cert)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// ponytail: reads both files on every handshake. Runners connect
		// rarely, so this is cheap; cache by mtime if handshakes get frequent.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if fresh, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
				last.Store(&fresh)
			}
			return last.Load(), nil
		},
	}, nil
}

// listensBeyondLoopback reports whether a listener on addr can be reached
// from another machine: any host that is not a loopback IP or localhost,
// including an empty host, which means every interface. An empty addr means
// no listener.
func listensBeyondLoopback(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
