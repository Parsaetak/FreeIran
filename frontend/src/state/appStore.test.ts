import { beforeEach, describe, expect, it, vi } from "vitest";
import { useAppStore } from "./appStore";
import { useQuickConnectStore } from "./quickConnectStore";

/**
 * v0.9.8.7 — the app store is the funnel for "freeiran:state" events.
 * Pinned here: the ingestion FINISHING falling edge is the
 * meaningful-invalidation signal that refreshes the Quick Connect
 * candidate list exactly once (event-driven, never a poll).
 */

const services = vi.hoisted(() => ({
  State: vi.fn(),
}));

vi.mock("../services", () => ({
  call: (operation: () => Promise<unknown>) => operation(),
  appService: {
    State: services.State,
  },
}));

function backendState(overrides: Partial<{ ingestion_running: boolean; status: string }> = {}) {
  return {
    status: overrides.status ?? "ready",
    version: "0.9.8.7",
    identity: "FreeIran",
    started_at: 0,
    config_count: 0,
    ingestion_running: overrides.ingestion_running ?? false,
    native_acceleration: "go",
    storage: {
      status: "",
      count: 0,
      chunk_count: 0,
      total_records: 0,
      dead_records: 0,
      disk_bytes: 0,
      pending_keys: 0,
      garbage_ratio: 0,
      lsn: 0,
    },
    last_ingestion: null,
    boot_phase: "ready",
    boot_timings: {},
  };
}

beforeEach(() => {
  vi.clearAllMocks();

  useAppStore.setState({
    status: "loading",
    backend: null,
    lastError: null,
    connected: false,
    bootPhase: null,
    bootTimings: null,
  });

  useQuickConnectStore.setState({
    candidates: [],
    loading: false,
    loaded: false,
    error: null,
    selected: null,
    stale: false,
  });
});

describe("app store event ingestion", () => {
  it("refreshes quick connect candidates exactly once when ingestion finishes", async () => {
    const qcInvalidate = vi.spyOn(useQuickConnectStore.getState(), "invalidate");

    // Warm the funnel: a first event establishes the "prev" baseline
    // (ingestion running).
    useAppStore.getState().ingestEvent(backendState({ ingestion_running: true }));
    useAppStore.getState().ingestEvent(backendState({ ingestion_running: false }));

    // Falling edge crossed exactly once → exactly one invalidation.
    expect(qcInvalidate).toHaveBeenCalledTimes(1);
  });

  it("does not invalidate on the rising edge or unrelated state flips", () => {
    const qcInvalidate = vi.spyOn(useQuickConnectStore.getState(), "invalidate");

    useAppStore.getState().ingestEvent(backendState({ ingestion_running: true }));
    useAppStore.getState().ingestEvent(backendState({ ingestion_running: true }));

    expect(qcInvalidate).not.toHaveBeenCalled();
  });

  it("keeps reference-stable sub-objects when content is unchanged", () => {
    const first = backendState();
    useAppStore.getState().ingestEvent(first);

    const storedBefore = useAppStore.getState().backend?.storage;

    useAppStore.getState().ingestEvent(backendState());

    expect(useAppStore.getState().backend?.storage).toBe(storedBefore);
  });
});
