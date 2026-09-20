import { beforeEach, describe, expect, it, vi } from "vitest";
import { useQuickConnectStore } from "./quickConnectStore";
import type { CandidateView } from "../services";

/**
 * Quick Connect store contract: loads ONLY the bounded ranked
 * candidate views (never the configuration database), orders them
 * through the pure picker model and tracks the explicit selection.
 */

const services = vi.hoisted(() => ({
  BestCandidates: vi.fn(),
}));

vi.mock("../services", () => ({
  call: (operation: () => Promise<unknown>) => operation(),
  connectionService: {
    BestCandidates: services.BestCandidates,
  },
}));

function view(fingerprint: string, latency: number, samples = 2): CandidateView {
  return {
    fingerprint,
    name: fingerprint,
    protocol: "vless",
    endpoint: `${fingerprint}.example:443`,
    class: "good",
    score: 0.5,
    latency_ms: latency,
    success_rate: 1,
    samples,
    tested_at: Date.now() - 1000,
    connectable: true,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  useQuickConnectStore.setState({
    candidates: [],
    loading: false,
    loaded: false,
    error: null,
    selected: null,
    stale: false,
  });
});

describe("quick connect store", () => {
  it("loads and orders candidates by measured ping", async () => {
    services.BestCandidates.mockResolvedValue([
      view("slow", 200),
      view("fast", 80),
      { ...view("dead", 10), class: "dead", connectable: false },
    ]);

    await useQuickConnectStore.getState().load();

    const state = useQuickConnectStore.getState();

    expect(services.BestCandidates).toHaveBeenCalledWith(100);
    expect(state.loaded).toBe(true);
    expect(state.error).toBeNull();
    expect(state.candidates.map((v) => v.fingerprint)).toEqual(["fast", "slow"]);
  });

  it("does not refetch once loaded unless forced", async () => {
    services.BestCandidates.mockResolvedValue([view("only", 100)]);

    await useQuickConnectStore.getState().load();
    await useQuickConnectStore.getState().load();

    expect(services.BestCandidates).toHaveBeenCalledTimes(1);

    await useQuickConnectStore.getState().load(true);

    expect(services.BestCandidates).toHaveBeenCalledTimes(2);
  });

  it("keeps the page usable when ranking fails", async () => {
    services.BestCandidates.mockRejectedValue(new Error("ranking unavailable"));

    await useQuickConnectStore.getState().load();

    const state = useQuickConnectStore.getState();

    expect(state.loaded).toBe(true);
    expect(state.candidates).toEqual([]);
    expect(state.error).toBe("ranking unavailable");
  });

  it("tracks selection and clears it explicitly", () => {
    useQuickConnectStore.getState().select("fp1");
    expect(useQuickConnectStore.getState().selected).toBe("fp1");

    // null = Auto (engine best selection).
    useQuickConnectStore.getState().select(null);
    expect(useQuickConnectStore.getState().selected).toBeNull();
  });

  // v0.9.8.7 — meaningful-invalidation refresh: invalidate() refreshes
  // exactly once per real ranking-input change, never polls.
  it("invalidate triggers exactly one bounded refresh after a load", async () => {
    services.BestCandidates.mockResolvedValue([view("a", 10)]);

    await useQuickConnectStore.getState().load();
    expect(services.BestCandidates).toHaveBeenCalledTimes(1);

    // A single invalidation → exactly one bounded refresh.
    useQuickConnectStore.getState().invalidate();

    await vi.waitFor(() => {
      expect(useQuickConnectStore.getState().loading).toBe(false);
      expect(useQuickConnectStore.getState().stale).toBe(false);
    });

    expect(services.BestCandidates).toHaveBeenCalledTimes(2);

    // A burst of invalidations converges to a bounded number of
    // refreshes and then stops growing entirely (quiescence).
    useQuickConnectStore.getState().invalidate();
    useQuickConnectStore.getState().invalidate();
    useQuickConnectStore.getState().invalidate();

    await vi.waitFor(() => {
      expect(useQuickConnectStore.getState().loading).toBe(false);
      expect(useQuickConnectStore.getState().stale).toBe(false);
    });

    await new Promise((resolve) => setTimeout(resolve, 20));

    const settled = services.BestCandidates.mock.calls.length;

    expect(settled).toBeGreaterThanOrEqual(3);
    expect(settled).toBeLessThanOrEqual(5); // bounded: initial + invalidations + drains

    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(services.BestCandidates).toHaveBeenCalledTimes(settled);
  });

  it("invalidate is a no-op before the first load", () => {
    // loaded=false: nothing to invalidate and no speculative fetch.
    useQuickConnectStore.getState().invalidate();

    expect(services.BestCandidates).not.toHaveBeenCalled();
    expect(useQuickConnectStore.getState().loaded).toBe(false);
  });

  it("invalidate during an in-flight load drains exactly one follow-up refresh", async () => {
    services.BestCandidates.mockImplementation(
      () => new Promise((resolve) => setTimeout(() => resolve([view("a", 10)]), 10)),
    );

    const loading = useQuickConnectStore.getState().load(true);
    expect(useQuickConnectStore.getState().loading).toBe(true);

    // A ranking-input change lands while the refresh runs: it must be
    // REMEMBERED (never dropped) and drain as one follow-up.
    useQuickConnectStore.getState().invalidate();
    expect(useQuickConnectStore.getState().stale).toBe(true);

    await loading;

    await vi.waitFor(() => {
      expect(useQuickConnectStore.getState().stale).toBe(false);
      expect(services.BestCandidates).toHaveBeenCalledTimes(2);
    });
  });
});
