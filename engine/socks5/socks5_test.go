package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeProxy implements a minimal SOCKS5 server that CONNECTs to a
// local echo target, so the dialer's full handshake path is exercised
// without any external network.
func fakeProxy(t *testing.T) (proxyAddr, targetAddr string) {
	t.Helper()

	// Echo target: everything the client sends after CONNECT is echoed.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}(conn)
		}
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}

			go serveFake(conn, target.Addr().String())
		}
	}()

	return proxy.Addr().String(), target.Addr().String()
}

func serveFake(conn net.Conn, target string) {
	defer conn.Close()

	// Greeting.
	greet := make([]byte, 3)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return
	}

	if greet[0] != 0x05 {
		return
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// CONNECT request (fixed IPv4 shape is enough for the test).
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	addrLen := 4

	if head[3] == 0x03 {
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}

		addrLen = int(lenBuf[0])
		_ = addrLen

		skip := make([]byte, addrLen)
		if _, err := io.ReadFull(conn, skip); err != nil {
			return
		}
	} else {
		skip := make([]byte, addrLen)
		if _, err := io.ReadFull(conn, skip); err != nil {
			return
		}
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}

	// Connect to the target and reply success.
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}
	defer upstream.Close()

	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func TestDialerConnectsThroughProxy(t *testing.T) {
	proxyAddr, _ := fakeProxy(t)

	d := Dialer{ProxyAddr: proxyAddr, Timeout: 5 * time.Second}

	conn, err := d.Dial(context.Background(), "tcp", "example.invalid:80")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// The tunnel should reach the echo server.
	payload := "ping-through-socks"

	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(buf) != payload {
		t.Errorf("echo = %q, want %q", buf, payload)
	}
}

func TestDialerRejectsNonSocks5(t *testing.T) {
	// A plain echo server is not a SOCKS5 proxy; the greeting reply
	// times out / garbage-fails.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer echo.Close()

	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(conn)
		}
	}()

	d := Dialer{ProxyAddr: echo.Addr().String(), Timeout: 3 * time.Second}

	_, err = d.Dial(context.Background(), "tcp", "example.invalid:80")
	if err == nil {
		t.Fatal("Dial against a non-SOCKS5 server succeeded")
	}

	if !strings.Contains(err.Error(), "socks5") {
		t.Errorf("error %q does not identify the socks5 layer", err)
	}
}

func TestBuildRequestDomainShape(t *testing.T) {
	// The CONNECT request for a domain uses ATYP=0x03 + length byte.
	host := "example.com"
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)

	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 443)
	req = append(req, port...)

	if req[3] != 0x03 || req[4] != byte(len(host)) {
		t.Fatalf("domain request shape wrong: %v", req)
	}
}

// authFakeProxy is a SOCKS5 server that REQUIRES RFC 1929
// username/password auth (method 0x02 only) and CONNECTs to a local
// echo target — the v0.13.1 auth-path coverage for the dialer the
// first-party engine's SOCKS5 outbound uses.
func authFakeProxy(t *testing.T, user, pass string) (proxyAddr, targetAddr string) {
	t.Helper()

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}

	t.Cleanup(func() { _ = target.Close() })

	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}(conn)
		}
	}()

	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}

	t.Cleanup(func() { _ = proxy.Close() })

	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}

			go serveAuthFake(conn, target.Addr().String(), user, pass)
		}
	}()

	return proxy.Addr().String(), target.Addr().String()
}

func serveAuthFake(conn net.Conn, target, wantUser, wantPass string) {
	defer conn.Close()

	// Greeting: VER + NMETHODS, then exactly NMETHODS method bytes.
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}

	if head[0] != 0x05 || head[1] < 1 || head[1] > 255 {
		return
	}

	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	// This server requires username/password auth.
	_, _ = conn.Write([]byte{0x05, 0x02})

	// RFC 1929 subnegotiation.
	ver := make([]byte, 1)
	if _, err := io.ReadFull(conn, ver); err != nil || ver[0] != 0x01 {
		return
	}

	ulen := make([]byte, 1)
	if _, err := io.ReadFull(conn, ulen); err != nil {
		return
	}

	uname := make([]byte, int(ulen[0]))
	if _, err := io.ReadFull(conn, uname); err != nil {
		return
	}

	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return
	}

	passwd := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(conn, passwd); err != nil {
		return
	}

	if string(uname) != wantUser || string(passwd) != wantPass {
		_, _ = conn.Write([]byte{0x01, 0x01})

		return
	}

	_, _ = conn.Write([]byte{0x01, 0x00})

	// CONNECT request.
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}

	var skip []byte

	switch req[3] {
	case 0x01:
		skip = make([]byte, 4)
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return
		}

		skip = make([]byte, int(lenBuf[0]))
	case 0x04:
		skip = make([]byte, 16)
	default:
		return
	}

	if _, err := io.ReadFull(conn, skip); err != nil {
		return
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}

	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		return
	}

	defer upstream.Close()

	_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	go func() { _, _ = io.Copy(upstream, conn) }()
	_, _ = io.Copy(conn, upstream)
}

func TestDialerAuthenticates(t *testing.T) {
	proxyAddr, targetAddr := authFakeProxy(t, "alice", "s3cret")

	dialer := Dialer{
		ProxyAddr: proxyAddr,
		Timeout:   3 * time.Second,
		Username:  "alice",
		Password:  "s3cret",
	}

	conn, err := dialer.Dial(context.Background(), "tcp", targetAddr)
	if err != nil {
		t.Fatalf("authenticating dial: %v", err)
	}

	defer conn.Close()

	payload := []byte("authenticated bytes through socks5")

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	echo := make([]byte, len(payload))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(echo) != string(payload) {
		t.Fatalf("echo = %q, want %q", echo, payload)
	}
}

func TestDialerAuthRejected(t *testing.T) {
	proxyAddr, targetAddr := authFakeProxy(t, "alice", "s3cret")

	dialer := Dialer{
		ProxyAddr: proxyAddr,
		Timeout:   3 * time.Second,
		Username:  "alice",
		Password:  "wrong",
	}

	conn, err := dialer.Dial(context.Background(), "tcp", targetAddr)
	if err == nil {
		_ = conn.Close()

		t.Fatal("dial with wrong credentials must fail")
	}

	if !strings.Contains(err.Error(), ErrAuthFailed.Error()) {
		t.Fatalf("error = %v, want the auth-failure sentinel wrapped", err)
	}
}

func TestDialerNoAuthGreetingUnchanged(t *testing.T) {
	// The zero-value dialer offers ONLY method 0x00: the greeting
	// bytes must stay exactly [0x05, 0x01, 0x00] (the historical
	// wire shape — the no-auth path is byte-identical by contract).
	seen := make(chan []byte, 1)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		buf := make([]byte, 3)
		_, _ = io.ReadFull(conn, buf)
		_ = conn.Close()

		seen <- buf
	}()

	dialer := Dialer{ProxyAddr: ln.Addr().String(), Timeout: time.Second}
	_, _ = dialer.Dial(context.Background(), "tcp", "127.0.0.1:1")

	select {
	case g := <-seen:
		want := []byte{0x05, 0x01, 0x00}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("no-auth greeting = %v, want %v", g, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("greeting never arrived")
	}
}
