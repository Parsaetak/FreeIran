import { useEffect, useRef, type ReactNode } from "react";
import { IconX } from "./Icons";

interface ConfirmDialogProps {
  open: boolean;
  title: string;
  body: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/** Modal confirm dialog with focus management and Escape handling. */
export function ConfirmDialog({
  open,
  title,
  body,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  danger = false,
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const confirmRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;

    confirmRef.current?.focus();

    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onCancel();
    };

    window.addEventListener("keydown", onKey);

    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div
      className="dialog-overlay"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onCancel();
      }}
    >
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title}>
        <div className="dialog-header">
          <h3 className="dialog-title">{title}</h3>
          <button
            type="button"
            className="icon-btn"
            aria-label="Close dialog"
            onClick={onCancel}
          >
            <IconX size={14} />
          </button>
        </div>

        <div className="dialog-body">{body}</div>

        <div className="dialog-footer">
          <button type="button" className="btn" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </button>

          <button
            type="button"
            ref={confirmRef}
            className={`btn ${danger ? "danger" : "primary"}`}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy && <span className="btn-spinner" aria-hidden />}
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

interface FormDialogProps {
  open: boolean;
  title: string;
  submitLabel?: string;
  busy?: boolean;
  onSubmit: () => void;
  onCancel: () => void;
  children: ReactNode;
}

/** Modal form dialog (add-source etc.). Submit is wired by the caller. */
export function FormDialog({
  open,
  title,
  submitLabel = "Save",
  busy = false,
  onSubmit,
  onCancel,
  children,
}: FormDialogProps) {
  useEffect(() => {
    if (!open) return;

    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onCancel();
    };

    window.addEventListener("keydown", onKey);

    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div
      className="dialog-overlay"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onCancel();
      }}
    >
      <form
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onSubmit={(event) => {
          event.preventDefault();
          onSubmit();
        }}
      >
        <div className="dialog-header">
          <h3 className="dialog-title">{title}</h3>
          <button
            type="button"
            className="icon-btn"
            aria-label="Close dialog"
            onClick={onCancel}
          >
            <IconX size={14} />
          </button>
        </div>

        <div className="dialog-body">{children}</div>

        <div className="dialog-footer">
          <button type="button" className="btn" onClick={onCancel} disabled={busy}>
            Cancel
          </button>

          <button type="submit" className="btn primary" disabled={busy}>
            {busy && <span className="btn-spinner" aria-hidden />}
            {submitLabel}
          </button>
        </div>
      </form>
    </div>
  );
}
