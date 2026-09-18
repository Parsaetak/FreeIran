# Provider Architecture (v0.9.8.1)

This document describes the unified provider architecture (§8–§14 of
the upgrade specification): ONE lifecycle contract for every
executable that can provide connectivity — the protocol cores (Xray,
V2Ray, sing-box) and the first-class engines Tor and Psiphon. The
implementation is `engine/provider`; the connection integration is
`engine/connection/provider.go`; the Auto mode and settings surface is
`engine/app/providerservice.go`.

Design rules:

- Providers and node CONFIGURATIONS are distinct concepts: node
  ranking stays in `engine/ranking`; provider lifecycle lives in
  `engine/provider`. Tor is never represented as a VLESS/VMess/Trojan
  node, and Psiphon is never an ordinary node protocol.
- No provider is forced into a single configuration format: each
  engine owns its own runtime configuration (torrc, tunnel-core JSON,
  core run-configs).
- ONE managed process supervisor (`system.ManagedProcess` — Windows
  job objects, no orphan processes) and ONE managed-binary pipeline
  shared by Tor and Psiphon, reusing `internal/httpx` exactly like
  `engine/coremgr`. No duplicate install machinery.
- Honest capability reporting: nothing is claimed that was not
  discovered from the real runtime (installed version, exposed proxy
  endpoints, bootstrap state).

## The Provider contract (`engine/provider/provider.go`)

| Method | Meaning |
|--------|---------|
| `Name()` / `Kind()` | identity and kind (`core` / `tor` / `psiphon`) |
| `Resolve(ctx)` | resolve the release to install/update towards |
| `Install(ctx)` / `Uninstall(ctx)` | managed binary lifecycle |
| `Start(ctx)` / `Stop(ctx)` | process lifecycle (serialised per provider by the `Manager`) |
| `State()` | `LifecycleState` (not_installed → installing → installed → … → running/stopped/failed) |
| `Info()` | version, source, license, notice, capabilities, last check |
| `Endpoints()` | local proxy endpoints (e.g. `socks5 127.0.0.1:<port>`) |
| `Health(ctx)` | measured health: process alive + a real handshake latency (never a process-alive claim alone) |
| `Cleanup(ctx)` | bounded log pruning; user data is never deleted |

The `Manager` registers providers, serialises Start/Stop per provider
(one managed instance each — §14), lists `Info` views for the UI and
exposes `Health`. Structured events (`provider_start`, `provider_exit`,
…) flow through `internal/logging` with error kinds.

### Kinds

| Kind | Providers | Notes |
|------|-----------|-------|
| `core` | xray, v2ray, sing-box | thin `CoreProviderAdapter` over the SAME `engine/coremgr` pipeline — installs/uninstalls delegate to the Managed Core Manager; no duplicate install machinery. Provider-level `Start` is honestly unsupported for cores: they run per node-configuration through the connection engine, not as long-lived standalone processes. |
| `tor` | tor | `engine/provider/tor.go` (below) |
| `psiphon` | psiphon | `engine/provider/psiphon.go` (below) |

## The shared managed-binary pipeline (`engine/provider/binary.go`)

```text
RESOLVE (metadata, official source, checksum authority)
   → DOWNLOAD  .part file via internal/httpx (resumable, stall-watched)
   → VERIFY    SHA-256 against the published checksum — MANDATORY:
               a release without a published checksum is REFUSED
   → UNPACK    tar.gz (tar-slip guarded)
   → VALIDATE  version probe against the release tag
   → SMOKE     one supervised launch that must start and stop cleanly
   → ACTIVATE  atomic rename staged → bin/; previous binary retained
               for rollback on failure
   → MANIFEST  manifest.json (version, source, checksum, dates, state)
```

Workspace layout:

```text
<workspace>/providers/<name>/
├── bin/            active executable
├── staging/        download + unpack area (transient)
├── manifest.json   install record
├── data/           runtime data (tor DataDirectory, psiphon DataRootDirectory)
├── cache/          runtime cache (tor CacheDirectory)
└── logs/           pruned (7 days / 16 files) by Cleanup
```

## Tor (`engine/provider/tor.go`, §8)

- **Source contract (verified live).** Official Tor Project
  distribution only:
  `https://dist.torproject.org/torbrowser/<ver>/tor-expert-bundle-<platform>-<ver>.tar.gz`
  with `sha256sums-signed-build.txt` as the checksum authority,
  fetched over TLS from the same host. No third-party mirrors, no
  scripts. The default channel is pinned to **15.0.20**; the
  "latest" channel resolves through the directory listing with alpha
  builds skipped.
- **Runtime configuration.** A torrc is generated per run:
  `SocksPort` on a reserved ephemeral port,
  `DataDirectory`/`CacheDirectory` inside the provider workspace,
  `Log notice stdout`. Nothing is written outside the workspace.
- **Bootstrap truth.** Progress is parsed from the REAL
  `Bootstrapped X% (Tag)` notice log lines Tor itself emits — never
  from timers. Readiness is also observed via the local SOCKS
  endpoint.
- **Bridges: user-provided ONLY.** Bridge lines are validated
  (transport + host:port + optional 40-hex fingerprint) and never
  hardcoded. Pluggable transports (obfs4, snowflake) run through
  user-configured plugin executable paths. WebTunnel is built into
  tor ≥ 0.4.8 and its availability is reported from the INSTALLED
  version only — never assumed.
- **Health.** Process alive AND a measured SOCKS handshake.
- **Licensing.** BSD-3-Clause (Tor Project); the attribution notice
  ("produced independently from the Tor® software…") is surfaced in
  the UI.
- **Honest limitation.** GPG verification of the checksum file itself
  is not performed. Checksums are fetched over TLS from the official
  host — the same authority model as the Tor Browser updater's
  initial bootstrap.

## Psiphon (`engine/provider/psiphon.go`, §9)

- **Channel policy.** The official tunnel-core console client channel
  (GitHub releases with published asset digests; default repository
  `Psiphon-Labs/psiphon-tunnel-core`). Channel status as of
  v0.9.8.1: the official releases currently publish only mobile
  client-library archives (Android/iOS) — no Windows console-client
  asset with a digest. The engine reports exactly that (honest
  unavailability) instead of weakening verification; should the
  project publish a digest-bearing console-client asset, managed
  installation starts working with no code change.
- **Refusal semantics (honest).** When the channel exposes no release
  assets with digests, `Resolve` reports unavailability and `Install`
  REFUSES — binaries are never executed unverified. No digest, no
  install; verification is never weakened to make installation
  easier.
- **User-provided binary.** The user can point FreeIran at a console
  client binary (`psiphon_user_binary`); it is validated and
  smoke-launched before adoption.
- **Runtime configuration.** A config JSON is generated per run:
  `LocalSocksProxyPort` + `LocalHttpProxyPort` on reserved ephemeral
  ports, `DataRootDirectory` inside the provider workspace. The
  user's extra config (`psiphon_extra_config`) is merged only after
  JSON validation, and engine-owned fields are never overridable.
- **Readiness truth.** Observed from the real runtime: both local
  proxy ports actually accepting connections; the client's tunnel
  output is reflected in bootstrap tags. Capabilities are reported
  ONLY when running.
- **Licensing.** Psiware license (Psiphon tunnel-core); attribution is
  surfaced in the UI and nothing is statically embedded into
  FreeIran's binary.

## Connection integration (`engine/connection/provider.go`, §11)

`Manager.ConnectProvider` runs provider routes through the SAME
lifecycle as configuration routes:

```text
select → start provider (waits for ready/bootstrap)
       → route via the provider's local SOCKS endpoint
       → VerifyTunnel (the SAME verification gate — never bypassed)
       → connected
       → the SAME monitor loop (provider health drives crash transitions)
       → disconnect stops the provider deterministically
```

`Reconnect` remembers the provider route and re-establishes it. The
session snapshot names the active provider and endpoint; exactly one
of configuration/provider route is active. `engine/connection/provider_test.go`
verifies the session contract end-to-end (§11), including
HTTP-through-provider via a real SOCKS relay.

## Auto mode (`engine/app/providerservice.go`, §12)

Auto mode selects between configurations and providers on EVIDENCE —
no hardcoded priority:

- installed availability gate (an uninstalled provider can never be
  chosen over an installed one),
- live health (measured handshake latency),
- last verified success freshness,
- measured latency,
- failure-streak stability,
- the config-side ranking composite for the Configurations route.

Choices are explainable like ranking: the selection returns the
ordered evidence and a human-readable reason. Mode values:
`auto | configs | tor | psiphon` (`provider_mode`).

## Process and memory safety (§14)

- One managed instance per provider: the `Manager` and the engines
  serialize Start/Stop; a second Start while running is rejected.
- Every provider process runs under `system.ManagedProcess` (Windows
  job objects with kill-on-close — no orphans on any exit path).
- A failed start stops the provider deterministically; no half-started
  process survives.
- `Cleanup` prunes provider logs (7 days / 16 files); user data is
  never deleted.
- The Internet-tools service bounds tool concurrency (3 tokens) —
  see docs/internet-tools.md.

## Settings reference

| Key | Meaning |
|-----|---------|
| `provider_mode` | `auto` / `configs` / `tor` / `psiphon` (Quick Connect selector) |
| `tor_bridge_lines` | user-provided bridge lines (validated: transport + host:port + optional 40-hex fingerprint) |
| `tor_transport_plugins` | transport name → user-provided client plugin executable (obfs4, snowflake) |
| `psiphon_extra_config` | advanced user JSON merged into the generated config (engine-owned fields not overridable) |
| `psiphon_user_binary` | optional user-provided console-client binary path |

All are non-sensitive preferences persisted under the workspace
config directory with 0600 permissions; bridge lines and plugin paths
are values the user themselves provided and are never logged.

## Testing strategy (`engine/provider/provider_test.go`)

- Deterministic stand-ins: `engine/provider/testdata/{faketor,fakepsiphon}`
  are built by the test harness in `TestMain` with the running Go
  toolchain — a missing fixture is a hard failure, never a silent
  skip (the fakecore discipline of docs/development.md).
- The full provider matrix — resolve, download (local httptest
  server), checksum verification (including refusal on digest
  mismatch), unpack, validate, install, start, bootstrap parsing,
  readiness, health, stop, uninstall — runs against the fakes; no
  test touches the live Tor network or Psiphon servers.
- HTTP-through-provider is exercised via a real SOCKS relay (the same
  approach as the v0.9.6 tunnel-verification tests), and the
  connection provider-session tests drive `ConnectProvider`
  end-to-end.
- Frontend: the provider store and provider-mode routing are covered
  by vitest (`frontend/src/state/providerStore.test.ts`,
  Quick Connect page tests).
