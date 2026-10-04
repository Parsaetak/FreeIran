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

// RouteAction is what the Router decided for one destination.
type RouteAction string

const (
	// ActionProxy sends the flow through the session's configured remote
	// outbound (the first-party SOCKS5/HTTP/Shadowsocks client).
	ActionProxy RouteAction = "proxy"

	// ActionDirect dials the destination without a remote. Only granted
	// when policy allows it (explicit local/private destinations).
	ActionDirect RouteAction = "direct"

	// ActionBlock refuses the destination: the flow fails closed, no
	// network sees it.
	ActionBlock RouteAction = "block"
)

// RouterDecision records WHY traffic took a path. Phase 2 turns the
// placeholder into a real decision: action, outbound, resolver choice,
// reason and the policy/rule identifier — produced by the ONE Router
// every path shares (local inbounds, TUN flows), never inside a
// protocol implementation.
type RouterDecision struct {
	// Action is what happens to the flow (proxy/direct/block).
	Action RouteAction

	// Outbound is the outbound the traffic was sent through.
	Outbound string

	// ResolverChoice names the DNS strategy for the destination:
	// "remote" (domain passed to the remote proxy — the default for
	// proxied flows), "engine" (resolved by the engine's bounded cache
	// through the constrained dialer), "none" (IP literal or block).
	ResolverChoice string

	// Reason is the human-readable, credential-free explanation.
	Reason string

	// RuleID identifies the matching rule/policy ("default-policy",
	// "private-direct", "block-list", ...).
	RuleID string
}

// Router is the routing authority seam: ONE deterministic decision
// function every traffic path calls. Implementations must be safe for
// concurrent use, side-effect free and credential-free.
type Router interface {
	// Decide returns the decision for target (host:port, host may be a
	// domain, an IPv4/IPv6 literal, or "" for diagnostics probes).
	Decide(ctx context.Context, target string) RouterDecision
}

// Resolver is the DNS authority seam. Phase 2 builds the central
// authority around this boundary: a bounded caching resolver with
// explicit bootstrap behavior sits BEHIND the same interface — the
// callers never change again.
type Resolver interface {
	// Resolve returns the addresses for host. Implementations must
	// honor ctx cancellation and must never invent addresses.
	Resolve(ctx context.Context, host string) ([]netip.Addr, error)
}

// parseShadowsocksMethod validates a Shadowsocks AEAD method name for
// the Route model (the protocol package owns the truth; this seam keeps
// protocol code out of the normalization path).
func parseShadowsocksMethod(method string) (string, error) {
	return NormalizeShadowsocksMethod(method)
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
