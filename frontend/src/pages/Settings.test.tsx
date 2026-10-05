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
import { act, cleanup, fireEvent, render } from "@testing-library/react";
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

/**
 * v0.14.2 Settings bounded-integer stepper regression tests.
 *
 * The six bounded integer settings render as [-] value [+] compact
 * groups (SettingNumberInput). These tests pin the observable contract:
 *
 *   - steppers wrap ONLY the bounded integer controls (never ports);
 *   - each step button carries an accessible name derived from the
 *     field label;
 *   - stepping clamps to [min, max] and the buttons disable at the
 *     bounds;
 *   - direct keyboard entry into the input still works (native number
 *     semantics preserved, no backend semantic change);
 *   - the section grouping (General → … → About) stays stable.
 */
describe("v0.14.2 Settings bounded-integer steppers", () => {
  const STEPPER_FIELDS = [
    { id: "refresh-interval", label: "Refresh interval" },
    { id: "log-max-mb", label: "Log size limit" },
    { id: "log-max-backups", label: "Log backups" },
    { id: "log-retention-days", label: "Log retention" },
    { id: "dev-queue-workers", label: "Test queue workers" },
    { id: "dev-net-timeout", label: "Network-test timeout" },
  ] as const;

  it("wraps every bounded integer control in a stepper with accessible step buttons", () => {
    const { container } = render(<SettingsPage />);

    for (const { id, label } of STEPPER_FIELDS) {
      const input = container.querySelector<HTMLInputElement>(`#${id}`);
      expect(input, `stepper field #${id} must exist`).not.toBeNull();

      const group = input?.closest(".number-stepper");
      expect(
        group,
        `bounded integer control #${id} must render inside .number-stepper`,
      ).not.toBeNull();

      const decrease = group?.querySelector<HTMLButtonElement>(`button[aria-label="Decrease ${label}"]`);
      const increase = group?.querySelector<HTMLButtonElement>(`button[aria-label="Increase ${label}"]`);

      expect(decrease, `#${id} decrease button must be named for screen readers`).not.toBeNull();
      expect(increase, `#${id} increase button must be named for screen readers`).not.toBeNull();
    }
  });

  it("does not add steppers to free-form port inputs", () => {
    const { container } = render(<SettingsPage />);

    for (const id of ["local-socks-port", "local-http-port"]) {
      const input = container.querySelector<HTMLInputElement>(`#${id}`);
      expect(input, `port control #${id} must exist`).not.toBeNull();
      expect(
        input?.closest(".number-stepper"),
        `port control #${id} must stay a free-form input (no stepper)`,
      ).toBeNull();
    }
  });

  it("steps a bounded value with the mouse and clamps at the minimum bound", () => {
    const { container } = render(<SettingsPage />);

    // dev_queue_workers is 0 in the fixture (= its minimum): decrease
    // must be disabled, increase must move exactly one step.
    const group = container.querySelector(`#dev-queue-workers`)?.closest(".number-stepper");
    expect(group).not.toBeNull();

    const input = group?.querySelector<HTMLInputElement>("input");
    const decrease = group?.querySelector<HTMLButtonElement>('button[aria-label="Decrease Test queue workers"]');
    const increase = group?.querySelector<HTMLButtonElement>('button[aria-label="Increase Test queue workers"]');

    expect(decrease?.disabled, "decrease must be disabled at the minimum").toBe(true);

    act(() => {
      fireEvent.click(increase!);
    });

    expect(input?.value, "increase must step the value by exactly 1").toBe("1");
  });

  it("disables the increase button at the maximum bound", () => {
    const maxed = {
      ...settingsFixture(),
      refresh_interval_minutes: 1440,
    } as unknown as Settings;

    act(() => {
      useSettingsStore.setState({ settings: maxed, loading: false, saving: false, lastError: null });
    });

    const { container } = render(<SettingsPage />);

    const group = container.querySelector(`#refresh-interval`)?.closest(".number-stepper");
    const increase = group?.querySelector<HTMLButtonElement>('button[aria-label="Increase Refresh interval"]');
    const decrease = group?.querySelector<HTMLButtonElement>('button[aria-label="Decrease Refresh interval"]');

    expect(increase?.disabled, "increase must be disabled at the maximum").toBe(true);
    expect(decrease?.disabled, "decrease must stay enabled below the maximum").toBe(false);
  });

  it("keeps direct keyboard entry into the input working", () => {
    const { container } = render(<SettingsPage />);

    const input = container.querySelector<HTMLInputElement>("#refresh-interval");
    expect(input).not.toBeNull();

    act(() => {
      fireEvent.change(input!, { target: { value: "120" } });
    });

    expect(input?.value, "direct entry must update the draft-backed input").toBe("120");
  });

  it("keeps the documented section grouping order", () => {
    const { container } = render(<SettingsPage />);

    const titles = Array.from(
      container.querySelectorAll<HTMLElement>(".card-title"),
    ).map((el) => el.textContent?.trim());

    expect(titles).toEqual([
      "General",
      "Connection",
      "Testing",
      "Test modes & ranking",
      "Appearance",
      "Reliability",
      "Diagnostics & support",
      "Developer",
      "About",
    ]);
  });

  it("keeps selects and toggles rendering with their current behavior", () => {
    const { container } = render(<SettingsPage />);

    for (const id of ["preferred-backend", "testing-policy", "test-mode", "sort-mode", "log-level"]) {
      const select = container.querySelector<HTMLSelectElement>(`#${id}`);
      expect(select, `select #${id} must exist`).not.toBeNull();
    }

    const trayToggle = container.querySelector<HTMLButtonElement>("#system-tray");
    expect(trayToggle, "system tray switch must exist").not.toBeNull();
    expect(trayToggle?.getAttribute("role")).toBe("switch");
    expect(trayToggle?.getAttribute("aria-checked")).toBe("true");
  });
});
