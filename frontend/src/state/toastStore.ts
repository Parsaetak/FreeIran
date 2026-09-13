import { create } from "zustand";

/**
 * Lightweight toast notifications for the outcome of key actions
 * (connect/disconnect, source changes, settings saves, storage
 * maintenance). Never render raw stack traces — BackendError
 * messages are safe, readable one-liners.
 */

export type ToastKind = "success" | "error" | "info";

export interface Toast {
  id: number;
  kind: ToastKind;
  title: string;
  message?: string;
}

interface ToastStore {
  toasts: Toast[];
  push: (kind: ToastKind, title: string, message?: string) => void;
  dismiss: (id: number) => void;
}

let nextId = 1;

export const useToastStore = create<ToastStore>((set) => ({
  toasts: [],

  push: (kind, title, message) =>
    set((state) => ({
      toasts: [...state.toasts, { id: nextId++, kind, title, message }].slice(-5),
    })),

  dismiss: (id) =>
    set((state) => ({ toasts: state.toasts.filter((t) => t.id !== id) })),
}));

/** Convenience helper so pages do not need the hook outside React. */
export function toast(kind: ToastKind, title: string, message?: string): void {
  useToastStore.getState().push(kind, title, message);
}

/** Raw error text for an unknown failure (no fallback applied). */
function rawErrorText(error: unknown, fallback: string): string {
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string" && error) return error;

  return fallback;
}

/**
 * Extracts a safe, readable message from an unknown failure (v0.9.1):
 * the backend's humanized format ("friendly sentence\n---\nTechnical
 * details: raw") is split so users only ever see the friendly part.
 */
export function describeError(error: unknown, fallback = "Something went wrong"): string {
  const raw = rawErrorText(error, fallback);

  const marker = "\n---\nTechnical details: ";
  const idx = raw.indexOf(marker);

  return idx >= 0 ? raw.slice(0, idx) : raw;
}

/**
 * Same as describeError but also returns the raw technical part for
 * expandable detail surfaces (v0.9.1 "what happened + why + what to
 * do next" messaging).
 */
export function describeErrorFull(
  error: unknown,
  fallback = "Something went wrong",
): { readable: string; technical: string } {
  const raw = rawErrorText(error, fallback);

  const marker = "\n---\nTechnical details: ";
  const idx = raw.indexOf(marker);

  if (idx >= 0) {
    return { readable: raw.slice(0, idx), technical: raw.slice(idx + marker.length) };
  }

  return { readable: raw, technical: "" };
}
