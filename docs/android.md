# Android Platform Strategy — Implementation-Ready Architecture Note

Status: **architecture note only (v0.11.0)**. No Android product exists,
no Android code is claimed, and nothing in the current tree is
Android-tested. This document exists so a future session starts from
an engineered plan and documented facts rather than from optimism.

The Go engine is OS-neutral by design, but "the engine compiles for
android" and "FreeIran runs on Android" are different claims. The gap
between them is enumerated below, honestly, with the requirements each
gap imposes.

## What transfers as-is (verified facts)

- `engine/config`, `engine/parser`, `engine/ranking`, `engine/tester`
  (pure-Go surface), `engine/store`, `engine/source`, `engine/errors`,
  `internal/*`: pure Go, no platform build tags, exercised on Linux
  and Windows. These packages expect to compile for `GOOS=android`
  unchanged — **expected, not yet verified**; the first Android task
  is proving it (`GOOS=android GOARCH=arm64 go build ./engine/...`).
- The managed-core model: download pinned cores with digest
  verification, stage, generate config, start, supervise, stop
  (`engine/coremgr`). The protocol adapters (`engine/core/*`) generate
  documents; sing-box publishes android-arm64 release binaries, which
  the core manager's install pipeline could pin exactly like the
  desktop ones.

## What does NOT transfer, and the real requirements

### 1. Platform tunnel boundary — Android has no WinINet

`engine/tunnel` is split by build tags today: `proxy_windows.go`
(WinINet, transactional ownership, crash recovery) and
`proxy_other.go` (honest "unsupported" backend). Android needs a
THIRD branch, and the only legitimate system-wide traffic path on
Android is a VPNService-based TUN — which is exactly the class of
implementation that failed the v0.9.8.6 desktop TUN review
(unverified rollback, unrecoverable states). Requirements:

- `engine/tunnel` gains an Android backend whose "enable/disable" is
  bound to the VPNService lifecycle (see §3), with the SAME
  transactional ownership contract as WinINet: durable state BEFORE
  activation, verified restoration, explicit residual errors,
  crash-marker recovery at boot.
- The backend must fail loudly when the VPN permission is missing or
  revoked — an "active" tunnel without the OS route is a lie the UI
  must never display.

### 2. UI separation — Wails does not run on Android

`cmd/freeiran` (Wails v3 desktop shell) and `frontend/` (React/TS) are
separate layers today, which is the only reason this is tractable.
Requirements:

- The frontend is rebuilt as a web UI inside an Android shell
  (Android WebView or a thin React Native/Compose host). The UI
  already speaks to the engine exclusively through Wails-generated
  bindings; on Android those bindings must be replaced by a bridge
  (for example an HTTP/JSON-RPC or WebSocket surface exported by the
  Go engine, or gomobile-bound methods). **No engine service may be
  rewritten for the UI** — the bridge is a transport, not a fork of
  the service layer.
- State synchronization stays event-driven
  (`internal/statepub`-style publishing over the bridge); the
  queue-driven incremental UI model transfers unchanged in concept.

### 3. Android VPNService integration — the hard part, in full

- **VpnService.Builder** requires `prepare()` consent (a system
  dialog) BEFORE the tunnel can establish: the connection flow gains
  a user-consent step that desktop flows do not have. Denial must
  surface as an explicit state, not a silent connect failure.
- The service runs in a dedicated Android process; the Go engine must
  run inside that process (gomobile bind or a subprocess). Splitting
  engine/UI across processes requires a defined IPC contract — the
  bridge from §2.
- File descriptors from `Builder.establish()` must be passed to the
  core (sing-box supports Android-specific TUN inbound via file
  descriptor). sing-box HAS first-class Android support upstream, but
  the integration (fd passing, DNS handling through the VPN,
  per-app routing) is native work — it does NOT fall out of the Go
  engine compiling.

### 4. Core distribution — pinned, digested, per-ABI

- The core manager's digest-mandatory install discipline applies
  unchanged; the pinned assets become android-arm64 (and armv7 if
  ever claimed). Official release binaries only.
- Android app-store packaging constraints (APK size, extracted
  native binaries) must be respected: cores ship inside the APK
  assets or are downloaded on demand with the existing
  digest-verified pipeline — on-demand matches the existing updater
  architecture more closely.

### 5. State synchronization + verification — unchanged standards

- The discover → test → rank → connect → verify → monitor → recover
  pipeline and its verification gates (Connected is never reported
  before external verification) apply verbatim. Quick Connect,
  recovery and the failure-evidence model (v0.11.0) are
  platform-neutral.
- Verification on Android must account for the VPN taking over ALL
  traffic: the verification probes then run inside the tunnel by
  construction, and the "direct" diagnostics ladder must be labeled
  accordingly (it measures through the VPN unless protected sockets
  are used).

### 6. Permissions — explicit, bounded, documented

Minimum set: `VpnService` (consent dialog), network state, and
foreground service (Android requires a persistent notification for
active VPNs — an honest UI surface). No location, no contacts, no
storage beyond app-private. The dangerous-process allowlist
discipline from security.yml applies to any future helper binaries.

## What must NOT happen (non-negotiables, carried from the desktop)

- The desktop TUN experiment stays EXPERIMENTAL/DISABLED
  (`engine/tunnel/tun_unavailable.go` documents the rollback/recovery
  defects that blocked it). An Android TUN must be a NEW
  transactional implementation that passes real rollback/recovery
  tests on real devices — never a re-enable of the old code.
- No stealth techniques whose correctness cannot be verified (no
  forged packets, no active-probe deception).
- No claim of Android support in any document or UI until the
  verification battery actually runs on an Android device/emulator
  CI leg.

## Suggested implementation ladder (each step independently verifiable)

1. `GOOS=android` compile proof for the pure-Go engine packages +
   CI leg (linux runner cross-compiles, no device needed).
2. Engine-in-a-process prototype: gomobile-bound (or bridged) engine
  running the existing Go test battery on an Android emulator
  (instrumentation host), no VPN yet.
3. VPNService skeleton: consent → establish → teardown with
  transactional state and crash-marker recovery proven by test.
4. sing-box Android TUN inbound wired through the fd; verification
  battery through the tunnel.
5. UI bridge + the existing React frontend on a device.
6. Distribution: pinned android-arm64 core assets through the
  existing digest-verified installer.

Each step ends with the same standard as this project's desktop
releases: claims limited to evidence actually executed.
