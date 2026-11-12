/** The planted controls of e2e/__tests__/row-limits.test.ts for the spellings of a wall-clock
 *  limit `bun test` honours beyond a bare `test`, `it`, hook and `setDefaultTimeout`. The rule
 *  reads each limit below, and the test asserts that each is refused on its own line.
 *
 * Measured in this issue's pod on 2026-11-12 with bun 1.4.4, each against a row or hook that
 * sleeps 400 ms under `bun test --timeout 60000`:
 *
 * - `jest.setTimeout(50)`, and `cap(50)` under `import { setDefaultTimeout as cap }`, stopped the
 *   row with "this test timed out after 50ms". `row(title, body, 9_000)` under `import { test as
 *   row }` passed under that same `cap(50)`, so a renamed `test` takes its limit too.
 * - `onTestFinished(fn, 50)` stopped its row at 50 ms, and so did `test(title, options, body)`
 *   with `options` a name bound to `{ timeout: 50 }`, a row called through a `const` bound to
 *   `test`, and rows reached through `const { test: t2 } = await import("bun:test")`, through a
 *   name bound to `await import("bun:test")` and through `require("bun:test")`.
 *
 * `@types/bun` 1.4.2 declares no `test(title, options, body)`, so that one row carries a
 * `@ts-expect-error`, which the rule reads as a reason too short to explain the limit.
 *
 * Its name ends `.fixture.ts` because `bun test` collects `*.test.ts` and its kin: nothing here
 * runs, and the limits below are source for the scan to parse. The lines are asserted in the
 * test, so a limit added here goes at the END of the file.
 */

import { setDefaultTimeout as cap, jest, onTestFinished, test as row } from "bun:test";

jest.setTimeout(90_000);

row("a row declared through a renamed import of `test`", () => {}, 90_000);

cap(90_000);

row("a row whose body registers a hook of its own", () => {
  onTestFinished(() => {}, 90_000);
});

const options = { timeout: 90_000 };
// @ts-expect-error: options before the body
row("a row whose options are a name", options, () => {});

const bound = row;
const boundAgain = bound;
boundAgain("a row declared through a name bound to a name bound to `test`", () => {}, 90_000);

const { test: destructured } = await import("bun:test");
destructured("a row declared through a name destructured off the module", () => {}, 90_000);

const loaded = await import("bun:test");
loaded.test("a row declared through a name bound to the module", () => {}, 90_000);

const required = require("bun:test");
required.test("a row declared through the module `require` loads", () => {}, 90_000);
