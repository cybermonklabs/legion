/** The split of a sharded run by measured row time (`e2e/shard-split.ts`), as the placement
 *  itself and as `e2e/shard-reporter.ts` applies it to the real config.
 *
 *  The second half is what the shard job rests on. Each of its legs - as many as the shard job's
 *  `matrix` in .github/workflows/dispatch-browser.yaml names, which is where this file reads the
 *  count - computes the placement on its own machine, and a leg that placed a file differently
 *  from the others would run it twice or leave it to nobody. So each selection is listed whole
 *  and then leg by leg through the reporter, every leg, and two things are held: every row of the
 *  whole listing is on exactly one leg (`legRowFaults`, e2e/leg-rows.ts, which names each row that
 *  is on none or on two), and each leg holds exactly the rows the placement of the whole listing
 *  gives it. The first catches a reporter that drops or doubles a file whichever leg it is placed
 *  on; the second is what tells this split from Playwright's own, whose count split is a
 *  partition too.
 *
 *  The listings of a selection run one after the other, and the two selections side by side, so
 *  the file holds at most two at once: a listing of the full config is a `node` of about 300 to
 *  500 MiB, and this file once started ten at once, four of which a Legion pod's memory guard
 *  killed at 6.8 to 7.1 of its 8 GiB.
 */
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

import { type ListedRow, rowsOfListing } from "../configurations";
import { legRowFaults } from "../leg-rows";
import { referencesLaneFiles } from "../reference-lane";
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

/** The shard job's leg count, read from its `matrix` in .github/workflows/dispatch-browser.yaml,
 *  where the run's legs are written (.github/ci-notes/dispatch-browser.md, "Why the leg count
 *  stands in three places"). The listings below split each selection as CI's legs split it, so
 *  the count is CI's own: a number written here as well would be a fourth place for it, and a
 *  test that listed four legs while CI ran two would hold a placement no leg ever computes. The
 *  legs must be numbered 1 to N, as `--shard=N/M` numbers them and e2e/shard-reporter.ts keeps
 *  leg N of M; anything else is refused by name rather than listed. */
function shardLegs(): number {
  const workflow = Bun.YAML.parse(
    readFileSync(
      new URL("../../../../.github/workflows/dispatch-browser.yaml", import.meta.url),
      "utf8"
    )
  ) as { jobs?: { shard?: { strategy?: { matrix?: { shard?: unknown } } } } } | null;
  const matrix = workflow?.jobs?.shard?.strategy?.matrix?.shard;
  if (
    !Array.isArray(matrix) ||
    matrix.length === 0 ||
    matrix.some((leg, index) => leg !== index + 1)
  ) {
    throw new Error(
      ".github/workflows/dispatch-browser.yaml's shard job does not name its legs 1 to N in " +
        `jobs.shard.strategy.matrix.shard: ${JSON.stringify(matrix)}`
    );
  }
  return matrix.length;
}

const legs = shardLegs();
const rowKey = (row: ListedRow) => `${row.project} › ${row.file} › ${row.title}`;

/** One selection listed whole and then each of its legs through the reporter, one listing after
 *  another. */
async function listedLegs(selection: readonly string[]) {
  const whole = await listing(import.meta.url, "e2e/playwright.config.ts", {}, selection);
  const sharded = [];
  for (let leg = 1; leg <= legs; leg += 1) {
    sharded.push(
      await listing(
        import.meta.url,
        "e2e/playwright.config.ts",
        {},
        [...selection, `--shard=${leg}/${legs}`],
        ["./e2e/shard-reporter.ts"]
      )
    );
  }
  return { sharded, whole };
}

/** Both selections start here, before any test, so the file holds at most two listings at once. */
const selections = {
  "the default selection": retained(listedLegs([])),
  "the references lane": retained(
    listedLegs([...projectArguments(["chromium"]), ...referencesLaneFiles()])
  ),
};

for (const [name, listed] of Object.entries(selections)) {
  test(
    `the ${legs} legs of ${name} hold each of its rows once, each leg the rows the placement gives it`,
    async () => {
      const { sharded, whole: listedWhole } = await listed;
      const whole = rowsOfListing(listedWhole);
      const perLeg = sharded.map((leg) => rowsOfListing(leg).map(rowKey).sort());
      expect(whole.length).toBeGreaterThan(0);
      // Every row on exactly one leg, the faults naming each row that is not.
      expect(
        legRowFaults(
          perLeg.map((ran, index) => ({
            leg: `shard-${index + 1}`,
            ran,
            selection: whole.map(rowKey),
          }))
        ),
        `the ${legs} legs of ${name} against its whole listing`
      ).toEqual([]);
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
      const unplaced = [...units.keys()].filter((unit) => !legOf.has(unit));
      expect(unplaced, `files of ${name} the placement puts on no leg`).toEqual([]);
      for (let leg = 1; leg <= legs; leg += 1) {
        const expected = whole
          .filter((row) => legOf.get(`${row.project}\n${row.file}`) === leg)
          .map(rowKey)
          .sort();
        expect(perLeg[leg - 1], `leg ${leg} of ${name} against its placement`).toEqual(expected);
      }
    },
    // One listing of the whole selection, then one for each leg.
    rowLimit(...Array.from({ length: legs + 1 }, () => "listing" as const))
  );
}
