import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// Builds into ../go/internal/httpapi/web_dist — the v0.5 forge binary
// embeds this directory via //go:embed (go/internal/httpapi/pwa.go) and
// serves the PWA shell from there. The V4 path ../foundry/web_dist is
// retired in V5 (the V4 dashboard is decommissioned at cutover).
// See docs/v5-plan.md Phase 4 and docs/v5-api-contract.md §"Static PWA
// shell routes".
export default defineConfig({
  plugins: [
    react(),
    VitePWA({
      registerType: 'autoUpdate',
      manifest: {
        name: 'The Forge — Orchestration Console',
        short_name: 'Forge',
        description: 'Multi-model inference orchestration console for the Forge mesh.',
        theme_color: '#100e0c',
        background_color: '#100e0c',
        display: 'standalone',
        start_url: '/',
        scope: '/',
        icons: [
          { src: '/pwa-192.png', sizes: '192x192', type: 'image/png' },
          // mobile F5: maskable variant so Android/Chromium launchers can
          // crop to their icon shape instead of letterboxing the mark.
          { src: '/pwa-512.png', sizes: '512x512', type: 'image/png', purpose: 'any maskable' },
        ],
      },
      workbox: {
        // Never cache API responses — the SSE/Query layer is the source of
        // truth for live data; only the app shell should work offline.
        navigateFallbackDenylist: [/^\/api\//],
        runtimeCaching: [
          {
            urlPattern: /^\/api\//,
            handler: 'NetworkOnly',
          },
        ],
      },
    }),
  ],
  build: {
    outDir: '../go/internal/httpapi/web_dist',
    // Don't empty the outDir — it lives outside the vite project root
    // (which vite refuses to empty by default for safety) and we keep a
    // .gitkeep sentinel there so the //go:embed directive in
    // go/internal/httpapi/pwa.go always finds at least one file on a fresh
    // clone (where `npm run build` hasn't run yet). Vite overwrites the
    // real build files in place.
    emptyOutDir: false,
    manifest: true,
    rollupOptions: {
      input: 'index.html',
    },
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:5000', changeOrigin: true },
      '/login': { target: 'http://localhost:5000', changeOrigin: true },
      '/logout': { target: 'http://localhost:5000', changeOrigin: true },
    },
  },
})
