import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwind from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'

// The SPA is embedded into the Go binary, so it is always served from a
// sub-path and never from a CDN. `base` must match config ui.path.
export default defineConfig({
  base: '/ui/',
  plugins: [
    tanstackRouter({ target: 'react', autoCodeSplitting: true }),
    react(),
    tailwind(),
  ],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // The server ships one binary; a handful of well-named chunks keeps the
    // embedded payload debuggable.
    chunkSizeWarningLimit: 900,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/v1': 'http://127.0.0.1:8080',
    },
  },
})
