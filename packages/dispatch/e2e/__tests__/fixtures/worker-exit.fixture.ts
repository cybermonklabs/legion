/** The one row of e2e/__tests__/playwright-worker-exit.test.ts's Playwright run
 *  (`worker-exit.config.ts`). It passes at once. With `WORKER_EXIT_HOLD_MAIN_MS` set, a
 *  worker-scoped fixture holds the worker's main thread that long in its teardown, which runs
 *  after the runner's stop: a worker that has not exited and sends no heartbeat, which the runner
 *  must still kill and report.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` so the suite's own config never runs it (see
 * `no-browser-row.fixture.ts`), and `bun test` collects only `*.test.ts`.
 */

import { test as base, expect } from "@playwright/test";

const test = base.extend<object, { holdMainThread: undefined }>({
  holdMainThread: [
    // Playwright refuses a first argument that is not a destructuring pattern.
    // biome-ignore lint/correctness/noEmptyPattern: Playwright reads the fixtures from it.
    async ({}, use) => {
      await use(undefined);
      const ms = Number(process.env.WORKER_EXIT_HOLD_MAIN_MS ?? 0);
      const end = Date.now() + ms;
      while (Date.now() < end) {
        // The hold is the point: a main thread that turns no event loop sends no heartbeat.
      }
    },
    { scope: "worker", auto: true },
  ],
});

test("a row that passes at once", async () => {
  expect(await Promise.resolve("passed")).toBe("passed");
});
