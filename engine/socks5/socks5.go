// Package socks5 implements the minimal SOCKS5 client FreeIran needs
// for connectivity measurement: a CONNECT tunnel with optional
// RFC 1929 username/password authentication.
//
// The engine otherwise depends only on the standard library; this
// subpackage keeps the same discipline (RFC 1928 + RFC 1929 subset,
// ~250 lines, fully testable against a local listener) instead of
// pulling in an external proxy library for one handshake.
package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Errors surfaced for the user-facing diagnostics layer.
var (
	ErrHandshakeFailed = errors.New("socks5: handshake failed")
	ErrConnectRefused  = errors.New("socks5: proxy refused the CONNECT request")
	ErrUnsupported     = errors.New("socks5: unsupported reply or address type")

	// ErrAuthFailed: the proxy rejected the RFC 1929 username/password
	// subnegotiation (v0.13.1 — the first-party engine's SOCKS5
	// outbound needs authenticated remotes).
	ErrAuthFailed = errors.New("socks5: proxy rejected the username/password authentication")
)

// Dialer connects to targets through a SOCKS5 proxy.
type Dialer struct {
	// ProxyAddr is the host:port of the SOCKS5 endpoint.
	ProxyAddr string

	// Timeout bounds the TCP dial to the proxy AND the handshake.
	Timeout time.Duration

	// Username and Password enable RFC 1929 username/password
	// authentication (v0.13.1). The zero value keeps the historical
	// no-auth behavior byte-identical: the greeting offers only
	// method 0x00.
	Username string
	Password string

	// DialFunc overrides the transport dial to the proxy (the
	// loop-prevention constrained dialer passes its bound dial here;
	// nil = plain net.DialTimeout). The handshake runs over the
	// returned connection unchanged.
	DialFunc func(ctx context.Context, network, address string) (net.Conn, error)
}

// Dial connects to addr ("host:port") through the configured proxy.
// It implements the subset of net.Dialer semantics the engine needs
// and is compatible with http.Transport's DialContext.
func (d Dialer) Dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: target address %q: %w", addr, err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: target port %q invalid", portStr)
	}

	timeout := d.Timeout

	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var proxy net.Conn

	deadline, hasDeadline := dialCtx.Deadline()

	switch {
	case d.DialFunc != nil:
		// The constrained transport (loop prevention) owns the dial.
		proxy, err = d.DialFunc(dialCtx, "tcp", d.ProxyAddr)
	case hasDeadline:
		proxy, err = net.DialTimeout("tcp", d.ProxyAddr, time.Until(deadline))
	default:
		proxy, err = net.DialTimeout("tcp", d.ProxyAddr, timeout)
	}

	if err != nil {
		return nil, fmt.Errorf("socks5: dial proxy %s: %w", d.ProxyAddr, err)
	}

	// Fail fast when the caller gives up during the handshake. The
	// watcher stops as soon as the handshake succeeds — otherwise the
	// deferred cancel below would poison the returned connection's
	// deadlines after Dial returns.
	watcherDone := make(chan struct{})

	go func() {
		select {
		case <-dialCtx.Done():
			_ = proxy.SetDeadline(time.Now())
		case <-watcherDone:
		}
	}()

	handshakeErr := handshake(proxy, host, port, d.Username, d.Password)

	if handshakeErr != nil {
		close(watcherDone)
		_ = proxy.Close()

		return nil, handshakeErr
	}

	close(watcherDone)

	if deadline, ok := dialCtx.Deadline(); ok {
		_ = proxy.SetDeadline(deadline)
	} else {
		_ = proxy.SetDeadline(time.Time{})
	}

	return proxy, nil
}

// handshake performs greeting + (optional RFC 1929 auth) + CONNECT +
// reply parsing.
func handshake(conn net.Conn, host string, port int, username, password string) error {
	// Greeting: VER=5, methods [NO-AUTH (0x00)] — or, with credentials,
	// [NO-AUTH, USERNAME/PASSWORD (0x02)] so a proxy that accepts both
	// keeps working and a proxy that requires auth can be satisfied.
	var greeting []byte

	hasAuth := username != "" || password != ""

	if hasAuth {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	} else {
		greeting = []byte{0x05, 0x01, 0x00}
	}

	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("%w: send greeting: %v", ErrHandshakeFailed, err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("%w: read greeting reply: %v", ErrHandshakeFailed, err)
	}

	if reply[0] != 0x05 {
		return fmt.Errorf("%w: proxy is not SOCKS5 (ver=0x%02x)", ErrHandshakeFailed, reply[0])
	}

	switch reply[1] {
	case 0x00:
		// No auth needed (or selected) — proceed.
	case 0x02:
		if !hasAuth {
			return fmt.Errorf("%w: proxy requires username/password authentication but no credentials were configured", ErrHandshakeFailed)
		}

		if err := authenticate(conn, username, password); err != nil {
			return err
		}
	case 0xFF:
		return fmt.Errorf("%w: proxy accepted no offered method (0xFF)", ErrHandshakeFailed)
	default:
		return fmt.Errorf("%w: proxy selected unsupported method 0x%02x", ErrHandshakeFailed, reply[1])
	}

	// CONNECT request: VER=5, CMD=1, RSV=0, ATYP, ADDR, PORT.
	req := []byte{0x05, 0x01, 0x00}

	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		// Domain name (ATYP=0x03).
		if len(host) > 255 {
			return fmt.Errorf("socks5: target host too long")
		}

		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	default:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	}

	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("%w: send connect: %v", ErrHandshakeFailed, err)
	}

	return readConnectReply(conn)
}

// authenticate performs the RFC 1929 username/password
// subnegotiation: VER=1, ULEN, UNAME, PLEN, PASSWD, expecting the
// VER=1 status-0 reply. Usernames and passwords are each bounded to
// 255 bytes by the protocol itself.
func authenticate(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return fmt.Errorf("%w: credentials exceed the 255-byte RFC 1929 limit", ErrHandshakeFailed)
	}

	req := make([]byte, 0, 3+len(username)+1+len(password))
	req = append(req, 0x01, byte(len(username)))
	req = append(req, username...)
	req = append(req, byte(len(password)))
	req = append(req, password...)

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("%w: send credentials: %v", ErrHandshakeFailed, err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("%w: read auth reply: %v", ErrHandshakeFailed, err)
	}

	if reply[0] != 0x01 {
		return fmt.Errorf("%w: auth reply version 0x%02x", ErrHandshakeFailed, reply[0])
	}

	if reply[1] != 0x00 {
		return fmt.Errorf("%w: status 0x%02x", ErrAuthFailed, reply[1])
	}

	return nil
}

// readConnectReply parses the variable-length CONNECT reply.
func readConnectReply(conn net.Conn) error {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("%w: read connect reply: %v", ErrConnectRefused, err)
	}

	if head[0] != 0x05 {
		return fmt.Errorf("%w: reply version 0x%02x", ErrHandshakeFailed, head[0])
	}

	if head[1] != 0x00 {
		return fmt.Errorf("%w: reply code 0x%02x (%s)", ErrConnectRefused, head[1], replyText(head[1]))
	}

	// Skip BND.ADDR/BND.PORT depending on ATYP.
	var rest int

	switch head[3] {
	case 0x01: // IPv4
		rest = 4 + 2
	case 0x03: // domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return fmt.Errorf("%w: read domain length: %v", ErrHandshakeFailed, err)
		}

		rest = int(lenBuf[0]) + 2
	case 0x04: // IPv6
		rest = 16 + 2
	default:
		return fmt.Errorf("%w: address type 0x%02x", ErrUnsupported, head[3])
	}

	skip := make([]byte, rest)
	if _, err := io.ReadFull(conn, skip); err != nil {
		return fmt.Errorf("%w: read reply tail: %v", ErrHandshakeFailed, err)
	}

	return nil
}

// replyText maps SOCKS5 reply codes to short human text.
func replyText(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return "unknown"
	}
}

// ParseHostPort is a convenience wrapper that tolerates bare hosts by
// appending the provided default port.
func ParseHostPort(addr, defaultPort string) (string, error) {
	if strings.Contains(addr, ":") {
		return addr, nil
	}

	return net.JoinHostPort(addr, defaultPort), nil
}
