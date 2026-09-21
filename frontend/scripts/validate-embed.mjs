/**
 * CI-facing embed validator: checks the ALREADY-STAGED embed tree at
 * cmd/freeiran/frontend/dist against the canonical inventory.
 *
 * Run from the repository root (paths below are repo-root-relative):
 *
 *      node frontend/scripts/validate-embed.mjs
 *
 * The validation logic itself lives in embed-inventory.mjs — the one
 * shared source of truth with scripts/copy-dist.mjs. This script only
 * resolves the repo-root-relative target and reports the outcome for
 * CI, so the workflow carries no duplicated path/allowlist logic.
 */
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import process from "node:process";
import { validateEmbedTree } from "./embed-inventory.mjs";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(scriptDir, "..", "..");
const target = join(repoRoot, "cmd", "freeiran", "frontend", "dist");

const problems = validateEmbedTree(target);

if (problems.length > 0) {
  console.error("EMBED INVENTORY VIOLATION — cmd/freeiran/frontend/dist:");
  for (const problem of problems) {
    console.error(`  - ${problem}`);
  }
  console.error(
    "Stale or hashed assets are forbidden; run `npm run build:embed` " +
      "in frontend/ and commit the result.",
  );
  process.exit(1);
}

console.log(
  "embed inventory OK: cmd/freeiran/frontend/dist carries exactly the " +
    "canonical stable assets (index.html, assets/index.js, assets/index.css, " +
    "assets/export-worker.js)",
);
