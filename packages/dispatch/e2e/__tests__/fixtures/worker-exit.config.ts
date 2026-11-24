/** The Playwright run of e2e/__tests__/playwright-worker-exit.test.ts: one worker, the one row of
 *  `worker-exit.fixture.ts`, no browser and no stack, and its output directory where the test says
 *  (`WORKER_EXIT_OUTPUT`), so the test can read what the run's runner left there. */
import { fileURLToPath } from "node:url";
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: fileURLToPath(new URL(".", import.meta.url)),
  testMatch: /worker-exit\.fixture\.ts$/,
  outputDir: process.env.WORKER_EXIT_OUTPUT,
  workers: 1,
  retries: 0,
  reporter: "list",
});
