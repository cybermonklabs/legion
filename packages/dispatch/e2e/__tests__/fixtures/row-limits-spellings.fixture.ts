/** The planted controls of e2e/__tests__/row-limits.test.ts for the spellings of a wall-clock
 *  limit that `bun test` honours and `e2e/row-limits.ts` does not read.
 *
 * Measured in this issue's pod on 2026-11-12 with bun 1.4.4, each against a row that sleeps
 * 400 ms:
 *
 * - `jest.setTimeout(50)` stopped the row with "this test timed out after 50ms".
 *   `jest.setTimeout` is declared in bun-types 1.4.2 (`test.d.ts`), and the bun suites of this
 *   repository write `jest.` in dozens of files, so it is a spelling a writer here reaches for.
 * - `import { setDefaultTimeout as cap } from "bun:test"` with `cap(50)` stopped it the same way.
 * - `import { test as row } from "bun:test"` with `row(title, body, 9_000)` passed under that
 *   same `cap(50)`, so the third argument of a renamed `test` is a limit Bun reads too.
 *
 * The rule reads `setDefaultTimeout` as a bare name, and a row from the names `test` and `it`
 * alone, so it reads none of the three - while the header of `e2e/row-limits.ts` says its four
 * places are "every place `bun test` reads one". A limit the rule does not read is a limit
 * outside it, which is what its own header says about `setDefaultTimeout`.
 *
 * Its name ends `.fixture.ts` because `bun test` collects `*.test.ts` and its kin: nothing here
 * runs, and the three limits below are source for the scan to parse.
 */

import { setDefaultTimeout as cap, jest, test as row } from "bun:test";

jest.setTimeout(90_000);

row("a row declared through a renamed import of `test`", () => {}, 90_000);

cap(90_000);
