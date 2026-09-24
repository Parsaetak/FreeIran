import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import { MenuSurface, type MenuAnchor } from "../components/MenuSurface";
import {
  dataService,
  connectionService,
  testQueueService,
  call,
  type Config,
  type ConfigDetail,
  type QueueStatsView,
} from "../services";
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
  applyQueueSnapshot,
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
import type { MenuItem } from "../components/common";
import {
  IconChevronDown,
  IconDots,
  IconPause,
  IconPlay,
  IconRefresh,
  IconSearch,
  IconX,
} from "../components/Icons";

const searchRunner = makeSearchRunner(250);

const PROTOCOL_FILTERS = ["vless", "vmess", "trojan", "shadowsocks", "hysteria2"];

/** Short display label for a protocol value. */
function protocolLabel(type: string): string {
  if (type === "shadowsocks") return "ss";

  return type || "—";
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

  const [groupFilter, setGroupFilter] = useState<string>("");
  const [organizeBy, setOrganizeBy] = useState<"" | "source" | "protocol" | "status">("");
  const [newGroupOpen, setNewGroupOpen] = useState(false);
  const [newGroupName, setNewGroupName] = useState("");

  useEffect(() => {
    void loadCollections();
  }, [loadCollections]);

  const [protocol, setProtocol] = useState("");
  const [detail, setDetail] = useState<ConfigDetail | null>(null);
  // v0.9.15: per-config live test state comes from the ONE shared
  // projection of the test queue (testProgress) — rows, detail panel
  // and Connection all read the same store; no page-private truth.
  const testStates = useTestProgress((state) => state.states);

  // v0.9.0: server-side status filter + sorting and bulk-test state.
  const [statusFilter, setStatusFilter] = useState<"" | "working" | "failed" | "untested">("");
  const [sortBy, setSortBy] = useState<"" | "latency" | "tested_at" | "protocol">("");
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
  const [contextMenu, setContextMenu] = useState<{ config: Config; anchor: MenuAnchor } | null>(null);
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

  const virtualizer = useVirtualizer({
    count: renderItems.length,
    estimateSize: (index) => (renderItems[index]?.kind === "header" ? 32 : narrow ? 88 : 50),
    overscan: 12,
    getScrollElement: () => parentRef.current,
  });

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
  const loadFiltered = useCallback(async (limit: number) => {
    if (statusFilter === "" && sortBy === "" && groupFilter === "") {
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
            sort_desc: sortBy === "latency",
            source: undefined,
            backend: undefined,
            group: groupFilter || undefined,
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
  }, [protocol, statusFilter, sortBy, searchQuery, groupFilter]);

  useEffect(() => {
    void loadFiltered(1000);
  }, [loadFiltered]);

  // Live queue stats while a batch is running. Adaptive cadence:
  // ~800 ms while work is queued/running, 5 s when idle — the panel
  // only renders once total_enqueued > 0, so a fixed 1.5 s poll just
  // burned binding traffic for a page left open in the background.
  // v0.9.15: the SAME snapshot feeds the shared testProgress store,
  // which diffs live fingerprints against the previous tick, detects
  // completions and patches their persisted results into the config
  // model incrementally (one GetConfig per changed row — never a
  // full-list refresh).
  useEffect(() => {
    let stop = false;
    let timer = 0;

    const cadence = (stats: QueueStatsView | null) =>
      stats && (stats.queue_depth > 0 || stats.active_workers > 0) ? 800 : 5000;

    const tick = async () => {
      let next = 5000;

      try {
        const stats = await call(() => testQueueService.Stats());

        if (!stop && stats) {
          setQueueStats(stats as QueueStatsView);
          next = cadence(stats as QueueStatsView);
        }

        // v0.9.7: track pause state.
        try {
          const paused = await call(() => testQueueService.Paused());
          if (!stop) setQueuePaused(Boolean(paused));
        } catch {
          /* best-effort */
        }

        // v0.9.15: ONE snapshot → shared live-state projection +
        // incremental result patches.
        try {
          const snapshot = await call(() => testQueueService.Snapshot(200));
          if (!stop && Array.isArray(snapshot)) {
            applyQueueSnapshot(snapshot);
          }
        } catch {
          /* best-effort */
        }
      } catch {
        /* polling is best-effort */
      }

      if (!stop) timer = window.setTimeout(tick, next);
    };

    void tick();

    return () => {
      stop = true;
      window.clearTimeout(timer);
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
          setDetail((prev) => (prev && prev.id === fingerprint ? (fresh as ConfigDetail) : prev));
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
          source: undefined,
          limit: undefined,
          priority: undefined,
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
  const showDetails = async (config: Config) => {
    const id = String(config["id"]);

    if (detail?.id === id) {
      setDetail(null);

      return;
    }

    try {
      const result = await call(() => connectionService.ConfigDetails(id));
      setDetail(result ?? null);
    } catch (error) {
      toast("error", "Details unavailable", describeError(error));
    }
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
    const reorderable = statusFilter === "" && sortBy === "" && !searchQuery && organizeBy === "" && groupFilter === "";
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

    items.push(
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
        {builtinGroups.map((group) => (
          <button
            key={group.id}
            type="button"
            className={`group-chip ${groupFilter === group.id ? "active" : ""}`}
            aria-pressed={groupFilter === group.id}
            title={BUILTIN_GROUP_HINTS[group.id] ?? undefined}
            onClick={() => setGroupFilter(groupFilter === group.id ? "" : group.id)}
          >
            {BUILTIN_GROUP_LABELS[group.id] ?? group.id}
            <span className="group-count">{group.count}</span>
          </button>
        ))}

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

      {/*
       * SEARCH / FILTER TOOLBAR (§6: the testing controls live in their
       * own dedicated bar below — the two concerns never compete).
       */}
      <div className="toolbar">
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

              void Promise.all(
                ids.map((id) =>
                  useCollectionsStore.getState().addToGroup(groupID, id).catch(() => undefined),
                ),
              ).then(() => {
                toast("success", `Added ${ids.length} to group`);
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
           * v0.9.13 COMPACT ROW (§2 Configuration surface): the
           * primary browsing surface is a two-line hierarchy —
           * line 1: name · protocol · health; line 2: endpoint ·
           * measured ping. Transport, security, URL-test internals,
           * test backend, source and timestamps live in the detail
           * panel and the row context menu, NOT in the scan surface.
           * No column-header row: the row itself is the label.
           */}
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
                title={searchQuery || protocol || groupFilter ? "No matching configurations" : "No configurations yet"}
                hint={
                  searchQuery || protocol || groupFilter
                    ? "Try a different search term, protocol filter or group."
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
                      className={`row selectable config-row-v3 ${detail?.id === id ? "selected" : ""} ${selected.has(id) ? "bulk-selected" : ""}`}
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

                          setContextMenu({
                            config,
                            anchor: { kind: "rect", rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom } },
                          });
                        }
                      }}
                      onContextMenu={(event) => {
                        // v0.9.13: right-click opens the row context
                        // menu — identical items to the ⋮ menu.
                        event.preventDefault();

                        setContextMenu({ config, anchor: { kind: "point", x: event.clientX, y: event.clientY } });
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

                      {/*
                       * TWO-LINE HIERARCHY (§2): primary = name +
                       * health state; secondary = endpoint + measured
                       * ping (+ one small contextual indicator).
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
                          title="More actions"
                          onClick={(event) => {
                            event.stopPropagation();

                            const rect = event.currentTarget.getBoundingClientRect();

                            setContextMenu({
                              config,
                              anchor: { kind: "rect", rect: { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom } },
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

        {detail && <DetailPanel detail={detail} row={visibleItems.find((item) => String(item["id"]) === detail.id) ?? null} onClose={() => setDetail(null)} />}
      </div>

      {/*
       * v0.9.13/v0.9.15 ROW CONTEXT MENU: one portal surface fed by
       * rowMenuItems() — opened by right-click, the ⋮ button and the
       * keyboard (Shift+F10 / Menu key). Placement, flipping,
       * clamping, keyboard navigation and focus return come from the
       * ONE shared MenuSurface; the old 240/340/348 hard-coded
       * geometry is gone.
       */}
      {contextMenu && (
        <MenuSurface
          anchor={contextMenu.anchor}
          items={rowMenuItems(contextMenu.config)}
          onClose={() => setContextMenu(null)}
          ariaLabel="Configuration actions"
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
