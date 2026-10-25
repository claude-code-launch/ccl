import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// `npm run dev` serves the UI with hot reload and forwards the API to a running
// `ccl serve --port 8099` (CCL_SERVE_URL, default http://127.0.0.1:8099; without
// --port the server picks a free port). `npm run build` writes ../web, which
// internal/webui embeds into the ccl binary.
const backend = process.env.CCL_SERVE_URL || "http://127.0.0.1:8099";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // Relative assets resolve against the <base> ccl serve fills in, so one build
  // works at the root and under --base-path.
  base: "./",
  build: {
    outDir: "../web",
    emptyOutDir: true,
    // The licenses of every bundled package ship with the page (and with the
    // ccl binary that embeds it); THIRD_PARTY_NOTICES.md points here.
    license: { fileName: "licenses.md" },
    // One small file per build keeps the embedded asset list readable.
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: backend, changeOrigin: true },
      "/health": { target: backend, changeOrigin: true },
    },
  },
  test: {
    environment: "node",
  },
});
