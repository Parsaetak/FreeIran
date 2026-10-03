// Package freecore is the first-party FreeIran connectivity engine
// (v0.13.1 Phase 1 foundation).
//
// The engine is FreeIran-owned Go code that carries real bytes for
// the protocol subset it genuinely implements — in v0.13.1 that is a
// local proxy pipeline sufficient for System Proxy:
//
//	local inbound (SOCKS5 + HTTP CONNECT, mixed on one port)
//	  → bounded session lifecycle
//	  → first-party dialer
//	  → remote outbound (SOCKS5 with optional RFC 1929 auth,
//	     HTTP CONNECT with optional basic auth, or direct)
//
// ARCHITECTURAL CONTRACT (docs/architecture.md v0.13.1 addendum,
// ROADMAP.md Phase 2):
//
//   - The engine is implemented as a backend inside the ONE core
//     execution boundary (engine/core): the same registry, the same
//     deterministic selection, the same connection state machine,
//     the same verification gate. It is NOT a parallel connection
//     manager and never spawns child processes — first-party
//     sessions run in-process.
//   - Normalization happens ONCE (Route.Normalize from the universal
//     config.Config); protocol implementations consume the internal
//     model, never the application configuration.
//   - The capability gate is honest: only SOCKS/HTTP remotes over
//     plain TCP with no security layer enter the first-party path.
//     Everything else is refused and continues through the external
//     compatibility cores (Xray/V2Ray/sing-box).
//   - Interfaces exist only at real subsystem boundaries (inbound,
//     outbound/dialer, resolver, transport) so Phase 2 can grow the
//     engine without re-abstracting it.
//   - Bounded by construction: session count cap, bounded header
//     reads, context cancellation end-to-end, no unbounded
//     per-request state, no retry storms, no fake capability flags.
//
// The engine does NOT implement TUN packet forwarding; the Phase 2
// device/control foundation lives in the freecore/tun subpackage.
package freecore

// Subsystem identifies the first-party engine in structured errors.
const Subsystem = "freecore"

// DisplayName is the honest, user-facing backend label surfaced by
// the UI and diagnostics when a session runs on the first-party
// engine (external backends surface their own names).
const DisplayName = "FreeIran Engine"
