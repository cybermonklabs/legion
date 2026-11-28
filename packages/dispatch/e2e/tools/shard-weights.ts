/** `bun e2e/tools/shard-weights.ts --main <count> [--run <id>]... [--cache <dir>]`: reads the
 *  newest <count> merged reports of main's full-lane runs, and that of each run `--run` names,
 *  and writes `e2e/shard-weights.json`, the row times `e2e/shard-split.ts` places a sharded run's
 *  rows by.
 *
 * Run it from packages/dispatch, with `gh` signed in to a token that reads this repository's
 * Actions artifacts. It lists the artifacts named `dispatch-browser-report-1` - the merged report
 * the merge job of .github/workflows/dispatch-browser.yaml uploads on a run's first attempt,
 * whose report.json holds every row of every shard and alone leg with its time - a hundred to a
 * call, until it has <count> of main's, and downloads each report it reads: about <count> + 3
 * calls of the repository's shared hourly budget, and two more for each run `--run` names. It
 * keeps the reports of a merge group's run (branch `gh-readonly-queue/main/...`) and of main's
 * own (the nightly), which run the full lane; a pull request's run is passed over unless `--run`
 * names it, since it times the pull request's code and not main's.
 * `--cache <dir>` keeps each extracted report as <dir>/<run id>.json and reads it from there next
 * time instead of downloading it again.
 *
 * `--run <id>`, which may be given more than once, is for a pull request that adds or grows
 * files. Main's reports hold none of their new rows until the files land, so until then the split
 * places those by their project's median row, however long they take. The pull request's own
 * full-lane run - a run under the `lane:dispatch_browser` label (`full-ci` is the other label that
 * gives it), which uploads the same merged report - has measured them: named with `--run`, its
 * report joins main's, each row's median is over every report that holds it, and the new files'
 * rows are placed by their measured time before main's reports hold them. The next read from main
 * alone replaces those numbers once the files land. A named run's artifact is found by the run's id
 * (`actions/runs/<id>/artifacts`); a run with none, or whose artifact has expired, ends the tool,
 * and a run named twice, or one that is also among main's newest <count>, is read once.
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
 * the 28 full-lane merge-group runs of 2026-11-26, the row-by-row placement at six legs put the
 * heaviest leg at 1.10 times the mean under a table four days old (at most 1.115) and
 * 1.04 under one seven days old (at most 1.05), against 1.04 for a fresh one; at four legs the
 * same tables gave 1.03 and 1.04, and placed by file 1.20 and 1.22, since each lacked files that
 * had landed since. The browser step's guard holds up to that 1.115, so each leg warns when the
 * table is more than 7 days old or a tenth of its rows are estimated (`tableWarnings`,
 * e2e/shard-split.ts): read it again then.
 */
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";

import { titleSeparator } from "../configurations";
import { median, type ShardWeights } from "../shard-split";

const usage =
  "usage: bun e2e/tools/shard-weights.ts --main <count> [--run <id>]... [--cache <dir>]\n" +
  "  --main   how many of the newest merged reports of main's full-lane runs to read\n" +
  "  --run    the id of a full-lane run whose merged report to read as well, repeatable: a pull\n" +
  "           request's own, so the files it adds or grows are placed by their measured time\n" +
  "           before main's reports hold them; a pull request's run is read only when named\n" +
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

/** One report's rows that ran, each keyed by `project`, `file`, its `describe` titles and its own
 *  title as one JSON array, to the seconds all its attempts took. JSON is the separator-free way
 *  to key strings: a title may hold a newline, which Playwright allows, and it stays one title. */
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
          JSON.stringify([test.projectName ?? "", spec.file ?? "", ...path, spec.title ?? ""]),
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
  const medians = new Map<string, Map<string, Map<string, number>>>();
  for (const [row, times] of byRow) {
    const [project = "", file = "", ...titles] = JSON.parse(row) as string[];
    const files = medians.get(project) ?? new Map<string, Map<string, number>>();
    medians.set(project, files);
    const ofFile = files.get(file) ?? new Map<string, number>();
    files.set(file, ofFile);
    ofFile.set(titles.join(titleSeparator), median(times));
  }
  const hundredths = (seconds: number) => Math.round(seconds * 100) / 100;
  // A map's entries by key, in the order `.sort()` gives strings: by UTF-16 code unit.
  const sortedEntries = <V>(map: ReadonlyMap<string, V>) =>
    [...map].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  const projects: Record<string, number> = {};
  const files: Record<string, Record<string, number>> = {};
  const rows: Record<string, Record<string, Record<string, number>>> = {};
  for (const [project, fileRows] of sortedEntries(medians)) {
    projects[project] = hundredths(median([...fileRows.values()].flatMap((r) => [...r.values()])));
    const perFile: Record<string, number> = {};
    const perRow: Record<string, Record<string, number>> = {};
    for (const [file, ofFile] of sortedEntries(fileRows)) {
      const seconds = [...ofFile.values()];
      perFile[file] = hundredths(seconds.reduce((sum, s) => sum + s, 0) / seconds.length);
      perRow[file] = Object.fromEntries(
        sortedEntries(ofFile).map(([title, rowMedian]) => [title, hundredths(rowMedian)])
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

/** An artifact as GitHub's API lists it, in the fields the tool reads; the API documents its
 *  `workflow_run` as nullable. */
type Artifact = {
  readonly id: number;
  readonly expired: boolean;
  readonly workflow_run?: {
    readonly id: number;
    readonly head_branch: string;
    readonly head_sha: string;
  } | null;
};

/** How many pages of a hundred artifacts the listing reads at most before it gives up: a pull
 *  request's run uploads the same artifact for its own lane, and those are most of the list -
 *  on 2026-11-22 the newest hundred held eleven of main's runs. */
const pagesAtMost = 10;

/** A run's merged report artifact: its id, the run that uploaded it and the commit that run ran. */
type RunArtifact = { readonly commit: string; readonly id: number; readonly run: number };

/** The artifacts of the newest `count` merged reports of main's full-lane runs. */
function mainArtifacts(count: number): RunArtifact[] {
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
          artifact.workflow_run != null &&
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
    return { commit: run.head_sha, id: artifact.id, run: run.id };
  });
}

/** The choice `namedArtifact` makes over GitHub's answers for run `run`: its first listed artifact
 *  that has not expired, with the commit that artifact's `workflow_run` names, else the commit of
 *  `runAnswer()`, the run's own answer, which is asked for only then; or, when the answer lists no
 *  artifact or every one it lists has expired, why the run is refused. */
export function namedArtifactOf(
  run: number,
  { artifacts }: { readonly artifacts: readonly Artifact[] },
  runAnswer: () => { readonly head_sha: string }
): RunArtifact | { readonly refusal: string } {
  const artifact = artifacts.find((listed) => !listed.expired);
  if (artifact === undefined) {
    return {
      refusal:
        artifacts.length === 0
          ? `run ${run} has no ${artifactName} artifact: name a full-lane run whose first attempt ` +
            "uploaded its merged report"
          : `run ${run}'s ${artifactName} artifact has expired: name a newer full-lane run`,
    };
  }
  if (artifact.workflow_run) {
    return { commit: artifact.workflow_run.head_sha, id: artifact.id, run };
  }
  return { commit: runAnswer().head_sha, id: artifact.id, run };
}

/** The artifact of the merged report of run `run`, which `--run` named; a run that has none, or
 *  whose artifact has expired, ends the tool. */
function namedArtifact(run: number): RunArtifact {
  const chosen = namedArtifactOf(
    run,
    JSON.parse(
      gh([
        "api",
        `repos/{owner}/{repo}/actions/runs/${run}/artifacts?name=${artifactName}`,
      ]).toString()
    ) as { artifacts: readonly Artifact[] },
    () =>
      JSON.parse(gh(["api", `repos/{owner}/{repo}/actions/runs/${run}`]).toString()) as {
        head_sha: string;
      }
  );
  if ("refusal" in chosen) {
    console.error(chosen.refusal);
    process.exit(1);
  }
  return chosen;
}

/** The rows of the merged report `id` holds, read from `<cache>/<run id>.json` when an earlier
 *  read left it there, else downloaded and extracted into it. */
function readReport(
  { commit, id, run }: RunArtifact,
  cache: string
): Parameters<typeof weightsOf>[0][number] {
  const file = join(cache, `${run}.json`);
  if (!existsSync(file)) {
    const zip = join(cache, `${run}.zip`);
    writeFileSync(zip, gh(["api", `repos/{owner}/{repo}/actions/artifacts/${id}/zip`]));
    const unzip = spawnSync("unzip", ["-p", zip, "report.json"], { maxBuffer: 1 << 30 });
    if (unzip.status !== 0) {
      console.error(`unzip -p ${zip} report.json exited ${unzip.status}:\n${unzip.stderr}`);
      process.exit(1);
    }
    writeFileSync(file, unzip.stdout);
  }
  const rows = rowTimes(JSON.parse(readFileSync(file, "utf8")));
  console.error(`run ${run} at ${commit.slice(0, 10)}: ${rows.size} rows`);
  return { commit, rows, run };
}

if (import.meta.main) {
  let count = 0;
  let runs: number[] = [];
  let cache = "";
  try {
    const { positionals, values } = parseArgs({
      allowPositionals: true,
      options: {
        cache: { type: "string" },
        main: { type: "string" },
        run: { multiple: true, type: "string" },
      },
      strict: true,
    });
    count = Number(values.main);
    if (positionals.length > 0 || !Number.isInteger(count) || count < 1) throw new Error(usage);
    const named = (values.run ?? []).map(Number);
    if (named.some((run) => !Number.isSafeInteger(run) || run < 1)) throw new Error(usage);
    // A run named twice is read, and named in the command, once.
    runs = [...new Set(named)];
    cache = values.cache ?? mkdtempSync(join(tmpdir(), "shard-weights-"));
  } catch {
    console.error(usage);
    process.exit(2);
  }
  mkdirSync(cache, { recursive: true });
  // Every artifact is found before any is downloaded, so a named run the tool refuses costs no
  // download; a named run that is also among main's newest is read once, as main's.
  const artifacts = mainArtifacts(count);
  for (const run of runs) {
    if (!artifacts.some((artifact) => artifact.run === run)) artifacts.push(namedArtifact(run));
  }
  const weights = weightsOf(
    artifacts.map((artifact) => readReport(artifact, cache)),
    new Date().toISOString(),
    `bun e2e/tools/shard-weights.ts --main ${count}${runs.map((run) => ` --run ${run}`).join("")}`
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
