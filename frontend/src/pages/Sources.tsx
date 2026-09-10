import { useState } from "react";
import { useSourcesStore } from "../state/stores";
import { truncate } from "../utilities/format";

export function SourcesPage() {
  const sources = useSourcesStore((state) => state.sources);
  const loading = useSourcesStore((state) => state.loading);
  const refreshing = useSourcesStore((state) => state.refreshing);
  const lastError = useSourcesStore((state) => state.lastError);

  const setEnabled = useSourcesStore((state) => state.setEnabled);
  const add = useSourcesStore((state) => state.add);
  const remove = useSourcesStore((state) => state.remove);
  const refresh = useSourcesStore((state) => state.refresh);

  const [formId, setFormId] = useState("");
  const [formName, setFormName] = useState("");
  const [formUrl, setFormUrl] = useState("");
  const [formError, setFormError] = useState<string | null>(null);

  const submit = async () => {
    setFormError(null);

    try {
      await add(formId.trim(), formName.trim(), formUrl.trim());

      setFormId("");
      setFormName("");
      setFormUrl("");
    } catch (error) {
      setFormError(error instanceof Error ? error.message : String(error));
    }
  };

  return (
    <div>
      <div className="toolbar">
        <h2 style={{ margin: 0, flex: 1 }}>Sources</h2>

        <button
          className="btn primary"
          onClick={() => void refresh()}
          disabled={refreshing}
        >
          {refreshing ? "Refreshing…" : "Refresh now"}
        </button>
      </div>

      {lastError && <div className="error-banner">{lastError}</div>}

      <div className="card">
        <h3 className="card-title">Configured sources</h3>

        {loading ? (
          <div className="loading-overlay">
            <div className="spinner" /> Loading…
          </div>
        ) : (
          sources.map((source) => (
            <div
              key={source.id}
              className="config-row"
              style={{ gridTemplateColumns: "40px 1fr 220px 90px" }}
            >
              <button
                className={`toggle ${source.enabled ? "on" : ""}`}
                title={source.enabled ? "Disable" : "Enable"}
                onClick={() => void setEnabled(source.id, !source.enabled)}
              />
              <div>
                <div>{source.name || source.id}</div>
                <div style={{ color: "var(--text-dim)", fontSize: 11 }}>
                  {truncate(source.url, 72)}
                </div>
              </div>
              <div style={{ color: "var(--text-dim)", fontFamily: "var(--mono)", fontSize: 11 }}>
                {source.last_hash ? `hash ${source.last_hash.slice(0, 8)}` : "not fetched"}
              </div>
              <button
                className="btn danger"
                onClick={() => void remove(source.id)}
              >
                Remove
              </button>
            </div>
          ))
        )}
      </div>

      <div className="card">
        <h3 className="card-title">Add source</h3>

        {formError && <div className="error-banner">{formError}</div>}

        <div className="toolbar">
          <input
            className="input"
            placeholder="id (e.g. my-source)"
            value={formId}
            onChange={(event) => setFormId(event.target.value)}
            style={{ width: 160 }}
          />
          <input
            className="input"
            placeholder="display name"
            value={formName}
            onChange={(event) => setFormName(event.target.value)}
            style={{ width: 200 }}
          />
          <input
            className="input"
            placeholder="https://…"
            value={formUrl}
            onChange={(event) => setFormUrl(event.target.value)}
          />
          <button
            className="btn primary"
            disabled={!formId.trim() || !formUrl.trim()}
            onClick={() => void submit()}
          >
            Add
          </button>
        </div>
      </div>
    </div>
  );
}
