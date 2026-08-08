/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "./src") },
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: "./src/test-setup.ts",
    // A fixed timezone, so that anything rendering an instant as a wall clock
    // (formatDateTime — see src/lib/dates.ts) is testable against a written-out
    // string instead of against a second call to the very function under test.
    // Moscow because it is the audience's own zone and it has no daylight
    // saving, so the offset is +03:00 on every date a test can pick.
    env: { TZ: "Europe/Moscow" },
  },
});
