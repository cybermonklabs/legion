import { expect, mock, test } from "bun:test";
import * as timers from "node:timers/promises";

import type { Page, Route } from "@playwright/test";

// The picker rows read each save's hold with `Date.now()` and assert it lasted at least
// `PICK_HOLD_MS`, and `pick-writes.ts` holds a save with `node:timers/promises`' `setTimeout`.
// Node's timer can end up to 1 ms early: a probe on the session image
// (dispatch://INFRA-983/comment/74b7abbc-95c8-4815-99b6-8b4d37d273c9) measured Node 24's
// `sleep(3000)` ending 2,999 ms later by `Date.now()` in 13 of 200 runs, and the shortest lasted
// 2,999.250 ms by `performance.now()`, so the timer ended early on the monotonic clock too; Bun
// 1.4.4 ended none of 200 early. The wall clock moving against the monotonic one a timer counts
// on, which a runner may slew or step, is a second cause of the same short read. Merge-group runs
// 44424277368, 44632529216 and 44650607607 read a hold of 2,999 ms and failed board.e2e.ts's
// lane-drag row on its first attempt. Here `pick-writes.ts` is handed a `setTimeout` that ends
// 1 ms before its delay, as Node's did, so a hold that only sleeps `PICK_HOLD_MS` reads 2,999 ms
// every time: the hold must last until `Date.now()` itself has moved that far. The row waits real
// time, since the timer is the thing under test: about 3 s.

/** How much of a delay `endsEarly` leaves to its loop on `Date.now()` rather than to its timer:
 *  enough for a timer that fires late on a loaded machine still to fire before the delay ends. */
const SPIN_MS = 50;

/** `node:timers/promises`' `setTimeout` as this file hands it to `pick-writes.ts`: it resolves
 *  1 ms before its delay by `Date.now()`, the clock the rows read a hold with. A real timer takes
 *  most of the delay, and the last `SPIN_MS` of it are read off `Date.now()` in a loop, so the
 *  sleep ends on the last millisecond before its delay rather than wherever a timer fires. */
function endsEarly<T>(delay = 1, value?: T): Promise<T | undefined> {
  const end = Date.now() + delay - 1;
  const { promise, resolve } = Promise.withResolvers<T | undefined>();
  setTimeout(
    () => {
      while (Date.now() < end) {
        // The last of the delay, read on the clock the hold is read with.
      }
      resolve(value);
    },
    Math.max(0, delay - 1 - SPIN_MS)
  );
  return promise;
}

/** The module's own exports, copied before the mock below replaces one of them. */
const ownTimers = { ...timers };

// Only `setTimeout` is replaced: every other export of the module stays the module's own.
mock.module("node:timers/promises", () => ({ ...ownTimers, setTimeout: endsEarly }));
// Imported after the mock, so the `sleep` `pick-writes.ts` binds is `endsEarly`: a static import
// is evaluated before any line of this file runs, and would bind the module's own timer.
const { holdIssuePatches, PICK_HOLD_MS } = await import("../pick-writes");

test("a held save lasts PICK_HOLD_MS by Date.now(), though its timer ends 1 ms early", async () => {
  let handler: ((route: Route) => Promise<void>) | undefined;
  const page = {
    route: async (_pattern: string, handle: (route: Route) => Promise<void>) => {
      handler = handle;
    },
  } as unknown as Page;
  let fallbacks = 0;
  const route = {
    fallback: async () => {
      fallbacks += 1;
    },
    request: () => ({ method: () => "PATCH", postDataJSON: () => ({ priority: 1 }) }),
  } as unknown as Route;

  const held = await holdIssuePatches(page, "CORE-1");
  held.release();
  if (handler === undefined) throw new Error("holdIssuePatches routed nothing");
  await handler(route);

  const [patch] = held.sent;
  expect(held.sent).toHaveLength(1);
  expect((patch?.endedAt ?? Number.NaN) - (patch?.arrivedAt ?? Number.NaN)).toBeGreaterThanOrEqual(
    PICK_HOLD_MS
  );
  expect(fallbacks).toBe(1);
  expect(held.forwarded).toEqual([{ priority: 1 }]);
  expect(held.holding()).toBe(0);
});
