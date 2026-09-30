# UI Architecture & Design System (v0.9.8)

This document describes the frontend architecture of the FreeIran
desktop UI, the design-system contract every page is held to, and the
v0.9.8 Quick Connect surface — including its configuration-selection
behaviour and the measured-ping ordering contract.

## Stack & boundaries

- **React 18 + TypeScript + Vite** served inside the Wails v3 webview;
  zustand stores hold all observable state. No router, no CSS
  framework: the design system is a single hand-maintained stylesheet
  (`frontend/src/styles/index.css`) organized in numbered sections
  (tokens → base → shell → controls → data display → log → overlays →
  motion → responsive → page-specific → Quick Connect).
- The UI talks to the Go engine **only** through generated Wails
  bindings, imported exclusively via `frontend/src/services/index.ts`.
  Backend errors are normalized once there (`call()` + `BackendError`).
- The backend is the source of truth for connection state. The UI
  never synthesizes states, fakes progress, or invents metrics; every
  number shown is measured by the engine and every loading state maps
  to a real operation.

## State stores (frontend/src/state)

| Store | Responsibility |
|-------|----------------|
| `connectionStore` | Mirrors the engine connection state machine (`freeiran:connection` events + explicit calls): `connect(configID)`, `connectBest(exclude)`, `disconnect`, `reconnect`. The state string (`disconnected…connected…connection_failed`) is the single source of truth — no parallel booleans. |
| `quickConnectStore` | v0.9.8: loads ONLY the bounded ranked candidate views (`BestCandidates`, ≤100, TTL-cached server-side), orders them via the pure picker model and tracks the explicit selection (`null` = Auto). Never touches the configuration database. |
| `startflowStore` | Adaptive discovery flow stages (`freeiran:startflow` events): detect → discover → test → rank → connect → verify. |
| `settingsStore` | Persisted settings + transient reduced-motion preview. |
| `appStore`, `sourcesStore`, `configsStore`, `toastStore` | App status/sources/config pages/notifications. |

Subscription discipline: pages subscribe with **selective zustand
selectors** (primitives and stable references). A connection-state or
metric change must not re-render the whole application.

## Quick Connect (v0.9.8)

Quick Connect is the application's home connection surface: first
navigation item, landing page, and intentionally minimal — one status
headline, one compact picker, one primary action.

### The single primary action

Exactly one primary element exists. It is **CONNECT** while
disconnected (and again on the failed state, as retry), a disabled
loading state (PREPARING… / CONNECTING… / VERIFYING…) while the
engine works, and the non-interactive **CONNECTED** status
representation once connected. A second competing connect-family
button is never rendered; disconnect lives on the advanced Connection
page, reachable through one small secondary link.

### Connect decision tree (all existing engine paths)

```text
CONNECT pressed
├─ explicit configuration selected in the picker
│    → ConnectionService.Connect(configID)
├─ ranked candidates available (verified or untested)
│    → ConnectionService.ConnectBest(...)   (engine ranking decides)
└─ no candidates at all
     → DiscoveryService.RunStartFlow()
       (detect → discover → test → rank → connect → verify)
```

Recovery, fallback and verification are the engine's own; Quick
Connect adds no duplicate process management and never bypasses
verification. If an active connection fails, the bounded recovery
supervisor handles fallback; the page shows the honest failed state
with the same single CONNECT action as retry.

### Measured-ping ordering (picker model)

The picker renders `CandidateView`s (credential-free: name, protocol,
class, measured latency, samples, freshness). Ordering is a pure,
unit-tested function (`frontend/src/utilities/quickConnectModel.ts`):

1. **verified usable candidates first** (connectable, class ≠ dead,
   has observations);
2. **by measured ping ascending** — `latency_ms` is the median of
   real test observations. Source-provided latency, stale estimates
   and historical guesses are never substituted: a candidate without
   a ping measurement sorts below every measured one;
3. ties break by the engine's own aggregates (recent success → sample
   count → freshness → score) and finally by fingerprint — the list is
   deterministic;
4. **untested candidates come last**, shown with "—" and a neutral
   status dot. A theoretically low latency can never outrank a
   verified usable candidate;
5. **dead / unconnectable candidates are excluded** from the picker.

Provenance labelling: fresh measurement → `verified` (green dot),
older than 30 minutes → `stale` (amber dot), no observations →
`untested` (grey dot, "—").

### Loading & connection animations

CSS/SVG only — no GIFs, no image assets, no fake progress percentages.
Each visual maps to a real backend state: preparing (scan pulse),
connecting (progressive orbit), verifying (radar heartbeat), connected
(stable success glow), failed (brief shake then stable error state).
Under `prefers-reduced-motion` or the in-app reduced-motion setting
(`html.reduced-motion`) every continuous animation is removed; state
changes remain readable through color/border/opacity transitions and
usability is never blocked.

### Quick Connect vs Connection vs Dashboard

| Surface | Role |
|---------|------|
| **Quick Connect** | simple · fast · minimal. One tap to the fastest measured connection. |
| **Connection** | advanced · diagnostic · controllable. Manual selection, attempt history, recovery details, system-proxy and TUN integration (v0.11.3: TUN runs the active configuration through the managed sing-box dataplane — Direct / System Proxy / TUN with observed status, interface, IPv4/IPv6, DNS, core and configuration surfaced while active), core inventory. |
| **Dashboard** | overview. Summarized connection state, onboarding, metrics. Its connect action stays functional and shares the visual language. |

## Quick Connect provider modes (v0.9.8.1)

- A compact provider-mode selector (a `radiogroup` with `Auto /
  Configurations / Tor / Psiphon`) sits directly ABOVE the
  configuration picker (§12). Uninstalled providers remain visible
  but are honestly marked unavailable — the choice is never silently
  removed; the backend's evidence-based Auto mode (see
  docs/providers.md) decides when selection is left to Auto.
- **Configurations** mode renders the classic picker and the v0.9.8
  decision tree unchanged. **Tor** / **Psiphon** modes route the
  single CONNECT action through the provider session lifecycle
  (`providerService.ConnectProvider`) — the SAME one primary action,
  the same state visuals, the same verification gate; no second
  connect-family control is ever added. **Auto** delegates to the
  backend's explainable evidence scoring.
- Picker rows now carry the protocol label, and the measured-ping
  ordering contract gains the v0.9.8.1 sub-millisecond rule: a
  measured `latency_ms` of 0 with `latency_ms_measured === true`
  renders **"< 1 ms"** and sorts FIRST among measured candidates
  (never as "0 ms", never as unmeasured — see docs/latency.md).
  Unmeasured rows keep the dash and sort last.
- The picker model and the provider-mode routing are unit-tested
  (`frontend/src/utilities/quickConnectModel-subms.test.ts`, the
  Quick Connect page suite).

## Cores page providers (v0.9.8.1)

- A **Providers** section below the managed-cores grid (§13) renders
  one card per Tor/Psiphon provider from
  `frontend/src/state/providerStore.ts` (backend `Info` views — no
  synthesized state). Each card reports the honest runtime facts:
  version, lifecycle state, runtime source, license + attribution
  notice, last check, local endpoints, measured health latency
  (sub-ms shown "< 1 ms") and capabilities — capabilities are shown
  only when the provider reports them from its real runtime.
- Card actions map one-to-one to the provider lifecycle —
  Install / Verify / Start / Stop / Uninstall — each a real backend
  call with loading state; nothing runs automatically. Core-kind
  providers (xray/v2ray/sing-box) keep their existing managed-core
  cards; the adapter exposes no provider-level Start for cores
  (they run per node-configuration through the connection engine).

## Network tools (v0.9.8.1, extended v0.9.8.5)

- The Network page gains the **Internet tools** section (§6): the
  grouped tool grid from the backend catalogue (connectivity /
  protocol / path / identity / tunnel), a per-tool target input for
  tools that accept one (validated server-side before any bytes
  leave the machine), and structured results — status, measured
  duration/latency (sub-ms as "< 1 ms"), transport, error kind.
- The via-tunnel toggle is rendered ONLY while a tunnel is active;
  it routes the tool through the session's local SOCKS endpoint and
  the result names the provider (path `direct|tunneled`).
- Nothing runs automatically: every tool execution is an explicit
  user action (the backend enforces user-triggered-only); honestly
  unsupported tools and privilege-gated ones (traceroute without a
  usable ICMP walker) render their honest statuses rather than fake
  results.
- **v0.12.1 status presentation (§3/§13):** every tool row/card shows
  the tool, what it measures (the catalogue's `what` line), the
  target, Direct/Via-tunnel, the latest result, the measured value
  and Run. The ten backend states get distinct bands — `ok` green,
  `partial`/`timeout`/`invalid_target` warn, `failed`/`unreachable`
  error — while `not_configured`, `unsupported` and `not_applicable`
  render in calm info/neutral bands and are NEVER styled as red
  failures. Tunnel diagnostics is blocked with the honest "No active
  tunnel" hint while no tunnel is active (no post-hoc failure).

### Network Identity card (v0.9.8.5)

- The top of the Network page carries the identity card: local IP
  (route-relevant, with its interface), public/exit IP (direct or
  tunnel, with a match comparison on tunneled runs), ISP/ASN/country
  metadata, and the checked-at time. The empty state explains that
  nothing is sent automatically; the card runs ONLY from its
  explicit **Check identity** button. A via-tunnel toggle appears
  only while a live tunnel exists. Unavailable metadata renders
  "Unknown — never fabricated".

### Staged diagnostics ladder (v0.9.8.5)

- Below the connectivity callout, the "Connection stages" card
  renders the nine-rung ladder (local link → local IP → DNS → TCP →
  TLS → HTTPS → captive portal → direct Internet → tunnel Internet)
  with per-rung status dots (ok / failed / neutral for skipped and
  not-checked), measured latency, failure class and the first
  failed rung emphasized.

### DNS diagnostic evidence (v0.9.8.5)

- A DNS tool run renders its structured per-resolver rows: one block
  per resolver with its A/AAAA outcomes, transport badges, best
  latency, answer counts with the first addresses, and honest
  failure classes for failing rows.

## Design-system contract

- **Tokens only**: spacing (`--space-1…8`), radii, text sizes,
  control heights (`--control-h*`), motion durations/easings. No
  one-off margins, magic offsets or duplicated CSS; layout problems
  are fixed in the parent layout (flex/grid/gap), not per element.
- **Page headers** use one structure everywhere:
  `.page-header` › `.page-heading` (`.page-title` + `.page-subtitle`)
  plus an optional `.page-actions` group.
- **Buttons** (`btn`, primary/secondary/ghost/danger × sm/default/lg,
  icon-only) share one height rhythm, gap, radius and focus ring.
  Loading buttons keep a reserved `.btn-icon-slot` so the spinner can
  never resize the control.
- **Icon hierarchy**: icon-only controls 15px, text buttons/card
  titles 14px, compact rows 13px, empty states 20px — consistent
  bounding boxes, stroke weight, `display` via flex alignment.
- **Empty states** share one structure (icon · title · short
  explanation · optional actions) centred through `EmptyState`;
  **loading states** use skeletons for content loading, spinners for
  short actions, and state animations for connection operations.
- **Tables** keep fixed action columns, aligned numerics, clipped
  long-text cells (`.cell-clip` + tooltip) and consistent row heights.
- **Accessibility**: semantic buttons, keyboard operability
  (including the Quick Connect listbox via the
  `aria-activedescendant` pattern), visible focus, `aria-live` status
  region and `aria-busy` during connection work, reduced-motion
  support, readable contrast on the dark palette.
- **Responsive**: the shell collapses the sidebar below 1100px; every
  page is verified at 1280×720, 1440×900, 1920×1080, 1024×768,
  800×600 and narrow widths with no overlap and no inaccessible
  controls.

## Testing the UI

`cd frontend && npm test` runs vitest. Pure modules (picker model,
row models, stores) test in the node environment; the Quick Connect
page suite runs under jsdom (`@testing-library/react`) and pins the
page contract: single primary action, picker ordering, all connection
states, keyboard operation, reduced-motion class, no-candidate
fallback to the discovery flow, and the one-poll-per-mount rule. The
Network page suite (v0.9.8.5) pins the identity card's no-auto-run
contract, the staged ladder rendering and the DNS evidence rows.


## Design system: WHITE / BLACK / RED (v0.10.1)

The visual identity is a three-part system, defined once in the
centralized token block at the top of
`frontend/src/styles/index.css` and consumed by every surface:

| Part | Role | Tokens |
|------|------|--------|
| BLACK | the surface hierarchy (shell, sidebar, cards, tables, overlays) | `--bg`, `--surface`, `--surface-raised`, `--surface-hover`, `--surface-active`, `--surface-sunken`, `--border*` |
| WHITE | the text hierarchy | `--text`, `--text-secondary`, `--text-dim`, `--text-faint` |
| RED | the brand/interactive accent (buttons, focus rings, selection, active states, highlights, links) | `--accent`, `--accent-strong`, `--accent-dim`, `--accent-border`, `--accent-text` |

**Status hues are semantic, not decorative.** Working/connected
states stay green (`--success`), warnings amber (`--warn`), failures
red (`--error`). At-a-glance state truth is never traded for palette
purity — a red "connected" would read as a failure. The brand accent
(`#e5484d`) and the failure red (`#f87171`) are distinct shades of
the same family, and the provenance dots (`verified` green, `stale`
amber, `untested` grey) keep their meaning.

Page-specific styles in the stylesheet's numbered sections may only
reference tokens — introducing a page-private color literal is a
review-blocking regression (the v0.10.1 audit found none; the last
brand-accent change was a token-only edit).

## v0.11.0 — ECH states, failure evidence, and truth notes

The configuration detail panel renders two new evidence sections,
both driven ONLY by recorded facts:

**Failure evidence** (shown when a failure streak or class exists):
the consecutive trailing failure count, the classified cause of the
most recent failure (the nine-class vocabulary), the credential-free
failure detail, and the last-failure / last-verified times. A muted
note states what the evidence DOES: repeated protocol-class failures
(tls/handshake/transport) demote the candidate in automatic selection
so other transports are preferred.

**Encrypted Client Hello** (shown only for ECH-configured records):
FOUR DISTINCT states, deliberately never merged into one badge:

1. **Configuration** — the stored record carries `ech_enabled`
   (user intent; nothing more).
2. **Core support** — an installed, available backend that supports
   this exact configuration also declares ECH (today: sing-box,
   schema-verified against the pinned 1.14.x core — originally probed
   against v1.14.0 in v0.11.0, green against the 1.14.1 pin since
   v0.11.4). Rendered as an
   explicit failure state when absent.
3. **Core acceptance** — the last test executed through an
   ECH-declaring backend SUCCEEDED (the real core accepted the
   generated ECH document and the tunnel verified). Rendered
   honestly as "not yet verified through an ECH-capable core" when
   absent.
4. **Connectivity** — the generic working/failed state every
   configuration has; ECH adds no separate claim.

A truth note under the section bounds the claim: ECH support is
schema-level evidence (config check + core startup), not proof of
live ECH negotiation. No "secure", "censorship-resistant" or
"unblockable" language appears anywhere in the ECH UI.

## v0.11.0 — dense configuration table, first-class groups, honest group actions

**Dense configuration table (wide viewports).** The primary browsing
surface is a professional single-line node table (36 px rows) with a
sticky, sortable column header — the familiar configuration-manager
interaction model, on FreeIran's own architecture and trust model:

```text
★ | Protocol | Name / Endpoint | Transport | Latency | Test status | Source | ⋮
```

- **Columns** show identity and measured evidence only: favorite,
  protocol, name, `address:port`, transport/security (`ws/tls`),
  measured latency, test status, source. Deep technical data
  (URL-test internals, test backend, timestamps, credential
  presence) stays in the detail panel.
- **Sorting** rides the ONE server-side filter pipeline: a header
  click applies `sort_by` (protocol, endpoint, latency, tested_at,
  source); clicking the active column clears it. No client-side
  re-sorting of a bounded window.
- **Virtualization is retained** on every path; narrow viewports
  keep the v0.9.13 two-line card row (88 px).
- **Testing lifecycle** renders the REAL states on every row —
  Idle → Queued → Preparing → Testing → Measuring → Passed / Failed
  / Timed out / Cancelled — the live ones from the queue's complete
  live-state projection, the terminal ones from persisted measured
  evidence. Nothing is ever called "verified" or "connected" because
  a process launched.

**First-class group navigation.** The group rail carries the built-in
evidence groups (All / Favorites / Working / Untested / Fast /
Recently tested) with LIVE counts from the store, plus user groups
with member counts. A selected group visibly becomes the active
browsing scope (active chip + server-side group filter). User groups
support create, inline RENAME, and DELETE — deleting a group never
touches configurations (they live in the store; the group only
references stable config IDs). While a user group is the active
scope, the toolbar offers "Remove from <group> (n)" for the selected
rows and the row menu offers membership removal — acting on the
ACTIVE group only, never a guess.

**Honest group actions (the v0.9.10 defect, root-fixed).** The
previous "Add selected to group" control swallowed per-item errors
(`.catch(() => undefined)`) and always reported success. The result
is now truthful: all succeeded → success; partial failure → a
diagnostic with the count AND the preserved backend error; all
failed → an error. A group-action failure can no longer produce a
success toast (regression-tested). Membership changes reconcile the
list, selection, group counts, active filter and detail panel
immediately through the one server-side filter — no full page
reload.

**Scope-aware bulk testing.** The testing bar states its scope with
live counts (Test selected (n), Test untested (n)) and the overflow
menu carries Retry failed / Retry timed out / Retest working. Every
bulk action is an explicit user origin; "Test selected" can never
silently become "test all 20,000" (see docs/architecture.md, Test
scheduling model).

---

## v0.11.2 addendum — internal configuration tabs + native tray

**Configurations** (frontend/src/pages/Configs.tsx):

- `All` is now a first-class scope. The default `groupFilter` value
  is `"all"` so the All chip is active on first load; clicking All
  is a no-op (the user cannot end up in a "no group selected"
  limbo); other chips toggle the active scope back to `"all"` when
  de-selected.
- New internal configuration tabs rail above the detail panel:
  opening config A then B produces `All | A | B`; opening A again
  FOCUSES A instead of creating a duplicate. Tabs are keyed by the
  stable configuration ID, never by row index. The rail supports:
  open, activate/focus, close, close others, close all, return to
  All. Closing the last open tab hides the rail and returns focus
  to the wide table.
- The existing dense table, virtualization, sorting/filtering,
  Favorites/Working/Untested/Fast/Recently tested/user groups,
  bulk testing and v0.11.0 group-action honesty are preserved.

**Native Windows tray** (cmd/freeiran/main.go):

- Wails v3 `SystemTray` carries: Show / Hide / Configurations /
  Network / Diagnostics / Settings / Quit. Navigation actions
  restore the main window AND emit `freeiran:navigate` with
  `{page}` so the React shell syncs the active page (no duplicate
  application windows are ever opened).
- Close main window → hide to tray (does not quit). Tray Quit runs
  the existing `applicationInstance.Shutdown()` hook so
  connection/provider/core cleanup uses the SAME lifecycle path as
  a normal close (no orphan processes).
- The BLACK/WHITE/RED icon family and generated Windows resources
  are preserved. No green icon derivatives are reintroduced.

**Diagnostics numerical spacing** (frontend/src/styles/index.css):

- `.stat-value` / `.stat-value.sm` / `.stat-label` now use
  `font-variant-numeric: tabular-nums` and a slight negative
  `letter-spacing` so latency / duration / port / counts / memory
  / queue / timings read compactly at 100/125/150/175% DPI
  scaling. No layout redesign of unrelated pages.

---

## v0.11.3 addendum — TUN controls + tray setting

**Connection page (TunnelModeCard):** the mode card now offers
Direct / System Proxy / TUN. The TUN button is enabled when a
configuration is connected (the dataplane routes that configuration)
and calls `TunnelService.EnableTUN(configID)`; the backend performs
the full transactional activation and the UI re-reads the
authoritative state afterwards. While a TUN session exists the card
renders the live snapshot: backend label, lifecycle status
(off/starting/active/stopping/failed), interface name, IPv4/IPv6,
DNS design, route design, sing-box core version, redacted
configuration and — on failure — the real error text. TUN is never
labelled a kill switch. (v0.11.4: the backend's activation gate is
harder — exact adapter identity, covering-route ownership and the
upstream loop check must all hold before `active` is published, and
failures surface the precise missing evidence — the card contract
above is unchanged. The dataplane label remains "sing-box native TUN
(Wintun)": TUN is sing-box-specific on Windows and the UI never
implies "all cores".)

**Settings page:** a new "System tray" toggle (Appearance section)
persists `tray_enabled` through the standard Settings save path. It
defaults to ON; turning it off removes the tray icon entirely and
makes the main window's close behave as a normal application close.
The tray menu's own "System Tray" checkbox and this toggle are two
views of the same persisted setting.

## v0.11.5 addendum — timestamp-free log rows, seq copy prefix, compact version

**Diagnostics log rows (LogEntry):** the runtime-log `Entry` schema no
longer carries a wall-clock timestamp (`ts` removed end to end — Go
schema, generated bindings, UI). The log row renders level,
subsystem, event, message and the correlation suffixes — the leading
`log-time` cell is gone (and its CSS rule with it). The "copy
diagnostics" text prefixes each line with the per-session `#seq`
instead of a timestamp: `#seq` is the actual ordering mechanism, and
inventing a synthetic time from it is explicitly avoided. Live tail,
level/subsystem/event filters, errors-only shortcut and Related-events
behavior are unchanged.

**Dashboard recent-events strip:** the event-age decoration
("3m ago") is removed with the timestamp it was computed from; rows
show status dot, message/event and subsystem/event only. The Dashboard
header's "App started …" line is untouched — it reads the engine's
`started_at` field, which is real uptime evidence, not a log-entry
timestamp.

**Version display:** the status bar and every user-facing version
surface render the compact form `v<VERSION>` (e.g. `v0.12.1`)
exactly — no commit, no Go
toolchain tuple. Developer build provenance (commit, Go version)
remains available on the Developer Info panel in Settings, which is
its designated surface.

---

## Future UI concepts (PLANNED — v0.12.0 documentation only)

The current UI is NOT redesigned for these; they are the recorded
surface contracts for future phases
([autonomous-connectivity.md](autonomous-connectivity.md)). Each mode
must never imply more protection than its evidence supports:

```text
Protected              Censorship Adaptive    Tor Circumvention
Onion Only             I2P                    Multi-Hop
Low-Metadata
```

Future evidence surfaces the UI will eventually render:

```text
ConnectionProof        — the proof fields (core/route/DNS/IPv4/IPv6/
                        public-IP/traffic/kill-switch, VerifiedAt)
trust topology         — local observer → hop 1 → hop 2 → exit →
                        destination (who observes what)
privacy exposure       — local MAC visibility, public IP, IPv6, DNS,
                        hardware-ID collection (none), provider
                        visibility, correlation limits
IPv4/IPv6 evidence     — protected / leak / unavailable states
DNS evidence           — resolver in use, leak-test state
kill-switch state      — DIRECT → ARMING → ... → MONITORING ladder
```

The evidence-first rule is inherited from the current surfaces: a
failure state is rendered honestly ("Connected but IPv6 not
verified"), and no badge may claim more than its evidence class.

## v0.12.1 — source/subscription scopes, targeted actions, context-menu policy

- **Scope rail (§16):** the configuration groups rail gains SOURCE
  scopes (one chip per source/subscription with its authoritative
  count from `SourceStatsList`). `All` stays the first-class default.
  Selecting a source selects the server-side `source` filter; counts
  are never recomputed in React.
- **Source scope header (§17):** while a source scope is active, a
  compact header shows the backend's measured evidence — name,
  enabled/disabled badge, ROUTE-trust band, configuration/working
  counts, last successful fetch, last failure and its recorded
  reason — plus the actions Update source (targeted refresh),
  Check source, Check untested and More (retry failed / retest
  working in scope) and Exit scope. A user group never shows an
  update action.
- **Sources page integration (§27):** source rows carry compact
  targeted actions — update this source, view its configurations
  (lands in the matching scope through the `freeiran:navigate` event),
  check its configurations (shared queue).
- **Context-menu policy (§25):** the native browser/WebView menu is
  suppressed app-wide (one shared policy mounted by the shell);
  right-click on configurations opens FreeIran's own MenuSurface —
  the ONE custom menu engine, with Escape, arrow keys, Home/End,
  focus return, outside click, viewport flipping, scroll handling and
  ARIA semantics intact. Text fields keep their native editing menu
  so clipboard and IME behaviour survive.
- **Keyboard UX (§30):** Ctrl+A selects the visible scope; Enter
  opens the focused configuration (existing); Shift+F10 / ContextMenu
  open the row menu (existing); Delete/Backspace removes from the
  current user group (the existing supported removal).
- **Performance rules (§28):** source selection is a server-side
  filter; a single test result patches one record; a source refresh
  reconciles its scope — virtualization, bounded page sizes and the
  hot cache are untouched (see docs/configurations.md).
