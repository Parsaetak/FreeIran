package freecore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingRouter wraps a Router and counts decisions — the probe for
// "every traffic path consults the ONE Router".
type countingRouter struct {
	inner Router
	count *atomic.Int64
}

// Decide implements Router.
func (c countingRouter) Decide(ctx context.Context, target string) RouterDecision {
	c.count.Add(1)

	return c.inner.Decide(ctx, target)
}

// blockingRouter refuses every target — the probe for "a BLOCK decision
// reaches the inbound that used to bypass the Router".
type blockingRouter struct {
	count *atomic.Int64
}

// Decide implements Router.
func (b blockingRouter) Decide(_ context.Context, _ string) RouterDecision {
	b.count.Add(1)

	return RouterDecision{
		Action:   ActionBlock,
		Outbound: "direct",
		Reason:   "test block-all policy",
		RuleID:   "test-block-all",
	}
}

// TestHTTPAbsoluteFormUsesCentralRouter is the bypass regression: the
// absolute-form path (GET http://host/path) MUST consult the central
// Router — the pre-0.14.1 shortcut went beginSession → outboundDecision
// and never saw a BLOCK decision. A block-all policy must produce a
// 403-class reply, ZERO origin dials and ONE recorded decision.
func TestHTTPAbsoluteFormUsesCentralRouter(t *testing.T) {
	// The origin: any contact at all is a routing bypass.
	originContact := make(chan struct{}, 1)

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

			select {
			case originContact <- struct{}{}:
			default:
			}

			_ = conn.Close()
		}
	}()

	var decisions atomic.Int64

	engine := directEngine(t, func(o *Options) {
		o.Router = blockingRouter{count: &decisions}
	})

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

	if !bytes.Contains(response, []byte("403")) {
		t.Fatalf("response = %q, want a 403-class refusal from the Router's BLOCK decision", response)
	}

	select {
	case <-originContact:
		t.Fatal("the origin was contacted despite a BLOCK decision — routing bypass")
	default:
	}

	if got := decisions.Load(); got != 1 {
		t.Fatalf("router consulted %d times, want exactly 1 for the absolute-form request", got)
	}
}

// TestHTTPConnectUsesCentralRouter: the same probe for the CONNECT
// form — the tunnel opens through one central decision. The target is
// a local echo listener (the route is DIRECT-shaped, so the dial lands
// on it).
func TestHTTPConnectUsesCentralRouter(t *testing.T) {
	var decisions atomic.Int64

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}

	t.Cleanup(func() { _ = echo.Close() })

	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	engine := directEngine(t, func(o *Options) {
		o.Router = countingRouter{inner: DefaultRouter(o.Route), count: &decisions}
	})

	conn, err := net.DialTimeout("tcp", engine.Endpoint(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial engine: %v", err)
	}

	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	request := "CONNECT " + echo.Addr().String() + " HTTP/1.1\r\nHost: " + echo.Addr().String() + "\r\n\r\n"

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("CONNECT request: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("CONNECT reply: %v", err)
	}

	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT reply = %q, want success through the central Router", line)
	}

	if got := decisions.Load(); got < 1 {
		t.Fatalf("router consulted %d times for CONNECT, want >= 1", got)
	}
}

// TestDirectDecisionUsesEngineResolver pins the resolver authority:
// a DIRECT dial of a DOMAIN target resolves through the ENGINE's
// resolver (the Router's "engine" choice made real — no hidden system
// resolution), and an IP literal dials untouched. The fake resolver
// records its queries; the fake transport records what it was asked
// to dial.
func TestDirectDecisionUsesEngineResolver(t *testing.T) {
	var (
		queries  atomic.Int64
		lastDial atomic.Value // string
	)

	resolver := fakeResolverFunc(func(ctx context.Context, host string) ([]netip.Addr, error) {
		queries.Add(1)

		if host != "target.invalid" {
			t.Errorf("resolver queried for %q, want target.invalid", host)
		}

		return []netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil
	})

	direct := &DirectOutbound{
		Timeout:   time.Second,
		Resolver:  resolver,
		transport: recordingDialer{last: &lastDial},
	}

	_, err := direct.Dial(context.Background(), "target.invalid:443")
	if err == nil {
		t.Fatal("expected the recording transport's refusal (the dial must flow through it)")
	}

	if got := queries.Load(); got != 1 {
		t.Fatalf("resolver queries = %d, want exactly 1 for the domain target", got)
	}

	if dialed, _ := lastDial.Load().(string); dialed != "203.0.113.9:443" {
		t.Fatalf("dial address = %q, want the RESOLVED literal 203.0.113.9:443", dialed)
	}

	// An IP literal target must NOT consult the resolver (a literal is
	// the Router's "none" choice; resolving it would be a second,
	// hidden DNS decision).
	queries.Store(0)

	_, err = direct.Dial(context.Background(), "198.51.100.7:80")
	if err == nil {
		t.Fatal("expected the recording transport's refusal for the literal dial")
	}

	if got := queries.Load(); got != 0 {
		t.Fatalf("resolver consulted %d times for an IP literal, want 0", got)
	}
}

// fakeResolverFunc adapts a function to the Resolver seam.
type fakeResolverFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// Resolve implements Resolver.
func (f fakeResolverFunc) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return f(ctx, host)
}

// recordingDialer is a Dialer that refuses every dial and records the
// address it was asked for.
type recordingDialer struct {
	last *atomic.Value
}

// DialContext implements Dialer.
func (d recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.last.Store(address)

	return nil, errors.New("recording dialer refuses (test)")
}
