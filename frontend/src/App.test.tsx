/**
 * @vitest-environment jsdom
 *
 * v0.12.2 regression guard for the blank-window root cause.
 *
 * v0.12.1 shipped App.tsx with useSuppressNativeContextMenu() called
 * INSIDE a useEffect callback while the helper itself calls useEffect
 * — a Rules of Hooks violation. The invalid-hook error threw inside
 * App's own effect, above every ErrorBoundary, and React unmounted
 * the entire tree: the backend reached application_ready and the
 * window appeared, but the visible UI was blank.
 *
 * No test rendered the real <App/>, so the violation was invisible.
 * This test renders the REAL App shell (pages stubbed, runtime
 * mocked) and must mount it without throwing — the exact failure
 * class that produced the blank window.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

vi.mock("@wailsio/runtime", () => ({
  Events: {
    On: () => () => undefined,
    Emit: () => undefined,
  },
  // Generated binding modules transitively imported by real stores use
  // these runtime helpers at module load; the shell test never calls
  // the backend, so permissive stubs are sufficient.
  Call: { ByID: () => new Promise<never>(() => undefined) },
  CancellablePromise: class {},
  Create: {
    Any: () => undefined,
    Array: () => [],
    Map: () => ({}),
    Nullable: () => undefined,
  },
}));

// Never-resolving service layer: the shell must mount and paint
// regardless of backend timing — exactly the property the desktop
// window needs. No promise rejections, no store crashes.
vi.mock("../services", () => ({
  call: () => new Promise<never>(() => undefined),
  diagnosticsService: {
    Version: () => new Promise<never>(() => undefined),
  },
}));

vi.mock("../pages/QuickConnect", () => ({
  QuickConnectPage: () => <section aria-label="Quick Connect (stub)">qc</section>,
}));

vi.mock("../pages/Sources", () => ({
  SourcesPage: () => <section aria-label="Sources (stub)">sources</section>,
}));

vi.mock("../pages/Configs", () => ({
  ConfigsPage: () => <section aria-label="Configurations (stub)">configs</section>,
}));

import { App } from "./App";

beforeAll(() => {
  // jsdom has no matchMedia; App uses it for the narrow-window collapse.
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    })),
  });
});

describe("App shell mount (v0.12.2 blank-window regression)", () => {
  afterEach(() => {
    cleanup();
  });

  it("mounts the real App shell without an invalid-hook error", () => {
    // With the v0.12.1 bug this render threw "Invalid hook call"
    // (hook inside useEffect callback) and nothing painted.
    expect(() => render(<App />)).not.toThrow();
  });

  it("paints the FreeIran shell with primary navigation", () => {
    render(<App />);

    expect(screen.getByText("FreeIran")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Connect/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Configurations/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Sources/ })).toBeTruthy();
  });
});
