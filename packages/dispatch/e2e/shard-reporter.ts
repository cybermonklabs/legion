import { readFileSync, writeFileSync } from "node:fs";
import { join, sep } from "node:path";
import type { FullConfig, Reporter, Suite, TestRun } from "@playwright/test/reporter";

import { rowId } from "./leg-rows";
import {
  dependencyProjects,
  placeUnits,
  type ShardWeights,
  tableWarnings,
  type Unit,
} from "./shard-split";

/** The file of measured row times the placement reads, beside this module. */
const weightsFile = new URL("./shard-weights.json", import.meta.url);

/** Places a sharded run's files on its legs by measured row time (`e2e/shard-split.ts` says why
 *  and how) and keeps only this leg's. A run with no `--shard` is left as it is: the hook
 *  returns before it touches anything.
 *
 *  It runs in `preprocess`, which Playwright calls once, before any row and before its own shard
 *  filter, with the whole selection the command line made - its projects, its `--grep` and its
 *  positional filters, but not `--shard` (types/testReporter.d.ts:230-242). It then tells
 *  Playwright to skip that filter, and excludes every file of every project that the placement
 *  gives another leg (`testRun.exclude`, :759-764: an excluded suite's rows are neither run nor
 *  reported). `--shard=N/M` still names the leg and the leg count, which Playwright still
 *  prints ("Running 404 tests using 4 workers, shard 2 of 6") and still writes into the blob
 *  report's metadata, so nothing downstream of the run reads a different shape.
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
 *  this leg's rows, the seconds of row time the placement expects of it and of the whole run, and
 *  how many of its rows were placed by their project's median because the table has no entry for
 *  their file - the share of the placement that is an estimate. Below it, a `::warning::` for a
 *  table older than the step's guard allows for, or one whose estimates are a tenth of the leg
 *  (`tableWarnings`), which the runner shows on the run's summary.
 *
 *  Where DISPATCH_E2E_LEG_ROWS_DIR names a directory - the shard job's, which the leg uploads
 *  beside its blob report - it writes two lists there, one row a line (`rowId`, e2e/leg-rows.ts):
 *  `shard-N.selection.jsonl`, every row of the selection as `preprocess` reads it before any
 *  exclusion, and `shard-N.ran.jsonl`, the rows `onBegin` is handed, which is what Playwright
 *  schedules on this leg after every filter. The merge job's reading one holds every leg's
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
    const files: { unit: Unit; suite: Suite; rows: number }[] = [];
    const setups = dependencyProjects(
      suite.suites.map((projectSuite) => projectSuite.project() ?? {})
    );
    const dir = process.env.DISPATCH_E2E_LEG_ROWS_DIR;
    if (dir) {
      this.lists = { dir, leg: `shard-${shard.current}`, setups };
      this.writeList("selection", suite);
    }
    for (const projectSuite of suite.suites) {
      const project = projectSuite.project()?.name ?? projectSuite.title;
      if (setups.has(project)) continue;
      for (const fileSuite of projectSuite.suites) {
        const tests = fileSuite.allTests();
        const runs = tests.filter((test) => test.expectedStatus !== "skipped").length;
        // A file suite's title is its path relative to the config's root, where a report names
        // the same file (playwright/lib/common/index.js:2872 at 1.64.1), so it keys the table.
        const file = fileSuite.title.split(sep).join("/");
        files.push({ rows: tests.length, suite: fileSuite, unit: { file, project, rows: runs } });
      }
    }
    const { loads, placed } = placeUnits(
      files.map((entry) => entry.unit),
      weights,
      shard.total
    );
    const legOf = new Map(placed.map((unit) => [`${unit.project}\n${unit.file}`, unit]));
    let rows = 0;
    let estimated = 0;
    for (const entry of files) {
      const unit = legOf.get(`${entry.unit.project}\n${entry.unit.file}`);
      if (unit?.shard === shard.current) {
        rows += entry.rows;
        if (!unit.measured) estimated += entry.rows;
      } else {
        testRun.exclude(entry.suite);
      }
    }
    const total = loads.reduce((sum, load) => sum + load, 0);
    process.stderr.write(
      `shard ${shard.current} of ${shard.total}, placed by measured row time: ${rows} rows, ` +
        `${Math.round(loads[shard.current - 1] ?? 0)} s of row time expected of the run's ` +
        `${Math.round(total)} s; ${estimated} of these rows are in files e2e/shard-weights.json ` +
        "has no entry for, placed by their project's median row\n"
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
