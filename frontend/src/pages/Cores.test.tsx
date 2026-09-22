/**
 * @vitest-environment jsdom
 *
 * v0.9.13 Cores page regression coverage (§3 + §4):
 *
 *   - an update-available card renders the version transition
 *     (v1.2.0 → v1.3.0), the authoritative download size from the
 *     retained manifest snapshot ("Update available · 18.4 MiB
 *     download") and the "Update to 1.3.0" action;
 *   - a missing size renders the truthful fallback, never a guess;
 *   - secondary operations live in the ⋮ overflow menu, not as one
 *     button per operation on every card;
 *   - the runtime section reports real counts from the lifecycle
 *     manifests.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CoresPage } from "./Cores";
import type { CoreLifecycleView } from "../services";

const serviceMocks = vi.hoisted(() => ({
  LifecycleInfo: vi.fn(),
  Install: vi.fn(),
  HealthCheck: vi.fn(),
  CheckForUpdates: vi.fn(),
  CheckAllForUpdates: vi.fn(),
  HealthCheckAll: vi.fn(),
  UpdateAll: vi.fn(),
  Repair: vi.fn(),
  Reinstall: vi.fn(),
  Rollback: vi.fn(),
  Enable: vi.fn(),
  Disable: vi.fn(),
  Uninstall: vi.fn(),
  State: vi.fn(),
  Memory: vi.fn(),
  Providers: vi.fn(),
  Health: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  coreService: {
    LifecycleInfo: serviceMocks.LifecycleInfo,
    Install: serviceMocks.Install,
    HealthCheck: serviceMocks.HealthCheck,
    CheckForUpdates: serviceMocks.CheckForUpdates,
    CheckAllForUpdates: serviceMocks.CheckAllForUpdates,
    HealthCheckAll: serviceMocks.HealthCheckAll,
    UpdateAll: serviceMocks.UpdateAll,
    Repair: serviceMocks.Repair,
    Reinstall: serviceMocks.Reinstall,
    Rollback: serviceMocks.Rollback,
    Enable: serviceMocks.Enable,
    Disable: serviceMocks.Disable,
    Uninstall: serviceMocks.Uninstall,
  },
  appService: { State: serviceMocks.State },
  diagnosticsService: { Memory: serviceMocks.Memory },
  providerService: {
    Providers: serviceMocks.Providers,
    Health: serviceMocks.Health,
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: { On: () => () => undefined },
  Browser: { OpenURL: vi.fn().mockResolvedValue(undefined) },
}));

vi.mock("../state/toastStore", () => ({
  toast: vi.fn(),
  describeError: (error: unknown) => String(error),
}));

vi.mock("../state/providerStore", () => ({
  useProviderStore: (selector: (state: { providers: unknown[]; loaded: boolean; load: () => Promise<void>; refresh: () => Promise<void> }) => unknown) =>
    selector({ providers: [], loaded: true, load: async () => undefined, refresh: async () => undefined }),
}));

vi.mock("../state/quickConnectStore", () => ({
  useQuickConnectStore: { getState: () => ({ invalidate: () => undefined }) },
}));

function manifest(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    name: "xray",
    state: "update_available",
    version: "1.2.0",
    channel: "stable",
    binary_path: "/cores/xray/bin/xray.exe",
    checksum_sha256: "abc",
    release_tag: "v1.3.0",
    release_url: "https://github.com/XTLS/Xray-core/releases/tag/v1.3.0",
    installed_at: "2026-09-01T00:00:00Z",
    last_checked: "2026-09-21T00:00:00Z",
    last_health_check: "",
    last_health_result: null,
    previous_version: "",
    failure_reason: "",
    failure_stage: "",
    latest_known: "1.3.0",
    ...overrides,
  };
}

function view(manifestOverrides: Record<string, unknown> = {}): CoreLifecycleView {
  return {
    manifest: manifest(manifestOverrides) as unknown as CoreLifecycleView["manifest"],
    discovered: true,
    runtime_state: "ready",
    runtime_version: "1.2.0",
    path: "/cores/xray/bin/xray.exe",
    failure_message: "",
  };
}

beforeEach(() => {
  vi.clearAllMocks();

  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });

  serviceMocks.LifecycleInfo.mockResolvedValue([view()]);
  serviceMocks.State.mockResolvedValue({ native_acceleration: "available" });
  serviceMocks.Memory.mockResolvedValue({
    pressure: { state: "normal" },
    booster: { queue_concurrency: 4 },
  });
  serviceMocks.Providers.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
});

describe("Cores update size (v0.9.13 §3)", () => {
  it("renders version transition, authoritative size and the update action", async () => {
    serviceMocks.LifecycleInfo.mockResolvedValue([
      view({ latest_asset_size: 18_400_000 }),
    ]);

    render(<CoresPage />);

    // Version transition.
    await waitFor(() => {
      expect(screen.getByText(/v1\.2\.0 → v1\.3\.0/)).toBeTruthy();
    });

    // Authoritative size with the accurate "download" label
    // (18400000 bytes → "18 MiB" per formatBytes).
    expect(screen.getByText("Update available · 18 MiB download")).toBeTruthy();

    // The update action names the target version.
    expect(screen.getByRole("button", { name: "Update to 1.3.0" })).toBeTruthy();
  });

  it("shows the truthful fallback when the size is unavailable", async () => {
    serviceMocks.LifecycleInfo.mockResolvedValue([
      view({ latest_asset_size: 0 }),
    ]);

    render(<CoresPage />);

    await waitFor(() => {
      expect(screen.getByText("Update available · Download size unavailable")).toBeTruthy();
    });

    expect(screen.queryByText(/MiB download/)).toBeNull();
  });

  it("keeps secondary operations in the overflow menu", async () => {
    serviceMocks.LifecycleInfo.mockResolvedValue([
      view({ latest_asset_size: 18_400_000, previous_version: "1.1.0" }),
    ]);

    render(<CoresPage />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "More actions for Xray-core" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "More actions for Xray-core" }));

    await waitFor(() => {
      expect(screen.getByRole("menuitem", { name: "Check update" })).toBeTruthy();
    });

    expect(screen.getByRole("menuitem", { name: "Verify" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Repair" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Reinstall" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Roll back" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Open official release page" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Uninstall" })).toBeTruthy();
  });
});

describe("Cores runtime section (v0.9.13 §4)", () => {
  it("reports real core counts and engine status", async () => {
    serviceMocks.LifecycleInfo.mockResolvedValue([
      view({ latest_asset_size: 1000 }),
      view({
        name: "v2ray",
        state: "ready",
        version: "5.0.0",
        latest_known: "",
        release_url: "https://example.com/v2ray",
      }),
    ]);

    render(<CoresPage />);

    await waitFor(() => {
      expect(screen.getByLabelText("Runtime status")).toBeTruthy();
    });

    const runtime = screen.getByLabelText("Runtime status");

    // 2 installed, 2 ready (update_available counts as ready), 1 update.
    expect(runtime.textContent).toContain("2/2 ready");
    expect(runtime.textContent).toContain("1 update available");
    // Native acceleration from the REAL AppState snapshot.
    expect(runtime.textContent).toContain("available");
    // Memory pressure + booster from the REAL Memory snapshot.
    expect(runtime.textContent).toContain("normal");
    expect(runtime.textContent).toContain("4 test workers");
  });

  it("renders the aggregate Verify all action", async () => {
    render(<CoresPage />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Verify all" })).toBeTruthy();
    });
  });
});
