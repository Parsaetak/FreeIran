package freecore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
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
}

// Engine is one first-party engine run: bound local inbounds, a
// bounded session registry and the outbound pipeline for one
// normalized route. It runs entirely in-process — no child process,
// no supervisor, no second connection authority.
type Engine struct {
	opts     Options
	route    Route
	outbound Outbound
	decision RouterDecision
	resolver Resolver

	registry *sessionRegistry

	handshakeTimeout time.Duration

	mixed net.Listener // SOCKS5 + HTTP CONNECT on one port
	http  net.Listener // dedicated HTTP inbound when configured

	ctx      context.Context
	cancel   context.CancelFunc
	acceptWG sync.WaitGroup

	stopOnce  sync.Once
	stopped   chan struct{}
	stopErr   error
	stopErrMu sync.Mutex
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

	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = DefaultHandshakeTimeout
	}

	engine := &Engine{
		opts:     opts,
		route:    opts.Route,
		outbound: outboundFor(opts.Route),
		decision: RouterDecision{
			Outbound: string(opts.Route.Outbound),
			Reason: fmt.Sprintf("v0.13.1 first-party path: all traffic through the configured %s remote %s",
				opts.Route.Outbound, opts.Route.Endpoint),
		},
		resolver: resolver,
		registry: newSessionRegistry(opts.MaxSessions),
		stopped:  make(chan struct{}),
	}

	engine.handshakeTimeout = handshakeTimeout

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

	e.mixed = mixed

	if e.opts.HTTPPort > 0 {
		httpListener, err := net.Listen("tcp", net.JoinHostPort(e.opts.LocalHost, strconv.Itoa(e.opts.HTTPPort)))
		if err != nil {
			_ = mixed.Close()

			return fmt.Errorf("freecore: bind http inbound %s:%d: %w",
				e.opts.LocalHost, e.opts.HTTPPort, err)
		}

		e.http = httpListener
	}

	e.ctx, e.cancel = context.WithCancel(ctx)

	e.acceptWG.Add(1)

	go e.acceptLoop(e.mixed, true)

	if e.http != nil {
		e.acceptWG.Add(1)

		go e.acceptLoop(e.http, false)
	}

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

	go func() {
		e.acceptWG.Wait()

		e.stopOnce.Do(func() { close(e.stopped) })
	}()

	return nil
}

// acceptLoop serves one listener until it closes.
func (e *Engine) acceptLoop(listener net.Listener, mixed bool) {
	defer e.acceptWG.Done()

	for {
		conn, err := listener.Accept()
		if err != nil {
			// A listener that failed for any reason other than
			// shutdown is an engine-level failure: stop honestly so
			// the connection manager observes it (never a silent
			// dead inbound).
			if !errors.Is(err, net.ErrClosed) {
				e.recordStop(fmt.Errorf("freecore: inbound accept: %w", err))
				e.Stop(0)
			}

			return
		}

		if mixed {
			go e.serveMixed(e.ctx, conn)
		} else {
			go (&HTTPConnectInbound{Engine: e}).Serve(e.ctx, conn)
		}
	}
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

// Stop shuts the engine down (idempotent, bounded by grace for the
// accept loops; sessions are cancelled, which closes their pipes).
func (e *Engine) Stop(grace time.Duration) {
	_ = grace // sessions close through context cancellation; nothing to wait for beyond accept loops

	if e.cancel != nil {
		e.cancel()
	}

	if e.mixed != nil {
		_ = e.mixed.Close()
	}

	if e.http != nil {
		_ = e.http.Close()
	}
}

// Wait blocks until the engine has stopped. A nil return is a clean
// stop; a non-nil return is a real engine failure the connection
// manager must observe (the same semantics a core process exit has).
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

// outboundDecision returns the outbound and the router decision for
// this engine run.
func (e *Engine) outboundDecision() (Outbound, RouterDecision) {
	return e.outbound, e.decision
}

// pipe runs the generic CONNECT-shaped pipeline: bounded session →
// outbound dial → protocol success reply → bidirectional relay. The
// inbound-specific replies are supplied by the callers so the engine
// core stays protocol-neutral.
func (e *Engine) pipe(
	ctx context.Context,
	conn net.Conn,
	inbound, target string,
	onSuccess func(upstreamLocal net.Addr) error,
	onRefusal func(error),
) {
	session, sctx, end, err := e.registry.begin(ctx)
	if err != nil {
		onRefusal(err)

		return
	}

	defer end()

	session.Inbound = inbound
	session.Target = target
	session.Outbound = e.decision.Outbound

	upstream, err := e.outbound.Dial(sctx, target)
	if err != nil {
		onRefusal(err)

		return
	}

	defer func() { _ = upstream.Close() }()

	if err := onSuccess(upstream.LocalAddr()); err != nil {
		return
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
