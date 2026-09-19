# FreeIran Roadmap

## FreeIran — Autonomous Local Connectivity Engine

FreeIran is a local Windows connectivity engine that discovers, tests, ranks, connects, monitors and recovers proxy/VPN connectivity through managed protocol cores and first-class providers.

The product pipeline is:

**discover → ingest → parse → deduplicate → test → score → select → connect → verify → monitor → recover → learn**

The application should feel like a mature desktop connectivity client rather than a collection of independent tools.

---

## Current Baseline

Current main:

* Version: `0.9.8.4`
* Platform focus: Windows x64
* Runtime: Go + Wails + React/TypeScript
* Core families: Xray, V2Ray, sing-box
* First-class providers: Tor, Psiphon
* Quick Connect: enabled (fresh-selection loop, verification-gated)
* Adaptive memory controller: enabled (evidence-gated growth)
* Managed workspace: enabled (application-folder-only)
* Managed provider/core installation: enabled
* Logging profiles: enabled (Normal / Detailed / Debug)

> Note: this section previously reported `0.9.8.2` with runtime
> findings from that era (unbounded queue-depth growth, readiness
> presented as success). Those findings described real defects that
> the v0.9.8.3 implementation already fixed; the baseline text lagged
> the repository and is corrected as of v0.9.8.4.

### v0.9.8.3 — completed historical work

The v0.9.8.3 implementation (shipped on `main` before this baseline
correction) completed the following roadmap items, verified against
the actual code rather than the stale notes above:

* Quick Connect fresh-selection loop — recent verified successes
  merged with the ranked set, deduplicated, shortlisted, fresh-tested
  within a bounded budget, re-ranked from fresh evidence, connected
  and verified (`engine/app/quickconnect.go`).
* Verified connection state — `connected_verified` is the only final
  success; the connection engine gates every attempt on an end-to-end
  Internet verification (`engine/connection/connection.go`,
  `verify.go`).
* Recovery re-ranking — automatic recovery reuses the exact
  fresh-selection loop, with its own exclusions and cooldowns
  (`engine/app/recoveryservice.go`).
* Configuration reorder — stable IDs and persisted user ordering
  (`engine/app/configorderservice.go`).
* Local proxy ports — user-selected SOCKS5/HTTP inbounds with
  preflight bind checks and honest conflict reporting.
* Application-folder workspace — everything lives in the application
  folder; migration from legacy trees (`docs/workspace.md`).
* Installer/uninstaller improvements — desktop shortcut, complete
  application-owned cleanup, bounded questions (`scripts/freeiran.iss`).
* Compact logging — level filter before formatting, one JSON
  serialization per emitted record, session/event identity only on
  correlated records (`internal/logging`).
* Adaptive memory improvements — queue-depth growth is evidence-gated
  with cooldowns and hysteresis; idle is a no-op (`engine/booster`).
* Tor installation improvements — first-class managed acquisition
  with checksum verification.
* Psiphon trust-boundary handling — honest acquisition state, explicit
  user action only.

The runtime findings listed in earlier revisions of this section
(items 7–10: unconditional queue-depth growth, readiness marked as
success, Quick Connect presenting unverified sessions) no longer hold
against the current implementation; the regression suites in
`engine/app`, `engine/connection`, `engine/booster` and the frontend
store contract tests pin this behaviour.

---

# 1. Product Principles

## 1.1 One application, one workspace

Everything FreeIran creates, stores, downloads, executes or manages must live inside the selected FreeIran application/workspace folder.

Target layout:

```text
FreeIran/
├── FreeIran.exe
├── uninstall.exe
├── config/
├── data/
├── cache/
├── logs/
├── cores/
│   ├── xray/
│   ├── v2ray/
│   └── sing-box/
├── providers/
│   ├── tor/
│   └── psiphon/
├── runtime/
├── docs/
└── deployment/
```

No normal application state should silently move to:

* `%APPDATA%`
* `%LOCALAPPDATA%`
* arbitrary `%TEMP%`
* another hidden application-data tree
* another working directory

Explicit operator overrides such as `FREEIRAN_HOME` may remain supported.

Temporary OS facilities may still be used where technically unavoidable, but FreeIran-owned persistent/runtime data must remain under the application workspace and be cleaned deterministically.

---

## 1.2 Core readiness is not connection success

The state machine must remain:

**select → prepare → start core/provider → wait ready → establish route → verify Internet → connected → monitor**

A local listener opening successfully is never enough to claim a verified connection.

The UI and logs must clearly distinguish:

* starting
* core/provider ready
* route established
* verification running
* verified usable
* failed
* recovering

---

## 1.3 Evidence beats assumptions

Every selection decision should be based on real observations:

* recent successful connections
* fresh test results
* measured latency
* stability
* recent failure streak
* protocol compatibility
* current core/provider health
* actual Internet verification

Never select a route because it is merely present in the database.

---

# 2. P0 — Fix Quick Connect and Connection Reliability

## 2.1 Fresh-test the best recent configurations before Quick Connect

Quick Connect must not simply trust a stale ranking snapshot.

Before connecting in **Configurations** or **Auto** mode:

1. Load the strongest recently successful configurations.
2. Load the strongest currently-ranked candidates.
3. Merge and deduplicate them.
4. Prefer configurations with:

   * recent verified success
   * recent measured latency
   * stable history
   * low recent failure streak
5. Test a bounded top set concurrently.
6. Record fresh results.
7. Recalculate ranking from the new evidence.
8. Attempt the highest-ranked currently usable candidate.
9. If the candidate fails during connection or verification, continue with the next freshly-ranked candidate.
10. Only after the recent shortlist is exhausted should the adaptive discovery path expand the search.

The default shortlist should be configurable, with a sensible bounded default such as the top 5–10 recent candidates.

Quick Connect should therefore behave as:

```text
Quick Connect
    ↓
load recent winners + current best
    ↓
deduplicate
    ↓
fresh bounded test
    ↓
re-rank
    ↓
connect
    ↓
verify Internet
    ↓
monitor
    ↓
recover/re-test/re-rank if needed
```

---

## 2.2 Persist "last working" evidence properly

Each configuration should maintain durable, bounded evidence such as:

* last verified success
* last failure
* success count
* consecutive failure count
* last measured latency
* latency measurement timestamp
* recent test timestamp
* protocol/core used
* verification result
* cooldown/penalty state

Do not treat source freshness as connection freshness.

A configuration that worked five seconds ago should have substantially different evidence from one that worked three days ago.

---

## 2.3 Recovery must reuse fresh ranking

Recovery should not simply select the next static candidate.

Recovery:

```text
failure
→ identify recent viable candidates
→ test bounded shortlist
→ update observations
→ re-rank
→ connect
→ verify
→ monitor
```

Repeatedly failing candidates must rapidly lose priority.

Recently verified working candidates should regain priority without permanently dominating fresh candidates.

---

## 2.4 Internet verification must become mandatory before success

A configuration/session should only become:

**CONNECTED / VERIFIED**

after an actual Internet verification succeeds through the active route.

Core readiness may be shown as a lower-level state but must never be confused with usable connectivity.

---

# 3. P0 — Tor and Psiphon Provider Installation

## 3.1 Tor installation must become a first-class working flow

The current Tor provider architecture is structurally correct but the end-to-end acquisition/install experience must be fixed.

Required flow:

```text
Check installed
→ resolve official release
→ determine OS/architecture
→ fetch authoritative checksum metadata
→ select correct asset
→ download
→ verify SHA-256
→ verify published signature where supported
→ unpack safely
→ locate executable
→ validate executable
→ smoke test
→ activate atomically
→ write manifest
→ report Installed
```

The resolver must support current stable official releases rather than depending forever on an obsolete hardcoded version.

The official Tor distribution currently publishes Windows x86_64 expert bundles and signed checksum metadata; the implementation should use that official publication model.

Required UI:

* Check
* Install
* Update
* Repair
* Start
* Stop
* Uninstall
* Refresh
* View verification details

Show real progress:

```text
Resolving
Downloading
Verifying
Unpacking
Validating
Smoke testing
Activating
Ready
```

Never display success before the final managed executable has passed validation.

---

## 3.2 Psiphon must have a clearly honest acquisition state

Psiphon remains a first-class provider.

However, FreeIran must not weaken executable trust requirements just to make automatic installation appear to work.

The current official Psiphon tunnel-core documentation points to `psiphon-tunnel-core-binaries` for official binaries, but that repository currently has no GitHub releases and is not a checksum-bearing release channel.

Therefore implement two explicit states:

### Managed install available

Only when FreeIran can obtain:

* official source
* deterministic platform/architecture asset
* trusted checksum/signature authority

Then use the normal managed installation pipeline.

### Managed install unavailable

Show:

> Automatic Psiphon installation is currently unavailable because the official binary source does not publish a trusted release/checksum channel for this platform.

Provide:

* Select Psiphon executable
* Validate
* Smoke test
* Copy into managed storage
* Activate
* Run

Never:

* download a moving branch blindly
* execute an unverified executable
* silently substitute a third-party build

Also investigate whether an official Psiphon Windows distribution can provide a future deterministic, verifiable source without changing Psiphon into an ordinary node protocol. The official Psiphon Windows project currently publishes signed releases, so this should be researched as a separate provider-distribution option rather than assuming tunnel-core binaries are the only possible acquisition route.

---

# 4. P0 — Core Manager Redesign

The core manager should behave like a mature desktop core manager.

Supported cores:

* Xray
* V2Ray
* sing-box

For each core provide one consistent lifecycle:

```text
Unknown
→ Checking
→ Available
→ Installing
→ Validating
→ Ready
→ Update available
→ Repairing
→ Broken
→ Removed
```

## 4.1 Core catalog

Maintain a unified catalog containing:

* core name
* platform
* architecture
* latest known version
* installed version
* release source
* download asset
* checksum
* release date
* executable path
* health status

No hardcoded assumptions about the installed binary.

---

## 4.2 One-click core installation

A user should be able to:

**Cores → Xray → Install**

and receive:

```text
Resolve release
→ download
→ checksum verification
→ unpack
→ executable validation
→ config validation
→ smoke launch
→ shutdown test
→ activate
→ Ready
```

No manual folder copying should be required.

---

## 4.3 Update and repair

Each core needs:

* Check for update
* Update
* Reinstall
* Repair
* Roll back where a previous verified version exists
* Remove

A broken installation should be repairable without deleting unrelated configuration data.

---

## 4.4 Installation diagnostics

When installation fails, show the actual failing stage:

```text
Resolve failed
Download failed
Checksum failed
Archive invalid
Executable missing
Executable invalid
Config validation failed
Smoke test failed
Activation failed
```

Do not reduce all installation failures to:

> Installation failed.

---

# 5. P0 — Quick Connect Provider UX

Quick Connect must become the single reliable entry point.

### Configurations

```text
fresh-test recent winners
→ re-rank
→ connect
→ verify
```

### Tor

```text
if installed:
    start → bootstrap → verify Internet → connected
else:
    show Install Tor action
```

### Psiphon

```text
if installed:
    start → local proxy ready → verify Internet → connected
else:
    show valid acquisition/install path
```

### Auto

Auto must compare real evidence across:

* configurations
* Tor
* Psiphon

It must not silently install software.

Auto may recommend an unavailable provider, but the UI must clearly explain:

```text
Available
Installed
Not installed
Unavailable
Failed
```

---

# 6. P0 — Configuration Ordering Bug

The configuration ordering/reordering implementation must be corrected so that changing order actually updates the complete ordered collection.

Requirements:

* Reordering one item changes the entire persisted order.
* The UI immediately reflects the new order.
* Reloading the application preserves the same order.
* Sorting/filtering must not mutate the underlying order accidentally.
* Virtualized rows must use stable configuration IDs as keys.
* Reordering must operate on IDs/records, never array indexes alone.
* Concurrent refreshes must not overwrite a newer user reorder.
* Persisted ordering must be deterministic.
* Multi-selection or grouped configuration tabs must never reorder only the first/top item unintentionally.

Add regression tests for:

* first → last
* last → first
* middle → first
* middle → last
* repeated moves
* filtered view reorder
* refresh after reorder
* application restart
* large configuration sets

---

# 7. P0 — User-Controlled Local Proxy Ports

Add a dedicated proxy-port configuration system similar to mature proxy clients.

Settings:

```text
SOCKS5 port
HTTP proxy port
```

The user may choose the preferred port.

Example:

```text
SOCKS5: 10808
HTTP:   10809
```

## Port validation

Before starting a session:

1. Validate range.
2. Validate loopback binding.
3. Check IPv4 availability.
4. Check IPv6 availability where applicable.
5. Detect conflicts.
6. Ensure the port is actually bindable.
7. Start the core/provider.
8. Verify that the expected listener is actually accepting.
9. Confirm the final endpoint matches the requested configuration.

Avoid a check-then-use race by performing the preflight immediately before launch and always validating the actual listener afterward.

If the preferred port is occupied:

```text
Port 10808 is unavailable.
Choose another port or allow automatic selection.
```

Do not silently change the user's preferred port without reporting it.

Optional future mode:

```text
Preferred port
Automatic fallback
```

---

# 8. P0 — Logging and Runtime Performance

The current logging format is unnecessarily expensive.

The default runtime log should contain only useful information:

```text
timestamp
level
subsystem
event
message
important fields only
```

Do NOT write `session_id` and `event_id` on every single record.

## Correlation model

Use correlation identifiers only when necessary:

* startup session
* connection/recovery episode
* explicit debugging
* error investigation

They should not be duplicated onto every routine record.

---

## 8.1 Remove repetitive policy logs

The current log sequence:

```text
queue 35000 → 36000
queue 36000 → 37000
queue 37000 → 38000
...
```

is not useful telemetry.

Normal adaptive operation should remain silent.

Log only:

* meaningful policy state change
* pressure transition
* significant concurrency change
* critical memory action
* failure
* recovery

---

## 8.2 Fix the underlying queue-depth behavior

The current booster increases queue depth by 1,000 every normal tick until the ceiling.

That is directly reflected in the supplied runtime log.

Change the behavior so queue depth grows only when there is evidence that additional queue capacity is beneficial.

Use:

```text
backlog
throughput
active workers
CPU pressure
memory pressure
queue utilization
```

The queue must stop growing when additional capacity provides no measurable benefit.

Add:

* hysteresis
* bounded growth
* cooldown
* utilization threshold
* memory-aware ceiling
* no-op stability period

Do not optimize merely by lowering the log volume; fix the unnecessary adaptation itself.

---

# 9. P0 — Clean Application-Local Installation

Redesign the Windows installer around a writable application directory.

The installed deployment should behave like:

```text
Chosen FreeIran installation folder
        ↓
FreeIran.exe
        ↓
all application state
        ↓
all cores
        ↓
all providers
        ↓
all logs
        ↓
all runtime/cache/data
```

Do not relocate the workspace to `%AppData%\FreeIran`.

## Installer requirements

### Installation

* standard Windows installer
* architecture-aware
* safe replacement of a running instance
* version metadata
* application icon
* optional desktop shortcut
* Start Menu shortcut
* clear installation folder selection
* writable-folder validation
* no hidden second workspace

### Default folder

Use a user-writable default installation directory.

The application folder itself becomes the workspace root.

### Uninstall

Provide a real uninstaller inside the application installation folder.

Uninstall must:

1. stop FreeIran
2. stop managed cores
3. stop Tor/Psiphon
4. clean managed runtime processes
5. remove the application folder
6. remove shortcuts
7. remove installer-generated metadata
8. leave no hidden FreeIran runtime tree elsewhere

Before destruction, provide a clear confirmation of what will be removed.

Optionally provide:

```text
Keep user data
Remove everything
Export data first
```

but the product must not silently leave a second hidden workspace.

---

# 10. P1 — Connection Testing Engine Upgrade

Create one canonical testing strategy for configuration connectivity.

Testing stages:

```text
cheap reachability
→ protocol handshake
→ measured latency
→ tunnel establishment
→ Internet verification
```

Do not perform expensive testing when an earlier cheap test already proves failure.

Use bounded parallelism.

Each test result should capture:

* success/failure
* measured latency
* measurement provenance
* failure category
* timeout
* protocol
* target
* timestamp
* test path
* core used

---

## 10.1 Test freshness

Classify observations as:

* fresh
* recent
* stale
* unavailable

Ranking must prefer fresh evidence over stale evidence.

---

## 10.2 Failed candidate cooldown

Repeatedly failing configurations should temporarily enter cooldown.

Cooldown should decay automatically.

A previously excellent configuration should be able to return after a successful retest.

---

# 11. P1 — Better Ranking Model

Ranking should combine:

```text
verified success
freshness
measured latency
success rate
failure streak
protocol reliability
recent tests
core compatibility
network environment
```

Never use static source order as a proxy for quality.

Do not overfit to latency alone.

A 50 ms unstable node should not automatically dominate a 70 ms stable verified node.

---

# 12. P1 — Unified Provider/Core Status Center

The Cores page should become the authoritative runtime management center.

Sections:

```text
Protocol Cores
    Xray
    V2Ray
    sing-box

Connectivity Providers
    Tor
    Psiphon
```

Every item exposes:

* installation state
* installed version
* latest known version
* source
* checksum
* health
* executable
* local endpoints
* runtime state
* install/update/repair/remove actions

No duplicate provider/core installation logic.

---

# 13. P1 — Connection Session Management

There must be exactly one authoritative connection session model.

The session owns:

* selected route
* active provider/core
* local proxy ports
* verification state
* latency evidence
* monitoring
* recovery episode
* cleanup

Disconnect must clean everything created by that session.

A new connection must not inherit stale local listeners or previous unmanaged state.

---

# 14. P1 — Runtime Cleanup and Resource Control

All application-owned runtime material must be bounded.

Implement:

* log rotation
* stale `.part` cleanup
* stale provider runtime cleanup
* abandoned core process cleanup
* bounded diagnostics history
* cache limits
* queue limits
* startup cleanup
* shutdown cleanup

Routine cleanup must never delete durable user configuration without explicit user action.

---

# 15. P1 — Logging Profiles — COMPLETED in v0.9.8.4

Support three logging levels:

### Normal

Minimal operational information.

### Detailed

Connection/provider/core lifecycle and useful diagnostics.

### Debug

Verbose tracing, correlation IDs and detailed subsystem fields.

Normal mode must be the default.

Debug identifiers must not consume normal runtime storage.

### Implementation (v0.9.8.4)

ONE authoritative policy lives in the logger
(`internal/logging/logging.go`) — no scattered `if debug` checks:

* `Profile` (normal / detailed / debug) + `Record.Lifecycle` tier;
  admission = severity floor × profile × lifecycle tier
  (`admitsPolicy`), checked BEFORE any formatting cost.
* Normal (default): routine verbose diagnostics suppressed; records
  stay compact — session/event identity only on correlation-opt-in
  records (v0.9.8.3 contract preserved).
* Detailed: adds lifecycle-tagged diagnostics (core start/stop,
  cleanup reclamation) and full identity on every emitted record;
  the dedupe gate still collapses repetitive ticks.
* Debug: verbose diagnostics plus correlation identifiers on every
  record — connection/recovery episode identifiers universally
  available; still bounded by rotation and age retention.
* Settings: `logging_profile` persisted with the existing settings
  system, validated, and applied to the live logger immediately on
  save (`engine/app/loggingservice.go`); Settings UI segmented
  control with concrete per-profile explanations.
* Safety and performance unchanged: redaction applies to every
  record in every profile; no per-record identity allocations in
  Normal; rotation/retention bounds apply to all profiles.
* Regression matrix: `internal/logging/logging_profile_test.go`
  (per-profile admission, runtime switching Normal → Detailed →
  Debug → Normal without restart, redaction, rotation/retention) and
  `engine/app/loggingservice_profile_test.go` (validation,
  persistence, live switch).

---

# 16. P1 — Performance Budget

FreeIran should optimize for:

* fast startup
* low idle CPU
* low idle disk I/O
* bounded memory
* fast connection attempt
* fast recovery
* minimal logging overhead

Measure rather than assume.

Track:

```text
startup → UI ready
Quick Connect → test start
Quick Connect → first connection
Quick Connect → verified Internet
recovery → next candidate
core start → local listener
provider start → verified route
```

Do not invent benchmark claims.

---

# 17. P1 — UI State Integrity

All UI surfaces must reflect actual backend state.

Never display:

* Connected before verification
* Installed before activation
* Ready before readiness observation
* Healthy without a health measurement
* Current ranking from stale data without freshness indication

The UI should explain why an action is unavailable.

---

# 18. P2 — Advanced Connectivity Features

After the core reliability work is complete:

* configurable SOCKS/HTTP proxy exposure
* system proxy integration
* TUN mode
* per-application routing
* route diagnostics
* DNS strategy
* proxy chain support
* connection profiles
* import/export profiles
* environment-aware route selection
* richer provider diagnostics

These must build on the existing connection engine rather than creating parallel networking implementations.

---

# 19. P2 — Provider Expansion

Potential future first-class providers may include:

* additional censorship-resistance transports
* additional tunnel engines
* additional verified protocol runtimes

Every new provider must satisfy the same contract:

```text
Resolve
Install
Verify
Start
Ready
Health
Connect
Monitor
Recover
Stop
Cleanup
```

No provider becomes a fake node configuration merely for UI convenience.

---

# 20. P2 — Recovery Intelligence

The recovery system should eventually learn from:

* previous success
* previous failure
* failure type
* route quality
* time of day
* network environment
* provider/core reliability
* recent verification
* configuration age

Recovery decisions must remain explainable.

Example:

```text
Switched because:
previous route failed Internet verification
candidate had successful verification 4 minutes ago
candidate measured 83 ms
candidate has zero recent failures
```

---

# 21. Data and Safety Invariants

These are non-negotiable.

### User configuration

Never silently delete.

### User-provided executables

Never move, rename or delete the original file.

### Managed executables

Only activate after:

```text
trusted source
→ checksum/signature validation
→ archive safety
→ executable validation
→ smoke test
```

### Network tools

Preserve:

* SSRF protection
* DNS rebinding protection
* redirect validation
* private-target restrictions
* bounded body sizes
* bounded timeouts

### Processes

Every child process must use the unified process-supervision system.

No provider-specific raw Windows process management.

---

# 22. Testing Strategy

Every implementation phase must add regression coverage.

## Backend

```text
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

## Connection

Test:

* fresh ranking
* stale ranking
* last-working candidates
* failed candidate cooldown
* retry
* recovery
* Internet verification
* disconnect cleanup

## Providers

Test:

* resolve
* install
* checksum
* invalid archive
* missing executable
* start
* readiness
* health
* stop
* uninstall
* rollback
* user-binary adoption

## Core manager

Test:

* fresh install
* already installed
* update
* repair
* rollback
* failed download
* checksum mismatch
* corrupted archive
* wrong architecture
* missing executable

## Proxy ports

Test:

* free port
* occupied port
* invalid port
* preferred port
* fallback port
* actual listener verification
* restart
* disconnect/reconnect

## Workspace

Test:

* portable deployment
* installed deployment
* explicit `FREEIRAN_HOME`
* application-local runtime
* uninstall cleanup
* migration safety
* no hidden workspace creation

## Logging

Test:

* default compact format
* no per-record session ID
* no per-record event ID
* debug correlation mode
* rotation
* bounded file growth
* no repetitive memory-policy spam

## Frontend

Test:

* Quick Connect fresh-test flow
* provider mode
* unavailable provider
* installation states
* configuration reorder
* persistence after reload
* proxy-port configuration
* connection state accuracy

---

# 23. Release Acceptance Gate

A release is complete only when all of these are true:

## Connection

* Quick Connect tests the recent best configurations again.
* Fresh results feed ranking.
* Connection uses freshly ranked evidence.
* Internet verification is mandatory.
* Recovery repeats the evidence cycle.

## Providers

* Tor installs and runs successfully through the managed pipeline.
* Psiphon has a safe managed installation path when a trusted official binary exists.
* Otherwise Psiphon clearly exposes the verified user-binary workflow.
* No unverified moving-branch executable is executed.

## Cores

* Xray installs correctly.
* V2Ray installs correctly.
* sing-box installs correctly.
* Update/repair/remove work.
* Version and checksum state are visible.

## Ports

* User-selected proxy ports work.
* Occupied ports are detected before connection.
* Actual listeners are verified.
* No unexplained silent port changes occur.

## Workspace

* Everything persistent and runtime-owned is inside the application folder.
* No hidden `%AppData%\FreeIran` workspace exists.
* Uninstall removes the installed application tree.
* Optional user-data preservation is explicit.

## Logging

* Default logs are compact.
* No per-record session/event IDs.
* Memory-policy spam is gone.
* Adaptive queue growth is evidence-driven.
* Log storage is bounded.

## Configuration ordering

* Reordering modifies the whole collection correctly.
* Persisted order survives reload/restart.
* Regression tests cover the previous bug.

## Verification

* Local tests pass.
* Windows CI passes.
* Runtime Windows smoke passes.
* Clean-room installation passes.
* Clean uninstall passes.
* No unsupported claims are reported.

---

# 24. Recommended Implementation Order

### Phase 1 — Connectivity correctness

1. Quick Connect recent-success retesting
2. Fresh ranking integration
3. Mandatory Internet verification
4. Recovery re-ranking
5. Configuration reorder bug

### Phase 2 — Runtime performance

6. Fix adaptive queue-depth growth
7. Remove repetitive memory-policy logging
8. Compact default logging
9. Log rotation and bounds

### Phase 3 — Cores and providers

10. Repair Tor resolution/install
11. Modernize core catalog and install/update/repair flow
12. Improve Psiphon acquisition UX
13. Preserve checksum/signature safety

### Phase 4 — Proxy control

14. User-selected SOCKS5 port
15. Optional HTTP port
16. Port preflight
17. Listener verification
18. Automatic fallback policy

### Phase 5 — Installation model

19. Redesign installed workspace to remain inside application folder
20. Clean installer
21. Optional desktop shortcut
22. Embedded uninstaller
23. Complete application-folder cleanup
24. No hidden secondary workspace

### Phase 6 — Full verification

25. Backend regression suite
26. Frontend regression suite
27. Windows CI
28. Windows runtime validation
29. Clean-room installation
30. Clean uninstall
31. Performance/resource validation
32. Release documentation

---

# 25. Completion Standard

FreeIran should reach the point where a new user can:

```text
Install FreeIran
→ optionally create desktop shortcut
→ open FreeIran
→ core/provider status is detected automatically
→ install required cores/providers from the app
→ choose preferred local proxy port
→ press Quick Connect
→ FreeIran tests its best recent configurations
→ re-ranks them
→ connects
→ verifies Internet
→ exposes the configured local proxy
→ monitors the route
→ automatically recovers when necessary
→ disconnects cleanly
→ exits without leaving orphan processes
→ uninstall removes the application cleanly
```

The product should require as little manual technical knowledge as possible while preserving explicit control, trustworthy verification, and transparent failure reasons.

---

# 26. Engineering Rule

Do not solve these issues as isolated UI patches.

The desired architecture is:

**one workspace → one lifecycle → one connection engine → one ranking engine → one testing engine → one process supervisor → one core/provider management model → one source of truth for runtime state.**

Every future feature should reuse those foundations rather than creating another parallel subsystem.
