import { useAppStore } from "../state/appStore";
import {
  formatBytes,
  formatDuration,
  formatNumber,
  relativeTime,
} from "../utilities/format";
import type { IngestionStats } from "../services";

export function DashboardPage() {
  const backend = useAppStore((state) => state.backend);
  const status = useAppStore((state) => state.status);

  if (!backend) {
    return (
      <div className="loading-overlay">
        <div className="spinner" /> Waiting for backend…
      </div>
    );
  }

  const ingestion = backend.last_ingestion as IngestionStats | null;

  return (
    <div>
      <h2>Dashboard</h2>

      <div className="stat-grid">
        <div className="stat">
          <div className="stat-value">{formatNumber(backend.config_count)}</div>
          <div className="stat-label">Configurations</div>
        </div>

        <div className="stat">
          <div className="stat-value">{formatNumber(backend.storage?.count ?? 0)}</div>
          <div className="stat-label">Stored records</div>
        </div>

        <div className="stat">
          <div className="stat-value">{formatNumber(backend.storage?.chunk_count ?? 0)}</div>
          <div className="stat-label">Chunks</div>
        </div>

        <div className="stat">
          <div className="stat-value">{formatBytes(backend.storage?.disk_bytes ?? 0)}</div>
          <div className="stat-label">Storage used</div>
        </div>

        <div className="stat">
          <div className="stat-value">{backend.native_acceleration}</div>
          <div className="stat-label">Native acceleration</div>
        </div>
      </div>

      {status === "degraded" && (
        <div className="error-banner">
          Storage verification reported problems — see Diagnostics.
        </div>
      )}

      {ingestion ? (
        <div className="card">
          <h3 className="card-title">Last ingestion</h3>

          <div className="stat-grid">
            <Stat label="Sources OK" value={`${ingestion.sources_ok}/${ingestion.sources_total}`} />
            <Stat label="Discovered" value={formatNumber(ingestion.discovered)} />
            <Stat label="Duplicates" value={formatNumber(ingestion.duplicates)} />
            <Stat label="Persisted" value={formatNumber(ingestion.persisted)} />
            <Stat label="Invalid" value={formatNumber(ingestion.invalid)} />
            <Stat label="Unchanged" value={formatNumber(ingestion.sources_unchanged)} />
          </div>

          <SourceTable stats={ingestion} />
        </div>
      ) : (
        <div className="card">
          <h3 className="card-title">Last ingestion</h3>
          <p style={{ color: "var(--text-dim)", margin: 0 }}>
            No ingestion cycle has completed yet. {backend.started_at > 0 &&
              `(app started ${relativeTime(backend.started_at)})`}
          </p>
        </div>
      )}
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="stat">
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}

function SourceTable({ stats }: { stats: IngestionStats }) {
  const rows = stats.per_source ?? [];

  if (rows.length === 0) return null;

  return (
    <table style={{ width: "100%", marginTop: 14, borderCollapse: "collapse" }}>
      <thead>
        <tr style={{ color: "var(--text-dim)", textAlign: "left" }}>
          <th style={cellStyle}>Source</th>
          <th style={cellStyle}>Result</th>
          <th style={cellStyle}>Found</th>
          <th style={cellStyle}>Unique</th>
          <th style={cellStyle}>Time</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((row) => (
          <tr key={row.source_id} style={{ borderTop: "1px solid var(--border)" }}>
            <td style={cellStyle}>{row.source_id}</td>
            <td style={cellStyle}>
              {row.error ? (
                <span className="badge failed">{row.error.slice(0, 40)}</span>
              ) : row.unchanged ? (
                <span className="badge unknown">unchanged</span>
              ) : (
                <span className="badge working">ok</span>
              )}
            </td>
            <td style={monoCell}>{formatNumber(row.discovered)}</td>
            <td style={monoCell}>{formatNumber(row.unique)}</td>
            <td style={monoCell}>{formatDuration(row.duration_ms)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

const cellStyle = { padding: "6px 8px" } as const;
const monoCell = { ...cellStyle, fontFamily: "var(--mono)" } as const;
