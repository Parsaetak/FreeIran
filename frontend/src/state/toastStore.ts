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

/** Extracts a safe, readable message from an unknown failure. */
export function describeError(error: unknown, fallback = "Something went wrong"): string {
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string" && error) return error;

  return fallback;
}
