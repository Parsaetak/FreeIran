/**
 * CSS contract tests for the v0.13.1 Settings compact-control system.
 *
 * jsdom does not compute stylesheets, so a DOM test alone cannot prove
 * the RENDERED layout. These tests read the stylesheet source and pin
 * the exact rules whose absence produced the v0.13.0 defect (oversized
 * wrappers around correctly-sized inputs):
 *
 *   - .settings-row > .field must NOT force min-width: 160px / flex: 1
 *     (the ragged two-line runtime-log row);
 *   - .field-grid.two must NOT stretch port pairs through 1fr tracks;
 *   - the sysint port grid must NOT stretch (flex: 1 1 auto);
 *   - the attribute-scoped numeric guard must exist for every settings
 *     surface (settings form, sysint, profile form) including spinner
 *     suppression;
 *   - the port variant and the range-slider treatment must exist;
 *   - the page-settings card-width cap must exist.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

const rawCSS = readFileSync(fileURLToPath(new URL("./index.css", import.meta.url)), "utf8");

/** Comments stripped so selector lookups never match prose that
 * merely mentions a selector. */
const css = rawCSS.replace(/\/\*[\s\S]*?\*\//g, "");

/** Extract the body of the first declaration block belonging to a
 * rule whose selector list contains `selector` (handles grouped
 * selectors: the lookup only needs the selector to appear before a
 * block opener, with only selector syntax in between). */
function ruleBody(selector: string): string {
  let from = 0;

  for (;;) {
    const idx = css.indexOf(selector, from);
    if (idx === -1) return "";

    const open = css.indexOf("{", idx);
    if (open === -1) return "";

    const between = css.slice(idx + selector.length, open);

    // Only selector-list syntax may sit between the match and the
    // block opener (commas, whitespace, further selectors) — a prose
    // mention followed by an unrelated block contains ";" or ":".
    if (!/[;:]/.test(between)) {
      const close = css.indexOf("}", open);
      if (close === -1) return "";

      return css.slice(open + 1, close);
    }

    from = idx + selector.length;
  }
}

describe("v0.13.1 settings compact-control CSS contract", () => {
  it("settings-row fields size to content (no 160px floor, no flex stretch)", () => {
    const body = ruleBody(".settings-row > .field");
    expect(body).not.toBe("");
    expect(body).not.toContain("min-width: 160px");
    expect(body).not.toContain("flex: 1");
    expect(body).toContain("flex: 0 1 auto");
    expect(body).toContain("min-width: 0");
  });

  it("paired port grids use content-sized tracks (no 1fr stretching)", () => {
    const body = ruleBody(".field-grid.two");
    expect(body).not.toBe("");
    expect(body).not.toContain("minmax(0, 1fr)");
    expect(body).toContain("max-content");
  });

  it("the sysint port grid does not stretch", () => {
    const body = ruleBody(".sysint-ports .field-grid.two");
    expect(body).not.toBe("");
    expect(body).not.toContain("flex: 1 1 auto");
    expect(body).toContain("flex: 0 0 auto");
  });

  it("number inputs are width-guarded on every settings surface", () => {
    for (const surface of [
      '.settings-form input[type="number"]',
      '.sysint input[type="number"]',
      '.qc-profile-form input[type="number"]',
    ]) {
      const body = ruleBody(surface);
      expect(body, `${surface} width guard must exist`).not.toBe("");
      expect(body).toContain("width: 110px");
      expect(body).toContain("max-width: 110px");
    }
  });

  it("the native number spinner is suppressed on every settings surface", () => {
    for (const surface of [".settings-form", ".sysint", ".qc-profile-form"]) {
      const needle = `${surface} input[type="number"]::-webkit-inner-spin-button`;
      expect(
        css.includes(needle),
        `${needle} suppression rule must exist`,
      ).toBe(true);
    }
  });

  it("the 90px port variant exists", () => {
    const body = ruleBody(".input.input-compact.input-port");
    expect(body).not.toBe("");
    expect(body).toContain("width: 90px");
    expect(body).toContain("max-width: 90px");
  });

  it("range sliders have a styled, width-constrained treatment", () => {
    const body = ruleBody('.settings-form input[type="range"].input-range');
    expect(body).not.toBe("");
    expect(body).toContain("width: 220px");
    expect(body).toContain("max-width: 100%");

    // the thumb must be rendered (appearance suppression alone would
    // make the slider invisible on WebView2/Chromium)
    expect(css).toContain("-webkit-slider-thumb");
    expect(css).toContain("-moz-range-thumb");
  });

  it("the page-settings card width cap exists", () => {
    const body = ruleBody(".page-settings .card");
    expect(body).not.toBe("");
    expect(body).toContain("max-width: 720px");
  });

  it("the base compact input cap is still in place", () => {
    const body = ruleBody(".input.input-compact");
    expect(body).not.toBe("");
    expect(body).toContain("width: 110px");
    expect(body).toContain("max-width: 110px");
  });
});
