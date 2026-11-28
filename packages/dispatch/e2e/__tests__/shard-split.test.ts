/** The split of a sharded run by measured row time (`e2e/shard-split.ts`), as the placement
 *  itself and as `e2e/shard-reporter.ts` applies it to the real config and to a fixture config.
 *
 *  The second half is what the shard job rests on. Each of its legs computes the placement on its
 *  own machine, and a leg that placed a row differently from the others would run it twice or
 *  leave it to nobody. So each selection is listed whole and then leg by leg through the reporter,
 *  at the leg count the shard job runs (`shardLegs`), and two things are held: every row of the whole listing is on exactly
 *  one leg (`legRowFaults`, e2e/leg-rows.ts, which names each row that is on none or on two), and
 *  each leg holds exactly the rows the placement of the whole listing gives it. The first catches
 *  a reporter that drops or doubles a row whichever leg it is placed on; the second is what tells
 *  this split from Playwright's own, whose count split is a partition too. Every row of the real
 *  config is a unit of its own (`groupOf`, e2e/shard-reporter.ts), so its placement is computed
 *  here row by row; the fixture config holds the rows the reporter must keep together, which the
 *  real config has none of. That computation keys each row by its title in the listing, and the
 *  reporter by its own (`rowOf`, e2e/shard-reporter.ts, which `preprocess` places every row as),
 *  so a second fixture config holds the two keys against each other in shapes the real config has
 *  none of either: a `describe` with no title, one inside it, and a title that holds a newline.
 *  Its reporter writes each row as `rowOf` builds it, so the keys held there are the ones
 *  `preprocess` places by.
 *
 *  The listings of a selection run one after the other, and the two selections side by side, so
 *  the file holds at most two at once: a listing of the full config is a `node` of about 300 to
 *  500 MiB, and this file once started ten at once, four of which a Legion pod's memory guard
 *  killed at 6.8 to 7.1 of its 8 GiB. `bun test` runs the file's tests one at a time, in a random
 *  order drawn from its seed (packages/dispatch/bunfig.toml sets `randomize`), so a fixture's test
 *  may run while both selections are still listing. Each of the two fixture tests therefore waits
 *  for both selections to end, fulfilled or rejected, before it starts a listing of its own, and
 *  its own listings run one after the other. A selection that fails fails its own test and no
 *  fixture's.
 */
import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  type Listing,
  pullRequestLaneProjects,
  rowsOfListing,
  tagged,
  titleSeparator,
} from "../configurations";
import { legRowFaults } from "../leg-rows";
import {
  dependencyProjects,
  placeUnits,
  type Row,
  rowSeconds,
  type ShardWeights,
  tableWarnings,
  type Unit,
  unitKey,
} from "../shard-split";
import { rowLimit } from "./bounded-subprocess";
import { listing, projectArguments, retained } from "./playwright-listing";

const weights = JSON.parse(
  readFileSync(new URL("../shard-weights.json", import.meta.url), "utf8")
) as ShardWeights;

const browserWorkflowPath = fileURLToPath(
  new URL("../../../../.github/workflows/dispatch-browser.yaml", import.meta.url)
);

/** The part of `.github/workflows/dispatch-browser.yaml` the leg count is read from: a part the
 *  file lacks reads as undefined, which `shardLegs` names. */
type BrowserWorkflow = {
  jobs?: { shard?: { strategy?: { matrix?: Record<string, unknown> | null } } };
} | null;

/** How many legs the shard job runs, read from its matrix. That is the count only while the
 *  matrix holds the one key `shard` listing 1 to N in order: an `include`, an `exclude` or a
 *  second key changes how many legs GitHub starts without changing the list, and a list with a
 *  hole leaves rows to a leg no job runs, since `--shard=N/M` numbers the legs 1 to M. Any other
 *  matrix is refused here rather than proved. */
function shardLegs(workflow: BrowserWorkflow): number {
  const matrix = workflow?.jobs?.shard?.strategy?.matrix;
  if (typeof matrix !== "object" || matrix === null) {
    throw new Error(`no jobs.shard.strategy.matrix in ${browserWorkflowPath}`);
  }
  const keys = Object.keys(matrix);
  if (keys.length !== 1 || keys[0] !== "shard") {
    throw new Error(
      `the shard job's matrix in ${browserWorkflowPath} holds ${keys.join(", ")}, not the one key shard`
    );
  }
  const listed = matrix.shard;
  if (
    !Array.isArray(listed) ||
    listed.length === 0 ||
    listed.some((leg, index) => leg !== index + 1)
  ) {
    throw new Error(
      `the shard matrix of ${browserWorkflowPath} is ${JSON.stringify(listed)}, not 1 to N`
    );
  }
  return listed.length;
}

const browserWorkflow = Bun.YAML.parse(
  readFileSync(browserWorkflowPath, "utf8")
) as BrowserWorkflow;
const legs = shardLegs(browserWorkflow);

test("the leg count is the shard matrix's one list of 1 to N", () => {
  // The controls: each plants one fault in a parsed copy of the workflow. An `include` or a second
  // key changes how many legs GitHub starts without changing the list; a hole or an empty list
  // numbers the legs other than `--shard` does.
  const planted = (change: (matrix: Record<string, unknown>) => void) => () => {
    const copy = structuredClone(browserWorkflow);
    const matrix = copy?.jobs?.shard?.strategy?.matrix;
    if (typeof matrix !== "object" || matrix === null) throw new Error("no matrix to plant in");
    change(matrix);
    return shardLegs(copy);
  };
  expect(planted((matrix) => Object.assign(matrix, { include: [{ shard: legs + 1 }] }))).toThrow(
    "not the one key shard"
  );
  expect(planted((matrix) => Object.assign(matrix, { browser: ["chromium", "webkit"] }))).toThrow(
    "not the one key shard"
  );
  expect(planted((matrix) => Object.assign(matrix, { shard: [1, 3] }))).toThrow("not 1 to N");
  expect(planted((matrix) => Object.assign(matrix, { shard: [] }))).toThrow("not 1 to N");
});

/** A table of three projects, one with a measured file and one measured row in it, for the
 *  placement's own rows. */
const table: ShardWeights = {
  files: { chromium: { "a.e2e.ts": 10 } },
  projects: { chromium: 2, iphone: 4, webkit: 9 },
  rows: { chromium: { "a.e2e.ts": { "a slow row": 30 } } },
  source: { command: "test", read: "test", runs: [] },
};

/** `row` as a unit of its own, keyed as the reporter keys one. */
function rowUnit(row: Row): Unit {
  return { key: unitKey("row", row.project, row.file, 0, [row.title]), rows: [row] };
}

/** `count` rows of `file` in `project`, each its own unit, titled `<prefix> <n>`. */
function rowUnits(project: string, file: string, prefix: string, count: number): Unit[] {
  return Array.from({ length: count }, (_, index) =>
    rowUnit({ file, project, runs: true, title: `${prefix} ${index + 1}` })
  );
}

test("a row the table holds costs its own time, else its file's mean row, else its project's median", () => {
  expect(rowSeconds({ file: "a.e2e.ts", project: "chromium", title: "a slow row" }, table)).toEqual(
    { by: "row", seconds: 30 }
  );
  expect(rowSeconds({ file: "a.e2e.ts", project: "chromium", title: "a new row" }, table)).toEqual({
    by: "file",
    seconds: 10,
  });
  expect(rowSeconds({ file: "new.e2e.ts", project: "chromium", title: "a row" }, table)).toEqual({
    by: "project",
    seconds: 2,
  });
  // A project the table has no row of takes the median of every project's median row.
  expect(rowSeconds({ file: "a.e2e.ts", project: "firefox", title: "a slow row" }, table)).toEqual({
    by: "project",
    seconds: 4,
  });
});

test("the heaviest unit goes first, each onto the leg with the least time so far", () => {
  const skipped: Row = { file: "e.e2e.ts", project: "iphone", runs: false, title: "skipped" };
  const units: Unit[] = [
    // 30 s: the row the table holds.
    {
      key: "a",
      rows: [{ file: "a.e2e.ts", project: "chromium", runs: true, title: "a slow row" }],
    },
    // 20 s: a describe of two rows the table does not hold, in its measured file.
    {
      key: "b",
      rows: [
        { file: "a.e2e.ts", project: "chromium", runs: true, title: "kept › one" },
        { file: "a.e2e.ts", project: "chromium", runs: true, title: "kept › two" },
      ],
    },
    // 16 s: four rows of a file the table has no entry for, by the project median.
    {
      key: "c",
      rows: Array.from({ length: 4 }, (_, index) => ({
        file: "d.e2e.ts",
        project: "iphone",
        runs: true,
        title: `row ${index}`,
      })),
    },
    // 0 s: a row skipped where it is declared weighs nothing and is still placed.
    { key: "d", rows: [skipped] },
  ];
  const { loads, placed } = placeUnits(units, table, 2);
  expect(placed.map((unit) => [unit.key, unit.seconds, unit.estimated, unit.shard])).toEqual([
    ["a", 30, 0, 1],
    ["b", 20, 0, 2],
    ["c", 16, 4, 2],
    ["d", 0, 0, 1],
  ]);
  expect(loads).toEqual([30, 36]);
});

test("a file's rows the table has no entry for spread over every leg, not onto one", () => {
  // The shape that set the legs apart on 2026-11-26: a new file of many rows, each placed by its
  // project's median, beside measured rows that already fill the legs evenly.
  const measured: Unit[] = [1, 2, 3, 4].map((leg) => ({
    key: `measured ${leg}`,
    rows: [{ file: "a.e2e.ts", project: "chromium", runs: true, title: "a slow row" }],
  }));
  const fresh = rowUnits("chromium", "new.e2e.ts", "new row", 36);
  const { placed } = placeUnits([...measured, ...fresh], table, 4);
  const perLeg = [1, 2, 3, 4].map(
    (leg) =>
      placed.filter(
        (unit) => unit.shard === leg && unit.rows.some((row) => row.file === "new.e2e.ts")
      ).length
  );
  expect(perLeg).toEqual([9, 9, 9, 9]);
});

test("the placement is the same whatever order the units come in, so every leg computes one", () => {
  const units: Unit[] = [
    ...rowUnits("chromium", "a.e2e.ts", "row", 13),
    ...rowUnits("iphone", "b.e2e.ts", "row", 11),
    ...rowUnits("webkit", "c.e2e.ts", "row", 16),
  ];
  const key = (unit: Unit & { shard: number }) => `${unit.key}:${unit.shard}`;
  const forward = placeUnits(units, table, 4).placed.map(key).sort();
  const backward = placeUnits([...units].reverse(), table, 4)
    .placed.map(key)
    .sort();
  expect(backward).toEqual(forward);
  expect(forward).toHaveLength(units.length);
});

test("a unit's key tells a describe from a row of its title, and one copy of a row from another", () => {
  const keys = [
    unitKey("row", "chromium", "a.e2e.ts", 0, ["sign-in"]),
    unitKey("describe", "chromium", "a.e2e.ts", 0, ["sign-in"]),
    unitKey("row", "chromium", "a.e2e.ts", 1, ["sign-in"]),
    unitKey("file", "chromium", "a.e2e.ts", 0, []),
  ];
  expect(new Set(keys).size).toBe(keys.length);
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

const rowKey = (row: Pick<Row, "project" | "file" | "title">) =>
  `${row.project} › ${row.file} › ${row.title}`;

/** One selection of `config` listed whole and then each of its legs through the reporter, one
 *  listing after another. */
async function listedLegs(config: string, selection: readonly string[]) {
  const whole = await listing(import.meta.url, config, {}, selection);
  const sharded: Listing[] = [];
  for (let leg = 1; leg <= legs; leg += 1) {
    sharded.push(
      await listing(
        import.meta.url,
        config,
        {},
        [...selection, `--shard=${leg}/${legs}`],
        ["./e2e/shard-reporter.ts"]
      )
    );
  }
  return { sharded, whole };
}

/** Each leg's rows as listed, and by `rowKey`, sorted; and the faults of the legs against the
 *  whole listing, which a partition of it has none of. */
function legsOf(listed: { sharded: readonly Listing[]; whole: Listing }) {
  const whole = rowsOfListing(listed.whole);
  const selection = whole.map(rowKey);
  const legRows = listed.sharded.map((leg) => rowsOfListing(leg));
  const perLeg = legRows.map((rows) => rows.map(rowKey).sort());
  const faults = legRowFaults(
    perLeg.map((ran, index) => ({ leg: `shard-${index + 1}`, ran, selection }))
  );
  return { faults, legRows, perLeg, whole };
}

/** Both selections start here, before any test, and run side by side; the fixture tests list
 *  only once both have ended, so the file holds at most two listings at once. */
const selections = {
  "the default selection": retained(listedLegs("e2e/playwright.config.ts", [])),
  "the pull-request lane": retained(
    listedLegs("e2e/playwright.config.ts", [
      ...projectArguments(pullRequestLaneProjects),
      "--grep",
      tagged("pr").source,
    ])
  ),
};

for (const [name, listed] of Object.entries(selections)) {
  test(
    `the ${legs} legs of ${name} hold each of its rows once, each leg the rows the placement gives it`,
    async () => {
      const { faults, perLeg, whole } = legsOf(await listed);
      expect(whole.length).toBeGreaterThan(0);
      // Every row on exactly one leg, the faults naming each row that is not.
      expect(faults, `the ${legs} legs of ${name} against its whole listing`).toEqual([]);
      const { placed } = placeUnits(
        whole.map((row) =>
          rowUnit({
            file: row.file,
            project: row.project,
            runs: row.expectedStatus !== "skipped",
            title: row.title,
          })
        ),
        weights,
        legs
      );
      for (let leg = 1; leg <= legs; leg += 1) {
        const expected = placed
          .filter((unit) => unit.shard === leg)
          .flatMap((unit) => unit.rows)
          .map(rowKey)
          .sort();
        expect(perLeg[leg - 1], `leg ${leg} of ${name} against its placement`).toEqual(expected);
      }
    },
    rowLimit(...Array.from({ length: legs + 1 }, () => "listing" as const))
  );
}

test(
  "a serial describe, a default-mode describe and a file of a project that is not fully parallel each stay on one leg, two serial describes of one title are placed apart, and free rows spread",
  async () => {
    // The full config's listings end first, whichever way (the file's header says why).
    await Promise.allSettled(Object.values(selections));
    const { faults, legRows, whole } = legsOf(
      await listedLegs("e2e/__tests__/fixtures/shard-groups.config.ts", [])
    );
    expect(whole).toHaveLength(44);
    expect(faults, `the ${legs} legs of the fixture against its whole listing`).toEqual([]);
    const legsHolding = (project: string, match: (title: string) => boolean) =>
      legRows.flatMap((rows, index) =>
        rows.some((row) => row.project === project && match(row.title)) ? [index + 1] : []
      );
    expect(
      legsHolding("parallel", (title) => title.startsWith(`serial rows${titleSeparator}`))
    ).toHaveLength(1);
    expect(
      legsHolding("parallel", (title) => title.startsWith(`default-mode rows${titleSeparator}`))
    ).toHaveLength(1);
    expect(legsHolding("sequential", () => true)).toHaveLength(1);
    // Two `serial` describes of one title, which share one unit key: each is a unit of its own,
    // so each stays on one leg. After the sequential project's file they are the two heaviest
    // units, so at more than two legs each goes onto a leg that is still empty, and the two land
    // on two legs. A placement looked up by key would put both on the leg of the later one.
    const first = legsHolding("parallel", (title) =>
      title.startsWith(`twin rows${titleSeparator}first twin row `)
    );
    const second = legsHolding("parallel", (title) =>
      title.startsWith(`twin rows${titleSeparator}second twin row `)
    );
    expect(first).toHaveLength(1);
    expect(second).toHaveLength(1);
    expect(first).not.toEqual(second);
    // The eight free rows of the fully parallel project, each a unit of its own, are spread:
    // the old split by file put all of them on one leg.
    const free = legsHolding("parallel", (title) => title.startsWith("free row "));
    expect(free.length).toBeGreaterThan(1);
  },
  rowLimit(
    // Both selections, side by side: the longer of two runs of `legs + 1` listings each.
    ...Array.from({ length: legs + 1 }, () => "listing" as const),
    // Then the fixture listed whole and leg by leg, one listing after another.
    ...Array.from({ length: legs + 1 }, () => "listing" as const)
  )
);

test(
  "the reporter keys each row as the listing does, inside a describe with no title and with a newline in its title too",
  async () => {
    // The full config's listings end first, whichever way (the file's header says why).
    await Promise.allSettled(Object.values(selections));
    const output = mkdtempSync(join(tmpdir(), "row-keys-"));
    try {
      const keys = join(output, "keys.json");
      const listed = await listing(
        import.meta.url,
        "e2e/__tests__/fixtures/row-keys.config.ts",
        { ROW_KEYS_OUTPUT: keys },
        [],
        ["./e2e/__tests__/fixtures/row-keys.reporter.ts"]
      );
      const listedRows = rowsOfListing(listed);
      // The listing keeps a `describe` with no title as a suite titled "", as a merged report does
      // (`rowTimes`, e2e/tools/shard-weights.ts), so a row inside one starts with the separator.
      expect(listedRows.map((row) => row.title)).toEqual([
        "a top-level row",
        "a row of\ntwo lines",
        `a titled describe${titleSeparator}a row in a titled describe`,
        `${titleSeparator}a row in an untitled describe`,
        `${titleSeparator}a titled describe in an untitled one${titleSeparator}a row two describes down`,
      ]);
      const reported = JSON.parse(readFileSync(keys, "utf8")) as Pick<
        Row,
        "project" | "file" | "title"
      >[];
      expect(reported.map(rowKey).sort()).toEqual(listedRows.map(rowKey).sort());
    } finally {
      rmSync(output, { force: true, recursive: true });
    }
  },
  rowLimit(
    // Both selections, side by side: the longer of two runs of `legs + 1` listings each.
    ...Array.from({ length: legs + 1 }, () => "listing" as const),
    // Then the fixture's one listing.
    "listing"
  )
);
