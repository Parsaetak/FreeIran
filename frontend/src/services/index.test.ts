import { describe, expect, it } from "vitest";

import { parseHumanizedError } from "./index";

/**
 * The backend renders user-facing errors as
 * "<readable sentence>\n---\nTechnical details: <raw>" (v0.9.0 §8).
 * The UI must split them reliably — including when the format is not
 * present (plain binding errors).
 */
describe("parseHumanizedError", () => {
  it("splits the readable part from the technical details", () => {
    const message =
      "V2Ray could not start because local port 10808 is already in use by another application.\n---\nTechnical details: dependency_unavailable: system/start: listen tcp 127.0.0.1:10808: bind: address already in use";

    const parsed = parseHumanizedError(message);

    expect(parsed.readable).toContain("port 10808 is already in use");
    expect(parsed.technical).toContain("dependency_unavailable");
  });

  it("passes plain messages through unchanged", () => {
    const parsed = parseHumanizedError("app: source \"x\" not found");

    expect(parsed.readable).toBe("app: source \"x\" not found");
    expect(parsed.technical).toBe("");
  });

  it("does not split when the marker appears mid-sentence", () => {
    const parsed = parseHumanizedError("prefix\n---\nTechnical details: ");

    // Empty technical tail still splits — readable is the first line.
    expect(parsed.readable).toBe("prefix");
    expect(parsed.technical).toBe("");
  });
});
