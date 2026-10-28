/// <reference types="vitest/config" />
import path from 'path'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'

// The bundle is embedded by the Go binary and served at /ui.
// https://vite.dev/config/
export default defineConfig({
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
  plugins: [
    tanstackRouter({
      target: 'react',
      autoCodeSplitting: true,
    }),
    react(),
    tailwindcss(),
  ],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
})
