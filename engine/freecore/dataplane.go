package freecore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Parsaetak/FreeIran/engine/freecore/netstack"
	"github.com/Parsaetak/FreeIran/engine/freecore/tun"
)

// TUN dataplane defaults. The local network is a documented
// point-to-point shape (198.18.0.0/15 benchmark space — never a real
// LAN), matching the collision-free adapter addressing the activation
// transaction negotiates.
const (
	DefaultTUNAddr4   = "198.18.0.1"
	DefaultTUNPrefix4 = "198.18.0.0/30"

	DefaultTUNAddr6   = "fdfe:dcba:9876::1"
	DefaultTUNPrefix6 = "fdfe:dcba:9876::/126"
)

// TUNDataplaneOptions configure one first-party TUN dataplane.
type TUNDataplaneOptions struct {
	// Device is the opened FreeIran TUN device (Wintun on Windows, or
	// an in-memory device in tests).
	Device tun.Device

	// Addr4/Prefix4 override the local interface addressing (defaults
	// above). The activation transaction passes what it configured on
	// the OS interface so the userspace stack and the OS never
	// disagree.
	Addr4   netip.Addr
	Prefix4 netip.Prefix
	Addr6   netip.Addr
	Prefix6 netip.Prefix

	// MaxFlows overrides the userspace stack's concurrent-flow bound
	// (0 = netstack default).
	MaxFlows int

	// UpstreamDialer replaces the engine's default upstream dialer
	// (tests). Production passes the loop-prevention constrained
	// dialer — see NewUpstreamDialer.
	UpstreamDialer Dialer
}

// TUNDataplane is the first-party TUN path: Wintun packets enter the
// FreeIran-owned userspace IP stack, accepted TCP flows become regular
// engine sessions (same Router, same outbound set, same bounded
// registry), and egress packets return through the device. There is no
// second dataplane, no second session table, no hidden direct path.
type TUNDataplane struct {
	engine *Engine
	stack  *netstack.Stack
}

// NewTUNDataplane builds the dataplane over an OPEN device. Binding
// the userspace stack to the device's negotiated MTU is enforced by
// the netstack constructor (a disagreement is a constructor error,
// fail-closed).
func NewTUNDataplane(engine *Engine, opts TUNDataplaneOptions) (*TUNDataplane, error) {
	if engine == nil {
		return nil, fmt.Errorf("freecore: tun dataplane requires an engine")
	}

	if opts.Device == nil {
		return nil, fmt.Errorf("freecore: tun dataplane requires an open device")
	}

	if !opts.Addr4.IsValid() {
		opts.Addr4 = netip.MustParseAddr(DefaultTUNAddr4)
		opts.Prefix4 = netip.MustParsePrefix(DefaultTUNPrefix4)
	}

	cfg := netstack.Config{
		Addr4:    opts.Addr4,
		Prefix4:  opts.Prefix4,
		MaxFlows: opts.MaxFlows,
	}

	if opts.Addr6.IsValid() {
		cfg.Addr6 = opts.Addr6
		cfg.Prefix6 = opts.Prefix6
	}

	stack, err := netstack.New(opts.Device, cfg, engineTUNFlowHandler(engine), nil)
	if err != nil {
		return nil, fmt.Errorf("freecore: tun dataplane: %w", err)
	}

	return &TUNDataplane{engine: engine, stack: stack}, nil
}

// engineTUNFlowHandler adapts userspace-stack flows to engine sessions. UDP
// is deliberately NOT handled yet: without a genuine first-party UDP
// relay there is no honest path for it — the stack classifies and
// counts those datagrams as unsupported (fail-closed), nothing leaks
// to the physical interface.
func engineTUNFlowHandler(engine *Engine) netstack.TCPHandler {
	return func(ctx context.Context, flow netstack.TCPFlow) {
		engine.HandleTUNFlow(ctx, flow.Conn, flow.Dst.String())
	}
}

// Run serves the dataplane until ctx is cancelled or the device
// terminates. It blocks; the nil return is a clean stop.
func (d *TUNDataplane) Run(ctx context.Context) error {
	return d.stack.Run(ctx)
}

// Close stops the dataplane (idempotent, safe concurrent with Run).
func (d *TUNDataplane) Close() error {
	return d.stack.Close()
}

// PacketStats returns the honest packet counters of the userspace
// stack (diagnostics surface; no synthetic numbers).
func (d *TUNDataplane) PacketStats() netstack.Stats {
	return d.stack.Stats()
}

// SessionCount returns the engine's live session count (TUN flows are
// ordinary engine sessions).
func (d *TUNDataplane) SessionCount() int {
	return d.engine.registry.Len()
}

// --- loop prevention (Phase D) ----------------------------------------------

// upstreamBinding is the platform seam the constrained dialer uses.
// It returns a dial function whose sockets are bound to the physical
// interface (nil = default routing, correct when no TUN owns the
// default route). The seam receives the CALLER'S CONTEXT and must
// honor it — a binding that drops ctx cannot be cancelled (the v0.14.0
// seam used net.Dialer.Dial and lost cancellation; v0.14.1 fixed the
// contract). Windows implements it with IP_UNICAST_IF /
// IPV6_UNICAST_IF over the observed physical interface index; other
// platforms return nil and the first-party TUN path is refused anyway.
var upstreamBinding = defaultUpstreamBinding

// NewUpstreamDialer builds the loop-prevention dialer for upstream
// engine connections while the first-party TUN owns traffic:
//
//   - constraint nil (no TUN) → the plain system dialer;
//   - constraint present → sockets bound to the constraint's PHYSICAL
//     interface, after Validate() refuses the fail-closed shapes
//     (missing physical interface; physical == TUN). A refused shape
//     fails the DIAL, not the loop guarantee: an unbound or TUN-bound
//     upstream is a loop risk, and a loop is worse than a refused
//     connection.
func NewUpstreamDialer(constraint *tun.UpstreamConstraint) Dialer {
	if constraint == nil {
		return plainSystemDialer{}
	}

	if err := constraint.Validate(); err != nil {
		return refusedDialer{cause: err}
	}

	binding := upstreamBinding(int(constraint.PhysicalInterfaceIndex))
	if binding == nil {
		// The platform cannot honor the constraint: fail the DIAL, not
		// the loop guarantee.
		return refusedDialer{cause: errNoPlatformBinding}
	}

	return &boundDialer{binding: binding, timeout: DefaultDialTimeout}
}

// plainSystemDialer is the default routing dialer (no TUN owns the
// default route, so no constraint applies).
type plainSystemDialer struct{}

// DialContext implements Dialer.
func (plainSystemDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: DefaultDialTimeout, KeepAlive: 30 * time.Second}

	return dialer.DialContext(ctx, network, address)
}

// Fail-closed dial causes (surfaced honestly, never swallowed).
var errNoPlatformBinding = errors.New("freecore: platform cannot honor the upstream binding constraint")

// refusedDialer refuses every dial (loop-prevention fail-closed).
type refusedDialer struct {
	cause error
}

// DialContext implements Dialer.
func (d refusedDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	return nil, fmt.Errorf("freecore: upstream dial %s refused: %w", address, d.cause)
}

// boundDialer dials through the platform binding hook. The caller's
// context flows INTO the binding: cancellation must reach the socket
// connect, not stop at the seam.
type boundDialer struct {
	binding func(ctx context.Context, network, address string) (net.Conn, error)
	timeout time.Duration
}

// DialContext implements Dialer.
func (d *boundDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.binding != nil {
		return d.binding(ctx, network, address)
	}

	dialer := &net.Dialer{Timeout: d.timeout, KeepAlive: 30 * time.Second}

	return dialer.DialContext(ctx, network, address)
}

// defaultUpstreamBinding is the platform-neutral fallback: it cannot
// guarantee the exclusion, so it reports inability (the caller fails
// closed). The Windows build provides the real binding.
func defaultUpstreamBinding(_ int) func(ctx context.Context, network, address string) (net.Conn, error) {
	return nil
}

// dataplaneOnce guards process-wide dataplane registration (no second
// dataplane authority may exist).
var dataplaneOnce sync.Once
