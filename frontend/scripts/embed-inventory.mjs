/**
 * The ONE authoritative embedded-asset inventory and validator.
 *
 * Consumers (single source of truth — no duplicated path logic):
 *   - scripts/copy-dist.mjs        (validates right after staging)
 *   - .github/workflows/ci.yml     ("Clean-room embed check" step runs
 *                                   scripts/validate-embed.mjs from the
 *                                   repository root)
 *
 * Canonical inventory (the user-facing filename contract, stable
 * across every release):
 *
 *      index.html
 *      assets/index.js
 *      assets/index.css
 *      assets/export-worker.js
 *
 * Any hashed artifact (index-*.js, app-*.css, exportWorker-*.js, ...),
 * duplicate worker or unexpected file is a hard failure.
 */
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

export const REQUIRED_ASSETS = [
  "index.html",
  "assets/index.js",
  "assets/index.css",
  "assets/export-worker.js",
];

/** Hashed-era artifact families that must never return, even under a
 * name the exact allowlist would already reject. */
const HASHED_PATTERN =
  /^assets\/(index|app|exportWorker|export-worker)-[A-Za-z0-9_-]+\.(js|css)$/;

/** Recursively lists every file below `dir` as a target-relative path. */
export function listFiles(dir, prefix = "") {
  const out = [];

  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const rel = prefix ? `${prefix}/${entry}` : entry;

    if (statSync(full).isDirectory()) {
      out.push(...listFiles(full, rel));
    } else {
      out.push(rel);
    }
  }

  return out.sort();
}

/**
 * Validates the staged embed tree against the canonical inventory.
 * Returns an array of human-readable problems (empty = valid).
 */
export function validateEmbedTree(target) {
  const problems = [];

  if (!existsSync(target)) {
    return [`embed tree does not exist: ${target}`];
  }

  const produced = listFiles(target);

  const missing = REQUIRED_ASSETS.filter((name) => !produced.includes(name));
  const unexpected = produced.filter((name) => !REQUIRED_ASSETS.includes(name));

  if (missing.length > 0) {
    problems.push(
      `embed inventory incomplete — missing: ${missing.join(", ")}`,
    );
  }

  if (unexpected.length > 0) {
    problems.push(
      `embed inventory has unexpected files: ${unexpected.join(", ")}`,
    );
  }

  // Hashed-era artifacts must never return, not even under a name the
  // exact allowlist would already reject — the explicit pattern
  // documents the forbidden families.
  const hashed = produced.filter((name) => HASHED_PATTERN.test(name));
  if (hashed.length > 0) {
    problems.push(`hashed asset filenames found: ${hashed.join(", ")}`);
  }

  // Every canonical asset must be present AND non-empty.
  for (const name of REQUIRED_ASSETS) {
    const full = join(target, name);

    if (existsSync(full) && statSync(full).size === 0) {
      problems.push(`embed asset is empty: ${name}`);
    }
  }

  // index.html must reference exactly the canonical stable names and
  // nothing hashed.
  const htmlPath = join(target, "index.html");

  if (existsSync(htmlPath)) {
    const html = readFileSync(htmlPath, "utf8");

    for (const ref of ["assets/index.js", "assets/index.css"]) {
      if (!html.includes(ref)) {
        problems.push(`index.html does not reference ${ref}`);
      }
    }

    if (/assets\/[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8,}\.(js|css)/.test(html)) {
      problems.push("index.html references a hashed asset filename");
    }
  }

  return problems;
}
