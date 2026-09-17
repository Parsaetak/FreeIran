import { create } from "zustand";
import { call, connectionService, type CandidateView } from "../services";
import { orderForQuickConnect } from "../utilities/quickConnectModel";

/**
 * v0.9.8 Quick Connect data store.
 *
 * Deliberately tiny: Quick Connect never loads the configuration
 * database (§29) — it fetches ONLY the backend's ranked candidate
 * views (ConnectionService.BestCandidates, credential-free and
 * TTL-cached server-side), orders them through the pure picker model
 * and tracks the user's explicit selection. Connection actions stay
 * in useConnectionStore; the backend remains the source of truth.
 *
 * `selected === null` means "Auto" — the engine's best-candidate
 * selection decides (the default path).
 */

/** Hard bound mirroring the backend clamp (BestCandidates ≤ 100). */
const MAX_CANDIDATES = 100;

interface QuickConnectStore {
  /** Ordered candidates (verified-by-measured-ping first). */
  candidates: CandidateView[];
  loading: boolean;
  /** At least one completed load (distinguishes "empty" from "not loaded"). */
  loaded: boolean;
  error: string | null;
  /** Explicitly selected fingerprint; null = Auto (best candidate). */
  selected: string | null;

  load: (force?: boolean) => Promise<void>;
  select: (fingerprint: string | null) => void;
}

export const useQuickConnectStore = create<QuickConnectStore>((set, get) => ({
  candidates: [],
  loading: false,
  loaded: false,
  error: null,
  selected: null,

  load: async (force = false) => {
    if (get().loading || (get().loaded && !force)) return;

    set({ loading: true });

    try {
      const views = await call(() => connectionService.BestCandidates(MAX_CANDIDATES));

      set({
        candidates: orderForQuickConnect(views ?? []),
        loading: false,
        loaded: true,
        error: null,
      });
    } catch (error) {
      // Ranking is best-effort for Quick Connect: the page stays
      // usable (Auto path) even when the ranking pass is unavailable.
      set({
        loading: false,
        loaded: true,
        error: error instanceof Error ? error.message : String(error),
      });
    }
  },

  select: (fingerprint) => set({ selected: fingerprint }),
}));
