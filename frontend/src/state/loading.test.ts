import { describe, expect, it } from "vitest";
import {
  BOOT_PHASES,
  DEFER_SKELETON_MS,
  IMMEDIATE_MS,
  bootPhaseIndex,
  bootPhaseLabel,
  skeletonVisible,
} from "./loading";

describe("skeletonVisible — the §8 timing policy", () => {
  it("shows nothing when not loading", () => {
    expect(skeletonVisible(null, 99999)).toBe(false);
  });

  it("suppresses the skeleton inside the immediate window (<100ms)", () => {
    expect(skeletonVisible(1000, 1000 + IMMEDIATE_MS - 1)).toBe(false);
  });

  it("suppresses the skeleton in the 100–180ms disturbance window", () => {
    expect(skeletonVisible(1000, 1000 + DEFER_SKELETON_MS - 1)).toBe(false);
  });

  it("shows the skeleton at or beyond the 180ms threshold", () => {
    expect(skeletonVisible(1000, 1000 + DEFER_SKELETON_MS)).toBe(true);
    expect(skeletonVisible(1000, 1000 + 5000)).toBe(true);
  });

  it("honours a custom threshold", () => {
    expect(skeletonVisible(0, 50, 40)).toBe(true);
    expect(skeletonVisible(0, 39, 40)).toBe(false);
  });
});

describe("boot phase scale — determinate progress", () => {
  it("contains the full backend lifecycle in order", () => {
    const keys = BOOT_PHASES.map((phase) => phase.key);

    expect(keys).toEqual([
      "boot",
      "workspace_ready",
      "store_metadata_ready",
      "services_ready",
      "ui_runtime_ready",
      "ui_ready",
      "background_warmup",
      "ready",
    ]);
  });

  it("maps known phases to their index", () => {
    expect(bootPhaseIndex("boot")).toBe(0);
    expect(bootPhaseIndex("services_ready")).toBe(3);
    expect(bootPhaseIndex("ready")).toBe(BOOT_PHASES.length - 1);
  });

  it("returns -1 for unknown or missing phases without throwing", () => {
    expect(bootPhaseIndex(null)).toBe(-1);
    expect(bootPhaseIndex(undefined)).toBe(-1);
    expect(bootPhaseIndex("some_future_phase")).toBe(-1);
  });

  it("labels unknown phases with the raw key (forward compatible)", () => {
    expect(bootPhaseLabel("services_ready")).toBe("Services ready");
    expect(bootPhaseLabel("some_future_phase")).toBe("some_future_phase");
    expect(bootPhaseLabel(null)).toBe("");
  });
});
