// tun_routes.go implements the v0.11.4 TUN routing-path observation
// model: the platform-neutral FACTS and VERDICTS the activation gate
// reasons over, plus the OS collection seams the Windows IP Helper
// layer fills in (tun_routes_windows.go).
//
// WHY (docs/tun.md "Activation evidence"): v0.11.3 proved activation
// with (a) an address-or-name interface match and (b) one successful
// no-explicit-proxy HTTPS request. Both are necessary but weaker than
// the contract requires: the interface match could accept the session
// address sitting on the WRONG adapter, and the probe could succeed
// while system traffic still bypassed the TUN through a stale route.
// v0.11.4 adds the missing evidence classes:
//
//   - route ownership: the session adapter must OWN the covering
//     IPv4 route state (0.0.0.0/0, or the 0.0.0.0/1 + 128.0.0.0/1
//     covering pair) before traffic is believed to traverse it;
//   - upstream pinning: after the tunneled request succeeds, the
//     sing-box process must own NO TCP socket sourced from the TUN
//     address (a socket sourced from the TUN is the route-loop
//     failure mode route.auto_detect_interface exists to prevent).
//
// The observation is READ-ONLY (query APIs only), BOUNDED (single
// snapshot per gate, no polling loops), DETERMINISTIC (pure verdict
// functions over fact slices) and TESTABLE (the collectors are seam
// variables; the verdicts are pure functions tested on every
// platform). No shell command is involved anywhere — netsh/route.exe
// parsing never returned.
package tunnel

import (
	"errors"
	"fmt"
)

// ErrTUNRouteObservationUnavailable is returned by the OS collection
// seams on platforms without the native observation layer (or when
// the native API fails): the activation gate FAILS CLOSED instead of
// publishing Active on unobserved routing state.
var ErrTUNRouteObservationUnavailable = errors.New("tunnel: native TUN route observation is unavailable on this platform")

// observedRoute is one IPv4 forwarding-table row normalized from the
// OS shape (Windows MIB_IPFORWARDROW; all fields host byte order).
type observedRoute struct {
	// Dest is the destination network (e.g. 0.0.0.0 for the default
	// route, 128.0.0.0 for the upper /1 half).
	Dest [4]byte
	// Mask is the network mask (0.0.0.0 for /0, 128.0.0.0 for /1).
	Mask [4]byte
	// IfIndex is the interface index owning the route.
	IfIndex uint32
	// Metric is the route metric (diagnostics only; the verdict
	// matches ownership, not best-route ranking).
	Metric uint32
}

// observedOwnerSocket is one TCP socket row normalized from the OS
// owner table (Windows MIB_TCPROW_OWNER_PID; addresses in host byte
// order).
type observedOwnerSocket struct {
	// PID is the owning process.
	PID uint32
	// LocalAddr is the socket's local IPv4 address (0.0.0.0 when the
	// socket is bound to the wildcard).
	LocalAddr [4]byte
	// LocalPort is the socket's local port (host byte order).
	LocalPort uint16
}

// tunRouteVerdict is the covering-route decision over an observed
// forwarding table.
type tunRouteVerdict struct {
	// Covered reports whether the TUN interface owns route state
	// covering the full IPv4 space (0.0.0.0/0, or the 0.0.0.0/1 +
	// 128.0.0.0/1 pair — the two shapes auto_route implementations
	// use).
	Covered bool
	// Detail is the bounded, privacy-safe explanation (which covering
	// shape matched, or why nothing matched). Never contains
	// addresses of the user's machines beyond the TUN session itself.
	Detail string
}

// routeCovers reports whether the (dest, mask) pair is exactly the
// default route 0.0.0.0/0.
func (r observedRoute) isDefault() bool {
	return r.Dest == [4]byte{} && r.Mask == [4]byte{}
}

// isLowerHalf reports whether the row is exactly 0.0.0.0/1 (the
// lower covering half).
func (r observedRoute) isLowerHalf() bool {
	return r.Dest == [4]byte{} && r.Mask == [4]byte{128, 0, 0, 0}
}

// isUpperHalf reports whether the row is exactly 128.0.0.0/1 (the
// upper covering half).
func (r observedRoute) isUpperHalf() bool {
	return r.Dest == [4]byte{128, 0, 0, 0} && r.Mask == [4]byte{128, 0, 0, 0}
}

// tunRouteCoveringVerdict decides whether the TUN adapter owns
// covering IPv4 route state. Pure function over the observed rows:
// the same table always yields the same verdict (deterministic,
// testable on every platform).
func tunRouteCoveringVerdict(routes []observedRoute, tunIfIndex uint32) tunRouteVerdict {
	if tunIfIndex == 0 {
		return tunRouteVerdict{Covered: false, Detail: "no TUN interface index to match routes against"}
	}

	lower, upper := false, false

	for _, r := range routes {
		if r.IfIndex != tunIfIndex {
			continue
		}

		if r.isDefault() {
			return tunRouteVerdict{
				Covered: true,
				Detail:  "default route 0.0.0.0/0 owned by the TUN interface",
			}
		}

		if r.isLowerHalf() {
			lower = true
		}

		if r.isUpperHalf() {
			upper = true
		}
	}

	if lower && upper {
		return tunRouteVerdict{
			Covered: true,
			Detail:  "covering route pair 0.0.0.0/1 + 128.0.0.0/1 owned by the TUN interface",
		}
	}

	switch {
	case lower && !upper:
		return tunRouteVerdict{Covered: false, Detail: "only the 0.0.0.0/1 half is on the TUN interface (128.0.0.0/1 is not)"}
	case upper && !lower:
		return tunRouteVerdict{Covered: false, Detail: "only the 128.0.0.0/1 half is on the TUN interface (0.0.0.0/1 is not)"}
	}

	return tunRouteVerdict{Covered: false, Detail: "no covering IPv4 route (0.0.0.0/0 or the /1 pair) is owned by the TUN interface"}
}

// tunPinningVerdict is the upstream-socket decision over an observed
// owner table.
type tunPinningVerdict struct {
	// Loop reports a sing-box TCP socket SOURCED from the TUN address:
	// the route-loop failure mode (upstream traffic re-entering the
	// TUN). A hard activation failure whenever true.
	Loop bool
	// Pinned reports positive evidence that at least one sing-box
	// upstream TCP socket is sourced from a non-TUN local address
	// (the physical side) — the observable form of
	// route.auto_detect_interface's guarantee for TCP-based
	// outbounds.
	Pinned bool
	// Detail is the bounded, privacy-safe explanation.
	Detail string
}

// tunUpstreamPinningVerdict decides the upstream-pinning facts for
// the sing-box process after the tunneled request succeeded. Pure
// function over the observed socket rows.
//
// UDP-family outbounds (WireGuard, Hysteria2, TUIC, QUIC transports)
// hold no TCP upstream socket, so Pinned can be false WITHOUT Loop —
// the honest "not observable through the TCP owner table" outcome
// (documented in docs/tun.md). Loop, however, is always decisive
// when present: a sing-box TCP socket sourced from the TUN address
// means the upstream dialed INTO the tunnel.
func tunUpstreamPinningVerdict(sockets []observedOwnerSocket, corePID uint32, tunAddr [4]byte) tunPinningVerdict {
	verdict := tunPinningVerdict{
		Detail: "no TCP upstream socket owned by the sing-box process is present in the owner table (typical for UDP-family outbounds; pinning is not observable through the TCP table)",
	}

	if corePID == 0 {
		verdict.Detail = "no sing-box PID to match upstream sockets against"

		return verdict
	}

	for _, s := range sockets {
		if s.PID != corePID {
			continue
		}

		if s.LocalAddr == tunAddr {
			verdict.Loop = true
			verdict.Detail = fmt.Sprintf("the sing-box process owns a TCP socket sourced from the TUN address (local port %d): upstream traffic is re-entering the tunnel", s.LocalPort)

			return verdict
		}
	}

	for _, s := range sockets {
		if s.PID != corePID {
			continue
		}

		// A real, bound, non-TUN local address on the physical side
		// (wildcard 0.0.0.0 and loopback are not sourcing evidence).
		if s.LocalAddr != [4]byte{} && s.LocalAddr != [4]byte{127, 0, 0, 1} && s.LocalAddr != tunAddr {
			verdict.Pinned = true
			verdict.Detail = fmt.Sprintf("the sing-box process owns a TCP upstream socket sourced from a non-TUN local address (local port %d): the upstream is pinned to the physical side", s.LocalPort)

			return verdict
		}
	}

	return verdict
}

// --- OS collection seams ---------------------------------------------------
//
// The Windows layer (tun_routes_windows.go) installs the real
// collectors at init; every other platform installs the error stub
// (tun_routes_default.go) so the activation gate fails closed with
// ErrTUNRouteObservationUnavailable instead of guessing. Exactly one
// of the two build-tagged files is compiled per platform. Tests
// replace these variables with deterministic fakes and restore them
// via t.Cleanup.

var (
	// collectForwardRoutes snapshots the IPv4 forwarding table
	// (read-only, single call, bounded).
	collectForwardRoutes func() ([]observedRoute, error)

	// collectOwnerTCPSockets snapshots the TCP owner table
	// (read-only, single call, bounded).
	collectOwnerTCPSockets func() ([]observedOwnerSocket, error)
)
