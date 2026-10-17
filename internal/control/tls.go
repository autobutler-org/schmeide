package control

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync/atomic"
)

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
