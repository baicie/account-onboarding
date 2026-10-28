import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// Built assets are embedded into the Go binary (internal/api/webdist) and served at /ui/.
export default defineConfig({
  plugins: [react()],
  // The bundle is served from /ui/app by the Go embed handler, so asset URLs must
  // carry that prefix; with plain '/ui/' every script and style request would 404.
  base: '/ui/app/',
  build: {
    outDir: path.resolve(__dirname, '../internal/api/webdist'),
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    proxy: {
      '/v1': { target: 'http://127.0.0.1:18788', changeOrigin: false },
      '/ui/api': { target: 'http://127.0.0.1:18788', changeOrigin: false },
    },
  },
})
