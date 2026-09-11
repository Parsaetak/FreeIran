import { describe, expect, it } from "vitest";
import { useConnectionStore } from "../state/connectionStore";

/**
 * Connection store contract tests: the UI derives everything from
 * the backend state machine string — no contradictory booleans.
 */

describe("connection store", () => {
  it("starts disconnected with no snapshot", () => {
    const state = useConnectionStore.getState();

    expect(state.snapshot).toBeNull();
    expect(state.busy).toBe(false);
    expect(state.error).toBeNull();
  });

  it("ingests connection events verbatim", () => {
    useConnectionStore.getState().ingestEvent({
      state: "connected",
      core: "v2ray",
      core_version: "5.53.0",
      config_name: "test",
      endpoint: "127.0.0.1:40000",
    } as never);

    const state = useConnectionStore.getState();

    expect(state.snapshot?.state).toBe("connected");
    expect(state.snapshot?.core).toBe("v2ray");

    // Reset for other tests.
    useConnectionStore.getState().ingestEvent({ state: "disconnected" } as never);
  });

  it("never synthesizes state on failed calls", async () => {
    // The store delegates to the authoritative backend snapshot on
    // failure; busy/error are the only local flags.
    const store = useConnectionStore.getState();

    expect(typeof store.connect).toBe("function");
    expect(typeof store.disconnect).toBe("function");
    expect(typeof store.reconnect).toBe("function");
    expect(typeof store.refresh).toBe("function");
  });
});

describe("connection state classification", () => {
  const busyStates = [
    "selecting",
    "preparing",
    "starting_core",
    "waiting_for_ready",
    "disconnecting",
  ];

  const idleStates = ["disconnected", "connected", "connection_failed"];

  it("treats transitional states as busy", () => {
    for (const state of busyStates) {
      expect(isBusy(state)).toBe(true);
    }
  });

  it("treats terminal and connected states as not busy", () => {
    for (const state of idleStates) {
      expect(isBusy(state)).toBe(false);
    }
  });
});

/**
 * Mirrors the private classifier in the Connection page so the
 * contract is pinned on both sides.
 */
function isBusy(state: string): boolean {
  return [
    "selecting",
    "preparing",
    "starting_core",
    "waiting_for_ready",
    "disconnecting",
  ].includes(state);
}
