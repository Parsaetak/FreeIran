// Package tun is the first-party TUN device/control-plane foundation
// (v0.13.1 Phase 1; ROADMAP.md Phase 2 item 1).
//
// WHAT THIS IS: the FreeIran-owned boundary the Phase 2 TUN packet
// dataplane will be built on — the device abstraction with a bounded
// packet channel, the deterministic FreeIran-owned adapter identity,
// the transaction/rollback state representation, the loop-prevention
// metadata the future upstream dialer must honor, the Windows Wintun
// adapter/session lifecycle (through the reviewed MIT-licensed
// golang.zx2c4.com/wintun binding) and a read-only Windows IP Helper
// observation seam.
//
// WHAT THIS IS NOT: a dataplane. Nothing here forwards packets,
// terminates TCP, answers DNS or routes traffic. The ONLY runnable
// TUN dataplane in v0.13.1 remains the managed sing-box native TUN
// backend (engine/tunnel), selected explicitly through the ONE
// TUNBackend authority — this package is deliberately NOT wired into
// any UI toggle, tunnel state or capability claim. When the Phase 2
// dataplane lands it enters through this boundary inside the ONE
// engine, never as a parallel TUN system.
//
// EVIDENCE CLASS: platform-neutral ownership/identity/rollback/
// loop-guard decisions are unit-tested on Linux; the Windows layers
// (Wintun binding, IP Helper collectors) compile in the windows/amd64
// cross-build. NO Windows runtime execution happened for this
// foundation in this release — the same honesty rule docs/tun.md's
// evidence ladder enforces everywhere else.
package tun

// Subsystem identifies the first-party TUN foundation in structured
// errors.
const Subsystem = "freecore.tun"
