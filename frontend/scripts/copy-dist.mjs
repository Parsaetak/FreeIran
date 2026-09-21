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
 * canonical asset inventory in scripts/embed-inventory.mjs — the ONE
 * shared validation source (CI's "Clean-room embed check" runs
 * scripts/validate-embed.mjs against the same inventory). Any hashed
 * artifact, duplicate worker or unexpected file fails the build
 * loudly — the committed embed tree must always be EXACTLY what the
 * pinned toolchain produces.
 */
import { cpSync, existsSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { listFiles, validateEmbedTree } from "./embed-inventory.mjs";

const frontendDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(frontendDir, "..", "..");

const source = join(repoRoot, "frontend", "dist");
const target = join(repoRoot, "cmd", "freeiran", "frontend", "dist");

if (!existsSync(source)) {
  console.error("frontend/dist does not exist — run `vite build` first");
  process.exit(1);
}

// Wipe the target COMPLETELY: stale artifacts from any previous build
// (hashed bundles, renamed-era files, duplicate workers) must never
// survive into the new embed tree.
rmSync(target, { recursive: true, force: true });

cpSync(source, target, { recursive: true });

// ---- inventory verification (shared validator) ------------------------

const problems = validateEmbedTree(target);

if (problems.length > 0) {
  console.error(
    "embed inventory verification failed — refusing to stage a broken embed:",
  );

  for (const problem of problems) {
    console.error(`  - ${problem}`);
  }

  console.error(
    "The frontend build must produce the canonical stable assets " +
      "(see vite.config.ts and scripts/embed-inventory.mjs).",
  );

  process.exit(1);
}

const produced = listFiles(target);

console.log(
  `copied frontend/dist -> cmd/freeiran/frontend/dist ` +
    `(${produced.length} files, canonical stable inventory verified)`,
);
