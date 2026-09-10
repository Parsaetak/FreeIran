import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// FreeIran frontend build.
//
// The generated Wails bindings import "@wailsio/runtime", resolved by
// npm at build time. Inside the desktop app the backend serves the
// built assets; no HTTP API is involved.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    target: "chrome120",
    sourcemap: false,
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
