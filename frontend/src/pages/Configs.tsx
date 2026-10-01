import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Events } from "@wailsio/runtime";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import { MenuSurface, type MenuAnchor } from "../components/MenuSurface";
import {
  dataService,
  connectionService,
  testQueueService,
  sourceService,
  proxyChainService,
  call,
  type Config,
  type ConfigDetail,
  type QueueStatsView,
  type QueueLiveStateView,
  type SourceStatsView,
} from "../services";
import { useChainStore } from "../state/chainStore";
import {
  formatLatency,
  formatNumber,
  latencyClass,
  relativeTime,
  truncate,
} from "../utilities/format";
import { configToRow } from "../utilities/export";
// v0.9.8.7: STATIC worker import — the worker factory is part of the
// main bundle and the worker FILE is only fetched when an export
// actually runs. This avoids the dynamic-import chunk that made Vite
// emit export-worker.js twice (once as a dynamic chunk, once from the
// worker pipeline).
import ExportWorker from "../workers/export-worker?worker";
import { useConnectionStore } from "../state/connectionStore";
// v0.9.15: the Quick Connect invalidation moved into the shared result
// patch path (state/testProgress.ts) — one invalidation per persisted
// result, no matter which page triggered the test.
import { describeError, toast } from "../state/toastStore";
import {
  applyLiveState,
  clearTestRequested,
  markTestRequested,
  onTestResult,
  useTestProgress,
  type LiveTestState,
} from "../state/testProgress";
import {
  useCollectionsStore,
  BUILTIN_GROUP_LABELS,
  BUILTIN_GROUP_HINTS,
} from "../state/collectionsStore";
import { EmptyState, Menu, ResultBadge, SegmentedControl } from "../components/common";
// v0.10.2: personal configuration import (paste / file → preview → save).
import { ImportDialog } from "../components/ImportDialog";
import { ChainEditor } from "../components/ChainEditor";
import type { MenuItem } from "../components/common";
import {
  IconChevronDown,
  IconDots,
  IconPause,
  IconPlay,
  IconRefresh,
  IconSearch,
  IconX,
  IconZap,
} from "../components/Icons";

const searchRunner = makeSearchRunner(250);

const PROTOCOL_FILTERS = ["vless", "vmess", "trojan", "shadowsocks", "hysteria2"];

/** Short display label for a protocol value. */
function protocolLabel(type: string): string {
  if (type === "shadowsocks") return "ss";

  return type || "—";
}

/**
 * v0.12.1 (§17): the trust band of the active source scope — the
 * backend's ROUTE-trust classification, never recomputed in React.
 */
function trustBadgeClass(trust: string): string {
  switch (trust) {
    case "official":
      return "success";
    case "user":
      return "info";
    case "public":
      return "warn";
    default:
      return "neutral";
  }
}

/** v0.9.10 organize-by render item (§6 Configuration grouping). */
type RenderItem =
  | { kind: "header"; key: string; label: string; count: number }
  | { kind: "row"; key: string; config: Config };

/** The bucket key of one configuration under the active organize mode. */
function organizeKeyOf(config: Config, mode: "source" | "protocol" | "status"): string {
  switch (mode) {
    case "source":
      return String(config["source"] || "unknown source");
    case "protocol":
      return protocolLabel(String(config["type"]));
    case "status": {
      const tested = Number(config["tested_at"] ?? 0) > 0;

      if (!tested) return "untested";

      return config["working"] ? "working" : "failed";
    }
  }
}

/**
 * v0.9.15: the REAL queue lifecycle rendered on rows and the detail
 * panel — the application's actual task states (queued → preparing →
 * testing → measuring), never fake timers. Labels/title maps keep the
 * wording consistent across every surface that reads testProgress.
 */
const LIVE_STATE_LABELS: Record<LiveTestState, string> = {
  queued: "Queued",
  preparing: "Preparing",
  testing: "Testing",
  measuring: "Measuring",
};

const LIVE_STATE_TITLES: Record<LiveTestState, string> = {
  queued: "Waiting in the test queue",
  preparing: "Preparing the runtime configuration",
  testing: "Core process testing in progress",
  measuring: "Measuring latency",
};

/**
 * v0.11.0: the FULL testing lifecycle rendered on every row — the
 * live queue states while a test runs, then the persisted terminal
 * outcome (Passed / Failed / Timed out) from measured evidence, and
 * Idle for configurations that were never tested. Cancelled is the
 * queue's explicit cancellation class.
 */
export type TestStatus =
  | "idle"
  | "queued"
  | "preparing"
  | "testing"
  | "measuring"
  | "passed"
  | "failed"
  | "timed_out"
  | "cancelled";

const TEST_STATUS_LABELS: Record<TestStatus, string> = {
  idle: "Idle",
  queued: "Queued",
  preparing: "Preparing",
  testing: "Testing",
  measuring: "Measuring",
  passed: "Passed",
  failed: "Failed",
  timed_out: "Timed out",
  cancelled: "Cancelled",
};

/** The truthful status of one configuration row (evidence, never guesses). */
function statusOf(config: Config, live: LiveTestState | undefined): TestStatus {
  if (live) {
    return live;
  }

  const testedAt = Number(config["tested_at"] ?? 0);

  if (testedAt <= 0) {
    return "idle";
  }

  if (config["working"]) {
    return "passed";
  }

  // v0.11.0: the classified failure reason distinguishes a timeout
  // from other failures; the legacy free-text fallback covers older
  // records stored before classes existed.
  const failureClass = String(config["last_failure_class"] ?? "");
  const failureReason = String(config["last_failure_reason"] ?? "");

  if (failureClass === "timeout" || failureReason.toLowerCase().includes("timeout")) {
    return "timed_out";
  }

  if (failureClass === "cancelled") {
    return "cancelled";
  }

  return "failed";
}

/** Inline chip for a config's live test state (row secondary line). */
function LiveTestChip({ state }: { state: LiveTestState | undefined }) {
  if (!state || state === "queued") {
    return state === "queued" ? (
      <span className="test-state queued" title={LIVE_STATE_TITLES.queued}>
        {LIVE_STATE_LABELS.queued}
      </span>
    ) : null;
  }

  return (
    <span className="test-state testing" title={LIVE_STATE_TITLES[state]}>
      <span className="btn-spinner" aria-hidden /> {LIVE_STATE_LABELS[state]}
    </span>
  );
}

/**
 * Virtualized configuration browser: protocol filters, latency color
 * coding, infinite scroll over paginated backend data and a detail
 * side panel. Only visible rows reach the DOM.
 */
export function ConfigsPage() {
  const items = useConfigsStore((state) => state.items);
  const total = useConfigsStore((state) => state.total);
  const loading = useConfigsStore((state) => state.loading);
  const searching = useConfigsStore((state) => state.searching);
  const searchQuery = useConfigsStore((state) => state.searchQuery);
  const hasMore = useConfigsStore((state) => state.hasMore);
  const lastError = useConfigsStore((state) => state.lastError);

  const setSearchQuery = useConfigsStore((state) => state.setSearchQuery);
  const loadMore = useConfigsStore((state) => state.loadMore);
  // v0.9.15: the page-level runSearch hook is gone — testing flows patch
  // results incrementally through the shared queue projection. The only
  // remaining full reload is moveConfig (a list-order change), which
  // reads the store directly.

  // v0.9.10: groups (built-in evidence groups + user groups) and the
  // organize-by view. Favorites toggle straight from every row.
  const builtinGroups = useCollectionsStore((state) => state.builtinGroups);
  const userGroups = useCollectionsStore((state) => state.userGroups);
  const favorites = useCollectionsStore((state) => state.favorites);
  const loadCollections = useCollectionsStore((state) => state.load);
  const toggleFavorite = useCollectionsStore((state) => state.toggleFavorite);

  // v0.12.1 (§16): the configuration scope rail gains SOURCE scopes.
  // The authoritative per-source evidence comes from the backend's
  // SourceStatsList — the UI never recomputes source health.
  const [sourceStats, setSourceStats] = useState<SourceStatsView[]>([]);
  const [sourceFilter, setSourceFilter] = useState<string>("");
  const [updatingSource, setUpdatingSource] = useState(false);

  // v0.12.2: PROXY CHAIN scopes. The rail lists the user's chains;
  // an active chain scope filters the table server-side by the
  // chain's hop IDs (ConfigFilter.ids — no full-database download)
  // and shows a chain scope header with Check/Connect/Edit.
  const chains = useChainStore((state) => state.chains);
  const loadChains = useChainStore((state) => state.load);
  const [chainFilter, setChainFilter] = useState<string>("");
  const [chainDetails, setChainDetails] = useState<Awaited<ReturnType<typeof proxyChainService.ProxyChainDetails>> | null>(null);
  const [chainEditor, setChainEditor] = useState<{ open: boolean; id: string | null }>({ open: false, id: null });
  const [chainBusy, setChainBusy] = useState(false);

  // v0.11.2: `All` is a first-class scope. The default value is the
  // stable id "all" so the All chip renders active on first load and
  // the backend's groupMatches returns true for "all" + "" — both
  // spellings are equivalent at the data layer.
  const [groupFilter, setGroupFilter] = useState<string>("all");
  const [organizeBy, setOrganizeBy] = useState<"" | "source" | "protocol" | "status">("");
  const [newGroupOpen, setNewGroupOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [newGroupName, setNewGroupName] = useState("");
  // v0.11.0: first-class group management — inline rename and
  // membership removal from the active user group.
  const [renamingGroup, setRenamingGroup] = useState<{ id: string; name: string } | null>(null);
  // v0.11.0: membership changes bump this to re-run the server-side
  // filtered view immediately (list + counts reconcile without a
  // full page reload).
  const [viewVersion, setViewVersion] = useState(0);

  useEffect(() => {
    void loadChains();
    void loadCollections();
  }, [loadChains, loadCollections]);

  // v0.12.1: source scopes with authoritative counts. Reloaded after
  // targeted updates (the update path refreshes this explicitly); the
  // rail itself stays read-only otherwise — no duplicated counters.
  const refreshSourceStats = useCallback(async () => {
    try {
      const stats = await call(() => sourceService.SourceStatsList());

      setSourceStats(stats ?? []);
    } catch {
      /* the rail simply stays empty — never invented locally */
    }
  }, []);

  useEffect(() => {
    void refreshSourceStats();
  }, [refreshSourceStats]);

  // v0.12.1 (§27): Sources page actions land here through the shared
  // navigation event: { page: "configs", source: <id> }.
  useEffect(() => {
    const off = Events.On("freeiran:navigate", (event: { data?: { source?: string } }) => {
      const target = event?.data?.source;

      if (target) {
        setSourceFilter(target);
        setGroupFilter("all");
      }
    });

    return () => {
      if (typeof off === "function") off();
    };
  }, []);


  const [protocol, setProtocol] = useState("");
  // v0.11.2: Internal configuration tabs. Opening config A then B
  // produces the rail `All | A | B`; opening A again FOCUSES A instead
  // of creating a duplicate. Tabs are keyed by the stable configuration
  // ID, never by row index, so reorder/sort/filter cannot misroute a
  // tab to the wrong config. The `All` tab is implicit (it is the
  // wide table itself); closing every open tab returns the focus to
  // the wide table — equivalent to the `All` scope.
  const [openTabs, setOpenTabs] = useState<ConfigDetail[]>([]);
  const [activeTabId, setActiveTabId] = useState<string | null>(null);

  // `detail` is the active tab's detail (or null when the wide table
  // is focused). Kept as a derived value so the existing DetailPanel
  // and refresh logic stay unchanged.
  const detail = activeTabId
    ? openTabs.find((tab) => tab.id === activeTabId) ?? null
    : null;
  // v0.9.15: per-config live test state comes from the ONE shared
  // projection of the test queue (testProgress) — rows, detail panel
  // and Connection all read the same store; no page-private truth.
  const testStates = useTestProgress((state) => state.states);

  // v0.9.0: server-side status filter + sorting and bulk-test state.
  const [statusFilter, setStatusFilter] = useState<"" | "working" | "failed" | "untested">("");
  const [sortBy, setSortBy] = useState<"" | "latency" | "tested_at" | "protocol" | "address" | "source">("");
  const [sortDesc, setSortDesc] = useState(false);
  const [filtered, setFiltered] = useState<Config[] | null>(null);
  const [filteredTotal, setFilteredTotal] = useState(0);
  const [filteredLoading, setFilteredLoading] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [queueStats, setQueueStats] = useState<QueueStatsView | null>(null);
  // v0.9.7: queue pause state for the testing control bar.
  const [queuePaused, setQueuePaused] = useState(false);
  // v0.9.13: connection-store busy state gates the Connect action in
  // the row menu (same policy as the detail panel's Connect button).
  const connectBusy = useConnectionStore((state) => state.busy);
  // v0.9.13: the ONE row-level action surface — opened by the ⋮
  // button, by right-click (contextmenu) and by the keyboard
  // context-menu invocation (Shift+F10 / Menu key). Same items,
  // same model; placement is handled by the ONE shared viewport-aware
  // portal surface (MenuSurface) — v0.9.15.
  const [contextMenu, setContextMenu] = useState<{
    config: Config;
    anchor: MenuAnchor;
    /** True when the ⋮ button holds the menu open (its re-click toggles). */
    viaTrigger: boolean;
    triggerId: string | null;
  } | null>(null);

  // The element that opened the row context menu (MenuSurface's toggle
  // contract: outside-pointer dismissal ignores it so the trigger's own
  // click toggles).
  const contextMenuTriggerRef = useRef<HTMLElement | null>(null);
  // v0.12.2: hops pre-seeded into the chain editor from a multi-row
  // selection ("Build proxy chain from selected").
  const pendingChainHops = useRef<string[]>([]);
  // v0.9.7: compact two-row card layout below 860px (actions get a
  // dedicated row) — the virtualizer sizes rows accordingly.
  const [narrow, setNarrow] = useState(
    () => typeof window !== "undefined" && window.matchMedia("(max-width: 860px)").matches,
  );

  const parentRef = useRef<HTMLDivElement>(null);

  // v0.9.13: favorites as a Set — favorite lookups stay O(1) per row
  // (the previous array.includes scanned per visible row per render;
  // with the real dataset size the row is rendered dozens of times).
  const favoriteSet = useMemo(() => new Set(favorites), [favorites]);

  // v0.11.0: the active browsing scope as a user group (null when the
  // scope is a built-in group or nothing) — drives scope-aware
  // membership actions (remove selected from THIS group).
  const activeUserGroupForBar = useMemo(
    () => userGroups.find((group) => group.id === groupFilter) ?? null,
    [userGroups, groupFilter],
  );

  // The array actually rendered: the server-filtered result when one
  // exists, otherwise the client-side protocol filter over items.
  const visibleItems = useMemo(() => {
    if (filtered !== null) return filtered;
    if (protocol === "") return items;

    return items.filter((item) => String(item["type"]) === protocol);
  }, [items, protocol, filtered]);

  /** v0.9.10 organize-by (§6): one flat render array whose items are
   * either section headers or configuration rows — the virtualizer
   * sizes both, so grouped browsing keeps its O(visible) DOM cost. */
  const renderItems = useMemo<Array<RenderItem>>(() => {
    if (organizeBy === "") {
      return visibleItems.map((config) => ({
        kind: "row" as const,
        key: String(config["id"]),
        config,
      }));
    }

    const buckets = new Map<string, Config[]>();

    for (const config of visibleItems) {
      const key = organizeKeyOf(config, organizeBy);
      const bucket = buckets.get(key);

      if (bucket) {
        bucket.push(config);
      } else {
        buckets.set(key, [config]);
      }
    }

    const out: RenderItem[] = [];

    for (const key of [...buckets.keys()].sort((a, b) => a.localeCompare(b))) {
      const bucket = buckets.get(key) ?? [];

      out.push({ kind: "header" as const, key: `h:${key}`, label: key, count: bucket.length });

      for (const config of bucket) {
        out.push({ kind: "row" as const, key: String(config["id"]), config });
      }
    }

    return out;
  }, [visibleItems, organizeBy]);

  // The virtualizer must size against the array that is actually
  // RENDERED (visibleItems): sizing against the raw `items` while a
  // protocol filter narrows the list produced a huge blank scroll
  // range and wasted overscan rows.
  useEffect(() => {
    const media = window.matchMedia("(max-width: 860px)");

    const onChange = () => setNarrow(media.matches);

    media.addEventListener("change", onChange);

    return () => media.removeEventListener("change", onChange);
  }, []);

  // v0.11.0: the wide surface is a DENSE single-line table (36px rows,
  // v2rayN-class information density) — the two-line card layout stays
  // for narrow viewports. The sticky header labels the columns and
  // drives server-side sorting.
  const virtualizer = useVirtualizer({
    count: renderItems.length,
    estimateSize: (index) => (renderItems[index]?.kind === "header" ? 32 : narrow ? 88 : 36),
    overscan: 12,
    getScrollElement: () => parentRef.current,
  });

  /** v0.11.0: a column header click applies the matching server-side
   * sort (the ONE filtering pipeline — never client-side re-sorting of
   * a bounded window). Clicking the active column flips direction. */
  const sortColumn = (by: "latency" | "tested_at" | "protocol" | "address" | "source") => {
    if (sortBy === by) {
      setSortBy("");
      setSortDesc(false);

      return;
    }

    setSortBy(by);
    setSortDesc(false);
  };

  // Protocol options present in the loaded window (stable order).
  const protocolOptions = useMemo(() => {
    const present = new Set(items.map((item) => String(item["type"])));

    const preferred = PROTOCOL_FILTERS.filter((p) => present.has(p));
    const extra = [...present]
      .filter((p) => p && !PROTOCOL_FILTERS.includes(p) && p !== "unknown")
      .sort();

    return [...preferred, ...extra].slice(0, 6);
  }, [items]);

  // Keep the virtualizer window in sync with the filtered count and
  // the narrow/wide row height switch.
  useEffect(() => {
    virtualizer.measure();
  }, [renderItems.length, narrow, virtualizer]);

  // Server-side filtered view: any status filter, group or sort
  // activates it (v0.9.10: the group filter rides the SAME pipeline).
  // v0.11.2: the "all" scope is equivalent to no group filter — it
  // matches every record on the server side (groupMatchesCompiled
  // returns true for both "" and "all"), so we skip the filtered
  // round trip when the user explicitly picked All + no other filter.
  // v0.12.2: when a chain scope is active, load its authoritative
  // details (hop ids, preview, usable state) from the backend.
  useEffect(() => {
    if (!chainFilter) {
      setChainDetails(null);

      return;
    }

    let cancelled = false;

    void (async () => {
      try {
        const details = await call(() => proxyChainService.ProxyChainDetails(chainFilter));

        if (!cancelled) setChainDetails(details);
      } catch (error) {
        if (!cancelled) {
          setChainDetails(null);
          toast("error", "Chain unavailable", describeError(error));
        }
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [chainFilter]);

  const loadFiltered = useCallback(async (limit: number) => {
    // v0.12.1: a source scope is a server-side filter
    // (cfg.Source == sourceID) — the database is never downloaded to
    // filter it in React.
    if (
      statusFilter === "" &&
      sortBy === "" &&
      (groupFilter === "" || groupFilter === "all") &&
      sourceFilter === "" &&
      !chainFilter
    ) {
      setFiltered(null);

      return;
    }

    setFilteredLoading(true);

    try {
      const page = await call(() =>
        dataService.ListConfigsFiltered(
          {
            // v0.9.8.7: the truthful generated ConfigFilter declares
            // every key (values may be undefined) — spell each key out.
            protocol: protocol || undefined,
            status: statusFilter || undefined,
            query: searchQuery || undefined,
            sort_by: sortBy || undefined,
            sort_desc: sortDesc,
            source: sourceFilter || undefined,
            backend: undefined,
            group: groupFilter || undefined,
            // v0.12.2: a chain scope is an explicit id allow-list.
            ids: chainDetails?.config_ids ?? (chainFilter ? [] : undefined),
          },
          0,
          limit,
        ),
      );

      setFiltered(page?.items ?? []);
      setFilteredTotal(page?.total ?? 0);
    } catch (error) {
      toast("error", "Filter failed", describeError(error));
    } finally {
      setFilteredLoading(false);
    }
  }, [protocol, statusFilter, sortBy, sortDesc, searchQuery, groupFilter, sourceFilter, chainFilter, chainDetails]);

  useEffect(() => {
    void loadFiltered(1000);
  }, [loadFiltered, viewVersion]);

  // Live queue state while a batch is running. v0.9.15: the previous
  // Stats + Paused + Snapshot(200) triple is replaced by ONE
  // authoritative read — LiveState — whose live fingerprint set is
  // COMPLETE. The complete set is what lets testProgress decide
  // terminal transitions without false completions (a task outside a
  // bounded page used to be misread as finished), and one call instead
  // of three removes two-thirds of the binding traffic.
  //
  // Push + pull, one source of truth: the backend emits
  // freeiran:queuestate (the SAME complete LiveStateView shape) on
  // every coalesced queue change; this page applies it immediately and
  // keeps a slow-cadence LiveState read as the recovery path for
  // missed events / background throttling. The backend queue stays
  // authoritative — this store is only its projection.
  useEffect(() => {
    let stop = false;
    let timer = 0;

    const cadence = (stats: QueueStatsView | null) =>
      stats && (stats.queue_depth > 0 || stats.active_workers > 0) ? 2000 : 5000;

    const consume = (view: QueueLiveStateView | null) => {
      if (!view) return;

      setQueueStats(view.stats as QueueStatsView);
      setQueuePaused(Boolean(view.paused));

      // The COMPLETE live set drives completion + result patches; the
      // bounded detail page (Snapshot) is fetched no longer — detail
      // states come from the same LiveState event when the backend
      // includes richer words (fingerprints render as queued).
      applyLiveState(view);
    };

    const tick = async () => {
      let next = 5000;

      try {
        const view = (await call(() => testQueueService.LiveState())) as QueueLiveStateView | null;

        if (!stop) {
          consume(view);
          next = cadence((view?.stats ?? null) as QueueStatsView | null);
        }
      } catch {
        /* polling is best-effort */
      }

      if (!stop) timer = window.setTimeout(tick, next);
    };

    const offQueueState = Events.On("freeiran:queuestate", (event: { data: QueueLiveStateView }) => {
      if (!stop) consume(event?.data ?? null);
    });

    void tick();

    return () => {
      stop = true;
      window.clearTimeout(timer);
      offQueueState();
    };
  }, []);

  // v0.9.15: completed tests patch the server-filtered view in place
  // (the store-backed list is patched by testProgress itself; this
  // covers the local filtered snapshot) and refresh an open detail
  // panel through the REDACTED detail binding (the detail surface
  // must never receive the raw config model). Selection, sorting,
  // scroll position and expansion all survive — the arrays keep
  // their shape and identity per row.
  useEffect(() => {
    let stale = false;

    const off = onTestResult((fingerprint, fresh) => {
      setFiltered((prev) =>
        prev === null
          ? prev
          : prev.some((item) => String(item["id"]) === fingerprint)
            ? prev.map((item) =>
                String(item["id"]) === fingerprint ? { ...item, ...fresh } : item,
              )
            : prev,
      );

      void refreshDetail(fingerprint);
    });

    const refreshDetail = async (fingerprint: string) => {
      try {
        const fresh = await call(() => connectionService.ConfigDetails(fingerprint));

        if (!stale && fresh) {
          // v0.11.2: refresh the open TAB whose stable id matches the
          // fingerprint, not a single detail slot. Tabs not matching
          // are left untouched; the active tab is the one whose detail
          // is mutated in place.
          setOpenTabs((tabs) =>
            tabs.map((tab) => (tab.id === fingerprint ? (fresh as ConfigDetail) : tab)),
          );
        }
      } catch {
        /* the config may have been removed while its test ran */
      }
    };

    return () => {
      stale = true;
      off();
    };
  }, []);

  const onListScroll = () => {
    // v0.9.13: an open row context menu is anchored to viewport
    // coordinates — scrolling the list would desync it from its row.
    // (MenuSurface also repositions on any scroll; closing here keeps
    // the interaction predictable for fast list scrolls.)
    setContextMenu(null);

    const element = parentRef.current;

    if (!element) return;

    if (element.scrollTop + element.clientHeight >= element.scrollHeight - 400) {
      void loadMore();
    }
  };

  // v0.12.1 (§30) KEYBOARD UX — professional desktop-client basics:
  //
  //   Ctrl+A  select every configuration in the visible scope
  //   Delete/Backspace  the EXISTING supported removal in the active
  //           context (leave the current user group)
  //
  // Text fields keep their native behaviour (the handler yields when
  // an editable element owns the focus). Shift+F10 / ContextMenu stay
  // row-level (already implemented on every row).
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const editable =
        target &&
        (target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.tagName === "SELECT" ||
          target.isContentEditable);

      if ((event.ctrlKey || event.metaKey) && (event.key === "a" || event.key === "A") && !editable) {
        // Ctrl+A: select the visible scope (bounded by the filter —
        // selection drives the existing bulk actions).
        event.preventDefault();

        setSelected((prev) => {
          const next = new Set(prev);

          for (const item of visibleItems) {
            next.add(String(item["id"]));
          }

          return next;
        });

        return;
      }

      if ((event.key === "Delete" || event.key === "Backspace") && !editable) {
        // Delete in a user-group scope = the existing membership
        // removal. Nowhere else does Delete have a supported action.
        const group = userGroups.find((candidate) => candidate.id === groupFilter);

        if (!group || selected.size === 0) return;

        event.preventDefault();

        const ids = [...selected];

        void useCollectionsStore
          .getState()
          .removeManyFromGroup(group.id, ids)
          .then(({ removed, failed, firstError }) => {
            if (failed === 0) {
              toast("success", `Removed ${removed} from ${group.name}`);
            } else if (removed > 0) {
              toast("warn", `Removed ${removed} of ${ids.length}`, `${failed} failed${firstError ? `: ${firstError}` : ""}`);
            } else {
              toast("error", `Could not remove from ${group.name}`, firstError ?? "All removals failed.");
            }

            setSelected(new Set());
            setViewVersion((v) => v + 1);
          });
      }
    };

    document.addEventListener("keydown", onKeyDown);

    return () => document.removeEventListener("keydown", onKeyDown);
  }, [visibleItems, selected, userGroups, groupFilter]);

    const exportCSV = () => {
    const worker = new ExportWorker();

    worker.postMessage({
      type: "export",
      rows: items.map(configToRow),
    });

    worker.onmessage = (event: MessageEvent<{ type: string; csv: string }>) => {
      if (event.data.type !== "export:done") return;

      const blob = new Blob([event.data.csv], { type: "text/csv" });
      const url = URL.createObjectURL(blob);

      const anchor = document.createElement("a");

      anchor.href = url;
      anchor.download = "freeiran-export.csv";
      anchor.click();

      URL.revokeObjectURL(url);
      worker.terminate();
      toast("success", "Export ready", "CSV downloaded.");
    };
  };


  /**
   * v0.9.8.3 manual ordering: move a configuration by STABLE ID within
   * the complete ordered collection. Only offered on the unfiltered,
   * unsorted view (the stored order is one authority — filtered views
   * must never corrupt it). After the move the list reloads from the
   * backend, so persistence and restart behavior stay observable.
   */
  const moveConfig = async (config: Config, direction: -1 | 1) => {
    const id = String(config["id"]);
    const index = visibleItems.findIndex((item) => String(item["id"]) === id);
    if (index < 0) return;

    const target = index + direction;
    if (target < 0 || target >= visibleItems.length) return;

    try {
      await call(() => dataService.MoveConfig(id, target));
      await useConfigsStore.getState().runSearch();
    } catch (error) {
      toast("error", "Reorder failed", describeError(error));
    }
  };

  // v0.9.15: single test = enqueue through the ONE authoritative
  // testing engine and return promptly. The backend collapses repeated
  // clicks into the queued task; the row's live chip updates instantly
  // (optimistic mark) and the result lands through the shared
  // incremental patch path — NO full-list refresh (a runSearch() over
  // ~17,000 configs per click was the wrong update model).
  const testConfig = async (config: Config) => {
    const id = String(config["id"]);

    markTestRequested(id);

    try {
      await dataService.TestConfig(id);
      toast("info", "Test queued", `${String(config["address"])}: queued for testing.`);
    } catch (error) {
      clearTestRequested(id);
      toast("error", "Test failed", describeError(error));
    }
  };

  const bulkTest = async (scope: string) => {
    try {
      const result = await call(() =>
        testQueueService.EnqueueByFilter({
          scope,
          fingerprints: scope === "selected" ? [...selected] : undefined,
          protocol: protocol || undefined,
          // v0.12.1: bulk tests honour the active source scope — the
          // ONE shared queue stays the sole testing authority.
          source: sourceFilter || undefined,
          limit: undefined,
          priority: undefined,
          // v0.11.0: the UI's testing actions are EXPLICIT user
          // actions — "Test selected" can never silently become a
          // background untested sweep.
          origin: "user",
        }),
      );

      if (result && result.enqueued === 0) {
        toast("info", "Nothing to test", "No configuration matched this scope.");
      } else {
        toast("success", "Test batch queued", `${result?.enqueued ?? 0} configuration(s) queued.`);
      }
    } catch (error) {
      toast("error", "Batch test failed", describeError(error));
    }
  };

  // v0.12.1 (§17/§18): the active source scope's OWN evidence view.
  const activeSourceScope = useMemo(

    () => sourceStats.find((source) => source.id === sourceFilter) ?? null,
    [sourceStats, sourceFilter],
  );

  // v0.12.2: the active chain scope's authoritative details.
  const activeChainScope = chainFilter ? chainDetails : null;

  /** Check chain — per-hop evidence + fresh end-to-end, honestly reported. */
  const checkActiveChain = async () => {
    if (!activeChainScope) return;

    setChainBusy(true);

    try {
      const result = await call(() => proxyChainService.CheckChain(activeChainScope.id));

      if (result.end_to_end.ok) {
        toast("success", "Chain verified", `End-to-end ${result.end_to_end.ping_ms ?? 0} ms through ${result.end_to_end.backend}.`);
      } else {
        toast("error", "Chain check failed", result.end_to_end.last_error || "No usable end-to-end connection was verified.");
      }
    } catch (error) {
      toast("error", "Chain check failed", describeError(error));
    } finally {
      setChainBusy(false);
    }
  };

  /** Connect chain — the SAME verified state machine, one core process. */
  const connectActiveChain = async () => {
    if (!activeChainScope) return;

    setChainBusy(true);

    try {
      await call(() => connectionService.ConnectChain(activeChainScope.id));

      toast("success", "Chain connected", `${activeChainScope.name} verified and connected.`);
    } catch (error) {
      toast("error", "Chain connection failed", describeError(error));
    } finally {
      setChainBusy(false);
    }
  };

  // v0.12.1 (§18): update ONE source through the targeted refresh —
  // the same ingestion architecture, never a second pipeline.
  const updateActiveSource = async () => {
    if (!activeSourceScope || updatingSource) return;

    setUpdatingSource(true);

    try {
      const stats = await call(() => sourceService.RefreshSource(activeSourceScope.id));

      await refreshSourceStats();

      setViewVersion((v) => v + 1);

      if (stats) {
        toast(
          "success",
          "Source updated",
          `${stats.name}: ${stats.config_count} configurations, ${stats.working_count} working.`,
        );
      } else {
        toast("success", "Source updated");
      }
    } catch (error) {
      toast("error", "Source update failed", describeError(error));
    } finally {
      setUpdatingSource(false);
    }
  };

  // v0.12.1 (§19): check ONE source through the shared bounded queue
  // (EnqueueByFilter with source = selectedSourceID). The UI never
  // loops and launches test processes itself.
  const checkActiveSource = async (scope: "all" | "untested" | "failed" | "working") => {
    if (!activeSourceScope) return;

    try {
      const result = await call(() =>
        testQueueService.EnqueueByFilter({
          scope,
          fingerprints: undefined,
          protocol: undefined,
          source: activeSourceScope.id,
          limit: undefined,
          priority: undefined,
          // Explicit user action — never a background sweep.
          origin: "user",
        }),
      );

      if (result && result.enqueued === 0) {
        toast("info", "Nothing to test", "No configuration matched this scope.");
      } else {
        toast("success", "Source check queued", `${result?.enqueued ?? 0} configuration(s) queued.`);
      }
    } catch (error) {
      toast("error", "Source check failed", describeError(error));
    }
  };

  const cancelAll = async () => {
    try {
      await call(() => testQueueService.CancelAll());
      toast("info", "Queue cleared", "Queued tests were cancelled; running tests finish or abort.");
    } catch (error) {
      toast("error", "Cancel failed", describeError(error));
    }
  };

  // v0.9.7: pause/resume the testing queue from the control bar.
  const togglePause = async () => {
    try {
      if (queuePaused) {
        await call(() => testQueueService.Resume());
        setQueuePaused(false);
        toast("info", "Queue resumed", "Queued tests continue.");
      } else {
        await call(() => testQueueService.Pause());
        setQueuePaused(true);
        toast("info", "Queue paused", "In-flight tests finish; queued tests wait.");
      }
    } catch (error) {
      toast("error", "Queue control failed", describeError(error));
    }
  };

  const toggleSelect = (id: string, checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);

      if (checked) {
        next.add(id);
      } else {
        next.delete(id);
      }

      return next;
    });
  };

  // Details view: credential material is redacted server-side; this
  // surface only receives presence flags.
  //
  // v0.11.2: opening a config that is ALREADY open FOCUSES that tab
  // instead of creating a duplicate. The tabs are keyed by stable
  // configuration ID; opening A then B then A leaves the rail as
  // `All | A | B` with A focused. The detail data is refreshed
  // in-place (so the focus also serves as a refresh action).
  const showDetails = async (config: Config) => {
    const id = String(config["id"]);

    // If the tab is already open, focus + refresh it (no duplicate).
    const existing = openTabs.find((tab) => tab.id === id);
    if (existing) {
      setActiveTabId(id);

      try {
        const fresh = await call(() => connectionService.ConfigDetails(id));
        if (fresh) {
          setOpenTabs((tabs) => tabs.map((tab) => (tab.id === id ? (fresh as ConfigDetail) : tab)));
        }
      } catch (error) {
        toast("error", "Details unavailable", describeError(error));
      }

      return;
    }

    try {
      const result = await call(() => connectionService.ConfigDetails(id));
      const next = result as ConfigDetail | null;
      if (!next) {
        return;
      }

      setOpenTabs((tabs) => [...tabs, next]);
      setActiveTabId(id);
    } catch (error) {
      toast("error", "Details unavailable", describeError(error));
    }
  };

  // closeConfigTab removes one tab from the rail. If the closed tab
  // was focused, focus moves to the previous tab (or back to the
  // wide `All` view when no tabs remain).
  const closeConfigTab = (id: string) => {
    setOpenTabs((tabs) => {
      const idx = tabs.findIndex((tab) => tab.id === id);
      if (idx === -1) {
        return tabs;
      }

      const next = tabs.filter((tab) => tab.id !== id);

      if (activeTabId === id) {
        // Move focus to the previous tab, or back to All when none.
        const prev = next[idx - 1] ?? next[0] ?? null;
        setActiveTabId(prev?.id ?? null);
      }

      return next;
    });
  };

  // closeOtherTabs keeps only the focused tab. Useful when the user
  // has accumulated many open configs and wants to clear the rail.
  const closeOtherTabs = () => {
    if (!activeTabId) {
      return;
    }

    setOpenTabs((tabs) => tabs.filter((tab) => tab.id === activeTabId));
  };

  // closeAllTabs returns the focus to the wide `All` view and clears
  // every open config tab. Stable IDs are preserved (the configs
  // themselves are untouched; only the open-tab rail is reset).
  const closeAllTabs = () => {
    setOpenTabs([]);
    setActiveTabId(null);
  };

  // v0.9.13: connect straight from a row/menu — the SAME connection
  // flow the detail panel uses (no new networking path).
  const connectToConfig = async (config: Config) => {
    try {
      await useConnectionStore.getState().connect(String(config["id"]));

      const error = useConnectionStore.getState().error;

      if (error) {
        toast("error", "Connection failed", error);
      } else {
        toast("success", "Connecting", "The connection state machine is starting.");
      }
    } catch (error) {
      toast("error", "Connection failed", describeError(error));
    }
  };

  // v0.9.13: copy the SAFE endpoint (address:port — exactly the values
  // already displayed on the row; no credential material, no full
  // configuration URL, honoring the redaction rules).
  const copyEndpoint = (config: Config) => {
    const endpoint = `${String(config["address"])}:${String(config["port"])}`;

    void navigator.clipboard
      .writeText(endpoint)
      .then(() => toast("success", "Endpoint copied", endpoint))
      .catch((error: unknown) => {
        toast("error", "Copy failed", describeError(error));
      });
  };

  // v0.9.13: the single action model for one configuration — rendered
  // by the ⋮ overflow button, the right-click context menu and the
  // keyboard context-menu invocation alike.
  const rowMenuItems = (config: Config): MenuItem[] => {
    const id = String(config["id"]);
    const name = String(config["name"] || "configuration");
    const tested = Number(config["tested_at"] ?? 0) > 0;
    const live = testStates[id] !== undefined;
    const reorderable = statusFilter === "" && sortBy === "" && !searchQuery && organizeBy === "" && (groupFilter === "" || groupFilter === "all");
    const visibleIndex = visibleItems.findIndex((item) => String(item["id"]) === id);

    const items: MenuItem[] = [
      {
        id: "test",
        label: live ? "Testing…" : tested ? "Retest" : "Test",
        disabled: live,
        onSelect: () => void testConfig(config),
      },
      {
        id: "connect",
        label: "Connect",
        disabled: connectBusy,
        onSelect: () => void connectToConfig(config),
      },
      {
        id: "favorite",
        label: favorites.includes(id) ? `Remove ${name} from favorites` : `Save ${name} as favorite`,
        onSelect: () => {
          void toggleFavorite(id).catch((error: unknown) => {
            toast("error", "Could not update favorites", describeError(error));
          });
        },
      },
      {
        id: "select",
        label: selected.has(id) ? "✓ Selected for bulk testing" : "Select for bulk testing",
        onSelect: () => toggleSelect(id, !selected.has(id)),
      },
    ];

    let firstGroup = true;

    for (const group of userGroups) {
      items.push({
        id: `group-${group.id}`,
        label: `Add to ${group.name}`,
        separatorBefore: firstGroup,
        onSelect: () => {
          void useCollectionsStore
            .getState()
            .addToGroup(group.id, id)
            .then(() => toast("success", `Added to ${group.name}`))
            .catch((error: unknown) => {
              toast("error", "Could not add to group", describeError(error));
            });
        },
      });

      firstGroup = false;
    }

    // v0.11.0: when browsing a user group, the row menu offers honest
    // membership removal — the group scope is the visible membership
    // contract, so removal acts on the ACTIVE group only.
    const activeUserGroup = userGroups.find((group) => group.id === groupFilter);

    if (activeUserGroup) {
      items.push({
        id: "remove-from-active-group",
        label: `Remove from ${activeUserGroup.name}`,
        separatorBefore: true,
        onSelect: () => {
          void useCollectionsStore
            .getState()
            .removeFromGroup(activeUserGroup.id, id)
            .then(() => {
              toast("success", `Removed from ${activeUserGroup.name}`);

              // Membership changed: the filtered view, selection, group
              // counts and the detail panel must reconcile immediately —
              // without a full page reload. Bumping the view version
              // re-runs the ONE server-side filter; the collections
              // reload refreshed the counts.
              setViewVersion((v) => v + 1);
            })
            .catch((error: unknown) => {
              toast("error", "Could not remove from group", describeError(error));
            });
        },
      });
    }

    items.push(
      ...(() => {
        const selectedIds = Array.from(selected);
        const inChainScope = Boolean(chainFilter);

        const items: MenuItem[] = [];

        if (inChainScope && chainDetails) {
          const hopPosition = chainDetails.config_ids.indexOf(config.id);

          if (hopPosition >= 0) {
            items.push({
              id: "chain-remove",
              label: "Remove from chain",
              disabled: chainDetails.config_ids.length <= 2,
              onSelect: () => {
                void (async () => {
                  try {
                    await call(() => proxyChainService.RemoveHop(chainFilter, config.id));
                    toast("success", "Hop removed", "The chain was updated.");
                  } catch (error) {
                    toast("error", "Could not remove hop", describeError(error));
                  }
                })();
              },
            });

            if (hopPosition > 0) {
              items.push({
                id: "chain-earlier",
                label: "Move earlier in chain",
                onSelect: () => {
                  void (async () => {
                    try {
                      await call(() => proxyChainService.ReorderHop(chainFilter, config.id, hopPosition - 1));
                    } catch (error) {
                      toast("error", "Could not reorder chain", describeError(error));
                    }
                  })();
                },
              });
            }

            if (hopPosition < chainDetails.config_ids.length - 1) {
              items.push({
                id: "chain-later",
                label: "Move later in chain",
                onSelect: () => {
                  void (async () => {
                    try {
                      await call(() => proxyChainService.ReorderHop(chainFilter, config.id, hopPosition + 1));
                    } catch (error) {
                      toast("error", "Could not reorder chain", describeError(error));
                    }
                  })();
                },
              });
            }
          }
        }

        if (selectedIds.length >= 2) {
          items.push({
            id: "chain-from-selected",
            label: `Build proxy chain from selected (${selectedIds.length})`,
            onSelect: () => {
              setChainEditor({ open: true, id: null });
              pendingChainHops.current = selectedIds;
            },
          });
        }

        if (selectedIds.length >= 1) {
          items.push({
            id: "chain-add-selected",
            label: "Add selected to a chain",
            disabled: chains.length === 0,
            onSelect: () => {
              void (async () => {
                if (chains.length === 1) {
                  try {
                    for (const id of selectedIds) {
                      await call(() => proxyChainService.AddHop(chains[0].id, id, -1));
                    }

                    toast("success", "Added to chain", chains[0].name);
                    void loadChains();
                  } catch (error) {
                    toast("error", "Could not add to chain", describeError(error));
                  }

                  return;
                }

                toast(
                  "info",
                  "Pick a chain",
                  chains.length === 0
                    ? "Create a chain first (+ New chain)."
                    : "Open the chain's Edit view to place these hops in order.",
                );
              })();
            },
          });
        }

        return items;
      })(),

      {
        id: "move-up",
        label: "Move up",
        separatorBefore: true,
        disabled: !reorderable || visibleIndex <= 0,
        onSelect: () => void moveConfig(config, -1),
      },
      {
        id: "move-down",
        label: "Move down",
        disabled: !reorderable || visibleIndex < 0 || visibleIndex >= visibleItems.length - 1,
        onSelect: () => void moveConfig(config, 1),
      },
      {
        id: "details",
        label: "View details",
        separatorBefore: true,
        onSelect: () => void showDetails(config),
      },
      {
        id: "copy",
        label: "Copy endpoint",
        onSelect: () => copyEndpoint(config),
      },
    );

    return items;
  };

  return (
    <div className="page-flex">
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Configurations</h1>
          <div className="page-subtitle">
            {filtered !== null
              ? `${formatNumber(filtered.length)} shown · ${formatNumber(filteredTotal)} matched`
              : `${formatNumber(items.length)} shown · ${formatNumber(total)} total`}
            {hasMore && searchQuery.trim() === "" && filtered === null ? " · scroll to load more" : ""}
          </div>
        </div>

        {/*
         * v0.9.13: view-level secondary controls (organize-by, export)
         * moved behind progressive disclosure — the primary toolbar
         * keeps search, status, protocol and sort visible.
         */}
        <div className="page-actions">
          <Menu
            ariaLabel="View and export options"
            label={
              <>
                View
                <IconChevronDown size={13} />
              </>
            }
            items={[
              {
                id: "organize-none",
                label: organizeBy === "" ? "✓ Group by: nothing" : "Group by: nothing",
                onSelect: () => setOrganizeBy(""),
              },
              {
                id: "organize-source",
                label: organizeBy === "source" ? "✓ Group by: source" : "Group by: source",
                onSelect: () => setOrganizeBy("source"),
              },
              {
                id: "organize-protocol",
                label: organizeBy === "protocol" ? "✓ Group by: protocol" : "Group by: protocol",
                onSelect: () => setOrganizeBy("protocol"),
              },
              {
                id: "organize-status",
                label: organizeBy === "status" ? "✓ Group by: status" : "Group by: status",
                onSelect: () => setOrganizeBy("status"),
              },
              {
                id: "export",
                label: "Export CSV",
                separatorBefore: true,
                onSelect: () => exportCSV(),
              },
            ]}
          />
        </div>
      </div>

      {lastError && <div className="error-banner">{lastError}</div>}

      {/*
       * v0.9.10 GROUPS RAIL (§6 Configuration grouping): built-in
       * evidence groups with live counts + the user's own groups.
       * One active group at a time; counts come from the backend's
       * measured evidence — never invented.
       */}
      <div className="toolbar config-groups" role="group" aria-label="Configuration groups">
        {builtinGroups.map((group) => {
          // v0.11.2: `All` is the first-class scope. It is rendered
          // ACTIVE whenever the effective filter is "all" OR the
          // empty-string fallback (both spellings mean "every config
          // at the data layer"); clicking All when it is already
          // active is a no-op so the user cannot accidentally end up
          // in a "no group selected" limbo — All is the safe default.
          const isAll = group.id === "all";
          const allActive = groupFilter === "all" || groupFilter === "";
          const active = isAll ? allActive : groupFilter === group.id;
          const onClick = isAll
            ? () => setGroupFilter("all")
            : () => setGroupFilter(groupFilter === group.id ? "all" : group.id);

          return (
            <button
              key={group.id}
              type="button"
              className={`group-chip ${active ? "active" : ""}`}
              aria-pressed={active}
              title={BUILTIN_GROUP_HINTS[group.id] ?? undefined}
              onClick={onClick}
            >
              {BUILTIN_GROUP_LABELS[group.id] ?? group.id}
              <span className="group-count">{group.count}</span>
            </button>
          );
        })}

        {/*
         * v0.12.1 (§16) SOURCE SCOPES: one chip per source/subscription
         * group with its authoritative configuration count. Selecting a
         * source clears the group scope — the two are independent
         * scopes of the same ONE server-side filter pipeline.
         */}
        {sourceStats.length > 0 && <span className="toolbar-divider" aria-hidden />}

        {sourceStats.map((source) => {
          const active = sourceFilter === source.id;

          return (
            <button
              key={source.id}
              type="button"
              className={`group-chip group-chip-source ${active ? "active" : ""}`}
              aria-pressed={active}
              title={`${source.name} — ${source.config_count} configurations (${source.working_count} working)`}
              onClick={() => {
                setSourceFilter(active ? "" : source.id);
                setGroupFilter("all");
                setStatusFilter("");
              }}
            >
              {source.name}
              <span className="group-count">{source.config_count}</span>
            </button>
          );
        })}

        {userGroups.length > 0 && <span className="toolbar-divider" aria-hidden />}

        {userGroups.map((group) => (
          <span key={group.id} className="group-chip-wrap">
            <button
              type="button"
              className={`group-chip ${groupFilter === group.id ? "active" : ""}`}
              aria-pressed={groupFilter === group.id}
              onClick={() => setGroupFilter(groupFilter === group.id ? "" : group.id)}
            >
              {group.name}
              <span className="group-count">{group.count}</span>
            </button>
            <button
              type="button"
              className="fav-toggle"
              style={{ width: 18, height: 18, fontSize: 11 }}
              aria-label={`Rename group ${group.name}`}
              title="Rename this group"
              onClick={() => setRenamingGroup({ id: group.id, name: group.name })}
            >
              ✎
            </button>
            <button
              type="button"
              className="fav-toggle"
              style={{ width: 18, height: 18, fontSize: 11 }}
              aria-label={`Delete group ${group.name}`}
              title="Delete this group (configurations are kept)"
              onClick={() => {
                if (groupFilter === group.id) setGroupFilter("");

                void useCollectionsStore.getState().deleteUserGroup(group.id).catch((error: unknown) => {
                  toast("error", "Could not delete group", describeError(error));
                });
              }}
            >
              ×
            </button>
          </span>
        ))}

        <button
          type="button"
          className="group-chip"
          aria-haspopup="dialog"
          aria-expanded={newGroupOpen}
          onClick={() => {
            setNewGroupOpen(true);
          }}
        >
          + New group
        </button>

        <span className="toolbar-spacer" />
      </div>

      {newGroupOpen && (
        <div className="toolbar">
          <input
            className="input"
            placeholder="Group name (e.g. Work, Personal, Travel)"
            aria-label="New group name"
            value={newGroupName}
            autoFocus
            onChange={(event) => setNewGroupName(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Escape") setNewGroupOpen(false);
            }}
          />
          <button
            type="button"
            className="btn primary sm"
            disabled={!newGroupName.trim()}
            onClick={() => {
              const name = newGroupName.trim();

              if (!name) return;

              setNewGroupName("");
              setNewGroupOpen(false);

              void useCollectionsStore
                .getState()
                .createUserGroup(name)
                .catch((error: unknown) => {
                  toast("error", "Could not create group", describeError(error));
                });
            }}
          >
            Create
          </button>
          <button type="button" className="btn sm" onClick={() => setNewGroupOpen(false)}>
            Cancel
          </button>
        </div>
      )}

      {renamingGroup && (
        <div className="toolbar">
          <input
            className="input"
            placeholder="New group name"
            aria-label="Rename group"
            value={renamingGroup.name}
            autoFocus
            onChange={(event) => setRenamingGroup({ ...renamingGroup, name: event.target.value })}
            onKeyDown={(event) => {
              if (event.key === "Escape") setRenamingGroup(null);
            }}
          />
          <button
            type="button"
            className="btn primary sm"
            disabled={!renamingGroup.name.trim()}
            onClick={() => {
              const { id, name } = renamingGroup;

              setRenamingGroup(null);

              void useCollectionsStore
                .getState()
                .renameUserGroup(id, name.trim())
                .then(() => toast("success", "Group renamed"))
                .catch((error: unknown) => {
                  toast("error", "Could not rename group", describeError(error));
                });
            }}
          >
            Rename
          </button>
          <button type="button" className="btn sm" onClick={() => setRenamingGroup(null)}>
            Cancel
          </button>
        </div>
      )}

        {/* v0.12.2: PROXY CHAIN scopes — ordered hop lists over existing
            configurations. A chain scope is NOT a remote source: it
            offers Check / Connect / Edit, never Update. */}
        {chains.length > 0 && <span className="toolbar-divider" aria-hidden />}
        {chains.map((chain) => {
          const active = chainFilter === chain.id;

          return (
            <button
              key={chain.id}
              type="button"
              className={`group-chip group-chip-chain ${active ? "active" : ""}`}
              aria-pressed={active}
              title={`${chain.name} — ${chain.hops} hop${chain.hops === 1 ? "" : "s"}`}
              onClick={() => {
                setChainFilter(active ? "" : chain.id);
                setSourceFilter("");
                setGroupFilter("all");
                setStatusFilter("");
              }}
            >
              {chain.name}
              <span className="group-count">{chain.hops}</span>
            </button>
          );
        })}

        <button
          type="button"
          className="group-chip group-chip-new"
          title="Build a proxy chain from two or more configurations"
          onClick={() => setChainEditor({ open: true, id: null })}
        >
          + New chain
        </button>

      {/*
       * v0.12.1 (§17) SOURCE SCOPE HEADER: when a real source/
       * subscription group is selected, a compact header shows the
       * backend's MEASURED evidence (counts, freshness, trust band)
       * and the per-source actions. A user group is NOT a remote
       * source — it never gets an Update action.
       */}
      {activeSourceScope && (
        <section className="source-scope-header" aria-label="Source scope">
          <div className="source-scope-main">
            <span className="source-scope-name" title={activeSourceScope.id}>
              {activeSourceScope.name}
            </span>
            <span className={`badge ${activeSourceScope.enabled ? "success" : "neutral"}`}>
              {activeSourceScope.enabled ? "Enabled" : "Disabled"}
            </span>
            <span className={`badge ${trustBadgeClass(activeSourceScope.trust)}`}>
              {activeSourceScope.trust || "public"}
            </span>
          </div>

          <div className="source-scope-evidence">
            <span>
              {formatNumber(activeSourceScope.config_count)} configurations
            </span>
            <span>
              {formatNumber(activeSourceScope.working_count)} working
            </span>
            {activeSourceScope.last_successful_fetch && (
              <span title={new Date(activeSourceScope.last_successful_fetch).toLocaleString()}>
                last fetch {relativeTime(new Date(activeSourceScope.last_successful_fetch).getTime())}
              </span>
            )}
            {activeSourceScope.last_failure && (
              <span
                className="source-scope-failure"
                title={activeSourceScope.last_failure_reason || undefined}
              >
                failed {relativeTime(new Date(activeSourceScope.last_failure).getTime())}
                {activeSourceScope.last_failure_reason ? ` — ${activeSourceScope.last_failure_reason}` : ""}
              </span>
            )}
          </div>

          <div className="source-scope-actions">
            <button
              type="button"
              className="btn sm primary"
              disabled={updatingSource}
              onClick={() => void updateActiveSource()}
            >
              {updatingSource ? <span className="btn-spinner" aria-hidden /> : <IconRefresh size={13} />}
              {updatingSource ? "Updating…" : "Update source"}
            </button>
            <button
              type="button"
              className="btn sm"
              disabled={updatingSource}
              onClick={() => void checkActiveSource("all")}
            >
              <IconPlay size={13} /> Check source
            </button>
            <button
              type="button"
              className="btn sm"
              disabled={updatingSource}
              onClick={() => void checkActiveSource("untested")}
            >
              Check untested
            </button>
            <Menu
              ariaLabel="More source actions"
              label={
                <>
                  More
                  <IconChevronDown size={13} />
                </>
              }
              items={[
                {
                  id: "check-failed",
                  label: "Retry failed in source",
                  disabled: updatingSource,
                  onSelect: () => void checkActiveSource("failed"),
                },
                {
                  id: "check-working",
                  label: "Retest working in source",
                  disabled: updatingSource,
                  onSelect: () => void checkActiveSource("working"),
                },
              ]}
            />
            <button type="button" className="btn sm ghost" onClick={() => setSourceFilter("")}>
              <IconX size={13} /> Exit scope
            </button>
          </div>
        </section>
      )}

      {activeChainScope && (
        <section className="source-scope-header" aria-label="Proxy chain scope">
          <div className="source-scope-main">
            <span className="source-scope-name" title={activeChainScope.id}>
              {activeChainScope.name}
            </span>
            <span className="badge neutral">
              {activeChainScope.hops.length} hop{activeChainScope.hops.length === 1 ? "" : "s"}
            </span>
            <span className={`badge ${activeChainScope.usable ? "success" : "error"}`}>
              {activeChainScope.usable ? "Complete" : "Missing hops"}
            </span>
            <span className="chain-scope-preview mono" title={activeChainScope.preview}>
              {activeChainScope.preview}
            </span>
          </div>

          <div className="source-scope-evidence">
            <span>
              {activeChainScope.hops.filter((hop) => hop.working).length}/{activeChainScope.hops.length} hops working
            </span>
            <span>
              {activeChainScope.hops.every((hop) => hop.tested_at) ? "all hops tested" : "some hops untested"}
            </span>
          </div>

          <div className="source-scope-actions">
            <button
              type="button"
              className="btn sm"
              disabled={chainBusy}
              onClick={() => void checkActiveChain()}
            >
              <IconPlay size={13} /> {chainBusy ? "Checking…" : "Check chain"}
            </button>
            <button
              type="button"
              className="btn sm primary"
              disabled={!activeChainScope.usable}
              onClick={() => void connectActiveChain()}
            >
              <IconZap size={13} /> Connect
            </button>
            <button
              type="button"
              className="btn sm"
              onClick={() => setChainEditor({ open: true, id: activeChainScope.id })}
            >
              Edit
            </button>
            <button type="button" className="btn sm ghost" onClick={() => setChainFilter("")}>
              <IconX size={13} /> Exit scope
            </button>
          </div>
        </section>
      )}

      {/*
       * SEARCH / FILTER TOOLBAR (§6: the testing controls live in their
       * own dedicated bar below — the two concerns never compete).
       */}
      <div className="toolbar">
        <button
          type="button"
          className="btn sm primary"
          onClick={() => setImportOpen(true)}
        >
          Import configurations
        </button>

        <input
          className="input"
          placeholder="Search by address, name or protocol…"
          aria-label="Search configurations"
          value={searchQuery}
          onChange={(event) => {
            setSearchQuery(event.target.value);
            searchRunner();
          }}
        />

        <SegmentedControl
          ariaLabel="Status filter"
          value={statusFilter}
          onChange={(value) => setStatusFilter(value)}
          options={[
            { value: "", label: "All" },
            { value: "working", label: "Working" },
            { value: "failed", label: "Failed" },
            { value: "untested", label: "Untested" },
          ]}
        />

        <select
          className="input slim"
          aria-label="Sort by"
          value={sortBy}
          onChange={(event) => setSortBy(event.target.value as typeof sortBy)}
        >
          <option value="">Default order</option>
          <option value="latency">Fastest ping</option>
          <option value="tested_at">Recently tested</option>
          <option value="protocol">Protocol</option>
        </select>

        <div className="segmented" role="radiogroup" aria-label="Protocol filter">
          <button
            type="button"
            role="radio"
            aria-checked={protocol === ""}
            className={`segmented-item ${protocol === "" ? "active" : ""}`}
            onClick={() => setProtocol("")}
          >
            All
          </button>

          {protocolOptions.map((option) => (
            <button
              key={option}
              type="button"
              role="radio"
              aria-checked={protocol === option}
              className={`segmented-item ${protocol === option ? "active" : ""}`}
              onClick={() => setProtocol(option)}
            >
              {protocolLabel(option)}
            </button>
          ))}
        </div>
      </div>

      {/*
       * TESTING CONTROL BAR (v0.9.7 §6): a dedicated testing control
       * area between the search toolbar and the table. Primary bulk
       * actions stay visible; pause/resume + recovery actions live in
       * the overflow menu so the bar can never overflow into the list.
       */}
      <div className="testing-bar" role="toolbar" aria-label="Testing controls">
        <span className="testing-bar-label">
          <IconRefresh size={14} />
          Testing
        </span>

        <button
          type="button"
          className="btn sm primary"
          disabled={filteredLoading || selected.size === 0}
          onClick={() => void bulkTest("selected")}
        >
          <IconPlay size={13} /> Test selected ({selected.size})
        </button>
        <button type="button" className="btn sm" disabled={filteredLoading} onClick={() => void bulkTest("all")}>
          <IconRefresh size={14} /> Test all
        </button>
        <button type="button" className="btn sm" disabled={filteredLoading} onClick={() => void bulkTest("untested")}>
          <IconRefresh size={14} /> Test untested
        </button>

        {/* v0.9.10: add the selected rows to one of the user's groups
            (persistent, stable IDs — configurations are never
            duplicated). Hidden until a selection exists so the default
            bar stays simple (progressive disclosure). */}
        {selected.size > 0 && userGroups.length > 0 && (
          <select
            className="input slim"
            aria-label="Add selected to group"
            defaultValue=""
            onChange={(event) => {
              const groupID = event.target.value;

              if (!groupID) return;

              const ids = [...selected];

              event.target.value = ""; // reset for the next use

              // v0.11.0 honest result handling: a partial or total
              // failure must NEVER produce a success toast. The outcome
              // is reported truthfully with the backend error preserved.
              void useCollectionsStore
                .getState()
                .addManyToGroup(groupID, ids)
                .then(({ added, failed, firstError }) => {
                  const groupName =
                    userGroups.find((group) => group.id === groupID)?.name ?? "group";

                  if (failed === 0) {
                    toast("success", `Added ${added} to ${groupName}`);
                  } else if (added > 0) {
                    toast(
                      "warn",
                      `Added ${added} of ${ids.length} to ${groupName}`,
                      `${failed} failed${firstError ? `: ${firstError}` : ""}`,
                    );
                  } else {
                    toast(
                      "error",
                      `Could not add to ${groupName}`,
                      firstError ?? "All additions failed.",
                    );
                  }
                });
            }}
          >
            <option value="">Add to group…</option>
            {userGroups.map((group) => (
              <option key={group.id} value={group.id}>
                {group.name}
              </option>
            ))}
          </select>
        )}

        {/* v0.11.0: scope-aware membership removal — while browsing a
            user group, the selected rows can leave that group. The
            action acts on the ACTIVE group only (never a guess). */}
        {selected.size > 0 && activeUserGroupForBar && (
          <button
            type="button"
            className="btn sm"
            disabled={filteredLoading}
            onClick={() => {
              const group = activeUserGroupForBar;
              const ids = [...selected];

              void useCollectionsStore
                .getState()
                .removeManyFromGroup(group.id, ids)
                .then(({ removed, failed, firstError }) => {
                  if (failed === 0) {
                    toast("success", `Removed ${removed} from ${group.name}`);
                  } else if (removed > 0) {
                    toast("warn", `Removed ${removed} of ${ids.length}`, `${failed} failed${firstError ? `: ${firstError}` : ""}`);
                  } else {
                    toast("error", `Could not remove from ${group.name}`, firstError ?? "All removals failed.");
                  }

                  setSelected(new Set());
                  setViewVersion((v) => v + 1);
                });
            }}
          >
            Remove from {activeUserGroupForBar.name} ({selected.size})
          </button>
        )}

        <div className="toolbar-spacer" />

        {queueStats && queueStats.total_enqueued > 0 && (
          <button
            type="button"
            className="btn sm"
            onClick={() => void togglePause()}
          >
            {queuePaused ? <IconPlay size={13} /> : <IconPause size={13} />}
            {queuePaused ? "Resume" : "Pause"}
          </button>
        )}

        <Menu
          ariaLabel="More testing actions"
          label={
            <>
              More actions
              <IconChevronDown size={13} />
            </>
          }
          items={[
            {
              id: "retry-failed",
              label: "Retry failed",
              disabled: filteredLoading,
              onSelect: () => void bulkTest("failed"),
            },
            {
              id: "retry-timeout",
              label: "Retry timed out",
              disabled: filteredLoading,
              onSelect: () => void bulkTest("timed_out"),
            },
            {
              id: "retest-working",
              label: "Retest working",
              disabled: filteredLoading,
              onSelect: () => void bulkTest("working"),
            },
            {
              id: "cancel-all",
              label: "Cancel all tests",
              disabled: filteredLoading,
              danger: true,
              separatorBefore: true,
              onSelect: () => void cancelAll(),
            },
          ]}
        />
      </div>

      {queueStats && queueStats.total_enqueued > 0 && (
        <section className={`queue-panel ${queuePaused ? "paused" : ""}`} aria-label="Test queue progress">
          <div className="queue-head">
            <span className="queue-title">Testing {queueStats.total_completed} / {queueStats.total_enqueued}{queuePaused ? " · paused" : ""}</span>
            {/* v0.9.8.5 UI audit: the dead "indeterminate-none" class
                is gone — this bar is fully determinate (the width below
                always reflects completed / enqueued). */}
            <div
              className="progress"
              role="progressbar"
              aria-label="Test queue progress"
              aria-valuemin={0}
              aria-valuemax={queueStats.total_enqueued}
              aria-valuenow={queueStats.total_completed}
            >
              <div
                className="progress-bar"
                style={{
                  width: `${Math.min(100, Math.round((queueStats.total_completed / Math.max(1, queueStats.total_enqueued)) * 100))}%`,
                }}
              />
            </div>
            <button type="button" className="btn sm ghost" onClick={() => void togglePause()}>
              {queuePaused ? "Resume" : "Pause"}
            </button>
            <button type="button" className="btn sm ghost danger" onClick={() => void cancelAll()}>
              Cancel all
            </button>
          </div>

          <div className="queue-body">
            <div className="queue-ping-group">
              <QueuePing label="avg ping" value={queueStats.avg_latency_ms} />
              <QueuePing label="fastest" value={queueStats.fastest_latency_ms} cls="ok" />
              <QueuePing label="slowest" value={queueStats.slowest_latency_ms} cls="warn" />
            </div>

            <div className="queue-stats">
              {/* v0.9.7 §19: one aggregated progress stream with the
                  per-state counters AND the live core-process census. */}
              <span className="queue-chip passed">
                Passed <b>{queueStats.total_passed}</b>
              </span>
              <span className="queue-chip failed">
                Failed <b>{queueStats.total_failed}</b>
              </span>
              <span className="queue-chip">
                Timeout <b>{queueStats.total_timed_out}</b>
              </span>
              <span className="queue-chip">
                Cancelled <b>{queueStats.total_cancelled}</b>
              </span>
              <span className="queue-chip active" title="Temporary protocol-core processes alive right now (bounded pool)">
                Active cores <b>{queueStats.active_cores ?? 0}</b>
              </span>
              <span className="queue-chip queued" title="Configurations waiting in the queue">
                Queue <b>{queueStats.queue_depth}</b>
              </span>
            </div>
          </div>
        </section>
      )}

      <div className={`configs-layout ${detail ? "with-panel" : ""}`}>
        <div className="card flush mb-0">
          {/*
           * v0.11.0 DENSE TABLE (§2 Configuration surface): the wide
           * browsing surface is a professional single-line node table
           * with a sticky, sortable header row — the familiar
           * configuration-manager interaction model (v2rayN-class
           * density on FreeIran's own architecture and trust model):
           *
           *   ★ | Protocol | Name | Address:Port | Transport |
           *   Ping | Status | Source | ⋮
           *
           * Deep technical data (security, URL-test internals, test
           * backend, timestamps) stays in the detail panel. Narrow
           * viewports keep the two-line card row. Virtualization is
           * retained on every path.
           */}
          {!narrow && (
            <div className="config-table-header" role="row" aria-label="Column headers">
              <span className="th th-fav" aria-hidden>★</span>
              <button type="button" className="th sortable" onClick={() => sortColumn("protocol")}>
                Protocol {sortBy === "protocol" ? (sortDesc ? "▾" : "▴") : ""}
              </button>
              <button type="button" className="th sortable" onClick={() => sortColumn("address")}>
                Address : Port {sortBy === "address" ? (sortDesc ? "▾" : "▴") : ""}
              </button>
              <span className="th th-transport">Transport</span>
              <span className="th th-security">Security</span>
              <button type="button" className="th sortable" onClick={() => sortColumn("latency")}>
                Latency {sortBy === "latency" ? (sortDesc ? "▾" : "▴") : ""}
              </button>
              <button type="button" className="th sortable" onClick={() => sortColumn("tested_at")}>
                Test status {sortBy === "tested_at" ? (sortDesc ? "▾" : "▴") : ""}
              </button>
              <button type="button" className="th sortable" onClick={() => sortColumn("source")}>
                Source {sortBy === "source" ? (sortDesc ? "▾" : "▴") : ""}
              </button>
              <span className="th th-actions" aria-hidden>⋮</span>
            </div>
          )}
          <div
            ref={parentRef}
            className="config-scroll"
            onScroll={onListScroll}
            role="list"
            aria-label="Configurations"
          >
            {renderItems.length === 0 && !loading && !searching ? (
              <EmptyState
                icon={<IconSearch size={20} />}
                title={
                  searchQuery || protocol || groupFilter || sourceFilter
                    ? "No matching configurations"
                    : "No configurations yet"
                }
                hint={
                  searchQuery || protocol || groupFilter || sourceFilter
                    ? "Try a different search term, protocol filter, source or group."
                    : "Add a source and refresh to populate the database."
                }
              />
            ) : (
              <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
                {virtualizer.getVirtualItems().map((virtualRow) => {
                  const item = renderItems[virtualRow.index];

                  if (!item) return null;

                  // v0.9.10: organize-by section header — a sticky
                  // separator naming the bucket and its live count.
                  if (item.kind === "header") {
                    return (
                      <div
                        key={item.key}
                        className="list-section-header"
                        role="presentation"
                        style={{
                          position: "absolute",
                          top: 0,
                          left: 0,
                          width: "100%",
                          height: virtualRow.size,
                          transform: `translateY(${virtualRow.start}px)`,
                          display: "flex",
                          alignItems: "center",
                          gap: 8,
                          padding: "0 12px",
                          fontSize: 11,
                          fontWeight: 600,
                          letterSpacing: "0.03em",
                          textTransform: "uppercase",
                          color: "var(--text-faint)",
                          background: "var(--surface-2, rgba(127,127,127,0.06))",
                          borderBottom: "1px solid var(--border-subtle)",
                        }}
                      >
                        <span>{item.label}</span>
                        <span className="group-count">{item.count}</span>
                      </div>
                    );
                  }

                  const config = item.config;
                  const id = String(config["id"]);
                  const isFavorite = favoriteSet.has(id);
                  const name = String(config["name"] || "unnamed");
                  const liveState = testStates[id];

                  return (
                    <div
                      key={id}
                      role="listitem"
                      aria-selected={detail?.id === id}
                      className={`row selectable config-row-v3 ${narrow ? "" : "config-row-table"} ${detail?.id === id ? "selected" : ""} ${selected.has(id) ? "bulk-selected" : ""}`}
                      tabIndex={0}
                      onClick={() => void showDetails(config)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          void showDetails(config);

                          return;
                        }

                        // v0.9.13: keyboard context-menu invocation —
                        // the SAME action model as right-click.
                        if (event.key === "ContextMenu" || (event.shiftKey && event.key === "F10")) {
                          event.preventDefault();

                          const rect = event.currentTarget.getBoundingClientRect();

                          // Keyboard opening has no toggle trigger element.
                          contextMenuTriggerRef.current = null;

                          setContextMenu({
                            config,
                            anchor: { kind: "rect", rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom } },
                            viaTrigger: false,
                            triggerId: null,
                          });
                        }
                      }}
                      onContextMenu={(event) => {
                        // v0.9.13: right-click opens the row context
                        // menu — identical items to the ⋮ menu.
                        event.preventDefault();

                        // Right-click opening has no toggle trigger.
                        contextMenuTriggerRef.current = null;

                        setContextMenu({
                          config,
                          anchor: { kind: "point", x: event.clientX, y: event.clientY },
                          viaTrigger: false,
                          triggerId: null,
                        });
                      }}
                      style={{
                        position: "absolute",
                        top: 0,
                        left: 0,
                        width: "100%",
                        height: virtualRow.size,
                        transform: `translateY(${virtualRow.start}px)`,
                      }}
                    >
                      {/* v0.9.10: favorite toggle — saved routes stay
                          reachable from Quick Connect; testing and
                          verification are never bypassed. */}
                      <button
                        type="button"
                        className={`fav-toggle ${isFavorite ? "active" : ""}`}
                        aria-pressed={isFavorite}
                        aria-label={
                          isFavorite
                            ? `Remove ${name} from favorites`
                            : `Save ${name} as favorite`
                        }
                        title={isFavorite ? "Remove from favorites" : "Save as favorite"}
                        onClick={(event) => {
                          event.stopPropagation();

                          void toggleFavorite(id).catch((error: unknown) => {
                            toast("error", "Could not update favorites", describeError(error));
                          });
                        }}
                      >
                        {isFavorite ? "★" : "☆"}
                      </button>

                      <span className={`proto-badge proto-${protocolClass(String(config["type"]))}`}>
                        {protocolLabel(String(config["type"]))}
                      </span>

                      {narrow ? (
                        <>
                          {/*
                           * TWO-LINE HIERARCHY (v0.9.13, narrow): primary =
                           * name + health state; secondary = endpoint +
                           * measured ping.
                           */}
                          <span className="cell-main">
                            <span className="cell-line">
                              <span className="cell-title">{name}</span>
                              <HealthBadge config={config} />
                            </span>
                            <span className="cell-line">
                              <span className="cell-sub">
                                {truncate(String(config["address"]), 40)}:{String(config["port"])}
                              </span>
                              <LiveTestChip state={testStates[id]} />
                              <PingCell config={config} />
                            </span>
                          </span>
                        </>
                      ) : (
                        <>
                          {/*
                           * DENSE TABLE CELLS (v0.11.0, wide): one line,
                           * professional node-manager density. Status is
                           * the FULL testing lifecycle (idle → queued →
                           * preparing → testing → measuring → passed /
                           * failed / timed out / cancelled) from measured
                           * evidence only.
                           */}
                          <span className="cell-name" title={name}>
                            {name}
                          </span>
                          <span className="cell-endpoint mono" title={`${String(config["address"])}:${String(config["port"])}`}>
                            {truncate(String(config["address"]), 34)}:{String(config["port"])}
                          </span>
                          <span className="cell-transport mono">
                            {String(config["network"] || "tcp")}
                          </span>
                          <span className="cell-security mono">
                            {String(config["security"] || "none")}
                          </span>
                          <span className="cell-ping">
                            <PingCell config={config} />
                          </span>
                          <span className={`cell-status status-${statusOf(config, testStates[id]).replace("_", "-")}`}>
                            {testStates[id] ? (
                              <LiveTestChip state={testStates[id]} />
                            ) : (
                              TEST_STATUS_LABELS[statusOf(config, testStates[id])]
                            )}
                          </span>
                          <span className="cell-source" title={String(config["source"] ?? "")}>
                            {truncate(String(config["source"] ?? "—"), 18)}
                          </span>
                        </>
                      )}

                      {/*
                       * ACTIONS cell (v0.9.7 §6): a dedicated wrapper —
                       * never a bare last grid child. The primary Test
                       * action stays visible; every remaining operation
                       * lives in the ⋮ menu (the same menu right-click
                       * and the keyboard open).
                       */}
                      <span className="actions">
                        {liveState ? (
                          <span className="test-state testing" title={LIVE_STATE_TITLES[liveState]}>
                            <span className="btn-spinner" aria-hidden /> {LIVE_STATE_LABELS[liveState]}
                          </span>
                        ) : (
                          <button
                            type="button"
                            className="btn sm"
                            onClick={(event) => {
                              event.stopPropagation();
                              void testConfig(config);
                            }}
                          >
                            Test
                          </button>
                        )}
                        <button
                          type="button"
                          className="btn sm ghost dots-btn"
                          aria-label={`More actions for ${name}`}
                          aria-haspopup="menu"
                          aria-expanded={
                            contextMenu?.viaTrigger && contextMenu.triggerId === String(config["id"])
                          }
                          title="More actions"
                          onClick={(event) => {
                            event.stopPropagation();

                            const triggerId = String(config["id"]);

                            // Toggle contract: only the ⋮ trigger that is
                            // CURRENTLY holding the menu open closes it on
                            // re-click (its own mousedown is ignored by
                            // MenuSurface, so the click must do the
                            // closing). A menu opened by right-click or
                            // keyboard re-anchors here instead — and in a
                            // real browser the preceding outside-mousedown
                            // already closed it, making this an open.
                            if (contextMenu?.viaTrigger && contextMenu.triggerId === triggerId) {
                              setContextMenu(null);
                              contextMenuTriggerRef.current = null;

                              return;
                            }

                            const rect = event.currentTarget.getBoundingClientRect();

                            contextMenuTriggerRef.current = event.currentTarget;

                            setContextMenu({
                              config,
                              anchor: { kind: "rect", rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom } },
                              viaTrigger: true,
                              triggerId,
                            });
                          }}
                        >
                          <IconDots size={15} />
                        </button>
                      </span>
                    </div>
                  );
                })}
              </div>
            )}

            {(loading || searching) && (
              <div className="loading-inline">
                <span className="btn-spinner" aria-hidden /> Loading…
              </div>
            )}
          </div>
        </div>

        {/* v0.11.2: Internal configuration tabs rail. Rendered ABOVE
//          the detail panel so the user sees `All | A | B` while the
//          active tab's detail fills the panel below. The `All` tab
//          is implicit — closing the last open tab returns focus to
//          the wide table (no `All` chip is rendered, only per-config
//          tabs). Tab bar is hidden when no tabs are open. */}
        {openTabs.length > 0 && (
          <div className="config-tabs-rail" role="tablist" aria-label="Open configurations">
            <button
              type="button"
              className={`config-tab config-tab-all ${activeTabId === null ? "active" : ""}`}
              role="tab"
              aria-selected={activeTabId === null}
              onClick={() => setActiveTabId(null)}
              title="Return to the wide configuration table"
            >
              All
            </button>
            {openTabs.map((tab) => (
              <div
                key={tab.id}
                className={`config-tab ${activeTabId === tab.id ? "active" : ""}`}
                role="tab"
                aria-selected={activeTabId === tab.id}
              >
                <button
                  type="button"
                  className="config-tab-label"
                  onClick={() => setActiveTabId(tab.id)}
                  title={tab.name || `${tab.address}:${tab.port}`}
                >
                  {tab.name || `${tab.address}:${tab.port}`}
                </button>
                <button
                  type="button"
                  className="config-tab-close"
                  aria-label={`Close ${tab.name || tab.id}`}
                  title="Close this tab"
                  onClick={() => closeConfigTab(tab.id)}
                >
                  ×
                </button>
              </div>
            ))}
            <div className="config-tab-actions">
              {openTabs.length > 1 && (
                <button
                  type="button"
                  className="config-tab-action"
                  onClick={closeOtherTabs}
                  title="Keep only the focused tab open"
                >
                  Close others
                </button>
              )}
              <button
                type="button"
                className="config-tab-action"
                onClick={closeAllTabs}
                title="Close every open configuration tab and return to All"
              >
                Close all
              </button>
            </div>
          </div>
        )}

        {detail && (
          <DetailPanel
            detail={detail}
            row={visibleItems.find((item) => String(item["id"]) === detail.id) ?? null}
            onClose={() => closeConfigTab(detail.id)}
          />
        )}

        {/* v0.10.2: personal configuration import flow. onSaved
            triggers the existing refresh so the new rows appear
            through the normal store projection. */}
        <ImportDialog
          open={importOpen}
          onClose={() => setImportOpen(false)}
          onSaved={() => {
            void useConfigsStore.getState().loadPage(0);
          }}
        />

      {/*
       * v0.12.2: the PROXY CHAIN editor — create/update ordered hop
       * lists over EXISTING configurations; validate before save;
       * Check hops / Check chain / Connect ride the existing queue
       * and the verified state machine.
       */}
      <ChainEditor
        open={chainEditor.open}
        chain={chainEditor.id ? chainDetails : null}
        initialHops={chainEditor.id ? [] : pendingChainHops.current}
        onClose={() => setChainEditor({ open: false, id: null })}
        onSaved={(chainID) => {
          pendingChainHops.current = [];
          setChainEditor({ open: false, id: null });

          void (async () => {
            try {
              const details = await call(() => proxyChainService.ProxyChainDetails(chainID));

              setChainDetails(details);
              setChainFilter(chainID);
              void loadChains();
            } catch (error) {
              toast("error", "Chain unavailable", describeError(error));
            }
          })();
        }}
      />
      </div>

      {/*
       * v0.9.13/v0.9.15 ROW CONTEXT MENU: one portal surface fed by
       * rowMenuItems() — opened by right-click, the ⋮ button and the
       * keyboard (Shift+F10 / Menu key). Placement, flipping,
       * clamping, keyboard navigation and focus return come from the
       * ONE shared MenuSurface; the old 240/340/348 hard-coded
       * geometry is gone. The trigger ref makes the opening ⋮ button
       * toggle the menu instead of flip-flopping.
       */}
      {contextMenu && (
        <MenuSurface
          anchor={contextMenu.anchor}
          items={rowMenuItems(contextMenu.config)}
          onClose={() => setContextMenu(null)}
          ariaLabel="Configuration actions"
          triggerRef={contextMenuTriggerRef}
        />
      )}
    </div>
  );
}

/**
 * Detail panel (v0.9.13): the ONE place for technical configuration
 * information, grouped for reading — Overview → Endpoint →
 * Measurement → Health → Source → Technical. Measured evidence rides
 * in from the row's config snapshot (the list model already carries
 * ping / url_test / test_backend); no new backend path.
 */
function DetailPanel({
  detail,
  row,
  onClose,
}: {
  detail: ConfigDetail;
  row: Config | null;
  onClose: () => void;
}) {
  const connectBusy = useConnectionStore((state) => state.busy);
  // v0.9.15: the live state is read from the ONE shared queue
  // projection — the panel reacts to the same task the row shows.
  const liveState = useTestProgress((state) => state.states[detail.id]);
  const testing = liveState !== undefined;

  const test = async () => {
    markTestRequested(detail.id);

    try {
      await dataService.TestConfig(detail.id);
      toast("info", "Test queued", "The test queue will run it; this panel updates when the result lands.");
    } catch (error) {
      clearTestRequested(detail.id);
      toast("error", "Test failed", describeError(error));
    }
  };

  const connectNow = async () => {
    await useConnectionStore.getState().connect(detail.id);

    const error = useConnectionStore.getState().error;

    if (error) {
      toast("error", "Connection failed", error);
    } else {
      toast("success", "Connecting", "The connection state machine is starting.");
      onClose();
    }
  };

  // v0.9.7 §18: honest, measured-only reporting — never fabricated.
  // The row snapshot (list model) carries the full measurement
  // evidence; the detail binding carries the endpoint facts.
  const rowAny = (row ?? detail) as unknown as Record<string, unknown>;
  const ping = rowAny["ping"] as
    | { median_ms?: number; samples?: number; packet_loss?: number }
    | undefined;
  const urlTest = rowAny["url_test"] as
    | { ok?: boolean; status?: number; total_ms?: number; timeout?: boolean }
    | undefined;
  const testBackend = rowAny["test_backend"] ? String(rowAny["test_backend"]) : "";
  const testedAt = Number(detail.tested_at ?? 0) || Number(rowAny["tested_at"] ?? 0);

  const testState = !testedAt
    ? { label: "untested", cls: "untested" }
    : detail.working
      ? { label: "passed", cls: "passed" }
      : { label: "failed", cls: "failed" };

  const rowClass = `detail-section`;

  return (
    <aside className="detail-panel" aria-label="Configuration details">
      <div className="card-header">
        <h3 className="card-title">{detail.name || `${detail.address}:${detail.port}`}</h3>
        <button type="button" className="icon-btn" aria-label="Close details" onClick={onClose}>
          <IconX size={14} />
        </button>
      </div>

      <div className="detail-panel-body">
        {/*
         * PRIMARY ACTION GROUP (v0.9.7 §6): Connect and Test now form
         * one clearly delimited action row — no button can ever be
         * hidden beneath descriptive text.
         */}
        <div className="detail-actions">
          <button
            type="button"
            className="btn primary"
            disabled={connectBusy}
            onClick={() => void connectNow()}
          >
            <IconPlay size={13} />
            Connect
          </button>

          <button type="button" className="btn" disabled={testing} onClick={() => void test()}>
            {testing ? <span className="btn-spinner" aria-hidden /> : <IconRefresh size={13} />}
            Test now
          </button>
        </div>

        {/* ---- OVERVIEW ---- */}
        <section className={rowClass}>
          <h4 className="detail-section-title">Overview</h4>
          <dl className="detail-grid">
            <dt>Name</dt>
            <dd>{detail.name || "unnamed"}</dd>

            <dt>Protocol</dt>
            <dd className="mono-cell">{detail.type}</dd>

            <dt>Test state</dt>
            <dd>
              <span className={`test-state ${testing ? "testing" : testState.cls}`}>
                {testing && <span className="btn-spinner" aria-hidden />}
                {testing ? "Testing" : testState.label}
              </span>
            </dd>

            <dt>Credentials</dt>
            <dd className="cell-sub">
              {[
                detail.has_uuid ? "UUID (redacted)" : null,
                detail.has_password ? "password (redacted)" : null,
                detail.has_public_key ? "public key (redacted)" : null,
              ]
                .filter(Boolean)
                .join(", ") || "none"}
            </dd>
          </dl>
        </section>

        {/* ---- ENDPOINT ---- */}
        <section className={rowClass}>
          <h4 className="detail-section-title">Endpoint</h4>
          <dl className="detail-grid">
            <dt>Address</dt>
            <dd className="mono-cell">
              {detail.address}:{detail.port}
            </dd>

            <dt>Redacted URL</dt>
            <dd className="mono-cell">{detail.display || "—"}</dd>

            <dt>Transport</dt>
            <dd className="mono-cell">{detail.network || "tcp"}</dd>

            <dt>Security</dt>
            <dd className="mono-cell">{detail.security || "none"}</dd>

            {detail.host && (
              <>
                <dt>Host</dt>
                <dd className="mono-cell">{detail.host}</dd>
              </>
            )}

            {detail.path && (
              <>
                <dt>Path</dt>
                <dd className="mono-cell">{detail.path}</dd>
              </>
            )}

            {detail.service && (
              <>
                <dt>gRPC service</dt>
                <dd className="mono-cell">{detail.service}</dd>
              </>
            )}

            {detail.server_name && (
              <>
                <dt>Server name</dt>
                <dd className="mono-cell">{detail.server_name}</dd>
              </>
            )}

            {detail.method && (
              <>
                <dt>Cipher</dt>
                <dd className="mono-cell">{detail.method}</dd>
              </>
            )}
          </dl>
        </section>

        {/* ---- MEASUREMENT ---- */}
        <section className={rowClass}>
          <h4 className="detail-section-title">Measurement</h4>
          <dl className="detail-grid">
            <dt>Ping</dt>
            <dd className="mono-cell">
              {ping && (ping.samples ?? 0) > 0
                ? `${formatLatency(ping.median_ms ?? 0)} (median of ${ping.samples})`
                : testedAt && detail.latency_ms
                  ? `~${formatLatency(detail.latency_ms)} (estimated)`
                  : "—"}
            </dd>

            <dt>URL test</dt>
            <dd className="mono-cell">
              {urlTest?.total_ms
                ? urlTest.ok
                  ? `HTTP ${urlTest.status} · ${urlTest.total_ms} ms`
                  : urlTest.timeout
                    ? "timeout"
                    : "failed"
                : "not run"}
            </dd>

            <dt>Test backend</dt>
            <dd className="mono-cell">{testBackend || "—"}</dd>

            <dt>Last test</dt>
            <dd>{testedAt ? relativeTime(testedAt) : "never"}</dd>
          </dl>
        </section>

        {/* ---- HEALTH ---- */}
        <section className={rowClass}>
          <h4 className="detail-section-title">Health</h4>
          <dl className="detail-grid">
            <dt>Result</dt>
            <dd>
              {testedAt ? (
                <ResultBadge ok={detail.working} okLabel="working" failLabel="failed" />
              ) : (
                <span className="badge neutral">untested</span>
              )}
            </dd>

            <dt>Verification</dt>
            <dd>
              {testedAt && detail.working
                ? "protocol + connectivity verified"
                : testedAt
                  ? "failed — see test state"
                  : "not verified"}
            </dd>

            <dt>Compatible cores</dt>
            <dd className="mono-cell">{detail.compatible_backends?.join(", ") || "none compatible"}</dd>
          </dl>
        </section>

        {/* ---- FAILURE EVIDENCE (v0.11.0) ---- */}
        {(Number(detail.failure_streak ?? 0) > 0 || Boolean(detail.last_failure_class)) && (
          <section className={rowClass}>
            <h4 className="detail-section-title">Failure evidence</h4>
            <dl className="detail-grid">
              <dt>Failure streak</dt>
              <dd className="mono-cell">{Number(detail.failure_streak ?? 0) || "—"}</dd>

              <dt>Recent failure class</dt>
              <dd className="mono-cell">{detail.last_failure_class || "unclassified"}</dd>

              {detail.last_failure_reason && (
                <>
                  <dt>Failure detail</dt>
                  <dd className="mono-cell">{detail.last_failure_reason}</dd>
                </>
              )}

              {Number(detail.last_failure_at ?? 0) > 0 && (
                <>
                  <dt>Last failure</dt>
                  <dd>{relativeTime(Number(detail.last_failure_at))}</dd>
                </>
              )}

              {Number(detail.last_success_at ?? 0) > 0 && (
                <>
                  <dt>Last verified</dt>
                  <dd>{relativeTime(Number(detail.last_success_at))}</dd>
                </>
              )}
            </dl>
            <p className="detail-note">
              Evidence is recorded from observed test outcomes; repeated protocol-class failures
              (tls / handshake / transport) demote this candidate in automatic selection so other
              transports are preferred.
            </p>
          </section>
        )}

        {/* ---- ENCRYPTED CLIENT HELLO (v0.11.0) ---- */}
        {detail.ech_configured && (
          <section className={rowClass}>
            <h4 className="detail-section-title">Encrypted Client Hello</h4>
            <dl className="detail-grid">
              <dt>Configuration</dt>
              <dd>
                <span className="badge success">configured</span>
              </dd>

              <dt>Core support</dt>
              <dd>
                {detail.ech_backend_support ? (
                  <span className="badge success">installed core with verified ECH schema</span>
                ) : (
                  <span className="badge error">no installed core with verified ECH support</span>
                )}
              </dd>

              <dt>Core acceptance</dt>
              <dd>
                {detail.ech_core_verified ? (
                  <span className="badge success">ECH document accepted, tunnel verified</span>
                ) : (
                  <span className="badge neutral">not yet verified through an ECH-capable core</span>
                )}
              </dd>

              <dt>Connectivity</dt>
              <dd>
                {testedAt
                  ? detail.working
                    ? "verified usable (through tunnel)"
                    : "failed — see health"
                  : "not tested"}
              </dd>
            </dl>
            <p className="detail-note">
              ECH support is schema-level evidence (config check + core startup with the pinned
              sing-box), not proof of live ECH negotiation with the remote server.
            </p>
          </section>
        )}

        {/* ---- SOURCE ---- */}
        {detail.source && (
          <section className={rowClass}>
            <h4 className="detail-section-title">Source</h4>
            <dl className="detail-grid">
              <dt>Source</dt>
              <dd>{detail.source}</dd>
            </dl>
          </section>
        )}
      </div>
    </aside>
  );
}

/**
 * v0.9.6 Ping cell: displays the MEASURED median TCP ping with its
 * provenance. Estimated/unavailable latencies are labelled as such —
 * never silently displayed as a ping (§10).
 */
function PingCell({ config }: { config: Config }) {
  const ping = config["ping"] as
    | { median_ms?: number; samples?: number; packet_loss?: number }
    | undefined;
  const testedAt = Number(config["tested_at"] ?? 0);
  const legacyMs = Number(config["latency_ms"] ?? 0);

  if (ping && (ping.samples ?? 0) > 0) {
    const stale = testedAt > 0 && Date.now() - testedAt > 30 * 60 * 1000;
    return (
      <span
        className={`latency ${latencyClass(ping.median_ms ?? 0)}`}
        title={`median of ${ping.samples} samples${
          ping.packet_loss ? ` · ${(ping.packet_loss * 100).toFixed(0)}% loss` : ""
        }${stale ? " · measurement stale" : ""}`}
      >
        {formatLatency(ping.median_ms ?? 0)}
        {stale ? <span className="chip mono"> stale</span> : null}
      </span>
    );
  }

  if (testedAt && legacyMs > 0) {
    // Legacy measured latency (single probe through the tunnel): an
    // honest estimate, labelled as such.
    return (
      <span
        className="latency none"
        title="estimated from the last tunnel probe — run a Ping test for a measured median"
      >
        ~{formatLatency(legacyMs)}
      </span>
    );
  }

  return <span className="value latency none">—</span>;
}

/** Health badge: measured working/failed state (never invented). */
function HealthBadge({ config }: { config: Config }) {
  if (!config["tested_at"]) return <span className="badge neutral">untested</span>;

  return config["working"] ? (
    <span className="badge success" data-tip={relativeTime(Number(config["tested_at"]))}>
      working
    </span>
  ) : (
    <span className="badge error" data-tip={relativeTime(Number(config["tested_at"]))}>
      failed
    </span>
  );
}

/** Prominent ping tile for the queue panel (hidden when no data yet). */
function QueuePing({ label, value, cls }: { label: string; value?: number; cls?: string }) {
  if (!value) {
    return (
      <span className="queue-ping">
        <span className="value latency none">—</span>
        <span className="label">{label}</span>
      </span>
    );
  }

  return (
    <span className="queue-ping">
      <span className={`value ${cls ?? latencyClass(value)}`}>{formatLatency(value)}</span>
      <span className="label">{label}</span>
    </span>
  );
}

/** CSS protocol class (defaults to unknown for exotic values). */
function protocolClass(type: string): string {
  return ["vless", "vmess", "trojan", "shadowsocks", "hysteria2", "hysteria", "tuic", "wireguard", "socks", "http", "unknown"].includes(type)
    ? type
    : "unknown";
}
