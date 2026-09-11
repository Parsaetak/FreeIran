import { useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useConfigsStore, makeSearchRunner } from "../state/stores";
import { dataService, connectionService, call, type Config, type ConfigDetail } from "../services";
import { formatDuration, formatNumber, relativeTime, truncate } from "../utilities/format";
import { configToRow } from "../utilities/export";

const searchRunner = makeSearchRunner(250);

/**
 * Virtualized configuration list.
 *
 * Only the visible window (~30 rows) is rendered to the DOM
 * regardless of dataset size; scrolling is handled by the
 * virtualizer. This keeps the UI responsive with very large datasets.
 */
export function ConfigsPage() {
  const items = useConfigsStore((state) => state.items);
  const total = useConfigsStore((state) => state.total);
  const loading = useConfigsStore((state) => state.loading);
  const searchQuery = useConfigsStore((state) => state.searchQuery);
  const lastError = useConfigsStore((state) => state.lastError);

  const setSearchQuery = useConfigsStore((state) => state.setSearchQuery);

  const [detail, setDetail] = useState<ConfigDetail | null>(null);

  const parentRef = useRef<HTMLDivElement>(null);

  const virtualizer = useVirtualizer({
    count: items.length,
    estimateSize: () => 36,
    overscan: 12,
    getScrollElement: () => parentRef.current,
  });

  const exportCSV = () => {
    void import("../workers/exportWorker?worker").then(
      ({ default: ExportWorker }) => {
        const worker = new ExportWorker();

        worker.postMessage({
          type: "export",
          rows: items.map(configToRow),
        });

        worker.onmessage = (
          event: MessageEvent<{ type: string; csv: string }>,
        ) => {
          if (event.data.type !== "export:done") return;

          const blob = new Blob([event.data.csv], { type: "text/csv" });
          const url = URL.createObjectURL(blob);

          const anchor = document.createElement("a");

          anchor.href = url;
          anchor.download = "freeiran-export.csv";
          anchor.click();

          URL.revokeObjectURL(url);
          worker.terminate();
        };
      },
    );
  };

  const testConfig = async (config: Config) => {
    try {
      await dataService.TestConfig(config["id"] as string);
      await useConfigsStore.getState().runSearch();
    } catch (error) {
      console.error("test failed", error);
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
      console.error("details failed", error);
    }
  };

  return (
    <div>
      <div className="toolbar">
        <h2 style={{ margin: 0, flex: 1 }}>Configurations</h2>

        <button className="btn" onClick={exportCSV}>
          Export CSV
        </button>
      </div>

      {lastError && <div className="error-banner">{lastError}</div>}

      <div className="toolbar">
        <input
          className="input"
          placeholder="Search by address, name or protocol…"
          value={searchQuery}
          onChange={(event) => {
            setSearchQuery(event.target.value);
            searchRunner();
          }}
        />
        <span style={{ color: "var(--text-dim)", fontSize: 12 }}>
          {formatNumber(items.length)} shown · {formatNumber(total)} total
        </span>
      </div>

      <div className="card" style={{ padding: 0 }}>
        <div className="config-row header">
          <span>Type</span>
          <span>Endpoint</span>
          <span>Status</span>
          <span>Latency</span>
          <span />
        </div>

        <div
          ref={parentRef}
          style={{ height: "calc(100vh - 280px)", overflowY: "auto" }}
        >
          <div
            style={{
              height: virtualizer.getTotalSize(),
              position: "relative",
            }}
          >
            {virtualizer.getVirtualItems().map((virtualRow) => {
              const config = items[virtualRow.index];

              if (!config) return null;

              return (
                <div
                  key={String(config["id"])}
                  className="config-row"
                  style={{
                    position: "absolute",
                    top: 0,
                    left: 0,
                    width: "100%",
                    transform: `translateY(${virtualRow.start}px)`,
                  }}
                >
                  <span className="badge working">
                    {String(config["type"])}
                  </span>
                  <span title={String(config["address"])}>
                    {truncate(String(config["address"]), 48)}:
                    {String(config["port"])}
                  </span>
                  <span>
                    <Badge config={config} />
                  </span>
                  <span>
                    {config["working"]
                      ? formatDuration(Number(config["latency_ms"] ?? 0))
                      : "—"}
                  </span>
                  <button
                    className="btn"
                    onClick={() => void testConfig(config)}
                  >
                    Test
                  </button>
                  <button
                    className="btn"
                    onClick={() => void showDetails(config)}
                  >
                    …
                  </button>
                </div>
              );
            })}
          </div>
        </div>

        {loading && (
          <div className="loading-overlay">
            <div className="spinner" /> Loading…
          </div>
        )}
      </div>

      {detail && <ConfigDetailsCard detail={detail} />}
    </div>
  );
}

function ConfigDetailsCard({ detail }: { detail: ConfigDetail }) {
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <h3 className="card-title">
        Configuration details{detail.name ? ` — ${detail.name}` : ""}
      </h3>

      <dl className="detail-grid">
        <dt>Protocol</dt>
        <dd style={{ fontFamily: "var(--mono)" }}>{detail.type}</dd>

        <dt>Address</dt>
        <dd style={{ fontFamily: "var(--mono)" }}>
          {detail.address}:{detail.port}
        </dd>

        <dt>Transport</dt>
        <dd style={{ fontFamily: "var(--mono)" }}>{detail.network || "tcp"}</dd>

        <dt>Security</dt>
        <dd style={{ fontFamily: "var(--mono)" }}>{detail.security || "none"}</dd>

        {detail.path && (
          <>
            <dt>Path</dt>
            <dd style={{ fontFamily: "var(--mono)" }}>{detail.path}</dd>
          </>
        )}

        {detail.host && (
          <>
            <dt>Host</dt>
            <dd style={{ fontFamily: "var(--mono)" }}>{detail.host}</dd>
          </>
        )}

        {detail.service && (
          <>
            <dt>gRPC service</dt>
            <dd style={{ fontFamily: "var(--mono)" }}>{detail.service}</dd>
          </>
        )}

        {detail.server_name && (
          <>
            <dt>Server name</dt>
            <dd style={{ fontFamily: "var(--mono)" }}>{detail.server_name}</dd>
          </>
        )}

        {detail.method && (
          <>
            <dt>Cipher</dt>
            <dd style={{ fontFamily: "var(--mono)" }}>{detail.method}</dd>
          </>
        )}

        <dt>Backends</dt>
        <dd>{detail.compatible_backends?.join(", ") || "none compatible"}</dd>

        <dt>Status</dt>
        <dd>
          {detail.working ? (
            <span className="badge working">working</span>
          ) : (
            <span className="badge unknown">untested</span>
          )}
          {detail.latency_ms !== undefined && detail.latency_ms > 0 && (
            <span> · {detail.latency_ms} ms</span>
          )}
          {detail.tested_at !== undefined && detail.tested_at > 0 && (
            <span> · tested {relativeTime(detail.tested_at)}</span>
          )}
        </dd>

        {detail.source && (
          <>
            <dt>Source</dt>
            <dd>{detail.source}</dd>
          </>
        )}

        <dt>Credentials</dt>
        <dd style={{ color: "var(--text-dim)" }}>
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
  );
}

function Badge({ config }: { config: Config }) {
  if (!config["tested_at"]) {
    return <span className="badge unknown">untested</span>;
  }

  return config["working"] ? (
    <span className="badge working">working</span>
  ) : (
    <span className="badge failed">failed</span>
  );
}
