import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: ".",
  testMatch: "**/*.spec.ts",
  fullyParallel: false,
  retries: 0,
  workers: 1,
  reporter: [["list"], ["html", { outputFolder: "playwright-report", open: "never" }]],

  use: {
    baseURL: "http://127.0.0.1:18080",
    // Avoid leaking real browser state between tests.
    storageState: { cookies: [], origins: [] },
  },

  projects: [
    {
      name: "chromium",
      // Use the installed Google Chrome instead of Playwright's own Chromium
      // build, so there is no browser to download or keep in sync with the
      // @playwright/test version. Needs Chrome on the machine running the tests.
      use: { ...devices["Desktop Chrome"], channel: "chrome" },
    },
  ],

  // Start the Go devserver before running tests.
  webServer: {
    command: "go run ./devserver",
    url: "http://127.0.0.1:18080/healthz",
    reuseExistingServer: !process.env.CI,
    stdout: "pipe",
    stderr: "pipe",
    timeout: 180000,
  },
});
