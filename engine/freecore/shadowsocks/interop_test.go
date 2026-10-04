package shadowsocks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

// Interoperability evidence against the independent reference
// implementation github.com/shadowsocks/go-shadowsocks2 (v0.1.5).
//
// The reference module's relay/tunnel drivers live in package main and
// are therefore not importable; this file re-implements exactly their
// wire-visible behavior using their importable protocol packages
// (core, socks): core.PickCipher + core.Cipher.StreamConn + socks
// address encoding, and a verbatim copy of their main.relay pump (the
// only importable substitute — see refRelay below).
//
// Evidence gate (see doc.go): for every supported method we must show
//   1. OUR client (ClientConfig.DialTCP) through THEIR server, and
//   2. THEIR client (core.StreamConn + socks.ParseAddr) through OUR
//      server fixture (ServeConn),
// with byte equality in both directions and clean end-of-stream.

const interopPassword = "interop-evidence-password-014"

// refRelay is a verbatim behavioral copy of go-shadowsocks2 main.relay
// (bidirectional copy with the 5s read-deadline unblock trick), so the
// reference server fixture below behaves like the real go-shadowsocks2
// tcpRemote on the wire.
func refRelay(left, right net.Conn) error {
	var err, err1 error
	var wg sync.WaitGroup
	var wait = 5 * time.Second
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err1 = io.Copy(right, left)
		_ = right.SetReadDeadline(time.Now().Add(wait)) // unblock read on right
	}()
	_, err = io.Copy(left, right)
	_ = left.SetReadDeadline(time.Now().Add(wait)) // unblock read on left
	wg.Wait()
	if err1 != nil && !errors.Is(err1, os.ErrDeadlineExceeded) {
		return err1
	}
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	return nil
}

// refPickCipher builds the reference implementation's cipher for one of
// our Method names, deriving the master key from the password exactly as
// their main does.
func refPickCipher(t *testing.T, method Method) core.Cipher {
	t.Helper()
	ciph, err := core.PickCipher(strings.ToUpper(string(method)), nil, interopPassword)
	if err != nil {
		t.Fatalf("reference PickCipher(%s): %v", method, err)
	}
	return ciph
}

// refServerFixture runs a go-shadowsocks2-style TCP remote on loopback:
// per connection — StreamConn cipher wrap, socks.ReadAddr target parse,
// dial the target, their relay. Mirrors tcpRemote from their main.
type refServerFixture struct {
	ln net.Listener
}

func newRefServerFixture(t *testing.T, method Method) *refServerFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reference server listen: %v", err)
	}
	ciph := refPickCipher(t, method)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(raw net.Conn) {
				defer raw.Close()
				sc := ciph.StreamConn(raw)

				tgt, err := socks.ReadAddr(sc)
				if err != nil {
					// Mirror their probe-resistant drain on failure.
					_, _ = io.Copy(io.Discard, sc)
					return
				}

				rc, err := net.Dial("tcp", tgt.String())
				if err != nil {
					return
				}
				defer rc.Close()

				_ = refRelay(sc, rc)
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return &refServerFixture{ln: ln}
}

// TestInteropOurClientThroughTheirServer runs OUR DialTCP client against
// the reference server fixture for every AEAD method.
func TestInteropOurClientThroughTheirServer(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			echo := newEchoServer(t)
			ref := newRefServerFixture(t, method)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, err := ClientConfig{
				Host:     "127.0.0.1",
				Port:     mustPort(ref.ln.Addr().String()),
				Password: interopPassword,
				Method:   method,
			}.DialTCP(ctx, echo.addr)
			if err != nil {
				t.Fatalf("our client through their server: DialTCP: %v", err)
			}
			defer client.Close()

			runInteropEcho(t, client)
		})
	}
}

// TestInteropTheirClientThroughOurServer drives the reference client
// (core.StreamConn + socks.ParseAddr, as their tcpLocal does) against OUR
// ServeConn fixture for every AEAD method.
func TestInteropTheirClientThroughOurServer(t *testing.T) {
	for _, method := range []Method{MethodAES128GCM, MethodAES256GCM, MethodChaCha20IETFPoly1305} {
		t.Run(string(method), func(t *testing.T) {
			echo := newEchoServer(t)
			server := newSSServer(t, method, interopPassword)

			raw, err := net.Dial("tcp", server.addr)
			if err != nil {
				t.Fatalf("dial our server: %v", err)
			}

			sc := refPickCipher(t, method).StreamConn(raw)

			// Their client sends the target as the first bytes of the
			// encrypted stream (socks.ParseAddr of host:port).
			tgt := socks.ParseAddr(echo.addr)
			if tgt == nil {
				t.Fatalf("reference ParseAddr(%q) failed", echo.addr)
			}
			if _, err := sc.Write(tgt); err != nil {
				t.Fatalf("their client send target: %v", err)
			}

			runInteropEcho(t, sc)

			// End the stream the reference way — close the connection —
			// and require our server fixture to relay it cleanly to
			// the end.
			_ = raw.Close()
			errs := server.waitConns(1, 10*time.Second)
			if len(errs) < 1 {
				t.Fatal("our server never finished serving the reference client")
			}
			if errs[0] != nil {
				t.Fatalf("ServeConn returned %v on the interop path", errs[0])
			}
		})
	}
}

// runInteropEcho exchanges a deterministic payload through an established
// tunnel (their client or ours) and requires byte equality both ways.
//
// Stream end is signaled by closing the tunnel rather than by TCP
// half-close: the reference implementation's streamConn (an embedding of
// the net.Conn interface) does not expose CloseWrite either, and their
// relay unblocks the reverse pump with read deadlines instead of
// half-closes — so both ends read exactly the expected number of bytes
// and then close, which both readers map to a clean boundary EOF.
func runInteropEcho(t *testing.T, tunnel net.Conn) {
	t.Helper()

	payload := deterministicPayload(64<<10+12345, 123456789)

	// Concurrent reader: echoed direction while we write.
	readDone := make(chan struct{})
	got := make([]byte, len(payload))
	var readErr error
	go func() {
		defer close(readDone)
		_, readErr = io.ReadFull(tunnel, got)
	}()

	if _, err := tunnel.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	select {
	case <-readDone:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the echoed direction")
	}
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
