import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 90_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:17700',
    trace: 'retain-on-failure',
  },
  // The accessibility audit runs as its own project per browser so that axe
  // never shares a browser process with the smoke test (DECISIONS D86).
  projects: [
    { name: 'chromium', testMatch: /smoke/, use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox', testMatch: /smoke/, use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit', testMatch: /smoke/, use: { ...devices['Desktop Safari'] } },
    { name: 'chromium-a11y', testMatch: /a11y/, use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox-a11y', testMatch: /a11y/, use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit-a11y', testMatch: /a11y/, use: { ...devices['Desktop Safari'] } },
  ],
  webServer: {
    command: 'node e2e/stack.mjs',
    url: 'http://127.0.0.1:17700/healthz',
    reuseExistingServer: false,
    timeout: 30_000,
  },
})
