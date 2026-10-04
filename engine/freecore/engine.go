package freecore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Parsaetak/FreeIran/internal/logging"
)

// Options configure one engine run (one configuration session).
type Options struct {
	// Route is the normalized remote the engine forwards through.
	Route Route

	// LocalHost is the inbound listen address (default 127.0.0.1 —
	// the local proxy surface is loopback by design).
	LocalHost string

	// LocalPort is the mixed inbound port (SOCKS5 + HTTP CONNECT
	// protocol-dispatched on one port, like the external cores'
	// mixed inbound). Must be resolved (>0) before Run.
	LocalPort int

	// HTTPPort optionally adds a dedicated HTTP CONNECT inbound
	// (0 = the mixed port serves both).
	HTTPPort int

	// MaxSessions bounds concurrent proxied connections (0 =
	// DefaultMaxSessions). Fail-closed beyond the cap.
	MaxSessions int

	// HandshakeTimeout bounds one inbound protocol handshake
	// (0 = DefaultHandshakeTimeout). A client that connects and
	// stalls cannot pin an engine goroutine.
	HandshakeTimeout time.Duration

	// Resolver overrides the DNS seam (tests; default system).
	Resolver Resolver

	// Router overrides the routing authority (tests; default
	// DefaultRouter for the run's route — Phase E: ONE decision
	// authority for every path, local inbounds and TUN flows alike).
	Router Router

	// UpstreamDialer is the loop-prevention constrained transport for
	// the engine's upstream dials (the TUN dataplane passes the
	// physical-interface bound dialer; nil = plain system dialing).
	UpstreamDialer Dialer
}

// Engine is one first-party engine run: bound local inbounds, a
// bounded session registry and the outbound pipeline for one
// normalized route. It runs entirely in-process — no child process,
// no supervisor, no second connection authority.
type Engine struct {
	opts     Options
	route    Route
	outbound Outbound
	router   Router
	resolver Resolver

	registry *sessionRegistry

	handshakeTimeout time.Duration

	mixed net.Listener // SOCKS5 + HTTP CONNECT on one port
	http  net.Listener // dedicated HTTP inbound when configured

	// stateMu guards the lifecycle fields below: Run binds listeners
	// and creates the engine context; Stop/Wait may legally be called
	// from a different goroutine at ANY time, including while Run is
	// still binding (Phase 2 hardening: lifecycle races are designed
	// for, not hoped away).
	stateMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc

	// serveWG covers every goroutine the engine owns that can keep a
	// session alive: both accept loops AND one goroutine per served
	// connection (mixed dispatch, dedicated HTTP, TUN flows). Wait()
	// returns only when this reaches zero, so engine shutdown implies
	// sessions drained — the Phase 2 hardening contract — instead of
	// merely "listeners closed, sessions still winding down".
	serveWG sync.WaitGroup

	stopOnce    sync.Once
	stoppedOnce sync.Once
	stopped     chan struct{}
	stopErr     error
	stopErrMu   sync.Mutex
}

// NewEngine validates the options and constructs the engine. Call
// Run before serving; the listeners are bound synchronously there so
// a bind failure surfaces as an honest Start error. (The package
// level New is the core-backend constructor, matching the
// xray.New()/singbox.New() convention the registry wiring uses.)
func NewEngine(opts Options) (*Engine, error) {
	if opts.LocalPort <= 0 || opts.LocalPort > 65535 {
		return nil, errors.New("freecore: local inbound port must be resolved before engine start")
	}

	if opts.LocalHost == "" {
		opts.LocalHost = "127.0.0.1"
	}

	if opts.HTTPPort < 0 || opts.HTTPPort > 65535 {
		return nil, errors.New("freecore: http inbound port out of range")
	}

	resolver := opts.Resolver
	if resolver == nil {
		resolver = SystemResolver()
	}

	engine := &Engine{
		opts:     opts,
		route:    opts.Route,
		outbound: outboundFor(opts.Route, opts.UpstreamDialer),
		resolver: resolver,
		router:   opts.Router,
		registry: newSessionRegistry(opts.MaxSessions),
		stopped:  make(chan struct{}),
	}

	if engine.router == nil {
		engine.router = DefaultRouter(opts.Route)
	}

	engine.handshakeTimeout = opts.HandshakeTimeout
	if engine.handshakeTimeout <= 0 {
		engine.handshakeTimeout = DefaultHandshakeTimeout
	}

	return engine, nil
}

// Run binds the listeners and serves until the context is cancelled
// or Stop is called. Binding happens synchronously before the accept
// loops start so callers observe bind failures immediately.
func (e *Engine) Run(ctx context.Context) error {
	mixed, err := net.Listen("tcp", net.JoinHostPort(e.opts.LocalHost, strconv.Itoa(e.opts.LocalPort)))
	if err != nil {
		return fmt.Errorf("freecore: bind mixed inbound %s:%d: %w",
			e.opts.LocalHost, e.opts.LocalPort, err)
	}

	e.stateMu.Lock()
	e.mixed = mixed
	e.stateMu.Unlock()

	if e.opts.HTTPPort > 0 {
		httpListener, err := net.Listen("tcp", net.JoinHostPort(e.opts.LocalHost, strconv.Itoa(e.opts.HTTPPort)))
		if err != nil {
			_ = mixed.Close()

			return fmt.Errorf("freecore: bind http inbound %s:%d: %w",
				e.opts.LocalHost, e.opts.HTTPPort, err)
		}

		e.stateMu.Lock()
		e.http = httpListener
		e.stateMu.Unlock()
	}

	e.stateMu.Lock()
	e.ctx, e.cancel = context.WithCancel(ctx)
	engineCtx := e.ctx
	e.stateMu.Unlock()

	e.serveWG.Add(1)

	go e.acceptLoop(engineCtx, mixed, true)

	if e.opts.HTTPPort > 0 {
		e.serveWG.Add(1)

		go e.acceptLoop(engineCtx, e.http, false)
	}

	// A parent-context cancellation (Connection Manager session
	// teardown, Instance.Close on any path) must produce the SAME
	// observable shutdown as an explicit Stop: listeners closed,
	// sessions cancelled, Wait unblocked. Without this watcher a
	// parent cancel left the listeners open — a silent half-dead
	// engine (the v0.13.1 defect this hardening closes).
	go func() {
		<-engineCtx.Done()
		// Parent cancellation is a legitimate shutdown path: it owns the
		// same drain gate as Stop, so the accept loops classify their
		// listener close as a clean shutdown, not an engine failure.
		e.registry.beginDrain()
		e.closeListeners()
	}()

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "engine_start",
		Message: fmt.Sprintf("FreeIran Engine serving %s (outbound %s via %s)",
			e.Endpoint(), e.route.Outbound, e.route.Endpoint),
		Core:     DisplayName,
		Listener: e.Endpoint(),
		Status:   "starting",
	})

	// Shutdown completeness is served-goroutine completeness: when the
	// last accept loop / session handler exits, the registry is
	// drained and Wait may report a clean stop.
	go func() {
		e.serveWG.Wait()

		e.markStopped()
	}()

	return nil
}

// markStopped signals Wait exactly once. It is deliberately separate
// from the stop-action once: Stop performs the shutdown actions, the
// completion monitor (or a drained Stop) signals completeness — one
// once per concern, never shared.
func (e *Engine) markStopped() {
	e.stoppedOnce.Do(func() { close(e.stopped) })
}

// ServeFlowsOnly runs the engine WITHOUT binding local inbounds — the
// first-party TUN mode: flows arrive through HandleTUNFlow (the
// userspace IP stack), not through the mixed port. Shutdown semantics
// are identical to Run (Stop/Wait/drain).
func (e *Engine) ServeFlowsOnly(ctx context.Context) error {
	e.stateMu.Lock()
	e.ctx, e.cancel = context.WithCancel(ctx)
	engineCtx := e.ctx
	e.stateMu.Unlock()

	go func() {
		<-engineCtx.Done()
		e.registry.beginDrain()
	}()

	logging.LogR(logging.Record{
		Level:     logging.LevelInfo,
		Subsystem: Subsystem,
		Event:     "engine_start",
		Message:   fmt.Sprintf("FreeIran Engine serving TUN flows (outbound %s via %s)", e.route.Outbound, e.route.Endpoint),
		Core:      DisplayName,
		Status:    "starting",
	})

	go func() {
		e.serveWG.Wait()

		e.markStopped()
	}()

	return nil
}

// acceptLoop serves one listener until it closes.
func (e *Engine) acceptLoop(ctx context.Context, listener net.Listener, mixed bool) {
	defer e.serveWG.Done()

	for {
		conn, err := listener.Accept()
		if err != nil {
			// Failure classification: an accept error is an engine-level
			// failure UNLESS the engine itself is shutting down. A
			// listener that dies (or is closed by something other than
			// Stop) while the engine believes itself running is recorded
			// honestly — never a silent dead inbound.
			if errors.Is(err, net.ErrClosed) && e.stopping() {
				return
			}

			if errors.Is(err, net.ErrClosed) {
				e.recordStop(errors.New("freecore: inbound listener closed while the engine was running"))
			} else {
				e.recordStop(fmt.Errorf("freecore: inbound accept: %w", err))
			}

			e.Stop(0)

			return
		}

		e.serveWG.Add(1)

		if mixed {
			go e.serveTracked(func() { e.serveMixed(ctx, conn) })
		} else {
			inbound := &HTTPConnectInbound{Engine: e}

			go e.serveTracked(func() { inbound.Serve(ctx, conn) })
		}
	}
}

// serveTracked runs one per-connection goroutine under the serveWG
// accounting that makes engine shutdown a drained shutdown. The accept
// loop increments BEFORE spawning so a Stop racing an accept cannot
// wait on a WaitGroup that never counted the connection.
func (e *Engine) serveTracked(fn func()) {
	defer e.serveWG.Done()

	fn()
}

// serveMixed dispatches one mixed-port connection by its first byte:
// 0x05 is a SOCKS5 greeting, anything else is treated as HTTP.
func (e *Engine) serveMixed(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(e.handshakeTimeout)); err != nil {
		return
	}

	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil {
		return
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}

	prefixed := &prefixConn{Conn: conn, prefix: first[0], pending: true}

	if sniffSOCKS5(first[0]) {
		(&SOCKS5Inbound{Engine: e}).Serve(ctx, prefixed)
		return
	}

	(&HTTPConnectInbound{Engine: e}).Serve(ctx, prefixed)
}

// Endpoint returns the mixed inbound endpoint (the one the connection
// manager's snapshot and System Proxy point at).
func (e *Engine) Endpoint() string {
	if e.mixed == nil {
		return net.JoinHostPort(e.opts.LocalHost, strconv.Itoa(e.opts.LocalPort))
	}

	return e.mixed.Addr().String()
}

// HTTPEndpoint returns the dedicated HTTP inbound endpoint ("" when
// the mixed port serves both).
func (e *Engine) HTTPEndpoint() string {
	if e.http == nil {
		return ""
	}

	return e.http.Addr().String()
}

// Snapshot is the honest engine state view for diagnostics.
type Snapshot struct {
	Endpoint     string `json:"endpoint"`
	HTTPEndpoint string `json:"http_endpoint,omitempty"`
	Outbound     string `json:"outbound"`
	Remote       string `json:"remote"`
	Sessions     int    `json:"sessions"`
	MaxSessions  int    `json:"max_sessions"`
}

// Snapshot returns the current engine state.
func (e *Engine) Snapshot() Snapshot {
	return Snapshot{
		Endpoint:     e.Endpoint(),
		HTTPEndpoint: e.HTTPEndpoint(),
		Outbound:     string(e.route.Outbound),
		Remote:       e.route.Endpoint.String(),
		Sessions:     e.registry.Len(),
		MaxSessions:  e.registry.max,
	}
}

// --- in-process runtime contract (engine/core.LaunchInProcess) -------------

// Stop shuts the engine down. Idempotent and safe under concurrent
// calls. Semantics (the Phase 2 hardening contract):
//
//  1. new sessions are refused immediately (registry drain gate);
//  2. live sessions receive cancellation — their client and upstream
//     connections close, which unblocks every pump goroutine;
//  3. listeners close so no new inbound connection is accepted;
//  4. with grace > 0, Stop waits up to that long for the session
//     registry to drain (every serve goroutine exits) before
//     returning. grace is a real knob: how long already-established
//     sessions may finish their in-flight work. grace == 0 means
//     fire-and-forget; Wait remains the completeness signal in every
//     case.
func (e *Engine) Stop(grace time.Duration) {
	e.stopOnce.Do(func() {
		// Refuse new sessions BEFORE cancelling: a session that begins
		// between the two would be born cancelled, which is correct but
		// noisy — refusing first is the honest contract.
		e.registry.beginDrain()

		e.stateMu.Lock()
		cancel := e.cancel
		e.stateMu.Unlock()

		if cancel != nil {
			cancel()
		}

		e.closeListeners()

		if grace > 0 {
			e.waitDrain(grace)
		}
	})
}

// closeListeners closes the inbound listeners. net.Listener.Close is
// idempotent and the reads are guarded, so concurrent Stop and
// context-cancellation paths cannot race the fields.
func (e *Engine) closeListeners() {
	e.stateMu.Lock()
	mixed, dedicated := e.mixed, e.http
	e.stateMu.Unlock()

	if mixed != nil {
		_ = mixed.Close()
	}

	if dedicated != nil {
		_ = dedicated.Close()
	}
}

// waitDrain waits up to max for every serve goroutine to exit (which
// implies the session registry reached zero). It waits on the same
// WaitGroup the completion monitor waits on — no polling, no missed
// wakeups, and the wait is bounded by the caller's grace budget.
func (e *Engine) waitDrain(max time.Duration) {
	done := make(chan struct{})

	go func() {
		e.serveWG.Wait()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(max):
	}
}

// Wait blocks until the engine has stopped AND every owned session
// goroutine exited. A nil return is a clean stop; a non-nil return is
// a real engine failure the connection manager must observe (the same
// semantics a core process exit has).
func (e *Engine) Wait(ctx context.Context) error {
	select {
	case <-e.stopped:
		e.stopErrMu.Lock()
		defer e.stopErrMu.Unlock()

		return e.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) recordStop(err error) {
	e.stopErrMu.Lock()
	e.stopErr = err
	e.stopErrMu.Unlock()
}

// --- the pipeline -----------------------------------------------------------

// beginSession opens a bounded session (see sessionRegistry).
func (e *Engine) beginSession(ctx context.Context) (*Session, context.Context, func(), error) {
	return e.registry.begin(ctx)
}

// outboundDecision returns the default outbound and the router
// decision for this engine run.
func (e *Engine) outboundDecision() (Outbound, RouterDecision) {
	return e.outbound, e.router.Decide(context.Background(), "")
}

// HandleTUNFlow serves one userspace-IP-stack TCP flow as a first-class
// engine session: the flow's conn behaves exactly like an accepted local
// inbound connection and runs through the SAME Router decision, the SAME
// outbound set and the SAME bounded session registry. The TUN path and
// the System Proxy path are ONE dataplane with ONE authority.
//
// The call BLOCKS until the flow ends — the userspace stack already runs
// each handler in its own goroutine, so the flow's lifetime is owned by
// exactly one goroutine from accept to close (no double-spawn, no
// handler racing its own teardown).
func (e *Engine) HandleTUNFlow(ctx context.Context, conn net.Conn, target string) {
	e.serveWG.Add(1)

	defer e.serveWG.Done()

	defer func() { _ = conn.Close() }()

	e.routeFlow(ctx, conn, "tun", target, nil, nil)
}

// routeFlow is the decision + dial + relay pipeline shared by the
// local inbounds and the TUN flow path (Phase E: routing is ONE
// authority — no protocol implementation hides a routing decision).
// The optional callbacks carry the inbound-specific success/refusal
// replies; the TUN path passes none (a flow that fails just closes).
func (e *Engine) routeFlow(
	ctx context.Context,
	conn net.Conn,
	inbound, target string,
	onSuccess func(upstreamLocal net.Addr) error,
	onRefusal func(error),
) {
	decision := e.router.Decide(ctx, target)

	var outbound Outbound

	switch decision.Action {
	case ActionBlock:
		// Fail closed: the flow ends, nothing escapes to any network.
		// The refuser decides whether the client is told anything; the
		// TUN path has no protocol reply to give, so it just closes.
		if onRefusal != nil {
			onRefusal(errRouteBlocked)
		}

		return
	case ActionDirect:
		outbound = &DirectOutbound{Timeout: DefaultDialTimeout}
	default:
		outbound = e.outbound
	}

	e.pipeOutbound(ctx, conn, inbound, target, outbound, decision, onSuccess, onRefusal)
}

// errRouteBlocked is the internal refusal for BLOCK decisions. It is
// never sent to a client as a tunnel; it exists so refusers can
// distinguish policy blocks from dial failures.
var errRouteBlocked = errors.New("freecore: destination blocked by routing policy")

// pipe runs the generic CONNECT-shaped pipeline for the local inbounds:
// router decision → bounded session → outbound dial → protocol success
// reply → bidirectional relay.
func (e *Engine) pipe(
	ctx context.Context,
	conn net.Conn,
	inbound, target string,
	onSuccess func(upstreamLocal net.Addr) error,
	onRefusal func(error),
) {
	e.routeFlow(ctx, conn, inbound, target, onSuccess, onRefusal)
}

// pipeOutbound runs the pipeline through ONE explicit outbound and
// records ONE explicit router decision on the session.
func (e *Engine) pipeOutbound(
	ctx context.Context,
	conn net.Conn,
	inbound, target string,
	outbound Outbound,
	decision RouterDecision,
	onSuccess func(upstreamLocal net.Addr) error,
	onRefusal func(error),
) {
	session, sctx, end, err := e.registry.begin(ctx)
	if err != nil {
		if onRefusal != nil {
			onRefusal(err)
		}

		return
	}

	defer end()

	session.Inbound = inbound
	session.Target = target
	session.Outbound = decision.Outbound

	upstream, err := outbound.Dial(sctx, target)
	if err != nil {
		if onRefusal != nil {
			onRefusal(err)
		} else if inbound == "tun" {
			// A TUN flow has no protocol reply channel: the honest
			// surface is a credential-free diagnostic record (bounded
			// logging, no payload).
			logging.LogR(logging.Record{
				Level:     logging.LevelWarn,
				Subsystem: Subsystem,
				Event:     "tun_flow_refused",
				Message:   fmt.Sprintf("TUN flow to %s refused: %v", target, err),
				Core:      DisplayName,
				Status:    "refused",
			})
		}

		return
	}

	defer func() { _ = upstream.Close() }()

	if onSuccess != nil {
		if err := onSuccess(upstream.LocalAddr()); err != nil {
			return
		}
	}

	relay(sctx, session, conn, conn, upstream)
}

// relay copies both directions with honest accounting until one side
// finishes, then closes both connections (full-duplex teardown).
// Context cancellation closes both connections, unblocking the
// copies — cancellation is real end-to-end.
func relay(ctx context.Context, session *Session, client net.Conn, clientReader io.Reader, upstream net.Conn) (up, dn int64) {
	upCh := make(chan int64, 1)
	dnCh := make(chan int64, 1)

	go func() {
		var err error

		up, err = io.Copy(upstream, clientReader)
		if err != nil {
			// A failed write half still closes the connection so the
			// peer's copy terminates.
			_ = upstream.Close()
		}

		upCh <- up
	}()

	go func() {
		var err error

		dn, err = io.Copy(client, upstream)
		if err != nil {
			_ = client.Close()
		}

		dnCh <- dn
	}()

	watchDone := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
			_ = upstream.Close()
		case <-watchDone:
		}
	}()

	// First finished direction tears both sides down (the classic
	// proxy teardown: an EOF in either direction ends the tunnel).
	select {
	case up = <-upCh:
		_ = client.Close()
		_ = upstream.Close()

		dn = <-dnCh
	case dn = <-dnCh:
		_ = client.Close()
		_ = upstream.Close()

		up = <-upCh
	}

	close(watchDone)

	session.Account(up, dn)

	return up, dn
}

// prefixConn re-injects the byte consumed by mixed-port protocol
// sniffing so the dispatched handler sees the untouched stream.
type prefixConn struct {
	net.Conn
	prefix  byte
	pending bool
}

// Read implements net.Conn.
func (p *prefixConn) Read(b []byte) (int, error) {
	if p.pending {
		if len(b) == 0 {
			return 0, nil
		}

		p.pending = false

		b[0] = p.prefix

		if len(b) > 1 {
			n, err := p.Conn.Read(b[1:])

			return n + 1, err
		}

		return 1, nil
	}

	return p.Conn.Read(b)
}

// shutdownObserved reports whether the engine has begun draining
// (used by tests and diagnostics; honest lifecycle visibility).
func (e *Engine) shutdownObserved() bool {
	select {
	case <-e.stopped:
		return true
	default:
		return false
	}
}

// stopping reports whether the engine has begun shutting down. New
// sessions are refused once this is true.
func (e *Engine) stopping() bool { return e.registry.isDraining() }

// draining exposes the registry's drain gate for tests.
func (e *Engine) draining() bool { return e.registry.isDraining() }

// ensure the atomic import stays used if the build trims helpers.
var _ = atomic.Int64{}
