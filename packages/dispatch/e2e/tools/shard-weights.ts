/** `bun e2e/tools/shard-weights.ts --main <count> [--cache <dir>]`: reads the newest <count>
 *  merged reports of main's full-lane runs and writes `e2e/shard-weights.json`, the row times
 *  `e2e/shard-split.ts` places a sharded run's rows by.
 *
 * Run it from packages/dispatch, with `gh` signed in to a token that reads this repository's
 * Actions artifacts. It lists the artifacts named `dispatch-browser-report-1` - the merged report
 * the merge job of .github/workflows/dispatch-browser.yaml uploads on a run's first attempt,
 * whose report.json holds every row of every shard and alone leg with its time - a hundred to a
 * call, until it has <count> of main's, and downloads each report it reads: about <count> + 3
 * calls of the repository's shared hourly budget. It keeps the reports of a merge group's run
 * (branch `gh-readonly-queue/main/...`) and of main's own (the nightly), which run the full lane;
 * a pull request's run is passed over, since its lane holds a few of the rows.
 * `--cache <dir>` keeps each extracted report as <dir>/<run id>.json and reads it from there next
 * time instead of downloading it again.
 *
 * What it writes, for each row that ran: the seconds every attempt of it took, summed, since a
 * retry is time its leg spent; the median of that over the reports, so one slow runner, which
 * slows every row of its leg alike, moves no row's number - the row's own weight; then the mean
 * of those medians over each file of each project, which is what a row the table does not hold
 * in a file it does is placed by, and the median over each project's rows, which is what a row
 * of a file the table has no entry for is placed by. Seconds are rounded to the hundredth.
 *
 * When to run it: each sharded leg prints how many of its rows were placed by their project's
 * median because their file has no entry here (`e2e/shard-reporter.ts`). Read on 2026-11-27 over
 * the 28 full-lane merge-group runs of 2026-11-26, the row-by-row placement put the heaviest leg
 * at 1.03 times the mean under a table four days old and 1.04 under one seven days old (at most
 * 1.06 each), against 1.02 for a fresh one; placed by file, those same two tables gave 1.20 and
 * 1.22, since each lacked files that had landed since. The browser step's guard holds up to that
 * 1.06, so each leg warns when the table is more than 7 days old or a tenth of its rows are
 * estimated (`tableWarnings`, e2e/shard-split.ts): read it again then.
 */
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";

import { median, type ShardWeights, titleSeparator } from "../shard-split";

const usage =
  "usage: bun e2e/tools/shard-weights.ts --main <count> [--cache <dir>]\n" +
  "  --main   how many of the newest merged reports of main's full-lane runs to read\n" +
  "  --cache  a directory that keeps each report as <run id>.json between runs";

/** The merged report's artifact on a run's first attempt (dispatch-browser.yaml's merge job). */
const artifactName = "dispatch-browser-report-1";

interface ReportSuite {
  readonly title?: string;
  readonly specs?: readonly ReportSpec[];
  readonly suites?: readonly ReportSuite[];
}

interface ReportSpec {
  readonly file?: string;
  readonly title?: string;
  readonly tests?: readonly {
    readonly projectName?: string;
    readonly results?: readonly { readonly duration?: number; readonly status?: string }[];
  }[];
}

/** One report's rows that ran: `project`, `file` and the title path, joined by newlines, to the
 *  seconds all its attempts took. */
export function rowTimes(report: {
  readonly suites?: readonly ReportSuite[];
}): Map<string, number> {
  const rows = new Map<string, number>();
  const walk = (suite: ReportSuite, path: readonly string[]): void => {
    for (const spec of suite.specs ?? []) {
      for (const test of spec.tests ?? []) {
        const ran = (test.results ?? []).filter((result) => result.status !== "skipped");
        if (ran.length === 0) continue;
        const seconds = ran.reduce((sum, result) => sum + (result.duration ?? 0), 0) / 1000;
        rows.set(
          [test.projectName ?? "", spec.file ?? "", ...path, spec.title ?? ""].join("\n"),
          seconds
        );
      }
    }
    for (const child of suite.suites ?? []) walk(child, [...path, child.title ?? ""]);
  };
  // A report's top-level suites are its files, whose title is the file: no part of a row's title.
  for (const file of report.suites ?? []) walk(file, []);
  return rows;
}

/** The table of the reports given, each with the run and commit it came from. */
export function weightsOf(
  reports: readonly { run: number; commit: string; rows: Map<string, number> }[],
  read: string,
  command: string
): ShardWeights {
  const byRow = new Map<string, number[]>();
  for (const report of reports) {
    for (const [row, seconds] of report.rows) {
      const times = byRow.get(row);
      if (times) times.push(seconds);
      else byRow.set(row, [seconds]);
    }
  }
  // Each row's median, by project, then file, then the row's titles below its file.
  const byFile = new Map<string, Map<string, Map<string, number>>>();
  for (const [row, times] of byRow) {
    const [project = "", file = "", ...titles] = row.split("\n");
    const files = byFile.get(project) ?? new Map<string, Map<string, number>>();
    byFile.set(project, files);
    const ofFile = files.get(file) ?? new Map<string, number>();
    files.set(file, ofFile);
    ofFile.set(titles.join(titleSeparator), median(times));
  }
  const hundredths = (seconds: number) => Math.round(seconds * 100) / 100;
  const projects: Record<string, number> = {};
  const files: Record<string, Record<string, number>> = {};
  const rows: Record<string, Record<string, Record<string, number>>> = {};
  for (const project of [...byFile.keys()].sort()) {
    const fileRows = byFile.get(project) ?? new Map<string, Map<string, number>>();
    projects[project] = hundredths(median([...fileRows.values()].flatMap((r) => [...r.values()])));
    const perFile: Record<string, number> = {};
    const perRow: Record<string, Record<string, number>> = {};
    for (const file of [...fileRows.keys()].sort()) {
      const ofFile = fileRows.get(file) ?? new Map<string, number>();
      const seconds = [...ofFile.values()];
      perFile[file] = hundredths(seconds.reduce((sum, s) => sum + s, 0) / seconds.length);
      perRow[file] = Object.fromEntries(
        [...ofFile.keys()].sort().map((title) => [title, hundredths(ofFile.get(title) ?? 0)])
      );
    }
    files[project] = perFile;
    rows[project] = perRow;
  }
  return {
    files,
    projects,
    rows,
    source: {
      command,
      read,
      runs: reports.map(({ commit, run }) => ({ commit, run })),
    },
  };
}

/** `gh` with these arguments, its stdout as bytes; a failed call ends the tool with its stderr. */
function gh(args: readonly string[]): Buffer {
  const result = spawnSync("gh", args, { maxBuffer: 1 << 30 });
  if (result.status !== 0) {
    console.error(`gh ${args.join(" ")} exited ${result.status}:\n${result.stderr}`);
    process.exit(1);
  }
  return result.stdout;
}

type Artifact = {
  readonly id: number;
  readonly expired: boolean;
  readonly workflow_run?: {
    readonly id: number;
    readonly head_branch: string;
    readonly head_sha: string;
  };
};

/** How many pages of a hundred artifacts the listing reads at most before it gives up: a pull
 *  request's run uploads the same artifact for its own lane, and those are most of the list -
 *  on 2026-11-22 the newest hundred held eleven of main's runs. */
const pagesAtMost = 10;

/** The newest `count` merged reports of main's full-lane runs, read through `cache`. */
function mainReports(
  count: number,
  cache: string
): { run: number; commit: string; rows: Map<string, number> }[] {
  const runs: Artifact[] = [];
  for (let page = 1; page <= pagesAtMost && runs.length < count; page += 1) {
    const listed = JSON.parse(
      gh([
        "api",
        `repos/{owner}/{repo}/actions/artifacts?name=${artifactName}&per_page=100&page=${page}`,
      ]).toString()
    ) as { artifacts: readonly Artifact[] };
    runs.push(
      ...listed.artifacts.filter(
        (artifact) =>
          !artifact.expired &&
          artifact.workflow_run !== undefined &&
          (artifact.workflow_run.head_branch === "main" ||
            artifact.workflow_run.head_branch.startsWith("gh-readonly-queue/main/"))
      )
    );
    if (listed.artifacts.length < 100) break;
  }
  if (runs.length < count) {
    console.error(
      `the newest ${pagesAtMost * 100} ${artifactName} artifacts hold ${runs.length} of main's ` +
        `full-lane runs, fewer than the ${count} asked for: ask for fewer`
    );
    process.exit(1);
  }
  return runs.slice(0, count).map((artifact) => {
    const run = artifact.workflow_run as NonNullable<Artifact["workflow_run"]>;
    const file = join(cache, `${run.id}.json`);
    if (!existsSync(file)) {
      const zip = join(cache, `${run.id}.zip`);
      writeFileSync(zip, gh(["api", `repos/{owner}/{repo}/actions/artifacts/${artifact.id}/zip`]));
      const unzip = spawnSync("unzip", ["-p", zip, "report.json"], { maxBuffer: 1 << 30 });
      if (unzip.status !== 0) {
        console.error(`unzip -p ${zip} report.json exited ${unzip.status}:\n${unzip.stderr}`);
        process.exit(1);
      }
      writeFileSync(file, unzip.stdout);
    }
    const rows = rowTimes(JSON.parse(readFileSync(file, "utf8")));
    console.error(`run ${run.id} at ${run.head_sha.slice(0, 10)}: ${rows.size} rows`);
    return { commit: run.head_sha, rows, run: run.id };
  });
}

if (import.meta.main) {
  let count = 0;
  let cache = "";
  try {
    const { positionals, values } = parseArgs({
      allowPositionals: true,
      options: { cache: { type: "string" }, main: { type: "string" } },
      strict: true,
    });
    count = Number(values.main);
    if (positionals.length > 0 || !Number.isInteger(count) || count < 1) throw new Error(usage);
    cache = values.cache ?? mkdtempSync(join(tmpdir(), "shard-weights-"));
  } catch {
    console.error(usage);
    process.exit(2);
  }
  mkdirSync(cache, { recursive: true });
  const reports = mainReports(count, cache);
  const weights = weightsOf(
    reports,
    new Date().toISOString(),
    `bun e2e/tools/shard-weights.ts --main ${count}`
  );
  const out = new URL("../shard-weights.json", import.meta.url);
  writeFileSync(out, `${JSON.stringify(weights, null, 2)}\n`);
  const files = Object.values(weights.files).reduce(
    (sum, perFile) => sum + Object.keys(perFile).length,
    0
  );
  console.error(
    `wrote ${out.pathname}: ${Object.keys(weights.projects).length} projects, ${files} files`
  );
}
