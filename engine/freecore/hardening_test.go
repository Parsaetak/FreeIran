package freecore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testRoute is the normalized route used by the lifecycle/router/DNS
// tests (a SOCKS5 remote shape; never dialed by these tests directly).
func testRoute() Route {
	return Route{
		ConfigID:    "hardening-test",
		Outbound:    OutboundSOCKS5,
		Endpoint:    Endpoint{Host: "198.51.100.1", Port: 1080},
		Network:     NetworkTCP,
		Security:    SecurityNone,
		DialTimeout: 3 * time.Second,
	}
}

// runtimeNumGoroutine isolates the runtime import for one probe.
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

// --- Phase A: engine lifecycle hardening ------------------------------------

// TestEngineStopDrainsSessions proves the Phase 2 shutdown contract:
// after Stop(grace>0) returns, the session registry has reached zero
// (every serve goroutine exited) and Wait reports a clean stop.
func TestEngineStopDrainsSessions(t *testing.T) {
	engine := directEngine(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	engine.Stop(2 * time.Second)

	if err := engine.Wait(ctx); err != nil {
		t.Fatalf("Wait after drained Stop = %v, want nil", err)
	}

	if !engine.registry.isDraining() {
		t.Fatal("registry must be draining after Stop")
	}
}

// TestEngineStopIdempotentConcurrent hammers Stop from many goroutines:
// it must be safe, idempotent in effect, and leave Wait reporting a
// clean stop.
func TestEngineStopIdempotentConcurrent(t *testing.T) {
	engine := directEngine(t, nil)

	var wg sync.WaitGroup

	for i := 0; i < 32; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			engine.Stop(time.Second)
		}()
	}

	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := engine.Wait(ctx); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

// TestEngineStopRefusesNewSessions proves the drain gate: after Stop
// begins, session begins are refused, not born-then-cancelled.
func TestEngineStopRefusesNewSessions(t *testing.T) {
	engine := directEngine(t, nil)

	engine.Stop(time.Second)

	_, _, _, err := engine.beginSession(context.Background())
	if err == nil {
		t.Fatal("beginSession after Stop must be refused")
	}

	var drain *drainingError

	if !errors.As(err, &drain) {
		t.Fatalf("refusal = %v, want the draining error", err)
	}
}

// TestEngineListenerFailureObservable proves a listener failure is an
// engine-level failure surfaced through Wait — never a silent dead
// inbound.
func TestEngineListenerFailureObservable(t *testing.T) {
	engine := directEngine(t, nil)

	// Kill the mixed listener directly: the accept loop must classify
	// the error, record the engine failure and stop.
	if err := engine.mixed.Close(); err != nil {
		t.Fatalf("listener close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := engine.Wait(ctx)
	if err == nil {
		t.Fatal("Wait = nil, want the accept failure (observable engine failure)")
	}
}

// TestEngineParentCancellationClosesListeners proves the hardening
// gap-closure: cancelling the PARENT context produces the same
// observable shutdown as Stop — listeners closed, Wait unblocked.
func TestEngineParentCancellationClosesListeners(t *testing.T) {
	port := freePort(t)

	engine, err := NewEngine(Options{
		Route:     testRoute(),
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

	// The listener is reachable, then the parent cancel tears it down.
	conn, dialErr := net.DialTimeout("tcp", engine.Endpoint(), time.Second)
	if dialErr == nil {
		_ = conn.Close()
	}

	cancel()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()

	if err := engine.Wait(waitCtx); err != nil {
		t.Fatalf("Wait after parent cancel = %v, want nil", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		c, dialErr := net.DialTimeout("tcp", engine.Endpoint(), 200*time.Millisecond)
		if dialErr != nil {
			// Listener is closed: the shutdown was real.
			return
		}

		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("listener still accepting after parent cancellation")
}

// TestEngineGraceWaitsForLiveSessions proves grace has real meaning: a
// live TUN flow (started through the REAL served pipeline) is allowed
// to finish within the grace window, and Stop only reports after every
// serve goroutine — including the flow handler — exited.
func TestEngineGraceWaitsForLiveSessions(t *testing.T) {
	remote := localSOCKS5Remote(t)

	remoteHost, remotePortStr, _ := net.SplitHostPort(remote)

	var remotePort int

	_, _ = fmtSscan(remotePortStr, &remotePort)

	engine, err := NewEngine(Options{
		Route: Route{
			Outbound:    OutboundSOCKS5,
			Endpoint:    Endpoint{Host: remoteHost, Port: remotePort},
			Network:     NetworkTCP,
			Security:    SecurityNone,
			DialTimeout: 2 * time.Second,
		},
		LocalHost: "127.0.0.1",
		LocalPort: freePort(t),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	client, server := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	go engine.HandleTUNFlow(context.Background(), server, "203.0.113.77:443")

	// Give the flow a moment to establish (bounded session + dial +
	// SOCKS5 handshake against the loopback fixture).
	time.Sleep(300 * time.Millisecond)

	if got := engine.registry.Len(); got != 1 {
		t.Fatalf("live sessions = %d, want 1 before stop", got)
	}

	stopReturned := make(chan struct{})

	go func() {
		engine.Stop(3 * time.Second)

		close(stopReturned)
	}()

	readErr := make(chan error, 1)

	go func() {
		buf := make([]byte, 1)
		_, err := client.Read(buf)

		readErr <- err
	}()

	select {
	case <-stopReturned:
		// Stop returned — the flow must have been torn down already.
		if got := engine.registry.Len(); got != 0 {
			t.Fatalf("registry = %d after drained Stop, want 0", got)
		}

		select {
		case err := <-readErr:
			if err == nil {
				t.Fatal("flow conn still delivering after drained stop")
			}
		case <-time.After(time.Second):
			t.Fatal("flow conn not closed after Stop returned")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Stop(grace) never returned")
	}
}

// TestEngineNoGoroutineOutlivesStop proves no engine-owned goroutine
// outlives the owning engine (bounded settle, race-detector covered).
func TestEngineNoGoroutineOutlivesStop(t *testing.T) {
	before := runtimeNumGoroutine()

	for i := 0; i < 5; i++ {
		engine := directEngine(t, nil)

		// A few live sessions through the real inbound.
		var wg sync.WaitGroup

		for j := 0; j < 3; j++ {
			wg.Add(1)

			go func() {
				defer wg.Done()

				conn, err := net.DialTimeout("tcp", engine.Endpoint(), time.Second)
				if err != nil {
					return
				}

				// Speak SOCKS5 to enter a session (it will be refused
				// and closed; the point is goroutine churn).
				_, _ = conn.Write([]byte{0x05, 0x01, 0x00})
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				buf := make([]byte, 2)
				_, _ = conn.Read(buf)
				_ = conn.Close()
			}()
		}

		time.Sleep(100 * time.Millisecond)
		engine.Stop(time.Second)
		wg.Wait()
	}

	time.Sleep(500 * time.Millisecond)

	after := runtimeNumGoroutine()

	if after > before+10 {
		t.Fatalf("goroutines grew from %d to %d across start/stop cycles", before, after)
	}
}

// --- Phase E: routing authority ---------------------------------------------

func TestRouterDefaultPolicyIsProxy(t *testing.T) {
	route := testRoute()
	router := DefaultRouter(route)

	for _, target := range []string{"203.0.113.9:443", "example.com:80", "8.8.8.8:53"} {
		decision := router.Decide(context.Background(), target)
		if decision.Action != ActionProxy {
			t.Fatalf("target %s: action = %s, want proxy (default policy)", target, decision.Action)
		}

		if decision.Outbound != string(route.Outbound) {
			t.Fatalf("target %s: outbound = %s", target, decision.Outbound)
		}

		if decision.ResolverChoice != "remote" {
			t.Fatalf("target %s: proxied domains resolve remotely, got %q", target, decision.ResolverChoice)
		}

		if decision.RuleID != "default-policy" {
			t.Fatalf("target %s: rule = %s", target, decision.RuleID)
		}
	}
}

func TestRouterMalformedTargetBlocks(t *testing.T) {
	router := DefaultRouter(testRoute())

	decision := router.Decide(context.Background(), "no-port-here")
	if decision.Action != ActionBlock {
		t.Fatalf("malformed target action = %s, want block (fail-closed)", decision.Action)
	}
}

func TestRouterPrivateDirectPolicy(t *testing.T) {
	route := testRoute()

	router := DefaultRouterWithPolicy(route, RouterPolicy{
		PrivateDirect:   true,
		DefaultOutbound: string(route.Outbound),
	})

	decision := router.Decide(context.Background(), "192.168.1.20:445")
	if decision.Action != ActionDirect || decision.RuleID != "private-direct" {
		t.Fatalf("private destination decision = %+v, want direct/private-direct", decision)
	}

	decision = router.Decide(context.Background(), "127.0.0.1:8080")
	if decision.Action != ActionDirect {
		t.Fatalf("loopback decision = %+v, want direct", decision)
	}

	// Public stays proxied.
	decision = router.Decide(context.Background(), "203.0.113.9:443")
	if decision.Action != ActionProxy {
		t.Fatalf("public decision = %+v, want proxy", decision)
	}
}

func TestRouterBlockList(t *testing.T) {
	router := DefaultRouterWithPolicy(testRoute(), RouterPolicy{
		PrivateDirect:   true,
		DefaultOutbound: string(OutboundSOCKS5),
		BlockCIDRs:      []netip.Prefix{netip.MustParsePrefix("10.66.0.0/16")},
	})

	decision := router.Decide(context.Background(), "10.66.3.4:993")
	if decision.Action != ActionBlock || decision.RuleID != "block-list" {
		t.Fatalf("blocked decision = %+v, want block/block-list", decision)
	}

	decision = router.Decide(context.Background(), "203.0.113.44:993")
	if decision.Action != ActionProxy {
		t.Fatalf("outside block list = %+v, want proxy", decision)
	}
}

func TestRouterDirectList(t *testing.T) {
	router := DefaultRouterWithPolicy(testRoute(), RouterPolicy{
		DefaultOutbound: string(OutboundSOCKS5),
		DirectCIDRs:     []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
	})

	decision := router.Decide(context.Background(), "198.51.100.7:80")
	if decision.Action != ActionDirect || decision.RuleID != "direct-list" {
		t.Fatalf("direct-list decision = %+v", decision)
	}
}

// TestRouterSharedByInboundAndTUN proves ONE authority: the session
// records the router decision, and the TUN flow path runs the same
// router (a BLOCK decision closes the TUN flow before any dial).
func TestRouterSharedByInboundAndTUN(t *testing.T) {
	var decisions atomic.Int64

	router := decisionCountingRouter{inner: DefaultRouter(testRoute()), count: &decisions}

	port := freePort(t)

	engine, err := NewEngine(Options{
		Route:     testRoute(),
		LocalHost: "127.0.0.1",
		LocalPort: port,
		Router:    router,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if err := engine.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Cleanup(func() { engine.Stop(time.Second) })

	// One local inbound flow and one TUN flow — both must consult the
	// SAME router.
	socks5RefusedProbe(t, engine.Endpoint(), "203.0.113.44:80", 6*time.Second)

	blocking := tunBlockedConn(t)

	go engine.HandleTUNFlow(context.Background(), blocking, "10.66.0.1:9999")

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if decisions.Load() >= 2 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if got := decisions.Load(); got < 2 {
		t.Fatalf("router consulted %d times across paths, want >= 2", got)
	}
}

type decisionCountingRouter struct {
	inner Router
	count *atomic.Int64
}

// Decide implements Router.
func (d decisionCountingRouter) Decide(ctx context.Context, target string) RouterDecision {
	d.count.Add(1)

	return d.inner.Decide(ctx, target)
}

// tunBlockedConn is a pipe-backed conn standing in for a TUN flow.
func tunBlockedConn(t *testing.T) net.Conn {
	t.Helper()

	client, server := net.Pipe()

	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	return server
}

// socks5RefusedProbe performs a minimal SOCKS5 CONNECT and expects the
// tunnel to fail (direct outbound to a closed port).
func socks5RefusedProbe(t *testing.T, addr, target string, budget time.Duration) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial inbound: %v", err)
	}

	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(budget))

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}

	greeting := make([]byte, 2)
	if _, err := readFullProbe(conn, greeting); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}

	port := 80
	request := []byte{0x05, 0x01, 0x00, 0x01, 203, 0, 113, 44, byte(port >> 8), byte(port)}
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("request: %v", err)
	}

	reply := make([]byte, 4)
	if _, err := readFullProbe(conn, reply); err != nil {
		t.Fatalf("reply head: %v", err)
	}

	if reply[1] == 0x00 {
		t.Fatal("flow to a dead target reported success")
	}
}

func readFullProbe(conn net.Conn, buf []byte) (int, error) {
	total := 0

	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}

	return total, nil
}

// --- Phase F: DNS authority --------------------------------------------------

type scriptedResolver struct {
	mu    sync.Mutex
	calls map[string]int
	fail  error
	addrs []netip.Addr
	delay time.Duration
}

func newScriptedResolver(addrs []netip.Addr, fail error) *scriptedResolver {
	return &scriptedResolver{calls: map[string]int{}, addrs: addrs, fail: fail}
}

// Resolve implements Resolver.
func (s *scriptedResolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	s.mu.Lock()
	s.calls[host]++
	delay := s.delay
	fail := s.fail
	addrs := s.addrs
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if fail != nil {
		return nil, fail
	}

	return addrs, nil
}

func (s *scriptedResolver) callsFor(host string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls[host]
}

func TestDNSCachePositiveAndNegative(t *testing.T) {
	upstream := newScriptedResolver([]netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil)
	resolver := NewCachingResolver(upstream, "socks5", 16, time.Minute, time.Minute)

	addrs, err := resolver.Resolve(context.Background(), "Example.com.")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if len(addrs) != 1 || addrs[0] != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("addrs = %v", addrs)
	}

	// Second lookup is a cache hit (one upstream call).
	if _, err := resolver.Resolve(context.Background(), "example.com"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	if got := upstream.callsFor("example.com"); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (bounded cache must serve the second)", got)
	}

	// Failure is negatively cached with the same discipline.
	failing := newScriptedResolver(nil, errors.New("servfail"))
	negResolver := NewCachingResolver(failing, "socks5", 16, time.Minute, time.Minute)

	if _, err := negResolver.Resolve(context.Background(), "dead.example"); err == nil {
		t.Fatal("expected failure")
	}

	if _, err := negResolver.Resolve(context.Background(), "dead.example"); err == nil {
		t.Fatal("expected cached failure")
	}

	if got := failing.callsFor("dead.example"); got != 1 {
		t.Fatalf("failing upstream calls = %d, want 1 (negative caching)", got)
	}
}

func TestDNSCacheBound(t *testing.T) {
	upstream := newScriptedResolver([]netip.Addr{netip.MustParseAddr("203.0.113.11")}, nil)
	resolver := NewCachingResolver(upstream, "direct", 8, time.Minute, time.Minute)

	for i := 0; i < 64; i++ {
		host := fmt.Sprintf("host%d.example", i)
		if _, err := resolver.Resolve(context.Background(), host); err != nil {
			t.Fatalf("resolve %s: %v", host, err)
		}
	}

	resolver.mu.RLock()
	size := len(resolver.cache)
	resolver.mu.RUnlock()

	if size > 8 {
		t.Fatalf("cache size = %d, want <= 8 (the bound is real)", size)
	}
}

func TestDNSCancellationReal(t *testing.T) {
	upstream := newScriptedResolver(nil, nil)
	upstream.delay = 5 * time.Second
	resolver := NewCachingResolver(upstream, "socks5", 16, time.Minute, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, err := resolver.Resolve(ctx, "slow.example")
	if err == nil {
		t.Fatal("expected cancellation error")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v — it is not real", elapsed)
	}

	if got := upstream.callsFor("slow.example"); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}

	// A caller-cancelled lookup must NOT be negatively cached.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	if _, err := resolver.Resolve(ctx2, "slow.example"); err == nil {
		t.Fatal("expected second cancellation error")
	}

	// Wait past the negative TTL window equivalent: the entry must not
	// exist at all.
	resolver.mu.RLock()
	_, cached := resolver.cache["slow.example"]
	resolver.mu.RUnlock()

	if cached {
		t.Fatal("caller-cancelled lookup was cached — cancellation must not be a DNS decision")
	}
}

func TestDNSObservations(t *testing.T) {
	upstream := newScriptedResolver([]netip.Addr{netip.MustParseAddr("203.0.113.12")}, nil)
	resolver := NewCachingResolver(upstream, "socks5", 16, time.Minute, time.Minute)

	_, _ = resolver.Resolve(context.Background(), "first.example")
	_, _ = resolver.Resolve(context.Background(), "first.example")

	observations := resolver.Observations()
	if len(observations) != 2 {
		t.Fatalf("observations = %d, want 2", len(observations))
	}

	first := observations[0]
	if first.Host != "first.example" || first.Resolver != "system" || !first.Success || first.CacheHit {
		t.Fatalf("first observation = %+v", first)
	}

	second := observations[1]
	if !second.CacheHit {
		t.Fatalf("second observation must record the cache hit: %+v", second)
	}

	if second.Duration != 0 {
		t.Logf("cache hit timing measured at %v (reported honestly)", second.Duration)
	}
}

func TestBootstrapResolverParsesIPAndDelegates(t *testing.T) {
	resolver := BootstrapResolver{Servers: []string{"127.0.0.1:1"}}

	// IP literals never touch the network.
	addrs, err := resolver.Resolve(context.Background(), "203.0.113.30")
	if err != nil || len(addrs) != 1 {
		t.Fatalf("literal resolve = %v / %v", addrs, err)
	}

	// No reachable server: honest failure, no invented answers.
	if _, err := resolver.Resolve(context.Background(), "unreachable.example"); err == nil {
		t.Fatal("bootstrap must fail honestly without reachable servers")
	}
}
