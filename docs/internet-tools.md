# Internet Tools (v0.9.8.1)

This document describes the shared Internet-Tools engine (§5 of the
upgrade specification): ONE bounded, cancellable, structured tool
architecture for every user-triggered network diagnostic. The engine
lives in `engine/netcheck` (tools.go, tools_impl.go, tools_path.go,
toolsafety.go) and extends the existing connectivity-diagnostics
package instead of spawning unrelated services. The app service
(`engine/app/internettools.go`) exposes it to the UI.

## The result contract

Every tool execution — regardless of tool — produces one
`ToolResult`:

```text
tool_id / target / started_at / finished_at / duration_ms /
status / transport / path (direct|tunneled) / provider /
measurement / error / details
```

- **Timeouts** are clamped to a bounded window: minimum 1 s, per-tool
  default (10 s standard; 20 s internet; 30 s traceroute; 15 s
  path-mtu/public-ip), hard cap 60 s. Cancellation is honoured at
  every stage.
- **Statuses**: `ok`, `failed`, `timeout`, `cancelled`,
  `invalid_target` (the safety layer rejected the target before any
  bytes left the machine), `unsupported` (honest capability report —
  see below).
- **Measurements** (`ToolMeasurement`) carry the v0.9.8.1 canonical
  latency semantics (`engine/tester/latency.go`): `Measured` is the
  authority, 0 ms + `Measured` = sub-millisecond, `SubMS` marks
  "< 1 ms" round trips. All values are credential-free.

## Tool catalogue

| Tool | Group | Transport | Default target | Notes |
|------|-------|-----------|----------------|-------|
| `internet` | connectivity | aggregate | builtin probe set | the classic connectivity report (bounded sub-probes) |
| `dns` | connectivity | UDP/TCP 53 | system resolver | resolve a name; answers reported as IP literals with a count |
| `tcp` | connectivity | TCP | `1.1.1.1:443` | raw reachability + RTT |
| `tls` | connectivity | TLS | `www.gstatic.com:443` | TCP + TLS handshake; negotiated version/cipher reported |
| `https` | connectivity | HTTP(S) | `https://www.gstatic.com/generate_204` | full GET with redirect/size caps |
| `http_connect` | protocol | HTTP proxy CONNECT | `http://127.0.0.1:1080` | local proxy reachability |
| `socks5` | protocol | SOCKS5 | `127.0.0.1:1080` | RFC 1928 CONNECT round trip |
| `websocket` | protocol | WS/WSS | `wss://echo.websocket.events` | upgrade handshake |
| `udp` | protocol | UDP | `1.1.1.1:53` | datagram round trip (DNS query payload) |
| `quic` | protocol | QUIC | — | **honestly unsupported**: no QUIC stack is compiled into this build; reports `unsupported` and points at the UDP tool. Nothing is claimed that was not verified. |
| `traceroute` | path | raw ICMP TTL walk | `1.1.1.1` | **privilege-gated**: raw ICMP sockets require administrator privileges; reports the honest "requires administrator privileges" error (`unsupported`) instead of pretending when the socket cannot be created. |
| `path_mtu` | path | DNS payload ladder | `1.1.1.1:53` | bounded unfragmented-payload ladder, honestly labelled: DNS QNAME encoding caps the ladder at ~300 bytes — verifying the full 1500-byte MTU would require privileged raw-socket probing. |
| `captive_portal` | path | HTTP | builtin portal probe set | portal detection with redirect reporting |
| `public_ip` | identity | HTTPS | builtin identity endpoints | exit IP direct vs through the active tunnel, with match verdict |
| `tunnel_diagnostics` | tunnel | measured | active tunnel | live truth about the active provider/core session (endpoints, health, measured latency) |

The catalogue with labels, groups, target-taking flags and default
timeouts is served to the UI through `ToolCatalogue()`; the Network
page renders it as a grouped grid (`frontend/src/pages/Network.tsx`).

## Safety policy (`engine/netcheck/toolsafety.go`, §7)

Every tool obeys the same hard rules:

- **Target validation before any bytes leave the machine**: URL
  scheme allowlist per tool family (`http`/`https`, `ws`/`wss`); URLs
  carrying credentials (userinfo) are rejected outright — credentials
  never ride a diagnostic URL; port bounds.
- **Private / link-local / loopback destinations are blocked by
  default for every generic diagnostic** (tcp, tls, https, websocket,
  dns, udp, traceroute, path_mtu). Private-target permission is
  EXPLICIT and tool-scoped: only the two genuinely local-endpoint
  tools — `socks5` and `http_connect`, whose defaults are
  `127.0.0.1` and whose purpose is testing the user's own local
  proxy — may target private addresses implicitly. Intentional local
  testing with a generic tool requires the explicit
  `AllowPrivateTargets` capability. (This is the v0.9.8.1
  Windows-CI root fix: the earlier policy treated generic tools as
  implicitly private-target-safe, which bypassed the rebinding
  guard.)
- **DNS-rebinding guard for direct probes**: the hostname is
  resolved, every answer is validated against the policy, and the
  connection is dialed to a validated address
  (resolve → validate → pin). A hostname resolving only to
  blocked ranges is refused before any connection.
- **Redirects are capped** at `MaxRedirects` (default 3) and every
  hop's destination is re-validated against the same no-credentials
  and private-destination policy as the initial target — on the
  direct path (where the dialer additionally guards at connection
  time) AND on the tunneled path (where the proxy resolves
  remotely, so hop validation is the guard). A redirect chain can
  never be steered onto a loopback/private endpoint.
- **Response bodies are capped** at `MaxResponseBytes`
  (default 256 KiB); bytes beyond the cap are neither read nor
  stored.
- **User-triggered ONLY**: no tool — especially public-IP lookups —
  ever runs automatically at startup or in the background; the
  service layer enforces this.
- **Bounded concurrency**: at most 3 tools run at once in the app
  service (`toolConcurrency = 3`); a fourth request fails honestly
  with "too many tools running concurrently" instead of queueing
  unbounded.
- Remote JavaScript is never executed and configuration credentials
  are never transmitted.

## Tunneled execution

When a tunnel (provider or core session) is active, tools can run
with `path = tunneled`: the runner's dialer routes through the active
session's local SOCKS endpoint, and the result names the provider.
The direct path keeps the DNS-rebinding guard; the tunneled path
delegates resolution to the remote endpoint (the proxy resolves
remotely — same documented semantics as the URL tester). The UI
offers the via-tunnel toggle only while a tunnel is active.

## Events

Every run emits structured, credential-free events through
`internal/logging`:

- `network_tool_start` (debug) — tool, timeout, path, provider;
- `network_tool_complete` (info) / `network_tool_failed` (warn) —
  tool, status, `duration_ms`, target and `error_kind` (fixed classes:
  timeout, refused, reset, tls, dns, unsupported, …).

Targets in events are the validated host:port / URL forms — never
credentials, UUIDs or subscription URIs.

## Testing

`engine/netcheck/tools_test.go` drives the catalogue, the result
contract, the timeout clamping ([1 s, 60 s]), the safety layer
(scheme allowlist, credential rejection, private-range blocking,
rebinding guard, redirect and size caps) and the honest
`unsupported`/`invalid_target` paths against local listeners and
`httptest` servers — no external infrastructure. Latency values in
results follow the canonical semantics and are covered by
`engine/tester/latency_test.go` (see docs/latency.md).
