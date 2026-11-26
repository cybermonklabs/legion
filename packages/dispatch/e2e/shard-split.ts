/** Which leg of a sharded run takes each file of each project: the files placed by their measured
 *  row time, so the legs end together, where Playwright's own `--shard` places rows by count.
 *
 * Why not Playwright's own split. Playwright 1.64.1 places test groups in the order it collects
 * them and cuts that order into legs of equal row COUNT (`filterForShard`,
 * node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:2462-2488).
 * Its one knob, `PWTEST_SHARD_WEIGHTS` (lib/cli/testActions.js:193-203), sets each leg's share of
 * that count, not of time. Rows do not cost the same - a stubbed row takes about 2 s and an issue
 * page row 10 - so on main at f02514268a the count split gave the full lane's heaviest leg 1.115
 * to 1.225 times the mean row time (the per-row medians of two sets of 13 and 14 of main's runs
 * that the table below was not read from), and the leg holding the slow chromium files was the
 * slowest of its run in 55 of the 85 runs of 2026-11-21/22 whose four legs all ran; the placement
 * below gives the same rows 1.007 and 1.008 times the mean. What 1.64.1 does offer is the hook this
 * module is called from: a reporter's `preprocess` sees the whole un-sharded selection, may call
 * `testRun.skipSharding()` and then `testRun.exclude()` what another leg runs
 * (types/testReporter.d.ts:230-258 and :789-794), which `e2e/shard-reporter.ts` does with the
 * placement below.
 *
 * The unit is one file of one project. A file's rows stay together, so a `describe` a file runs
 * in `serial` mode is never cut across two machines; and the file is what the weights are kept
 * for, since a row title changes far more often than a file name. 311 units hold the full lane's
 * 1,617 rows at f02514268a, and the largest, `chromium` `issue-page-i.e2e.ts`, weighs 309 of the
 * 6,872 s the table expects of them, under a fifth of a leg.
 *
 * A unit's weight is its rows times the seconds one of its rows took on main
 * (`e2e/shard-weights.json`, which `e2e/tools/shard-weights.ts` writes from main's merged
 * reports). Its rows are counted now, not when the weights were read, so a file that grew is
 * heavier at once. A file the table has no entry for - a file added since, or renamed - takes
 * the median row of its project, and a project the table has none for takes the median of every
 * project's: an estimate from the same measurements, never nothing, so a new file is placed on
 * the day it lands and the split never refuses a run. How far those estimates are from the truth
 * is what makes the table worth reading again: each leg prints how many of its rows were placed
 * by one (`e2e/shard-reporter.ts`), and `tableWarnings` below says when the table is old enough,
 * or the estimates many enough, that the browser step's hang guard no longer covers the drift.
 *
 * The placement is the longest-first greedy: the heaviest unit first, each onto the leg with the
 * least time so far. It is a function of the selection and the table alone - the units are sorted
 * by weight, then project, then file, and a tie between legs goes to the lower number - so every
 * leg of a run, each computing it on its own machine from the same checkout, computes the same
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
};

/** One file of one project in the run's selection, with the rows of it that will run: a row
 *  skipped where it is declared costs nothing, so it is not counted, as Playwright's own split
 *  does not count it. */
export type Unit = { readonly project: string; readonly file: string; readonly rows: number };

/** A unit with what it is expected to cost and the leg it goes to, numbered from 1. */
export type Placed = Unit & {
  readonly seconds: number;
  /** False where the file had no entry and its project's median row stood in. */
  readonly measured: boolean;
  readonly shard: number;
};

/** The median of `values`, which must not be empty. `e2e/tools/shard-weights.ts` takes the
 *  table's medians with it too. */
export function median(values: readonly number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1
    ? (sorted[middle] as number)
    : ((sorted[middle - 1] as number) + (sorted[middle] as number)) / 2;
}

/** Seconds one row of `unit` is expected to take, and whether the table measured its file. */
export function rowSeconds(
  unit: Pick<Unit, "project" | "file">,
  weights: ShardWeights
): { seconds: number; measured: boolean } {
  const measured = weights.files[unit.project]?.[unit.file];
  if (measured !== undefined) return { measured: true, seconds: measured };
  const projectMedian = weights.projects[unit.project] ?? median(Object.values(weights.projects));
  return { measured: false, seconds: projectMedian };
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
    const { measured, seconds } = rowSeconds(unit, weights);
    return { ...unit, measured, seconds: unit.rows * seconds };
  });
  weighed.sort(
    (a, b) =>
      b.seconds - a.seconds ||
      (a.project < b.project ? -1 : a.project > b.project ? 1 : 0) ||
      (a.file < b.file ? -1 : a.file > b.file ? 1 : 0)
  );
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

/** How old the table may be before a leg warns: the browser step's 15-minute guard is
 *  449 s x 1.75 x 1.11, and 1.11 is the worst share of the mean the heaviest leg took under a
 *  table up to 7 days old, of the two read (.github/ci-notes/dispatch-browser.md, the browser
 *  step's budget). Past that no table was measured, and one that drifted to the count split's 1.21
 *  would leave a healthy p90 leg on the slowest runner seen near 951 s, past the guard. */
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
        `(${read}), past the ${tableDaysAtMost} days the browser step's 15-minute guard allows for its drift; ` +
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
