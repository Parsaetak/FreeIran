import { create } from "zustand";
import { call, dataService, type Config } from "../services";
import { useConfigsStore } from "./stores";
import { useQuickConnectStore } from "./quickConnectStore";

/**
 * v0.9.15 — ONE shared live-testing truth for the whole UI.
 *
 * The test queue is the authoritative testing engine; this store is
 * its projection: which fingerprints are queued / preparing / testing
 * / measuring right now, and an incrementally patched view of results.
 *
 * Every page (Configs rows, detail panel, Connection) reads THIS store
 * instead of keeping private testing state, so the whole application
 * agrees about config health while tests run — and a finished test
 * patches its config in place (one GetConfig per changed fingerprint)
 * instead of rebuilding a 16,000-row dataset through a full refresh.
 *
 * CORRECTNESS MODEL (the false-completion fix):
 *
 *   - The backend LiveState contract is the COMPLETENESS authority: it
 *     carries the COMPLETE live fingerprint set plus a monotonic change
 *     version. A fingerprint that was live and is now absent from the
 *     complete set reached a real terminal state — its persisted
 *     result is patched in exactly once.
 *
 *   - A bounded task page (Snapshot(200)) is a DETAIL view of the
 *     priority head only. Absence from a bounded page means NOTHING:
 *     with 1,000 live tasks a watched test may sit outside the top
 *     200 for its whole life, and treating that as completion produced
 *     false completions, premature GetConfig calls and state flicker.
 *     applyQueueSnapshot therefore only merges detail state words and
 *     NEVER decides completion.
 *
 *   - Optimistic marks (requested[]) carry the change version of the
 *     click. They are promoted to "observed" when the live set covers
 *     them; when a strictly newer version still does not cover them,
 *     the task either finished entirely inside the observation gap or
 *     the enqueue never landed — either way the mark is retired
 *     through the same one-shot patch path (idempotent for both).
 *
 *   - DEDUP INVARIANT: the same fingerprint has AT MOST ONE result
 *     patch outstanding or queued. Backlog pushes are marked
 *     scheduled, closing the historical gap that allowed repeated
 *     scheduling before the backlog drained.
 */

/** Live (non-terminal) task states reported by the queue snapshot. */
export type LiveTestState = "queued" | "preparing" | "testing" | "measuring";

interface TestProgressState {
  /** fingerprint → live state (only non-terminal tasks appear). */
  states: Record<string, LiveTestState>;

  /**
   * Monotonic counter bumped after each persisted result was patched
   * into the configs store. Cheap subscription signal for surfaces
   * that need "results changed" (Connection best-candidate refresh)
   * without any polling of their own.
   */
  resultsVersion: number;

  /**
   * Optimistic mark from the click that enqueued a single test — keeps
   * the row's queued chip INSTANT until the live set confirms (or the
   * result patch retires it when the task finished within the gap).
   */
  requested: Record<string, true>;
}

/**
 * Fingerprints the backend's COMPLETE live set currently reports.
 * Rebuilt (not merged) on every authoritative read.
 */
const observedLive = new Set<string>();

/** The queue change version last observed from the backend. */
let lastVersion = 0;

/**
 * Optimistic marks awaiting their first live observation, with the
 * queue version at which the click happened.
 */
const optimistic = new Map<string, number>();

/** Result subscribers (fp, patched config snapshot). */
type ResultListener = (fingerprint: string, config: Config) => void;
const listeners = new Set<ResultListener>();

/** Bounded in-flight patch budget — never pile up GetConfig calls. */
const MAX_INFLIGHT_PATCHES = 64;
let inflightPatches = 0;
const patchBacklog: string[] = [];

/**
 * Fingerprints with a patch operation scheduled or queued (the dedup
 * set — see the DEDUP INVARIANT above).
 */
const patchScheduled = new Set<string>();

export const useTestProgress = create<TestProgressState>(() => ({
  states: {},
  resultsVersion: 0,
  requested: {},
}));

/** Marks a config as user-requested for testing (optimistic, instant). */
export function markTestRequested(fingerprint: string) {
  // The mark anchors at the CURRENT version: any strictly newer
  // authoritative read that does not cover the fingerprint retires it
  // (the task finished within the gap, or the enqueue never landed).
  if (!optimistic.has(fingerprint)) {
    optimistic.set(fingerprint, lastVersion);
  }

  useTestProgress.setState((state) => ({
    requested: { ...state.requested, [fingerprint]: true },
    states: { ...state.states, [fingerprint]: "queued" },
  }));
}

/** Drops an optimistic mark (enqueue failed — nothing will run). */
export function clearTestRequested(fingerprint: string) {
  optimistic.delete(fingerprint);

  useTestProgress.setState((state) => {
    const requested = { ...state.requested };
    delete requested[fingerprint];

    const states = { ...state.states };
    delete states[fingerprint];

    return { requested, states };
  });
}

const LIVE_STATES: ReadonlySet<string> = new Set([
  "queued",
  "preparing",
  "testing",
  "measuring",
]);

/**
 * Feeds ONE complete, authoritative queue-state read (the LiveState
 * contract) into the store. This is the ONLY path that decides
 * completion.
 *
 * @param view    the complete live-state view (version + fingerprints).
 * @param details optional bounded task page supplying per-task state
 *                detail (queued/testing/...) — merged as detail ONLY;
 *                it never contributes completion decisions.
 */
export function applyLiveState(
  view: { version?: number; fingerprints?: string[] },
  details?: Array<{ fingerprint?: string; state?: string }>,
) {
  const version = Number(view?.version ?? 0);
  const versionAdvanced = version > lastVersion;
  lastVersion = version;

  const nextLive = new Set<string>();

  for (const fp of view?.fingerprints ?? []) {
    const key = String(fp ?? "");

    if (key) nextLive.add(key);
  }

  // Terminal transitions: fingerprints the COMPLETE set observed live
  // before and does not cover anymore.
  const finished: string[] = [];

  for (const fp of observedLive) {
    if (!nextLive.has(fp)) {
      finished.push(fp);
    }
  }

  // Optimistic marks: promote when the live set covers them; retire
  // through the patch path when a strictly newer version still does
  // not (finished inside the gap, or the enqueue never landed — the
  // patch is idempotent for both).
  for (const [fp, markedAt] of [...optimistic]) {
    if (nextLive.has(fp)) {
      optimistic.delete(fp);
      observedLive.add(fp);
    } else if (versionAdvanced && markedAt < version) {
      optimistic.delete(fp);
      finished.push(fp);
    }
  }

  // Rebuild the authoritative observed set.
  observedLive.clear();

  for (const fp of nextLive) {
    observedLive.add(fp);
  }

  // Live detail states: the authoritative set decides WHO is live; the
  // bounded page (when provided) decides the finer state words for the
  // tasks it covers. Tasks beyond the page render as queued (honest:
  // they ARE queued — the page is bounded, not the queue).
  const nextStates: Record<string, LiveTestState> = {};

  for (const fp of observedLive) {
    nextStates[fp] = "queued";
  }

  for (const task of details ?? []) {
    const fp = String(task?.fingerprint ?? "");
    const live = String(task?.state ?? "");

    if (!fp || !LIVE_STATES.has(live)) continue;

    if (observedLive.has(fp)) {
      nextStates[fp] = live as LiveTestState;
    }
  }

  useTestProgress.setState((current) => ({
    // Optimistic marks keep their chip until covered or retired.
    states: { ...nextStates, ...optimisticChips(current.requested, nextStates) },
  }));

  for (const fp of finished) {
    scheduleResultPatch(fp);
  }
}

/**
 * Merges a bounded task page as DETAIL only (never completion): detail
 * state words win for the fingerprints the authoritative live set (or
 * an optimistic mark) already knows are live; nothing is removed from
 * the live set here — a task outside a bounded page is NOT finished.
 */
export function applyQueueSnapshot(tasks: Array<{ fingerprint?: string; state?: string }>) {
  const detailStates: Record<string, LiveTestState> = {};

  for (const task of tasks) {
    const fp = String(task?.fingerprint ?? "");
    const live = String(task?.state ?? "");

    if (!fp || !LIVE_STATES.has(live)) continue;

    detailStates[fp] = live as LiveTestState;
  }

  useTestProgress.setState((current) => ({
    states: {
      ...current.states,
      ...detailStates,
      ...optimisticChips(current.requested, { ...current.states, ...detailStates }),
    },
  }));
}

/** Requested marks whose state is not already covered by live detail. */
function optimisticChips(
  requested: Record<string, true>,
  states: Record<string, LiveTestState>,
): Record<string, LiveTestState> {
  const out: Record<string, LiveTestState> = {};

  for (const fp of Object.keys(requested)) {
    if (!states[fp]) out[fp] = "queued";
  }

  return out;
}

/**
 * Coalesced Quick Connect invalidation (v0.9.15): a large batch used to
 * fire one BestCandidates fetch PER persisted result — a backend fetch
 * storm for a 16,000-config run. Results arrive in bursts, so the
 * invalidation is coalesced on a trailing 750 ms window: at most one
 * refresh per burst, always including the newest result (the backend
 * invalidates its ranking snapshot on the same events, so nothing is
 * stale between flushes — only the UI fetch is batched).
 */
const QC_FLUSH_WINDOW_MS = 750;

let qcFlushTimer: number | null = null;
let qcDirty = false;

function invalidateQuickConnectCoalesced() {
  qcDirty = true;

  if (qcFlushTimer !== null) return;

  qcFlushTimer = window.setTimeout(() => {
    qcFlushTimer = null;

    if (!qcDirty) return;

    qcDirty = false;

    useQuickConnectStore.getState().invalidate();
  }, QC_FLUSH_WINDOW_MS);
}

function scheduleResultPatch(fingerprint: string) {
  // DEDUP INVARIANT: at most one patch operation per fingerprint may be
  // scheduled OR queued. The backlog push marks the fingerprint as
  // scheduled too — the historical gap (backlog entries without a
  // scheduled mark) allowed repeated scheduling of the same
  // fingerprint before the backlog drained.
  if (patchScheduled.has(fingerprint)) return;

  patchScheduled.add(fingerprint);

  if (inflightPatches >= MAX_INFLIGHT_PATCHES) {
    // Backlog drains one entry per completed patch (finally below).
    // The explicit bounded queue guarantees nothing is silently
    // dropped; the scheduled mark guarantees no duplicates.
    patchBacklog.push(fingerprint);

    return;
  }

  void patchResult(fingerprint);
}

async function patchResult(fingerprint: string) {
  inflightPatches++;

  try {
    const config = await call(() => dataService.GetConfig(fingerprint));

    if (config) {
      // The ONE authoritative result path: the queue adapter persisted
      // the outcome; here it is reflected into the shared list model
      // (patch in place — no full refresh anywhere).
      useConfigsStore.getState().patchConfig(fingerprint, config);
      invalidateQuickConnectCoalesced();

      useTestProgress.setState((state) => ({
        resultsVersion: state.resultsVersion + 1,
      }));

      for (const listener of listeners) {
        try {
          listener(fingerprint, config);
        } catch {
          /* a broken listener never breaks the patch path */
        }
      }
    }
  } catch {
    /* best-effort: the config may have been deleted while queued */
  } finally {
    inflightPatches--;

    patchScheduled.delete(fingerprint);

    // Clear the optimistic mark: the task reached a terminal state.
    useTestProgress.setState((state) => {
      if (!(fingerprint in state.requested) && !(fingerprint in state.states)) {
        return state;
      }

      const requested = { ...state.requested };
      delete requested[fingerprint];

      const states = { ...state.states };

      // Only clear when no newer live task took over the fingerprint.
      if (states[fingerprint] && !observedLive.has(fingerprint)) {
        delete states[fingerprint];
      }

      return { requested, states };
    });

    const next = patchBacklog.shift();

    if (next) {
      void patchResult(next);
    }
  }
}

/** Subscribes to patched results (fingerprint + fresh config). */
export function onTestResult(listener: ResultListener): () => void {
  listeners.add(listener);

  return () => listeners.delete(listener);
}
