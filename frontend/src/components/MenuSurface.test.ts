import { describe, expect, it } from "vitest";
import { computeMenuPosition, type MenuAnchor } from "./MenuSurface";

/**
 * v0.9.15: pins the ONE shared viewport-aware placement algorithm —
 * prefer down/right, flip up/left when room runs out, clamp inside the
 * gutter at the viewport edges. Every menu in the application depends
 * on these rules.
 */

const VIEW = { width: 1280, height: 800 };
const SIZE = { width: 220, height: 300 };

const rect = (left: number, top: number, right: number, bottom: number): MenuAnchor => ({
  kind: "rect",
  rect: { left, top, right, bottom },
});

describe("computeMenuPosition", () => {
  it("opens downward and to the right in the middle of the viewport", () => {
    const pos = computeMenuPosition(rect(600, 300, 640, 330), SIZE, VIEW);

    expect(pos.top).toBe(330 + 4); // below the trigger
    expect(pos.left).toBe(600); // extends rightward
  });

  it("flips upward when there is no room below (bottom edge)", () => {
    const pos = computeMenuPosition(rect(600, 700, 640, 760), SIZE, VIEW);

    expect(pos.top + SIZE.height).toBeLessThanOrEqual(VIEW.height - 8);
    expect(pos.top).toBe(700 - 4 - SIZE.height); // above the trigger
  });

  it("flips left when there is no room to the right (right edge)", () => {
    const pos = computeMenuPosition(rect(1200, 300, 1240, 330), SIZE, VIEW);

    expect(pos.left + SIZE.width).toBeLessThanOrEqual(VIEW.width - 8);
    expect(pos.left).toBe(1240 - SIZE.width); // right edge aligned
  });

  it("flips right when anchored at the left edge", () => {
    const pos = computeMenuPosition({ kind: "point", x: 2, y: 300 }, SIZE, VIEW);

    expect(pos.left).toBeGreaterThanOrEqual(8);
    expect(pos.left + SIZE.width).toBeLessThanOrEqual(VIEW.width - 8);
  });

  it("clamps inside the gutter when neither vertical placement fits (tall menu, short window)", () => {
    const tiny = { width: 1280, height: 400 };
    const tall = { width: 220, height: 384 }; // fits exactly between the gutters
    const pos = computeMenuPosition(rect(600, 380, 640, 390), tall, tiny);

    expect(pos.top).toBeGreaterThanOrEqual(8);
    expect(pos.top + tall.height).toBeLessThanOrEqual(tiny.height - 8);
  });

  it("keeps the top at the gutter when the menu is taller than the usable viewport (the surface scrolls)", () => {
    const tiny = { width: 1280, height: 400 };
    const taller = { width: 220, height: 450 };
    const pos = computeMenuPosition(rect(600, 380, 640, 390), taller, tiny);

    expect(pos.top).toBe(8);
  });

  it("clamps horizontally inside the gutter on a narrow window", () => {
    const narrow = { width: 320, height: 800 };
    const wide = { width: 300, height: 200 };

    const pos = computeMenuPosition({ kind: "point", x: 160, y: 300 }, wide, narrow);

    expect(pos.left).toBeGreaterThanOrEqual(8);
    expect(pos.left + wide.width).toBeLessThanOrEqual(narrow.width - 8);
  });

  it("keeps point-anchored context menus fully inside the viewport (all four edges)", () => {
    const points = [
      { x: 10, y: 10 },
      { x: VIEW.width - 10, y: 10 },
      { x: 10, y: VIEW.height - 10 },
      { x: VIEW.width - 10, y: VIEW.height - 10 },
      { x: VIEW.width / 2, y: VIEW.height / 2 },
    ];

    for (const point of points) {
      const pos = computeMenuPosition({ kind: "point", ...point }, SIZE, VIEW);

      expect(pos.left).toBeGreaterThanOrEqual(8);
      expect(pos.top).toBeGreaterThanOrEqual(8);
      expect(pos.left + SIZE.width).toBeLessThanOrEqual(VIEW.width - 8);
      expect(pos.top + SIZE.height).toBeLessThanOrEqual(VIEW.height - 8);
    }
  });
});
