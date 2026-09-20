import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// FreeIran frontend build.
//
// The generated Wails bindings import "@wailsio/runtime", resolved by
// npm at build time. Inside the desktop app the backend serves the
// built assets; no HTTP API is involved.
//
// v0.9.8.7 — STABLE, UNIFIED ASSET FILENAMES: the embedded production
// tree uses fixed logical names that are replaced IN PLACE on every
// release — no content hashes, ever:
//
//      dist/index.html
//      dist/assets/app.js             (main entry bundle)
//      dist/assets/app.css            (entry stylesheet)
//      dist/assets/export-worker.js   (CSV export worker)
//
// The Wails asset handler serves the embed with a version-aware
// cache policy (cmd/freeiran), so updated content is re-fetched
// after an upgrade without renaming files. CI's clean-room embed
// check enforces this exact inventory and fails on any hashed or
// stale artifact.
const stableOutput = {
  entryFileNames: "assets/app.js",
  chunkFileNames: "assets/[name].js",
  // The entry stylesheet is bundled into the "index" entry chunk, so
  // Vite names it index.css — canonically renamed to app.css. Every
  // other asset keeps its logical name.
  assetFileNames: (info: { names?: readonly string[] }) => {
    const names = info.names ?? [];

    if (names.some((name) => name === "index.css")) {
      return "assets/app.css";
    }

    return "assets/[name][extname]";
  },
};

export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "chrome120",
    sourcemap: false,
    rollupOptions: {
      output: stableOutput,
    },
  },
  // Worker bundles get their own stable name. format "es" makes the
  // `?worker` import a single module-worker chunk of the MAIN build —
  // exactly one export-worker.js is emitted (the default iife format
  // emitted the file twice: once from the worker pipeline and once
  // from the dynamic-import chunk, which the clean-room embed check
  // forbids).
  worker: {
    format: "es",
    rollupOptions: {
      output: {
        entryFileNames: "assets/[name].js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
