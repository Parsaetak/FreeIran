import { beforeEach, describe, expect, it, vi } from "vitest";
import { useProviderStore } from "./providerStore";
import type { ProviderInfoView } from "../services";

/**
 * Provider store contract (v0.9.8.1 §12): the persisted mode and the
 * provider list come from the backend verbatim; a failed load keeps
 * the page usable with the default Auto mode; a load already in
 * flight never double-fetches.
 */

const services = vi.hoisted(() => ({
  Mode: vi.fn(),
  List: vi.fn(),
  SetMode: vi.fn(),
}));

vi.mock("../services", () => ({
  call: (operation: () => Promise<unknown>) => operation(),
  providerService: {
    Mode: services.Mode,
    List: services.List,
    SetMode: services.SetMode,
  },
}));

function info(name: string): ProviderInfoView {
  return {
    name,
    kind: name === "tor" ? "tor" : "psiphon",
    installed: true,
    state: "installed",
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  useProviderStore.setState({
    mode: "auto",
    providers: [],
    loaded: false,
    loading: false,
    error: null,
  });
});

describe("provider store", () => {
  it("loads mode and providers", async () => {
    services.Mode.mockResolvedValue("tor");
    services.List.mockResolvedValue([info("tor"), info("psiphon")]);

    await useProviderStore.getState().load();

    const state = useProviderStore.getState();

    expect(services.Mode).toHaveBeenCalledTimes(1);
    expect(services.List).toHaveBeenCalledTimes(1);
    expect(state.mode).toBe("tor");
    expect(state.providers).toHaveLength(2);
    expect(state.loaded).toBe(true);
    expect(state.loading).toBe(false);
    expect(state.error).toBeNull();
  });

  it("falls back to auto mode on unknown value", async () => {
    services.Mode.mockResolvedValue("bogus");
    services.List.mockResolvedValue([]);

    await useProviderStore.getState().load();

    expect(useProviderStore.getState().mode).toBe("auto");
    expect(useProviderStore.getState().loaded).toBe(true);
  });

  it("keeps page usable when provider load fails", async () => {
    services.Mode.mockRejectedValue(new Error("provider engine unavailable"));

    await useProviderStore.getState().load();

    const state = useProviderStore.getState();

    expect(state.loaded).toBe(true);
    expect(state.loading).toBe(false);
    expect(state.error).toBeTruthy();
    expect(state.mode).toBe("auto");
    expect(state.providers).toEqual([]);
  });

  it("setMode persists and applies the backend-normalized value", async () => {
    services.SetMode.mockResolvedValue("psiphon");

    await useProviderStore.getState().setMode("psiphon");

    expect(services.SetMode).toHaveBeenCalledWith("psiphon");
    expect(useProviderStore.getState().mode).toBe("psiphon");
  });

  it("load is a no-op while already loading", async () => {
    useProviderStore.setState({ loading: true });

    await useProviderStore.getState().load();

    expect(services.Mode).not.toHaveBeenCalled();
    expect(services.List).not.toHaveBeenCalled();
    expect(useProviderStore.getState().loading).toBe(true);
  });
});
