import { defineConfig } from '@playwright/test'

/**
 * The end-to-end suite runs against a real nanoasr binary with real weights,
 * so it lives beside the Go integration tests rather than in the fast lane:
 * `npm test` stays a second, and this is opt-in.
 *
 * NANOASR_E2E_URL points at an already-running server. Without it the suite
 * skips, so a checkout with no models does not fail.
 */
export default defineConfig({
  testDir: './e2e',
  timeout: 120_000,
  expect: { timeout: 60_000 },
  fullyParallel: false,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: process.env.NANOASR_E2E_URL ?? 'http://127.0.0.1:8080',
    trace: 'retain-on-failure',
    launchOptions: {
      // Chromium is preinstalled in the container image used for CI.
      ...(process.env.PLAYWRIGHT_CHROMIUM
        ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM }
        : {}),
    },
  },
})
