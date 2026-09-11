import { useEffect } from "react";
import { useToastStore, type Toast as ToastModel } from "../state/toastStore";
import { IconAlert, IconCheck, IconInfo, IconX } from "./Icons";

const TOAST_TTL_MS = 4000;

/** Bottom-right toast stack with auto-dismiss. */
export function Toasts() {
  const toasts = useToastStore((state) => state.toasts);
  const dismiss = useToastStore((state) => state.dismiss);

  if (toasts.length === 0) return null;

  return (
    <div className="toast-viewport" role="status" aria-live="polite">
      {toasts.map((item) => (
        <ToastItem key={item.id} toast={item} onDismiss={dismiss} />
      ))}
    </div>
  );
}

function ToastItem({
  toast,
  onDismiss,
}: {
  toast: ToastModel;
  onDismiss: (id: number) => void;
}) {
  useEffect(() => {
    const timer = window.setTimeout(() => onDismiss(toast.id), TOAST_TTL_MS);

    return () => window.clearTimeout(timer);
  }, [toast.id, onDismiss]);

  return (
    <div className={`toast ${toast.kind}`}>
      <span className="toast-icon" aria-hidden>
        {toast.kind === "success" ? (
          <IconCheck size={15} />
        ) : toast.kind === "error" ? (
          <IconAlert size={15} />
        ) : (
          <IconInfo size={15} />
        )}
      </span>

      <div className="toast-content">
        <div className="toast-title">{toast.title}</div>
        {toast.message && <div className="toast-message">{toast.message}</div>}
      </div>

      <button
        type="button"
        className="toast-close"
        aria-label="Dismiss notification"
        onClick={() => onDismiss(toast.id)}
      >
        <IconX size={12} />
      </button>
    </div>
  );
}
