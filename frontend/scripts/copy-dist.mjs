/**
 * Copies the Vite build output into the Go embed directory.
 *
 * `cmd/freeiran` embeds `frontend/dist` relative to its own package
 * directory (go:embed cannot traverse parent directories), so the
 * production build is copied there before `go build`. A placeholder
 * index.html is committed so the Go build also works before any
 * frontend build (tests, compile-validation).
 */
import { cpSync, existsSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const frontendDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(frontendDir, "..", "..");

const source = join(repoRoot, "frontend", "dist");
const target = join(repoRoot, "cmd", "freeiran", "frontend", "dist");

if (!existsSync(source)) {
  console.error("frontend/dist does not exist — run `vite build` first");
  process.exit(1);
}

rmSync(target, { recursive: true, force: true });

cpSync(source, target, { recursive: true });

console.log("copied frontend/dist -> cmd/freeiran/frontend/dist");
