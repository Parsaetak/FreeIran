import { useEffect, useMemo, useState } from "react";
import { useSettingsStore } from "../state/settingsStore";
import { useConnectionStore } from "../state/connectionStore";
import { type Settings } from "../services";
import { toast } from "../state/toastStore";
import { EmptyState } from "../components/common";

const BACKEND_OPTIONS = ["xray", "v2ray", "sing-box"] as const;
const TESTING_POLICIES = ["off", "on_add", "periodic"] as const;
const LOG_LEVELS = ["debug", "info", "warn", "error"] as const;

const REFRESH_MIN = 5;
const REFRESH_MAX = 1440;
const LOG_MB_MIN = 1;
const LOG_MB_MAX = 512;
const LOG_BACKUPS_MIN = 1;
const LOG_BACKUPS_MAX = 16;

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
    a.reduced_motion === b.reduced_motion
  );
}

/** Settings page (§30): preferences with inline validation + dirty state. */
export function SettingsPage() {
  const settings = useSettingsStore((state) => state.settings);
  const loading = useSettingsStore((state) => state.loading);
  const saving = useSettingsStore((state) => state.saving);
  const lastError = useSettingsStore((state) => state.lastError);
  const save = useSettingsStore((state) => state.save);
  const setMotionOverride = useSettingsStore((state) => state.setMotionOverride);

  const backends = useConnectionStore((state) => state.backends);

  const [draft, setDraft] = useState<Settings | null>(null);

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

  return (
    <div>
      <div className="page-header">
        <div>
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

      {lastError && <div className="error-banner">{lastError}</div>}

      <SettingErrors errors={errors} />

      <div className="card">
        <div className="card-header">
          <h3 className="card-title eyebrow">General</h3>
        </div>

        <div className="card-body settings-form">
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
              available on this machine.
            </span>
          </div>
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <h3 className="card-title eyebrow">Sources</h3>
        </div>

        <div className="card-body settings-form">
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
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <h3 className="card-title eyebrow">Runtime log</h3>
        </div>

        <div className="card-body settings-form">
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

          <div className="settings-row">
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
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <h3 className="card-title eyebrow">Accessibility</h3>
        </div>

        <div className="card-body settings-form">
          <div className="settings-row align-start">
            <div>
              <div className="field-label">Reduced motion</div>
              <div className="field-hint">
                Minimizes animation across the interface (connect pulses,
                streaming transitions, skeletons). Applied immediately.
              </div>
            </div>

            <button
              type="button"
              role="switch"
              aria-checked={draft.reduced_motion}
              aria-label="Reduced motion"
              className={`switch ${draft.reduced_motion ? "on" : ""}`}
              onClick={() => {
                const next = !draft.reduced_motion;

                update({ reduced_motion: next });
                setMotionOverride(next);
              }}
            />
          </div>
        </div>
      </div>
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

  return errors;
}

function fieldError(
  errors: Array<{ key: string; message: string }>,
  key: string,
): string | null {
  return errors.find((error) => error.key === key)?.message ?? null;
}
