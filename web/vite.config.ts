import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// Built assets are embedded into the Go binary (internal/api/webdist) and served at /ui/.
export default defineConfig({
  plugins: [react()],
  base: '/ui/',
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
