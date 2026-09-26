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
import { SORT_MODES, TEST_MODES } from "../types/discovery";

const BACKEND_OPTIONS = ["xray", "v2ray", "sing-box"] as const;
const TESTING_POLICIES = ["off", "on_add", "periodic"] as const;
const LOG_LEVELS = ["debug", "info", "warn", "error"] as const;

/**
 * v0.9.8.4 logging profiles (roadmap P1 §15): ONE authoritative
 * policy lives in the logger (internal/logging); this control just
 * selects it. Explanations are concrete — no vague wording.
 */
const LOGGING_PROFILES: Array<{
  value: string;
  label: string;
  description: string;
}> = [
  {
    value: "normal",
    label: "Normal",
    description: "Compact operational log. Answers: what happened, is the app healthy, what failed. Bulk testing is summarized (start / progress / completion), routine per-config core launches are not recorded one by one; failures always are.",
  },
  {
    value: "detailed",
    label: "Detailed",
    description: "Troubleshooting detail on top of Normal: per-launch core lifecycle (start/ready/exit), correlation ids, queue and recovery diagnostics. Bounded — no per-test spam.",
  },
  {
    value: "debug",
    label: "Debug",
    description: "Full diagnostic verbosity: every subsystem record with structured identifiers. Still redacted, rotated, deduplicated and retention-bounded.",
  },
];

const REFRESH_MIN = 5;
const REFRESH_MAX = 1440;
const LOG_MB_MIN = 1;
const LOG_MB_MAX = 512;
const LOG_BACKUPS_MIN = 1;
const LOG_BACKUPS_MAX = 16;
const LOG_RETENTION_MIN = 1;
const LOG_RETENTION_MAX = 365;
const QUEUE_WORKERS_MAX = 64;
const NET_TIMEOUT_MAX = 120;

/** Display defaults for unset (0) engine values. */
function normalize(settings: Settings): Settings {
  return {
    ...settings,
    refresh_interval_minutes: settings.refresh_interval_minutes || REFRESH_MIN,
    log_max_bytes_mb: settings.log_max_bytes_mb || 5,
    log_max_backups: settings.log_max_backups || 4,
    log_retention_days: settings.log_retention_days || 7,
    logging_profile: settings.logging_profile || "normal",
    local_socks_port: settings.local_socks_port || 0,
    local_http_port: settings.local_http_port || 0,
  };
}

function sameSettings(a: Settings, b: Settings): boolean {
  return (
    a.preferred_backend === b.preferred_backend &&
    a.refresh_interval_minutes === b.refresh_interval_minutes &&
    a.testing_policy === b.testing_policy &&
    a.log_level === b.log_level &&
    a.logging_profile === b.logging_profile &&
    a.log_max_bytes_mb === b.log_max_bytes_mb &&
    a.log_max_backups === b.log_max_backups &&
    a.log_retention_days === b.log_retention_days &&
    a.local_socks_port === b.local_socks_port &&
    a.local_http_port === b.local_http_port &&
    a.reduced_motion === b.reduced_motion &&
    a.allow_untrusted_public_routes === b.allow_untrusted_public_routes &&
    a.dev_verbose_diagnostics === b.dev_verbose_diagnostics &&
    a.dev_queue_workers === b.dev_queue_workers &&
    a.dev_net_timeout_seconds === b.dev_net_timeout_seconds &&
    a.dev_force_go_fallback === b.dev_force_go_fallback &&
    a.disable_auto_recovery === b.disable_auto_recovery
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
            <span className="btn-icon-slot" aria-hidden>
              {saving && <span className="btn-spinner" />}
            </span>
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

      <SettingsSection
        title="Test modes & ranking"
        hint="v0.9.6: how candidates are measured and ranked (§9/§10)."
      >
        <div className="field">
          <label className="field-label" htmlFor="test-mode">
            Test mode
          </label>

          <select
            id="test-mode"
            className="select"
            value={draft.test_mode || "ping_url"}
            onChange={(event) => update({ test_mode: event.target.value })}
          >
            {TEST_MODES.map((mode) => (
              <option key={mode.id} value={mode.id}>
                {mode.label}
              </option>
            ))}
          </select>

          <span className="field-hint">
            Ping measures real endpoint latency over repeated TCP samples; URL
            proves usable HTTP connectivity through the tunnel; Full runs the
            complete verification.
          </span>
        </div>

        <div className="field">
          <label className="field-label" htmlFor="test-samples">
            Ping samples: {draft.test_ping_samples || 4}
          </label>

          <input
            id="test-samples"
            type="range"
            min={1}
            max={16}
            step={1}
            value={draft.test_ping_samples || 4}
            onChange={(event) =>
              update({ test_ping_samples: Number(event.target.value) })
            }
          />

          <span className="field-hint">
            More samples give a stabler median and jitter estimate; each sample
            costs one TCP round trip.
          </span>
        </div>

        <div className="field">
          <label className="field-label" htmlFor="test-url">
            URL test target
          </label>

          <input
            id="test-url"
            type="text"
            className="input"
            placeholder="https://www.gstatic.com/generate_204 (default)"
            value={draft.test_url ?? ""}
            onChange={(event) => update({ test_url: event.target.value })}
          />

          <span className="field-hint">
            The endpoint URL tests request through each candidate's tunnel. A
            204-style endpoint keeps the measurement small and cache-free.
          </span>
        </div>

        <div className="field">
          <label className="field-label" htmlFor="test-max">
            Max candidates per flow: {draft.test_max_candidates || 20}
          </label>

          <input
            id="test-max"
            type="range"
            min={5}
            max={200}
            step={5}
            value={draft.test_max_candidates || 20}
            onChange={(event) =>
              update({ test_max_candidates: Number(event.target.value) })
            }
          />

          <span className="field-hint">
            Bounds how many candidates one Smart Start run measures (5–200).
            Defaults stay lightweight.
          </span>
        </div>

        <div className="field">
          <label className="field-label" htmlFor="sort-mode">
            Ranking order
          </label>

          <select
            id="sort-mode"
            className="select"
            value={draft.sort_mode || "best_overall"}
            onChange={(event) => update({ sort_mode: event.target.value })}
          >
            {SORT_MODES.map((mode) => (
              <option key={mode.id} value={mode.id}>
                {mode.label}
              </option>
            ))}
          </select>

          <span className="field-hint">
            Ping-sorted lists only display MEASURED latencies — estimated or
            stale numbers are labelled, never silently substituted.
          </span>
        </div>

        <ToggleRow
          id="enable-racing"
          label="Race top candidates"
          hint="When connecting, the top 2–4 candidates are raced in parallel and the first VERIFIED-USABLE connection wins; losing attempts are cancelled cleanly."
          checked={Boolean(draft.enable_racing)}
          onChange={(next) => update({ enable_racing: next })}
        />

        {draft.enable_racing ? (
          <div className="field">
            <label className="field-label" htmlFor="racing-candidates">
              Racers: {draft.racing_candidates || 2}
            </label>

            <input
              id="racing-candidates"
              type="range"
              min={2}
              max={4}
              step={1}
              value={draft.racing_candidates || 2}
              onChange={(event) =>
                update({ racing_candidates: Number(event.target.value) })
              }
            />

            <span className="field-hint">
              Each racer temporarily starts one protocol-core process for the
              duration of the race.
            </span>
          </div>
        ) : null}
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
        title="Reliability"
        hint="What FreeIran does when the active connection fails."
      >
        <ToggleRow
          id="auto-recovery"
          label="Automatic recovery"
          hint="When the active connection fails, FreeIran switches to the next best healthy candidate automatically (bounded retries, recently failed servers are skipped). This is the engine's core behaviour — disable only if you want full manual control."
          checked={!draft.disable_auto_recovery}
          onChange={(next) => update({ disable_auto_recovery: !next })}
        />
        <ToggleRow
          id="allow-untrusted-public-routes"
          label="Allow public untrusted routes in Quick Connect"
          hint="Off by default: Quick Connect and Auto only connect through official or user-configured sources. Public nodes stay fully usable through explicit selection on the Configs page. A public node can be fast, stable and verified reachable while remaining untrusted — reliability and route trust are separate dimensions."
          checked={draft.allow_untrusted_public_routes ?? false}
          onChange={(next) => update({ allow_untrusted_public_routes: next })}
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
          label="Logging"
          hint="How much the persistent runtime log records. The change applies immediately — no restart. Secrets are never logged in any profile."
        >
          <div className="field">
            <div
              className="segmented"
              role="radiogroup"
              aria-label="Logging profile"
            >
              {LOGGING_PROFILES.map((profile) => (
                <button
                  key={profile.value}
                  type="button"
                  role="radio"
                  aria-checked={draft.logging_profile === profile.value}
                  className={`segmented-item ${
                    draft.logging_profile === profile.value ? "active" : ""
                  }`}
                  onClick={() => update({ logging_profile: profile.value })}
                >
                  {profile.label}
                </button>
              ))}
            </div>

            <span className="field-hint" aria-live="polite">
              {
                LOGGING_PROFILES.find(
                  (profile) => profile.value === (draft.logging_profile || "normal"),
                )?.description
              }
            </span>
          </div>
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

            <div className={`field ${fieldError(errors, "log_retention") ? "invalid" : ""}`}>
              <label className="field-label" htmlFor="log-retention-days">
                Retention (days)
              </label>

              <input
                id="log-retention-days"
                className="input"
                type="number"
                min={LOG_RETENTION_MIN}
                max={LOG_RETENTION_MAX}
                value={draft.log_retention_days}
                onChange={(event) =>
                  update({ log_retention_days: Number(event.target.value) })
                }
              />

              {fieldError(errors, "log_retention") && (
                <span className="field-error">{fieldError(errors, "log_retention")}</span>
              )}
            </div>
          </div>
        </ActionRow>

        <ActionRow
          label="Local proxy ports"
          hint="SOCKS5 inbound used by protocol-core sessions (1024-65535, 0 = automatic). The optional HTTP inbound can be enabled with its own port. Conflicts are detected before launch and reported; the actual port is never changed silently."
        >
          <div className="field-grid two">
            <div className={`field ${fieldError(errors, "socks_port") ? "invalid" : ""}`}>
              <label className="field-label" htmlFor="local-socks-port">
                SOCKS5 port (0 = automatic)
              </label>

              <input
                id="local-socks-port"
                className="input"
                type="number"
                min={0}
                max={65535}
                placeholder="10808"
                value={draft.local_socks_port || 0}
                onChange={(event) =>
                  update({ local_socks_port: Number(event.target.value) })
                }
              />

              {fieldError(errors, "socks_port") && (
                <span className="field-error">{fieldError(errors, "socks_port")}</span>
              )}
            </div>

            <div className={`field ${fieldError(errors, "http_port") ? "invalid" : ""}`}>
              <label className="field-label" htmlFor="local-http-port">
                HTTP port (0 = disabled)
              </label>

              <input
                id="local-http-port"
                className="input"
                type="number"
                min={0}
                max={65535}
                placeholder="10809"
                value={draft.local_http_port || 0}
                onChange={(event) =>
                  update({ local_http_port: Number(event.target.value) })
                }
              />

              {fieldError(errors, "http_port") && (
                <span className="field-error">{fieldError(errors, "http_port")}</span>
              )}
            </div>
          </div>
        </ActionRow>

        <ActionRow
          label="Application directories"
          hint="Inspect the data directory (database, caches) or the runtime logs."
        >
          <button type="button" className="btn ghost" onClick={() => void openDataDir()}>
            <IconFolder size={14} />
            Open data
          </button>
          <button type="button" className="btn ghost" onClick={() => void openLogsDir()}>
            <IconFolder size={14} />
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
            <IconFolder size={14} aria-hidden /> Open workspace
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

  // v0.9.8.7: the regenerated (truthful) bindings mark these numeric
  // settings optional (they are pointers in Go). Narrow through
  // locals so undefined is rejected by the same validation that
  // bounds the value.
  const refreshMinutes = draft.refresh_interval_minutes;

  if (
    refreshMinutes === undefined ||
    !Number.isInteger(refreshMinutes) ||
    refreshMinutes < REFRESH_MIN ||
    refreshMinutes > REFRESH_MAX
  ) {
    errors.push({
      key: "refresh",
      message: `Refresh interval must be a whole number between ${REFRESH_MIN} and ${REFRESH_MAX} minutes.`,
    });
  }

  const logMaxBytes = draft.log_max_bytes_mb;

  if (
    logMaxBytes === undefined ||
    !Number.isInteger(logMaxBytes) ||
    logMaxBytes < LOG_MB_MIN ||
    logMaxBytes > LOG_MB_MAX
  ) {
    errors.push({
      key: "log_mb",
      message: `Log size limit must be between ${LOG_MB_MIN} and ${LOG_MB_MAX} MB.`,
    });
  }

  const logBackups = draft.log_max_backups;

  if (
    logBackups === undefined ||
    !Number.isInteger(logBackups) ||
    logBackups < LOG_BACKUPS_MIN ||
    logBackups > LOG_BACKUPS_MAX
  ) {
    errors.push({
      key: "log_backups",
      message: `Log backups must be between ${LOG_BACKUPS_MIN} and ${LOG_BACKUPS_MAX}.`,
    });
  }

  const logRetention = draft.log_retention_days;

  if (
    logRetention === undefined ||
    !Number.isInteger(logRetention) ||
    logRetention < LOG_RETENTION_MIN ||
    logRetention > LOG_RETENTION_MAX
  ) {
    errors.push({
      key: "log_retention",
      message: `Log retention must be between ${LOG_RETENTION_MIN} and ${LOG_RETENTION_MAX} days.`,
    });
  }

  const devQueueWorkers = draft.dev_queue_workers;

  if (
    devQueueWorkers === undefined ||
    !Number.isInteger(devQueueWorkers) ||
    devQueueWorkers < 0 ||
    devQueueWorkers > QUEUE_WORKERS_MAX
  ) {
    errors.push({
      key: "dev_workers",
      message: `Test queue workers must be a whole number between 0 and ${QUEUE_WORKERS_MAX} (0 = adaptive).`,
    });
  }

  const devNetTimeout = draft.dev_net_timeout_seconds;

  if (
    devNetTimeout === undefined ||
    !Number.isInteger(devNetTimeout) ||
    devNetTimeout < 0 ||
    devNetTimeout > NET_TIMEOUT_MAX
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
