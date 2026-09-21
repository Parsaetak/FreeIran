import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
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
import { useQuickConnectStore } from "../state/quickConnectStore";
import { describeError, toast } from "../state/toastStore";
import {
  useCollectionsStore,
  BUILTIN_GROUP_LABELS,
  BUILTIN_GROUP_HINTS,
} from "../state/collectionsStore";
import { EmptyState, Menu, ResultBadge, SegmentedControl } from "../components/common";
import {
  IconChevronDown,
  IconDownload,
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
  const runSearch = useConfigsStore((state) => state.runSearch);

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
  const [testingId, setTestingId] = useState<string | null>(null);

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
  // v0.9.7: per-row queued state derived from the queue snapshot.
  const [queuedIds, setQueuedIds] = useState<Set<string>>(new Set());
  // v0.9.7: compact two-row card layout below 860px (actions get a
  // dedicated row) — the virtualizer sizes rows accordingly.
  const [narrow, setNarrow] = useState(
    () => typeof window !== "undefined" && window.matchMedia("(max-width: 860px)").matches,
  );

  const parentRef = useRef<HTMLDivElement>(null);

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
    estimateSize: (index) => (renderItems[index]?.kind === "header" ? 32 : narrow ? 78 : 44),
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

        // v0.9.7: track pause state + per-row queued fingerprints.
        try {
          const paused = await call(() => testQueueService.Paused());
          if (!stop) setQueuePaused(Boolean(paused));
        } catch {
          /* best-effort */
        }

        try {
          const snapshot = await call(() => testQueueService.Snapshot(200));
          if (!stop && Array.isArray(snapshot)) {
            const live = snapshot.filter(
              (task: { state?: string }) =>
                task?.state === "queued" || task?.state === "preparing" || task?.state === "testing" || task?.state === "measuring",
            );
            setQueuedIds(new Set(live.map((task: { fingerprint?: string }) => String(task?.fingerprint ?? ""))));
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

  const onListScroll = () => {
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

  const testConfig = async (config: Config) => {
    const id = String(config["id"]);

    setTestingId(id);

    try {
      await dataService.TestConfig(id);
      await runSearch();
      // v0.9.8.7: a persisted test result changed the ranking inputs —
      // the Quick Connect candidate list refreshes once, on this
      // meaningful invalidation (no polling anywhere).
      useQuickConnectStore.getState().invalidate();
      toast("success", "Test finished", `${config["address"]}: responded.`);
    } catch (error) {
      toast("error", "Test failed", describeError(error));
    } finally {
      setTestingId(null);
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

        <div className="page-actions">
          <button type="button" className="btn" onClick={exportCSV}>
            <IconDownload size={14} />
            Export CSV
          </button>
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

        <label className="organize-control">
          <span className="organize-label">Group by</span>
          <select
            className="input slim"
            aria-label="Group configurations by"
            value={organizeBy}
            onChange={(event) => setOrganizeBy(event.target.value as typeof organizeBy)}
          >
            <option value="">Nothing</option>
            <option value="source">Source</option>
            <option value="protocol">Protocol</option>
            <option value="status">Status</option>
          </select>
        </label>
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
              onSelect: () => void bulkTest("failed"),
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
          <div className="row header config-row-v2">
            <span aria-hidden />
            <span>Proto</span>
            <span>Endpoint</span>
            <span className="hide-md">Transport</span>
            <span title="Measured TCP ping (median of samples)">Ping</span>
            <span className="hide-md" title="URL test through the tunnel">URL</span>
            <span className="hide-md">Health</span>
            <span className="hide-sm">Source</span>
            <span className="actions-header">Actions</span>
          </div>

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

                  return (
                    <div
                      key={String(config["id"])}
                      role="listitem"
                      className={`row selectable config-row-v2 ${detail?.id === String(config["id"]) ? "selected" : ""}`}
                      tabIndex={0}
                      onClick={() => void showDetails(config)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          void showDetails(config);
                        }
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
                      <input
                        type="checkbox"
                        className="row-check"
                        aria-label="Select for bulk testing"
                        checked={selected.has(String(config["id"]))}
                        onClick={(event) => event.stopPropagation()}
                        onChange={(event) => toggleSelect(String(config["id"]), event.target.checked)}
                      />

                      {/* v0.9.10: favorite toggle — saved routes stay
                          reachable from Quick Connect; testing and
                          verification are never bypassed. */}
                      <button
                        type="button"
                        className={`fav-toggle ${favorites.includes(String(config["id"])) ? "active" : ""}`}
                        aria-pressed={favorites.includes(String(config["id"]))}
                        aria-label={
                          favorites.includes(String(config["id"]))
                            ? `Remove ${String(config["name"] || "configuration")} from favorites`
                            : `Save ${String(config["name"] || "configuration")} as favorite`
                        }
                        title={favorites.includes(String(config["id"])) ? "Remove from favorites" : "Save as favorite"}
                        onClick={(event) => {
                          event.stopPropagation();

                          void toggleFavorite(String(config["id"])).catch((error: unknown) => {
                            toast("error", "Could not update favorites", describeError(error));
                          });
                        }}
                      >
                        {favorites.includes(String(config["id"])) ? "★" : "☆"}
                      </button>

                      <span className={`proto-badge proto-${protocolClass(String(config["type"]))}`}>
                        {protocolLabel(String(config["type"]))}
                      </span>

                      <span className="cell-main">
                        <div className="cell-title">{String(config["name"] || "unnamed")}</div>
                        <div className="cell-sub">
                          {truncate(String(config["address"]), 40)}:{String(config["port"])}
                        </div>
                      </span>

                      <span className="hide-md chip-row">
                        {config["network"] && <span className="chip mono">{String(config["network"])}</span>}
                        {config["security"] && <span className="chip mono">{String(config["security"])}</span>}
                        {!config["network"] && !config["security"] && <span className="chip mono">tcp</span>}
                      </span>

                      <span className="cell-latency">
                        <PingCell config={config} />
                        {config["test_backend"] && (
                          <span className="chip mono hide-sm" title="Backend that ran the last test">
                            {String(config["test_backend"])}
                          </span>
                        )}
                      </span>

                      <span className="hide-md">
                        <URLCell config={config} />
                      </span>

                      <span className="hide-md">
                        <HealthBadge config={config} />
                      </span>

                      <span className="hide-sm">
                        {config["source"] ? <span className="chip">{truncate(String(config["source"]), 14)}</span> : <span className="chip">—</span>}
                      </span>

                      {/*
                       * ACTIONS cell (v0.9.7 §6): a dedicated wrapper —
                       * never a bare last grid child. Fixed column width,
                       * nowrap, never shrinks: the Test action stays
                       * visible regardless of content length.
                       */}
                      <span className="actions">
                        {statusFilter === "" && sortBy === "" && !searchQuery && organizeBy === "" && groupFilter === "" && (
                          <>
                            <button
                              type="button"
                              className="btn sm"
                              aria-label={`Move ${String(config["name"] || "configuration")} up`}
                              title="Move up"
                              disabled={virtualRow.index === 0}
                              onClick={(event) => {
                                event.stopPropagation();
                                void moveConfig(config, -1);
                              }}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              className="btn sm"
                              aria-label={`Move ${String(config["name"] || "configuration")} down`}
                              title="Move down"
                              disabled={virtualRow.index >= renderItems.length - 1}
                              onClick={(event) => {
                                event.stopPropagation();
                                void moveConfig(config, 1);
                              }}
                            >
                              ↓
                            </button>
                          </>
                        )}
                        {queuedIds.has(String(config["id"])) && testingId !== String(config["id"]) && (
                          <span className="test-state queued" title="Waiting in the test queue">Queued</span>
                        )}
                        {testingId === String(config["id"]) ? (
                          <span className="test-state testing" title="Test in progress">
                            <span className="btn-spinner" aria-hidden /> Testing
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

        {detail && <DetailPanel detail={detail} onClose={() => setDetail(null)} />}
      </div>
    </div>
  );
}

function DetailPanel({ detail, onClose }: { detail: ConfigDetail; onClose: () => void }) {
  const connect = useConnectionStore((state) => state.connect);
  const connectBusy = useConnectionStore((state) => state.busy);
  const runSearch = useConfigsStore((state) => state.runSearch);

  const [testing, setTesting] = useState(false);

  const test = async () => {
    setTesting(true);

    try {
      await dataService.TestConfig(detail.id);
      await runSearch();
      // v0.9.8.7: one bounded Quick Connect refresh per persisted test
      // result (see testConfig above).
      useQuickConnectStore.getState().invalidate();
      toast("success", "Test finished", "The result was stored with the configuration.");
    } catch (error) {
      toast("error", "Test failed", describeError(error));
    } finally {
      setTesting(false);
    }
  };

  const connectNow = async () => {
    await connect(detail.id);

    const error = useConnectionStore.getState().error;

    if (error) {
      toast("error", "Connection failed", error);
    } else {
      toast("success", "Connecting", "The connection state machine is starting.");
      onClose();
    }
  };

  // v0.9.7 §18: honest, measured-only reporting — never fabricated.
  const detailAny = detail as unknown as Record<string, unknown>;
  const ping = detailAny["ping"] as
    | { median_ms?: number; samples?: number; packet_loss?: number }
    | undefined;
  const urlTest = detailAny["url_test"] as
    | { ok?: boolean; status?: number; total_ms?: number; timeout?: boolean }
    | undefined;
  const testedAt = Number(detail.tested_at ?? 0);

  const testState = !testedAt
    ? { label: "untested", cls: "untested" }
    : detail.working
      ? { label: "passed", cls: "passed" }
      : { label: "failed", cls: "failed" };

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

        <dl className="detail-grid">
          <dt>Test state</dt>
          <dd>
            <span className={`test-state ${testing ? "testing" : testState.cls}`}>
              {testing && <span className="btn-spinner" aria-hidden />}
              {testing ? "Testing" : testState.label}
            </span>
          </dd>

          <dt>Last test</dt>
          <dd>{testedAt ? relativeTime(testedAt) : "never"}</dd>

          <dt>Ping</dt>
          <dd className="mono-cell">
            {ping && (ping.samples ?? 0) > 0
              ? `${formatLatency(ping.median_ms ?? 0)} (median of ${ping.samples})`
              : testedAt && detail.latency_ms
                ? `~${formatLatency(detail.latency_ms)} (estimated)`
                : "—"}
          </dd>

          <dt>URL</dt>
          <dd className="mono-cell">
            {urlTest?.total_ms
              ? urlTest.ok
                ? `HTTP ${urlTest.status} · ${urlTest.total_ms} ms`
                : urlTest.timeout
                  ? "timeout"
                  : "failed"
              : "not run"}
          </dd>

          <dt>Health</dt>
          <dd>
            {testedAt ? (
              <ResultBadge ok={detail.working} okLabel="working" failLabel="failed" />
            ) : (
              <span className="badge neutral">untested</span>
            )}
          </dd>

          <dt>Core</dt>
          <dd className="mono-cell">{detail.compatible_backends?.join(", ") || "none compatible"}</dd>

          <dt>Verification</dt>
          <dd>
            {testedAt && detail.working
              ? "protocol + connectivity verified"
              : testedAt
                ? "failed — see test state"
                : "not verified"}
          </dd>

          <dt>Protocol</dt>
          <dd className="mono-cell">{detail.type}</dd>

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

          {detail.path && (
            <>
              <dt>Path</dt>
              <dd className="mono-cell">{detail.path}</dd>
            </>
          )}

          {detail.host && (
            <>
              <dt>Host</dt>
              <dd className="mono-cell">{detail.host}</dd>
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

          {detail.source && (
            <>
              <dt>Source</dt>
              <dd>{detail.source}</dd>
            </>
          )}

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

/** v0.9.6 URL cell: measured HTTP connectivity through the tunnel. */
function URLCell({ config }: { config: Config }) {
  const url = config["url_test"] as
    | { ok?: boolean; status?: number; total_ms?: number; timeout?: boolean }
    | undefined;

  if (!url || !url.total_ms) {
    return <span className="value latency none">not run</span>;
  }

  return (
    <span
      className={`latency ${url.ok ? latencyClass(url.total_ms ?? 0) : "bad"}`}
      title={
        url.ok
          ? `HTTP ${url.status} in ${url.total_ms} ms through the tunnel`
          : `failed${url.timeout ? " (timeout)" : ""}`
      }
    >
      {url.ok ? `${url.total_ms} ms` : url.timeout ? "timeout" : "failed"}
    </span>
  );
}

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
