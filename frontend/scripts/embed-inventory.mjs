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
 * v0.9.10 code-split chunks (§7 Startup + performance): the six
 * secondary surfaces are lazy-loaded through React.lazy, and Vite
 * emits each one as a DETERMINISTIC, hash-free chunk named after its
 * page module. The chunk names below are part of the stable filename
 * contract exactly like the four canonical names — hashed artifacts
 * remain forbidden.
 */
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

export const REQUIRED_ASSETS = [
  "index.html",
  "assets/index.js",
  "assets/index.css",
  "assets/export-worker.js",
];

/** v0.9.10: deterministic lazy-load chunks (hash-free, module-named):
 * the six secondary page surfaces plus the shared binding chunks
 * Rollup splits alongside them. Every name is module-derived and
 * stable across builds; the inventory stays explicit so any NEW chunk
 * fails validation loudly and is added deliberately. */
export const LAZY_PAGE_ASSETS = [
  "assets/Dashboard.js",
  "assets/Connection.js",
  "assets/Cores.js",
  "assets/Network.js",
  "assets/Diagnostics.js",
  "assets/Settings.js",
  "assets/discovery.js",
  "assets/logservice.js",
  "assets/storageservice.js",
];

/** Everything the embed tree may legally contain. */
export const ALLOWED_ASSETS = [...REQUIRED_ASSETS, ...LAZY_PAGE_ASSETS];

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
  const unexpected = produced.filter((name) => !ALLOWED_ASSETS.includes(name));

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
