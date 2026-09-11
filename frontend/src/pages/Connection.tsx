import { useEffect, useState } from "react";
import { useConnectionStore } from "../state/connectionStore";
import { useConfigsStore } from "../state/stores";
import { call } from "../services";
import { connectionService } from "../services";
import type { BackendView, ConfigDetail, ConnectionSnapshot } from "../services";
import { formatDuration } from "../utilities/format";

/**
 * Connection page: protocol-core backend cards, the connection state
 * machine, configuration selection and the attempt history. All
 * credential material is redacted by the backend before it reaches
 * this view.
 */
export function ConnectionPage() {
  const snapshot = useConnectionStore((state) => state.snapshot);
  const backends = useConnectionStore((state) => state.backends);
  const busy = useConnectionStore((state) => state.busy);
  const error = useConnectionStore((state) => state.error);
  const connect = useConnectionStore((state) => state.connect);
  const disconnect = useConnectionStore((state) => state.disconnect);
  const reconnect = useConnectionStore((state) => state.reconnect);
  const refreshBackends = useConnectionStore((state) => state.refreshBackends);

  const items = useConfigsStore((state) => state.items);
  const loadPage = useConfigsStore((state) => state.loadPage);

  const [selected, setSelected] = useState("");
  const [detail, setDetail] = useState<ConfigDetail | null>(null);

  useEffect(() => {
    if (items.length === 0) {
      void loadPage(0);
    }
  }, [items.length, loadPage]);

  useEffect(() => {
    if (!selected) {
      setDetail(null);
      return;
    }

    let cancelled = false;

    void call(() => connectionService.ConfigDetails(selected)).then((result) => {
      if (!cancelled) setDetail(result ?? null);
    });

    return () => {
      cancelled = true;
    };
  }, [selected]);

  const connected = snapshot?.state === "connected";
  const active = snapshot !== null && isBusyState(snapshot.state);

  return (
    <div>
      <h2>Connection</h2>

      <ConnectionStatusCard snapshot={snapshot} />

      <div className="card" style={{ marginTop: 16 }}>
        <h3 className="card-title">Protocol cores</h3>

        <div className="stat-grid" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))" }}>
          {backends.map((backend) => (
            <BackendCard key={backend.name} backend={backend} />
          ))}
        </div>

        <button
          className="secondary"
          style={{ marginTop: 12 }}
          onClick={() => void refreshBackends()}
        >
          Re-detect cores
        </button>
      </div>

      <div className="card" style={{ marginTop: 16 }}>
        <h3 className="card-title">Connect a configuration</h3>

        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <select
            value={selected}
            onChange={(event) => setSelected(event.target.value)}
            style={{ minWidth: 320 }}
            disabled={active || busy}
          >
            <option value="">Select a configuration…</option>
            {items.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name || item.address} · {item.type} · {item.address}
              </option>
            ))}
          </select>

          <button
            disabled={!selected || active || busy}
            onClick={() => void connect(selected)}
          >
            Connect
          </button>

          <button
            disabled={!connected || active || busy}
            onClick={() => void disconnect()}
          >
            Disconnect
          </button>

          <button
            disabled={connected || active || busy || !snapshot?.config_id}
            onClick={() => void reconnect()}
          >
            Reconnect
          </button>
        </div>

        {error && (
          <div className="error-banner" style={{ marginTop: 12 }}>
            {error}
          </div>
        )}

        {detail && <ConfigDetailsPanel detail={detail} />}
      </div>

      {snapshot && (snapshot.attempts?.length ?? 0) > 0 && (
        <div className="card" style={{ marginTop: 16 }}>
          <h3 className="card-title">Attempt history</h3>

          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ color: "var(--text-dim)", textAlign: "left" }}>
                <th style={cellStyle}>Backend</th>
                <th style={cellStyle}>Result</th>
                <th style={cellStyle}>Duration</th>
                <th style={cellStyle}>Detail</th>
              </tr>
            </thead>
            <tbody>
              {snapshot.attempts?.map((attempt, index) => (
                <tr key={`${attempt.backend}-${index}`} style={{ borderTop: "1px solid var(--border)" }}>
                  <td style={cellStyle}>{attempt.backend}</td>
                  <td style={cellStyle}>
                    {attempt.ok ? (
                      <span className="badge working">started</span>
                    ) : (
                      <span className="badge failed">failed</span>
                    )}
                  </td>
                  <td style={monoCell}>{formatDuration(attempt.duration_ms)}</td>
                  <td style={cellStyle}>{attempt.error || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function ConnectionStatusCard({ snapshot }: { snapshot: ConnectionSnapshot | null }) {
  if (!snapshot || snapshot.state === "disconnected") {
    return (
      <div className="card">
        <div style={{ display: "flex", gap: 12, alignItems: "center" }}>
          <span className="badge unknown">disconnected</span>
          <span style={{ color: "var(--text-dim)" }}>
            No active session. Pick a configuration below and connect.
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="card">
      <div className="stat-grid">
        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            {snapshot.core || "—"}
          </div>
          <div className="stat-label">Core</div>
        </div>

        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            {snapshot.core_version || "—"}
          </div>
          <div className="stat-label">Version</div>
        </div>

        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            <StateBadge state={snapshot.state} />
          </div>
          <div className="stat-label">State</div>
        </div>

        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            {snapshot.latency_ms !== undefined && snapshot.latency_ms > 0
              ? `${snapshot.latency_ms} ms`
              : "—"}
          </div>
          <div className="stat-label">Latency</div>
        </div>

        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            {snapshot.config_name || snapshot.config_display || "—"}
          </div>
          <div className="stat-label">Configuration</div>
        </div>

        <div className="stat">
          <div className="stat-value" style={{ fontSize: 18 }}>
            {snapshot.endpoint || "—"}
          </div>
          <div className="stat-label">Local endpoint</div>
        </div>
      </div>

      {snapshot.last_error && (
        <div className="error-banner" style={{ marginTop: 10 }}>
          {snapshot.last_error}
        </div>
      )}
    </div>
  );
}

function StateBadge({ state }: { state: string }) {
  const className =
    state === "connected"
      ? "badge working"
      : state === "connection_failed"
        ? "badge failed"
        : "badge unknown";

  return <span className={className}>{state.replace(/_/g, " ")}</span>;
}

function BackendCard({ backend }: { backend: BackendView }) {
  const statusClass =
    backend.status === "available"
      ? "badge working"
      : backend.status === "invalid"
        ? "badge failed"
        : "badge unknown";

  return (
    <div className="stat" style={{ border: "1px solid var(--border)", borderRadius: 8, padding: 12 }}>
      <div className="stat-value" style={{ fontSize: 16 }}>
        {backend.name}
      </div>
      <div className="stat-label" style={{ display: "flex", gap: 6, alignItems: "center" }}>
        <span className={statusClass}>{backend.status}</span>
        {backend.version && <span>{backend.version}</span>}
      </div>
      {backend.status !== "available" && (
        <div style={{ marginTop: 6, color: "var(--text-dim)", fontSize: 12 }}>
          Install {backend.name} into the managed cores directory or PATH
          {backend.pinned_version ? ` (verified reference: ${backend.pinned_version})` : ""}.
        </div>
      )}
      {backend.summary && (
        <div style={{ marginTop: 6, color: "var(--text-dim)", fontSize: 12 }}>
          {backend.summary}
        </div>
      )}
    </div>
  );
}

function ConfigDetailsPanel({ detail }: { detail: ConfigDetail }) {
  return (
    <div style={{ marginTop: 16, borderTop: "1px solid var(--border)", paddingTop: 12 }}>
      <h4 style={{ margin: "0 0 8px" }}>Configuration details</h4>

      <dl className="detail-grid">
        <dt>Protocol</dt>
        <dd style={monoCell}>{detail.type}</dd>

        <dt>Address</dt>
        <dd style={monoCell}>
          {detail.address}:{detail.port}
        </dd>

        <dt>Transport</dt>
        <dd style={monoCell}>{detail.network || "tcp"}</dd>

        <dt>Security</dt>
        <dd style={monoCell}>{detail.security || "none"}</dd>

        {detail.path && (
          <>
            <dt>Path</dt>
            <dd style={monoCell}>{detail.path}</dd>
          </>
        )}

        {detail.host && (
          <>
            <dt>Host</dt>
            <dd style={monoCell}>{detail.host}</dd>
          </>
        )}

        {detail.service && (
          <>
            <dt>Service</dt>
            <dd style={monoCell}>{detail.service}</dd>
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
        </dd>

        {detail.source && (
          <>
            <dt>Source</dt>
            <dd>{detail.source}</dd>
          </>
        )}

        <dt>Credentials</dt>
        <dd style={{ color: "var(--text-dim)" }}>
          {credentialSummary(detail)}
        </dd>
      </dl>
    </div>
  );
}

function credentialSummary(detail: ConfigDetail): string {
  const parts: string[] = [];

  if (detail.has_uuid) parts.push("UUID (redacted)");
  if (detail.has_password) parts.push("password (redacted)");
  if (detail.has_public_key) parts.push("public key (redacted)");

  return parts.length > 0 ? parts.join(", ") : "none";
}

function isBusyState(state: string): boolean {
  return (
    state === "selecting" ||
    state === "preparing" ||
    state === "starting_core" ||
    state === "waiting_for_ready" ||
    state === "disconnecting"
  );
}

const cellStyle = { padding: "6px 8px" } as const;
const monoCell = { padding: "6px 8px", fontFamily: "var(--mono)" } as const;
