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
import { useConnectionStore } from "../state/connectionStore";
import { describeError, toast } from "../state/toastStore";
import { EmptyState, Menu, ResultBadge, SegmentedControl } from "../components/common";
import { IconChevronDown, IconDownload, IconPlay, IconRefresh, IconSearch, IconX } from "../components/Icons";

const searchRunner = makeSearchRunner(250);

const PROTOCOL_FILTERS = ["vless", "vmess", "trojan", "shadowsocks", "hysteria2"];

/** Short display label for a protocol value. */
function protocolLabel(type: string): string {
  if (type === "shadowsocks") return "ss";

  return type || "—";
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

  const parentRef = useRef<HTMLDivElement>(null);

  // The array actually rendered: the server-filtered result when one
  // exists, otherwise the client-side protocol filter over items.
  const visibleItems = useMemo(() => {
    if (filtered !== null) return filtered;
    if (protocol === "") return items;

    return items.filter((item) => String(item["type"]) === protocol);
  }, [items, protocol, filtered]);

  // The virtualizer must size against the array that is actually
  // RENDERED (visibleItems): sizing against the raw `items` while a
  // protocol filter narrows the list produced a huge blank scroll
  // range and wasted overscan rows.
  const virtualizer = useVirtualizer({
    count: visibleItems.length,
    estimateSize: () => 44,
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

  // Keep the virtualizer window in sync with the filtered count.
  useEffect(() => {
    virtualizer.measure();
  }, [visibleItems.length, virtualizer]);

  // Server-side filtered view: any status filter or sort activates it.
  const loadFiltered = useCallback(async (limit: number) => {
    if (statusFilter === "" && sortBy === "") {
      setFiltered(null);

      return;
    }

    setFilteredLoading(true);

    try {
      const page = await call(() =>
        dataService.ListConfigsFiltered(
          {
            protocol: protocol || undefined,
            status: statusFilter || undefined,
            query: searchQuery || undefined,
            sort_by: sortBy || undefined,
            sort_desc: sortBy === "latency",
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
  }, [protocol, statusFilter, sortBy, searchQuery]);

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
    void import("../workers/exportWorker?worker").then(({ default: ExportWorker }) => {
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
    });
  };

  const testConfig = async (config: Config) => {
    const id = String(config["id"]);

    setTestingId(id);

    try {
      await dataService.TestConfig(id);
      await runSearch();
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
       * Testing workspace toolbar (v0.9.1): primary actions are always
       * visible; secondary/recovery actions live in a compact overflow
       * menu so the toolbar can never overflow into the list below.
       */}
      <div className="toolbar">
        <button
          type="button"
          className="btn"
          disabled={filteredLoading || selected.size === 0}
          onClick={() => void bulkTest("selected")}
        >
          <IconPlay size={13} /> Test selected ({selected.size})
        </button>
        <button type="button" className="btn" disabled={filteredLoading} onClick={() => void bulkTest("all")}>
          <IconRefresh size={13} /> Test all
        </button>
        <button type="button" className="btn" disabled={filteredLoading} onClick={() => void bulkTest("untested")}>
          <IconRefresh size={13} /> Test untested
        </button>

        <div className="toolbar-spacer" />

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
              id: "retest-failed",
              label: "Retest failed",
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
        <section className="queue-panel" aria-label="Test queue progress">
          <div className="queue-head">
            <span className="queue-title">Testing</span>
            <span className="queue-count">
              {queueStats.total_completed}/{queueStats.total_enqueued} done
            </span>
            <div
              className={`progress ${queueStats.total_completed >= queueStats.total_enqueued ? "" : "indeterminate-none"}`}
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
              <span className="queue-chip queued">
                queued <b>{queueStats.queue_depth}</b>
              </span>
              <span className="queue-chip active">
                active <b>{queueStats.active_workers}</b>
              </span>
              <span className="queue-chip passed">
                passed <b>{queueStats.total_passed}</b>
              </span>
              <span className="queue-chip failed">
                failed <b>{queueStats.total_failed}</b>
              </span>
              <span className="queue-chip">
                cancelled <b>{queueStats.total_cancelled}</b>
              </span>
              {queueStats.total_timed_out > 0 && (
                <span className="queue-chip failed">
                  timed out <b>{queueStats.total_timed_out}</b>
                </span>
              )}
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
            <span />
          </div>

          <div
            ref={parentRef}
            className="config-scroll"
            onScroll={onListScroll}
            role="list"
            aria-label="Configurations"
          >
            {visibleItems.length === 0 && !loading && !searching ? (
              <EmptyState
                icon={<IconSearch size={20} />}
                title={searchQuery || protocol ? "No matching configurations" : "No configurations yet"}
                hint={
                  searchQuery || protocol
                    ? "Try a different search term or protocol filter."
                    : "Add a source and refresh to populate the database."
                }
              />
            ) : (
              <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
                {virtualizer.getVirtualItems().map((virtualRow) => {
                  const config = visibleItems[virtualRow.index];

                  if (!config) return null;

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

                      <button
                        type="button"
                        className="btn sm"
                        disabled={testingId === String(config["id"])}
                        onClick={(event) => {
                          event.stopPropagation();
                          void testConfig(config);
                        }}
                      >
                        {testingId === String(config["id"]) ? (
                          <span className="btn-spinner" aria-hidden />
                        ) : (
                          "Test"
                        )}
                      </button>
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

  return (
    <aside className="detail-panel" aria-label="Configuration details">
      <div className="card-header">
        <h3 className="card-title">{detail.name || `${detail.address}:${detail.port}`}</h3>
        <button type="button" className="icon-btn" aria-label="Close details" onClick={onClose}>
          <IconX size={14} />
        </button>
      </div>

      <div className="detail-panel-body">
        <div className="toolbar">
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
            {testing && <span className="btn-spinner" aria-hidden />}
            Test now
          </button>
        </div>

        <dl className="detail-grid">
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

          <dt>Backends</dt>
          <dd>{detail.compatible_backends?.join(", ") || "none compatible"}</dd>

          <dt>Status</dt>
          <dd>
            {detail.tested_at ? (
              <ResultBadge ok={detail.working} okLabel="working" failLabel="failed" />
            ) : (
              <span className="badge neutral">untested</span>
            )}
          </dd>

          <dt>Latency</dt>
          <dd className={`latency ${latencyClass(detail.latency_ms)}`}>
            {formatLatency(detail.latency_ms)}
          </dd>

          <dt>Last tested</dt>
          <dd>{detail.tested_at ? relativeTime(detail.tested_at) : "never"}</dd>

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
