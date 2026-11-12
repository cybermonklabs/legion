/** The planted controls of e2e/__tests__/row-limits.test.ts: one wall-clock limit of every shape
 *  the rule has an answer for, so proving the check looks needs no edit of a real test file.
 *  Without them a check that finds nothing in the directory would read the same whether it
 *  looked or not.
 *
 * Its name ends `.fixture.ts` because `bun test` collects `*.test.ts` and its kin: this file runs
 * nowhere, and the rows and hooks below are source for the scan to parse rather than work for a
 * runner to do. `testFiles` skips this whole directory for the same reason - every limit below
 * but two is written to be refused, and the rule is about the directory's own tests.
 *
 * The shared `rowLimit` is imported under a name no test file uses, because the rule reads the
 * IMPORT and not the word: a call of `boundedBy` below is a limit read from the bounds, and this
 * file's own `rowLimit`, which comes from nowhere near e2e/__tests__/bounded-subprocess.ts, is
 * not.
 *
 * `minimumReasonWords` stands below for a limit named by an import. What that export means where
 * it is declared has nothing to do with this file: the control is about the spelling of the
 * limit, never about its value, and none of these rows runs.
 *
 * A row states its options after its body here, and never before it. Bun reads `test(title,
 * options, body)` too, and the rule reads that form, but `@types/bun` 1.4.2 declares only
 * `test(title, body, options)`: a control of the other form would not compile, and this file
 * compiles with the rest of the suite. The hook below carries its options as an object, where
 * the installed types do allow it.
 *
 * The lines are asserted in the test, so a limit added here goes at the END of the file.
 */

import { afterAll, beforeAll, it, setDefaultTimeout, test } from "bun:test";

import { minimumReasonWords } from "../../fixed-waits";
import { rowLimit as boundedBy } from "../bounded-subprocess";

/** A `rowLimit` of this file's own, so the control below calls one the rule must refuse. */
function rowLimit(milliseconds: number): number {
  return milliseconds;
}

/** A limit this file binds a name to, which is how this directory wrote its limits before they
 *  were read from the bounds of the children a row waits for. */
const planted = 90_000;

setDefaultTimeout(90_000);

test("a row whose limit is a literal", () => {}, 90_000);

test("a row whose limit is a name this file binds", () => {}, planted);

test("a row whose limit is a name this file imports", () => {}, minimumReasonWords);

test("a row whose limit is a timeout option", () => {}, { timeout: 90_000 });

it("a row declared by `it`", () => {}, 90_000);

test.skipIf(false)("a row declared through `skipIf`", () => {}, 90_000);

test.each([1])("a row declared through `each`", () => {}, 90_000);

beforeAll(async () => {}, 60_000);

afterAll(async () => {}, { timeout: 60_000 });

// too short
test("a row under a reason of too few words", () => {}, 90_000);

// A reason far enough above the limit that the next reader of the limit does not see it, which
// is the whole of what this half of the rule is about.

test("a row under a reason a blank line carried away", () => {}, 90_000);

test("a row whose reason trails its limit", () => {}, 90_000); // A reason the rule never reads.

test("a row whose rowLimit is this file's own", () => {}, rowLimit(90_000));

// This row starts a server of its own and stops it itself, so the minute here is what fails a
// stop that never returns rather than a bound on a child nobody started.
test("a row under a reason of its own", () => {}, 90_000);

test("a row whose limit is read from the bounds", () => {}, boundedBy("short"));

test.skip("a row declared through `skip`", () => {}, 90_000);
