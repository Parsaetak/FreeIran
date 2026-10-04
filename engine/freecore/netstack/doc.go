// Package netstack is the FreeIran-owned userspace IP stack adapter
// (ROADMAP v0.14.0 Phase 2): it terminates TCP and UDP flows that arrive
// as raw IP packets on a freecore.tun.Device and hands them to FreeIran
// handlers as plain net.Conn / net.PacketConn.
//
// WHAT THIS IS: the isolation wall in front of gVisor netstack. Every
// gVisor type (tcpip.Address, stack.PacketBuffer, channel.Endpoint,
// waiter.Queue, …) is constructed, driven and destroyed strictly inside
// this package — the public API uses only net, netip, context and
// freecore.tun types. A future swap of the IP-stack engine (or an
// upstream gVisor API break) is therefore a private affair of this
// package, never a refactor of the dataplane that sits on top.
//
// WHAT THIS IS NOT: a router, a DNS resolver or a policy engine. The
// stack accepts every flow that survives its bounded resources and
// passes it to the handler; what the handler does with a flow (relay
// through an outbound, refuse, log) is decided above this package —
// handler-side destination validation is deliberately NOT this
// package's job.
//
// HONESTY AND BOUNDS (the invariants the implementation defends):
//
//   - Every inbound packet is classified and counted: injected,
//     Malformed (unparseable/oversize) or Unsupported (parsed but no
//     handler exists, e.g. ICMP or UDP without a UDP handler). No path
//     silently discards a packet without a counter.
//   - Every bound is real: the link queue depth (QueueSize), the flow
//     registry (MaxFlows — beyond the cap a SYN is answered with RST,
//     a UDP datagram is counted and dropped) and the per-flow
//     goroutines. No unbounded map, no unbounded goroutine spawn.
//   - Shutdown is honest and idempotent: Close cancels the internal
//     context, closes every open flow connection, closes the link
//     endpoint and joins its goroutines with a bounded wait.
//
// EVIDENCE CLASS: unit tests (config validation, family classification,
// counters, close idempotency) plus an in-memory integration test that
// drives real bytes end-to-end: a second gVisor stack plays the
// application behind a MemDevice bridge, the stack under test relays
// the accepted TCP flow to a real loopback echo server. Linux only —
// no OS TUN device is opened anywhere in this package's tests.
package netstack

// Subsystem identifies the userspace IP stack in structured errors.
const Subsystem = "freecore.netstack"
