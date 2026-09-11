import { useState } from "react";
import { useSourcesStore } from "../state/stores";
import { truncate } from "../utilities/format";
import { describeError, toast } from "../state/toastStore";
import { ConfirmDialog, FormDialog } from "../components/Dialog";
import { EmptyState } from "../components/common";
import { IconPlus, IconRefresh, IconSources, IconTrash } from "../components/Icons";

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
        <div>
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
            {refreshing ? <span className="btn-spinner" aria-hidden /> : <IconRefresh size={14} />}
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
