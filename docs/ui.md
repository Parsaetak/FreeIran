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
| **Connection** | advanced · diagnostic · controllable. Manual selection, attempt history, recovery details, tunnel/system integration, core inventory. |
| **Dashboard** | overview. Summarized connection state, onboarding, metrics. Its connect action stays functional and shares the visual language. |

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
fallback to the discovery flow, and the one-poll-per-mount rule.
