import { readFileSync, writeFileSync } from "node:fs";
import { join, sep } from "node:path";
import type { FullConfig, Reporter, Suite, TestCase, TestRun } from "@playwright/test/reporter";

import { rowId } from "./leg-rows";
import {
  dependencyProjects,
  placeUnits,
  type Row,
  type ShardWeights,
  tableWarnings,
  titleSeparator,
  type UnitKind,
  unitKey,
} from "./shard-split";

/** The file of measured row times the placement reads, beside this module. */
const weightsFile = new URL("./shard-weights.json", import.meta.url);

/** The modes Playwright 1.64.1 gives a suite: `none` unless set (`Suite`'s constructor,
 *  playwright/lib/common/index.js:2555), `serial`, `parallel` or `default` from a `describe` or
 *  its `configure` (:2306-2309 and :2343-2346), and `parallel` on a project suite whose project is
 *  `fullyParallel` (lib/runner/index.js:2674-2675). */
const parallelModes: Readonly<Record<string, true>> = {
  default: true,
  none: true,
  parallel: true,
  serial: true,
};

/** The mode `suite` runs its rows in. Playwright's reporter API does not name it, so it is read
 *  off the field Playwright's own split reads (`createTestGroups`, lib/runner/index.js:2414-2420).
 *  A Playwright that renamed or dropped the field fails the leg here, by name and before any row,
 *  rather than cut a `serial` `describe` across machines in silence. */
function parallelModeOf(suite: Suite): string {
  const mode = "_parallelMode" in suite ? suite._parallelMode : undefined;
  if (typeof mode !== "string" || parallelModes[mode] !== true) {
    throw new Error(
      `e2e/shard-reporter.ts cannot read how the suite "${suite.title}" runs its rows: ` +
        `Suite._parallelMode is ${JSON.stringify(mode)}, not one of ${Object.keys(parallelModes).join(", ")}, ` +
        "the modes Playwright 1.64.1's createTestGroups groups rows by; read that function in the " +
        "installed Playwright and write groupOf again from it"
    );
  }
  return mode;
}

/** The rows `test` is placed with, as the suite or row that holds them all: the row alone; the
 *  outermost `describe` around it that runs in `serial` or `default` mode, whose rows Playwright
 *  runs in order in one worker; or its whole file, where no suite around it runs in `parallel`
 *  mode. That is the group Playwright 1.64.1's own split would move it in (`createTestGroups`,
 *  lib/runner/index.js:2414-2440). Every project of this config is `fullyParallel` and no spec
 *  file sets a mode, so every row of it is its own group today; e2e/__tests__/shard-split.test.ts
 *  lists a fixture config that holds the other three shapes. */
export function groupOf(test: TestCase): { holder: TestCase | Suite; kind: UnitKind } {
  let insideParallel = false;
  let outermostSequential: Suite | undefined;
  let file: Suite | undefined;
  for (let parent: Suite | undefined = test.parent; parent; parent = parent.parent) {
    const mode = parallelModeOf(parent);
    if (mode === "serial" || mode === "default") outermostSequential = parent;
    if (mode === "parallel") insideParallel = true;
    if (parent.type === "file") file = parent;
  }
  if (file === undefined) {
    throw new Error(`the row "${test.title}" is in no file suite, so it cannot be placed`);
  }
  if (!insideParallel) return { holder: file, kind: "file" };
  if (outermostSequential === undefined) return { holder: test, kind: "row" };
  return {
    holder: outermostSequential,
    kind: outermostSequential.type === "file" ? "file" : "describe",
  };
}

/** Places a sharded run's rows on its legs by measured row time (`e2e/shard-split.ts` says why
 *  and how) and keeps only this leg's. A run with no `--shard` is left as it is: the hook
 *  returns before it touches anything.
 *
 *  It runs in `preprocess`, which Playwright calls once, before any row and before its own shard
 *  filter, with the whole selection the command line made - its projects, its `--grep` and its
 *  positional filters, but not `--shard` (types/testReporter.d.ts:230-242). It then tells
 *  Playwright to skip that filter, and excludes every row, `describe` and file of every project
 *  that the placement gives another leg (`testRun.exclude`, :759-764: an excluded suite's rows are
 *  neither run nor reported), a whole suite where none of its rows is this leg's, so no empty
 *  suite is left behind. `--shard=N/M` still names the leg and the leg count, which Playwright
 *  still prints ("Running 404 tests using 4 workers, shard 2 of 4") and still writes into the
 *  blob report's metadata, so nothing downstream of the run reads a different shape.
 *
 *  It is named in `e2e/playwright.config.ts`'s reporter list, so a local `--shard` run splits as
 *  CI does, and on the shard job's command line (.github/scripts/dispatch-browser-shard-suite.sh),
 *  because a `--reporter` there replaces the config's list. A run that loads it twice would call
 *  `skipSharding` twice, which Playwright refuses by name.
 *
 *  A project another project names in `dependencies` or `teardown` is left out of the placement:
 *  Playwright runs it in full on every leg and refuses `exclude` on it (`dependencyProjects`, in
 *  e2e/shard-split.ts). No project of this config has either today.
 *
 *  It prints one line, on stderr so a `--list --reporter=json` run's stdout stays the JSON it is:
 *  this leg's rows and the units they were placed in, the seconds of row time the placement
 *  expects of it and of the whole run, and how many of its rows were placed by their project's
 *  median because the table has no entry for their file - the share of the placement that is an
 *  estimate. Below it, a `::warning::` for a table older than the step's guard allows for, or one
 *  whose estimates are a tenth of the leg (`tableWarnings`), which the runner shows on the run's
 *  summary.
 *
 *  Where DISPATCH_E2E_LEG_ROWS_DIR names a directory - the shard job's, which the leg uploads
 *  beside its blob report - it writes two lists there, one row a line (`rowId`, e2e/leg-rows.ts):
 *  `shard-N.selection.jsonl`, every row of the selection as `preprocess` reads it before any
 *  exclusion, and `shard-N.ran.jsonl`, the rows `onBegin` is handed, which is what Playwright
 *  schedules on this leg after every filter. The merge job's reading one holds the four legs'
 *  lists against each other (e2e/tools/leg-rows.ts): a row of the selection that no leg ran, or
 *  that two ran, fails it by name. The second list is read from Playwright and not from this
 *  hook's own choice, so a fault in the choice cannot hide in both. */
export default class ShardReporter implements Reporter {
  /** Where this leg writes its lists, and which leg and setup projects they are of; unset where
   *  the run is not sharded or names no directory. */
  private lists?: { readonly dir: string; readonly leg: string; readonly setups: Set<string> };

  printsToStdio(): boolean {
    return false;
  }

  async preprocess({
    config,
    suite,
    testRun,
  }: {
    config: FullConfig;
    suite: Suite;
    testRun: TestRun;
  }): Promise<void> {
    const shard = config.shard;
    if (!shard) return;
    testRun.skipSharding();
    const weights = JSON.parse(readFileSync(weightsFile, "utf8")) as ShardWeights;
    const setups = dependencyProjects(
      suite.suites.map((projectSuite) => projectSuite.project() ?? {})
    );
    const dir = process.env.DISPATCH_E2E_LEG_ROWS_DIR;
    if (dir) {
      this.lists = { dir, leg: `shard-${shard.current}`, setups };
      this.writeList("selection", suite);
    }
    const units = new Map<TestCase | Suite, { key: string; rows: Row[] }>();
    const unitOfRow = new Map<TestCase, string>();
    const fileSuites: Suite[] = [];
    for (const projectSuite of suite.suites) {
      const project = projectSuite.project()?.name ?? projectSuite.title;
      if (setups.has(project)) continue;
      for (const fileSuite of projectSuite.suites) {
        fileSuites.push(fileSuite);
        // A file suite's title is its path relative to the config's root, where a report names
        // the same file (playwright/lib/common/index.js:2872 at 1.64.1), so it keys the table.
        const file = fileSuite.title.split(sep).join("/");
        for (const test of fileSuite.allTests()) {
          const { holder, kind } = groupOf(test);
          let unit = units.get(holder);
          if (unit === undefined) {
            // A title path is the root's empty title, the project, the file, then the titles
            // below the file: the table's and the listing's own key.
            const titles = holder.titlePath().slice(3);
            unit = { key: unitKey(kind, project, file, test.repeatEachIndex, titles), rows: [] };
            units.set(holder, unit);
          }
          unit.rows.push({
            file,
            project,
            runs: test.expectedStatus !== "skipped",
            title: test.titlePath().slice(3).join(titleSeparator),
          });
          unitOfRow.set(test, unit.key);
        }
      }
    }
    const { loads, placed } = placeUnits([...units.values()], weights, shard.total);
    const legOf = new Map(placed.map((unit) => [unit.key, unit.shard]));
    const isMine = (test: TestCase) => legOf.get(unitOfRow.get(test) ?? "") === shard.current;
    const keepMine = (holder: Suite): void => {
      // A copy: `entries()` is the suite's own array, which each exclusion splices.
      for (const entry of [...holder.entries()]) {
        if (entry.type === "test") {
          if (!isMine(entry)) testRun.exclude(entry);
          continue;
        }
        const tests = entry.allTests();
        if (!tests.some(isMine)) testRun.exclude(entry);
        else if (!tests.every(isMine)) keepMine(entry);
      }
    };
    for (const fileSuite of fileSuites) {
      const tests = fileSuite.allTests();
      if (!tests.some(isMine)) testRun.exclude(fileSuite);
      else if (!tests.every(isMine)) keepMine(fileSuite);
    }
    const mine = placed.filter((unit) => unit.shard === shard.current);
    const rows = mine.reduce((sum, unit) => sum + unit.rows.length, 0);
    const estimated = mine.reduce((sum, unit) => sum + unit.estimated, 0);
    const total = loads.reduce((sum, load) => sum + load, 0);
    process.stderr.write(
      `shard ${shard.current} of ${shard.total}, placed by measured row time: ${rows} rows in ` +
        `${mine.length} units, ${Math.round(loads[shard.current - 1] ?? 0)} s of row time ` +
        `expected of the run's ${Math.round(total)} s; ${estimated} of these rows are in files ` +
        "e2e/shard-weights.json has no entry for, placed by their project's median row\n"
    );
    for (const warning of tableWarnings(weights.source.read, new Date(), rows, estimated)) {
      process.stderr.write(`::warning title=shard ${shard.current} row-time table::${warning}\n`);
    }
  }

  onBegin(_config: FullConfig, suite: Suite): void {
    if (this.lists) this.writeList("ran", suite);
  }

  /** Writes `<dir>/shard-N.<kind>.jsonl`: every row of `suite` outside the setup projects,
   *  sorted, one a line. */
  private writeList(kind: "selection" | "ran", suite: Suite): void {
    if (!this.lists) return;
    const { dir, leg, setups } = this.lists;
    const rows = suite.suites
      .filter((projectSuite) => !setups.has(projectSuite.project()?.name ?? projectSuite.title))
      .flatMap((projectSuite) => projectSuite.allTests().map((test) => rowId(test.titlePath())))
      .sort();
    writeFileSync(join(dir, `${leg}.${kind}.jsonl`), rows.length > 0 ? `${rows.join("\n")}\n` : "");
  }
}
