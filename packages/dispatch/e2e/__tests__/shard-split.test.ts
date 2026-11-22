/** The split of a sharded run by measured row time (`e2e/shard-split.ts`), as the placement
 *  itself and as `e2e/shard-reporter.ts` applies it to the real config.
 *
 *  The second half is what the shard job rests on. Each of its four legs computes the placement
 *  on its own machine, and a leg that placed a file differently from the others would run it
 *  twice or leave it to nobody, with nothing red: the merge job's row count reads what the legs
 *  ran, not what they should have run. So each selection is listed whole, and then one leg of it
 *  through the reporter, which must hold exactly the rows the placement of the whole listing
 *  gives that leg; the placement itself must put every file of the whole listing on one leg. That
 *  comparison is the one that can tell this split from Playwright's own: a leg listed without the
 *  reporter holds the count split's rows, which it does not match.
 *
 *  One real leg per selection, and the two listings of a selection one after the other: a listing
 *  of the full config is a `node` of about 440 to 500 MiB, and this file started ten at once
 *  before, four of which a Legion pod's memory guard killed at 6.8 to 7.1 of its 8 GiB. The legs
 *  not listed are the same function of the same listing, which the rows above hold for every
 *  order and tie.
 */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { type ListedRow, pullRequestLaneProjects, rowsOfListing, tagged } from "../configurations";
import {
  dependencyProjects,
  placeUnits,
  rowSeconds,
  type ShardWeights,
  tableWarnings,
  type Unit,
} from "../shard-split";
import { rowLimit } from "./bounded-subprocess";
import { listing, projectArguments, retained } from "./playwright-listing";

const weights = JSON.parse(
  readFileSync(new URL("../shard-weights.json", import.meta.url), "utf8")
) as ShardWeights;

/** A table of two projects, one with a measured file, for the placement's own rows. */
const table: ShardWeights = {
  files: { chromium: { "a.e2e.ts": 10 } },
  projects: { chromium: 2, iphone: 4, webkit: 9 },
  source: { command: "test", read: "test", runs: [] },
};

test("a file the table measured costs its rows times its row, and any other its project's median", () => {
  expect(rowSeconds({ file: "a.e2e.ts", project: "chromium" }, table)).toEqual({
    measured: true,
    seconds: 10,
  });
  expect(rowSeconds({ file: "new.e2e.ts", project: "chromium" }, table)).toEqual({
    measured: false,
    seconds: 2,
  });
  // A project the table has no row of takes the median of every project's median row.
  expect(rowSeconds({ file: "a.e2e.ts", project: "firefox" }, table)).toEqual({
    measured: false,
    seconds: 4,
  });
});

test("the heaviest file goes first, each onto the leg with the least time so far", () => {
  const units: Unit[] = [
    { file: "a.e2e.ts", project: "chromium", rows: 3 }, // 30 s
    { file: "b.e2e.ts", project: "chromium", rows: 5 }, // 10 s, by the project median
    { file: "c.e2e.ts", project: "iphone", rows: 5 }, // 20 s
    { file: "d.e2e.ts", project: "iphone", rows: 4 }, // 16 s
    { file: "e.e2e.ts", project: "iphone", rows: 0 }, // every row skipped: 0 s
  ];
  const { loads, placed } = placeUnits(units, table, 2);
  expect(placed.map((unit) => [unit.file, unit.seconds, unit.shard])).toEqual([
    ["a.e2e.ts", 30, 1],
    ["c.e2e.ts", 20, 2],
    ["d.e2e.ts", 16, 2],
    ["b.e2e.ts", 10, 1],
    ["e.e2e.ts", 0, 2],
  ]);
  expect(loads).toEqual([40, 36]);
});

test("the placement is the same whatever order the files come in, so every leg computes one", () => {
  const units: Unit[] = Array.from({ length: 40 }, (_, index) => ({
    file: `f${index % 13}.e2e.ts`,
    project: ["chromium", "iphone", "webkit"][index % 3] as string,
    rows: (index * 7) % 5,
  }));
  const key = (unit: Unit & { shard: number }) => `${unit.project}/${unit.file}:${unit.shard}`;
  const forward = placeUnits(units, table, 4).placed.map(key).sort();
  const backward = placeUnits([...units].reverse(), table, 4)
    .placed.map(key)
    .sort();
  expect(backward).toEqual(forward);
  expect(forward).toHaveLength(units.length);
});

test("a leg count that is no whole number above zero, or a table of no project, is refused", () => {
  expect(() => placeUnits([], table, 0)).toThrow("a whole number of legs");
  expect(() => placeUnits([], { ...table, projects: {} }, 4)).toThrow("holds no project");
});

test("a table more than seven days old, or a tenth of a leg's rows estimated, warns; a fresh one does not", () => {
  const now = new Date("2026-11-22T12:00:00.000Z");
  expect(tableWarnings("2026-11-15T12:00:00.000Z", now, 400, 39)).toEqual([]);
  const old = tableWarnings("2026-11-15T11:00:00.000Z", now, 400, 0);
  expect(old).toHaveLength(1);
  expect(old[0]).toContain("7.0 days ago");
  expect(old[0]).toContain("bun e2e/tools/shard-weights.ts --main 17");
  const estimated = tableWarnings("2026-11-22T00:00:00.000Z", now, 400, 40);
  expect(estimated).toHaveLength(1);
  expect(estimated[0]).toStartWith("40 of this leg's 400 rows");
  // A read the table does not hold as a date warns rather than passing as fresh.
  expect(tableWarnings("test", now, 400, 0)[0]).toContain("at no date it names");
});

test("a project another names as a dependency or teardown is a setup project, and no other", () => {
  expect(
    dependencyProjects([
      { dependencies: ["setup"], teardown: "cleanup" },
      { dependencies: [] },
      {},
      { dependencies: ["setup", "seed"] },
    ])
  ).toEqual(new Set(["setup", "cleanup", "seed"]));
  expect(dependencyProjects([{}, { dependencies: [] }])).toEqual(new Set());
});

const legs = 4;
const rowKey = (row: ListedRow) => `${row.project}\n${row.file}\n${row.title}`;

/** One selection listed whole and then, after that listing ends, its leg `leg` through the
 *  reporter: never two listings of one selection at once. */
async function listedLeg(selection: readonly string[], leg: number) {
  const whole = await listing(import.meta.url, "e2e/playwright.config.ts", {}, selection);
  const sharded = await listing(
    import.meta.url,
    "e2e/playwright.config.ts",
    {},
    [...selection, `--shard=${leg}/${legs}`],
    ["./e2e/shard-reporter.ts"]
  );
  return { leg, sharded, whole };
}

/** Both selections start here, before any test, so the file holds at most two listings at once.
 *  A different leg of each, so the reporter is read at more than one `shard.current`. */
const selections = {
  "the default selection": retained(listedLeg([], 2)),
  "the pull-request lane": retained(
    listedLeg([...projectArguments(pullRequestLaneProjects), "--grep", tagged("pr").source], 3)
  ),
};

for (const [name, listed] of Object.entries(selections)) {
  test(
    `a leg of ${name} holds the rows the placement gives it, and the placement puts every file on one leg`,
    async () => {
      const { leg, sharded, whole: listedWhole } = await listed;
      const whole = rowsOfListing(listedWhole);
      const units = new Map<string, Unit>();
      for (const row of whole) {
        const unit = units.get(`${row.project}\n${row.file}`) ?? {
          file: row.file,
          project: row.project,
          rows: 0,
        };
        units.set(`${row.project}\n${row.file}`, {
          ...unit,
          rows: unit.rows + (row.expectedStatus === "skipped" ? 0 : 1),
        });
      }
      const { placed } = placeUnits([...units.values()], weights, legs);
      const legOf = new Map(placed.map((unit) => [`${unit.project}\n${unit.file}`, unit.shard]));
      // Every file of the whole listing is placed once, on a leg of the four.
      expect(placed).toHaveLength(units.size);
      expect(legOf.size).toBe(units.size);
      expect([...legOf.values()].every((shard) => shard >= 1 && shard <= legs)).toBe(true);
      const expected = whole
        .filter((row) => legOf.get(`${row.project}\n${row.file}`) === leg)
        .map(rowKey)
        .sort();
      const actual = rowsOfListing(sharded).map(rowKey).sort();
      expect(whole.length).toBeGreaterThan(0);
      expect(actual.length).toBeGreaterThan(0);
      expect(actual.length).toBeLessThan(whole.length);
      expect(actual).toEqual(expected);
    },
    rowLimit("listing", "listing")
  );
}
