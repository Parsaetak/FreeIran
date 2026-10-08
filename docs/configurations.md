# Configuration Workspace

Authoritative description of the Configurations surface
(`frontend/src/pages/Configs.tsx` + `engine/app` services) as of
**v0.12.1**. Current behavior of the frozen product only. Research
directions that once lived in docs/autonomous-connectivity.md are
ARCHIVED HISTORICAL RESEARCH — not upcoming work.

## What a "folder" is

A configuration folder is a **logical browsing scope**, not a
filesystem directory. FreeIran never introduced a second persistence
system to create folders: every scope is a view over the same
configuration store, resolved through the ONE server-side filter
pipeline (`DataService.ListConfigsFiltered`).

The scopes:

| Scope | Meaning | Filter mapping |
|-------|---------|----------------|
| All | every configuration (`all` is the stable default id) | `group: "all"` (matches everything server-side) |
| Favorites | the user's favorited fingerprints | `group: "favorites"` (collections.json membership, O(1) compiled set) |
| Working / Failed / Untested | measured evidence groups | `status: "working" / "failed" / "untested"` |
| User Groups | user-created sets (`g-...` ids) | `group: <id>` (stable config IDs, persisted) |
| Source / Subscription scopes | one source's configurations | `source: <sourceID>` (cfg.Source == sourceID) |

Scope chips carry live counts and every count describes the WHOLE
dataset — the All badge equals the authoritative store count, never a
bounded prefix (the pre-v0.13.0 scan stopped at 4,000). Built-in and
group counts are computed by one uncapped store scan over a four-field
projection per record and served from an authoritative cached count
snapshot invalidated by real store/test/collection changes; **source
scope counts come from the authoritative
`SourceService.SourceStatsList()`** — the UI never recomputes source
health and never downloads the database to filter it in React.

Filtered/sorted scopes are fully paginated server-side
(`DataService.ListConfigsFiltered`): the total is the TRUE global
match count, ordering is global with a stable config-ID tie-breaker,
and only the requested page window crosses the service boundary — no
visible-results caps anywhere in the pipeline.

## Source scopes and the scope header

Selecting a source scope shows a compact header with the backend's
measured evidence only:

- source name, enabled/disabled state, ROUTE-trust band
  (`official` / `user` / `public`);
- configuration count and working count;
- factual freshness — `Updated just now` / `Updated 12m ago` /
  `Updated Oct 1, 13:05` (exact time in the tooltip) or `Never
  fetched`; a never-fetched source serialises WITHOUT a timestamp
  (`*time.Time` + `omitempty`), and impossible ages render as never
  fetched. Content hashes are internal provenance/dedupe evidence and
  never ordinary UI.

Header actions:

- **Update source** — `SourceService.RefreshSource(id)`: a targeted
  refresh of EXACTLY ONE source through the SAME ingestion
  architecture (one fetcher, one parser pipeline, one store, one
  scheduler). It validates the source ID, respects the enabled
  policy, preserves dedupe/content-hash behaviour, publishes the
  normal source-refresh state and returns the source's bounded stats.
- **Check source / Check untested / Retry failed / Retest working** —
  `TestQueueService.EnqueueByFilter` with
  `source = selectedSourceID`; the ONE shared bounded queue stays the
  sole test authority. The UI never loops or launches test processes
  itself.

A **user group is not a remote source**: group scopes expose
test/rename/membership actions and never an "Update source" action.

### Refresh concurrency contract

Targeted refresh and full ingestion cycles share the same single-owner
gate (the app's `ingesting` flag). While a full cycle runs, a targeted
refresh waits (bounded) and then takes the gate; while a targeted
refresh holds it, a concurrent full cycle is skipped by its existing
skip-if-busy CAS. Two refresh requests can never create two ingestion
authorities.

## The dense table

The wide browsing surface is a virtualized single-line table
(`@tanstack/react-virtual`):

```
★ | Protocol | Name / Endpoint | Transport | Latency | Test status | Source | ⋮
```

- Only visible rows reach the DOM; organize-by groupings render as
  virtualized section headers.
- Column clicks apply server-side sorts (the ONE filter pipeline —
  no client-side re-sorting of a bounded window).
- Status is the FULL testing lifecycle rendered from measured
  evidence: idle → queued → preparing → testing → measuring →
  passed / failed / timed out / cancelled. The live states come from
  the ONE shared queue projection (`state/testProgress.ts`); no
  page-private test state exists.
- After a single test, ONE record is patched in place — never a
  20k-record reload. After one source refresh, the affected scope
  reconciles through the smallest existing authoritative read.
- Deep technical data (security, transport internals, test backend,
  timestamps) stays in the detail panel/tabs; per-config
  `ConfigDetails(id)` is the redacted detail surface with Connect /
  Test now / Close actions and ECH/compatibility evidence where
  applicable.

## Multi-select actions

The existing selection set drives the bulk bar: Test selected, Add to
group, remove from the current user group, and the shared queue's
scoped batches (all / untested / failed / timed out / working /
selected). Every testing action enqueues through the shared queue —
`origin: "user"` keeps explicit user actions from silently becoming
background sweeps.

## Context menu

ONE custom menu engine (`components/MenuSurface.tsx`) serves every
row menu: opened by the ⋮ button, by right-click and by keyboard
(ContextMenu / Shift+F10). Placement is viewport-aware (flipping,
clamping, portal, scroll/resize repositioning); dismissal is Escape,
outside click and toggle re-click; focus returns to the trigger.

Row actions (the single `rowMenuItems()` model): Test/Retest,
Connect, Favorite, Select, Add to group, Remove from active group
(when browsing one), Move up/down (unfiltered view only), View
details, Copy endpoint (the safe address:port pair only).

The **native browser/WebView context menu is suppressed app-wide**
(`utilities/contextMenuPolicy.ts`, mounted once by the shell):
right-click on rows opens MenuSurface; right-click on whitespace,
panels, tabs, buttons or the page background opens nothing. Text
fields keep their native editing menu — Ctrl+C / Ctrl+V / Ctrl+A and
IME editing are never broken.

## Keyboard interaction

| Key | Action |
|-----|--------|
| Ctrl+A | select every configuration in the visible scope |
| Enter / Space | open/activate the focused configuration |
| Shift+F10 / ContextMenu | open the row's FreeIran context menu |
| Delete / Backspace | the existing supported removal in context: leave the current user group |

Selection, details and the queue projection behave identically for
keyboard and pointer flows.

## Performance rules (protected)

- virtualized rendering for every row surface;
- server-side filtering and sorting (bounded page sizes);
- the hot cache absorbs repeated page reads;
- selective Zustand subscriptions; no page-private truth;
- no N+1 `ConfigDetails` calls and no N+1 source-stats calls;
- no full-database refresh after a single test; source selection uses
  the backend `cfg.Source` filter, not a React-side download.

These rules were earned against ~20k-configuration runtimes; changes
that re-introduce O(store) work per interaction regress them.

## Proxy chains (v0.12.2)

The scope rail gains PROXY CHAIN scopes beside the source scopes and
user groups — plus "+ New chain". A chain is an ORDERED list of
existing configurations (2–4 hops, index 0 = first hop, last =
egress) persisted in the collections sidecar (schema v2, group kind
`proxy_chain`). Chain records carry configuration IDs only — never
credentials, never payloads.

- **Chain scope.** Selecting a chain filters the table SERVER-SIDE by
  the chain's hop IDs (`ConfigFilter.ids`); the scope header shows
  hop count, completeness, the compiled preview (A → B → C), per-hop
  working evidence and the actions Check chain / Connect / Edit. A
  chain scope NEVER offers "Update source" — a chain is not a remote
  source.
- **Editor.** The compact editor lists hops in order with move
  up/down, remove, protocol + endpoint + latest evidence per hop, a
  live preview and validation before save (min 2 hops, max 4, no
  duplicates, no missing configurations, no nested chains). Check
  hops runs through the ONE test queue; Check chain reports per-hop
  evidence plus a fresh end-to-end measurement; Connect runs the
  SAME verified state machine (one core process — Xray
  `sockopt.dialerProxy` / sing-box `detour`; V2Ray refuses chains
  explicitly).
- **Row menu.** With a multi-row selection the menu offers "Build
  proxy chain from selected"; a chain scope's menu adds remove /
  reorder for its hops. All actions ride the ONE MenuSurface.
