import { defineConfig, devices } from "@playwright/test";

// The journey runs against a built, seeded instance (CI starts one with docker
// compose and `babki seed`); BASE_URL names it.
export default defineConfig({
  testDir: "e2e",
  timeout: 60_000,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: process.env.BASE_URL ?? "http://localhost:8080",
    locale: "ru-RU",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
