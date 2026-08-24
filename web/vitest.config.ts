import { defineConfig } from 'vitest/config'
import { fileURLToPath } from 'node:url'

// Unit tests only: pure logic that would otherwise be checked by clicking.
// Anything needing a browser is a Playwright test instead (see e2e/).
export default defineConfig({
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    include: ['src/**/*.test.ts'],
    environment: 'node',
  },
})
