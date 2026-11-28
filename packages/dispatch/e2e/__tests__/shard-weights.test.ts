/** What `e2e/tools/shard-weights.ts` reads out of a merged report and writes into the table
 *  `e2e/shard-split.ts` places rows by. */
import { expect, test } from "bun:test";

import { titleSeparator } from "../configurations";
import { rowTimes, weightsOf } from "../tools/shard-weights";

/** A report of one file, `a.e2e.ts`, whose rows ran in `chromium` with the durations given, in
 *  milliseconds, one attempt per entry. */
function report(rows: Record<string, readonly number[]>, project = "chromium") {
  return {
    suites: [
      {
        specs: Object.entries(rows).map(([title, attempts]) => ({
          file: "a.e2e.ts",
          tests: [
            {
              projectName: project,
              results: attempts.map((duration) => ({ duration, status: "passed" })),
            },
          ],
          title,
        })),
        suites: [
          {
            specs: [
              {
                file: "a.e2e.ts",
                tests: [{ projectName: project, results: [{ duration: 0, status: "skipped" }] }],
                title: "a skipped row",
              },
            ],
            title: "a describe",
          },
        ],
        title: "a.e2e.ts",
      },
    ],
  };
}

test("a row's time is every attempt it took, and a row that never ran is no row", () => {
  const rows = rowTimes(report({ "one attempt": [2000], "a retry": [3000, 1500] }));
  expect([...rows.entries()]).toEqual([
    [JSON.stringify(["chromium", "a.e2e.ts", "one attempt"]), 2],
    [JSON.stringify(["chromium", "a.e2e.ts", "a retry"]), 4.5],
  ]);
});

test("a row is its median over the reports, a file the mean of its rows, a project the median", () => {
  const runs = [
    { first: [1000], second: [4000] },
    { first: [2000], second: [6000] },
    { first: [9000], second: [5000] },
  ].map((rows, index) => ({ commit: `c${index}`, rows: rowTimes(report(rows)), run: index }));
  const weights = weightsOf(
    runs,
    "2026-11-22T00:00:00.000Z",
    "bun e2e/tools/shard-weights.ts --main 3"
  );
  // first: median of 1, 2 and 9 is 2; second: median of 4, 6 and 5 is 5. The file is their mean,
  // 3.5, and the project their median, the same 3.5. One slow run moved neither row.
  expect(weights.rows).toEqual({ chromium: { "a.e2e.ts": { first: 2, second: 5 } } });
  expect(weights.files).toEqual({ chromium: { "a.e2e.ts": 3.5 } });
  expect(weights.projects).toEqual({ chromium: 3.5 });
  expect(weights.source.runs).toEqual([
    { commit: "c0", run: 0 },
    { commit: "c1", run: 1 },
    { commit: "c2", run: 2 },
  ]);
});

test("a row inside a describe is keyed by its titles below the file, as a listing names it", () => {
  const nested = {
    suites: [
      {
        suites: [
          {
            specs: [
              {
                file: "a.e2e.ts",
                tests: [
                  { projectName: "chromium", results: [{ duration: 1500, status: "passed" }] },
                ],
                title: "a row",
              },
            ],
            title: "a describe",
          },
        ],
        title: "a.e2e.ts",
      },
    ],
  };
  const weights = weightsOf(
    [{ commit: "c0", rows: rowTimes(nested), run: 0 }],
    "2026-11-27T00:00:00.000Z",
    "bun e2e/tools/shard-weights.ts --main 1"
  );
  expect(weights.rows).toEqual({ chromium: { "a.e2e.ts": { "a describe › a row": 1.5 } } });
});

test("a row's titles stay whole in its key: a newline in a row's or a describe's title, and a describe with no title", () => {
  /** A spec of `a.e2e.ts` titled `title` whose one attempt in `chromium` took `duration` ms. */
  const spec = (title: string, duration: number) => ({
    file: "a.e2e.ts",
    tests: [{ projectName: "chromium", results: [{ duration, status: "passed" }] }],
    title,
  });
  const titled = {
    suites: [
      {
        specs: [spec("a row of\ntwo lines", 1000)],
        suites: [
          { specs: [spec("a row", 2000)], title: "a describe of\ntwo lines" },
          { specs: [spec("a row", 3000)], title: "" },
        ],
        title: "a.e2e.ts",
      },
    ],
  };
  const weights = weightsOf(
    [{ commit: "c0", rows: rowTimes(titled), run: 0 }],
    "2026-11-28T00:00:00.000Z",
    "bun e2e/tools/shard-weights.ts --main 1"
  );
  // As a listing names them (`rowsOfListing`, e2e/configurations.ts): each title whole, and a
  // `describe` with no title as an empty one before the separator.
  expect(weights.rows).toEqual({
    chromium: {
      "a.e2e.ts": {
        [`a describe of\ntwo lines${titleSeparator}a row`]: 2,
        "a row of\ntwo lines": 1,
        [`${titleSeparator}a row`]: 3,
      },
    },
  });
});
