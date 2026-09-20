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
 *
 * v0.9.8.7 — meaningful-invalidation refresh: candidates refresh when
 * a ranking INPUT actually changes (ingestion finished, a test result
 * persisted, core availability changed) — one bounded refresh per
 * event, never a poll, never a per-render refetch. An invalidation
 * arriving while a refresh is in flight is remembered (`stale`) and
 * drains as exactly one follow-up refresh, so a real change can never
 * be silently dropped by an unlucky timing race.
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
  /**
   * v0.9.8.7: an invalidation was absorbed while a refresh was in
   * flight; the owning load drains it as one follow-up refresh.
   */
  stale: boolean;

  load: (force?: boolean) => Promise<void>;
  select: (fingerprint: string | null) => void;
  invalidate: () => void;
}

export const useQuickConnectStore = create<QuickConnectStore>((set, get) => ({
  candidates: [],
  loading: false,
  loaded: false,
  error: null,
  selected: null,
  stale: false,

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

    // Drain an invalidation that arrived while this refresh ran:
    // exactly one follow-up, preserving the never-dropped guarantee
    // without ever stacking concurrent fetches.
    if (get().stale) {
      set({ stale: false });

      void get().load(true);
    }
  },

  select: (fingerprint) => set({ selected: fingerprint }),

  invalidate: () => {
    // An in-flight refresh started BEFORE this change: mark stale so
    // the owning load runs exactly one follow-up (valid even during
    // the very first load — a real change must never be dropped).
    if (get().loading) {
      set({ stale: true });

      return;
    }

    if (!get().loaded) return;

    // One bounded refresh per invalidation. The server-side ranking
    // snapshot was already invalidated by the same event on the
    // backend, so this call rebuilds from current data.
    void get().load(true);
  },
}));
