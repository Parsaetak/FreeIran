package freecore

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/socks5"
)

// Outbound is the remote side of the engine: it turns a dial request
// into an established, ready-to-pump net.Conn. Implementations are
// the first-party protocol clients — no external process is ever
// involved.
type Outbound interface {
	// Name is the outbound identity used in sessions and diagnostics.
	Name() string

	// Dial connects to address (host:port) honoring ctx cancellation
	// and the route's timing bounds.
	Dial(ctx context.Context, address string) (net.Conn, error)
}

// --- direct ---------------------------------------------------------------

// DirectOutbound dials the destination with no remote in between.
type DirectOutbound struct {
	// Timeout bounds one dial (zero = DefaultDialTimeout).
	Timeout time.Duration
}

// Name implements Outbound.
func (d *DirectOutbound) Name() string { return string(OutboundDirect) }

// Dial implements Outbound through the standard library dialer with
// context propagation and bounded connect time.
func (d *DirectOutbound) Dial(ctx context.Context, address string) (net.Conn, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("freecore: direct dial %s: %w", address, err)
	}

	return conn, nil
}

// --- SOCKS5 ----------------------------------------------------------------

// SOCKS5Outbound forwards through a SOCKS5 remote with optional RFC
// 1929 username/password auth. It is built on the reviewed in-repo
// client (engine/socks5) — the same client the connection engine's
// verification gate already exercises on every session.
type SOCKS5Outbound struct {
	// Proxy is the remote SOCKS5 endpoint.
	Proxy Endpoint

	// Username/Password enable RFC 1929 auth when set.
	Username string
	Password string

	// Timeout bounds the proxy dial and handshake (zero =
	// DefaultDialTimeout).
	Timeout time.Duration
}

// Name implements Outbound.
func (s *SOCKS5Outbound) Name() string { return string(OutboundSOCKS5) }

// Dial implements Outbound through the first-party SOCKS5 client.
func (s *SOCKS5Outbound) Dial(ctx context.Context, address string) (net.Conn, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	dialer := socks5.Dialer{
		ProxyAddr: s.Proxy.String(),
		Timeout:   timeout,
		Username:  s.Username,
		Password:  s.Password,
	}

	conn, err := dialer.Dial(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("freecore: socks5 outbound via %s: %w", s.Proxy, err)
	}

	return conn, nil
}

// --- HTTP CONNECT ----------------------------------------------------------

// HTTPConnectOutbound forwards through an HTTP proxy using the
// CONNECT method (RFC 9110 §9.3.6) with optional Basic proxy
// authorization.
type HTTPConnectOutbound struct {
	// Proxy is the remote HTTP proxy endpoint.
	Proxy Endpoint

	// Username/Password enable Proxy-Authorization when set.
	Username string
	Password string

	// Timeout bounds the proxy dial and the CONNECT exchange (zero =
	// DefaultDialTimeout).
	Timeout time.Duration
}

// Name implements Outbound.
func (h *HTTPConnectOutbound) Name() string { return string(OutboundHTTP) }

// Dial implements Outbound: dial the proxy, send CONNECT, verify the
// 2xx reply, then hand back the raw tunnel.
func (h *HTTPConnectOutbound) Dial(ctx context.Context, address string) (net.Conn, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	dialer := &net.Dialer{Timeout: timeout}

	proxyConn, err := dialer.DialContext(ctx, "tcp", h.Proxy.String())
	if err != nil {
		return nil, fmt.Errorf("freecore: http outbound dial %s: %w", h.Proxy, err)
	}

	deadline := time.Now().Add(timeout)
	_ = proxyConn.SetDeadline(deadline)

	if err := h.connect(proxyConn, address); err != nil {
		_ = proxyConn.Close()

		return nil, err
	}

	// The tunnel carries arbitrary bytes from here: clear the
	// handshake deadline and propagate the caller's cancellation by
	// closing the conn when ctx ends (the watcher stops itself once
	// the caller's context is done AND the connection is returned —
	// here the returned conn's lifetime belongs to the session).
	_ = proxyConn.SetDeadline(time.Time{})

	watchDone := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			_ = proxyConn.SetDeadline(time.Now())
		case <-watchDone:
		}
	}()

	return &watchedConn{Conn: proxyConn, done: watchDone}, nil
}

// connect writes the CONNECT request and verifies the reply.
func (h *HTTPConnectOutbound) connect(conn net.Conn, address string) error {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("freecore: http outbound target %q: %w", address, err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("freecore: http outbound target port %q invalid", portStr)
	}

	request := "CONNECT " + net.JoinHostPort(host, portStr) + " HTTP/1.1\r\n" +
		"Host: " + net.JoinHostPort(host, portStr) + "\r\n" +
		"Proxy-Connection: keep-alive\r\n"

	if h.Username != "" || h.Password != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(h.Username + ":" + h.Password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}

	request += "\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		return fmt.Errorf("freecore: http outbound send CONNECT: %w", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, DefaultHeaderLimit)), nil)
	if err != nil {
		return fmt.Errorf("freecore: http outbound read CONNECT reply: %w", err)
	}

	// Drain the reply body (bounded; proxies send none on success).
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	_ = response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("freecore: http outbound CONNECT refused: %s", response.Status)
	}

	return nil
}

// watchedConn stops the context watcher when the tunnel closes so the
// watcher goroutine cannot outlive the connection it guards.
type watchedConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

// Close implements net.Conn.
func (w *watchedConn) Close() error {
	w.once.Do(func() { close(w.done) })

	return w.Conn.Close()
}

// outboundFor builds the first-party outbound for a normalized route.
func outboundFor(route Route) Outbound {
	switch route.Outbound {
	case OutboundSOCKS5:
		return &SOCKS5Outbound{
			Proxy:    route.Endpoint,
			Username: route.Username,
			Password: route.Password,
			Timeout:  route.DialTimeout,
		}
	case OutboundHTTP:
		return &HTTPConnectOutbound{
			Proxy:    route.Endpoint,
			Username: route.Username,
			Password: route.Password,
			Timeout:  route.DialTimeout,
		}
	default:
		return &DirectOutbound{}
	}
}
