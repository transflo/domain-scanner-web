import { defineConfig, devices } from "@playwright/test"

// UI checks across screen sizes and browser engines. Run against a live stack:
//   E2E_BASE_URL=http://localhost:3000 E2E_PASSWORD=<admin password> pnpm e2e
// Mobile and tablet profiles use Chromium's device emulation (the pragmatic default); Firefox and
// WebKit projects add the other two real engines.

const chromium = <T extends object>(d: T) => ({ ...d, defaultBrowserType: "chromium" as const })

const emulated = {
  "desktop-1920": { ...devices["Desktop Chrome"], viewport: { width: 1920, height: 1080 } },
  "desktop-1440": { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
  "laptop-1024": { ...devices["Desktop Chrome"], viewport: { width: 1024, height: 768 } },
  "ipad-portrait": chromium(devices["iPad (gen 7)"]),
  "ipad-landscape": chromium(devices["iPad (gen 7) landscape"]),
  "iphone-se": chromium(devices["iPhone SE"]),
  "iphone-14-pro-max": chromium(devices["iPhone 14 Pro Max"]),
  "pixel-7": devices["Pixel 7"],
  "galaxy-s9-plus": chromium(devices["Galaxy S9+"]),
  "small-android-320": {
    ...devices["Pixel 7"],
    viewport: { width: 320, height: 568 },
    screen: { width: 320, height: 568 },
  },
} as const

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./e2e-results",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: true,
  workers: 4,
  retries: 1,
  reporter: [["list"], ["html", { open: "never", outputFolder: "e2e-report" }]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:3000",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    { name: "setup", testMatch: /auth\.setup\.ts/ },
    ...Object.entries(emulated).map(([name, use]) => ({
      name,
      use: { ...use, storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
      testIgnore: /auth\.setup\.ts/,
    })),
    {
      name: "firefox-desktop",
      use: { ...devices["Desktop Firefox"], viewport: { width: 1440, height: 900 }, storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
    },
    {
      name: "firefox-narrow",
      use: { ...devices["Desktop Firefox"], viewport: { width: 390, height: 844 }, hasTouch: true, storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
    },
    {
      name: "webkit-desktop",
      use: { ...devices["Desktop Safari"], viewport: { width: 1440, height: 900 }, storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
    },
    {
      name: "webkit-iphone-13",
      use: { ...devices["iPhone 13"], storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
    },
    {
      name: "webkit-ipad",
      use: { ...devices["iPad Pro 11"], storageState: "e2e/.auth/state.json" },
      dependencies: ["setup"],
    },
  ],
})
