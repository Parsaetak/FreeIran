package freecore

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// echoServer starts a TCP echo server; everything sent to it comes
// back verbatim.
func echoServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// freePort reserves one ephemeral port for the engine inbound.
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	defer func() { _ = ln.Close() }()

	return ln.Addr().(*net.TCPAddr).Port
}

// directEngine starts an engine that forwards DIRECTLY to targets —
// the minimal first-party path with real bytes, no remote proxy.
func directEngine(t *testing.T, opts func(*Options)) *Engine {
	t.Helper()

	port := freePort(t)

	options := Options{
		Route: Route{
			Outbound:    OutboundDirect,
			Endpoint:    Endpoint{Host: "127.0.0.1", Port: 0},
			Network:     NetworkTCP,
			Security:    SecurityNone,
			DialTimeout: 3 * time.Second,
		},
		LocalHost: "127.0.0.1",
		LocalPort: port,
	}

	if opts != nil {
		opts(&options)
	}

	engine, err := NewEngine(options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		engine.Stop(0)
	})

	if err := engine.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	return engine
}

// socks5ClientHandshake performs a raw SOCKS5 CONNECT against the
// engine's inbound and returns the raw tunnel connection.
func socks5ClientHandshake(t *testing.T, addr, target string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}

	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("greeting reply = %v, want no-auth accepted", reply)
	}

	host, portStr, _ := net.SplitHostPort(target)

	var port int

	_, _ = fmtSscan(portStr, &port)

	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port&0xFF))

	if _, err := conn.Write(request); err != nil {
		t.Fatalf("connect request: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("connect reply: %v", err)
	}

	if head[1] != 0x00 {
		t.Fatalf("connect reply code = 0x%02x, want success", head[1])
	}

	// Skip the bound address.
	var rest int

	switch head[3] {
	case 0x01:
		rest = 4 + 2
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			t.Fatalf("bound length: %v", err)
		}

		rest = int(lenBuf[0]) + 2
	case 0x04:
		rest = 16 + 2
	default:
		t.Fatalf("bound ATYP = 0x%02x", head[3])
	}

	skip := make([]byte, rest)
	if _, err := io.ReadFull(conn, skip); err != nil {
		t.Fatalf("bound tail: %v", err)
	}

	return conn
}

// fmtSscan is a tiny strconv wrapper kept local to the test file.
func fmtSscan(s string, v *int) (int, error) {
	n := 0

	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return n, errBadPort
		}

		n = n*10 + int(s[i]-'0')
	}

	*v = n

	return 1, nil
}

var errBadPort = &portError{}

type portError struct{}

func (*portError) Error() string { return "bad port" }

// TestSOCKS5InboundForwardsRealBytes drives the full first-party
// pipeline: SOCKS5 handshake → session → direct outbound → echo
// target → bidirectional relay. Real bytes both directions.
func TestSOCKS5InboundForwardsRealBytes(t *testing.T) {
	target := echoServer(t)
	engine := directEngine(t, nil)

	conn := socks5ClientHandshake(t, engine.Endpoint(), target)
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	up := []byte("freecore first-party bytes upstream")
	if _, err := conn.Write(up); err != nil {
		t.Fatalf("write: %v", err)
	}

	echo := make([]byte, len(up))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(echo) != string(up) {
		t.Fatalf("echo = %q, want %q", echo, up)
	}
}

// TestHTTPConnectInboundForwardsRealBytes drives the HTTP CONNECT
// form of the mixed inbound.
func TestHTTPConnectInboundForwardsRealBytes(t *testing.T) {
	target := echoServer(t)
	engine := directEngine(t, nil)

	conn, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n")); err != nil {
		t.Fatalf("connect: %v", err)
	}

	br := bufio.NewReader(conn)

	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}

	if want := "HTTP/1.1 200 Connection Established"; len(status) < len(want) || status[:len(want)] != want {
		t.Fatalf("status = %q, want %q", status, want)
	}

	// Drain the remaining head (empty line).
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("drain head: %v", err)
		}

		if line == "\r\n" || line == "\n" {
			break
		}
	}

	up := []byte("http connect tunnel bytes")

	if _, err := conn.Write(up); err != nil {
		t.Fatalf("tunnel write: %v", err)
	}

	echo := make([]byte, len(up))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if string(echo) != string(up) {
		t.Fatalf("echo = %q, want %q", echo, up)
	}
}

// TestHTTPAbsoluteFormForwardsRealRequests drives the plain-HTTP
// proxying form (GET http://host/path) — what WinINet uses for
// http:// targets.
func TestHTTPAbsoluteFormForwardsRealRequests(t *testing.T) {
	// A tiny origin server answering one fixed response.
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("origin listen: %v", err)
	}

	t.Cleanup(func() { _ = origin.Close() })

	go func() {
		for {
			conn, err := origin.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()

				br := bufio.NewReader(c)
				_, _ = br.ReadString('\n')

				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" || line == "\n" {
						break
					}
				}

				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello"))
			}(conn)
		}
	}()

	engine := directEngine(t, nil)

	conn, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	request := "GET http://" + origin.Addr().String() + "/path HTTP/1.1\r\n" +
		"Host: " + origin.Addr().String() + "\r\n\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("absolute-form request: %v", err)
	}

	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if len(response) < 4 || string(response[:4]) != "HTTP" {
		t.Fatalf("response = %q, want an HTTP status line", response)
	}

	if string(response[len(response)-5:]) != "hello" {
		t.Fatalf("response body = %q, want the origin body", string(response[len(response)-5:]))
	}
}

// TestSOCKS5RefusesUDPAssociate proves the honest capability refusal:
// UDP ASSOCIATE (cmd 0x03) gets the command-not-supported reply, not
// a fake tunnel.
func TestSOCKS5RefusesUDPAssociate(t *testing.T) {
	engine := directEngine(t, nil)

	conn, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}

	// UDP ASSOCIATE to 0.0.0.0:0.
	if _, err := conn.Write([]byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("udp associate: %v", err)
	}

	head := make([]byte, 10)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("reply: %v", err)
	}

	if head[1] != 0x07 {
		t.Fatalf("UDP ASSOCIATE reply code = 0x%02x, want 0x07 (command not supported)", head[1])
	}
}

// TestSessionLifecycleAndAccounting verifies sessions close cleanly
// and the engine's session accounting reflects real connections.
func TestSessionLifecycleAndAccounting(t *testing.T) {
	target := echoServer(t)
	engine := directEngine(t, nil)

	conn := socks5ClientHandshake(t, engine.Endpoint(), target)

	// The session exists while the tunnel is open.
	deadline := time.Now().Add(2 * time.Second)

	for engine.Snapshot().Sessions == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if engine.Snapshot().Sessions != 1 {
		t.Fatalf("live sessions = %d, want 1", engine.Snapshot().Sessions)
	}

	_ = conn.Close()

	deadline = time.Now().Add(2 * time.Second)

	for engine.Snapshot().Sessions != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if engine.Snapshot().Sessions != 0 {
		t.Fatalf("sessions after close = %d, want 0", engine.Snapshot().Sessions)
	}
}

// TestEngineShutdownCancelsSessions proves end-to-end cancellation:
// stopping the engine closes open tunnels (the copies unblock), not
// just the listeners.
func TestEngineShutdownCancelsSessions(t *testing.T) {
	target := echoServer(t)

	port := freePort(t)

	engine, err := NewEngine(Options{
		Route: Route{
			Outbound:    OutboundDirect,
			Network:     NetworkTCP,
			Security:    SecurityNone,
			DialTimeout: 3 * time.Second,
		},
		LocalHost: "127.0.0.1",
		LocalPort: port,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := engine.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	conn := socks5ClientHandshake(t, engine.Endpoint(), target)

	readErr := make(chan error, 1)

	go func() {
		buf := make([]byte, 16)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Read(buf)
		readErr <- err
	}()

	cancel()
	engine.Stop(0)

	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("read on a cancelled tunnel must fail, not return data")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not close the open tunnel within the bound")
	}
}

// TestEngineSessionLimitIsFailClosed proves the bounded session
// registry refuses beyond the cap instead of growing.
func TestEngineSessionLimitIsFailClosed(t *testing.T) {
	target := echoServer(t)

	engine := directEngine(t, func(o *Options) { o.MaxSessions = 1 })

	first := socks5ClientHandshake(t, engine.Endpoint(), target)
	defer first.Close()

	// Hold one live session with pending data.
	if _, err := first.Write([]byte("hold")); err != nil {
		t.Fatalf("hold write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for engine.Snapshot().Sessions < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	second, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}

	defer second.Close()

	_ = second.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := second.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("second greeting: %v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(second, reply); err != nil {
		t.Fatalf("second reply: %v", err)
	}

	// The refusal is protocol-level: no-auth accepted at greeting,
	// then the CONNECT gets general failure (0x01).
	if _, err := second.Write([]byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}); err != nil {
		t.Fatalf("second connect: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(second, head); err != nil {
		t.Fatalf("second connect reply: %v", err)
	}

	if head[1] != 0x01 {
		t.Fatalf("over-limit CONNECT reply = 0x%02x, want 0x01 (refused, fail-closed)", head[1])
	}
}

// TestEngineWaitCleanStop verifies the InProcessRuntime contract:
// Wait returns nil after a clean stop.
func TestEngineWaitCleanStop(t *testing.T) {
	engine := directEngine(t, nil)

	waitDone := make(chan error, 1)

	go func() { waitDone <- engine.Wait(context.Background()) }()

	engine.Stop(0)

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("clean stop Wait = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Stop")
	}
}

// silentServer accepts TCP connections and never speaks — the real
// remote-side stall the dial/handshake bounds exist for.
func silentServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("silent listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			// Hold the connection open, say nothing.
			go func(c net.Conn) {
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String()
}

// TestOutboundTimeoutBoundsStalledRemote proves the timeout contract
// with a REAL stalled remote: the first-party SOCKS5 outbound's
// dial+handshake bound refuses the session within the configured
// window — no hang, no retry storm, the honest refusal reply.
func TestOutboundTimeoutBoundsStalledRemote(t *testing.T) {
	target := echoServer(t)
	remote := silentServer(t)

	remoteHost, remotePortStr, _ := net.SplitHostPort(remote)

	var remotePort int

	_, _ = fmtSscan(remotePortStr, &remotePort)

	engine := directEngine(t, func(o *Options) {
		o.Route.Outbound = OutboundSOCKS5
		o.Route.Endpoint = Endpoint{Host: remoteHost, Port: remotePort}
		o.Route.DialTimeout = 250 * time.Millisecond
	})

	started := time.Now()

	conn := socks5ClientHandshakeExpectRefusal(t, engine.Endpoint(), target)

	_ = conn.Close()

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stalled remote was not bounded: %v", elapsed)
	}
}

// socks5ClientHandshakeExpectRefusal performs the SOCKS5 greeting and
// a CONNECT whose upstream dial stalls: the engine must reply with
// general failure (0x01) once the dial bound elapses.
func socks5ClientHandshakeExpectRefusal(t *testing.T, addr, target string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}

	host, portStr, _ := net.SplitHostPort(target)

	var port int

	_, _ = fmtSscan(portStr, &port)

	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	request = append(request, byte(port>>8), byte(port&0xFF))

	if _, err := conn.Write(request); err != nil {
		t.Fatalf("connect request: %v", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		t.Fatalf("connect reply: %v", err)
	}

	if head[1] != 0x01 {
		t.Fatalf("stalled-dial reply code = 0x%02x, want 0x01 (refused within the bound)", head[1])
	}

	return conn
}

// TestInboundHandshakeTimeoutBoundsStalledClients proves the inbound
// handshake bound: a client that connects and never speaks is closed
// by the engine within the configured window — it cannot pin a
// goroutine or a session slot.
func TestInboundHandshakeTimeoutBoundsStalledClients(t *testing.T) {
	engine := directEngine(t, func(o *Options) { o.HandshakeTimeout = 250 * time.Millisecond })

	conn, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	defer conn.Close()

	started := time.Now()

	// Say nothing. The engine must close the connection.
	buf := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Read(buf); err == nil {
		t.Fatal("a stalled client must be closed by the engine")
	}

	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("stalled handshake not bounded: %v", elapsed)
	}
}
