// The admin dashboard's build (ADR-0029). The bundle lands in internal/web/admin/dist/, which the
// hub embeds and serves at /admin under the pages' Content-Security-Policy: no inline script or
// style, nothing from another origin, so nothing is inlined as a data: URL and the module preload
// polyfill (an inline snippet in some setups) is off.
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// `npm run dev` proxies the API, the login and the hub's stylesheets to a hub on this address
// (`make up` serves one on 8080), so the dashboard works against real data while it is edited.
const hub = process.env.GRAVEL_HUB ?? "http://127.0.0.1:8080";

export default defineConfig({
  base: "/admin/",
  plugins: [react(), tailwindcss()],
  build: {
    // Make empties it first, keeping the .gitkeep that lets a clone without a bundle compile.
    outDir: "../../internal/web/admin/dist",
    emptyOutDir: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    sourcemap: false,
  },
  server: {
    proxy: {
      "/gravel.hub.v1.": hub,
      "/static/": hub,
      "/theme.css": hub,
      "/login": hub,
      "/auth/": hub,
      "/account": hub,
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["src/test/setup.ts"],
    css: false,
  },
});
