/** What `e2e/tools/shard-weights.ts` reads out of a merged report and writes into the table
 *  `e2e/shard-split.ts` places files by. */
import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";

import { admittedRows, configuredProjects, rowTimes, weightsOf } from "../tools/shard-weights";

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
    ["chromium\na.e2e.ts\none attempt", 2],
    ["chromium\na.e2e.ts\na retry", 4.5],
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
  expect(weights.files).toEqual({ chromium: { "a.e2e.ts": 3.5 } });
  expect(weights.projects).toEqual({ chromium: 3.5 });
  expect(weights.source.runs).toEqual([
    { commit: "c0", run: 0 },
    { commit: "c1", run: 1 },
    { commit: "c2", run: 2 },
  ]);
});

test("the table this checkout commits names only projects its config names and specs it holds", () => {
  // The control of the tool's filter: read with this checkout's own config and e2e/, every row of
  // the committed table would be written again, so the filter leaves out no row of a real run.
  const e2e = new URL("..", import.meta.url);
  const projects = configuredProjects(readFileSync(new URL("playwright.config.ts", e2e), "utf8"));
  const table = JSON.parse(readFileSync(new URL("shard-weights.json", e2e), "utf8")) as {
    files: Record<string, Record<string, number>>;
  };
  const rows = new Map(
    Object.entries(table.files).flatMap(([project, files]) =>
      Object.keys(files).map((file) => [`${project}\n${file}\na row`, 1] as const)
    )
  );
  expect(rows.size).toBeGreaterThan(0);
  const { refused } = admittedRows(rows, projects, (file) => existsSync(new URL(file, e2e)));
  expect(refused).toEqual([]);
});

test("a row is written only for a configured project and a spec of e2e/ by its bare name", () => {
  const rows = new Map([
    ["chromium\na.e2e.ts\nkept", 1],
    ["chromium\n../a.e2e.ts\na path out of e2e/", 1],
    ["chromium\nb.e2e.ts\na spec this checkout lacks", 1],
    ["firefox\na.e2e.ts\na project the config does not name", 1],
    ["chromium\na.spec.ts\nno spec's shape", 1],
  ]);
  const { admitted, refused } = admittedRows(
    rows,
    new Set(["chromium"]),
    (file) => file === "a.e2e.ts"
  );
  expect([...admitted.keys()]).toEqual(["chromium\na.e2e.ts\nkept"]);
  expect(refused).toHaveLength(4);
});
