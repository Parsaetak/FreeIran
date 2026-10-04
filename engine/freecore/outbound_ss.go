package freecore

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Parsaetak/FreeIran/engine/freecore/shadowsocks"
)

// NormalizeShadowsocksMethod validates the method string through the
// protocol package and returns the canonical name.
func NormalizeShadowsocksMethod(method string) (string, error) {
	m, err := shadowsocks.ParseMethod(method)
	if err != nil {
		return "", fmt.Errorf("freecore: %w", err)
	}

	return string(m), nil
}

// ShadowsocksOutbound forwards through a Shadowsocks AEAD remote
// (Phase H: the first encrypted first-party protocol slice). The
// framing, KDF and nonce handling live in the protocol package behind
// interoperability evidence (docs/protocols.md).
type ShadowsocksOutbound struct {
	// Proxy is the remote Shadowsocks endpoint.
	Proxy Endpoint

	// Password is the shared secret (master key material).
	Password string

	// Method is the AEAD method name (validated at normalization).
	Method string

	// Timeout bounds the server dial and the AEAD handshake (zero =
	// DefaultDialTimeout).
	Timeout time.Duration

	// transport is the optional constrained dialer (loop prevention);
	// nil = plain system dialing.
	transport Dialer
}

// Name implements Outbound.
func (s *ShadowsocksOutbound) Name() string { return string(OutboundShadowsocks) }

// Dial implements Outbound: establishes the AEAD tunnel to the remote
// carrying the requested target address; the returned conn is plaintext
// after the framing layer.
func (s *ShadowsocksOutbound) Dial(ctx context.Context, address string) (net.Conn, error) {
	cfg := shadowsocks.ClientConfig{
		Host:     s.Proxy.Host,
		Port:     s.Proxy.Port,
		Password: s.Password,
		Method:   shadowsocks.Method(s.Method),
		Timeout:  s.Timeout,
	}

	conn, err := cfg.DialTCP(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("freecore: shadowsocks outbound via %s: %w", s.Proxy, err)
	}

	return conn, nil
}
