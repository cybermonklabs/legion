/** The split of a sharded run by measured row time (`e2e/shard-split.ts`), as the placement
 *  itself and as `e2e/shard-reporter.ts` applies it to the real config.
 *
 *  The second half is what the shard job rests on. Each of its four legs computes the placement
 *  on its own machine, and a leg that placed a file differently from the others would run it
 *  twice or leave it to nobody, with nothing red: the merge job's row count reads what the legs
 *  ran, not what they should have run. So the config is listed whole and then once per leg
 *  through the reporter, and the legs must hold every row exactly once, each leg the very rows
 *  the placement gives it. That last comparison is the one that can tell this split from
 *  Playwright's own: a leg listed without the reporter holds the count split's rows, which are
 *  also a partition and which it does not match.
 */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { type ListedRow, pullRequestLaneProjects, rowsOfListing, tagged } from "../configurations";
import { placeUnits, rowSeconds, type ShardWeights, type Unit } from "../shard-split";
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

const legs = 4;
const rowKey = (row: ListedRow) => `${row.project}\n${row.file}\n${row.title}`;

/** The rows of one selection, listed whole and once per leg through the reporter. */
function listedLegs(selection: readonly string[]) {
  return {
    legs: Array.from({ length: legs }, (_, index) =>
      retained(
        listing(
          import.meta.url,
          "e2e/playwright.config.ts",
          {},
          [...selection, `--shard=${index + 1}/${legs}`],
          ["./e2e/shard-reporter.ts"]
        )
      )
    ),
    whole: retained(listing(import.meta.url, "e2e/playwright.config.ts", {}, selection)),
  };
}

/** Every listing starts here, before any test, so they collect at once. */
const selections = {
  "the default selection": listedLegs([]),
  "the pull-request lane": listedLegs([
    ...projectArguments(pullRequestLaneProjects),
    "--grep",
    tagged("pr").source,
  ]),
};

for (const [name, listings] of Object.entries(selections)) {
  test(
    `the four legs of ${name} hold every row once, each leg the rows the placement gives it`,
    async () => {
      const whole = rowsOfListing(await listings.whole);
      const perLeg = await Promise.all(
        listings.legs.map(async (leg) => rowsOfListing(await leg).map(rowKey))
      );
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
      const legOf = new Map(
        placeUnits([...units.values()], weights, legs).placed.map((unit) => [
          `${unit.project}\n${unit.file}`,
          unit.shard,
        ])
      );
      const expected = Array.from({ length: legs }, (_, index) =>
        whole
          .filter((row) => legOf.get(`${row.project}\n${row.file}`) === index + 1)
          .map(rowKey)
          .sort()
      );
      expect(whole.length).toBeGreaterThan(0);
      expect(perLeg.flat().sort()).toEqual(whole.map(rowKey).sort());
      expect(perLeg.map((rows) => [...rows].sort())).toEqual(expected);
    },
    rowLimit("listing")
  );
}
