/** Which leg of a sharded run takes each row of each project: the rows placed by their measured
 *  time, so the legs end together, where Playwright's own `--shard` places rows by count.
 *
 * Why not Playwright's own split. Playwright 1.64.1 places test groups in the order it collects
 * them and cuts that order into legs of equal row COUNT (`filterForShard`,
 * node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:2468-2494).
 * Its one knob, `PWTEST_SHARD_WEIGHTS` (lib/cli/testActions.js:193-203), sets each leg's share of
 * that count, not of time. Rows do not cost the same - a stubbed row takes about 2 s, an issue
 * page row 10 and a layout row up to 77 - so on main at f02514268a the count split gave the full
 * lane's heaviest leg 1.115 to 1.225 times the mean row time. What 1.64.1 does offer is the hook
 * this module is called from: a reporter's `preprocess` sees the whole un-sharded selection, may
 * call `testRun.skipSharding()` and then `testRun.exclude()` what another leg runs
 * (types/testReporter.d.ts:230-258 and :789-794), which `e2e/shard-reporter.ts` does with the
 * placement below.
 *
 * The unit is what Playwright's own split moves as one: a test group (`createTestGroups`,
 * lib/runner/index.js:2385-2463). In a project that runs its files fully parallel - every project
 * of this config, `fullyParallel: true` - a group is one row, unless a `describe` around it runs
 * in `serial` or `default` mode, when the outermost such `describe` is one group; in a project
 * that does not, a whole file is one group. `e2e/shard-reporter.ts#groupOf` reads them as
 * Playwright does, so a `describe` that must run in order on one machine is never cut across two.
 *
 * Why the unit is not the file, which it was until the measurements below. A file is one block
 * of time on one leg, so its weight has to be right for the legs to balance, and the weight of a
 * file added after the table was read is a guess: its project's median row. On 2026-11-26 the
 * table was four days old, and two files added since held a median 853 s of the full lane's
 * 9,086: `stub` `layout-rule.stub.e2e.ts`, 35 rows placed at 69 s and taking 346 to 875 s, and
 * `chromium` `foot-sweep.e2e.ts`, 30 rows placed at 89 s and taking 302 to 449 s. Each landed
 * whole on one leg - the first on leg 4 and the second on leg 2 in all 28 merge-group runs of
 * 17:03Z to 23:04Z - and those two were the slowest legs of every one of those runs: the heaviest
 * leg held 1.21 times the mean row time (1.06 to 1.38), what the count split gave. On the pull
 * request lane the layout file alone was 33 to 44% of the lane's row time, and its leg held 2.0
 * times the mean. Placed row by row, a file the table has no entry for is many small units of its
 * project's median, which the placement spreads over every leg in about equal counts, so its
 * error is shared by all four instead of landing on one. Placed by this module over those 28 runs'
 * own rows, with that same four-day-old table, the heaviest leg holds 1.03 times the mean (at most
 * 1.06), and with a table seven days old 1.04 (at most 1.06); with the table read on 2026-11-27,
 * on the 20 of those runs it was not read from, 1.02 (at most 1.03), where placing that same table
 * by file still leaves 1.05 (at most 1.08).
 *
 * A row's weight is the seconds it took on main (`e2e/shard-weights.json`, which
 * `e2e/tools/shard-weights.ts` writes from main's merged reports): the row's own median where the
 * table holds the row; else the mean row of its file, for a row added or renamed in a file the
 * table holds; else the median row of its project, and for a project the table has none for the
 * median of every project's. Each is an estimate from the same measurements, never nothing, so a
 * new row is placed on the day it lands and the split never refuses a run. How far those
 * estimates are from the truth is what makes the table worth reading again: each leg prints how
 * many of its rows were placed by a project's median (`e2e/shard-reporter.ts`), and
 * `tableWarnings` below says when the table is old enough, or the estimates many enough, that the
 * browser step's hang guard no longer covers the drift.
 *
 * The placement is the longest-first greedy: the heaviest unit first, each onto the leg with the
 * least time so far. It is a function of the selection and the table alone - the units are sorted
 * by weight and then by their key, and a tie between legs goes to the lower number - so every leg
 * of a run, each computing it on its own machine from the same checkout, computes the same
 * placement, and each unit lands on exactly one leg. That is what keeps a row from being run
 * twice or by nobody, and `e2e/__tests__/shard-split.test.ts` holds it against the real config.
 *
 * Nothing here imports Playwright, so every function is callable from a plain Bun test.
 */

/** What `e2e/shard-weights.json` holds. */
export type ShardWeights = {
  /** Where the numbers were read: the runs whose merged reports they come from and the command
   *  that read them, so a reader can read them again. */
  readonly source: {
    readonly read: string;
    readonly command: string;
    readonly runs: readonly { readonly run: number; readonly commit: string }[];
  };
  /** Seconds a row of each project took: the median over that project's rows. */
  readonly projects: Readonly<Record<string, number>>;
  /** Seconds a row of each file took, by project and then by file (relative to the config's
   *  root, as a report names it): the mean over that file's rows. */
  readonly files: Readonly<Record<string, Readonly<Record<string, number>>>>;
  /** Seconds each row took, by project, then by file, then by the row's title below its file
   *  (`Row.title`): the median over the reports. */
  readonly rows: Readonly<
    Record<string, Readonly<Record<string, Readonly<Record<string, number>>>>>
  >;
};

/** The character Playwright joins a title path with, which a row's `title` below is joined with
 *  as `playwright test --list` and the list reporter print it. */
export const titleSeparator = " › ";

/** One row of the run's selection: its project, its file (relative to the config's root, as a
 *  report names it), its titles below the file joined by `titleSeparator`, and whether it will
 *  run. A row skipped where it is declared costs nothing, so it weighs nothing, as Playwright's
 *  own split does not count it; it is still placed, on exactly one leg, which reports it skipped. */
export type Row = {
  readonly project: string;
  readonly file: string;
  readonly title: string;
  readonly runs: boolean;
};

/** The rows Playwright keeps on one machine (the module's head says which), under a key no other
 *  unit of the run shares (`unitKey`). */
export type Unit = { readonly key: string; readonly rows: readonly Row[] };

/** A unit with what it is expected to cost, how many of its rows that will run were weighed by
 *  their project's median, and the leg it goes to, numbered from 1. */
export type Placed = Unit & {
  readonly seconds: number;
  readonly estimated: number;
  readonly shard: number;
};

/** What a unit holds: one row, the rows of one `describe`, or the rows of one file. */
export type UnitKind = "row" | "describe" | "file";

/** The key of the unit of `kind` that `titles` name in `file` of `project`: the row's own titles
 *  for a row, the `describe`'s for a `describe`, none for a file. The kind is in the key because
 *  a `describe` and a row beside it may carry one title. `repeat` is the copy's
 *  `repeatEachIndex`, which tells apart the copies `--repeat-each` makes of one row. */
export function unitKey(
  kind: UnitKind,
  project: string,
  file: string,
  repeat: number,
  titles: readonly string[]
): string {
  return JSON.stringify([project, file, repeat, kind, titles.join(titleSeparator)]);
}

/** The median of `values`, which must not be empty. `e2e/tools/shard-weights.ts` takes the
 *  table's medians with it too. */
export function median(values: readonly number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1
    ? (sorted[middle] as number)
    : ((sorted[middle - 1] as number) + (sorted[middle] as number)) / 2;
}

/** Seconds `row` is expected to take, and what the table weighed it by: the row itself, its
 *  file's mean row, or its project's median row. */
export function rowSeconds(
  row: Pick<Row, "project" | "file" | "title">,
  weights: ShardWeights
): { seconds: number; by: "row" | "file" | "project" } {
  const own = weights.rows[row.project]?.[row.file]?.[row.title];
  if (own !== undefined) return { by: "row", seconds: own };
  const file = weights.files[row.project]?.[row.file];
  if (file !== undefined) return { by: "file", seconds: file };
  const projectMedian = weights.projects[row.project] ?? median(Object.values(weights.projects));
  return { by: "project", seconds: projectMedian };
}

/** Every unit placed on one of `total` legs, longest first onto the least-loaded leg, with each
 *  leg's expected seconds. The same units and table give the same placement whatever order the
 *  units come in. */
export function placeUnits(
  units: readonly Unit[],
  weights: ShardWeights,
  total: number
): { placed: Placed[]; loads: number[] } {
  if (!Number.isInteger(total) || total < 1) {
    throw new Error(`a run splits into a whole number of legs, at least one, not ${total}`);
  }
  if (Object.keys(weights.projects).length === 0) {
    throw new Error("e2e/shard-weights.json holds no project, so no unit has a weight");
  }
  const weighed = units.map((unit) => {
    let seconds = 0;
    let estimated = 0;
    for (const row of unit.rows) {
      if (!row.runs) continue;
      const weight = rowSeconds(row, weights);
      seconds += weight.seconds;
      if (weight.by === "project") estimated += 1;
    }
    return { ...unit, estimated, seconds };
  });
  weighed.sort((a, b) => b.seconds - a.seconds || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  const loads = Array.from({ length: total }, () => 0);
  const placed = weighed.map((unit) => {
    let lightest = 0;
    for (let leg = 1; leg < total; leg += 1) {
      if ((loads[leg] as number) < (loads[lightest] as number)) lightest = leg;
    }
    loads[lightest] = (loads[lightest] as number) + unit.seconds;
    return { ...unit, shard: lightest + 1 };
  });
  return { loads, placed };
}

/** How old the table may be before a leg warns: the browser step's 21-minute guard is
 *  666 s x 1.75 x 1.06, and 1.06 is the worst share of the mean the heaviest leg took under a
 *  table up to 7 days old, of the two read (.github/ci-notes/dispatch-browser.md, the browser
 *  step's budget). Past that no table was measured, and one that drifted to the count split's 1.21
 *  would leave a healthy p90 leg on the slowest runner seen near 1,410 s, past the guard. */
export const tableDaysAtMost = 7;

/** The share of a leg's rows that may be placed by an estimate before the leg warns: the reading
 *  the table's tool names as the time to read it again. */
export const estimatedShareAtMost = 0.1;

/** The warnings a leg prints about the table it was placed by, each a sentence that names what
 *  to run: one when the table was read more than `tableDaysAtMost` days before `now`, one when
 *  `estimated` of the leg's `rows` reach `estimatedShareAtMost`. None for a fresh table whose
 *  files are measured. */
export function tableWarnings(read: string, now: Date, rows: number, estimated: number): string[] {
  const warnings: string[] = [];
  const days = (now.getTime() - Date.parse(read)) / 86_400_000;
  if (!(days <= tableDaysAtMost)) {
    warnings.push(
      `e2e/shard-weights.json was read ${Number.isNaN(days) ? "at no date it names" : `${days.toFixed(1)} days ago`} ` +
        `(${read}), past the ${tableDaysAtMost} days the browser step's 18-minute guard allows for its drift; ` +
        "read it again from packages/dispatch: bun e2e/tools/shard-weights.ts --main 17"
    );
  }
  if (rows > 0 && estimated / rows >= estimatedShareAtMost) {
    warnings.push(
      `${estimated} of this leg's ${rows} rows are in files e2e/shard-weights.json has no entry for, ` +
        `at least ${estimatedShareAtMost * 100}% of them; read it again from packages/dispatch: ` +
        "bun e2e/tools/shard-weights.ts --main 17"
    );
  }
  return warnings;
}

/** The projects a run holds as setup or teardown of another: each name in any project's
 *  `dependencies` or `teardown`. Playwright 1.64.1 runs such a project in full on every leg - it
 *  marks every project its closure reaches below the top as a dependency
 *  (`buildProjectsClosure`, lib/runner/index.js:2183-2203) and refuses `testRun.exclude` on its
 *  suite (:2363-2374) - so the placement leaves it out rather than excluding from it. */
export function dependencyProjects(
  projects: readonly { readonly dependencies?: readonly string[]; readonly teardown?: string }[]
): Set<string> {
  const names = new Set<string>();
  for (const project of projects) {
    for (const name of project.dependencies ?? []) names.add(name);
    if (project.teardown !== undefined) names.add(project.teardown);
  }
  return names;
}
