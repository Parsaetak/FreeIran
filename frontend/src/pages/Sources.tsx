import { useEffect, useState } from "react";
import { useSourcesStore } from "../state/stores";
import { useReliabilityStore } from "../state/collectionsStore";
import { truncate } from "../utilities/format";
import { describeError, toast } from "../state/toastStore";
import { ConfirmDialog, FormDialog } from "../components/Dialog";
import { EmptyState } from "../components/common";
import { IconPlus, IconRefresh, IconSources, IconTrash } from "../components/Icons";
import { formatLatency } from "../utilities/format";

/** Sources page: managed subscription lists with refresh flow. */
export function SourcesPage() {
  const sources = useSourcesStore((state) => state.sources);
  const loading = useSourcesStore((state) => state.loading);
  const refreshing = useSourcesStore((state) => state.refreshing);
  const lastError = useSourcesStore((state) => state.lastError);
  const lastIngestion = useSourcesStore((state) => state.lastIngestion);

  const setEnabled = useSourcesStore((state) => state.setEnabled);
  const add = useSourcesStore((state) => state.add);
  const remove = useSourcesStore((state) => state.remove);
  const refresh = useSourcesStore((state) => state.refresh);

  const [pendingIds, setPendingIds] = useState<string[]>([]);
  const [removeTarget, setRemoveTarget] = useState<{ id: string; label: string } | null>(null);
  const [removing, setRemoving] = useState(false);
  const [addOpen, setAddOpen] = useState(false);

  // v0.9.10: the evidence-based reliability dashboard — loaded per
  // mount (the backend serves it cached) and reloaded after each
  // refresh cycle completes (evidence changed).
  const reliability = useReliabilityStore((state) => state.report);
  const reliabilityLoading = useReliabilityStore((state) => state.loading);
  const loadReliability = useReliabilityStore((state) => state.load);

  useEffect(() => {
    void loadReliability();
  }, [loadReliability]);

  useEffect(() => {
    if (!refreshing && lastIngestion) {
      void loadReliability();
    }
  }, [refreshing, lastIngestion, loadReliability]);

  const toggle = async (id: string, enabled: boolean) => {
    setPendingIds((current) => [...current, id]);

    try {
      await setEnabled(id, enabled);
    } catch (error) {
      toast("error", "Could not update source", describeError(error));
    } finally {
      setPendingIds((current) => current.filter((value) => value !== id));
    }
  };

  const refreshNow = async () => {
    try {
      const stats = await refresh();

      if (stats) {
        toast(
          "success",
          "Refresh complete",
          `${stats.discovered} discovered · ${stats.persisted} new · ${stats.duplicates} duplicates`,
        );
      } else {
        toast("info", "Refresh complete");
      }
    } catch (error) {
      toast("error", "Refresh failed", describeError(error));
    }
  };

  return (
    <div>
      <div className="page-header">
        <div className="page-heading">
          <h1 className="page-title">Sources</h1>
          <div className="page-subtitle">
            Subscription lists scanned for proxy configurations.
          </div>
        </div>

        <div className="page-actions">
          <button type="button" className="btn" onClick={() => setAddOpen(true)}>
            <IconPlus size={14} />
            Add source
          </button>

          <button
            type="button"
            className="btn primary"
            onClick={() => void refreshNow()}
            disabled={refreshing}
          >
            <span className="btn-icon-slot" aria-hidden>
              {refreshing ? <span className="btn-spinner" /> : <IconRefresh size={14} />}
            </span>
            Refresh now
          </button>
        </div>
      </div>

      {/* Flowing progress bar while ingesting */}
      {refreshing && (
        <div className="progress indeterminate" role="progressbar" aria-label="Refreshing sources">
          <div className="progress-bar" />
        </div>
      )}

      {lastError && <div className="error-banner">{lastError}</div>}

      {/* v0.9.10: source reliability dashboard — measured evidence
          only; “Not enough data” where evidence is missing. */}
      <SourceReliabilityCard report={reliability} loading={reliabilityLoading} />

      {lastIngestion && !refreshing && (
        <div className="toolbar">
          <span className="badge success">last refresh</span>
          <span className="chip mono">{lastIngestion.discovered} fetched</span>
          <span className="chip mono">{lastIngestion.persisted} persisted</span>
          <span className="chip mono">{lastIngestion.duplicates} duplicates</span>
          {lastIngestion.invalid > 0 && (
            <span className="chip mono">{lastIngestion.invalid} invalid</span>
          )}
        </div>
      )}

      <div className="card flush">
        <div className="card-header">
          <h3 className="card-title">Configured sources</h3>
          <span className="chip mono">{sources.length}</span>
        </div>

        {loading ? (
          <div className="loading-inline">
            <span className="btn-spinner" aria-hidden /> Loading…
          </div>
        ) : sources.length === 0 ? (
          <EmptyState
            icon={<IconSources size={20} />}
            title="No sources configured"
            hint="Add a subscription URL to start discovering configurations."
            action={
              <button type="button" className="btn primary" onClick={() => setAddOpen(true)}>
                <IconPlus size={14} />
                Add your first source
              </button>
            }
          />
        ) : (
          sources.map((source) => {
            const pending = pendingIds.includes(source.id);

            return (
              <div className="row source-row" key={source.id}>
                <button
                  type="button"
                  role="switch"
                  aria-checked={source.enabled}
                  aria-label={`${source.enabled ? "Disable" : "Enable"} ${source.name || source.id}`}
                  className={`switch ${source.enabled ? "on" : ""}`}
                  disabled={pending}
                  onClick={() => void toggle(source.id, !source.enabled)}
                />

                <span className="cell-main">
                  <div className="cell-title">
                    {source.name || source.id}
                    {source.name && <span className="cell-sub-inline"> ({source.id})</span>}
                  </div>
                  <div className="cell-sub" data-tip={source.url}>
                    {truncate(source.url, 76)}
                  </div>
                </span>

                {source.trust === "user" || source.trust === "official" ? (
                  <span className="badge success hide-sm" data-tip="Official or user-configured source: eligible for automatic route selection.">
                    {source.trust === "official" ? "official" : "user"}
                  </span>
                ) : (
                  <span
                    className="badge warn hide-sm"
                    data-tip="Public third-party source: untrusted routes. Quick Connect excludes these unless you enable 'Allow public untrusted routes' in Settings."
                  >
                    public · untrusted
                  </span>
                )}

                {source.last_hash ? (
                  <span className="badge success hide-sm">synced · {source.last_hash.slice(0, 8)}</span>
                ) : (
                  <span className="badge neutral hide-sm">not fetched</span>
                )}

                <button
                  type="button"
                  className="btn sm danger"
                  aria-label={`Remove ${source.name || source.id}`}
                  onClick={() =>
                    setRemoveTarget({ id: source.id, label: source.name || source.id })
                  }
                >
                  <IconTrash size={13} />
                  Remove
                </button>
              </div>
            );
          })
        )}
      </div>

      <ConfirmDialog
        open={removeTarget !== null}
        title="Remove source"
        body={
          <>
            Remove <b>{removeTarget?.label}</b> from the source list? Already
            stored configurations are kept; only future refreshes skip it.
          </>
        }
        confirmLabel="Remove"
        danger
        busy={removing}
        onCancel={() => setRemoveTarget(null)}
        onConfirm={() => {
          if (!removeTarget) return;

          setRemoving(true);

          void remove(removeTarget.id)
            .then(() => {
              toast("success", "Source removed", removeTarget.label);
              setRemoveTarget(null);
            })
            .catch((error: unknown) => {
              toast("error", "Could not remove source", describeError(error));
            })
            .finally(() => setRemoving(false));
        }}
      />

      <AddSourceDialog
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onAdd={async (id, name, url) => {
          await add(id, name, url);
          toast("success", "Source added", name || id);
        }}
      />
    </div>
  );
}

/** Add-source dialog with client-side validation. */
function AddSourceDialog({
  open,
  onClose,
  onAdd,
}: {
  open: boolean;
  onClose: () => void;
  onAdd: (id: string, name: string, url: string) => Promise<void>;
}) {
  const [formId, setFormId] = useState("");
  const [formName, setFormName] = useState("");
  const [formUrl, setFormUrl] = useState("");
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const idTaken = useSourcesStore((state) =>
    state.sources.some((source) => source.id === formId.trim()),
  );

  const idInvalid = formId.trim() !== "" && !/^[a-zA-Z0-9][a-zA-Z0-9_-]*$/.test(formId.trim());
  const urlInvalid = formUrl.trim() !== "" && !/^https?:\/\//i.test(formUrl.trim());

  const valid =
    formId.trim() !== "" &&
    formUrl.trim() !== "" &&
    !idInvalid &&
    !urlInvalid &&
    !idTaken;

  const submit = async () => {
    if (!valid) return;

    setBusy(true);
    setFormError(null);

    try {
      await onAdd(formId.trim(), formName.trim(), formUrl.trim());

      setFormId("");
      setFormName("");
      setFormUrl("");
      setFormError(null);
      onClose();
    } catch (error) {
      setFormError(describeError(error, "The backend rejected this source."));
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDialog
      open={open}
      title="Add source"
      submitLabel="Add source"
      busy={busy}
      onSubmit={() => void submit()}
      onCancel={() => {
        setFormError(null);
        onClose();
      }}
    >
      <div className={`field ${formId && (idInvalid || idTaken) ? "invalid" : ""}`}>
        <label className="field-label" htmlFor="source-id">
          Identifier
        </label>

        <input
          id="source-id"
          className="input"
          placeholder="my-source"
          value={formId}
          onChange={(event) => setFormId(event.target.value)}
          autoFocus
        />

        {idTaken ? (
          <span className="field-error">A source with this identifier already exists.</span>
        ) : idInvalid ? (
          <span className="field-error">
            Use letters, numbers, dashes or underscores (starting with a letter or digit).
          </span>
        ) : (
          <span className="field-hint">Unique short identifier used in logs and stats.</span>
        )}
      </div>

      <div className="field">
        <label className="field-label" htmlFor="source-name">
          Display name <span className="field-hint">(optional)</span>
        </label>

        <input
          id="source-name"
          className="input"
          placeholder="Community list"
          value={formName}
          onChange={(event) => setFormName(event.target.value)}
        />
      </div>

      <div className={`field ${formUrl && urlInvalid ? "invalid" : ""}`}>
        <label className="field-label" htmlFor="source-url">
          URL
        </label>

        <input
          id="source-url"
          className="input"
          placeholder="https://…"
          value={formUrl}
          onChange={(event) => setFormUrl(event.target.value)}
        />

        {urlInvalid ? (
          <span className="field-error">The URL must start with http:// or https://.</span>
        ) : (
          <span className="field-hint">Plain-text subscription containing share links.</span>
        )}
      </div>

      {formError && <div className="error-banner">{formError}</div>}
    </FormDialog>
  );
}

/**
 * v0.9.10 source reliability dashboard: an evidence-based health view
 * of every source. Numbers come ONLY from the backend's measured
 * evidence (fetch counters, last refresh pipeline, per-source store
 * scan); where evidence is insufficient the card says
 * "Not enough data" instead of inventing a score (§6).
 */
function SourceReliabilityCard({
  report,
  loading,
}: {
  report: import("../../bindings/github.com/Parsaetak/FreeIran/engine/app/models.js").SourceReliabilityReport | null;
  loading: boolean;
}) {
  if (!report && loading) {
    return (
      <div className="card flush">
        <div className="card-header">
          <h3 className="card-title">Source reliability</h3>
        </div>
        <div className="loading-inline">
          <span className="btn-spinner" aria-hidden /> Measuring source health…
        </div>
      </div>
    );
  }

  if (!report) return null;

  const overall = report.overall;
  const enoughData = overall.success_rate_pct >= 0;
  const lastRefresh = overall.last_refresh_at
    ? new Date(overall.last_refresh_at).toLocaleString()
    : "never";

  return (
    <div className="card flush">
      <div className="card-header">
        <h3 className="card-title">Source reliability</h3>
        <span className="chip mono">{report.sources?.length ?? 0} sources</span>
      </div>

      <div className="card-body reliability-summary">
        <div className="reliability-stat">
          <span className="stat-label">Enabled sources</span>
          <span className="stat-value">
            {overall.enabled_sources}/{overall.total_sources}
          </span>
          <span className="stat-sub">participate in refresh cycles</span>
        </div>

        <div className="reliability-stat">
          <span className="stat-label">Success rate</span>
          <span className="stat-value">
            {enoughData ? `${overall.success_rate_pct}%` : "—"}
          </span>
          <span className="stat-sub">
            {enoughData
              ? `${overall.working_configs} working of ${overall.tested_configs} tested`
              : "Not enough data — test connections first"}
          </span>
        </div>

        <div className="reliability-stat">
          <span className="stat-label">Median latency</span>
          <span className="stat-value">
            {(overall.median_latency_ms ?? 0) > 0 ? formatLatency(overall.median_latency_ms ?? 0) : "—"}
          </span>
          <span className="stat-sub">
            {(overall.median_latency_ms ?? 0) > 0 ? "measured on working routes" : "no measurement yet"}
          </span>
        </div>

        <div className="reliability-stat">
          <span className="stat-label">Configurations</span>
          <span className="stat-value">{overall.persisted_configs}</span>
          <span className="stat-sub">
            {overall.untested_configs} untested · {overall.working_configs} working
          </span>
        </div>

        <div className="reliability-stat">
          <span className="stat-label">Last refresh</span>
          <span className="stat-value" style={{ fontSize: 12 }}>
            {overall.last_refresh_state}
          </span>
          <span className="stat-sub">{lastRefresh}</span>
        </div>
      </div>

      {(report.sources?.length ?? 0) > 0 && (
        <div role="table" aria-label="Per-source reliability">
          <div className="src-health-head" role="row">
            <span role="columnheader">Source</span>
            <span role="columnheader">Persisted</span>
            <span role="columnheader">Working</span>
            <span role="columnheader">Tested</span>
            <span role="columnheader">Success</span>
            <span role="columnheader">Median</span>
            <span role="columnheader">Fetches</span>
          </div>

          {report.sources.map((entry) => (
            <div className="src-health-row" role="row" key={entry.id}>
              <span className="src-name" role="cell" title={entry.id}>
                {truncate(entry.name || entry.id, 34)}
                {!entry.enabled && <span className="cell-sub-inline"> (disabled)</span>}
              </span>
              <span className="src-cell" role="cell">
                {entry.persisted_configs || "—"}
              </span>
              <span className="src-cell" role="cell">
                {entry.working_configs || "—"}
              </span>
              <span className="src-cell" role="cell">
                {entry.tested_configs || "—"}
              </span>
              <span className={`src-cell ${entry.success_rate_pct < 0 ? "na" : ""}`} role="cell">
                {entry.success_rate_pct >= 0 ? `${entry.success_rate_pct}%` : "no data"}
              </span>
              <span className="src-cell" role="cell">
                {(entry.median_latency_ms ?? 0) > 0 ? formatLatency(entry.median_latency_ms ?? 0) : "—"}
              </span>
              <span
                className={`src-cell ${entry.fetch_enough_data ? "" : "na"}`}
                role="cell"
                title={
                  entry.fetch_enough_data
                    ? `${entry.success_count}/${entry.fetch_count} fetches succeeded`
                    : "never fetched"
                }
              >
                {entry.fetch_enough_data ? `${entry.fetch_success_pct}%` : "—"}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
