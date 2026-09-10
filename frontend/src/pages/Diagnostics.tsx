import { useEffect, useState } from "react";
import { appService, storageService, diagnosticsService, call } from "../services";
import { useAppStore } from "../state/appStore";
import {
  formatBytes,
  formatNumber,
  formatPercent,
} from "../utilities/format";
import type { CacheStats, CoreBinary, MetricsSnapshot, SystemInfo, VerifyResult } from "../services";

export function DiagnosticsPage() {
  const [cacheStats, setCacheStats] = useState<CacheStats | null>(null);
  const [metrics, setMetrics] = useState<MetricsSnapshot | null>(null);
  const [systemInfo, setSystemInfo] = useState<SystemInfo | null>(null);
  const [cores, setCores] = useState<CoreBinary[]>([]);
  const [verifyResult, setVerifyResult] = useState<VerifyResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const backend = useAppStore((state) => state.backend);

  const reload = async () => {
    try {
      const [caches, snap, info, coreList] = await Promise.all([
        call(() => appService.CacheStats()),
        call(() => diagnosticsService.Metrics()),
        call(() => diagnosticsService.SystemInfo()),
        call(() => diagnosticsService.Cores()),
      ]);

      setCacheStats(caches);
      setMetrics(snap);
      setSystemInfo(info);
      setCores(coreList);
    } catch (error) {
      setMessage(error instanceof Error ? error.message : String(error));
    }
  };

  useEffect(() => {
    void reload();
  }, [backend?.last_ingestion]);

  const runVerify = async () => {
    setBusy(true);
    setMessage(null);

    try {
      const result = await call(() => storageService.Verify());

      setVerifyResult(result);

      setMessage(
        result.ok
          ? `All ${result.chunks_checked} chunks verified.`
          : `Verification failed: ${result.error}`,
      );
    } catch (error) {
      setMessage(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  const runCompact = async () => {
    setBusy(true);
    setMessage(null);

    try {
      await call(() => storageService.Compact());

      setMessage("Compaction finished.");
      await reload();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  const runMigration = async () => {
    setBusy(true);
    setMessage(null);

    try {
      const result = await call(() =>
        storageService.MigrateLegacy(
          (document.getElementById("legacy-path") as HTMLInputElement).value,
        ),
      );

      setMessage(
        `Migrated ${result.migrated} records (skipped ${result.skipped}).`,
      );
    } catch (error) {
      setMessage(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <h2>Diagnostics</h2>

      {message && (
        <div
          className={verifyResult && !verifyResult.ok ? "error-banner" : ""}
          style={{ marginBottom: 14 }}
        >
          {message}
        </div>
      )}

      {metrics && (
        <div className="card">
          <h3 className="card-title">Engine metrics</h3>

          <div className="stat-grid">
            <Metric label="Startup" value={`${formatNumber(metrics.startup_ms)} ms`} />
            <Metric label="Records processed" value={formatNumber(metrics.records_processed)} />
            <Metric label="Deduplicated" value={formatNumber(metrics.records_deduplicated)} />
            <Metric label="Cache hit rate" value={formatPercent(metrics.cache_hit_rate)} />
            <Metric label="Chunks read / written" value={`${formatNumber(metrics.chunks_read)} / ${formatNumber(metrics.chunks_written)}`} />
            <Metric label="Memory (heap)" value={formatBytes(metrics.memory_estimate_mb * 1024 * 1024)} />
            <Metric label="Active workers" value={formatNumber(metrics.active_workers)} />
            <Metric label="Tests executed" value={formatNumber(metrics.tests_executed)} />
            <Metric label="Native fallbacks" value={formatNumber(metrics.native_fallback_hits)} />
            <Metric label="Goroutines" value={formatNumber(metrics.num_goroutine)} />
          </div>
        </div>
      )}

      {cacheStats && (
        <div className="card">
          <h3 className="card-title">Caches</h3>

          <div className="stat-grid">
            <Metric label="Hot config entries" value={formatNumber(cacheStats.hot_config_entries)} />
            <Metric label="Hot config hits" value={formatNumber(cacheStats.hot_config_hits)} />
            <Metric label="Hot config misses" value={formatNumber(cacheStats.hot_config_misses)} />
            <Metric label="Hot config hit rate" value={formatPercent(cacheStats.hot_config_hit_rate)} />
          </div>

          <button className="btn" onClick={() => void call(() => appService.ClearCaches()).then(reload)}>
            Clear caches
          </button>
        </div>
      )}

      <div className="card">
        <h3 className="card-title">Storage maintenance</h3>

        <div className="toolbar">
          <button className="btn" disabled={busy} onClick={() => void runVerify()}>
            Verify integrity
          </button>
          <button className="btn" disabled={busy} onClick={() => void runCompact()}>
            Compact
          </button>
        </div>

        <div className="toolbar">
          <input
            id="legacy-path"
            className="input"
            placeholder="Path to legacy freeiran JSON database"
          />
          <button className="btn" disabled={busy} onClick={() => void runMigration()}>
            Migrate
          </button>
        </div>
      </div>

      {systemInfo && (
        <div className="card">
          <h3 className="card-title">System</h3>

          <div className="stat-grid">
            <Metric label="Platform" value={`${systemInfo.os}/${systemInfo.arch}`} />
            <Metric label="CPU cores" value={formatNumber(systemInfo.num_cpu)} />
            <Metric label="Go runtime" value={systemInfo.go_version} />
            <Metric label="Host" value={systemInfo.hostname} />
          </div>
        </div>
      )}

      <div className="card">
        <h3 className="card-title">Protocol cores</h3>

        {cores.length === 0 ? (
          <p style={{ color: "var(--text-dim)", margin: 0 }}>
            No protocol cores discovered. Place xray, sing-box or WireGuard
            executables in the application cores directory.
          </p>
        ) : (
          cores.map((core) => (
            <div key={core.path} className="config-row">
              <span>{core.name}</span>
              <span>{core.path}</span>
              <span>{core.version}</span>
              <span />
              <span />
            </div>
          ))
        )}
      </div>
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="stat">
      <div className="stat-value" style={{ fontSize: 16 }}>
        {value}
      </div>
      <div className="stat-label">{label}</div>
    </div>
  );
}
