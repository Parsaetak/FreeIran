import { useCallback, useEffect, useRef, useState } from "react";
import {
  appService,
  storageService,
  diagnosticsService,
  logService,
  call,
} from "../services";
import { useAppStore } from "../state/appStore";
import { describeError, toast } from "../state/toastStore";
import { levelAtLeast } from "../utilities/logFilter";
import {
  formatBytes,
  formatClock,
  formatNumber,
  formatPercent,
} from "../utilities/format";
import {
  EmptyState,
  SegmentedControl,
  StatTile,
} from "../components/common";
import {
  IconCopy,
  IconFolder,
  IconPause,
  IconPlay,
  IconRefresh,
  IconTrash,
} from "../components/Icons";
import type {
  CacheStats,
  CoreBinary,
  LogEntry,
  MetricsSnapshot,
  StorageDiagnostics,
  SystemInfo,
  VerifyResult,
} from "../services";

const LOG_CAP = 600;
const POLL_MS = 1000;
const COPY_LIMIT = 400;

type LevelFilter = "" | "debug" | "info" | "warn" | "error";

/** Diagnostics page: live runtime log viewer + subsystem stat cards. */
export function DiagnosticsPage() {
  return (
    <div>
      <div className="page-header">
        <div>
          <h1 className="page-title">Diagnostics</h1>
          <div className="page-subtitle">
            Live runtime log, storage health and engine metrics.
          </div>
        </div>
      </div>

      <LogViewer />
      <MaintenanceCards />
    </div>
  );
}

/* -------------------------------------------------------------------------
   Runtime log viewer
   ------------------------------------------------------------------------- */

function LogViewer() {
  const backend = useAppStore((state) => state.backend);

  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [lastSeq, setLastSeq] = useState(0);
  const [level, setLevel] = useState<LevelFilter>("");
  const [subsystem, setSubsystem] = useState("");
  const [subsystems, setSubsystems] = useState<string[]>([]);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [paused, setPaused] = useState(false);
  const [logPath, setLogPath] = useState("");
  const [failed, setFailed] = useState(false);

  const lastSeqRef = useRef(0);
  const atBottomRef = useRef(true);
  const scrollRef = useRef<HTMLDivElement>(null);

  // Debounce the text query (200ms).
  useEffect(() => {
    const timer = window.setTimeout(() => setQuery(queryInput.trim()), 200);

    return () => window.clearTimeout(timer);
  }, [queryInput]);

  const applyPage = useCallback(
    (page: { entries: LogEntry[]; last_seq: number }, replace: boolean) => {
      lastSeqRef.current = page.last_seq;
      setLastSeq(page.last_seq);
      setFailed(false);

      setEntries((previous) => {
        const merged = replace ? page.entries : [...previous, ...page.entries];

        return merged.length > LOG_CAP ? merged.slice(-LOG_CAP) : merged;
      });
    },
    [],
  );

  const fetchFull = useCallback(async () => {
    try {
      const page = await call(() =>
        logService.Recent({
          since_seq: 0,
          limit: LOG_CAP,
          subsystem,
          query,
          level,
        }),
      );

      applyPage(page, true);
    } catch {
      setFailed(true);
    }
  }, [subsystem, query, level, applyPage]);

  // Filter changes rebuild the view from the beginning of the buffer.
  useEffect(() => {
    lastSeqRef.current = 0;
    setEntries([]);
    void fetchFull();
  }, [fetchFull]);

  // Live tail poll (paused stops the timer, keeps the view).
  useEffect(() => {
    if (paused) return;

    const tick = async () => {
      try {
        const page = await call(() =>
          logService.Recent({
            since_seq: lastSeqRef.current,
            limit: 200,
            subsystem,
            query,
            level,
          }),
        );

        if (page.entries.length > 0) {
          applyPage(page, false);
        } else {
          lastSeqRef.current = page.last_seq;
          setLastSeq(page.last_seq);
        }
      } catch {
        setFailed(true);
      }
    };

    const timer = window.setInterval(() => void tick(), POLL_MS);

    return () => window.clearInterval(timer);
  }, [paused, subsystem, query, level, applyPage]);

  // Metadata: subsystems + log file location.
  useEffect(() => {
    void call(() => logService.Subsystems())
      .then((list) => setSubsystems(list))
      .catch(() => setSubsystems([]));

    void call(() => logService.LogFile())
      .then((path) => setLogPath(path))
      .catch(() => setLogPath(""));
  }, [backend?.last_ingestion]);

  // Keep pinned to the bottom only while the user is already there.
  useEffect(() => {
    const element = scrollRef.current;

    if (element && atBottomRef.current) {
      element.scrollTop = element.scrollHeight;
    }
  }, [entries]);

  const onScroll = () => {
    const element = scrollRef.current;

    if (!element) return;

    atBottomRef.current =
      element.scrollHeight - element.scrollTop - element.clientHeight < 40;
  };

  const clearDisplay = async () => {
    try {
      await call(() => logService.Clear());

      lastSeqRef.current = 0;
      atBottomRef.current = true;
      setEntries([]);
      setLastSeq(0);
      setSubsystems([]);

      toast("success", "Display cleared", "The on-disk log is preserved.");
      void fetchFull();
    } catch (error) {
      toast("error", "Could not clear", describeError(error));
    }
  };

  const copyDiagnostics = async () => {
    const text = entriesToText(entries.slice(-COPY_LIMIT));

    try {
      await navigator.clipboard.writeText(text);
      toast("success", "Diagnostics copied", `${Math.min(entries.length, COPY_LIMIT)} entries on the clipboard.`);
    } catch {
      toast("error", "Copy failed", "Clipboard access was denied by the platform.");
    }
  };

  const openLocation = async () => {
    try {
      await call(() => logService.OpenLogsDir());
    } catch (error) {
      toast("error", "Could not open location", describeError(error));
    }
  };

  const attention = entries.filter((entry) => levelAtLeast(entry.level, "warn")).length;

  return (
    <div className="card">
      <div className="card-header">
        <h3 className="card-title">Runtime log</h3>
        <span className="chip mono">{formatNumber(entries.length)} entries</span>
        {attention > 0 && (
          <span className="badge warn">{formatNumber(attention)} warn/error</span>
        )}
      </div>

      <div className="log-toolbar">
        <SegmentedControl<LevelFilter>
          ariaLabel="Severity filter"
          value={level}
          onChange={setLevel}
          options={[
            { value: "", label: "All" },
            { value: "debug", label: "Debug" },
            { value: "info", label: "Info" },
            { value: "warn", label: "Warn" },
            { value: "error", label: "Error" },
          ]}
        />

        <select
          className="select"
          aria-label="Subsystem filter"
          value={subsystem}
          onChange={(event) => setSubsystem(event.target.value)}
        >
          <option value="">All subsystems</option>
          {subsystems.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </select>

        <input
          className="input"
          placeholder="Filter by text…"
          aria-label="Filter log text"
          value={queryInput}
          onChange={(event) => setQueryInput(event.target.value)}
        />

        <button
          type="button"
          className="icon-btn"
          aria-label={paused ? "Resume live tail" : "Pause live tail"}
          title={paused ? "Resume live tail" : "Pause live tail"}
          onClick={() => setPaused((value) => !value)}
        >
          {paused ? <IconPlay size={15} /> : <IconPause size={15} />}
        </button>

        <button
          type="button"
          className="icon-btn"
          aria-label="Clear display"
          title="Clear display"
          onClick={() => void clearDisplay()}
        >
          <IconTrash size={15} />
        </button>

        <button
          type="button"
          className="icon-btn"
          aria-label="Copy diagnostics"
          title="Copy visible entries"
          onClick={() => void copyDiagnostics()}
        >
          <IconCopy size={15} />
        </button>

        <button
          type="button"
          className="icon-btn"
          aria-label="Open log location"
          title="Open log location"
          onClick={() => void openLocation()}
        >
          <IconFolder size={15} />
        </button>
      </div>

      <div className="log-entries" ref={scrollRef} onScroll={onScroll}>
        {entries.length === 0 ? (
          failed ? (
            <div className="log-empty">
              The log service is unreachable — the backend may be restarting.
            </div>
          ) : (
            <div className="log-empty">No entries match the current filters.</div>
          )
        ) : (
          entries.map((entry) => <LogRow key={entry.seq} entry={entry} />)
        )}
      </div>

      <div className="log-footer">
        <span data-tip={logPath || undefined}>
          {logPath ? `log: ${logPath}` : "log location unknown"}
        </span>
        {lastSeq > 0 && <span>seq {formatNumber(lastSeq)}</span>}

        <span className="follow-hint">
          <span className={`status-dot ${paused ? "bad" : "ok"}`} aria-hidden />
          {paused ? "paused" : "live"}
        </span>
      </div>
    </div>
  );
}

function LogRow({ entry }: { entry: LogEntry }) {
  return (
    <div className={`log-entry ${entry.level === "warn" || entry.level === "error" ? `level-${entry.level}` : ""}`}>
      <span className="log-time">{formatClock(entry.ts)}</span>
      <span className={`log-level ${entry.level || "info"}`}>{entry.level || "info"}</span>
      <span className="log-subsystem">{entry.subsystem}</span>
      <span className="log-event">{entry.event}</span>
      <span className="log-msg">
        {entry.message}
        {entry.operation && <span className="log-suffix">{entry.operation}</span>}
        {entry.error_kind && <span className="log-suffix error-kind">{entry.error_kind}</span>}
      </span>
    </div>
  );
}

/** Plain-text rendering used by "copy diagnostics". */
export function entriesToText(entries: LogEntry[]): string {
  return entries
    .map((entry) => {
      const suffix = [
        entry.operation ? `op=${entry.operation}` : "",
        entry.error_kind ? `error=${entry.error_kind}` : "",
      ]
        .filter(Boolean)
        .join(" ");

      return `${entry.ts} ${entry.level.toUpperCase().padEnd(5)} ${entry.subsystem} ${entry.event}: ${entry.message}${suffix ? ` (${suffix})` : ""}`;
    })
    .join("\n");
}

/* -------------------------------------------------------------------------
   Storage / metrics / system cards
   ------------------------------------------------------------------------- */

function MaintenanceCards() {
  const backend = useAppStore((state) => state.backend);

  const [cacheStats, setCacheStats] = useState<CacheStats | null>(null);
  const [metrics, setMetrics] = useState<MetricsSnapshot | null>(null);
  const [storeDiag, setStoreDiag] = useState<StorageDiagnostics | null>(null);
  const [systemInfo, setSystemInfo] = useState<SystemInfo | null>(null);
  const [cores, setCores] = useState<CoreBinary[]>([]);
  const [verifyResult, setVerifyResult] = useState<VerifyResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [legacyPath, setLegacyPath] = useState("");

  const reload = useCallback(async () => {
    try {
      const [caches, snap, diag, info, coreList] = await Promise.all([
        call(() => appService.CacheStats()),
        call(() => diagnosticsService.Metrics()),
        call(() => diagnosticsService.StoreDiagnostics()),
        call(() => diagnosticsService.SystemInfo()),
        call(() => diagnosticsService.Cores()),
      ]);

      setCacheStats(caches);
      setMetrics(snap);
      setStoreDiag(diag);
      setSystemInfo(info);
      setCores(coreList);
    } catch (error) {
      toast("error", "Diagnostics unavailable", describeError(error));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload, backend?.last_ingestion]);

  const runVerify = async () => {
    setBusy(true);

    try {
      const result = await call(() => storageService.Verify());

      setVerifyResult(result);

      toast(
        result.ok ? "success" : "error",
        result.ok ? "Integrity verified" : "Verification failed",
        result.ok
          ? `All ${formatNumber(result.chunks_checked)} chunks checked.`
          : result.error || "Unknown integrity error.",
      );
    } catch (error) {
      toast("error", "Verification failed", describeError(error));
    } finally {
      setBusy(false);
    }
  };

  const runCompact = async () => {
    setBusy(true);

    try {
      await call(() => storageService.Compact());
      await reload();
      toast("success", "Compaction finished", "The store was compacted.");
    } catch (error) {
      toast("error", "Compaction failed", describeError(error));
    } finally {
      setBusy(false);
    }
  };

  const runMigration = async () => {
    if (!legacyPath.trim()) {
      toast("info", "Path required", "Enter the path to a legacy JSON database first.");

      return;
    }

    setBusy(true);

    try {
      const result = await call(() => storageService.MigrateLegacy(legacyPath.trim()));

      toast(
        "success",
        "Migration complete",
        `Migrated ${formatNumber(result.migrated)} records, skipped ${formatNumber(result.skipped)}.`,
      );
      setLegacyPath("");
    } catch (error) {
      toast("error", "Migration failed", describeError(error));
    } finally {
      setBusy(false);
    }
  };

  const clearCaches = async () => {
    try {
      await call(() => appService.ClearCaches());
      await reload();
      toast("success", "Caches cleared");
    } catch (error) {
      toast("error", "Could not clear caches", describeError(error));
    }
  };

  return (
    <>
      {metrics && (
        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Engine metrics</h3>
          </div>

          <div className="stat-grid">
            <StatTile label="Startup" value={`${formatNumber(metrics.startup_ms)} ms`} />
            <StatTile label="Records processed" value={formatNumber(metrics.records_processed)} />
            <StatTile label="Deduplicated" value={formatNumber(metrics.records_deduplicated)} />
            <StatTile label="Cache hit rate" value={formatPercent(metrics.cache_hit_rate)} />
            <StatTile
              label="Chunks read / written"
              value={`${formatNumber(metrics.chunks_read)} / ${formatNumber(metrics.chunks_written)}`}
            />
            <StatTile
              label="Memory (heap)"
              value={formatBytes(metrics.memory_estimate_mb * 1024 * 1024)}
            />
            <StatTile label="Active workers" value={formatNumber(metrics.active_workers)} />
            <StatTile
              label="Tests executed"
              value={formatNumber(metrics.tests_executed)}
              sub={metrics.tests_working !== undefined ? `${formatNumber(metrics.tests_working)} working` : undefined}
            />
            <StatTile label="Native fallbacks" value={formatNumber(metrics.native_fallback_hits)} />
            <StatTile label="Goroutines" value={formatNumber(metrics.num_goroutine)} />
          </div>
        </div>
      )}

      {cacheStats && (
        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Caches</h3>
            <button type="button" className="btn sm" onClick={() => void clearCaches()}>
              <IconRefresh size={13} />
              Clear caches
            </button>
          </div>

          <div className="stat-grid">
            <StatTile label="Hot config entries" value={formatNumber(cacheStats.hot_config_entries)} />
            <StatTile label="Hot config hits" value={formatNumber(cacheStats.hot_config_hits)} />
            <StatTile label="Hot config misses" value={formatNumber(cacheStats.hot_config_misses)} />
            <StatTile label="Hot config hit rate" value={formatPercent(cacheStats.hot_config_hit_rate)} />
          </div>
        </div>
      )}

      {storeDiag && (
        <div className="card">
          <div className="card-header">
            <h3 className="card-title">Storage subsystem</h3>
            <span className={`badge ${storeDiag.status === "ok" ? "success" : "warn"}`}>
              {storeDiag.status}
            </span>
          </div>

          <div className="stat-grid">
            <StatTile
              label="Open chunk handles"
              value={`${formatNumber(storeDiag.open_files)} / ${formatNumber(storeDiag.open_files_max)}`}
            />
            <StatTile label="Handle cache hits" value={formatNumber(storeDiag.cache_hits)} />
            <StatTile label="Handle cache evictions" value={formatNumber(storeDiag.cache_evictions)} />
            <StatTile label="Pending tables" value={formatNumber(storeDiag.pending_tables)} />
            <StatTile label="Pending keys" value={formatNumber(storeDiag.pending_keys)} />
            <StatTile label="Memtable" value={formatBytes(storeDiag.memtable_bytes)} />
            <StatTile label="WAL size" value={formatBytes(storeDiag.wal_bytes)} />
            <StatTile label="WAL segments" value={formatNumber(storeDiag.wal_segments)} />
            <StatTile label="Flushes" value={formatNumber(storeDiag.flushes)} />
            <StatTile label="Last flush" value={`${formatNumber(storeDiag.last_flush_ms)} ms`} />
            <StatTile label="Compactions" value={formatNumber(storeDiag.compactions)} />
            <StatTile label="Last compaction" value={`${formatNumber(storeDiag.last_compact_ms)} ms`} />
          </div>

          {storeDiag.flush_error && (
            <div className="error-banner">Flush error: {storeDiag.flush_error}</div>
          )}
        </div>
      )}

      <div className="card">
        <div className="card-header">
          <h3 className="card-title">Storage maintenance</h3>
          {busy && (
            <span className="chip">
              working <span className="btn-spinner" aria-hidden />
            </span>
          )}
        </div>

        <div className="toolbar">
          <button type="button" className="btn" disabled={busy} onClick={() => void runVerify()}>
            Verify integrity
          </button>
          <button type="button" className="btn" disabled={busy} onClick={() => void runCompact()}>
            Compact
          </button>

          {verifyResult && (
            <span className={`badge ${verifyResult.ok ? "success" : "error"}`}>
              last verify: {formatNumber(verifyResult.chunks_checked)} chunks{" "}
              {verifyResult.ok ? "OK" : "failed"}
            </span>
          )}
        </div>

        <div className="toolbar">
          <input
            className="input"
            placeholder="Path to legacy freeiran JSON database"
            aria-label="Legacy database path"
            value={legacyPath}
            onChange={(event) => setLegacyPath(event.target.value)}
          />
          <button type="button" className="btn" disabled={busy} onClick={() => void runMigration()}>
            Migrate
          </button>
        </div>
      </div>

      {systemInfo && (
        <div className="card">
          <div className="card-header">
            <h3 className="card-title">System</h3>
          </div>

          <div className="stat-grid">
            <StatTile label="Platform" value={`${systemInfo.os}/${systemInfo.arch}`} />
            <StatTile label="CPU cores" value={formatNumber(systemInfo.num_cpu)} />
            <StatTile label="Go runtime" value={systemInfo.go_version} />
            <StatTile label="Host" value={systemInfo.hostname} />
          </div>
        </div>
      )}

      <div className="card">
        <div className="card-header">
          <h3 className="card-title">Discovered core binaries</h3>
        </div>

        {cores.length === 0 ? (
          <EmptyState
            title="No protocol cores discovered"
            hint="Place xray, sing-box or v2ray executables in the application cores directory."
          />
        ) : (
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Path</th>
                <th>Version</th>
              </tr>
            </thead>
            <tbody>
              {cores.map((core) => (
                <tr key={core.path}>
                  <td className="mono-cell">{core.name}</td>
                  <td className="mono-cell" data-tip={core.path}>
                    {core.path}
                  </td>
                  <td className="mono-cell">{core.version || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
