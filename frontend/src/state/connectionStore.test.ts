import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "@testing-library/react";
import { useConnectionStore } from "../state/connectionStore";
import type { ConnectionSnapshot } from "../services";

/**
 * @vitest-environment jsdom
 *
 * Connection store contract tests: the UI derives everything from
 * the backend state machine string — no contradictory booleans.
 *
 * v0.9.8.4: stale-operation protection — operation A starts, operation
 * B starts, A resolves after B ⇒ A cannot overwrite B. The generation
 * guard covers connect, connectBest, refresh and the error path, and
 * authoritative events always outrank unresolved promises.
 *
 * The interleavings below are the REAL ones the store must defend
 * against: refresh()/refreshBackends() carry no busy guard, events
 * arrive while operations are pending, and a between-test reset
 * (documented resetStores pattern) clears busy while an operation
 * from the previous case is still in flight.
 */

const mocks = vi.hoisted(() => ({
  ConnectionState: vi.fn(),
  RefreshBackends: vi.fn(),
  Connect: vi.fn(),
  ConnectBest: vi.fn(),
  Disconnect: vi.fn(),
  Reconnect: vi.fn(),
  On: vi.fn(() => () => undefined),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  connectionService: {
    ConnectionState: mocks.ConnectionState,
    RefreshBackends: mocks.RefreshBackends,
    Connect: mocks.Connect,
    ConnectBest: mocks.ConnectBest,
    Disconnect: mocks.Disconnect,
    Reconnect: mocks.Reconnect,
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: { On: mocks.On, Emit: vi.fn() },
}));

/** Deferred promise: the test decides when each operation resolves. */
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;

  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });

  return { promise, resolve, reject };
}

function snapshot(state: string, extra: Record<string, unknown> = {}): ConnectionSnapshot {
  return { state, ...extra } as unknown as ConnectionSnapshot;
}

function reset() {
  act(() => {
    useConnectionStore.setState({
      snapshot: null,
      backends: [],
      health: null,
      busy: false,
      error: null,
    });
    useConnectionStore.getState().invalidatePendingOperations();
  });
}

beforeEach(() => {
  // resetAllMocks (not clearAllMocks): an unconsumed
  // mockImplementationOnce from one case must never leak into the next.
  vi.resetAllMocks();
  mocks.On.mockImplementation(() => () => undefined);
  reset();
});

afterEach(() => {
  // Never leak an armed operation into the next test.
  useConnectionStore.getState().invalidatePendingOperations();
});

describe("connection store", () => {
  it("starts disconnected with no snapshot", () => {
    const state = useConnectionStore.getState();

    expect(state.snapshot).toBeNull();
    expect(state.busy).toBe(false);
    expect(state.error).toBeNull();
  });

  it("ingests connection events verbatim", () => {
    act(() => {
      useConnectionStore.getState().ingestEvent(
        snapshot("connected", {
          core: "v2ray",
          core_version: "5.53.0",
          config_name: "test",
          endpoint: "127.0.0.1:40000",
        }),
      );
    });

    const state = useConnectionStore.getState();

    expect(state.snapshot?.state).toBe("connected");
    expect(state.snapshot?.core).toBe("v2ray");

    act(() => {
      useConnectionStore.getState().ingestEvent(snapshot("disconnected"));
    });
  });

  it("exposes every v0.9.8.3 state machine state in its contract", () => {
    // The authoritative backend states (engine/connection). The store
    // mirrors them 1:1 — connected_verified is the ONLY final success.
    const states = [
      "disconnected",
      "selecting",
      "preparing",
      "starting_core",
      "waiting_for_ready",
      "connected",
      "verifying",
      "connected_verified",
      "disconnecting",
      "connection_failed",
    ];

    for (const state of states) {
      act(() => {
        useConnectionStore.getState().ingestEvent(snapshot(state));
      });

      expect(useConnectionStore.getState().snapshot?.state).toBe(state);
    }
  });

  it("never synthesizes state on failed calls", () => {
    // The store delegates to the authoritative backend snapshot on
    // failure; busy/error are the only local flags.
    const store = useConnectionStore.getState();

    expect(typeof store.connect).toBe("function");
    expect(typeof store.disconnect).toBe("function");
    expect(typeof store.reconnect).toBe("function");
    expect(typeof store.refresh).toBe("function");
    expect(typeof store.invalidatePendingOperations).toBe("function");
  });
});

describe("stale-operation protection (v0.9.8.4)", () => {
  it("a late connect() result cannot overwrite the newer state", async () => {
    const slow = deferred<ConnectionSnapshot>();

    mocks.Connect.mockImplementationOnce(() => slow.promise);

    // Operation A starts…
    const first = useConnectionStore.getState().connect("cfg-a");

    // …the documented between-test reset clears busy while A is still
    // in flight (the exact outstanding-promise leak the guard exists
    // for)… then operation B starts.
    act(() => {
      useConnectionStore.setState({ busy: false });
    });

    mocks.Connect.mockResolvedValueOnce(
      snapshot("connected_verified", { config_id: "cfg-b" }),
    );

    await act(async () => {
      await useConnectionStore.getState().connect("cfg-b");
    });

    expect(useConnectionStore.getState().snapshot?.config_id).toBe("cfg-b");
    expect(useConnectionStore.getState().busy).toBe(false);

    // …and A resolves AFTER B: it must not overwrite the newer state.
    slow.resolve(snapshot("connected_verified", { config_id: "cfg-a" }));
    await act(async () => {
      await first;
    });

    expect(useConnectionStore.getState().snapshot?.config_id).toBe("cfg-b");
    expect(useConnectionStore.getState().error).toBeNull();
  });

  it("a late connectBest() result cannot overwrite the newer state", async () => {
    const slowBest = deferred<never>();

    mocks.ConnectBest.mockImplementationOnce(() => slowBest.promise);

    const viaBest = useConnectionStore.getState().connectBest();

    act(() => {
      useConnectionStore.setState({ busy: false });
    });

    mocks.Connect.mockResolvedValueOnce(
      snapshot("connected_verified", { config_id: "cfg-b" }),
    );

    await act(async () => {
      await useConnectionStore.getState().connect("cfg-b");
    });

    slowBest.resolve({
      snapshot: snapshot("connected_verified", { config_id: "cfg-best" }),
      chosen: {},
      candidates: 1,
    } as never);

    const result = await act(async () => await viaBest);

    // The stale operation yields nothing to its caller and must NOT
    // be applied to the store.
    expect(result).toBeNull();
    expect(useConnectionStore.getState().snapshot?.config_id).toBe("cfg-b");
  });

  it("a late refresh() result cannot overwrite a newer state", async () => {
    const slowRefresh = deferred<ConnectionSnapshot>();

    mocks.ConnectionState.mockImplementationOnce(() => slowRefresh.promise);

    const pending = useConnectionStore.getState().refresh();

    // A newer authoritative transition lands while the read is pending.
    act(() => {
      useConnectionStore.getState().ingestEvent(
        snapshot("connected_verified", { config_id: "live" }),
      );
    });

    slowRefresh.resolve(snapshot("disconnected"));
    await act(async () => {
      await pending;
    });

    // The stale refresh must not report the session disconnected.
    expect(useConnectionStore.getState().snapshot?.state).toBe("connected_verified");
    expect(useConnectionStore.getState().snapshot?.config_id).toBe("live");
  });

  it("a late refresh() result cannot overwrite a newer connect()", async () => {
    const slowRefresh = deferred<ConnectionSnapshot>();

    mocks.ConnectionState.mockImplementationOnce(() => slowRefresh.promise);

    const pending = useConnectionStore.getState().refresh();

    mocks.Connect.mockResolvedValueOnce(
      snapshot("connected_verified", { config_id: "cfg-b" }),
    );

    await act(async () => {
      await useConnectionStore.getState().connect("cfg-b");
    });

    slowRefresh.resolve(snapshot("selecting"));
    await act(async () => {
      await pending;
    });

    expect(useConnectionStore.getState().snapshot?.state).toBe("connected_verified");
  });

  it("a stale error cannot replace the current state", async () => {
    const failing = deferred<ConnectionSnapshot>();

    mocks.Connect.mockImplementationOnce(() => failing.promise);

    const first = useConnectionStore.getState().connect("cfg-dead");

    // A newer operation completes while A is still pending…
    mocks.ConnectionState.mockResolvedValueOnce(
      snapshot("connected_verified", { config_id: "cfg-live" }),
    );

    await act(async () => {
      await useConnectionStore.getState().refresh();
    });

    // …then A fails: neither its error nor its authoritative-failed
    // snapshot may replace the newer state.
    failing.reject(new Error("stale failure"));
    await act(async () => {
      await first;
    });

    expect(useConnectionStore.getState().error).toBeNull();
    expect(useConnectionStore.getState().snapshot?.state).toBe("connected_verified");
    expect(useConnectionStore.getState().snapshot?.config_id).toBe("cfg-live");
  });

  it("an in-generation failure still applies the authoritative snapshot and error", async () => {
    mocks.Connect.mockImplementationOnce(async () => {
      throw new Error("dial failed");
    });
    mocks.ConnectionState.mockResolvedValueOnce(
      snapshot("connection_failed", { last_error: "dial failed" }),
    );

    await act(async () => {
      await useConnectionStore.getState().connect("cfg-broken");
    });

    const state = useConnectionStore.getState();

    expect(state.error).toBe("dial failed");
    expect(state.snapshot?.state).toBe("connection_failed");
    expect(state.busy).toBe(false);
  });

  it("a stale operation cannot clear the busy flag of the newer operation", async () => {
    const slow = deferred<ConnectionSnapshot>();
    const newer = deferred<ConnectionSnapshot>();

    mocks.Connect.mockImplementationOnce(() => slow.promise);

    const first = useConnectionStore.getState().connect("cfg-a");

    act(() => {
      useConnectionStore.setState({ busy: false });
    });

    mocks.Connect.mockImplementationOnce(() => newer.promise);

    const second = useConnectionStore.getState().connect("cfg-b");

    // B owns busy now. A's late resolution must leave B's flag alone.
    slow.resolve(snapshot("connected_verified", { config_id: "cfg-a" }));
    await act(async () => {
      await first;
    });

    expect(useConnectionStore.getState().busy).toBe(true);

    newer.resolve(snapshot("connected_verified", { config_id: "cfg-b" }));
    await act(async () => {
      await second;
    });

    expect(useConnectionStore.getState().busy).toBe(false);
    expect(useConnectionStore.getState().snapshot?.config_id).toBe("cfg-b");
  });

  it("disconnect invalidates the still-pending connect", async () => {
    const slow = deferred<ConnectionSnapshot>();
    const teardown = deferred<ConnectionSnapshot>();

    mocks.Connect.mockImplementationOnce(() => slow.promise);

    const connecting = useConnectionStore.getState().connect("cfg-a");

    act(() => {
      useConnectionStore.setState({ busy: false });
    });

    mocks.Disconnect.mockImplementationOnce(() => teardown.promise);

    const stopping = useConnectionStore.getState().disconnect();

    // Disconnect applies first; the connect resolves afterwards.
    teardown.resolve(snapshot("disconnected"));

    await act(async () => {
      await stopping;
    });

    slow.resolve(snapshot("connected_verified", { config_id: "cfg-a" }));

    await act(async () => {
      await connecting;
    });

    expect(useConnectionStore.getState().snapshot?.state).toBe("disconnected");
    expect(useConnectionStore.getState().busy).toBe(false);
  });

  it("invalidation drops every pending result (unmount / test isolation)", async () => {
    const slow = deferred<ConnectionSnapshot>();

    mocks.ConnectionState.mockImplementationOnce(() => slow.promise);

    const pending = useConnectionStore.getState().refresh();

    act(() => {
      useConnectionStore.getState().invalidatePendingOperations();
    });

    slow.resolve(snapshot("connected_verified"));
    await act(async () => {
      await pending;
    });

    expect(useConnectionStore.getState().snapshot).toBeNull();
  });
});

/**
 * v0.9.12 — busy ownership on authoritative events.
 *
 * The backend Connect/Reconnect calls are long-running and the state
 * machine publishes real transitions WHILE they run. Each broadcast
 * invalidates the outstanding operation; the release contract proved
 * below is that the invalidated operation ALSO loses busy ownership —
 * otherwise every real connect resolved stale with busy=true stuck
 * and Connect/Disconnect/Reconnect stayed disabled until restart.
 */
describe("busy ownership on authoritative events (v0.9.12)", () => {
  it("an authoritative event during a blocking connect releases busy", async () => {
    const slow = deferred<ConnectionSnapshot>();

    mocks.Connect.mockImplementationOnce(() => slow.promise);

    const connecting = useConnectionStore.getState().connect("cfg-a");

    expect(useConnectionStore.getState().busy).toBe(true);

    // The machine transitions while Connect is still running: the
    // broadcast is newer evidence than the unresolved promise.
    act(() => {
      useConnectionStore.getState().ingestEvent(
        snapshot("connected", { config_id: "cfg-a" }),
      );
    });

    expect(useConnectionStore.getState().busy).toBe(false);
    expect(useConnectionStore.getState().snapshot?.state).toBe("connected");

    // The stale promise resolves afterwards: it must not overwrite
    // the authoritative snapshot (and busy stays released).
    slow.resolve(snapshot("connected_verified", { config_id: "cfg-a" }));
    await act(async () => {
      await connecting;
    });

    expect(useConnectionStore.getState().snapshot?.state).toBe("connected");
    expect(useConnectionStore.getState().busy).toBe(false);
  });

  it("an authoritative event during a failing connect releases busy", async () => {
    const failing = deferred<never>();

    mocks.Connect.mockImplementationOnce(() => failing.promise);

    const connecting = useConnectionStore.getState().connect("cfg-a");

    expect(useConnectionStore.getState().busy).toBe(true);

    act(() => {
      useConnectionStore.getState().ingestEvent(
        snapshot("connection_failed", { last_error: "dial failed" }),
      );
    });

    expect(useConnectionStore.getState().busy).toBe(false);
    expect(useConnectionStore.getState().snapshot?.state).toBe("connection_failed");

    // A subsequent operation can start immediately (the leak used to
    // make this impossible: busy stayed true forever).
    mocks.Disconnect.mockResolvedValueOnce(snapshot("disconnected"));

    await act(async () => {
      await useConnectionStore.getState().disconnect();
    });

    expect(useConnectionStore.getState().busy).toBe(false);
    expect(useConnectionStore.getState().snapshot?.state).toBe("disconnected");

    // The original failing promise rejects into the stale guard: it
    // must not corrupt the newer state.
    failing.reject(new Error("dial failed"));
    await act(async () => {
      await connecting;
    });

    expect(useConnectionStore.getState().snapshot?.state).toBe("disconnected");
    expect(useConnectionStore.getState().busy).toBe(false);
  });
});
