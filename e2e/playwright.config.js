// @ts-check
const { defineConfig, devices } = require('@playwright/test');

// The suite drives the real UI against cmd/demo: the real engine, real Noise
// sessions and real bbolt, with scripted peers over an in-memory discovery hub.
// That means no multicast is needed, so it runs the same on a laptop and in CI.
const PORT = process.env.CHIRP_E2E_PORT || '7788';

module.exports = defineConfig({
  // The screenshot capture lives beside the suite but is not part of it; it
  // runs through `make screenshots`, which points testDir at this file.
  testDir: process.env.CHIRP_SHOTS ? '.' : './tests',
  testMatch: process.env.CHIRP_SHOTS ? 'screenshots.spec.js' : undefined,
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } },
    { name: 'phone', use: { ...devices['Pixel 7'] } },
  ],
  webServer: {
    command: `../bin/chirp-demo -http 127.0.0.1:${PORT} -name Tester`,
    url: `http://127.0.0.1:${PORT}/api/state`,
    // Never reuse a running demo. The UI is embedded in the binary with
    // go:embed, so a leftover server from an earlier build serves stale
    // markup and quietly makes the whole run meaningless - which is exactly
    // how a fixed contrast bug appeared to still be failing.
    reuseExistingServer: false,
    timeout: 30_000,
  },
});
