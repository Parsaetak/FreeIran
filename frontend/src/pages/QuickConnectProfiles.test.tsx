/**
 * @vitest-environment jsdom
 *
 * v0.9.11 Connection Profiles — Quick Connect integration tests
 * (P2 §18): the profile surface renders ONLY what the backend
 * reports, activation updates the UI from the returned authoritative
 * state (never optimistic local state), the management section wires
 * create / rename-edit / duplicate / delete / set default to the
 * backend, and every connect path stays untouched (profiles never
 * bypass the engine).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QuickConnectPage } from "./QuickConnect";
import { useProfilesStore } from "../state/profilesStore";
import { useConnectionStore } from "../state/connectionStore";
import { useQuickConnectStore } from "../state/quickConnectStore";
import { useStartFlowStore } from "../state/startflowStore";
import { useSettingsStore } from "../state/settingsStore";
import { useProviderStore } from "../state/providerStore";

const mocks = vi.hoisted(() => ({
  BestCandidates: vi.fn(),
  Connect: vi.fn(),
  ConnectBest: vi.fn(),
  RunStartFlow: vi.fn(),
  ProviderMode: vi.fn(),
  ProviderList: vi.fn(),
  ProviderSetMode: vi.fn(),
  ProviderConnect: vi.fn(),
  ProviderConnectAuto: vi.fn(),
  ProfileList: vi.fn(),
  ProfileActive: vi.fn(),
  ProfileSetActive: vi.fn(),
  ProfileCreate: vi.fn(),
  ProfileUpdate: vi.fn(),
  ProfileRename: vi.fn(),
  ProfileDuplicate: vi.fn(),
  ProfileDelete: vi.fn(),
  ProfileSetDefault: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  connectionService: {
    BestCandidates: mocks.BestCandidates,
    Connect: mocks.Connect,
    ConnectBest: mocks.ConnectBest,
    ConnectionState: vi.fn(),
    RefreshBackends: vi.fn(),
    Disconnect: vi.fn(),
    Reconnect: vi.fn(),
  },
  discoveryService: {
    StartFlowStatus: vi.fn(async () => ({ stage: "idle", running: false })),
    Environment: vi.fn(async () => ({
      signals: [],
      restricted: false,
      deep_discovery_advised: false,
      summary: "",
      analyzed_at: "2026-01-01T00:00:00Z",
    })),
    RunStartFlow: mocks.RunStartFlow,
    CancelStartFlow: vi.fn(),
    DiscoverNow: vi.fn(),
  },
  settingsService: {
    Get: vi.fn(async () => ({ reduced_motion: false })),
    Save: vi.fn(),
  },
  providerService: {
    Mode: mocks.ProviderMode,
    List: mocks.ProviderList,
    SetMode: mocks.ProviderSetMode,
    Connect: mocks.ProviderConnect,
    ConnectAuto: mocks.ProviderConnectAuto,
  },
  profileService: {
    List: mocks.ProfileList,
    Active: mocks.ProfileActive,
    SetActive: mocks.ProfileSetActive,
    Create: mocks.ProfileCreate,
    Update: mocks.ProfileUpdate,
    Rename: mocks.ProfileRename,
    Duplicate: mocks.ProfileDuplicate,
    Delete: mocks.ProfileDelete,
    SetDefault: mocks.ProfileSetDefault,
    ClearDefault: vi.fn(),
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: { On: vi.fn(() => () => undefined), Emit: vi.fn() },
}));

function profileView(id: string, overrides: Record<string, unknown> = {}) {
  return {
    id,
    name: `Profile ${id}`,
    mode: "auto",
    config_available: false,
    active: false,
    default: false,
    ...overrides,
  };
}

function resetStores() {
  act(() => {
    useConnectionStore.setState({ snapshot: null, backends: [], health: null, busy: false, error: null });
    useQuickConnectStore.setState({ candidates: [], loading: false, loaded: false, error: null, selected: null });
    useStartFlowStore.setState({ status: null, environment: null, busy: false, error: null, lastResult: null });
    useSettingsStore.setState({ settings: null, loading: false, saving: false, lastError: null, motionOverride: null });
    useProviderStore.setState({ mode: "auto", providers: [], loaded: false, loading: false, error: null });
    useProfilesStore.setState({ profiles: [], active: null, loaded: false, loading: false, error: null });
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  resetStores();

  mocks.BestCandidates.mockResolvedValue([]);
  mocks.RunStartFlow.mockResolvedValue({ discovered: 0, valid: 0, duplicates: 0, tested: 0, verified: false, duration_ms: 0 });
  mocks.ProviderMode.mockResolvedValue("auto");
  mocks.ProviderList.mockResolvedValue([]);
  mocks.ProviderSetMode.mockImplementation(async (mode: string) => mode);
  mocks.ProfileList.mockResolvedValue([]);
  mocks.ProfileActive.mockResolvedValue([null, false]);
  mocks.ProfileSetActive.mockImplementation(async (id: string) => profileView(id, { active: true }));
});

afterEach(() => {
  cleanup();
});

describe("Quick Connect profiles", () => {
  it("hides the profile surface entirely when the backend has no profiles", async () => {
    const { container } = render(<QuickConnectPage onNavigate={vi.fn()} />);

    await waitFor(() => expect(useProfilesStore.getState().loaded).toBe(true));

    expect(container.querySelector(".qc-profiles")).toBeNull();
  });

  it("renders one chip per backend profile with the active flag from authoritative state", async () => {
    mocks.ProfileList.mockResolvedValue([
      profileView("p-1", { name: "Home" }),
      profileView("p-2", { name: "Work", default: true }),
    ]);
    mocks.ProfileActive.mockResolvedValue([profileView("p-1", { name: "Home", active: true }), true]);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    const home = await screen.findByRole("radio", { name: /Home/ });
    const work = screen.getByRole("radio", { name: /Work/ });

    expect(home.getAttribute("aria-checked")).toBe("true");
    expect(work.getAttribute("aria-checked")).toBe("false");
  });

  it("activates a profile through the backend and re-syncs mode + configuration selection", async () => {
    mocks.ProfileList.mockResolvedValue([
      profileView("p-1", { name: "Work", mode: "configs", config_id: "cfg-123", config_available: true }),
    ]);
    mocks.ProfileActive.mockResolvedValue([profileView("p-1", { active: true }), true]);
    mocks.ProfileSetActive.mockResolvedValue(profileView("p-1", { name: "Work", active: true }));
    mocks.ProviderMode.mockResolvedValue("configs");
    mocks.BestCandidates.mockResolvedValue([
      {
        fingerprint: "cfg-123",
        name: "cfg-123",
        protocol: "vless",
        endpoint: "cfg.example:443",
        class: "good",
        score: 0.9,
        latency_ms: 42,
        success_rate: 1,
        samples: 2,
        tested_at: Date.now() - 1000,
        connectable: true,
      },
    ]);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    const chip = await screen.findByRole("radio", { name: /Work/ });

    fireEvent.click(chip);

    await waitFor(() => expect(mocks.ProfileSetActive).toHaveBeenCalledWith("p-1"));

    // The page re-read the provider mode the backend applied (configs)
    // and preselected the profile's configuration.
    await waitFor(() => expect(useProviderStore.getState().mode).toBe("configs"));
    await waitFor(() => expect(useQuickConnectStore.getState().selected).toBe("cfg-123"));

    // The activated chip reflects the authoritative active marker.
    await waitFor(() => expect(chip.getAttribute("aria-checked")).toBe("true"));
  });

  it("surfaces activation failures honestly and keeps the previous state", async () => {
    mocks.ProfileList.mockResolvedValue([profileView("p-1", { name: "Home" })]);
    mocks.ProfileActive.mockResolvedValue([null, false]);
    mocks.ProfileSetActive.mockRejectedValue(new Error("profile references a missing configuration"));

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    const chip = await screen.findByRole("radio", { name: /Home/ });

    fireEvent.click(chip);

    const alert = await screen.findByRole("alert");

    expect(alert.textContent).toContain("missing configuration");
    expect(chip.getAttribute("aria-checked")).toBe("false");
  });

  it("creates a profile through the manage section (name + mode + ports)", async () => {
    mocks.ProfileList.mockResolvedValue([profileView("p-1", { name: "Home" })]);
    mocks.ProfileActive.mockResolvedValue([null, false]);
    mocks.ProfileCreate.mockResolvedValue(profileView("p-9", { name: "Travel", active: false }));

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await screen.findByRole("radio", { name: /Home/ });

    fireEvent.click(screen.getByText("Manage profiles"));

    fireEvent.click(screen.getByText("New profile"));

    const nameInput = screen.getByLabelText("Name") as HTMLInputElement;
    fireEvent.change(nameInput, { target: { value: "Travel" } });

    const modeSelect = screen.getByLabelText("Mode") as HTMLSelectElement;
    fireEvent.change(modeSelect, { target: { value: "configs" } });

    fireEvent.click(screen.getByText("Create profile"));

    await waitFor(() =>
      expect(mocks.ProfileCreate).toHaveBeenCalledWith({
        name: "Travel",
        mode: "configs",
        local_socks_port: 0,
        local_http_port: 0,
      }),
    );
  });

  it("wires duplicate, delete and set-default to the backend", async () => {
    mocks.ProfileList.mockResolvedValue([
      profileView("p-1", { name: "Home" }),
      profileView("p-2", { name: "Work" }),
    ]);
    mocks.ProfileActive.mockResolvedValue([null, false]);
    mocks.ProfileDuplicate.mockResolvedValue(profileView("p-3", { name: "Home (copy)" }));
    mocks.ProfileSetDefault.mockResolvedValue(undefined);
    mocks.ProfileDelete.mockResolvedValue(undefined);

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await screen.findByRole("radio", { name: /Home/ });

    fireEvent.click(screen.getByText("Manage profiles"));

    const rows = screen.getAllByRole("listitem");

    // Row actions for the SECOND profile (Work).
    const workRow = rows.find((row) => row.textContent?.includes("Work")) as HTMLElement;

    fireEvent.click(within(workRow).getByText("Duplicate"));
    await waitFor(() => expect(mocks.ProfileDuplicate).toHaveBeenCalledWith("p-2"));

    fireEvent.click(within(workRow).getByText("Set default"));
    await waitFor(() => expect(mocks.ProfileSetDefault).toHaveBeenCalledWith("p-2"));

    fireEvent.click(within(workRow).getByText("Delete"));
    await waitFor(() => expect(mocks.ProfileDelete).toHaveBeenCalledWith("p-2"));
  });

  it("renames a profile through the edit form (Update carries the new name)", async () => {
    mocks.ProfileList.mockResolvedValue([profileView("p-1", { name: "Home" })]);
    mocks.ProfileActive.mockResolvedValue([null, false]);
    mocks.ProfileUpdate.mockResolvedValue(profileView("p-1", { name: "Home Office" }));

    render(<QuickConnectPage onNavigate={vi.fn()} />);

    await screen.findByRole("radio", { name: /Home/ });

    fireEvent.click(screen.getByText("Manage profiles"));
    fireEvent.click(screen.getByText("Rename / edit"));

    const nameInput = screen.getByLabelText("Name") as HTMLInputElement;
    expect(nameInput.value).toBe("Home");
    fireEvent.change(nameInput, { target: { value: "Home Office" } });

    fireEvent.click(screen.getByText("Save changes"));

    await waitFor(() =>
      expect(mocks.ProfileUpdate).toHaveBeenCalledWith(
        "p-1",
        expect.objectContaining({ name: "Home Office" }),
      ),
    );
  });
});

// Tiny within-helper (avoid importing @testing-library/dom separately).
function within(element: HTMLElement) {
  return {
    getByText(text: string): HTMLElement {
      const candidates = Array.from(element.querySelectorAll("button, span, a"));
      const found = candidates.find((node) => node.textContent?.trim() === text);
      if (!found) throw new Error(`within: ${text} not found`);
      return found as HTMLElement;
    },
  };
}
