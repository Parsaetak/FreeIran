package freecore

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Endpoint is the engine's address model: where a connection goes,
// independent of which protocol carries it.
type Endpoint struct {
	Host string // hostname or IP literal (never carries credentials)
	Port int    // 1..65535
}

// String renders host:port for logs and diagnostics (credential-free
// by construction — credentials never enter this type).
func (e Endpoint) String() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// Network identifies the transport family of one hop. Phase 1
// implements TCP only; the type exists so Phase 2 additions (QUIC,
// WebSocket, ...) are new values, not new shapes.
type Network string

// NetworkTCP is the only network the v0.13.1 engine carries.
const NetworkTCP Network = "tcp"

// TransportSecurity describes the security layer of one hop. Phase 1
// implements None; TLS/REALITY are Phase 2 values behind this same
// boundary (ROADMAP.md item 12).
type TransportSecurity string

// SecurityNone is the only security layer the v0.13.1 engine carries.
const SecurityNone TransportSecurity = "none"

// Dialer is the one first-party dial seam: every outbound, direct or
// remote, connects through it. It matches net.Dialer's DialContext
// shape so standard-library and HTTP-stack consumers compose.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// RouterDecision records WHY traffic took a path. v0.13.1 routing is
// trivial by design (everything flows through the session's remote
// outbound); the decision object exists so the Phase 2 routing
// authority (ROADMAP.md item 3) has one place to grow without
// changing the session pipeline.
type RouterDecision struct {
	// Outbound is the outbound the traffic was sent through.
	Outbound string

	// Reason is the human-readable, credential-free explanation.
	Reason string
}

// Resolver is the DNS authority seam. v0.13.1 resolves through the
// system resolver — exactly what the external cores do by default
// for their own upstream dials — behind this boundary; the Phase 2
// central DNS authority (ROADMAP.md item 2) replaces the
// implementation, not the callers.
type Resolver interface {
	// Resolve returns the addresses for host. Implementations must
	// honor ctx cancellation and must never invent addresses.
	Resolve(ctx context.Context, host string) ([]netip.Addr, error)
}

// systemResolver resolves through the Go default resolver.
type systemResolver struct{}

// Resolve implements Resolver via net.DefaultResolver.
func (systemResolver) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}

	return addrs, nil
}

// SystemResolver returns the default system-resolver-backed Resolver.
func SystemResolver() Resolver { return systemResolver{} }

// Bounded timing defaults for engine hops. These are honest bounds,
// not fake timings: they cap how long one phase of a first-party
// session may hang before the session fails with a real error.
const (
	// DefaultDialTimeout bounds one outbound TCP dial (matches the
	// engine-wide core startup conventions).
	DefaultDialTimeout = 15 * time.Second

	// DefaultHandshakeTimeout bounds one inbound protocol handshake
	// (SOCKS5 greeting/request, HTTP request head) — a client that
	// connects and stalls cannot pin an engine goroutine forever.
	DefaultHandshakeTimeout = 30 * time.Second

	// DefaultHeaderLimit bounds the request head an HTTP inbound will
	// parse before refusing with 431 semantics.
	DefaultHeaderLimit = 16 * 1024
)
