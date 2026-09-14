import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSettingsStore } from "../state/settingsStore";
import { useConnectionStore } from "../state/connectionStore";
import {
  call,
  appService,
  diagnosticsService,
  storageService,
  logService,
  type DeveloperInfoView,
  type DiagnosticReportView,
  type Settings,
} from "../services";
import { describeError, toast } from "../state/toastStore";
import { EmptyState, TechDetails } from "../components/common";
import { ConfirmDialog } from "../components/Dialog";
import { IconFolder } from "../components/Icons";

const BACKEND_OPTIONS = ["xray", "v2ray", "sing-box"] as const;
const TESTING_POLICIES = ["off", "on_add", "periodic"] as const;
const LOG_LEVELS = ["debug", "info", "warn", "error"] as const;

const REFRESH_MIN = 5;
const REFRESH_MAX = 1440;
const LOG_MB_MIN = 1;
const LOG_MB_MAX = 512;
const LOG_BACKUPS_MIN = 1;
const LOG_BACKUPS_MAX = 16;
const QUEUE_WORKERS_MAX = 64;
const NET_TIMEOUT_MAX = 120;

/** Display defaults for unset (0) engine values. */
function normalize(settings: Settings): Settings {
  return {
    ...settings,
    refresh_interval_minutes: settings.refresh_interval_minutes || REFRESH_MIN,
    log_max_bytes_mb: settings.log_max_bytes_mb || 5,
    log_max_backups: settings.log_max_backups || 4,
  };
}

function sameSettings(a: Settings, b: Settings): boolean {
  return (
    a.preferred_backend === b.preferred_backend &&
    a.refresh_interval_minutes === b.refresh_interval_minutes &&
    a.testing_policy === b.testing_policy &&
    a.log_level === b.log_level &&
    a.log_max_bytes_mb === b.log_max_bytes_mb &&
    a.log_max_backups === b.log_max_backups &&
    a.reduced_motion === b.reduced_motion &&
    a.dev_verbose_diagnostics === b.dev_verbose_diagnostics &&
    a.dev_queue_workers === b.dev_queue_workers &&
    a.dev_net_timeout_seconds === b.dev_net_timeout_seconds &&
    a.dev_force_go_fallback === b.dev_force_go_fallback
  );
}

/**
 * Settings page (§30, IA reworked in v0.9.1): sections run from
 * everyday (General) to expert (Developer), each option showing a
 * label, a short explanation, its control and a sensible default.
 * Developer options are all wired to real engine behaviour — see
 * engine/app/loggingservice.go (Settings + applySettings).
 */
export function SettingsPage() {
  const settings = useSettingsStore((state) => state.settings);
  const loading = useSettingsStore((state) => state.loading);
  const saving = useSettingsStore((state) => state.saving);
  const lastError = useSettingsStore((state) => state.lastError);
  const save = useSettingsStore((state) => state.save);
  const setMotionOverride = useSettingsStore((state) => state.setMotionOverride);

  const backends = useConnectionStore((state) => state.backends);

  const [draft, setDraft] = useState<Settings | null>(null);
  const [devInfo, setDevInfo] = useState<DeveloperInfoView | null>(null);
  const [reportLoading, setReportLoading] = useState(false);
  const [reportCopied, setReportCopied] = useState(false);
  const [cacheCleared, setCacheCleared] = useState(false);
  const [rebuildConfirm, setRebuildConfirm] = useState(false);
  const [rebuildBusy, setRebuildBusy] = useState(false);

  // Developer info is read-only and cheap: load once, best-effort.
  useEffect(() => {
    let cancelled = false;

    void call(() => diagnosticsService.DeveloperInfo())
      .then((info) => {
        if (!cancelled) setDevInfo(info as DeveloperInfoView);
      })
      .catch(() => {
        /* the panel simply stays empty */
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const buildReport = async (): Promise<{ text: string } | null> => {
    setReportLoading(true);

    try {
      const report = (await call(() => diagnosticsService.BuildDiagnosticReport())) as DiagnosticReportView;
      const formatted = (report as unknown as { FormatDiagnosticReport?: () => string })
        .FormatDiagnosticReport?.() ?? formatReport(report);

      return { text: formatted };
    } catch (error) {
      toast("error", "Report unavailable", describeError(error));

      return null;
    } finally {
      setReportLoading(false);
    }
  };

  const copyReport = async () => {
    const report = await buildReport();

    if (!report) return;

    try {
      await navigator.clipboard.writeText(report.text);
      setReportCopied(true);
      window.setTimeout(() => setReportCopied(false), 2000);
    } catch {
      toast("error", "Clipboard unavailable", "Use Export report instead.");
    }
  };

  const exportReport = async () => {
    const report = await buildReport();

    if (!report) return;

    const blob = new Blob([report.text], { type: "text/plain" });
    const url = URL.createObjectURL(blob);

    const anchor = document.createElement("a");

    anchor.href = url;
    anchor.download = "freeiran-diagnostics.txt";
    anchor.click();

    URL.revokeObjectURL(url);
  };

  const openDataDir = async () => {
    try {
      await call(() => storageService.OpenDataDir());
    } catch (error) {
      toast("error", "Could not open data directory", describeError(error));
    }
  };

  const openLogsDir = async () => {
    try {
      await call(() => logService.OpenLogsDir());
    } catch (error) {
      toast("error", "Could not open logs directory", describeError(error));
    }
  };

  // v0.9.2 developer maintenance actions (safe vs destructive is kept
  // explicit: cleanup/runtime removal are safe; index rebuild touches
  // derived state and therefore asks for confirmation first).
  const openWorkspace = async () => {
    try {
      await call(() => storageService.OpenWorkspace());
    } catch (error) {
      toast("error", "Could not open workspace", describeError(error));
    }
  };

  const runCleanupNow = async () => {
    try {
      const result = await call(() => storageService.CleanupNow());

      if (result.rate_limited) {
        toast("info", "Cleanup skipped", "A cleanup pass just ran — try again in a minute.");
      } else {
        toast("success", "Cleanup finished", `Reclaimed ${result.bytes} bytes.`);
      }
    } catch (error) {
      toast("error", "Cleanup failed", describeError(error));
    }
  };

  const removeStaleRuntime = async () => {
    try {
      const reclaimed = await call(() => storageService.RemoveStaleRuntime());

      toast("success", "Runtime files removed", `Reclaimed ${Number(reclaimed)} bytes.`);
    } catch (error) {
      toast("error", "Could not remove runtime files", describeError(error));
    }
  };

  const rebuildIndex = async () => {
    setRebuildBusy(true);

    try {
      await call(() => storageService.RebuildIndex());
      toast("success", "Index rebuilt", "The configuration index was rebuilt from chunk files.");
    } catch (error) {
      toast("error", "Index rebuild failed", describeError(error));
    } finally {
      setRebuildBusy(false);
      setRebuildConfirm(false);
    }
  };

  const clearCaches = async () => {
    try {
      await call(() => appService.ClearCaches());
      setCacheCleared(true);
      window.setTimeout(() => setCacheCleared(false), 2000);
      toast("success", "Caches cleared", "Source and hot-config caches were emptied.");
    } catch (error) {
      toast("error", "Could not clear caches", describeError(error));
    }
  };

  // Reconcile the draft whenever the saved settings change and the
  // user is not mid-edit.
  useEffect(() => {
    if (settings && (draft === null || sameSettings(draft, settings))) {
      setDraft(normalize(settings));
    }
  }, [settings]);

  const dirty = useMemo(
    () => (settings && draft ? !sameSettings(normalize(settings), draft) : false),
    [settings, draft],
  );

  const errors = useMemo(() => validateDraft(draft), [draft]);

  if (loading && !draft) {
    return (
      <div className="skeleton-panel" aria-busy="true">
        <div className="skeleton title" />
        <div className="skeleton tile" />
        <div className="skeleton tile" />
      </div>
    );
  }

  if (!draft) {
    return (
      <EmptyState
        title={lastError ? "Settings unavailable" : "Settings not loaded"}
        hint={lastError ?? "The settings service did not answer."}
      />
    );
  }

  const update = (patch: Partial<Settings>) => {
    setDraft((current) => (current ? { ...current, ...patch } : current));
  };

  const onSave = async () => {
    if (!draft || errors.length > 0) return;

    const saved = await save(draft);

    if (saved) {
      setDraft(normalize(saved));
      toast("success", "Settings saved", "Preferences are now active.");
    } else {
      toast("error", "Could not save settings", lastError ?? "The backend rejected the change.");
    }
  };

  const onRevert = () => {
    if (settings) setDraft(normalize(settings));
    setMotionOverride(null);
  };

  const backendOptions = [
    ...new Set([...BACKEND_OPTIONS, ...backends.map((b) => b.name)]),
  ];

  const saveError = lastError ?? "";

  return (
    <div>
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Settings</h1>
          <div className="page-subtitle">
            Preferences persist to the application configuration directory and
            apply immediately where possible.
          </div>
        </div>

        <div className="page-actions">
          {dirty && <span className="badge warn">Unsaved changes</span>}

          <button type="button" className="btn" onClick={onRevert} disabled={!dirty || saving}>
            Revert
          </button>

          <button
            type="button"
            className="btn primary"
            disabled={!dirty || saving || errors.length > 0}
            onClick={() => void onSave()}
          >
            {saving && <span className="btn-spinner" aria-hidden />}
            Save
          </button>
        </div>
      </div>

      {saveError && (
        <div className="error-banner">
          <div>
            <div>Settings could not be saved. Fix the highlighted fields, then save again.</div>
            <TechDetails details={saveError} />
          </div>
        </div>
      )}

      <SettingErrors errors={errors} />

      <SettingsSection title="General" hint="Source refresh cadence and deployment mode.">
        <div className={`field ${fieldError(errors, "refresh") ? "invalid" : ""}`}>
          <label className="field-label" htmlFor="refresh-interval">
            Refresh interval (minutes)
          </label>

          <input
            id="refresh-interval"
            className="input"
            type="number"
            min={REFRESH_MIN}
            max={REFRESH_MAX}
            value={draft.refresh_interval_minutes}
            onChange={(event) =>
              update({ refresh_interval_minutes: Number(event.target.value) })
            }
          />

          {fieldError(errors, "refresh") ? (
            <span className="field-error">{fieldError(errors, "refresh")}</span>
          ) : (
            <span className="field-hint">
              How often sources are re-fetched ({REFRESH_MIN}–{REFRESH_MAX}).
            </span>
          )}
        </div>

        <InfoRow
          label="Deployment mode"
          value={devInfo ? (devInfo.portable_mode ? "Portable" : "Standard (per-user)") : "…"}
          hint={
            devInfo?.portable_mode
              ? "All state stays inside the deployment directory."
              : "State lives in the per-user application data directory."
          }
        />
      </SettingsSection>

      <SettingsSection title="Connection" hint="Which protocol core is preferred when connecting.">
        <div className="field">
          <label className="field-label" htmlFor="preferred-backend">
            Preferred core
          </label>

          <select
            id="preferred-backend"
            className="select"
            value={draft.preferred_backend}
            onChange={(event) => update({ preferred_backend: event.target.value })}
          >
            <option value="">Auto (engine default)</option>
            {backendOptions.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>

          <span className="field-hint">
            Only used when the core is compatible with the configuration and
            available on this machine; otherwise the engine falls back
            automatically.
          </span>
        </div>
      </SettingsSection>

      <SettingsSection title="Testing" hint="Background configuration testing policy.">
        <div className="field">
          <label className="field-label" htmlFor="testing-policy">
            Config testing policy
          </label>

          <select
            id="testing-policy"
            className="select"
            value={draft.testing_policy}
            onChange={(event) => update({ testing_policy: event.target.value })}
          >
            <option value="">Engine default</option>
            {TESTING_POLICIES.map((policy) => (
              <option key={policy} value={policy}>
                {policy}
              </option>
            ))}
          </select>

          <span className="field-hint">
            "on_add" tests configurations once when first stored; "periodic"
            re-tests them in the background.
          </span>
        </div>
      </SettingsSection>

      <SettingsSection title="Appearance" hint="Motion and visual comfort.">
        <ToggleRow
          id="reduced-motion"
          label="Reduced motion"
          hint="Minimizes animation across the interface (connect pulses, streaming transitions, skeletons). Applied immediately."
          checked={draft.reduced_motion}
          onChange={(next) => {
            update({ reduced_motion: next });
            setMotionOverride(next);
          }}
        />
      </SettingsSection>

      <SettingsSection
        title="Diagnostics & support"
        hint="Self-service tools for reports and logs. Nothing here sends data anywhere."
      >
        <ActionRow
          label="Diagnostic report"
          hint="A sanitized summary (version, platform, core states, storage and network status) for bug reports. Never contains credentials."
        >
          <button type="button" className="btn ghost" disabled={reportLoading} onClick={() => void copyReport()}>
            {reportCopied ? "Copied" : "Copy report"}
          </button>
          <button type="button" className="btn ghost" disabled={reportLoading} onClick={() => void exportReport()}>
            Export report
          </button>
        </ActionRow>

        <ActionRow
          label="Runtime log"
          hint="Minimum level and rotation limits for the persistent log file. Changes apply on save."
        >
          <div className="settings-row">
            <div className="field">
              <label className="field-label" htmlFor="log-level">
                Minimum level
              </label>

              <select
                id="log-level"
                className="select"
                value={draft.log_level}
                onChange={(event) => update({ log_level: event.target.value })}
              >
                <option value="">Engine default (info)</option>
                {LOG_LEVELS.map((lvl) => (
                  <option key={lvl} value={lvl}>
                    {lvl}
                  </option>
                ))}
              </select>
            </div>

            <div className={`field ${fieldError(errors, "log_mb") ? "invalid" : ""}`}>
              <label className="field-label" htmlFor="log-max-mb">
                Max size (MB)
              </label>

              <input
                id="log-max-mb"
                className="input"
                type="number"
                min={LOG_MB_MIN}
                max={LOG_MB_MAX}
                value={draft.log_max_bytes_mb}
                onChange={(event) =>
                  update({ log_max_bytes_mb: Number(event.target.value) })
                }
              />

              {fieldError(errors, "log_mb") && (
                <span className="field-error">{fieldError(errors, "log_mb")}</span>
              )}
            </div>

            <div className={`field ${fieldError(errors, "log_backups") ? "invalid" : ""}`}>
              <label className="field-label" htmlFor="log-max-backups">
                Kept backups
              </label>

              <input
                id="log-max-backups"
                className="input"
                type="number"
                min={LOG_BACKUPS_MIN}
                max={LOG_BACKUPS_MAX}
                value={draft.log_max_backups}
                onChange={(event) =>
                  update({ log_max_backups: Number(event.target.value) })
                }
              />

              {fieldError(errors, "log_backups") && (
                <span className="field-error">{fieldError(errors, "log_backups")}</span>
              )}
            </div>
          </div>
        </ActionRow>

        <ActionRow
          label="Application directories"
          hint="Inspect the data directory (database, caches) or the runtime logs."
        >
          <button type="button" className="btn ghost" onClick={() => void openDataDir()}>
            <IconFolder size={13} />
            Open data
          </button>
          <button type="button" className="btn ghost" onClick={() => void openLogsDir()}>
            <IconFolder size={13} />
            Open logs
          </button>
        </ActionRow>
      </SettingsSection>

      <SettingsSection
        title="Developer"
        hint="Advanced engine controls. Defaults are right for almost everyone."
        action={
          <button
            type="button"
            className="btn sm ghost"
            disabled={!dirty && draft.dev_queue_workers === 0 && draft.dev_net_timeout_seconds === 0 && !draft.dev_force_go_fallback && !draft.dev_verbose_diagnostics}
            onClick={() =>
              update({
                dev_verbose_diagnostics: false,
                dev_queue_workers: 0,
                dev_net_timeout_seconds: 0,
                dev_force_go_fallback: false,
              })
            }
          >
            Reset to defaults
          </button>
        }
      >
        <ToggleRow
          id="dev-verbose"
          label="Verbose diagnostics"
          hint="Adds runtime detail (memory, native acceleration, paths, queue internals) to the diagnostic report."
          checked={draft.dev_verbose_diagnostics}
          onChange={(next) => update({ dev_verbose_diagnostics: next })}
        />

        <ToggleRow
          id="dev-fallback"
          label="Force Go fallback for native acceleration"
          hint="Pins hashing and URL scanning to the portable Go implementation, exactly like FREEIRAN_NATIVE=off. Applied immediately."
          checked={draft.dev_force_go_fallback}
          onChange={(next) => update({ dev_force_go_fallback: next })}
        />

        <div className={`field ${fieldError(errors, "dev_workers") ? "invalid" : ""}`}>
          <label className="field-label" htmlFor="dev-queue-workers">
            Test queue workers (0 = adaptive)
          </label>

          <input
            id="dev-queue-workers"
            className="input"
            type="number"
            min={0}
            max={QUEUE_WORKERS_MAX}
            value={draft.dev_queue_workers}
            onChange={(event) => update({ dev_queue_workers: Number(event.target.value) })}
          />

          {fieldError(errors, "dev_workers") ? (
            <span className="field-error">{fieldError(errors, "dev_workers")}</span>
          ) : (
            <span className="field-hint">
              Fixed worker-pool size for the test queue (0–{QUEUE_WORKERS_MAX}). 0 lets the
              memory booster adapt to system pressure.
            </span>
          )}
        </div>

        <div className={`field ${fieldError(errors, "dev_net_timeout") ? "invalid" : ""}`}>
          <label className="field-label" htmlFor="dev-net-timeout">
            Network-test timeout (seconds, 0 = default)
          </label>

          <input
            id="dev-net-timeout"
            className="input"
            type="number"
            min={0}
            max={NET_TIMEOUT_MAX}
            value={draft.dev_net_timeout_seconds}
            onChange={(event) => update({ dev_net_timeout_seconds: Number(event.target.value) })}
          />

          {fieldError(errors, "dev_net_timeout") ? (
            <span className="field-error">{fieldError(errors, "dev_net_timeout")}</span>
          ) : (
            <span className="field-hint">
              Per-probe timeout for Network Diagnostics (0–{NET_TIMEOUT_MAX}). Raise it on
              high-latency links.
            </span>
          )}
        </div>

        <ActionRow
          label="Runtime caches"
          hint="Empties the source and hot-config caches. Configurations, sources and settings are not touched."
        >
          <button type="button" className="btn ghost" onClick={() => void clearCaches()}>
            {cacheCleared ? "Cleared" : "Clear caches"}
          </button>
        </ActionRow>

        <ActionRow
          label="Workspace cleanup"
          hint="Runs the safe cleanup pass: stale runtime files, obsolete WAL segments, dead chunks, old staging. Never touches configurations or settings."
        >
          <button type="button" className="btn ghost" onClick={() => void runCleanupNow()}>
            Cleanup now
          </button>
          <button type="button" className="btn ghost" onClick={() => void removeStaleRuntime()}>
            Remove stale runtime files
          </button>
        </ActionRow>

        <ActionRow
          label="Rebuild configuration index"
          hint="Rebuilds the derived index from the immutable chunk files. Stored data is not modified; the store briefly pauses reads while the index is rewritten."
        >
          <button type="button" className="btn" disabled={rebuildBusy} onClick={() => setRebuildConfirm(true)}>
            {rebuildBusy ? "Rebuilding…" : "Rebuild index"}
          </button>
        </ActionRow>

        <ActionRow
          label="Workspace folder"
          hint="Shows the workspace path (also listed below) and opens it in the system file manager. Every subsystem — config, data, cache, logs, cores, runtime — lives under this single root."
        >
          <button type="button" className="btn ghost" onClick={() => void openWorkspace()}>
            <IconFolder aria-hidden /> Open workspace
          </button>
        </ActionRow>

        {devInfo && (
          <dl className="detail-grid dev-info">
            <dt>Version</dt>
            <dd className="mono-cell">
              {devInfo.version} ({devInfo.commit})
            </dd>
            <dt>Runtime</dt>
            <dd className="mono-cell">
              {devInfo.go_version} · {devInfo.platform}
            </dd>
            <dt>Native acceleration</dt>
            <dd>{devInfo.native_acceleration}</dd>
            <dt>Queue (live)</dt>
            <dd className="mono-cell">
              depth {devInfo.queue_depth} · active {devInfo.active_workers} · enqueued{" "}
              {devInfo.total_enqueued} · passed {devInfo.total_passed} · failed {devInfo.total_failed}
            </dd>
            <dt>Workspace</dt>
            <dd className="mono-cell">
              {devInfo.base_dir}
              {" · "}
              {devInfo.workspace_writable === false ? "read-only" : "writable"}
            </dd>
            <dt>Data directory</dt>
            <dd className="mono-cell">{devInfo.data_dir}</dd>
            <dt>Logs directory</dt>
            <dd className="mono-cell">{devInfo.logs_dir}</dd>
            <dt>Runtime directory</dt>
            <dd className="mono-cell">{devInfo.runtime_dir || "—"}</dd>
            {devInfo.migration?.migrated && (
              <>
                <dt>Workspace migration</dt>
                <dd className="mono-cell">
                  {devInfo.migration.files ?? 0} files from {devInfo.migration.source}
                </dd>
              </>
            )}
          </dl>
        )}
      </SettingsSection>

      <SettingsSection title="About" hint="Build identity and license.">
        <dl className="detail-grid">
          <dt>Application</dt>
          <dd>FreeIran — free, open-source VPN configuration manager</dd>
          <dt>Version</dt>
          <dd className="mono-cell">{devInfo ? `${devInfo.version} (${devInfo.commit})` : "…"}</dd>
          <dt>License</dt>
          <dd>MIT — see LICENSE in the installation directory</dd>
        </dl>
      </SettingsSection>

      {/* v0.9.2: index rebuild is the only maintenance action that
          rewrites derived state — confirm before running. */}
      <ConfirmDialog
        open={rebuildConfirm}
        title="Rebuild configuration index?"
        body="The index will be rebuilt from the immutable chunk files. Configurations are not modified. The store pauses briefly while the new index is written."
        confirmLabel={rebuildBusy ? "Rebuilding…" : "Rebuild"}
        busy={rebuildBusy}
        onConfirm={() => void rebuildIndex()}
        onCancel={() => {
          if (!rebuildBusy) setRebuildConfirm(false);
        }}
      />
    </div>
  );
}

/* ----- section + row primitives (shared settings look) ----- */

function SettingsSection({
  title,
  hint,
  action,
  children,
}: {
  title: string;
  hint?: string;
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="card">
      <div className="card-header">
        <div className="card-heading">
          <h3 className="card-title eyebrow">{title}</h3>
          {hint && <div className="card-subtitle">{hint}</div>}
        </div>
        {action && <div className="card-header-actions">{action}</div>}
      </div>

      <div className="card-body settings-form">{children}</div>
    </div>
  );
}

/** Label + switch row. */
function ToggleRow({
  id,
  label,
  hint,
  checked,
  onChange,
}: {
  id: string;
  label: string;
  hint: string;
  checked: boolean;
  onChange: (next: boolean) => void;
}) {
  return (
    <div className="settings-row align-start">
      <div>
        <label className="field-label" htmlFor={id}>
          {label}
        </label>
        <div className="field-hint">{hint}</div>
      </div>

      <button
        id={id}
        type="button"
        role="switch"
        aria-checked={checked}
        aria-label={label}
        className={`switch ${checked ? "on" : ""}`}
        onClick={() => onChange(!checked)}
      />
    </div>
  );
}

/** Label + arbitrary actions row. */
function ActionRow({
  label,
  hint,
  children,
}: {
  label: string;
  hint: string;
  children: ReactNode;
}) {
  return (
    <div className="settings-row align-start">
      <div>
        <div className="field-label">{label}</div>
        <div className="field-hint">{hint}</div>
      </div>

      <div className="toolbar mb-0">{children}</div>
    </div>
  );
}

/** Read-only label + value row. */
function InfoRow({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="settings-row align-start">
      <div>
        <div className="field-label">{label}</div>
        {hint && <div className="field-hint">{hint}</div>}
      </div>

      <span className="badge neutral">{value}</span>
    </div>
  );
}

function SettingErrors({
  errors,
}: {
  errors: Array<{ key: string; message: string }>;
}) {
  if (errors.length === 0) return null;

  return (
    <div className="error-banner">
      <div>
        {errors.map((error) => (
          <div key={error.key}>{error.message}</div>
        ))}
      </div>
    </div>
  );
}

/** Field-scoped validation messages (keyed for inline placement). */
function validateDraft(draft: Settings | null): Array<{ key: string; message: string }> {
  if (!draft) return [];

  const errors: Array<{ key: string; message: string }> = [];

  if (
    !Number.isInteger(draft.refresh_interval_minutes) ||
    draft.refresh_interval_minutes < REFRESH_MIN ||
    draft.refresh_interval_minutes > REFRESH_MAX
  ) {
    errors.push({
      key: "refresh",
      message: `Refresh interval must be a whole number between ${REFRESH_MIN} and ${REFRESH_MAX} minutes.`,
    });
  }

  if (
    !Number.isInteger(draft.log_max_bytes_mb) ||
    draft.log_max_bytes_mb < LOG_MB_MIN ||
    draft.log_max_bytes_mb > LOG_MB_MAX
  ) {
    errors.push({
      key: "log_mb",
      message: `Log size limit must be between ${LOG_MB_MIN} and ${LOG_MB_MAX} MB.`,
    });
  }

  if (
    !Number.isInteger(draft.log_max_backups) ||
    draft.log_max_backups < LOG_BACKUPS_MIN ||
    draft.log_max_backups > LOG_BACKUPS_MAX
  ) {
    errors.push({
      key: "log_backups",
      message: `Log backups must be between ${LOG_BACKUPS_MIN} and ${LOG_BACKUPS_MAX}.`,
    });
  }

  if (
    !Number.isInteger(draft.dev_queue_workers) ||
    draft.dev_queue_workers < 0 ||
    draft.dev_queue_workers > QUEUE_WORKERS_MAX
  ) {
    errors.push({
      key: "dev_workers",
      message: `Test queue workers must be a whole number between 0 and ${QUEUE_WORKERS_MAX} (0 = adaptive).`,
    });
  }

  if (
    !Number.isInteger(draft.dev_net_timeout_seconds) ||
    draft.dev_net_timeout_seconds < 0 ||
    draft.dev_net_timeout_seconds > NET_TIMEOUT_MAX
  ) {
    errors.push({
      key: "dev_net_timeout",
      message: `Network-test timeout must be a whole number between 0 and ${NET_TIMEOUT_MAX} seconds (0 = default).`,
    });
  }

  return errors;
}

function fieldError(
  errors: Array<{ key: string; message: string }>,
  key: string,
): string | null {
  return errors.find((error) => error.key === key)?.message ?? null;
}

/** Local renderer used when the backend method result arrives as a plain object. */
function formatReport(report: DiagnosticReportView): string {
  const lines = [
    "FreeIran Diagnostic Report",
    `Version: ${report.version}`,
    `Platform: ${report.platform}`,
    `Generated: ${report.generated_at}`,
    "",
    `Application status: ${report.app_status}`,
    `Configurations: ${report.config_count}`,
    `Storage: ${report.storage}`,
    `Connection: ${report.connection}`,
    `Network: ${report.network_state}${report.network_note ? ` — ${report.network_note}` : ""}`,
    "",
    "Cores:",
    ...(report.cores && report.cores.length > 0 ? report.cores.map((c) => `  ${c}`) : ["  (none discovered)"]),
  ];

  if (report.warnings && report.warnings.length > 0) {
    lines.push("", "Warnings:", ...report.warnings.map((w) => `  ${w}`));
  }

  if (report.technical && report.technical.length > 0) {
    lines.push("", "Technical details:", ...report.technical.map((t) => `  ${t}`));
  }

  return lines.join("\n");
}
