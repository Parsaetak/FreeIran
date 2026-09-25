import { useCallback, useRef, useState } from "react";
import { IconX } from "./Icons";
import { importService, call } from "../services";
import { describeError, toast } from "../state/toastStore";
import type {
  ImportedConfigView,
  ImportPreview,
} from "../../bindings/github.com/Parsaetak/FreeIran/engine/app/importtypes.js";

interface ImportDialogProps {
  open: boolean;
  onClose: () => void;
  onSaved: () => void;
}

type Phase = "input" | "preview" | "saving";

/**
 * ImportDialog implements the v0.10.2 personal-configuration import
 * flow: paste or pick a file → parse → redacted capability preview →
 * save. Connecting afterwards targets the exact imported config IDs —
 * no hidden substitution.
 */
export function ImportDialog({ open, onClose, onSaved }: ImportDialogProps) {
  const [phase, setPhase] = useState<Phase>("input");
  const [payload, setPayload] = useState("");
  const [preview, setPreview] = useState<ImportPreview | null>(null);
  const [error, setError] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const reset = useCallback(() => {
    setPhase("input");
    setPayload("");
    setPreview(null);
    setError("");
  }, []);

  const close = useCallback(() => {
    onClose();
    // keep state so a re-open preserves the draft until reset
  }, [onClose]);

  const readPreview = useCallback(
    async (text: string) => {
      setError("");

      try {
        const result = await call(() => importService.PreviewImport(text));

        setPreview(result);
        setPhase("preview");
      } catch (e) {
        setError(describeError(e, "the payload could not be parsed"));
      }
    },
    [],
  );

  const onPickFile = useCallback(
    async (file: File | undefined) => {
      if (!file) return;

      const text = await file.text();
      setPayload(text);
      await readPreview(text);
    },
    [readPreview],
  );

  const save = useCallback(async () => {
    if (!payload) return;

    setPhase("saving");

    try {
      const result = await call(() => importService.SaveImportedConfigs(payload));

      toast(
        "success",
        "Configurations imported",
        `${result.saved_count} saved (${result.updated_count} updated), ${result.executable_ids?.length ?? 0} executable by an installed core`,
      );

      reset();
      onSaved();
      close();
    } catch (e) {
      setError(describeError(e, "the import could not be saved"));
      setPhase("preview");
    }
  }, [payload, onSaved, close, reset]);

  if (!open) return null;

  const executableCount =
    preview?.imported.filter((row) => row.executable).length ?? 0;

  return (
    <div
      className="dialog-overlay"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) close();
      }}
    >
      <div
        className="dialog dialog-wide"
        role="dialog"
        aria-modal="true"
        aria-label="Import configurations"
      >
        <div className="dialog-header">
          <h3 className="dialog-title">Import configurations</h3>
          <button
            type="button"
            className="icon-btn"
            aria-label="Close dialog"
            onClick={close}
          >
            <IconX />
          </button>
        </div>

        <div className="dialog-body import-body">
          {phase === "input" && (
            <>
              <p className="hint">
                Paste a share link, a list of links, a base64 subscription or a
                WireGuard configuration file. Nothing is saved until you review
                the preview.
              </p>
              <textarea
                className="import-textarea"
                aria-label="Configuration payload"
                spellCheck={false}
                placeholder={"vless://…\nss://…\ntrojan://…\nss:// (base64 subscription)\n[Interface] …"}
                value={payload}
                onChange={(event) => setPayload(event.target.value)}
                rows={10}
              />
              <div className="import-actions">
                <button
                  type="button"
                  className="btn sm"
                  onClick={() => fileRef.current?.click()}
                >
                  Choose file…
                </button>
                <input
                  ref={fileRef}
                  type="file"
                  accept=".txt,.conf,.ini,.json,text/*"
                  hidden
                  onChange={(event) => void onPickFile(event.target.files?.[0])}
                />
                <button
                  type="button"
                  className="btn sm primary"
                  disabled={!payload.trim()}
                  onClick={() => void readPreview(payload)}
                >
                  Preview
                </button>
              </div>
              {error && <p className="import-error">{error}</p>}
            </>
          )}

          {phase === "preview" && preview && (
            <>
              <p className="hint">
                Detected format: <strong>{preview.format}</strong> ·{" "}
                {preview.imported.length} parsed · {executableCount} executable
                by an installed core
                {preview.rejected.length > 0 &&
                  ` · ${preview.rejected.length} rejected`}
              </p>

              <div className="import-table" role="table" aria-label="Import preview">
                <div className="import-row import-head" role="row">
                  <span role="columnheader">#</span>
                  <span role="columnheader">Name</span>
                  <span role="columnheader">Protocol</span>
                  <span role="columnheader">Server</span>
                  <span role="columnheader">Status</span>
                </div>
                {preview.imported.map((row: ImportedConfigView) => (
                  <div className="import-row" role="row" key={`${row.index}-${row.config_id}`}>
                    <span role="cell">{row.index + 1}</span>
                    <span role="cell" title={row.redacted}>{row.name || row.redacted}</span>
                    <span role="cell">{row.protocol}</span>
                    <span role="cell">{row.address}:{row.port}</span>
                    <span role="cell">
                      {row.executable ? (
                        <span className="badge ok">executable</span>
                      ) : (
                        <span
                          className="badge warn"
                          title={row.backends?.length ? "declared but rejected by validation" : "no installed core supports it"}
                        >
                          not runnable
                        </span>
                      )}
                      {row.warnings?.map((warning) => (
                        <span className="import-warning" key={warning} title={warning}>
                          {" "}⚠
                        </span>
                      ))}
                    </span>
                  </div>
                ))}
              </div>

              {preview.rejected.length > 0 && (
                <ul className="import-rejections">
                  {preview.rejected.map((entry, index) => (
                    <li key={index}>{entry.reason}</li>
                  ))}
                </ul>
              )}

              {error && <p className="import-error">{error}</p>}

              <div className="import-actions">
                <button
                  type="button"
                  className="btn sm"
                  onClick={() => setPhase("input")}
                >
                  Edit payload
                </button>
                <button
                  type="button"
                  className="btn sm"
                  onClick={() => void save()}
                >
                  Save {preview.imported.length}
                </button>
              </div>
            </>
          )}

          {phase === "saving" && <p className="hint">Saving…</p>}
        </div>
      </div>
    </div>
  );
}
