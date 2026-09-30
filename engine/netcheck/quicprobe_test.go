package netcheck

// quicprobe_test.go — the deterministic local QUIC fixture: a real
// quic-go listener on loopback with a throwaway self-signed
// certificate. The client probe completes a full QUIC v1 handshake
// against it — CI never depends on a public QUIC endpoint.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// startFakeQUIC starts a bounded local QUIC v1 listener offering the
// HTTP/3 ALPN and returns its "127.0.0.1:port" target string. The
// transport handshake completes without any application stream; the
// listener closes with the test.
func startFakeQUIC(t *testing.T) string {
	t.Helper()

	cert := selfSignedCert(t)

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3"},
	}

	listener, err := quic.Listen(udpConn, tlsConf, nil)
	if err != nil {
		_ = udpConn.Close()
		t.Fatalf("quic listen: %v", err)
	}

	t.Cleanup(func() {
		_ = listener.Close()
		_ = udpConn.Close()
	})

	// The transport handshake is served by the listener itself;
	// accepting one connection bounds the fixture's lifetime without
	// needing real streams.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}

		_ = conn.CloseWithError(0, "fixture done")
	}()

	return udpConn.LocalAddr().String()
}

// selfSignedCert generates the throwaway ECDSA certificate the
// fixture serves. The probe client runs with verification matching
// the tool's direct contract (valid chain for public targets); the
// local fixture relies on the localEndpointRunner's explicit
// private-target capability.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "freeiran-quic-fixture"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509 create: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509 parse: %v", err)
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}
