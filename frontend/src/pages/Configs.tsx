import { useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import {
  dataService,
  connectionService,
  call,
  type Config,
  type ConfigDetail,
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
import { EmptyState, ResultBadge } from "../components/common";
import { IconDownload, IconPlay, IconSearch, IconX } from "../components/Icons";

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

  const parentRef = useRef<HTMLDivElement>(null);

  const virtualizer = useVirtualizer({
    count: items.length,
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

  const visibleItems = useMemo(
    () => (protocol === "" ? items : items.filter((item) => String(item["type"]) === protocol)),
    [items, protocol],
  );

  // Keep the virtualizer window in sync with the filtered count.
  useEffect(() => {
    virtualizer.measure();
  }, [visibleItems.length, virtualizer]);

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
    <div>
      <div className="page-header">
        <div>
          <h1 className="page-title">Configurations</h1>
          <div className="page-subtitle">
            {formatNumber(items.length)} shown · {formatNumber(total)} total
            {hasMore && searchQuery.trim() === "" ? " · scroll to load more" : ""}
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

      <div className={`configs-layout ${detail ? "with-panel" : ""}`}>
        <div className="card flush mb-0">
          <div className="row header config-row-v2">
            <span>Proto</span>
            <span>Endpoint</span>
            <span className="hide-md">Transport</span>
            <span>Latency</span>
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

                      <span className={`latency ${latencyClass(Number(config["latency_ms"] ?? 0))}`}>
                        {formatLatency(Number(config["latency_ms"] ?? 0))}
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

/** CSS protocol class (defaults to unknown for exotic values). */
function protocolClass(type: string): string {
  return ["vless", "vmess", "trojan", "shadowsocks", "hysteria2", "hysteria", "tuic", "wireguard", "socks", "http", "unknown"].includes(type)
    ? type
    : "unknown";
}
