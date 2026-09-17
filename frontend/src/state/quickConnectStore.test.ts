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
});
