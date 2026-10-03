/**
 * @vitest-environment jsdom
 *
 * v0.13.1 Settings compact-control regression tests.
 *
 * v0.13.0 shipped compact-input CSS but the RENDERED layout stayed
 * oversized: the wrappers (settings-row fields with min-width 160px,
 * stretched grid tracks, the sysint stretch, the profile form's
 * divergent styling) kept numeric controls inside oversized
 * containers, and the range sliders were unstyled full-width. The
 * fix is a layout-system change; these tests pin its observable
 * surface so a regression to the v0.13.0 layout fails CI:
 *
 *   - every numeric control in Settings carries the compact
 *     treatment (input-compact; ports the 90px input-port variant);
 *   - the three range sliders carry the input-range hook;
 *   - the page root exposes the page-settings hook (the card-width
 *     cap anchor);
 *   - the companion CSS contract test (styles/index.css.test.ts)
 *     pins the stylesheet rules themselves.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import { SettingsPage } from "./Settings";
import { useSettingsStore } from "../state/settingsStore";
import { useConnectionStore } from "../state/connectionStore";
import type { Settings } from "../services";

const mocks = vi.hoisted(() => ({
  DeveloperInfo: vi.fn(),
  BuildDiagnosticReport: vi.fn(),
  ClearCaches: vi.fn(),
}));

vi.mock("../services", () => ({
  call: async (operation: () => Promise<unknown>) => operation(),
  diagnosticsService: {
    DeveloperInfo: mocks.DeveloperInfo,
    BuildDiagnosticReport: mocks.BuildDiagnosticReport,
  },
  appService: {
    ClearCaches: mocks.ClearCaches,
  },
  storageService: {
    OpenDataDir: vi.fn(),
    OpenWorkspace: vi.fn(),
    CleanupNow: vi.fn(),
    RemoveStaleRuntime: vi.fn(),
    RebuildIndex: vi.fn(),
  },
  logService: {
    OpenLogsDir: vi.fn(),
  },
}));

vi.mock("@wailsio/runtime", () => ({
  Events: { On: vi.fn(() => () => undefined), Emit: vi.fn() },
  Create: Object.assign(
    (type: unknown) => type,
    {
      Array: (fn: unknown) => fn,
      Nullable: (fn: unknown) => fn,
      Map: (fnK: unknown, _fnV: unknown) => fnK,
      Any: undefined,
      Default: (fn: unknown) => fn,
    },
  ),
}));

/** A complete-enough Settings payload for a fully-interactive page. */
function settingsFixture() {
  return {
    preferred_backend: "",
    connect_mode: "",
    refresh_interval_minutes: 60,
    testing_policy: "on_add",
    log_level: "",
    logging_profile: "normal",
    log_max_bytes_mb: 64,
    log_max_backups: 5,
    log_retention_days: 30,
    reduced_motion: false,
    tray_enabled: true,
    local_socks_port: 0,
    local_http_port: 0,
    allow_untrusted_public_routes: false,
    dev_verbose_diagnostics: false,
    dev_queue_workers: 0,
    dev_net_timeout_seconds: 0,
    dev_force_go_fallback: false,
    test_mode: "ping",
    test_ping_samples: 4,
    test_url: "",
    test_url_timeout_seconds: 0,
    test_max_candidates: 20,
    sort_mode: "latency",
    enable_racing: true,
    racing_candidates: 3,
    disable_auto_recovery: false,
  } as unknown as Settings;
}

beforeEach(() => {
  vi.clearAllMocks();
  mocks.DeveloperInfo.mockResolvedValue({
    version: "0.13.1",
    commit: "dev",
    go_version: "go1.26.8",
    workspace: "/tmp/freeiran",
    store: {},
    cores: [],
  });

  act(() => {
    useSettingsStore.setState({
      settings: settingsFixture(),
      loading: false,
      saving: false,
      lastError: null,
      motionOverride: null,
    });
    useConnectionStore.setState({
      snapshot: null,
      backends: [],
      health: null,
      busy: false,
      error: null,
    });
  });
});

afterEach(cleanup);

describe("v0.13.1 Settings compact controls", () => {
  it("renders the page with the page-settings layout hook", () => {
    const { container } = render(<SettingsPage />);

    expect(container.querySelector(".page-settings")).not.toBeNull();
  });

  it("keeps every numeric input compact", () => {
    const { container } = render(<SettingsPage />);

    const compact = [
      "refresh-interval",
      "log-max-mb",
      "log-max-backups",
      "log-retention-days",
      "dev-queue-workers",
      "dev-net-timeout",
    ];

    for (const id of compact) {
      const input = container.querySelector<HTMLInputElement>(`#${id}`);
      expect(input, `numeric control #${id} must exist`).not.toBeNull();
      expect(
        input?.classList.contains("input-compact"),
        `numeric control #${id} must carry the compact treatment`,
      ).toBe(true);
      expect(
        input?.classList.contains("input"),
        `numeric control #${id} must carry the base input styling`,
      ).toBe(true);
    }
  });

  it("sizes port inputs with the 90px input-port variant", () => {
    const { container } = render(<SettingsPage />);

    for (const id of ["local-socks-port", "local-http-port"]) {
      const input = container.querySelector<HTMLInputElement>(`#${id}`);
      expect(input, `port control #${id} must exist`).not.toBeNull();
      expect(
        input?.classList.contains("input-port"),
        `port control #${id} must carry the 90px port variant`,
      ).toBe(true);
    }
  });

  it("hooks every range slider into the styled compact treatment", () => {
    const { container } = render(<SettingsPage />);

    for (const id of ["test-samples", "test-max", "racing-candidates"]) {
      const input = container.querySelector<HTMLInputElement>(`#${id}`);
      expect(input, `range slider #${id} must exist`).not.toBeNull();
      expect(
        input?.classList.contains("input-range"),
        `range slider #${id} must carry the input-range styling hook`,
      ).toBe(true);
    }
  });

  it("exposes no unclassed numeric input on the page", () => {
    const { container } = render(<SettingsPage />);

    const numerics = container.querySelectorAll<HTMLInputElement>('input[type="number"]');
    expect(numerics.length).toBeGreaterThanOrEqual(8);

    for (const input of numerics) {
      expect(
        input.classList.contains("input-compact"),
        `unclassed numeric input #${input.id} — every numeric control must carry the compact treatment`,
      ).toBe(true);
    }
  });
});
