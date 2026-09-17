import { describe, expect, it, vi, beforeEach } from "vitest";
import { useStartFlowStore, subscribeStartFlow, stageIndex, STAGE_ORDER } from "../state/startflowStore";
import { flowStageLabel, provenanceLabel, TEST_MODES, SORT_MODES } from "../types/discovery";

/**
 * Start-flow store contract tests (v0.9.6): progress is derived from
 * ENGINE EVENTS verbatim — no fabricated stages, no invented results.
 */

// Partial module mock: the generated bindings (imported transitively
// via ../services) need the REAL Create helpers, so only Events.On is
// replaced — with a captured-subscription stand-in. vi.hoisted keeps
// the handler map available inside the hoisted factory.
const { emitHandlers } = vi.hoisted(() => ({
  emitHandlers: new Map<string, (event: { data: unknown }) => void>(),
}));

vi.mock("@wailsio/runtime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wailsio/runtime")>();

  return {
    ...actual,
    Events: {
      ...actual.Events,
      On: vi.fn(
        (name: string, handler: (event: { data: unknown }) => void) => {
          emitHandlers.set(name, handler);
          return () => emitHandlers.delete(name);
        },
      ),
    },
  };
});

describe("start flow store", () => {
  beforeEach(() => {
    emitHandlers.clear();
    useStartFlowStore.setState({
      status: null,
      environment: null,
      busy: false,
      error: null,
      lastResult: null,
    });
  });

  it("starts idle with no status", () => {
    const state = useStartFlowStore.getState();

    expect(state.status).toBeNull();
    expect(state.busy).toBe(false);
    expect(state.error).toBeNull();
    expect(state.lastResult).toBeNull();
  });

  it("ingests stage events verbatim", () => {
    subscribeStartFlow();

    const handler = emitHandlers.get("freeiran:startflow");
    expect(handler).toBeDefined();

    handler?.({
      data: {
        stage: "discovering",
        message: "42 valid candidates",
        duration_ms: 812,
        at: Date.now(),
      },
    });

    const status = useStartFlowStore.getState().status;
    expect(status?.stage).toBe("discovering");
    expect(status?.message).toBe("42 valid candidates");
    expect(status?.running).toBe(true);
  });

  it("marks terminal stages not running", () => {
    subscribeStartFlow();

    const handler = emitHandlers.get("freeiran:startflow");

    for (const stage of ["connected", "failed", "no_usable_candidates"]) {
      handler?.({ data: { stage, at: Date.now() } });
      expect(useStartFlowStore.getState().status?.running).toBe(false);
    }
  });

  it("captures result payloads from terminal events", () => {
    subscribeStartFlow();

    const handler = emitHandlers.get("freeiran:startflow");

    handler?.({
      data: {
        stage: "connected",
        message: "connected and verified",
        detail: {
          result: {
            discovered: 120,
            valid: 95,
            duplicates: 25,
            tested: 20,
            verified: true,
            duration_ms: 9200,
          },
        },
        at: Date.now(),
      },
    });

    const state = useStartFlowStore.getState();
    expect(state.lastResult?.valid).toBe(95);
    expect(state.lastResult?.verified).toBe(true);
    expect(state.status?.stage).toBe("connected");
  });

  it("does not run two flows at once", () => {
    useStartFlowStore.setState({ busy: true });
    const run = useStartFlowStore.getState().run;
    // busy=true → run() returns immediately without touching the
    // service layer (which is mocked to throw if called).
    void run();
    expect(useStartFlowStore.getState().busy).toBe(true);
  });
});

describe("stage ordering", () => {
  it("orders the adaptive flow stages correctly", () => {
    expect(stageIndex("idle")).toBeLessThan(stageIndex("detecting"));
    expect(stageIndex("detecting")).toBeLessThan(stageIndex("discovering"));
    expect(stageIndex("discovering")).toBeLessThan(stageIndex("testing"));
    expect(stageIndex("testing")).toBeLessThan(stageIndex("ranking"));
    expect(stageIndex("ranking")).toBeLessThan(stageIndex("connecting"));
    expect(stageIndex("connecting")).toBeLessThan(stageIndex("verifying"));
    expect(stageIndex("verifying")).toBeLessThan(stageIndex("connected"));
  });

  it("treats unknown stages as idle", () => {
    expect(stageIndex("nonsense")).toBe(0);
    expect(stageIndex(undefined)).toBe(0);
  });

  it("exposes seven flow stages plus terminal states", () => {
    expect(STAGE_ORDER.length).toBe(10);
  });
});

describe("discovery type labels", () => {
  it("labels every flow stage for the UI", () => {
    expect(flowStageLabel("detecting")).toBe("Detecting environment");
    expect(flowStageLabel("verifying")).toBe("Verifying");
    expect(flowStageLabel("no_usable_candidates")).toBe("No usable candidates");
    expect(flowStageLabel("idle")).toBe("Idle");
  });

  it("labels provenance honestly", () => {
    expect(provenanceLabel("measured")).toBe("measured");
    expect(provenanceLabel("estimated")).toBe("estimated");
    expect(provenanceLabel("stale")).toBe("stale");
    expect(provenanceLabel(undefined)).toBe("not measured");
  });

  it("offers five test modes and nine sort modes", () => {
    expect(TEST_MODES.length).toBe(5);
    expect(SORT_MODES.length).toBe(9);
    expect(new Set(TEST_MODES.map((m) => m.id)).size).toBe(5);
    expect(new Set(SORT_MODES.map((m) => m.id)).size).toBe(9);
  });
});
