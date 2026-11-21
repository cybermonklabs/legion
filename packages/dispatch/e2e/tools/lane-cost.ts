/** `bun e2e/tools/lane-cost.ts run <run id> [attempt]` and
 *  `bun e2e/tools/lane-cost.ts base <day> <until>`: the readings INFRA-1109's checks deee9ac4
 *  and e62534dd take of the browser lane - one run's cost, and the day's base set, the green
 *  full lanes of the default branch that a head's cost is held against. It is in the tree so that
 *  every reading of the base set uses the same arithmetic.
 *
 *    bun e2e/tools/lane-cost.ts run 44823289704
 *    bun e2e/tools/lane-cost.ts run 44823289704 2
 *    bun e2e/tools/lane-cost.ts base 2026-11-20 2026-11-20T22:12:57Z
 *
 *  `run` prints one run's reading as JSON, by "How a lane's cost is read" in
 *  dispatch://INFRA-1109's verification document. It reads the run's jobs whose name starts
 *  `dispatch-browser`, each at the attempt it last ran in (`filter=latest`), or the jobs of
 *  attempt `<attempt>` where the command names one, and leaves out a job that was skipped and one
 *  that has no `started_at` or no `completed_at`. The run's own fields are read at the same
 *  attempt: `runs/<id>`, which GitHub answers with the run's latest attempt, or
 *  `runs/<id>/attempts/<attempt>`. A run of the nightly workflow, whose jobs are named `full / …`,
 *  is not read: its jobs fail the filter above.
 *
 *  - cost: the sum over those jobs of `completed_at` less `started_at`, in seconds. `per_job`
 *    holds each job's, under its name after `dispatch-browser / `;
 *  - own: the binaries job's seconds, plus the slowest of the legs after it, plus the merge job's;
 *  - wall: the latest `completed_at` less the earliest `started_at`;
 *  - queue: the sum over the jobs of `started_at` less `created_at`;
 *  - notes: the merge job's own `##[notice]` lines. rows: N in the first of them that says
 *    "merged report holds", where it reads "the merged report holds N rows", and null otherwise;
 *  - merge_job: the merge job's id; merge_runner: the first of its labels, and "?" when the job
 *    has no labels; merge_seconds: its seconds, which are `per_job.merge`;
 *  - run: the run's id; head, jobs (how many were read), all_success, and the run's event,
 *    attempt, conclusion and updated_at, as GitHub gives them.
 *
 *  Run 44823289704 reads cost 2,719 s, own 1,223 s, wall 1,229 s, queue 18 s and 1,609 rows.
 *
 *  `base` prints, as JSON and sorted by `updated_at`, the runs of ci.yaml created on `<day>` (UTC)
 *  that count for the base set: event merge_group, the merge queue's runs, which are how ci.yaml
 *  runs the full lane on the tree the default branch lands as (ci.yaml runs on pull_request,
 *  merge_group and workflow_dispatch alone; a dispatch is either the bypass check's run at
 *  main's tip for a push the queue did not test, `main-bypass-check.yaml`, or a run started by
 *  hand, and neither is a run of the queue); concluded success; updated at or before `<until>`;
 *  at least 8 dispatch-browser jobs that were not skipped, every one of them successful; and a
 *  merged report of at least 1,000 rows. Each is printed with its run, event, head, cost, rows,
 *  jobs, own and updated_at.
 *
 *  THE DAY'S LISTING. GitHub lists at most 1,000 runs for a filtered query and drops the rest
 *  without saying so: ci.yaml had 1,695 completed runs on 2026-11-20, and the listing of that day
 *  lost the oldest of them. The lane command of dispatch://INFRA-1109's verification document
 *  says how one run is read and nothing of how the day's runs are listed, which is how a short
 *  base set went unnoticed. So the listing names its event and `status=success`, which GitHub
 *  answers with the runs that concluded success, and its `total_count` is read first. The cap and
 *  the length check apply to those successful runs: a listing GitHub counts at or over 1,000 runs
 *  is refused, naming the event, the day and the count, and so is a listing whose length differs
 *  from its `total_count`. A day refused at the cap is read in parts: `<day>` also takes a range
 *  of GitHub's `created` filter, such as `2026-11-20T00:00:00Z..2026-11-20T11:59:59Z`, and the
 *  parts' base sets are joined.
 *
 *  Every read of GitHub is a `gh api` call that names `repos/cybermonklabs/legion/...` in full,
 *  as the pod's gh, Legion's wrapper, requires. A call that fails stops the reading with the
 *  call, its exit and its stderr, and exit 1. A bad command line prints the usage, and exit 2.
 */
import { parseArgs } from "node:util";

const usage =
  "usage: bun e2e/tools/lane-cost.ts run <run id> [attempt]\n" +
  "       bun e2e/tools/lane-cost.ts base <day> <until>\n" +
  "  run    one run's reading, as JSON: its dispatch-browser jobs, each at the attempt it last\n" +
  "         ran in, or the jobs of <attempt>\n" +
  "  base   the green default-branch full lanes of ci.yaml created on <day> and updated at or\n" +
  "         before <until>, as JSON\n" +
  "  <day>    a UTC date, YYYY-MM-DD, or a range of GitHub's `created` filter, <from>..<to>,\n" +
  "           each a date or a time YYYY-MM-DDTHH:MM:SSZ\n" +
  "  <until>  a UTC time as GitHub writes one, YYYY-MM-DDTHH:MM:SSZ";

const REPO = "cybermonklabs/legion";

/** The most runs GitHub lists for one filtered query (see THE DAY'S LISTING, above). */
const LISTING_CAP = 1000;

/** The event ci.yaml runs the default branch's full lane on: the merge queue's. */
const EVENT = "merge_group";

/** The fewest dispatch-browser jobs, not skipped, of a run of the full lane. */
const FULL_LANE_JOBS = 8;

/** The fewest rows the merged report of a run of the full lane holds. */
const FULL_LANE_ROWS = 1000;

const DATE_OR_TIME = String.raw`\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}:\d{2}Z)?`;
const dayPattern = new RegExp(`^${DATE_OR_TIME}(?:\\.\\.${DATE_OR_TIME})?$`, "u");
/** GitHub's form of a time. `<until>` is compared with `updated_at` as text, which is a
 *  comparison of times only between two texts of this one form. */
const timePattern = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/u;
const idPattern = /^[1-9][0-9]*$/u;

/** A job as GitHub's jobs API gives it, in the fields read here. */
interface Job {
  readonly id: number;
  readonly name: string;
  readonly conclusion: string | null;
  readonly created_at: string;
  readonly started_at: string | null;
  readonly completed_at: string | null;
  readonly labels?: readonly string[] | null;
}

/** A job that started and ended. */
interface RanJob extends Job {
  readonly started_at: string;
  readonly completed_at: string;
}

/** A run as GitHub's workflow runs API gives it, in the fields read here. */
interface WorkflowRun {
  readonly id: number;
  readonly event: string;
  readonly conclusion: string | null;
  readonly head_sha: string;
  readonly run_attempt: number;
  readonly updated_at: string;
}

/** One run's reading, as `run` prints it. */
interface Reading {
  readonly run: string;
  readonly event: string;
  readonly head: string;
  readonly attempt: number;
  readonly conclusion: string | null;
  readonly updated_at: string;
  readonly jobs: number;
  readonly per_job: Readonly<Record<string, number>>;
  readonly all_success: boolean;
  readonly cost: number;
  readonly own: number;
  readonly wall: number;
  readonly queue: number;
  readonly rows: number | null;
  readonly merge_job: number;
  readonly merge_runner: string;
  readonly merge_seconds: number;
  readonly notes: readonly string[];
}

/** One run of the base set, as `base` prints it. */
type BaseRun = Pick<
  Reading,
  "run" | "event" | "head" | "cost" | "rows" | "jobs" | "own" | "updated_at"
>;

type Command =
  | { readonly name: "run"; readonly run: string; readonly attempt: string | undefined }
  | { readonly name: "base"; readonly day: string; readonly until: string };

/** Prints why the reading stops, on stderr, and exits 1. */
function fail(problem: string): never {
  console.error(`lane-cost: ${problem}`);
  process.exit(1);
}

/** The stdout of `gh api <args>`. A call that fails stops the reading. */
function gh(...args: string[]): string {
  const call = Bun.spawnSync(["gh", "api", ...args], {
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  });
  if (!call.success) {
    const stderr = call.stderr.toString().trim().slice(0, 300);
    fail(`gh api ${args.join(" ")}: exit ${call.exitCode}: ${stderr}`);
  }
  return call.stdout.toString();
}

/** The values of a paginated `gh api <endpoint> --jq '...|@json'` listing, one a line. */
function listing<T>(endpoint: string, jq: string): T[] {
  return gh(endpoint, "--paginate", "--jq", `${jq}|@json`)
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as T);
}

/** The instant one of GitHub's ISO 8601 times names, in milliseconds. */
function instant(text: string): number {
  const ms = Date.parse(text);
  if (Number.isNaN(ms)) fail(`${JSON.stringify(text)} is not a time`);
  return ms;
}

/** A job a reading counts: one that was not skipped, and started and ended. */
function ran(job: Job): job is RanJob {
  return job.conclusion !== "skipped" && Boolean(job.started_at) && Boolean(job.completed_at);
}

/** A job's name after its caller's: `shard (1)` of `dispatch-browser / shard (1)`. */
function legOf(name: string): string {
  const at = name.indexOf(" / ");
  if (at < 0) fail(`job ${JSON.stringify(name)} names no leg after "dispatch-browser / "`);
  return name.slice(at + " / ".length);
}

/** The run as GitHub gives it: at its latest attempt, or at `attempt`. */
function workflowRun(run: string, attempt?: string): WorkflowRun {
  const where = attempt === undefined ? `runs/${run}` : `runs/${run}/attempts/${attempt}`;
  return JSON.parse(gh(`repos/${REPO}/actions/${where}`)) as WorkflowRun;
}

/** The run's dispatch-browser jobs: each at the attempt it last ran in, or those of `attempt`. */
function jobsOf(run: string, attempt?: string): Job[] {
  const where =
    attempt === undefined
      ? `runs/${run}/jobs?filter=latest&per_page=100`
      : `runs/${run}/attempts/${attempt}/jobs?per_page=100`;
  return listing<Job>(
    `repos/${REPO}/actions/${where}`,
    '.jobs[]|select(.name|startswith("dispatch-browser"))'
  );
}

/** The reading of the run `info`, from `listed`, its dispatch-browser jobs at the same attempt. */
function reading(info: WorkflowRun, listed: readonly Job[]): Reading {
  const run = String(info.id);
  const jobs = listed.filter(ran);
  if (jobs.length === 0) fail(`run ${run}: no dispatch-browser job started and ended`);
  const perJob = new Map<string, number>();
  let firstStart = Number.POSITIVE_INFINITY;
  let lastEnd = Number.NEGATIVE_INFINITY;
  let queue = 0;
  for (const job of jobs) {
    const started = instant(job.started_at);
    const completed = instant(job.completed_at);
    perJob.set(legOf(job.name), Math.trunc((completed - started) / 1000));
    firstStart = Math.min(firstStart, started);
    lastEnd = Math.max(lastEnd, completed);
    queue += (started - instant(job.created_at)) / 1000;
  }

  const merge =
    jobs.find((job) => job.name.endsWith("/ merge")) ?? fail(`run ${run}: no merge job ran`);
  const log = gh("--allow-escape-sequences", `repos/${REPO}/actions/jobs/${merge.id}/logs`);
  const notes = log
    .split(/\r\n|\r|\n/u)
    .filter((line) => line.includes("##[notice]"))
    .map((line) => line.replace(/^\S+ ##\[notice\]/u, "").trim());
  const rowsNote = notes.find((note) => note.includes("merged report holds"));
  const rowsMatch =
    rowsNote === undefined ? null : /the merged report holds (\d+) rows/u.exec(rowsNote);

  const binaries = perJob.get("binaries") ?? fail(`run ${run}: no binaries job ran`);
  const mergeSeconds = perJob.get("merge") ?? fail(`run ${run}: no job reads as the merge leg`);
  const legs = [...perJob]
    .filter(([leg]) => leg !== "binaries" && leg !== "merge")
    .map(([, seconds]) => seconds);
  if (legs.length === 0) fail(`run ${run}: no leg ran between binaries and merge`);
  let cost = 0;
  for (const seconds of perJob.values()) cost += seconds;

  return {
    run,
    event: info.event,
    head: info.head_sha,
    attempt: info.run_attempt,
    conclusion: info.conclusion,
    updated_at: info.updated_at,
    jobs: jobs.length,
    per_job: Object.fromEntries(perJob),
    all_success: jobs.every((job) => job.conclusion === "success"),
    cost,
    own: binaries + Math.max(...legs) + mergeSeconds,
    wall: Math.trunc((lastEnd - firstStart) / 1000),
    queue: Math.trunc(queue),
    rows: rowsMatch === null ? null : Number(rowsMatch[1]),
    merge_job: merge.id,
    merge_runner: merge.labels?.[0] ?? "?",
    merge_seconds: mergeSeconds,
    notes,
  };
}

/** The number a `--jq .total_count` output gives. */
function totalCount(text: string): number {
  const total = text.trim();
  if (!/^[0-9]+$/u.test(total)) fail(`total_count is ${JSON.stringify(total)}, not a count`);
  return Number(total);
}

/** The successful runs of ci.yaml created on `day`, of the event the full lane runs on. A
 *  listing GitHub may have cut short is refused (see THE DAY'S LISTING, above). */
function dayRuns(day: string): WorkflowRun[] {
  const query = `repos/${REPO}/actions/workflows/ci.yaml/runs?created=${day}&status=success&event=${EVENT}`;
  const total = totalCount(gh(`${query}&per_page=1`, "--jq", ".total_count"));
  if (total >= LISTING_CAP) {
    fail(
      `successful ${EVENT} runs of ${day}: ${total}, at or over GitHub's cap of ${LISTING_CAP} ` +
        "on one listing; split the day"
    );
  }
  const runs = listing<WorkflowRun>(`${query}&per_page=100`, ".workflow_runs[]");
  if (runs.length !== total) {
    fail(`successful ${EVENT} runs of ${day}: listed ${runs.length}, GitHub counts ${total}`);
  }
  return runs;
}

function byUpdatedAt(a: BaseRun, b: BaseRun): number {
  if (a.updated_at < b.updated_at) return -1;
  return a.updated_at > b.updated_at ? 1 : 0;
}

/** The day's base set: its green default-branch full lanes updated at or before `until`. */
function base(day: string, until: string): BaseRun[] {
  const picked: BaseRun[] = [];
  for (const run of dayRuns(day)) {
    if (run.updated_at > until) continue;
    const jobs = jobsOf(String(run.id));
    const notSkipped = jobs.filter((job) => job.conclusion !== "skipped");
    if (
      notSkipped.length < FULL_LANE_JOBS ||
      !notSkipped.every((job) => job.conclusion === "success")
    ) {
      continue;
    }
    const read = reading(run, jobs);
    if (read.rows === null || read.rows < FULL_LANE_ROWS) continue;
    picked.push({
      run: read.run,
      event: read.event,
      head: read.head,
      cost: read.cost,
      rows: read.rows,
      jobs: read.jobs,
      own: read.own,
      updated_at: read.updated_at,
    });
  }
  return picked.sort(byUpdatedAt);
}

/** The command line, or the usage and exit 2. `parseArgs` refuses any option, which this
 *  command line has none of, so that leaves the same way as a bad argument. */
function readArguments(): Command {
  try {
    const { positionals } = parseArgs({ allowPositionals: true, strict: true });
    const [name, ...rest] = positionals;
    if (
      name === "run" &&
      rest.length >= 1 &&
      rest.length <= 2 &&
      rest.every((text) => idPattern.test(text))
    ) {
      return { name, run: rest[0], attempt: rest[1] };
    }
    if (
      name === "base" &&
      rest.length === 2 &&
      dayPattern.test(rest[0]) &&
      timePattern.test(rest[1])
    ) {
      return { name, day: rest[0], until: rest[1] };
    }
    throw new Error(usage);
  } catch {
    console.error(usage);
    process.exit(2);
  }
}

if (import.meta.main) {
  const command = readArguments();
  const result =
    command.name === "run"
      ? reading(workflowRun(command.run, command.attempt), jobsOf(command.run, command.attempt))
      : base(command.day, command.until);
  console.log(JSON.stringify(result, null, 1));
}
