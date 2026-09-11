# FreeIran Replacement Manifest — v0.4.0

## Package

| Field | Value |
|-------|-------|
| Version | 0.4.0 |
| Base reference | commit `e6defe229718c6b5c55b0ed5a719048d83744d12` (v0.3.0) |
| Package | `FreeIran-v0.4.0.zip` — complete source repository replacement |
| Verified by | Clean extraction into a fresh directory; full build + test matrix re-run from the extracted tree (see "Verification") |
| Excluded from package | `.git`, `frontend/node_modules`, `frontend/dist` (build output), `native/build`, `.cores` (CI core installs), no secrets, no local runtime data |

## Objective

Turn the v0.3.0 protocol-agnostic configuration manager into a real
multi-core runtime system: three genuine protocol-core backends
(Xray, V2Ray, sing-box) behind one abstraction, deterministic
backend selection, a supervised process lifecycle, an explicit
connection state machine, core-based testing and real-binary CI
verification — without regressing the v0.3.0 data layer.

## Protocol-core changes

### New execution boundary (engine/core, rewritten)

- `Core` interface: `Name / Supports / Validate / BuildConfig / Start`
  — stateless, concurrency-safe, adapted to the existing codebase.
- `Capabilities`: declarative per-backend feature model (protocols,
  transports, securities, flows, TLS-mandatory protocols). Every
  support decision in the application flows through capability
  resolution — no protocol-specific if/else outside the adapters.
- `Registry`: priority-ordered registration, executable discovery via
  `system.CoreLocator` (managed `cores/` directory → PATH), availability
  states (available / missing / invalid), version reporting,
  background refresh.
- `Select`: deterministic, explainable backend resolution —
  compatible candidates ordered by user preference (only when
  compatible AND available) → registry priority → name; returns the
  chosen core, a human-readable reason and ordered fallbacks.
- `Instance`: explicit lifecycle states (`created → starting → running
  → stopping → stopped`; errors: `start_failed / crashed / unhealthy /
  timed_out`) — no ambiguous booleans. Shared launcher writes the
  0600 runtime config into a 0700 temp directory, spawns the core with
  redacting output capture and polls the local listener for readiness.
- `HealthReport`: process health (`process_alive`) separated from
  network health (`listener_ready`) with latency.
- `GenCache`: in-memory, TTL-bounded (5 min, 16 entries) generation
  cache keyed by fingerprint + backend; invalidated wholesale on
  binary-path/runtime-options changes; credential-bearing documents
  are never persisted and never cached indefinitely.
- `RedactLogText`: URL-userinfo pattern redaction
  (`vless://***@host:443`) on top of explicit secret-value redaction.

### Backends (all real implementations, no fakes)

| Backend | Dialect | Verified feature set |
|---------|---------|----------------------|
| `engine/core/v2ray` | V4 JSON (V2Fly v5 accepts via `v2ray run/test -c`) | vless, vmess, trojan, shadowsocks, socks, http; tcp, ws, grpc, http/h2, quic; tls (+uTLS fingerprint, ALPN); NO reality, NO flow (absent from V2Fly) |
| `engine/core/xray` | V4 JSON + Xray extensions (reuses the shared V4 generator) | all of the above minus plain quic/h2 (removed upstream in favour of XHTTP) plus reality, xtls-rprx-vision, xhttp |
| `engine/core/singbox` | native sing-box JSON | all listed protocols; tcp, ws, grpc, http, quic, httpupgrade; tls, reality + vision (utls); mixed inbound; trojan TLS-mandatory |

The Xray adapter deliberately reuses `v2ray.BuildV4Document` with
dialect options — the shared V4 lineage cannot drift, and the
capability matrix encodes the verified divergences.

### Connection manager (engine/connection, new)

Explicit state machine — `disconnected → selecting → preparing →
starting_core → waiting_for_ready → connected → disconnecting`, with
`connection_failed` — one active session, bounded fallback (max 3
attempts) with per-attempt reason recording, port stability across
reconnects, background health monitor with crash detection, and a
terminal `Shutdown` used by the application shutdown ordering
(disconnect session → stop core → clean temp configs → close store).

### Tester integration (engine/tester, new CoreProbe)

Config → candidate backend (capability-first, bounded fallback) →
validate → start temporary core → wait readiness → latency →
deterministic shutdown → result. Test instances can never leak:
teardown is deferred before the outcome is even computed.

### System layer changes

- `ProcessSpec` gained `Stdout`/`Stderr` writers (output capture with
  redaction; default `io.Discard`); both platform implementations
  rewritten to the simpler `cmd.Stdout = writer` form (exec owns the
  copy goroutines; `Wait` joins them).
- Version probing tries `--version`, `-version` and the `version`
  subcommand — the three cores genuinely disagree (v2ray v5 and
  sing-box reject `--version`).
- `WellKnownCores` now includes `v2ray` (was missing).
- `queryCoreVersion` moved to the platform-neutral file.

### Normalized model (engine/config)

New protocol-detail fields: `flow`, `encryption`, `alter_id`,
`header_type`, `alpn`, `spider_x`. **Fingerprint deliberately
unchanged** — record identity stays endpoint + credentials + transport
so every v0.2/v0.3 store remains valid without migration (see
docs/storage-format.md §9). New `redact.go`: `SecretFields`,
`Redacted()`, `DisplayURL()` (`vless://***@host:443 (tls,ws)`).

### Parser hardening (engine/parser)

- Captures flow/encryption/alpn/spider/header-type/alterId from URI
  forms and vmess JSON.
- Parses complete V2Ray/Xray client JSON shares
  (`{"outbounds":[...]}`) into normalized configurations — proxy
  outbounds only, routing helpers skipped, bounded at 64 outbounds.
- Input size guard (32 MiB) independent of the source-layer cap.
- Hostile-input fuzz test added (never panics).

### App integration (engine/app)

`ConnectionService` (bound to the UI): `Connect/ConnectConfig/
Disconnect/Reconnect/ConnectionState/Health/Backends/RefreshBackends/
ConfigDetails`. Backend views carry name, status, version, path,
priority, capability summary, notes and the verified reference
version + official source. `ConfigDetails` (§17 view) exposes
protocol/address/port/transport/security/compatible backends/latency/
last test/source/status plus credential PRESENCE flags — never values.
App lifecycle: registry refresh is background; `Shutdown` orders
scheduler stop → context cancel → session disconnect (core stop +
temp cleanup) → store close. `cmd/freeiran` registers the service and
emits `freeiran:connection` events alongside `freeiran:state`.

### Metrics (engine/metrics)

New counters: `core_selections`, `core_fallbacks`, `core_starts`,
`core_start_failures`, `core_crashes`, `avg_core_startup_ms` — wired
into the connection manager and surfaced in the diagnostics snapshot.

### Frontend

- Hand-written Wails bindings following the v0.3.0 precedent
  (FNV-32a of the fully-qualified `package.Service.Method`):
  `connectionservice.js`, `connection/models.js` (Snapshot, Attempt),
  `core/models.js` (HealthReport), extended `app/models.js`
  (BackendView, ConfigDetail).
- New **Connection page**: backend cards (status/version/summary +
  pinned reference), configuration picker, connect/disconnect/
  reconnect, live state machine display (core, version, state,
  configuration, latency, local endpoint) and the attempt history
  table with failure reasons.
- **Configurations page**: per-row details view with redaction —
  protocol, endpoint, transport, security, compatible backends,
  status/latency/last-test, source, credential presence flags.
- `connectionStore` subscribes to `freeiran:connection`; busy/error
  are the only local flags — state always comes from the backend.

### CI

- New `protocol-cores` job: installs the pinned v2ray v5.53.0, Xray
  v26.3.27 and sing-box v1.14.0 releases (official sources,
  SHA-256-verified) and runs the real-binary smoke suites — each
  core's own validator accepts every generated document AND a full
  startup/listener/shutdown cycle runs per protocol. No public proxy
  server is contacted; the Windows job now gates on it.
- Benchmark smoke extended with `engine/core` + `engine/core/v2ray`.
- The security workflow is unchanged (gitleaks + pinned dual-target
  govulncheck + vet + the suspicious-pattern gate); the new pure-Go
  protocol-core packages fall under the existing scan scopes with no
  new dependencies.

## Protocol-core versions (pinned, verified)

| Core | Version | Source | Verification |
|------|---------|--------|--------------|
| V2Ray | **5.53.0** | github.com/v2fly/v2ray-core | `v2ray test` accepts all 7 generated protocol docs; full startup cycles pass; REALITY absence confirmed against the binary; linux-amd64 SHA-256 `a7bc11ff…1f7f25` |
| Xray | **26.3.27** | github.com/XTLS/Xray-core | `xray run -test` accepts all 9 docs incl. REALITY+vision and XHTTP; plain quic/h2 rejected upstream (encoded in capabilities); SHA-256 `8255dd93…6845ed` |
| sing-box | **1.14.0** | github.com/SagerNet/sing-box | `sing-box check` accepts all 10 docs; startup cycles pass; SHA-256 `57b3da14…9bbd04` |

No "latest" resolution anywhere; pins live in `engine/core/versions.go`
and docs/development.md. No new Go module dependencies were added —
the entire protocol-core layer is standard library.

## Files added

```text
engine/core/capability.go        capability model + resolution
engine/core/registry.go          backend registry + discovery states
engine/core/selection.go         deterministic explainable selection
engine/core/health.go            listener probing + port reservation
engine/core/logs.go              bounded redacting log capture
engine/core/runconfig.go         temporary runtime-config lifecycle
engine/core/gencache.go          generation cache (memory-only, TTL)
engine/core/versions.go          pinned/verified core releases
engine/core/contract/contract.go shared backend contract suite
engine/core/contract/smoke.go    real-binary smoke harness
engine/core/testdata/fakecore/   process stand-in (config-driven port)
engine/core/v2ray/               V2Ray adapter + shared V4 generator
engine/core/xray/                Xray adapter
engine/core/singbox/             sing-box adapter
engine/connection/               connection manager + state machine
engine/config/redact.go          credential redaction surface
engine/app/connectionservice.go UI service surface
+ per-package tests and benchmarks (see "Tests executed")
frontend/.../connection/models.js, core/models.js, connectionservice.js
frontend/src/state/connectionStore.ts (+ test)
frontend/src/pages/Connection.tsx
```

## Files changed

`engine/core/core.go` (rewritten: Core interface, Instance,
launcher), `engine/core/core_test.go` (replaced by
registry/selection/lifecycle suites), `engine/config/config.go` (new
fields, fingerprint unchanged), `engine/parser/parser.go` (new field
capture, V2Ray JSON outbounds, size guard), `engine/metrics/metrics.go`
(core counters), `engine/app/app.go` (registry + connection wiring,
shutdown ordering), `engine/tester/` (CoreProbe),
`system/system.go`, `system/process_unix.go`,
`system/process_windows.go` (writers, version probe forms, v2ray
discovery), `cmd/freeiran/main.go` (service registration +
connection broadcaster), `.github/workflows/ci.yml` (protocol-cores
job, benchmark scope, 0.4.0-ci version), `frontend/src/App.tsx`, `frontend/src/pages/Configs.tsx`
(details view), `frontend/src/services/index.ts`,
`frontend/src/types/ui.ts`, `frontend/src/styles/index.css`,
`frontend/bindings/.../app/models.js`, `VERSION`,
`internal/version/version.go`, `frontend/package.json`, README + all
six docs, `.gitignore` (`.cores/`).

## Files removed

None. The v0.3.0 cleanup already removed the dead v0.1 systems; no
new obsolescence was created (the old protocol-keyed `core.Registry`
was replaced in place by the backend-keyed registry in the same
package).

## Storage changes

**None.** The chunked store, WAL, registry, index and migration are
byte-identical to v0.3.0. Fingerprints are unchanged by design (new
protocol-detail fields excluded from identity), so existing stores
open with zero migration. The full lifecycle/recovery matrix re-ran
as a regression gate (see "Tests executed").

## Performance changes

All v0.3.0 paths unchanged (store suite re-measured in CI). New
measured protocol-core paths (2 vCPU, go1.26.8): config generation
12–16 µs/doc (1.2 µs cached on reconnect), validation 236 ns,
capability check 37 ns, backend selection 3.1 µs, log redaction 5 µs
per write, generation-cache hit 70 ns, real core spawn-to-ready
~100–140 ms (core-process-bound). Full tables:
docs/performance.md §4b.

## CI changes

New `protocol-cores` job (pinned SHA-256-verified core installs +
real-binary smoke suites for all three adapters); Windows job gates
on it; benchmark smoke includes the core packages. No `|| true`, no
allowed failures, no `@latest` tooling.

## Security changes

- Credential redaction at every boundary (logs, errors, selection
  reasons, UI snapshots, details view) with tests asserting
  non-leakage against synthetic secrets.
- Temporary runtime configs: 0600 in 0700 temp dirs, removed after
  failed startups and on shutdown, process stopped before removal
  (Windows discipline), bounded-remove retries.
- Executable discovery restricted to controlled locations (managed
  cores dir + PATH); nothing from downloaded data is ever executed;
  user binaries never replaced.
- Managed-distribution architecture reserved (pins + docs) with the
  mandatory verification chain documented for future installers.

## Tests executed (from the packaged tree)

- `gofmt -l ./engine ./system ./cmd ./internal` — clean
- `go vet ./engine/... ./system/... ./internal/...` — clean
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./cmd/...` — clean
- `go test -count=1 ./engine/... ./system/... ./internal/...` — all
  pass (19 packages; includes the adapter contract suites, connection
  state machine, tester core probe, parser hardening incl. hostile
  input fuzz, app connection integration, and the complete v0.3.0
  store lifecycle matrix)
- `go test -race -count=1 ./engine/... ./system/... ./internal/...` —
  all pass
- Real-binary smoke suites (pinned cores, checksum-verified):
  `v2ray` — 7 protocol/transport combinations PASS;
  `xray` — 9 combinations incl. REALITY+vision and XHTTP PASS;
  `sing-box` — 10 combinations PASS. Each covers core-native config
  validation AND full startup → listener-ready → shutdown → cleanup.
- `make -C native test` (C++) — pass
- `CGO_ENABLED=1 go test -tags native_accel -count=1 ./engine/native` — pass
- Frontend: `npm ci`, `npm run typecheck`, `npm test` (12/12),
  `npm run build:embed`
- Windows desktop build: `GOOS=windows GOARCH=amd64 CGO_ENABLED=0
  go build` — passes
- Benchmarks: 14 protocol-core/store-adjacent benchmarks plus the
  v0.3.0 suite re-run

## Known limitations

- Hysteria2/TUIC/WireGuard configurations parse, validate and store,
  but no v0.4 backend adapter executes them yet (sing-box adapter
  scope was cut to the six normalized protocols with verified
  smoke coverage; the capability model and contract suite make the
  v0.5 additions mechanical).
- REALITY configurations require Xray or sing-box; with only V2Ray
  installed, selection fails by design with an explainable error.
- Through-proxy end-to-end connectivity (an HTTP request through the
  tunnel) is intentionally NOT part of the health ladder: it would
  make CI and local testing depend on external servers. Health
  verifies process + listener; full connectivity is observed through
  real usage.
- The generation cache keys binary-path as the version invalidation
  proxy (backends do not learn their version independently of the
  registry); switching binaries invalidates, same-binary upgrades
  re-verify through the pinned capability matrix instead.
- Core process stop on Windows is grace-period + Kill (no SIGTERM);
  the cores do not implement Windows console control handlers.
- govulncheck's OSV feed evolves; the weekly scheduled scan may flag
  future stdlib advisories — by design.
- Chunk compression (FIRC flag bit 0) remains defined but unwritten
  (unchanged from v0.3.0).
