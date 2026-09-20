/**
 * Copies the Vite build output into the Go embed directory.
 *
 * `cmd/freeiran` embeds `frontend/dist` relative to its own package
 * directory (go:embed cannot traverse parent directories), so the
 * production build is copied there before `go build`. A placeholder
 * index.html is committed so the Go build also works before any
 * frontend build (tests, compile-validation).
 *
 * CLEAN-ROOM EMBED GUARANTEE: the target is wiped completely, the
 * fresh output is copied, and the result is verified against the
 * canonical asset inventory below. Any hashed artifact, duplicate
 * worker or unexpected file fails the build loudly — the committed
 * embed tree must always be EXACTLY what the pinned toolchain
 * produces.
 *
 * CANONICAL INVENTORY (the user-facing filename contract, stable
 * across every release):
 *
 *      index.html
 *      assets/index.js
 *      assets/index.css
 *      assets/export-worker.js
 */
import { cpSync, existsSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const frontendDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(frontendDir, "..", "..");

const source = join(repoRoot, "frontend", "dist");
const target = join(repoRoot, "cmd", "freeiran", "frontend", "dist");

/**
 * The one authoritative embedded asset inventory. The frontend build
 * output must be exactly this set — stable logical filenames, no
 * content hashes, no duplicates, no compatibility copies. CI verifies
 * the generated tree against the same list (see
 * .github/workflows/ci.yml, "Clean-room embed check").
 */
const REQUIRED_ASSETS = [
  "index.html",
  "assets/index.js",
  "assets/index.css",
  "assets/export-worker.js",
];

if (!existsSync(source)) {
  console.error("frontend/dist does not exist — run `vite build` first");
  process.exit(1);
}

// Wipe the target COMPLETELY: stale artifacts from any previous build
// (hashed bundles, renamed-era files, duplicate workers) must never
// survive into the new embed tree.
rmSync(target, { recursive: true, force: true });

cpSync(source, target, { recursive: true });

// ---- inventory verification -------------------------------------------

/** Recursively lists every file below `dir` as a target-relative path. */
function listFiles(dir, prefix = "") {
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

const produced = listFiles(target);
const missing = REQUIRED_ASSETS.filter((name) => !produced.includes(name));
const unexpected = produced.filter((name) => !REQUIRED_ASSETS.includes(name));

if (missing.length > 0) {
  console.error(
    `embed inventory incomplete — missing: ${missing.join(", ")}. ` +
      "The frontend build did not produce the canonical stable assets " +
      "(see vite.config.ts); refusing to stage a broken embed.",
  );
  process.exit(1);
}

if (unexpected.length > 0) {
  console.error(
    `embed inventory has unexpected files: ${unexpected.join(", ")}. ` +
      "Hashed or stale artifacts are forbidden in the embed tree " +
      "(stable filenames only — see vite.config.ts).",
  );
  process.exit(1);
}

// Every canonical asset must be present AND non-empty.
for (const name of REQUIRED_ASSETS) {
  if (statSync(join(target, name)).size === 0) {
    console.error(`embed asset is empty: ${name}`);
    process.exit(1);
  }
}

// index.html must reference exactly the canonical stable names.
const html = readFileSync(join(target, "index.html"), "utf8");

for (const ref of ["assets/index.js", "assets/index.css"]) {
  if (!html.includes(ref)) {
    console.error(`index.html does not reference ${ref} — stable-name contract broken`);
    process.exit(1);
  }
}

if (/assets\/[A-Za-z0-9_-]+-[A-Za-z0-9_-]{8,}\.(js|css)/.test(html)) {
  console.error("index.html references a hashed asset filename — forbidden");
  process.exit(1);
}

console.log(
  `copied frontend/dist -> cmd/freeiran/frontend/dist ` +
    `(${produced.length} files, canonical stable inventory verified)`,
);
