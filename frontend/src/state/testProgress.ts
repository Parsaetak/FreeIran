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
   * the row's queued chip INSTANT until the first snapshot confirms
   * (or the result patch clears it when the task finished within the
   * polling gap).
   */
  requested: Record<string, true>;
}

/** Fingerprints observed live at least once; diffed per tick to detect completion. */
const watched = new Set<string>();

/** Result subscribers (fp, patched config snapshot). */
type ResultListener = (fingerprint: string, config: Config) => void;
const listeners = new Set<ResultListener>();

/** Bounded in-flight patch budget — never pile up GetConfig calls. */
const MAX_INFLIGHT_PATCHES = 64;
let inflightPatches = 0;
const patchBacklog: string[] = [];

/** Fingerprints currently patched (dedup across ticks). */
const patchScheduled = new Set<string>();

export const useTestProgress = create<TestProgressState>(() => ({
  states: {},
  resultsVersion: 0,
  requested: {},
}));

/** Marks a config as user-requested for testing (optimistic, instant). */
export function markTestRequested(fingerprint: string) {
  watched.add(fingerprint);

  useTestProgress.setState((state) => ({
    requested: { ...state.requested, [fingerprint]: true },
    states: { ...state.states, [fingerprint]: "queued" },
  }));
}

/** Drops an optimistic mark (enqueue failed — nothing will run). */
export function clearTestRequested(fingerprint: string) {
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
 * Feeds one queue snapshot (pending + in-flight tasks) into the store.
 * Fingerprints that LEFT the live set reached a terminal state: their
 * persisted result is patched into the config model incrementally.
 */
export function applyQueueSnapshot(tasks: Array<{ fingerprint?: string; state?: string }>) {
  const nextStates: Record<string, LiveTestState> = {};

  for (const task of tasks) {
    const fp = String(task?.fingerprint ?? "");
    const state = String(task?.state ?? "");

    if (!fp || !LIVE_STATES.has(state)) continue;

    nextStates[fp] = state as LiveTestState;
    watched.add(fp);
  }

  // Completed since the previous tick: live before, absent now.
  const finished: string[] = [];

  for (const fp of [...watched]) {
    if (!(fp in nextStates)) {
      watched.delete(fp);
      finished.push(fp);
    }
  }

  useTestProgress.setState((state) => ({
    // Merge: snapshot states win; optimistic marks for tasks the poll
    // has not observed yet stay visible (instant UI, no flicker).
    states: { ...state.states, ...nextStates },
  }));

  for (const fp of finished) {
    scheduleResultPatch(fp);
  }
}

function scheduleResultPatch(fingerprint: string) {
  if (patchScheduled.has(fingerprint)) return;

  if (inflightPatches >= MAX_INFLIGHT_PATCHES) {
    // Backlog drains on the following ticks (the poll loop re-observes
    // terminal transitions through `watched`... they already left it).
    // Keep an explicit bounded queue so nothing is silently dropped.
    patchBacklog.push(fingerprint);

    return;
  }

  patchScheduled.add(fingerprint);
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
      useQuickConnectStore.getState().invalidate();

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
      if (states[fingerprint] && !watched.has(fingerprint)) {
        delete states[fingerprint];
      }

      return { requested, states };
    });

    const next = patchBacklog.shift();

    if (next) {
      scheduleResultPatch(next);
    }
  }
}

/** Subscribes to patched results (fingerprint + fresh config). */
export function onTestResult(listener: ResultListener): () => void {
  listeners.add(listener);

  return () => listeners.delete(listener);
}
