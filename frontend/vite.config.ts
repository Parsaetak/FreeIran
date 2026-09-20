import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// FreeIran frontend build.
//
// The generated Wails bindings import "@wailsio/runtime", resolved by
// npm at build time. Inside the desktop app the backend serves the
// built assets; no HTTP API is involved.
//
// STABLE ASSET FILENAME CONTRACT (permanent): the embedded production
// tree uses fixed logical names that are replaced IN PLACE on every
// release — no content hashes, ever:
//
//      dist/index.html
//      dist/assets/index.js            (main entry bundle)
//      dist/assets/index.css           (entry stylesheet)
//      dist/assets/export-worker.js    (CSV export worker)
//
// index.js / index.css are the entry's natural names (the HTML entry
// is index.html), so the build produces them without any renaming.
// The Wails asset handler serves the embed with a revalidation cache
// policy (cmd/freeiran), so updated content is re-fetched after an
// upgrade without renaming files. copy-dist.mjs and the CI embed
// check enforce this exact inventory and fail on any hashed or stale
// artifact.
const stableOutput = {
  entryFileNames: "assets/index.js",
  chunkFileNames: "assets/[name].js",
  assetFileNames: "assets/[name][extname]",
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
  // Worker bundles keep their source-derived logical name
  // (export-worker.ts -> assets/export-worker.js). format "es" makes
  // the `?worker` import a single module-worker chunk of the MAIN
  // build — exactly one export-worker.js is emitted (the default iife
  // format emits the file twice: once from the worker pipeline and
  // once from the dynamic-import chunk, which the embed check
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
