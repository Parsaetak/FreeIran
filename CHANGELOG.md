# Changelog

Historical release record — the authoritative home for release facts
and evidence. Structure since the v0.12.0 consolidation: current
product description lives in the README, forward phases in ROADMAP.md,
detailed architecture in the docs/ tree (see docs/README.md). Older
entries below v0.11.0 are compacted to version / purpose / key
changes / important evidence and limitations; their full original
narratives remain retrievable from git history. Historical entries
are historical: version numbers, core pins and verification claims
inside them describe the state of that release, not the current
state.

## v0.14.1 — CI repair and first-party engine correctness

v0.14.1 is a correctness release: it removes the two v0.14.0 CI
failure causes (a cancellation scheduling race in the Shadowsocks
suite; secret-scanner findings on deterministic KDF test vectors) and
repairs every latent defect the audit behind them surfaced across the
first-party engine, the TUN control plane and the routing/DNS call
graph. No new capability is claimed anywhere; every claim this entry
makes is pinned by a test named below.

### The two CI failures (root-caused and fixed)

- **Shadowsocks cancellation was a scheduling bet.**
  `TestCancellationMidStreamFailsCleanly` slept 100ms and ASSUMED a
  concurrent 8MiB `Write` had already parked — on a runner where the
  write was still in flight (loopback buffering), the write completed
  after cancellation and the test failed with "Write succeeded after
  cancellation", for all three AEAD methods (CI run 37188707721).
  The test now proves the park as an OBSERVED FACT against a gated
  raw transport: cancellation must interrupt the parked write with a
  `net.ErrClosed`-class transport error, make post-cancel I/O fail
  fast, and leave no watcher goroutine behind. No sleeps as
  synchronization.
- **Deterministic KDF vectors looked like secrets to the scanner.**
  Gitleaks 8.24.3 flagged four `generic-api-key` findings in
  `kdf_test.go` (lines 23/25/27/28 of the v0.14.0 tree): the
  EVP_BytesToKey vectors were stored as hex STRINGS with
  password-shaped names, plus `key=<hex>` in comments. The vectors are
  now typed byte-array literals preserving the exact expected bytes,
  with the scanner-facing representation gone and the independent
  in-test KDF verification untouched. `.gitleaks.toml` was NOT
  weakened (verified: `gitleaks dir .` over the fixed tree reports no
  findings; the security job's `generic-api-key` rule stays fully
  active).

### Shadowsocks (first-party protocol slice)

- **`net.Conn` concurrency contract made real (item K).**
  `chunkReader` and `chunkWriter` own their mutable framing state
  (nonce counters, scratch buffers, payload windows) behind
  per-direction mutexes: concurrent Reads serialize, concurrent
  Writes serialize, Read and Write stay parallel, and Close/cancel
  racing I/O is safe. Pinned by `TestTunnelConcurrentIODirections`
  and `TestTunnelCloseRacesIO` under `-race`.
- **Cancellation is observable inside the tunnel.** `tunnelConn`
  carries a fail-fast closed flag set BEFORE the raw transport close;
  the context watcher (`watchLoop`) is a named, leak-free goroutine
  whose exit is observable (`watchDone`). Blocked I/O is interrupted
  by the raw close; post-cancel I/O fails fast with a
  `net.ErrClosed`-wrapped error; transport errors are preserved
  (`errors.Is`-able).
- **Half-close is a documented DECISION, not an accident (item L).**
  `CloseWrite` is deliberately not reachable on the tunnel: the only
  wire form a client half-close could take (an implicit zero-length
  chunk) is not interoperable with the reference implementation, and
  faking one is forbidden. The relay's own `CloseWrite` to the plain
  TCP target is unchanged and separately proven
  (`TestRelayHalfClosePropagatesToTarget`,
  `TestTunnelHalfCloseExplicitlyUnsupported`).

### Engine: one Router, one resolver authority, one lifetime

- **HTTP absolute-form no longer bypasses the Router (item B).** The
  plain-HTTP proxy path went `beginSession → outboundDecision`,
  skipping routing entirely. Absolute-form now flows through the SAME
  `routeFlow` pipeline as CONNECT, SOCKS5 and TUN: one Router
  decision, one session, one dial point. BLOCK decisions refuse with
  403-class, DIRECT decisions dial directly, PROXY goes through the
  configured remote. Pinned by
  `TestHTTPAbsoluteFormUsesCentralRouter` (a block-all policy must
  produce 403 and zero origin contact).
- **The DNS authority is real (item J).** `Options.Resolver` is now
  the engine's actual authority: a DIRECT decision on a domain target
  resolves through the engine resolver before dialing
  (`DirectOutbound.Resolver`), IP literals never consult DNS, and
  proxied flows preserve the hostname for remote-side resolution with
  the choice RECORDED on the session (`Session.ResolverChoice`).
  Bootstrap resolution for the TUN's own upstream needs stays a
  separate, loop-prevention-constrained concern
  (`BootstrapResolver`). No Windows adapter DNS is touched anywhere.
  Pinned by `TestDirectDecisionUsesEngineResolver`.
- **`ServeFlowsOnly` cannot complete before its service lifetime ends
  (item A).** The v0.14.0 completion monitor waited on a WaitGroup
  that could still be at zero while the TUN dataplane was fully
  capable of delivering flows (and `HandleTUNFlow`'s `Add` raced that
  zero-counter `Wait`). The TUN path now holds an explicit admission
  SENTINEL taken synchronously before the monitor starts; flow
  admission and sentinel release are serialized under one mutex;
  stop/cancel closes admission first, owned flows drain, and
  completion signals exactly once. Pinned by
  `TestServeFlowsOnlyLifetimeCompletesOnlyAfterStop` and
  `TestServeFlowsOnlyCancelClosesAdmission`.

### Windows TUN control plane

- **The upstream constraint model says what it means (item C).** The
  single `ExcludedInterfaceIndex` field was documented as "the TUN to
  avoid" while every producer filled it with the PHYSICAL index the
  dialer then bound TO — including `LoopGuard.Constraint()`, which
  could bind upstream traffic TO THE TUN. The model is now explicit:
  `TUNInterfaceIndex` (forbidden as a bind target) and
  `PhysicalInterfaceIndex` (the bind target), with `Validate()`
  refusing missing-physical and physical==TUN fail-closed. The
  activation fails closed when no physical interface can be observed.
- **Cancellation reaches the Windows dialer (item D).** The binding
  seam is `func(ctx, network, address)` end to end and the Windows
  implementation uses `net.Dialer.DialContext` — a cancelled upstream
  dial stops dialing. Pinned by
  `TestBoundDialerContextReachesBinding` (a deterministically blocked
  bind observes the caller's cancellation; no sleeps).
- **`IP_UNICAST_IF` / `IPV6_UNICAST_IF` are now encoded correctly and
  differently (item E).** v0.14.0 applied one byte-order treatment to
  both families and inferred the family from hostname text (a domain
  resolving to v6 got the v4 option). v0.14.1: IPv4
  `IP_UNICAST_IF` carries the interface index in NETWORK byte order,
  IPv6 `IPV6_UNICAST_IF` carries it NATIVE, and the family comes from
  the RESOLVED destination address (or an explicit `tcp4`/`tcp6`) —
  an ambiguous family fails closed. Pinned by
  `TestUnicastSocketOptionIPv4Literal`, `TestUnicastSocketOptionIPv6Literal`,
  `TestEffectiveFamily`, `TestUnicastSocketOptionHostnameAmbiguousRefused`
  (Windows test surface).
- **Wintun send errors surface (item F).** `WritePacket` ignored the
  ring entirely: `session.SendPacket` cannot report failure, so
  ring-full (`ERROR_BUFFER_OVERFLOW`) and terminating-session
  (`ERROR_HANDLE_EOF`) packets were dropped while the code returned
  nil. The send path now allocates through `AllocateSendPacket`
  first (where Wintun reports those errors), copies, then submits:
  one attempt, no unbounded retry, no silent loss, no fake success
  counter. Pinned by the `TestWintun*` suite (Windows).
- **Additive IP mutation only (item G).** The activation used
  `SetIPAddressesForFamily`, which FLUSHES the whole address family
  on the interface before adding — destroying unrelated user-created
  addresses. The activation primitive is now `AddIPAddress`
  (`CreateUnicastIpAddressEntry`) per address, with rollback deleting
  exactly the entries the session created
  (`DeleteUnicastIpAddressEntry`), through a testable additive
  `IPMutator` seam. Pinned by
  `TestApplyInterfaceConfigPreservesForeignState` (pre-existing
  foreign address → enable → still exists → disable → still exists)
  and `TestApplyInterfaceConfigMidBatchFailureRollsBackExactly`.
- **IPv6 covering routes are real (item H).** v0.14.0 assigned an
  IPv6 address to the first-party TUN but installed IPv4 covering
  routes only — the dataplane could not honestly claim dual-stack
  routing. v0.14.1 installs the IPv6 split-default pair (`::/1` +
  `8000::/1`) through the same additive, exact-rollback IP Helper
  authority, verifies ownership of EVERY covered route (both
  families) through the dual-stack `MIB_IPFORWARD_ROW2` lookup bound
  to the TUN's LUID (`VerifyRoutesOwnedByLUID`), and the address-plan
  collision check now covers both families.

### TUN evidence semantics

- **`Active` no longer exceeds its evidence (item I).** The
  first-party backend's `TUNSnapshot` carries an explicit evidence
  ladder — Compiled / PlatformReady / StackReady / RouteReady /
  TrafficVerified — where every rung is earned by an actual gate and
  `TrafficVerified` is NEVER set by the activation (no physical
  traffic probe exists, and unit/in-memory evidence must never be
  promoted into Windows runtime evidence). The managed sing-box TUN
  remains the explicit compatibility fallback whenever the first-party
  capability gate does not hold.

## v0.14.0 — Phase 2: first FreeIran-owned TUN dataplane, real routing/DNS, first encrypted protocol

v0.14.0 crosses the architectural boundary ROADMAP.md defines for
Phase 2: the FreeIran-owned network dataplane now carries real TCP
traffic end to end, routing and DNS are real engine authorities
instead of placeholders, and the first encrypted protocol slice
(Shadowsocks AEAD TCP) ships behind interoperability evidence. Every
capability the engine still does not implement keeps running through
the existing external cores — nothing was silently reclassified, and
no unsupported first-party claim is made anywhere.

### Engine hardening (Phase A)

- **Drained shutdown.** `Engine.Stop(grace)` now has real semantics:
  new sessions are refused (registry drain gate), live sessions are
  cancelled, listeners close, and with `grace > 0` Stop waits (bounded)
  until the session registry reaches zero. `Wait` reports completion
  only after every engine-owned goroutine exited — a parent-context
  cancellation now produces the same observable shutdown as an
  explicit Stop (the v0.13.1 defect where a parent cancel left the
  listeners open is closed).
- **Listener failure is observable.** A listener that dies (or is
  closed by something other than Stop) while the engine believes
  itself running is recorded as an engine failure through `Wait` —
  never a silent dead inbound.
- **Lifecycle races designed for, not hoped away.** Stop/Wait/Run are
  safe under concurrent callers (state mutex + separated stop-action /
  completion onces); targeted `-race` coverage pins engine stop,
  session creation/removal, listener failure, and goroutine-lifetime
  invariants.

### First-party TUN dataplane (Phase B)

- **`engine/freecore/netstack`** isolates gVisor netstack (pinned:
  `github.com/sagernet/gvisor@v0.0.0-20250325023245-7a9c0f5725fb`, the
  build-friendly fork sing-box ships) behind FreeIran interfaces — no
  gVisor type crosses the package boundary. Promiscuous + spoofing
  NIC, catch-all route table, TCP forwarder with a real half-open
  bound (`maxInFlight=1024`; this fork treats 0 as drop-everything),
  bounded flow registry, and honest counters for every disposition
  (in/out/dropped/malformed/unsupported/refused). UDP without a
  handler is an explicit fail-closed classification — it is counted
  and refused, never silently leaked to the physical interface.
- **`engine/freecore/tun.MemDevice`** is the platform-neutral in-memory
  device (same Device contract) that makes the whole packet path
  testable on any OS.
- **End-to-end evidence (in-memory integration, Linux):** application
  packets → fake TUN device → userspace IP stack → engine session →
  Router (default policy → PROXY) → first-party SOCKS5 outbound →
  loopback SOCKS5 fixture → real echo server → back through every
  layer, IPv4 and IPv6, byte-for-byte, under `-race`. Measured in CI
  conditions (`TestPhase2Measurements`, in-memory, not a network
  benchmark): engine start ~52µs, dataplane start ~0.4ms, 4 MiB echo
  round-trip ~13 MiB/s, zero external child processes, PID 0.

### TUN activation transaction + loop prevention (Phases C/D)

- The ONE TunnelService now selects the TUN dataplane per activation:
  the first-party backend (`freecoreTUNBackend`) is preferred when the
  FreeIran Engine genuinely supports the configuration and the
  platform gate (Windows + elevation) holds; the managed sing-box
  dataplane remains the explicit fallback for everything else. The
  selection is logged (`tun_backend_selected`) and surfaced through
  `TUNSnapshot.Backend`; the two dataplanes are mutually exclusive.
- The first-party activation transaction validates the route, checks
  elevation BEFORE any mutation, re-derives the deterministic adapter
  identity, refuses address-plan collisions against live observation,
  opens the Wintun device, and applies interface addresses + covered
  routes (0.0.0.0/1 + 128.0.0.0/1 split-default shape) through the
  FreeIran-owned IP surface (`freecore/tun/winip_windows.go`, over the
  production-proven wireguard-windows `winipcfg` helpers) with every
  mutation recorded as an undo. Rollback runs undos in reverse,
  surfaces residual failures honestly, and closes the device.
- **Loop prevention** is a first-class dialer contract:
  `freecore.NewUpstreamDialer(constraint)` binds engine upstreams to
  the physical interface observed live (`IP_UNICAST_IF` /
  `IPV6_UNICAST_IF`) so an upstream can never re-enter the FreeIran
  TUN. When the platform cannot honor the constraint the upstream
  dial is REFUSED — fail-closed beats a loop. The constraint is built
  from current OS observation on every activation (never a hardcoded
  gateway or adapter index), and the verification gate re-checks the
  adapter, the covered-route ownership and the upstream interface.

### Real routing authority (Phase E)

- `Router` decisions now carry action (DIRECT / PROXY / BLOCK),
  outbound, resolver choice, reason and rule id. The default policy
  preserves the v0.13.1 contract (everything through the configured
  remote); explicit policies gain block lists, direct lists and
  private-direct. Malformed targets BLOCK (fail-closed). The local
  inbounds and the TUN flow path call the SAME router — no protocol
  implementation hides a decision.

### Real DNS authority (Phase F)

- `freecore.CachingResolver` is the bounded DNS authority behind the
  existing `Resolver` seam: bounded cache (512 entries), positive and
  negative TTLs, context-honoring lookups, caller-cancellation never
  cached as a decision, and a bounded observation ring recording
  resolver, route, success/failure class, cache state and timing.
- `freecore.BootstrapResolver` resolves the proxy endpoint's own name
  over explicit DNS servers through the constrained dialer — bootstrap,
  destination resolution and TUN-originated DNS stay separated, no
  recursion into the TUN, and Windows adapter DNS settings are never
  touched by the engine.

### SOCKS5 + Shadowsocks (Phases G/H)

- The first-party SOCKS5 client gained a transport-dial override (the
  loop-prevention dialer owns the proxy dial under TUN) with the
  handshake unchanged and all existing loopback coverage intact.
- **Shadowsocks AEAD TCP** (`engine/freecore/shadowsocks`):
  aes-128-gcm, aes-256-gcm, chacha20-ietf-poly1305; EVP_BytesToKey
  master keys, HKDF-SHA1 "ss-subkey" per-connection subkeys, LE nonce
  counters, 0x3FFF chunk limit, fail-closed auth/tamper/oversize
  handling, honest-idempotent close. Evidence: KDF vectors against an
  independent reimplementation, framing round-trip/tamper tests, a
  full client↔server round-trip suite, AND reference
  interoperability both directions against
  `github.com/shadowsocks/go-shadowsocks2 v0.1.5` for all three
  methods (our client through their server; their client through our
  fixture). Two implementation bugs found and fixed by the evidence
  suite (reader buffer aliasing; close-after-cancel semantics).
  Deprecated stream ciphers and AEAD-2022 are NOT implemented.

### Backend selection ownership (Phase I)

- `core.Registry.Select` now enforces first-party capability
  ownership: when the FreeIran Engine genuinely supports the
  configuration it OWNS the selection, and an ordinary external
  `PreferredBackend` setting only orders the external cores (a stale
  preference can no longer silently launch an external core for a
  supported route). Regression matrix added (SOCKS/HTTP/Shadowsocks
  first-party routes, VLESS/TLS/QUIC external routes, preference and
  fallback ordering).

### Compatibility and honesty

- External cores (Xray/V2Ray/sing-box/Mihomo) continue serving every
  unsupported capability; System Proxy still launches no child process
  for first-party-supported routes; the connection state machine, the
  core registry, the TUN controller, the config store and the test
  queue remain the only authorities — no duplicate manager exists.
- Evidence classes for this release: Levels 1–3 (compile, unit,
  in-memory integration, real protocol interoperability) plus
  compile-verified Windows platform code. Physical elevated Windows
  TUN runtime (Level 5) with real traffic is NOT claimed in this
  release; docs/tun.md records the exact gate the activation verifies
  today. CI results are checked before any CI claim.

## v0.13.1 — Phase 1 foundation: FreeIran Engine, first-party System Proxy path, TUN foundation, Windows icon + Settings repairs

v0.13.1 is the first half of a deliberate two-phase transition
(ROADMAP.md): it lays the first-party engine foundation and repairs
the application surfaces, while every capability the engine does not
genuinely implement keeps running through the existing external
cores. No external core was replaced in this release; none is
claimed to be.

### Windows icon — the real root cause, fixed

- **Root cause (found by tracing the pinned Wails
  v3.0.0-beta.19 source, not by guessing).** Wails beta.19 assigns
  the window icon by loading `RT_GROUP_ICON` with **numeric
  resource ID 3** from the executable
  (`NewIconFromResource(GetModuleHandle(""), 3)` in
  `webview_window_windows.go`), and only falls back to
  `application.Options.Icon` bytes when that lookup fails; the
  window class icon is a separate lookup of icon-group ID 32512 that
  no Wails build satisfies. FreeIran's committed `.syso` stored its
  only icon group under the resource **name** `"APP"` (a go-winres
  string-name key), so the ID-3 lookup always failed, no `WM_SETICON`
  was ever sent, and the window fell back to the class icon — which
  is itself NULL — producing the generic/default titlebar and
  taskbar icon. Explorer still showed the right file icon (it takes
  the first group regardless of name) and the tray was correct
  (runtime PNG), which is exactly the "missing taskbar icon +
  mismatched opened-window icon" defect: the v0.13.0 resource
  inspection passed because it never checked the group's name/ID.
- **Fix.** `build/winres.json` now emits the icon group under the
  numeric ID the pinned Wails loads (`"#3"` instead of `"APP"`), the
  `.syso` was regenerated from it, and `application.Options.Icon` is
  set from the same canonical embedded PNG (`internal/appicon`) as a
  documented defense-in-depth fallback for builds whose resource
  lookup fails. Executable, window and tray representations all
  derive from the one canonical icon family.
- **Test hardened.** `TestWindowsGUIIcon` now also asserts the
  single `RT_GROUP_ICON` is registered under **numeric ID 3** — the
  exact regression class that shipped v0.13.0 with a broken desktop
  icon while the test was green.
- **Evidence honesty.** Proven: resource layout of the linked PE
  (group ID 3, multi-size family, byte-identical to the canonical
  asset), the code path of the pinned Wails loader, and the
  Windows-targeted compile. NOT proven in CI: live shell rendering
  (no interactive Windows session exists in CI); the release notes
  and docs state this explicitly instead of claiming "taskbar
  fixed".

### Settings — numeric controls actually compact

- The v0.13.0 compact-input CSS capped the `<input>` boxes but left
  the WRAPPERS oversized: `.settings-row > .field` forced
  `min-width: 160px` and `flex: 1` (the four runtime-log fields
  wrapped raggedly), `.field-grid.two` gave each port field a
  full-width grid track, and the QuickConnect profile-form ports
  used a divergent second styling path with no compact class at
  all. Numeric inputs now render in genuinely compact fields
  (~80–110 px) inside compact rows: the runtime-log fields share one
  tight row, port pairs size to content, profile-form ports reuse
  the same compact treatment, and the three test/racing range
  sliders are styled and constrained instead of stretching
  full-width unstyled.
- The number-input spinner is suppressed so the compact width is
  usable digits, labels/descriptions/warnings are unchanged, and
  keyboard navigation/focus visibility/native input behavior are
  untouched.
- Regression coverage: a Settings DOM test asserts every numeric
  control carries the compact treatment, and a CSS contract test
  pins the layout rules that kept the wrappers oversized
  (`min-width` on settings-row fields, grid track stretching,
  sysint port stretching) so the rendered layout cannot silently
  regress to v0.13.0's.

### FreeIran Engine foundation (`engine/freecore`) — real code, real bytes

- The first-party Go engine exists with a deliberate, minimal
  internal model: `Engine` (lifecycle, bounded session registry,
  capability reporting), `Session` (bounded lifecycle, cancellation,
  metadata, byte counters), inbound/outbound/dialer/router-decision
  and DNS-resolver interfaces, and a transport/security abstraction
  that exists only where a real subsystem boundary sits.
- **One-time normalization**: the universal `config.Config` is
  converted into the engine model exactly once
  (`freecore.Normalize`); protocol implementations never re-parse
  the application configuration.
- **Local proxy pipeline that actually carries traffic**: a local
  HTTP CONNECT inbound (CONNECT tunneling plus absolute-form
  forwarding) and a local SOCKS5 inbound (RFC 1928 subset:
  no-auth CONNECT; BIND/UDP-ASSOCIATE refused honestly), served on
  the same port pair the external cores expose, forwarding through
  SOCKS5, HTTP-CONNECT and direct outbounds with context
  cancellation, deadlines, bounded buffers and per-session
  accounting end-to-end. No external process is spawned for
  first-party-supported sessions — the bytes move through
  FreeIran-owned Go code.
- **Honest capability gate**: the engine declares exactly
  SOCKS/HTTP remotes over plain TCP with no security layer (plus
  direct). VLESS, VMess, Trojan, Shadowsocks, the QUIC family,
  WireGuard, TLS/REALITY transports, UDP and proxy chains are
  refused by `Supports`/`Validate` and continue through the
  existing external-core compatibility path.

### System Proxy decoupled from sing-box (first-party path)

- The FreeIran Engine is registered in the ONE core registry as an
  in-process backend (`freecore`, selection priority 0). For a
  supported route the ONE connection state machine selects it,
  the local proxy endpoint is owned by the engine, and enabling
  System Proxy points WinINet at that engine-owned endpoint —
  sing-box (or any external core) is never launched for the
  session. The tray's System Proxy toggle follows the same path.
- Everything around WinINet is unchanged and still owned by the one
  TunnelService authority: per-connection capture/apply/verify/
  restore, idempotent ON/OFF, clean rollback on failed enable,
  durable crash/stale-ownership recovery, and Main/tray convergence
  on the same state.
- **Honest backend identity everywhere**: connection snapshots,
  the Backends view, tunnel state and the UI label the active
  backend — `FreeIran Engine` for the first-party path, the
  external core name for fallback paths. The app-level
  `EnableSystemProxy` records the real backend of the endpoint it
  points WinINet at.
- Unsupported routes are provably unchanged: registry selection
  still resolves them to Xray/V2Ray/sing-box, with the existing
  preferred-backend setting and bounded fallback intact.

### TUN — first-party device/control foundation (NOT a dataplane)

- `engine/freecore/tun` establishes the first-party TUN boundary
  for Phase 2: a `Device` interface with a packet channel boundary,
  a deterministic FreeIran-owned adapter identity, a
  transaction/rollback state representation, loop-prevention
  metadata (the interface/route exclusion facts the future upstream
  dialer must honor), the Windows Wintun adapter/session lifecycle
  through the reviewed `golang.zx2c4.com/wintun` binding (MIT), and
  a read-only Windows IP Helper observation seam over
  `golang.org/x/sys/windows`.
- Platform-neutral ownership/identity/rollback/loop-guard decisions
  are unit-tested on Linux; the Windows-specific layers compile in
  the windows/amd64 cross-build.
- **No fake TUN**: nothing in the UI or the tunnel state exposes
  the first-party TUN as working. The sing-box native TUN backend
  remains the only runnable TUN dataplane and the explicit
  selection; the first-party packet dataplane is Phase 2 item 1
  (ROADMAP.md).

### Version / infrastructure

- VERSION, `internal/version`, `frontend/package.json` and the
  Windows resource metadata are consistently 0.13.1 (the CI version
  audit enforces this).
- Documentation set updated as the Phase 1/Phase 2 contract:
  README (current product, honest engine status), ROADMAP (the
  two-phase program with per-item purpose/dependencies/boundary/
  evidence/fallback/retirement gates), docs/architecture.md (the
  engine boundary), docs/tun.md (the first-party TUN foundation and
  its explicit not-a-dataplane status), docs/protocols.md (the
  first-party capability row with its evidence class),
  docs/autonomous-connectivity.md (Phase 2 handoff).

## v0.13.0 — dataset truth (counts/pagination/sorting), source freshness, Main-page system integration, tray controls, Windows icon root-fix

v0.13.0 makes the configuration workspace honest at the real dataset
size, moves system integration to the Main page as a first-class
block, extends the native tray with real proxy/TUN toggles, and
root-fixes the Windows executable icon chain. Interaction reference
for the workspace work: current upstream v2rayN (dense grid, stable
global sort, factual status text) — adopted as interaction semantics
only; FreeIran's single-store/single-supervisor/single-connection
architecture remains authoritative and no visuals were cloned.

### Configuration dataset truth (backend)

- **All badge equals the real store count.**
  `builtinGroupCounts()` stopped at `candidateScanLimit` (4000) — the
  All badge capped at 4000 on a 23,871-record store. The count scan is
  now UNBOUNDED and counts the whole store through a four-field
  projection per record (no credential material retained); the result
  is served from one authoritative cached count snapshot invalidated
  by real store/test/collection changes (store count + collections
  revision identity, 15 s freshness backstop for the
  `recently_tested` window drift). The store is never duplicated.
- **No 20,000-match truncation in filtered listings.**
  `ListConfigsFiltered` materialised at most `maxFilteredMatches`
  (20,000) full configurations and reported the bound as Total. The
  listing now materialises ONE lightweight sort key per match
  (id/latency/tested/working/protocol/source/address/manual-order
  position), computes the TRUE global total, orders globally and
  decodes only the requested page window back through the store + hot
  cache. Memory stays bounded; the dataset does not.
- **Global deterministic sort with a config-ID tie-breaker.** Every
  explicit sort (latency/tested_at/protocol/source/address) is a
  total order: the primary field (with direction), then ascending
  config ID in BOTH directions — paging cannot duplicate or skip
  records when values tie (equal latencies are the common case).
  Manual ordering (no explicit sort) keeps the v0.9.8.3 semantics
  (positioned first in stored order, then store order). The old
  `sortConfigs`/`applyConfigOrder` full-materialisation paths are
  replaced by the key-based comparator.

### Configuration workspace (frontend)

- **Filtered scopes paginate for real.** The filtered view was a
  one-shot `loadFiltered(1000)` — a 23,871-record store showed 1000
  rows, period. Filtered scopes now load 200-row pages with real
  infinite scroll in EVERY scope (status/protocol/sort/source/group/
  chain/search), driven by the same scroll handler as the unfiltered
  list; a request sequence guards stale async responses and page
  state resets on every query/filter/sort/scope change. The protocol
  filter and bare search go through the ONE server-side pipeline
  (they previously ran client-side over a bounded loaded window).
- **Sort click cycle fixed.** `sortColumn()` cleared the active sort
  on the second click (a v0.12.2 state bug). First click ASC, second
  click DESC, and the sort never silently clears.
- **Source rail hygiene.** With more than five subscriptions the
  remaining source chips collapse behind one compact "More sources"
  disclosure — the top of the page stays scannable; no behavior
  change, same chips and counts.

### Source freshness contract

- **Root cause of the "739889d ago" age.** `source.Stats` serialised
  `LastSuccessfulFetch`/`LastFailure` as Go `time.Time` — a zero time
  marshals as `0001-01-01T00:00:00Z` (encoding/json never omits a
  struct), which the UI converted into an impossible age. The UI
  projection now carries `*time.Time` with `omitempty`: never-fetched
  sources serialise WITHOUT the timestamp and render "Never fetched";
  valid times stay exact. `sourceFreshness()` renders the factual
  labels ("Updated just now", "Updated 12m ago", "Updated Oct 1,
  13:05" / exact time in the title) and treats impossible ages as
  never-fetched (defense in depth).
- **Hashes are internal again.** The Sources page shows freshness,
  not `synced · <hash8>` — content hashes are provenance/dedupe
  evidence, never ordinary UI.

### System integration on the Main page (honest prerequisites)

- System Proxy ON/OFF and TUN/VPN ON/OFF are first-class toggles on
  Quick Connect, bound to the ONE TunnelService (no second
  integration manager). Local inbound port settings (SOCKS5 / HTTP)
  ride the ONE settings path. Prerequisites are stated and enforced:
  the system proxy needs a connected session (it points WinINet at
  the live local inbound); TUN needs a selected/connected
  configuration (it compiles through the managed sing-box dataplane).
  Disconnected state stays meaningful: the real backend mode is
  shown, OFF really turns integration off, and stale app-owned proxy
  residue from a crashed session is removable without a connection
  (`TunnelService.CleanupStaleOwnership` replays the recorded
  previous state through the ONE WinINet authority).
- Connection keeps the detailed lifecycle evidence (mode, live TUN
  snapshot, durable ownership) and drops the duplicate control
  buttons — two controls for one dataplane was two truths.
- `SetTunnelStateListener` (App) publishes the post-call tunnel state
  after every TunnelService mutation — enables, disables, failed
  enables and cleanup — so tray and UI always observe the
  authoritative dataplane state.

### System tray (real toggles, no drift)

- The native tray menu gains System Proxy and TUN (VPN) checkboxes
  bound to the same TunnelService path as the UI. The checkbox
  visuals are owned by a sync from the authoritative services, never
  by the click itself: the menu re-reads the real state after every
  tray action, after every TunnelService transition and on every
  connection snapshot — failed enables, disconnects, restarts and
  rebuilds cannot leave the menu lying. Honest refusals are logged
  when a prerequisite (live inbound, selected configuration) is
  missing. `tray_enabled` and graceful shutdown behavior unchanged.

### Windows icon root-fix

- **Root cause.** `build/winres.json` fed `assets/freeiran-icon.png`
  (a single 256 px image) to `RT_GROUP_ICON` — go-winres resizes one
  image into the icon group, which is exactly why small surfaces
  (titlebar/taskbar) looked poor. The canonical multi-size family
  (`assets/freeiran-icon.ico`, 16–256 px, seven images — the ONE icon
  asset authority produced by `scripts/genicon.py`) exists for this.
  Additionally, in go-winres's array form every entry goes through
  `image.Decode` (PNG-only); an `.ico` must be referenced with the
  single-string form. The committed `.syso` was also STALE (built
  from a 0.11.0-era winres.json).
- **Fix.** `winres.json` references `../assets/freeiran-icon.ico`
  (string form) and the committed
  `cmd/freeiran/rsrc_windows_amd64.syso` is rebuilt reproducibly from
  it at the release version (manifest identity now 0.13.0.0).
- **Regression check.** `TestWindowsGUIIcon` (Windows, wired like the
  PE subsystem check) verifies the BUILT executable: exactly one
  `RT_GROUP_ICON`, a multi-size `RT_ICON` family, and every icon
  image byte-identical to `assets/freeiran-icon.ico` — a stale
  `.syso` or a foreign icon fails the build. Tray and Linux window
  icon keep using the same canonical icon family.

### Verification

- New Go tests: uncapped group counts vs. store count (4,250 records
  > old 4,000 cap), count-snapshot invalidation on membership change,
  filtered global sort/paging determinism (600 records with heavy
  ties; no duplicates/gaps across pages; deterministic DESC with the
  ascending ID tie-break), filtered Total beyond the removed 20,000
  cap (20,500 records, tail page present), zero-time source-stats
  serialization. Proxy-chain suite unchanged and green.
- Frontend: updated pagination/sort-cycle expectations; shell test
  still proves primary navigation. `v2rayN`-derived changes are the
  interaction semantics above — each change has a concrete
  implementation justification, nothing is a visual clone.

## v0.12.2 — blank-window root-cause repair, Tor/Psiphon removal, proxy chains, route evidence

v0.12.2 repairs the v0.12.1 blank-window regression at its root,
removes Tor and Psiphon from the active product (vertical removal —
implementation, lifecycle, UI, bindings, settings and discovery —
never only buttons), introduces real proxy chains compiled into ONE
core process, and polishes verified-route selection with an honest
evidence line.

### Blank-window root cause (frontend)

- **Root cause.** `App.tsx` called `useSuppressNativeContextMenu()`
  INSIDE a `useEffect` callback while the helper itself calls
  `useEffect` — a Rules of Hooks violation. The invalid-hook error
  threw inside App's own effect, above every error boundary, and
  React unmounted the whole tree: the backend reached
  `application_ready` / `window_show` / `warmup_complete` and the
  window appeared, but the visible UI was blank.
- **Fix.** The policy hook is mounted at the TOP LEVEL of the App
  component (the smallest correct architecture); the app-wide
  right-click policy itself is unchanged — rows still open FreeIran's
  own MenuSurface, text fields keep native clipboard/IME behaviour.
- **Regression guards.** A shell test renders the REAL `<App/>` and
  must paint the FreeIran navigation (the exact failure class the
  v0.12.1 tests could not see), plus a static Rules-of-Hooks check
  that walks every production source file's AST and rejects any hook
  called inside a nested callback (no ESLint exists in this repo;
  the checker also proves itself against the historical defect).

### Tor / Psiphon removal (vertical)

- `engine/provider` (Tor engine, Psiphon engine, managed-binary
  pipeline, adoption/discovery, fake binaries, all provider tests)
  deleted; `engine/connection/provider.go` provider sessions deleted;
  the `ProviderService` surface and its bindings deleted.
- Provider settings (`provider_mode`, `tor_bridge_lines`,
  `tor_transport_plugins`, `psiphon_extra_config`,
  `psiphon_user_binary`) removed; a persisted legacy
  `provider_mode` MIGRATES to the surviving Quick Connect route mode
  (`tor`/`psiphon` -> `auto`) — startup never bricks on a removed
  mode. Legacy tor/psiphon PROFILE modes migrate to `auto` instead
  of dropping the record.
- Core discovery specs for tor/psiphon removed from
  `system/execdiscovery`; the Cores page "Providers" section is gone
  (protocol-core management is the whole surface); Quick Connect lost
  the Tor/Psiphon modes and their education copy.
- Preserved: the core managers (xray/v2ray/sing-box/mihomo), core
  discovery/reuse, the checksum/trust pipeline, process supervision,
  and the connection lifecycle — untouched.

### Proxy Chains (one topology, one process)

- **Data model on the EXISTING collections authority.**
  `collections.json` moves to schema v2: groups carry a stable
  `kind` — `user` or `proxy_chain`; v1 documents migrate on load
  (every existing group becomes `user`). Chain records reference
  configuration IDs only (2-4 ordered hops) — credentials and
  configuration payloads are never copied; atomic persistence and
  the future-schema refusal are unchanged.
- **Service.** `ProxyChainService` (same authority): List / Create /
  Rename / Delete / AddHop / RemoveHop / ReorderHop / Details /
  Validate / CheckChain, with credential-free projections and
  actionable errors. A chain is not a remote source — no update
  semantics anywhere.
- **Compilation.** Chains compile into ONE core process through the
  existing per-core compiler boundaries: Xray via
  `streamSettings.sockopt.dialerProxy` (egress = `proxy`, earlier
  hops = `chain-N` tags, each dialing its predecessor), sing-box via
  the outbound `detour` field. V2Ray refuses chains explicitly
  ("does not support proxy chains"); Mihomo has no connection
  adapter (unchanged). Chain compositions fingerprint over every
  hop (`ChainFingerprint`), so runtime files and generation-cache
  keys never collide across chains.
- **Execution.** `ConnectionService.ConnectChain` rides the EXISTING
  connection state machine (select -> prepare -> start ONE core ->
  ready -> verify -> connected -> monitor -> recover). Chain
  readiness alone is never success; the same multi-target
  verification gate applies. Snapshots display the chain identity
  ("(proxy chain)") and the session label distinguishes the route
  kind for network tools.
- **Checks.** `CheckChain` = per-hop stored evidence PLUS a fresh
  end-to-end measurement through the compiled chain (existing
  tester, bounded, instance closed afterwards). "Check hops" runs
  through the ONE test queue.

### Configurations UX (v2rayN interaction concepts, FreeIran design)

- Scope rail gains PROXY CHAIN scopes (name + hop count) beside the
  source scopes and user groups, plus "+ New chain".
- The chain scope header shows hop count, completeness, the compiled
  preview (A -> B -> C), per-hop working evidence, and Check chain /
  Connect / Edit — never "Update source".
- The dense table gains a dedicated SECURITY column (transport and
  security are separate facts now) and an explicit Address : Port
  column; virtualization and server-side filtering are unchanged.
- Row context menu (the ONE MenuSurface) gains real chain actions:
  build a chain from the selected rows, add the selection to an
  existing chain, and remove/reorder hops inside a chain scope. No
  fake edit actions.
- The chain editor: ordered hop list with move up/down and remove,
  protocol + endpoint + latest evidence per hop, a live preview,
  validation before save, and Check hops / Check chain / Connect.
  Hops are referenced, never copied or edited through the chain.

### Verified-route selection evidence

- The Quick Connect picker shows the compact evidence line for the
  selected route — "Selected because: verified 2 min ago · 126 ms ·
  8/10 recent successes" — derived only from recorded observations
  (measurement freshness, real latency, the success rate over the
  recorded samples). No invented confidence percentages, no opaque
  score; with no verified candidate the line says exactly that.

### Packaging, version, docs

- VERSION, `internal/version`, `build/winres.json`, the frontend
  package version and all active version strings are 0.12.2;
  historical v0.12.1 entries remain historical only.
- README is current-product-only (no release-history duplication);
  CHANGELOG owns the release narrative; docs updated for the
  provider removal and the chain architecture.

## v0.12.1 — network-tool truth, per-source configuration workspace, desktop-class context UX

v0.12.1 fixes what the v0.12.0 runtime log exposed (six mislabeled
network diagnostics, the `core_discovered` provenance leak) and turns
Configurations into a source/subscription-aware workspace — on the
existing architecture, with no second engine anywhere.

### Network tools — status semantics and logging (§2–§4)

- **Ten-state tool model.** `ToolStatus` grows `partial`,
  `not_configured`, `not_applicable` and `unreachable` beside the
  existing `ok` / `failed` / `timeout` / `cancelled` /
  `invalid_target` / `unsupported`. Every state has a DISTINCT
  structured event (`network_tool_partial`,
  `network_tool_not_configured`, `network_tool_unreachable`, ...):
  the old contract collapsed every non-OK outcome into
  `network_tool_failed`. Pinned by `TestToolStatusEventMapping`.
- **Severity policy.** Environment facts (not configured / not
  applicable / unsupported / cancelled / partial) log at info;
  measured path verdicts (unreachable) and genuine failures warn.
- **DNS (§5).** Aggregation is query-level: some answers + some
  failures → `partial` with the successful measurements retained and
  the dominant failure class attached; all applicable resolvers
  failing → `failed`; a bounded encrypted-DoH comparison row
  (cloudflare-dns.com, RFC 8484 JSON) now rides INSIDE the existing
  diagnostic (never a second resolver subsystem) — a hijacked
  plaintext path beside a working encrypted path is real censorship
  evidence. Tunneled runs fetch DoH through the tunnel dialer.
- **HTTP CONNECT / SOCKS5 (§6/§7).** The hard-coded
  `127.0.0.1:1080` default is gone. Target resolution:
  explicit user target → protocol-compatible local endpoint supplied
  by the service layer (the live session's SOCKS endpoint for SOCKS5;
  a configured local HTTP inbound for HTTP CONNECT — the SOCKS
  session endpoint is never offered to the HTTP tool) →
  `not_configured`. A missing proxy renders "Not configured", not a
  red failure.
- **QUIC (§8).** A real, bounded QUIC v1 handshake probe with the
  HTTP/3 ALPN, built on `github.com/quic-go/quic-go` — the minimal
  dedicated diagnostic dependency (measurement only; never a
  dataplane, never user traffic). Resolve→validate→pin honours the
  private-range policy; silence classifies `unreachable`, an active
  but failed handshake classifies `failed`, completion is `ok` with
  ALPN/TLS/cipher details. Deterministic local `quic-go` listener
  fixtures; CI never depends on a public QUIC endpoint.
- **WebSocket (§9).** Default runs probe a bounded two-endpoint
  curated set and aggregate honestly: all succeed → `ok`, mixed →
  `partial` with per-target evidence, all fail → `failed`. An
  explicit target answers exactly that target (one probe, no
  fallback). An endpoint outage never becomes a censorship verdict.
- **Traceroute (§10).** Windows now walks the path through the native
  IP Helper ICMP API (IcmpCreateFile/IcmpSendEcho with per-probe TTL
  in IP_OPTION_INFORMATION) — user-mode, no elevation, no
  tracert.exe/route.exe/PowerShell/shell parsing. Unix keeps the raw
  ICMP walker behind the shared `hopWalker` interface. Status
  semantics: privilege gap → `unsupported`; explicit
  destination-unreachable evidence → `unreachable`; hops observed
  then silence → `unreachable` WITH the hop evidence; complete route
  → `ok`. Hops are never fabricated.
- **Tunnel diagnostics (§11).** Direct path + no tunnel →
  `not_applicable` ("No active tunnel" — the v0.11.2 wording read as
  a tunnel breakdown); tunneled + no tunnel → `not_configured`;
  active tunnel → the existing live snapshot + real SOCKS5 CONNECT.
  The UI greys the action with the honest hint while no tunnel is
  active.
- **Full catalogue audit (§12).** Every tool's target handling,
  prerequisites, status classification, timeout, cancellation,
  direct/tunneled semantics, logging and UI label verified; the
  shared ToolRunner, catalogue, safety layer, timeout policy, result
  model and log path remain the ONE of each (no duplicate network
  diagnostics engine).

### Runtime-log privacy (§33)

- **`core_discovered` is provenance-free again.** The normal log
  prints the compact form ("xray 26.3.27 available"); commit hashes,
  `go1.x` tuples and paths move to a debug-severity
  `core_discovered_details` record (developer diagnostics keep full
  provenance). `compactCoreVersion` accepts digits-and-dots tokens
  only, so runtime/commit fragments can never re-enter the normal
  log. Pinned by `TestCoreDiscoveredMessageProvenanceFree`.

### Configuration workspace (§14–§22)

- **Source scopes in the scope rail (§15/§16).** A "folder" is a
  logical browsing scope; source/subscription scopes map to
  `cfg.Source` and filter through the ONE server-side pipeline
  (`DataService.ListConfigsFiltered` with `source`). Counts come from
  the authoritative `SourceStatsList` — no React-side health
  recomputation, no duplicated counters.
- **Source scope header (§17).** Name, enabled/disabled, trust band,
  configuration/working counts, last successful fetch, last failure
  and its recorded reason — backend evidence only. Actions: **Update
  source**, **Check source**, **Check untested**, and More
  (retry failed / retest working in scope).
- **Targeted refresh (§18).** `SourceService.RefreshSource(id)`
  refreshes exactly one source through the SAME ingestion
  architecture (one fetcher, one parser pipeline, one store, one
  scheduler): validates the ID, respects the enabled policy,
  preserves dedupe/content-hash behaviour, updates stored configs,
  publishes normal source-refresh state and returns the source's
  bounded stats. Single-owner gate: targeted refresh and full cycles
  share the `ingesting` flag — two requests can never create two
  ingestion authorities (`TestConcurrentRefreshSourceOnlyOneCycle`).
- **Targeted check (§19).** Per-source check actions enqueue through
  the existing `TestQueueService.EnqueueByFilter` with
  `source = selectedSourceID` — the queue stays the sole test
  authority.
- **User groups (§20).** Test/rename/membership actions only — a user
  group is not a remote source and never offers "Update source".
- **Interaction density (§21/§26).** The dense virtualized table,
  sticky sortable header, organize-by, detail tabs and multi-select
  action model are preserved unchanged; v2rayN's interaction patterns
  remain the reference, never its visuals.
- **Sources integration (§27).** Source rows gain targeted actions
  (update this source / view its configurations / check them) that
  land in the matching configuration scope through the existing
  `freeiran:navigate` event model.
- **Performance (§28).** Source selection is a server-side filter,
  single-test results patch one record, source refresh reconciles the
  affected scope — no full-database refresh, no N+1 calls, and the
  ~20k-configuration virtualized surface is untouched.

### Desktop-class context/keyboard UX (§25/§29/§30)

- **The native browser/WebView context menu is suppressed
  app-wide** through one shared policy
  (`utilities/contextMenuPolicy.ts`): right-click on configurations
  opens FreeIran's own MenuSurface (unchanged single menu engine —
  Escape, arrows, Home/End, focus return, viewport flipping, scroll
  handling and ARIA semantics preserved); right-click anywhere else
  opens nothing. Text fields keep their native editing menu so
  Ctrl+C/Ctrl+V/Ctrl+A and IME editing are never broken.
- **Keyboard basics:** Ctrl+A selects the visible scope;
  Shift+F10 / ContextMenu open the row menu (existing); Delete in a
  user-group scope is the existing membership removal.

### Version + verification (§32/§34/§37)

- Version surfaces bumped to **0.12.1** (VERSION,
  `internal/version/version.go`, frontend package, package-lock,
  `build/winres.json`); historical release references preserved.
- Docs: README/ROADMAP/CHANGELOG updated; `docs/internet-tools.md`
  documents the exact ten-state semantics; `docs/configurations.md`
  (new) is the authoritative description of the configuration
  workspace; `docs/ui.md`, `docs/architecture.md` and
  `docs/autonomous-connectivity.md` reconciled.
- Tests: Go — status/event mapping, DNS aggregation, endpoint-tool
  prerequisites, QUIC local handshake + silence + tunneled-unsupported,
  WebSocket aggregation, tunnel-diagnostics applicability, targeted
  refresh (scoping/enabled policy/single-owner gate/concurrency),
  provenance-free core messages. Frontend — prerequisite vs failure
  presentation, tunnel-diagnostics gating, tool descriptions, source
  scope rail/header/actions, user-group action differences, row
  right-click MenuSurface, native-menu suppression policy,
  keyboard Ctrl+A. All tool fixtures local and deterministic.

## v0.12.0 — documentation consolidation + autonomous connectivity architecture handoff

v0.12.0 is a documentation/architecture-consolidation release: no
features, no architecture replaced, no dataplane change. Its purpose
is to permanently capture the FreeIran R&D direction (September 2026
report, `Parsaetak/Contents` Research-and-Development
`FreeIran-2026-09.md`) inside the canonical repository documentation
so future coding agents can implement it incrementally without
rediscovering the research.

### Canonical future-architecture document

- **`docs/autonomous-connectivity.md` (new)** — the single authority
  for long-term architecture: product objective
  ("the transport connects, the engine decides, the evidence
  proves"), the seven-layer threat model, full-device protection
  target, central DNS authority, IPv4/IPv6 proof states +
  `ConnectionProof` contract, the Windows Filtering Platform
  kill-switch state machine (explicitly future work — the current
  TUN is NOT a kill switch), censorship differential evidence
  (probe ladder + privacy-safe failure metadata), the adaptive
  transport engine, transport/protocol roadmap, AmneziaWG
  integration model (one lifecycle, no GUI fork, no second process
  manager), Tor modes (Tor Circumvention / Onion Only), I2P as a
  private overlay, multi-hop as trust topology, the canonical routing
  engine (engine/routing/), infrastructure-aware recovery,
  evidence-first recovery policy, the privacy exposure audit,
  public-IP verification states, privacy modes, the
  browser-anonymity boundary, the extended provider lifecycle,
  testing/clean-room acceptance, and the non-goals (banned claims).
  Every future item is explicitly PLANNED/NOT VERIFIED; no current
  capability is invented.
- **`docs/README.md` (new)** — documentation index: which document is
  authoritative for which domain (current behavior vs future
  contract vs release history), version source of truth, wording
  rules for current/historical/planned statements.

### Existing documentation updated (no duplication)

- `docs/architecture.md`: "Current architecture vs target
  architecture" section added (target components explicitly do not
  exist yet); stale Android-section wording that still described the
  desktop TUN as EXPERIMENTAL/DISABLED corrected (the desktop TUN is
  current since v0.11.3; Android remains a plan).
- `docs/tun.md`: evergreen header; new "Planned future work"
  section (IPv6 leak proof, DNS engine, WFP kill switch, continuous
  monitoring — all PLANNED); the evidence ladder is unchanged and
  not weakened.
- `docs/protocols.md`: current pin corrected to sing-box 1.14.1
  (stale 1.14.0-as-current wording; the 1.14.0 references in
  per-row evidence remain, now explicitly labeled as the evidence as
  executed in those release cycles); compact future
  adaptive-transport section (implemented / planned / not verified
  split; upstream support never promotes a row).
- `docs/providers.md`: future providers section (Tor circumvention,
  I2P, AmneziaWG) under the SAME one-manager lifecycle.
- `docs/security.md`: future privacy-security contract (minimal
  metadata, no hardware identifiers, no telemetry, evidence-first
  protection, WFP kill-switch target, privacy-safe diagnostics,
  banned claims).
- `docs/ui.md`: future UI concepts (privacy modes, ConnectionProof,
  trust topology, evidence surfaces) — current UI not redesigned;
  stale 1.14.0-as-current ECH wording corrected.
- `docs/ci.md`: future acceptance matrix (unit/integration/Windows/
  clean-room targets); all existing v0.11.5 CI evidence preserved.
- `docs/development.md`: documentation-maintenance rules (version
  source of truth, docs authority hierarchy, generated
  frontend/embed workflow, no stale current-version claims,
  current-vs-historical terminology, core-pin policy).

### README / ROADMAP / CHANGELOG de-duplication

- README rewritten around the current product: identity, current
  verified capabilities, current limitations, architecture summary,
  security/privacy contract, build, repository structure, short
  roadmap, documentation links, license. The fourteen repeated
  "What's new in v0.x" sections (v0.9.8.6 → v0.11.5) moved out —
  release history lives here. The stale repository-structure line
  that said `VERSION 0.9.11` is fixed (generic reference to the
  version source of truth).
- ROADMAP rewritten as a forward phase ladder (baseline, v0.12.0,
  Phases 1–6 matching the R&D phase order, testing/evidence
  standard, release acceptance, compact historical milestone table);
  the old per-release status narratives and duplicated
  implementation detail are removed (facts preserved here and in
  docs/); no future item is marked completed.
- This changelog keeps release facts; older entries compacted per
  the header note.

### Version bump + stale-consistency cleanup

- Version metadata moved to 0.12.0 in: `VERSION`,
  `internal/version/version.go` (default + comment),
  `frontend/package.json`, `frontend/package-lock.json`,
  `build/winres.json` (file/product version + manifest identity).
  Historical references (v0.11.4/v0.11.5/v0.10.4 and older, core
  pins, fixture versions) are untouched — classified by a repo-wide
  audit, only current-release surfaces updated.
- `engine/tunnel/tunnel.go` package comment repaired: it still said
  "TUN mode: DISABLED in this release" (the pre-v0.11.3 state);
  it now describes the current sing-box dataplane backend and
  preserves the v0.9.8.6 removal rationale as history. Comment-only
  change — no code behavior touched.
- sing-box upstream re-checked: upstream stable is 1.14.2 (also
  1.15.0-alpha.x prereleases); FreeIran intentionally retains the
  verified 1.14.1 pin — no digest fabricated, no half-upgrade, no
  dataplane change for documentation freshness (see
  docs/development.md core-pin policy).

### Verification scope (v0.12.0, explicit)

Documentation/version/comment changes only. Verified locally:
`git diff --check` (clean), full-tree `go build ./...` + `go vet` of
the touched Go surfaces (internal/version, engine/tunnel) + the
version-surface unit tests, frontend dependency install (`npm ci`)
+ typecheck + unit tests + production `build:embed`, a repo-wide
version/stale-text audit, a documentation link/path audit and a
clean-package audit of the source archive. Remote Actions were NOT
re-run (no push, no PR, no release — per the release rules); the
next maintainer push produces that evidence. The Windows TUN
physical runtime remains NOT VERIFIED (unchanged; docs/tun.md).

NOT VERIFIED in v0.12.0: any runtime behavior change (there is none
— no functional code changed); post-change remote CI/Security
verdicts (arrive with the next push).

---

## v0.11.5 — Windows GUI launch fix, real GUI launch proof, metadata-free runtime log

Focused repair release; no features, no architecture replaced.
Evidence scope: full local Linux `go test` suite (engine/system/
internal), windows/amd64 `go vet` of the complete tree, the
windows/amd64 `-H=windowsgui` desktop build with release stamps,
PE-subsystem check (WINDOWS_GUI) via debug/pe, frontend typecheck +
unit tests + production `build:embed`, binding regeneration with the
pinned wails3 v3.0.0-beta.19 CLI (twice, byte-identical, binding
contract tests green). Remote Actions NOT re-run.

- **GUI startup defect — root-caused and fixed (the headline).** The
  v0.11.4-ci binary booted the engine (application_start →
  store_open → manager_init → application_ready) but never showed a
  window. Root cause verified against the pinned wails
  v3.0.0-beta.19 source: the pre-Run initial tray reconcile
  dispatched through `application.InvokeAsync`, whose first step
  dereferences Wails' platform-app handle — assigned only inside
  `Run()` — so the panic killed the process on the main goroutine
  after the engine logs and before any window existed (windowsgui
  binaries have no console; no visible trace). Contributing
  fragility: a beta.19 pre-Run window is built without
  `WS_VISIBLE`, pre-Run `Show()` is a no-op, and visibility
  otherwise depended on WebView2 navigation completing. Fix: the ONE
  main window is created and shown from the `ApplicationStarted`
  event (deterministic create → show → hook → tray sequence); all
  v0.11.3 tray semantics unchanged; no second window, no sleeps, no
  headless downgrade.
- **Real Windows GUI launch proof.** New windows-only
  `TestWindowsGUILaunchProof` launches the ACTUAL built FreeIran.exe
  (not `--smoke-test`) in an isolated `FREEIRAN_HOME` workspace and
  FAILS unless a visible top-level window owned by the launched PID
  is observed natively (user32 EnumWindows + IsWindowVisible +
  GetWindowThreadProcessId) within a bounded timeout; distinguishes
  "process alive" from "GUI visible"; dumps window inventory/output/
  runtime-log tail on failure; always terminates by exact PID
  (tree force-kill). Wired into the CI Windows job and the release
  workflow after the real desktop executable is built; the release
  install rehearsal runs it against the INSTALLED executable.
- **Runtime log is metadata-free.** `ts` removed from the Entry
  schema and every UI/copy surface; no `commit` field and no
  Go-toolchain metadata; `seq` remains the ordering/paging
  mechanism; `application_start` compact ("FreeIran v0.11.5
  starting"); user-facing version renders exactly `v0.11.5`
  everywhere through `version.String()`; copied diagnostics carry
  the per-session seq prefix; developer-only provenance (commit, Go
  version) stays on the Developer Info surface. Regressions assert
  the raw JSON shape.
- **Wails warning bridge.** Wails-internal warnings/errors
  (WebView2 probing, navigation, environment) land in the runtime
  log (subsystem `wails`) through an slog bridge + ErrorHandler,
  with a deliberate Warn floor so Wails' own toolchain/vcs startup
  records can never reintroduce metadata.
- **Version/binding cleanup.** All version surfaces moved to 0.11.5;
  bindings regenerated with the pinned wails3 CLI (closing
  PRE-EXISTING v0.11.4 drift: OpenShellAtDataDir, CoreMihomo,
  TUNSnapshot, removed Entry timestamp).
- **sing-box drift check.** Upstream stable was 1.14.2; the verified
  1.14.1 pin retained deliberately (no unverified checksum, no
  half-upgrade); the Windows TUN physical runtime remained NOT
  VERIFIED (docs/tun.md).

NOT VERIFIED: the GUI launch proof's live verdict in the authoring
environment (executes in CI/release); post-release remote Actions.

---

## v0.11.4 — security repair, Windows CI hardening, TUN correctness hardening, documentation alignment

Repair + hardening release; no features, no architecture replaced.
Evidence scope: full Linux Go suite, windows/amd64 vet + build,
generated-config verification against the real pinned 1.14.1 binary,
Gitleaks reproduction with the exact CI scanner version, live-run
diagnosis of the Windows CI failure.

- **Security root-cause fix (the headline).** The v0.11.3 Security
  run's secret-scan failure (a committed WireGuard-shaped test
  literal in `engine/core/singbox/tun_test.go`) fixed at the ROOT:
  fixture material derived deterministically at runtime — no
  allowlist added, no rule weakened, no path excluded — plus a
  package-level regression test failing during ordinary `go test` if
  any committed key-shaped literal returns to the singbox test
  surface. docs/security.md describes the ACTUAL scan semantics.
- **TUN correctness hardening (three defects closed):** address
  selection FAILS CLOSED when every IPv4 candidate collides (no
  silent overlapping-range fallback); activation identity requires
  the EXACT session adapter (recorded name AND expected address on
  that same interface); "real traffic" proof strengthened with
  native read-only route observation (Windows IP Helper forwarding
  table: the TUN must OWN the covering IPv4 routes) plus the
  upstream route-loop check (no sing-box TCP socket sourced from the
  TUN address; positive physical-side pinning when TCP-observable;
  UDP-family outbounds honestly reported "not observable"). No
  netsh/route.exe/PowerShell anywhere; observation is lazy,
  bounded, fail-closed syscalls.
- **Windows CI hardening.** The v0.11.3 Windows job failed with a
  hosted-runner communication loss (47m vs v0.11.2's 3m07s);
  diagnosed from live run evidence; hardened with bounded
  `timeout-minutes: 60` and ZERO coverage loss.
- **sing-box 1.14.0 → 1.14.1 alignment.** CI real-core job + docs +
  evidence aligned; SHA-256 computed from the OFFICIAL upstream
  release asset; new real-binary test passes the complete TUN
  document through `sing-box check` of the pinned binary; the full
  existing real-binary smoke suite passes against 1.14.1.
- **Tray persistence regression checks + documentation truth
  repairs** (corrected Wintun packaging model — the official
  wintun.dll embedded in the sing-box binary via sing-tun v0.9.3,
  loaded from memory; honest evidence ladder; corrected scan
  semantics).

NOT VERIFIED: end-to-end Windows TUN runtime on a physical elevated
host (documented as such in docs/tun.md); post-release remote
Actions.

---

## v0.11.3 — functional Windows TUN (sing-box dataplane), tray ON/OFF setting, Security workflow repairs

Capability + repair release. Evidence scope: full Linux Go suite,
windows/amd64 cross-build + vet, frontend typecheck + tests +
production build, `sing-box check` of the generated TUN document
against the pinned real binary.

- **Functional Windows TUN (the headline).** TUN is no longer the
  v0.9.8.6 honest refusal: the managed, digest-verified sing-box
  core IS the TUN dataplane (native sing-box tun inbound,
  Wintun-backed). Transactional, OBSERVED activation: elevation
  check → verified core → compatibility validation → collision-free
  addressing from the live interface table → launch through the
  existing supervisor → readiness → adapter observed → DNS hijack
  (answered by sing-box's resolver over the proxy; system adapter
  DNS never mutated) → real no-explicit-proxy tunneled request
  verified → durable session marker → Active. Transactional disable
  with honest residual reporting; crash recovery via boot-time stale
  report inspecting only FreeIran's own recorded adapter. No new
  supervisor/queue/downloader. Full design: docs/tun.md.
- **Security workflow repairs (both push failures):** historical
  synthetic-fixture commit allowlist in `.gitleaks.toml`
  (commit-scoped, default rules fully active) and the reviewed
  `system/open_shell.go` dangerous-pattern exception with the
  contract inline (docs/security.md).
- **System Tray ON/OFF as a persistent setting** (`tray_enabled`,
  default ON, through the ONE settings store; native checkbox;
  setting-aware close-to-tray; recoverable from Settings).
- Everything from v0.11.2 preserved.

NOT VERIFIED: end-to-end Windows TUN runtime on a physical elevated
host (requires an elevated Windows host; documented in docs/tun.md).

---

## v0.11.2 — Mihomo managed core, internal config tabs, native Windows tray, honest tunnel diagnostics

Feature + repair release. Evidence scope: full Go suite (Linux) +
windows/amd64 cross-build + vet + the frontend battery, all executed
before packaging.

- **Mihomo as a coremgr-managed core** (download/health/UI; same
  verified pipeline, `.gz` extraction added); the connection-engine
  adapter deliberately out of scope — Mihomo is NOT advertised as a
  runnable backend until that adapter ships (asserted by test).
- **Configurations All scope + internal configuration tabs** keyed by
  stable configuration ID (open/focus/close/close others/close
  all/return to All).
- **Honest tunnel diagnostics** (Direct → "No tunnel selected";
  Tunnel path inactive → "Tunnel unavailable: no active tunnel" —
  never silently reported as tunneled).
- **Native Windows taskbar + system tray** (menu, show/focus
  restore, close-to-tray, teardown ordering).
- **Tor/Psiphon verification hardening; safe "Open shell here"**
  (exact, justified scanner exception); Diagnostics numerical
  spacing; queue/memory/logging safeguards preserved.

---

## v0.11.0 — compact Windows CI (two-layer proof), failure classification, transport agility, ECH foundation

CI-architecture + resilience-foundation release. Evidence scope:
full Linux suite including `-race`, frontend battery, real-core
verification with the three pinned cores (including the new ECH
shapes), security battery, windows/amd64 cross-build +
PE-subsystem verification, Windows compile-surface proof.

- **Windows CI redesigned to a two-layer proof** (local
  windows/amd64 vet+build from Linux + the Windows-native job)
  after measuring that the full hosted matrix duplicated
  platform-neutral execution; every Windows-specific behavior still
  executes on Windows.
- **Nine-class failure classification** (dns/tcp/tls/handshake/
  listener/verify/reset/timeout/transport) derived from observed
  failed-facet shapes only; stored per configuration and per
  history; repeated protocol-specific streaks demote candidates in
  automatic selection.
- **Route freshness / evidence tuple** (rank must not outrun the
  evidence that produced it).
- **Transport agility — preference, not a second failover engine.**
- **ECH support, schema-verified, sing-box-only** (four dedicated
  fields; evidence table of accept/reject shapes against the pinned
  binaries; honestly scoped: check/startup-level acceptance, NOT
  live ECH negotiation — docs/protocols.md).
- Machine-generated bindings restored (hand-maintained layer
  removed); bounded test admission; compact Normal logging; dense
  Configurations table; Black/White/Red icon; Android strategy
  documented as a plan.

---

## v0.10.5 — comment-aware security scanning, discovery route-trust policy

Security-tooling correctness + trust-policy gap fix. The raw-text
suspicious-pattern scanner (which could not distinguish documentation
from executable code) was replaced with a tokenizing, comment-aware
scanner — same detection intent, fewer false classes; discovery gained
an explicit route-trust boundary (source content routing policy).
Evidence: full local suites + the security battery; no scan rule
weakened.

## v0.10.4 — TUIC/Hysteria semantic corrections, WinINet bypass-grammar fix, documented query order

Protocol-semantics correction release against the pinned sing-box
binary: TUIC `udp_relay_mode` domain corrected to native | quic (the
prior "quadratic" was an invented value); Hysteria2 obfs domain
(salamander | gecko) and Hysteria v1 obfs (the v1 password string)
corrected; WireGuard endpoint semantics pinned; WinINet bypass-list
grammar fixed; the URL-test query order documented. All variants
re-verified through the real pinned binary (`check` + startup +
listener readiness).

## v0.10.3 — Windows WinINet ABI/semantics repair, protocol data-model truth pass, secret-free test material

Correctness release: v0.10.2 Windows failures repaired at root cause
(wrong test expectation, WinINet fidelity gap, cross-process test
race); TUIC/Hysteria/WireGuard protocol details moved into their OWN
semantic fields (replacing the v0.10.2 Network-slot overloading that
broke capability matching); test material made secret-free
(deterministically derived fixtures — the discipline that later
became the v0.11.4 root fix).

## v0.10.2 — Windows system-proxy repair, transactional ownership, personal import, real QUIC/WireGuard runtime

The v0.10.1 Windows regression fixed AT ROOT CAUSE (a broken WinINet
ABI that made every system-proxy activation fail on Windows); the
system-proxy ownership lifecycle made fully transactional; first-class
personal configuration import (URL lists, base64 subscriptions,
v2ray-style JSON, WireGuard INI — see docs/protocols.md); REAL runtime
support for Hysteria2, TUIC, WireGuard and Hysteria v1 through
sing-box (real-binary smoke evidence in docs/protocols.md).

## v0.10.1 — crash-safe system proxy, Windows CI survivability, WHITE/BLACK/RED theme

Trust-boundary release: system-proxy ownership durable across crashes
(durable marker + boot-time stale report — the pattern TUN later
adopted); the Windows CI failure class that made runs un-diagnosable
addressed at the workflow level; visual system reworked into the
WHITE/BLACK/RED identity.

## v0.9.15 — deep fixes: Tor acquisition root cause, one testing path, incremental UI, shared menu placement

Deep-fix release from a full acquisition/testing/UI audit: Tor
acquisition root cause repaired (not retried around); one testing
path (no duplicate admission paths); incremental UI updates; shared
menu placement. No new subsystem.

## v0.9.14 — idempotent local discovery, artifact reuse, full v0.9.13 closure

"Do the work once" as an architectural invariant: one discovery
authority, one reuse decision model, precise invalidation; version
consistency closed (winres had kept a stale 0.9.12 PE resource while
VERSION said 0.9.13 — the class of drift v0.12.0 audits for).

## v0.9.13 — clean runtime logging, configuration surface redesign, update-size transparency

Product-quality update: exactly ONE producer of the successful
`application_start` record; configuration surface redesigned;
update-size transparency (download sizes surfaced honestly).

## v0.9.12 — provider lifecycle monotonicity, binding-contract verification, persistence hardening

Full engineering closure of v0.9.11: the provider restart race
(`restart state = "starting"`) fixed at ROOT CAUSE; the same
monotonic-state invariant enforced across every asynchronous event
source in the provider layer; Wails binding-contract verification
introduced (the mechanism that later caught binding drift in
v0.11.5); persistence hardening.

## v0.9.11 — Windows lifecycle closure, host-independent archive security, Connection Profiles

Windows test-oracle root cause closed (CI run evidence); archive
security made host-independent (no toolchain-dependent extraction
behavior); Connection Profiles (first P2 roadmap feature). Validation
bounded by the actually-executed matrix.

## v0.9.10 — connection-lifetime architecture repair + beginner-first product release

The connection-lifecycle root-cause release: lifecycle proofs
asserted by the repository's own test matrix (unit + -race + native
+ fake-core lifecycle batteries + frontend battery + Windows
cross-compile + clean-room build); beginner-first product surface.

## v0.9.9 — core engine execution, runtime performance and deep functional upgrade

Core-engine/runtime release: execution and correctness upgrades
across the core pipeline, verified by unit + -race + native + real
protocol-core smoke + clean-room build.

## v0.9.8.8 — deep cleanup, stable filenames and repository hygiene

The TS2393 duplicate-worker root cause (CI run 35519469195) closed;
final canonical embedded-asset inventory (`index.html`,
`assets/index.js`, `assets/index.css`, `assets/export-worker.js` —
stale hashed/legacy files removed); repository hygiene. (Entry
compacted from the original README-referencing note in v0.12.0.)

## v0.9.8.7 — determinism and responsiveness release

CI recovery, event-driven UI synchronization, stable embedded asset
filenames, truthful Wails bindings (v-prefix normalization that
un-skipped the Windows suite), verified lifecycle/security
regressions.

## v0.9.8.6 — reliability, security and hygiene release

Deterministic Windows session teardown; session generation guards;
the route-trust boundary; **TUN disabled for cause** (the
non-transactional Wintun backend: inverted route tracking, DNS
"restore" that wrote DHCP, unverifiable curl/PowerShell Wintun
acquisition, unbounded extraction — removed rather than fixed; the
unique removal rationale is preserved historically in
`engine/tunnel/tun.go` comments and docs/tun.md); executable digest
enforcement; bounded archive extraction; explicit HTTP proxy policy;
pinned Wails toolchain contract.

## v0.9.8.4 — connection-state integrity + Logging Profiles (P1)

The v0.9.8.3 frontend CI failure resolved at its root (a stale
connection-state contract in tests); one asynchronous-operation
policy protecting the connection store; Logging Profiles shipped.

## v0.9.8.3 — Windows-CI correctness and process-supervision completion

The two v0.9.8.1 Windows failures closed at root causes; provider
subprocess unification completed; every claim backed by
actually-executed verification.

## v0.9.8.1 — latency representation fix, Tor/Psiphon providers, Internet tools

The latency measurement representation fixed at root (the Windows CI
failure — see docs/latency.md); Tor and Psiphon became first-class
providers on ONE shared managed-binary pipeline
(docs/providers.md); the user-triggered Internet-tools engine added
(engine/netcheck).

## v0.9.8 — Quick Connect UX + visual professionalization

A new Quick Connect home surface built entirely on the existing
connection engine; full-application visual professionalization (one
design system, no per-page hacks).

## v0.9.7 — structured logging, session identity, memory and discovery upgrades

Deep runtime/UI/discovery/memory upgrade from measured bulk-testing
behavior (thousands of queued configurations, many temporary core
processes, repetitive log events): structured logging with session
identity; adaptive memory pressure; discovery scaling.

## v0.9.6 — discovery → testing → ranking → connection rebuilt on real measurements

Complete engineering upgrade: the v0.9.5 Windows regression
root-caused and fixed at the exact defect site; the
discovery → testing → ranking → connection chain rebuilt around real
measurements with provenance discipline (multi-level discovery,
ping/URL test modes, measured ranking, verified connections +
controlled racing — docs/discovery.md, docs/latency.md).

## v0.9.5 — repository integrity

Repository made to match what v0.9.4 documented; stale artifacts
across the 0.9.1–0.9.4 packaging sessions removed; the v0.9.4
regression actually fixed (the httpx Windows finalization failure);
full verification matrix re-run end to end.

## v0.9.4 — startup lifecycle, release pipeline, version gate

Unified boot lifecycle (`boot → workspace_ready →
store_metadata_ready → services_ready`) with visible progress;
release packaging (Windows installer, release checksums); the
version-consistency gate (CI refuses when tag/VERSION disagree).

## v0.9.3 — the autonomous connection engine

FreeIran behaves like an automatic connectivity engine rather than a
configuration manager: Connect selects the best viable candidate from
real test history, validates it, chooses the backend and verifies the
result — the foundation the current connection engine still uses.

## v0.9.1 — configuration workspace fixes

Structural layout bug fixed (the per-row Test button wrapped onto an
invisible grid row); the testing workspace made readable.

## v0.9.0 — core loading fixed end to end

A managed core is never "installed" merely because a file exists: the
full pipeline `discover → verify → version → config validation →
launch → readiness → health → usable` runs before the UI reports
anything.

## v0.8.0 — Windows process supervision repaired and hardened

The Win32 job-object root cause (`SetInformationJobObject` BOOL
return-value protocol bug — the v0.7.0 CI failure) fixed; process
supervision hardened (kill-on-close job objects, CREATE_NO_WINDOW);
adaptive memory controller (Memory Booster 2.0) and desktop service
wiring.

## v0.6.0 — usable Windows VPN/proxy client

Managed Core Manager (Xray/V2Ray/sing-box install/verify/update/
rollback), no-console process launch, bounded-worker Test Queue,
System Proxy (WinINet), Speed Booster, expanded sources with
metadata, capability-driven failover. (The TUN mode shipped here was
DISABLED in v0.9.8.6 for cause; TUN returned in v0.11.3 as the
sing-box native dataplane — docs/tun.md.)
