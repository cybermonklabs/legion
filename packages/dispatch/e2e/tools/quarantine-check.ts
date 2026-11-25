/** `bun e2e/tools/quarantine-check.ts`: the half of the browser suite's quarantine that depends on
 *  the date (R4 of dispatch://INFRA-983), which `.github/workflows/dispatch-browser-nightly.yaml`
 *  runs and no pull request does. `e2e/quarantine.ts` holds the list and says what reads it.
 *
 *  It exits 1 when it names any of these:
 *
 *  - a row flaky in `flakyRunsToQuarantine` or more runs of the last `windowDays` days that the
 *    quarantine does not hold, with the runs and the lines to add to `quarantine`, or, for a row
 *    the quarantine cannot take (`quarantineRefusal` of `e2e/quarantine.ts`: a row of the
 *    embedder job or of the two-process job), the lines of a fix entry to add to `fixes`;
 *  - an entry of `quarantine` held longer than `windowDays` days, with its issue and its lines;
 *  - an entry of `fixes` whose commit is not on main;
 *  - a row flaky in that many runs whose file is in the checkout while no listing of the
 *    checkout holds any row of that file: this cannot read the row as it stands. That is a
 *    failed read, and named as one.
 *
 *  A row flaky in that many runs that no row of the checkout's listings is named as, while they
 *  hold other rows of its file, is named with a warning and fails nothing. It was renamed,
 *  deleted, or moved to another file or project since those runs, so its flakes cannot be
 *  matched to a row of the checkout, and no entry can name it. A notice of a run before
 *  cybermonklabs/legion#4523 that names a row inside a `describe` by its own title alone is
 *  named with the row it means, and fails nothing (`countedRuns` says until when). A row whose
 *  file the checkout does not hold does not stand: it is named, and fails nothing.
 *
 *  A fix entry older than `windowDays` days, by its commit's time, is named too, for its owner to
 *  delete, and fails nothing: every flake the window holds is after it.
 *
 *  R4's "flaky twice" counts the row as it stands. A flake counts only from a run whose head -
 *  the commit the run tested, its `head_sha` - holds the commit that last changed the row's own
 *  lines, and holds the row's fix too where `fixes` names one. The row's lines run from its line
 *  in the listing of the checkout to the line before the next row of its file there, or to the
 *  file's end, and their last change is the newest commit of the file's blame at the checkout
 *  that overlaps them. A run's time does not say which commits its head holds: a nightly started
 *  by hand at a branch head blames the branch's own commits, and a merge group of main created
 *  after them holds none of them until the branch lands. A run whose head GitHub's compare cannot
 *  place is counted, and the summary names it with the reason. A change elsewhere in the file
 *  moves nothing. The summary names each run it left out, and why, and counts them.
 *
 *  The runs it reads are the scheduled runs of the nightly and the merge-group runs of ci.yaml,
 *  and no other: a pull request's head can carry its own defect. What it reads of each is the
 *  `dispatch-browser flaky row` notices of its merge job (the "Reading two" step of
 *  .github/workflows/dispatch-browser.yaml's merge job prints one per flaky row): the run listing
 *  through REST (`actions: read`), a day of a workflow a call, and the merge job's check runs in
 *  each run's check suite with their annotations (`checks: read`) through GraphQL, 100 suites a
 *  call and a call more for any page beyond the first (`readSuites`). For the rows flaky in two
 *  runs it also reads the checkout (`readCheckout`), in four listings at once: their files in one
 *  process, the whole suite under each of `twoProcessRuns`, and the embedder job's command. The
 *  last three are `listUnquarantinableFiles`'s, which gives the files the quarantine cannot take
 *  from them; the two-process listings also hold the two-process job's rows, which no
 *  one-process listing holds. It reads each candidate file's blame, one GraphQL request per
 *  file. Then it reads GitHub's compare of each of a row's commits, its last change and its fix,
 *  with the head of each run that named it, one REST call a pair (`readHolds`). It sends every
 *  request itself with the job's token, through one client and its retry ladder (`gitHub`); a
 *  refused blame fails the check, naming the file, and a refused compare leaves its run counted,
 *  and named. For each fix it reads GitHub's compare of the checkout with the fix's commit.
 *
 *  Those reads are the adapter's, below `report`, and they decide nothing; `flakyRows`,
 *  `rowLines`, `lastChange`, `ownTitleRows`, `pairKey`, `decide` and `report` are pure over what
 *  was read and the moment given them, which is how `e2e/__tests__/quarantine-check.test.ts`
 *  drives them. It drives `blame` against a stand-in for GitHub's answer, `readDeadline` against
 *  a stand-in for `/proc`, `gitHub`, `readHistory` and `readHolds` against a local stand-in for
 *  GitHub's API, and `readCheckout` against that stand-in and listings of the checkout. Only the
 *  command line, `readDeadline` and `gitHub`'s wait for a rate limit read the clock or the
 *  processes.
 *
 *  Environment: GITHUB_REPOSITORY (`owner/name`), GH_TOKEN or GITHUB_TOKEN, GITHUB_SHA (the
 *  commit checked out; `main` when unset), GITHUB_API_URL (https://api.github.com when unset),
 *  JOB_TIMEOUT_MINUTES and STEP_TIMEOUT_MINUTES (the job's and the step's timeout-minutes, which
 *  the nightly's job and step set: the reading waits for GitHub's rate limit only where the wait
 *  ends `waitMarginSeconds` before the earlier of the two limits, as `readDeadline` reads them,
 *  and with neither set it waits for nothing) and, where it is set, GITHUB_STEP_SUMMARY, which
 *  the summary is appended to as well as printed. It writes nothing to GitHub. A read GitHub
 *  refuses - three times, or once with a 403 that names no rate limit, or with a rate limit whose
 *  time falls past that deadline - exits 1 too, and says it is a failed read rather than a
 *  finding; a refused compare of a run's head (`readHolds`) alone leaves that run counted, and
 *  named. Its last line on stderr gives the REST and GraphQL calls it made, the GraphQL points
 *  they cost, and what the token has left of each budget. Usage errors exit 2.
 *  From a workstation, with gh signed in, in a checkout of the commit GITHUB_SHA names:
 *
 *    GITHUB_REPOSITORY=cybermonklabs/legion GH_TOKEN="$(gh auth token)" GITHUB_SHA=<sha> \
 *      bun e2e/tools/quarantine-check.ts
 */
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { listing, listUnquarantinableFiles } from "../__tests__/playwright-listing";
import {
  entryLines,
  type FixEntry,
  fixes,
  noUnquarantinableFiles,
  type QuarantineEntry,
  type QuarantineRow,
  quarantine,
  quarantineRefusal,
  rowName,
  titleSeparator,
  type UnquarantinableFiles,
} from "../quarantine";

/** How far back a flaky row counts, and how long an entry may be held: R4's 14 days. */
export const windowDays = 14;

/** How many runs a row must be flaky in for the quarantine to owe it an entry: R4's "twice". */
export const flakyRunsToQuarantine = 2;

/** The runs whose flaky rows count, each with the workflow that holds them and the name of its
 *  merge job's check run: a called workflow's job is named `<the caller's job> / <its job>`, and
 *  ci.yaml calls dispatch-browser.yaml from `dispatch-browser`. The nightly has called it from a
 *  job of that id too since cybermonklabs/legion#5175 landed on main (4a3eb021, created
 *  2026-11-23T21:37:21Z), and from `full` before it. A scheduled run runs main's copy of the
 *  workflow as it stood when the run was created, so a nightly run created before that moment
 *  named its merge job `full / merge`, and `before` reads such a run under that name: each run is
 *  read under the one name it carries (`mergeJobOf`). The old name matters while a run created
 *  before the rename is inside the `windowDays` window read back from now, which is until
 *  2026-12-07T21:37:21Z; `before` can be deleted after that.
 *
 *  The notices' names changed the same way. The merge job of a run whose head holds
 *  cybermonklabs/legion#4523 names a row by its full title path, its `describe` titles before its
 *  own (`.github/scripts/dispatch-browser-reading-flake-rate.sh`), and that of a run whose head
 *  does not names it by its own title alone. So until 14 days after that change lands on main,
 *  a notice of an older run names a row inside a `describe` by a name no row of the checkout's
 *  listings has, and its flakes are not counted: `readCheckout` finds the rows whose full title
 *  ends in that title (`ownTitleRows`), and `decide` names them in `ownTitle`, each with the row
 *  it means, rather than warning of them as it does of a row renamed, deleted or moved
 *  (`unmatched`). The reading is of the notice's shape, so until then a row moved into a
 *  `describe` whose notices name its title from before the move reads the same way;
 *  `ownTitleRows` can be deleted after that. */
export const countedRuns = [
  {
    before: { at: "2026-11-23T21:37:21Z", mergeJob: "full / merge" },
    event: "schedule",
    mergeJob: "dispatch-browser / merge",
    workflow: "dispatch-browser-nightly.yaml",
  },
  { event: "merge_group", mergeJob: "dispatch-browser / merge", workflow: "ci.yaml" },
] as const;

/** The name of the merge job's check run of the run of `counted` created at `createdAt`:
 *  `counted.before`'s for a run created before its moment, `counted.mergeJob` otherwise. */
export function mergeJobOf(counted: (typeof countedRuns)[number], createdAt: string): string {
  return "before" in counted && Date.parse(createdAt) < Date.parse(counted.before.at)
    ? counted.before.mergeJob
    : counted.mergeJob;
}

/** What one run's merge jobs said. */
export interface RunReading {
  readonly id: number;
  /** The run's event: `schedule`, `merge_group`, `pull_request`. */
  readonly event: string;
  /** When the run was created, as GitHub gives it. */
  readonly createdAt: string;
  /** The commit the run tested, the run listing's `head_sha`: a merge group's merge commit, or
   *  the commit of main a scheduled run checked out. */
  readonly headSha: string;
  /** The run's page. */
  readonly url: string;
  /** The message of every `dispatch-browser flaky row` notice of its merge jobs, one merge job
   *  per attempt: `<file>:<line> [<project>] <title>`. */
  readonly flakyRows: readonly string[];
  /** How many flaky rows its flake-rate notices counted beyond the row notices read: rows that
   *  are in that run's summary and not in its annotations. */
  readonly unread: number;
}

/** One range of a file's blame, as GitHub's GraphQL `Commit.blame` gives it. */
export interface BlameRange {
  readonly startingLine: number;
  readonly endingLine: number;
  readonly commit: { readonly oid: string; readonly committedDate: string };
}

/** The last change of one row's own lines on main. */
export interface RowChange {
  readonly oid: string;
  readonly committedDate: string;
  /** The lines the change was read over, `<file>:<start>-<end>`. */
  readonly lines: string;
}

/** A fix entry with its commit's time on main, or `undefined` where main does not hold it. */
export interface FixReading {
  readonly entry: FixEntry;
  readonly committedDate: string | undefined;
}

/** The counted runs, and each row their notices name with the runs that named it. */
export interface FlakyRows {
  readonly counted: readonly RunReading[];
  readonly rows: ReadonlyMap<string, { readonly row: QuarantineRow; readonly runs: RunReading[] }>;
  readonly unparsed: readonly { readonly run: RunReading; readonly message: string }[];
}

/** What the readings say of the quarantine. */
export interface Decision extends FlakyRows {
  /** Each row flaky in enough runs whose heads hold its last change, and its fix where `fixes`
   *  names one, that the quarantine does not hold. */
  readonly toQuarantine: readonly {
    readonly row: QuarantineRow;
    readonly runs: readonly RunReading[];
    readonly change: RowChange;
  }[];
  /** Each run a row flaky in enough runs was not counted in, what its head does not hold
   *  (`missing`: the row's last change, or its fix), and why. */
  readonly leftOut: readonly {
    readonly row: QuarantineRow;
    readonly run: RunReading;
    readonly missing: "change" | "fix";
    readonly why: string;
  }[];
  /** Each run counted for a row flaky in enough runs though it cannot be told whether its head
   *  holds the row's last change or its fix, and why. */
  readonly unplaced: readonly {
    readonly row: QuarantineRow;
    readonly run: RunReading;
    readonly why: string;
  }[];
  /** A row flaky in enough runs whose file the checkout does not hold: the row does not stand. */
  readonly absent: readonly QuarantineRow[];
  /** A row flaky in enough runs whose notices name by its own title alone a row inside a
   *  `describe` of its file and project that the checkout's listings hold, `listed` by `rowName`,
   *  as a run whose head does not hold cybermonklabs/legion#4523 named such a row (`countedRuns`
   *  says until when). Its flakes are not counted. */
  readonly ownTitle: readonly { readonly row: QuarantineRow; readonly listed: readonly string[] }[];
  /** A row flaky in enough runs whose name no row of the checkout's listings has, while they hold
   *  other rows of its file, with the runs that named it: a row renamed, deleted, or moved to
   *  another file or project since those runs. Their notices cannot be matched to a row of the
   *  checkout, so its flakes are not counted. It is named with a warning and fails nothing: no
   *  entry can name a row the suite does not run (`e2e/__tests__/quarantine.test.ts`). */
  readonly unmatched: readonly {
    readonly row: QuarantineRow;
    readonly runs: readonly RunReading[];
  }[];
  /** A row flaky in enough runs whose file the checkout holds while no listing of the checkout
   *  holds any row of that file, with the runs that named it. This reading cannot read the row
   *  as it stands. A failed read: its flakes are not counted, and the check fails. */
  readonly unlisted: readonly {
    readonly row: QuarantineRow;
    readonly runs: readonly RunReading[];
  }[];
  /** Each quarantine entry held longer than the window, with how many days it has been held. */
  readonly overdue: readonly { readonly entry: QuarantineEntry; readonly days: number }[];
  /** Each fix entry whose commit main does not hold. */
  readonly offMain: readonly FixEntry[];
  /** Each fix entry whose commit is older than the window, which does nothing any more. */
  readonly spent: readonly { readonly entry: FixEntry; readonly committedDate: string }[];
}

const dayMs = 86_400_000;
const noticePattern = /^(?<file>[^\s:]+):(?<line>\d+) \[(?<project>[^\]]+)\] (?<title>.+)$/u;
const flakyRowTitle = "dispatch-browser flaky row";
const flakeRateTitle = "dispatch-browser flake rate";
const quarantinePath = "packages/dispatch/e2e/quarantine.ts";

/** The runs that count at `now` - of an event of `countedRuns`, created in the `windowDays` days
 *  before it - and each row their notices name. A row counts once per run, however many
 *  attempts named it, since a re-run's merge job joins the blob reports of the legs it did not
 *  re-run and names their flaky rows again. */
export function flakyRows(runs: readonly RunReading[], now: Date): FlakyRows {
  const end = now.getTime();
  const events = new Set<string>(countedRuns.map((counted) => counted.event));
  const counted = runs.filter((run) => {
    const created = Date.parse(run.createdAt);
    return events.has(run.event) && created >= end - windowDays * dayMs && created <= end;
  });
  const rows = new Map<string, { row: QuarantineRow; runs: RunReading[] }>();
  const unparsed: { run: RunReading; message: string }[] = [];
  for (const run of counted) {
    const named = new Set<string>();
    for (const message of run.flakyRows) {
      const groups = noticePattern.exec(message)?.groups;
      if (groups === undefined) {
        unparsed.push({ message, run });
        continue;
      }
      const row = { file: groups.file, project: groups.project, title: groups.title };
      const name = rowName(row);
      if (named.has(name)) continue;
      named.add(name);
      const seen = rows.get(name) ?? { row, runs: [] };
      seen.runs.push(run);
      rows.set(name, seen);
    }
  }
  return { counted, rows, unparsed };
}

/** A row's own lines: from `line`, its line in the listing, to the line before the next row of
 *  its file there (`fileRows`, every row's line), or to the file's last line, `fileLength`. */
export function rowLines(
  line: number,
  fileRows: readonly number[],
  fileLength: number
): { start: number; end: number } {
  const next = fileRows.filter((other) => other > line).sort((a, b) => a - b)[0];
  return { end: next === undefined ? fileLength : next - 1, start: line };
}

/** The newest commit of `ranges` that overlaps lines `start` to `end`, or `undefined` where none
 *  does. */
export function lastChange(
  ranges: readonly BlameRange[],
  start: number,
  end: number
): BlameRange["commit"] | undefined {
  return ranges
    .filter((range) => range.startingLine <= end && range.endingLine >= start)
    .map((range) => range.commit)
    .sort((a, b) => Date.parse(b.committedDate) - Date.parse(a.committedDate))[0];
}

/** The rows of `listed` that a notice naming `row` by its own title alone may mean, by `rowName`:
 *  those of its file and project inside a `describe` whose full title ends in its title. A run
 *  whose head does not hold cybermonklabs/legion#4523 named such a row that way (`countedRuns`
 *  says until when this matters). */
export function ownTitleRows(row: QuarantineRow, listed: readonly QuarantineRow[]): string[] {
  return listed
    .filter(
      (other) =>
        other.file === row.file &&
        other.project === row.project &&
        other.title.endsWith(`${titleSeparator}${row.title}`)
    )
    .map(rowName);
}

/** What the checkout says of one candidate row, a row flaky in `flakyRunsToQuarantine` runs, by
 *  `kind`:
 *
 *  - `listed`: a row of the checkout's listings is named as it, by `rowName`. `change` is the
 *    last change of its own lines: their newest commit in its file's blame at the checkout.
 *  - `ownTitle`: no row is named as it, and `listed` names, by `rowName`, the rows its notices
 *    may mean by its own title alone (`ownTitleRows`).
 *  - `unmatched`: no row is named as it or is such a row, while the listings hold other rows of
 *    its file: it was renamed, deleted, or moved to another file or project.
 *  - `unlisted`: the checkout holds its file, and no listing holds any row of that file.
 *  - `gone`: the checkout does not hold its file. */
export type CandidateReading =
  | { readonly kind: "listed"; readonly change: RowChange }
  | { readonly kind: "ownTitle"; readonly listed: readonly string[] }
  | { readonly kind: "unmatched" }
  | { readonly kind: "unlisted" }
  | { readonly kind: "gone" };

/** Whether a run's head holds a commit, by `pairKey`: `true` or `false` as GitHub's compare of
 *  the two answers, or, where the compare cannot tell, why. */
export type Holds = ReadonlyMap<string, boolean | string>;

/** The key of `Holds` for whether `head` holds `commit`: the range of their compare. */
export function pairKey(commit: string, head: string): string {
  return `${commit}...${head}`;
}

/** A commit the head of a run must hold for a flake of a row in it to count: what its absence is
 *  (`missing`), the commit, and how the summary names it (`what`). */
interface RowCommit {
  readonly missing: "change" | "fix";
  readonly oid: string;
  readonly what: string;
}

/** The commits a run's head must hold for a flake of a row in it to count: the last change of the
 *  row's own lines, `change`, and its fix where `fixes` names one. */
function rowCommits(change: RowChange, fix: FixEntry | undefined): RowCommit[] {
  const commits: RowCommit[] = [
    {
      missing: "change",
      oid: change.oid,
      what: `its own lines' last change, ${change.oid} at ${change.committedDate}`,
    },
  ];
  if (fix !== undefined) {
    commits.push({ missing: "fix", oid: fix.fixed, what: `its fix, ${fix.fixed}` });
  }
  return commits;
}

/** What `runs` say of `entries` and `fixReadings` at `now`, with `readings` what the checkout says
 *  of each row flaky in `flakyRunsToQuarantine` runs, by `rowName`, and `holds` whether the head of
 *  each run that named such a row holds the row's last change and its fix (`readCheckout` and
 *  `readHolds` read them). A `listed` row counts a run whose head holds its last change and,
 *  where `fixes` names a fix of it, that fix too. A run whose head `holds` cannot place against
 *  one of those commits - GitHub could not tell, or the pair was not read - is counted, and named
 *  in `unplaced` with the reason. Any other row is named in the field of `Decision` its reading
 *  says: `ownTitle`, `unmatched`, `unlisted`, which fails the check, or `absent` for one `gone`.
 *  A row with no reading throws: `readCheckout` reads one for each such row, so a missing one is
 *  a defect of the reading, and is not read as a row that passes. A quarantine entry is overdue
 *  when more than `windowDays` calendar days (UTC) lie between the day it was added and the day
 *  of `now`. */
export function decide(
  runs: readonly RunReading[],
  entries: readonly QuarantineEntry[],
  fixReadings: readonly FixReading[],
  readings: ReadonlyMap<string, CandidateReading>,
  now: Date,
  holds: Holds
): Decision {
  const flaky = flakyRows(runs, now);
  const held = new Set(entries.map(rowName));
  const toQuarantine: { row: QuarantineRow; runs: RunReading[]; change: RowChange }[] = [];
  const leftOut: Decision["leftOut"][number][] = [];
  const unplaced: Decision["unplaced"][number][] = [];
  const absent: QuarantineRow[] = [];
  const ownTitle: Decision["ownTitle"][number][] = [];
  const unmatched: Decision["unmatched"][number][] = [];
  const unlisted: Decision["unlisted"][number][] = [];
  for (const [name, { row, runs: named }] of [...flaky.rows].sort(([a], [b]) =>
    a.localeCompare(b)
  )) {
    if (named.length < flakyRunsToQuarantine) continue;
    const reading = readings.get(name);
    if (reading === undefined) {
      throw new Error(
        `${name} was flaky in ${named.length} runs, and the checkout's reading says nothing of ` +
          "it: readCheckout reads every row flaky in that many runs, so the quarantine was not " +
          "checked."
      );
    }
    if (reading.kind === "gone") absent.push(row);
    else if (reading.kind === "ownTitle") ownTitle.push({ listed: reading.listed, row });
    else if (reading.kind === "unmatched") unmatched.push({ row, runs: named });
    else if (reading.kind === "unlisted") unlisted.push({ row, runs: named });
    if (reading.kind !== "listed") continue;
    const { change } = reading;
    const fix = fixReadings.find((fixReading) => rowName(fixReading.entry) === name)?.entry;
    const commits = rowCommits(change, fix);
    const counting: RunReading[] = [];
    for (const run of named) {
      const placed = commits.map((commit) => ({
        ...commit,
        holds: holds.get(pairKey(commit.oid, run.headSha)) ?? "the pair was not compared",
      }));
      const lacking = placed.find((commit) => commit.holds === false);
      if (lacking !== undefined) {
        leftOut.push({
          missing: lacking.missing,
          row,
          run,
          why: `its head ${run.headSha} does not hold ${lacking.what}`,
        });
        continue;
      }
      for (const commit of placed) {
        if (typeof commit.holds === "string") {
          unplaced.push({
            row,
            run,
            why:
              `counted, though it cannot be told whether its head ${run.headSha} holds ` +
              `${commit.what}: ${commit.holds}`,
          });
        }
      }
      counting.push(run);
    }
    if (counting.length >= flakyRunsToQuarantine && !held.has(name)) {
      toQuarantine.push({ change, row, runs: counting });
    }
  }
  const today = Date.parse(`${now.toISOString().slice(0, 10)}T00:00:00Z`);
  const overdue = entries
    .map((entry) => ({
      days: Math.round((today - Date.parse(`${entry.added}T00:00:00Z`)) / dayMs),
      entry,
    }))
    .filter(({ days }) => days > windowDays);
  const spent: { entry: FixEntry; committedDate: string }[] = [];
  for (const { committedDate, entry } of fixReadings) {
    if (
      committedDate !== undefined &&
      Date.parse(committedDate) < now.getTime() - windowDays * dayMs
    ) {
      spent.push({ committedDate, entry });
    }
  }
  return {
    ...flaky,
    absent,
    leftOut,
    offMain: fixReadings
      .filter((reading) => reading.committedDate === undefined)
      .map((r) => r.entry),
    overdue,
    ownTitle,
    spent,
    toQuarantine,
    unlisted,
    unmatched,
    unplaced,
  };
}

/** `entry`'s lines as `entryLines` prints them, fenced as TypeScript under a list item of the
 *  summary, with a blank line before and after. */
function fencedEntry(entry: QuarantineEntry | FixEntry): string[] {
  return ["", "  ```ts", ...entryLines(entry).map((line) => `  ${line}`), "  ```", ""];
}

/** The decision for a person and for the log: `summary` is Markdown, the step summary's;
 *  `annotations` are workflow commands, one per finding; `failed` is whether the run must fail.
 *  A row to quarantine whose file is one of `unquarantinable` (`quarantineRefusal`) is named
 *  with the lines of a fix entry in place of a quarantine entry's. */
export function report(
  decision: Decision,
  now: Date,
  unquarantinable: UnquarantinableFiles
): { annotations: string[]; failed: boolean; summary: string[] } {
  const today = now.toISOString().slice(0, 10);
  const runLink = (run: RunReading) =>
    `[run ${run.id}](${run.url}) of ${run.createdAt.slice(0, 10)} (${run.event})`;
  const flaky = decision.counted.filter((run) => run.flakyRows.length > 0);
  const setAside = { change: 0, fix: 0 };
  for (const { missing } of decision.leftOut) setAside[missing] += 1;
  const summary = [
    `### The quarantine on ${today}`,
    "",
    `Read the merge jobs of ${decision.counted.length} runs of the ${windowDays} days before ` +
      `${now.toISOString()}: ` +
      countedRuns
        .map(
          (counted) =>
            `${decision.counted.filter((run) => run.event === counted.event).length} ` +
            `${counted.event} runs of ${counted.workflow}`
        )
        .join(" and ") +
      `. ${flaky.length} of them named a flaky row. Flakes set aside because the run's head did ` +
      `not hold the row's last change: ${setAside.change}; because it did not hold the row's ` +
      `fix: ${setAside.fix}. A pull request's runs are not read: its head can carry its own ` +
      "defect.",
    "",
    `#### Flaky in ${flakyRunsToQuarantine} or more runs, and not quarantined`,
    "",
  ];
  const annotations: string[] = [];
  if (decision.toQuarantine.length === 0) {
    summary.push(
      `No row the quarantine does not hold was flaky in ${flakyRunsToQuarantine} runs whose ` +
        "heads hold its last change."
    );
  }
  for (const { change, row, runs } of decision.toQuarantine) {
    const refusal = quarantineRefusal(row, unquarantinable);
    summary.push(
      `- \`${rowName(row)}\`, flaky in ${runs.length} runs whose heads hold the last change of ` +
        `its own lines (${change.lines}), ${change.oid} at ${change.committedDate}: ` +
        `${runs.map(runLink).join(", ")}.`,
      ""
    );
    const urls = runs.map((run) => run.url).join(" ");
    if (refusal === undefined) {
      summary.push(
        `  The controller files the issue that owns it. Add these lines to \`quarantine\` in ` +
          `\`${quarantinePath}\`, with that issue's key for \`<KEY>\`:`,
        ...fencedEntry({ ...row, added: today, issue: "dispatch://<KEY>" })
      );
      annotations.push(
        `::error title=dispatch-browser quarantine::${rowName(row)} was flaky in ${runs.length} ` +
          `runs of ${windowDays} days and is not quarantined: ${urls}`
      );
      continue;
    }
    summary.push(
      `  The quarantine cannot take it: its file is \`${row.file}\`, ${refusal}. Fix the row in ` +
        "its own lines or write a fix entry. A change to its own lines needs no entry: no flake " +
        "of a run whose head does not hold that change counts. For a fix elsewhere, the " +
        "controller files the issue that owns the row, and once the commit that fixes it is on " +
        `main, add these lines to \`fixes\` in \`${quarantinePath}\`, with that issue's key for ` +
        "`<KEY>` and the commit's full sha for `<SHA>`:",
      ...fencedEntry({ ...row, fixed: "<SHA>", issue: "dispatch://<KEY>" })
    );
    annotations.push(
      `::error title=dispatch-browser quarantine::${rowName(row)} was flaky in ${runs.length} ` +
        `runs of ${windowDays} days, and the quarantine cannot take a row of ${row.file}: fix ` +
        `the row in its own lines or write a fix entry: ${urls}`
    );
  }
  if (decision.unlisted.length > 0) {
    summary.push("", "#### Flaky rows of a file no listing holds", "");
  }
  for (const { row, runs } of decision.unlisted) {
    summary.push(
      `- \`${rowName(row)}\`, flaky in ${runs.length} runs: ${runs.map(runLink).join(", ")}. ` +
        `The checkout holds its file, \`${row.file}\`, and no listing of the checkout holds any ` +
        "row of that file: the file holds no row now, or a job runs it in a way these listings " +
        "do not. So this reading cannot read the row as it stands, and its flakes are not " +
        "counted. This is a failed read of those runs and not a finding."
    );
    annotations.push(
      `::error title=dispatch-browser quarantine::${rowName(row)} was flaky in ${runs.length} ` +
        `runs of ${windowDays} days, and no listing of the checkout holds any row of ` +
        `${row.file}, which the checkout holds, so this is a failed read and its flakes are not ` +
        `counted: ${runs.map((run) => run.url).join(" ")}`
    );
  }
  if (
    decision.leftOut.length > 0 ||
    decision.ownTitle.length > 0 ||
    decision.unmatched.length > 0 ||
    decision.absent.length > 0
  ) {
    summary.push("", "#### Flakes left out", "");
  }
  for (const { row, run, why } of decision.leftOut) {
    summary.push(`- \`${rowName(row)}\` in ${runLink(run)}: ${why}.`);
  }
  for (const { listed, row } of decision.ownTitle) {
    summary.push(
      `- \`${rowName(row)}\`: its notices name a row inside a \`describe\` by its own title ` +
        "alone, as a run whose head does not hold cybermonklabs/legion#4523 named one, and the " +
        `checkout's listings name that row ${listed.map((name) => `\`${name}\``).join(" or ")}. ` +
        "Its flakes are not counted."
    );
  }
  for (const { row, runs } of decision.unmatched) {
    summary.push(
      `- \`${rowName(row)}\`, flaky in ${runs.length} runs: ${runs.map(runLink).join(", ")}. ` +
        `The listings of its file, \`${row.file}\`, hold no row of that title in its project, ` +
        `\`${row.project}\`: the row was renamed, deleted, or moved to another file or project ` +
        "since those runs, and their notices cannot be matched to a row of the checkout. Its " +
        "flakes are not counted."
    );
    annotations.push(
      `::warning title=dispatch-browser quarantine::${rowName(row)} was flaky in ${runs.length} ` +
        `runs of ${windowDays} days, and the listings of ${row.file} hold no row of that title ` +
        `in ${row.project}: a row renamed, deleted, or moved to another file or project since ` +
        `then, so its flakes are not counted: ${runs.map((run) => run.url).join(" ")}`
    );
  }
  for (const row of decision.absent) {
    summary.push(
      `- \`${rowName(row)}\`: the checkout does not hold its file, \`${row.file}\`, so the row ` +
        "does not stand."
    );
  }
  if (decision.unplaced.length > 0) {
    summary.push("", "#### Flakes counted from runs that could not be placed", "");
  }
  for (const { row, run, why } of decision.unplaced) {
    summary.push(`- \`${rowName(row)}\` in ${runLink(run)}: ${why}.`);
  }
  summary.push("", `#### Held longer than ${windowDays} days`, "");
  if (decision.overdue.length === 0) {
    summary.push(`No entry has been held longer than ${windowDays} days.`);
  }
  for (const { days, entry } of decision.overdue) {
    summary.push(
      `- \`${rowName(entry)}\`, added ${entry.added}, ${days} days ago, owned by ${entry.issue}. ` +
        `Once that issue's fix lands, remove these lines from \`${quarantinePath}\`:`,
      ...fencedEntry(entry)
    );
    annotations.push(
      `::error title=dispatch-browser quarantine::${rowName(entry)} has been quarantined ` +
        `${days} days, more than ${windowDays}; its issue is ${entry.issue}`
    );
  }
  if (decision.offMain.length > 0 || decision.spent.length > 0) {
    summary.push("", "#### Fixes", "");
  }
  for (const entry of decision.offMain) {
    summary.push(
      `- \`${rowName(entry)}\` names its fix as ${entry.fixed}, which main does not hold. ` +
        `Write the commit on main that fixed it in \`${quarantinePath}\`.`
    );
    annotations.push(
      `::error title=dispatch-browser quarantine::the fix of ${rowName(entry)}, ${entry.fixed}, ` +
        "is not on main"
    );
  }
  for (const { committedDate, entry } of decision.spent) {
    summary.push(
      `- \`${rowName(entry)}\`, fixed in ${entry.fixed} at ${committedDate}, more than ` +
        `${windowDays} days ago, so the entry does nothing. Its owner, ${entry.issue}, removes ` +
        `these lines from \`${quarantinePath}\`:`,
      ...fencedEntry(entry)
    );
  }
  const unread = decision.counted.filter((run) => run.unread > 0);
  if (unread.length > 0 || decision.unparsed.length > 0) {
    summary.push("", "#### What this reading could not count", "");
  }
  for (const run of unread) {
    summary.push(
      `- ${runLink(run)}: its flake rate counts ${run.unread} more flaky rows than its row ` +
        "notices name, so those rows are in its summary alone and are not counted here."
    );
    annotations.push(
      `::warning title=dispatch-browser quarantine::run ${run.id} counts ${run.unread} flaky ` +
        "rows that no row notice names, and they are not counted"
    );
  }
  for (const { message, run } of decision.unparsed) {
    summary.push(`- ${runLink(run)}: a flaky-row notice that names no row: \`${message}\`.`);
  }
  return {
    annotations,
    failed:
      decision.toQuarantine.length > 0 ||
      decision.unlisted.length > 0 ||
      decision.overdue.length > 0 ||
      decision.offMain.length > 0,
    summary,
  };
}

/** The reads `readHistory` and the rest make, and what they cost. */
export interface GitHub {
  /** Every page of `path`, the list of each page taken from `key` of its body (or the body itself
   *  where it is a list), with the `total_count` the last page gave. */
  everyPage<Item>(path: string, key: string): Promise<{ items: Item[]; total: number }>;
  /** The body of `path`, or `undefined` where GitHub has no such thing (404 or 422). */
  one(path: string): Promise<unknown>;
  /** The body GitHub's GraphQL endpoint answers `query` with, on the same ladder. */
  graphql(query: string, variables: Record<string, unknown>): Promise<unknown>;
  /** The calls made so far, retries included, REST's and GraphQL's apart; the points GraphQL's
   *  answers gave as their `rateLimit { cost }`, summed; and what each budget had left, as the
   *  last REST answer's `x-ratelimit-remaining` and the last GraphQL answer's
   *  `rateLimit { remaining }` gave it. */
  spent(): {
    calls: number;
    graphqlCalls: number;
    graphqlCost: number;
    graphqlRemaining: number | null;
    remaining: string | null;
  };
}

/** How long a wait for a primary rate limit runs past the reset GitHub names, in seconds: the
 *  reset is a whole second of GitHub's clock, which the runner's may stand behind. */
const resetMarginSeconds = 3;

/** How long a call waits for a secondary rate limit whose answer names no `retry-after`, in
 *  seconds: GitHub's documented minimum of one minute (REST API, "Rate limits for the REST API",
 *  "Exceeding the rate limit"), which gh-read.sh's gh_wait_reset waits too. */
const secondaryLimitSeconds = 60;

/** A secondary rate limit as GitHub's answer words it, read as gh-read.sh's `_gh_read_refusal`
 *  reads it: "You have exceeded a secondary rate limit", or GitHub's older "abuse detection
 *  mechanism". */
const secondaryLimitPattern = /secondary rate limit|abuse detection mechanism/i;

const failedRead =
  "This is a failed read of the runs and not a finding: the quarantine was not checked.";

/** Seconds the deadline must leave after the time GitHub names: GH_WAIT_MARGIN of
 *  `.github/scripts/gh-read.sh`, whose gh_wait_reset holds the scripts' waits to the same rule.
 *  The call after the wait, and the reading's work after it, run in that time. */
export const waitMarginSeconds = 30;

/** The latest moment a wait may end (`at`, epoch milliseconds) and the limit it is (`of`), or why
 *  there is none (`none`), in which case `gitHub` waits for nothing. */
export type Deadline =
  | { readonly at: number; readonly of: "job" | "step" }
  | { readonly none: string };

/** The fields of a `/proc/<pid>/stat` line after its comm, and the comm: the comm can hold spaces
 *  and parentheses, so the fields are read after its last `)`. `undefined` where the file cannot
 *  be read. */
function procStat(proc: string, pid: number): { comm: string; fields: string[] } | undefined {
  let line: string;
  try {
    line = readFileSync(`${proc}/${pid}/stat`, "utf8");
  } catch {
    return undefined;
  }
  const close = line.lastIndexOf(")");
  return {
    comm: line.slice(line.indexOf("(") + 1, close),
    fields: line
      .slice(close + 2)
      .trim()
      .split(/\s+/u),
  };
}

/** The deadline gh-read.sh's `_gh_read_deadline` reads, read the same way: the earlier of
 *  JOB_TIMEOUT_MINUTES after the job's start and STEP_TIMEOUT_MINUTES after the step's, of those
 *  `env` sets. GitHub enforces a job's timeout-minutes from the job's start and a step's from the
 *  step's, and the runner exposes neither start to a step, so both are read off the process tree
 *  under `proc`: the runner starts one Runner.Worker process for each job, before its first step,
 *  and each step's shell is that process's child. The nearest Runner.Worker among the parents of
 *  `pid` is the job's start, and its child on the way down is the step's (this process itself,
 *  where the step's shell ran it with `exec`, which keeps the start). The start times are clock
 *  ticks since boot, `btime` of `proc`'s `stat`, at `ticksPerSecond` (`getconf CLK_TCK` where it
 *  is not given). With neither limit set, a limit that is no whole number of minutes, or no
 *  Runner.Worker among the parents (a container job, a workstation), there is no deadline.
 *
 *  Read here rather than passed in by the step: the step could only read the same tree in shell,
 *  and node:fs reads `/proc` in Bun as it reads any file. */
export function readDeadline(
  env: Readonly<Record<string, string | undefined>>,
  {
    pid = process.pid,
    proc = "/proc",
    ticksPerSecond,
  }: { pid?: number; proc?: string; ticksPerSecond?: number } = {}
): Deadline {
  const job = env.JOB_TIMEOUT_MINUTES ?? "";
  const step = env.STEP_TIMEOUT_MINUTES ?? "";
  if (job === "" && step === "") {
    return {
      none: "this step names no time limit to wait within (JOB_TIMEOUT_MINUTES is not set)",
    };
  }
  for (const [name, value] of [
    ["JOB_TIMEOUT_MINUTES", job],
    ["STEP_TIMEOUT_MINUTES", step],
  ]) {
    if (value !== "" && !/^\d+$/u.test(value)) {
      return { none: `${name} is '${value}', not a whole number of minutes` };
    }
  }
  let worker: { comm: string; fields: string[] } | undefined;
  let child: { comm: string; fields: string[] } | undefined;
  for (let at = pid; at > 1; ) {
    const stat = procStat(proc, at);
    if (stat === undefined) break;
    if (stat.comm === "Runner.Worker") {
      worker = stat;
      break;
    }
    child = stat;
    at = Number(stat.fields[1]);
  }
  if (worker === undefined || child === undefined) {
    return {
      none: "this step's start cannot be read: no Runner.Worker process is among its parents",
    };
  }
  const boot = Number(/^btime (\d+)$/mu.exec(readFileSync(`${proc}/stat`, "utf8"))?.[1]);
  const ticks =
    ticksPerSecond ?? Number(Bun.spawnSync(["getconf", "CLK_TCK"]).stdout.toString().trim());
  if (!Number.isInteger(boot) || !Number.isInteger(ticks) || ticks <= 0) {
    return { none: "this step's start cannot be read: no boot time or clock tick to read it in" };
  }
  const end = (stat: { fields: string[] }, minutes: string) =>
    boot + Math.floor(Number(stat.fields[19]) / ticks) + Number(minutes || "0") * 60;
  const jobEnd = end(worker, job);
  const stepEnd = end(child, step);
  return job !== "" && (step === "" || jobEnd <= stepEnd)
    ? { at: jobEnd * 1000, of: "job" }
    : { at: stepEnd * 1000, of: "step" };
}

/** The time GitHub's answer's `headers` say it may be asked again, and what in them names it
 *  (`named`): `retry-after` seconds from now where it is present (a secondary limit's), never
 *  sooner than a second from now, so a `retry-after: 0` is not asked again in a tight loop; or
 *  else `resetMarginSeconds` past `x-ratelimit-reset` where `x-ratelimit-remaining` is 0 (a
 *  primary limit's, REST's or GraphQL's, which `x-ratelimit-resource` names). The reset's time
 *  is already past where the reset, plus the margin, has gone by: such an answer names no time
 *  to wait for, and `gitHub`'s ladder takes its own attempts for it rather than ask again once a
 *  second until the deadline. `undefined` where the headers say neither. */
function rateLimitWait(headers: Headers): { at: number; named: string } | undefined {
  const retryAfter = headers.get("retry-after");
  if (retryAfter !== null && /^\d+$/.test(retryAfter)) {
    return {
      at: Date.now() + Math.max(1, Number(retryAfter)) * 1000,
      named: `retry-after: ${retryAfter}`,
    };
  }
  const reset = headers.get("x-ratelimit-reset");
  if (headers.get("x-ratelimit-remaining") === "0" && reset !== null && /^\d+$/.test(reset)) {
    const resource = headers.get("x-ratelimit-resource");
    return {
      at: (Number(reset) + resetMarginSeconds) * 1000,
      named:
        `x-ratelimit-reset: ${reset} (x-ratelimit-remaining: 0` +
        `${resource === null ? "" : `, x-ratelimit-resource: ${resource}`}), plus ` +
        `${resetMarginSeconds} s`,
    };
  }
  return undefined;
}

/** The time a 403's or a 429's `body` says GitHub may be asked again, and what names it
 *  (`named`): `secondaryLimitSeconds` from now where the body names a secondary limit
 *  (`secondaryLimitPattern`), and `undefined` where it does not. gh-read.sh's gh_read reads the
 *  same rule: a secondary limit whose answer names no `retry-after` waits 60 s. A caller reads it
 *  only where `rateLimitWait` finds nothing in the headers, so a `retry-after` comes first. */
function secondaryLimitWait(body: string): { at: number; named: string } | undefined {
  if (!secondaryLimitPattern.test(body)) return undefined;
  return {
    at: Date.now() + secondaryLimitSeconds * 1000,
    named:
      "secondary rate limit, which its body names with no retry-after, and GitHub's " +
      `documented minimum wait of ${secondaryLimitSeconds} s for one`,
  };
}

/** One attempt's outcome: GitHub's answer, or what refused it - `what` as the line before a wait
 *  names it, `wait` where the refusal is GitHub's rate limit and says when to ask again, and
 *  `final` where it is one that a second asking is refused the same way. A `wait` already past
 *  outweighs `final`: the rate limit refused the call, and the ladder's attempts answer it. */
type Attempt =
  | { readonly body: unknown; readonly next: string | undefined }
  | {
      readonly refusal: string;
      readonly what?: string;
      readonly wait?: { readonly at: number; readonly named: string };
      readonly final?: boolean;
    };

/** The GitHub API at `api`, read with `token`. A call is three attempts at most, `retrySeconds`
 *  and then twice that apart, each bounded at 30 s. The default of `retrySeconds`, 10, is the
 *  ladder's own: 10 s and then 20 s, the ladder `.github/scripts/gh-read.sh` gives the scripts
 *  beside it, whose GH_RETRY_SECONDS sets what `retrySeconds` sets here, and which the tests set
 *  lower. Every wait, the ladder's and a rate limit's, is a call of `sleep`, `Bun.sleep` unless a
 *  test records the waits in its place. GitHub refuses a call now and then (a 502, a dropped
 *  connection), and one refusal must decide nothing. The ladder holds except where GitHub's
 *  answer says more:
 *
 *  - A 403 or 429 with `retry-after`, or with `x-ratelimit-remaining: 0` and `x-ratelimit-reset`,
 *    is GitHub's rate limit, secondary or primary, REST's or GraphQL's; so is GraphQL's primary
 *    limit, which answers 200 with a `RATE_LIMITED` error and those two headers; and so is a 403
 *    or 429 with neither whose body names a secondary rate limit, which GitHub sends now and then
 *    and which names the time `secondaryLimitSeconds`, 60 s, from now, as gh-read.sh reads it.
 *    The call waits for the time GitHub names, read off the headers (`rateLimitWait`) and then a
 *    403's or a 429's body (`secondaryLimitWait`), printing one line through `log` before it, and
 *    asks again; that wait takes none of the three attempts. `deadline` bounds each wait by the
 *    rule gh-read.sh's gh_wait_reset holds the scripts' waits to: a wait is made only when the
 *    time GitHub names falls at least `waitMarginSeconds` before the deadline, and otherwise the
 *    call throws, naming the refusal, that time and the limit. Its default, no deadline, waits
 *    for nothing.
 *  - A primary limit whose reset, plus `resetMarginSeconds`, has already passed names no time to
 *    wait for. The call prints one line through `log` that says so and takes the ladder's three
 *    attempts, `retrySeconds` and then twice that apart: a wait of a second for it would ask
 *    again once a second until the deadline. Such a 403 or `RATE_LIMITED` error does not throw
 *    at once.
 *  - A 403 that carries neither header and names no secondary limit, such as a permission
 *    refusal, and a `RATE_LIMITED` error that names no time, throw at once: asking again is
 *    refused the same way.
 *
 *  A call refused three times otherwise throws, naming the call.
 *
 *  Where it still differs from gh-read.sh's gh_wait_reset and gh_read: it waits
 *  `resetMarginSeconds`, 3 s, past a primary limit's reset, where gh-read.sh waits 1 s; it reads
 *  that reset off the refusal's own headers, where gh-read.sh reads GET /rate_limit; a reset
 *  that has already passed takes the ladder's three attempts here, where gh-read.sh asks once
 *  more at once; it waits for a secondary limit's time itself, where gh-read.sh waits a second
 *  past it, its clock being whole seconds; a call refused again after a wait may wait again
 *  here, where gh-read.sh fails it by name; and with no deadline a rate limit throws at once
 *  here, where gh_read still takes its ladder's two later attempts. */
export function gitHub(
  api: string,
  token: string,
  {
    deadline = { none: "no time limit was given to wait within" },
    retrySeconds = 10,
    log = (line: string) => console.error(line),
    sleep = Bun.sleep,
  }: {
    deadline?: Deadline;
    retrySeconds?: number;
    log?: (line: string) => void;
    sleep?: (ms: number) => Promise<unknown>;
  } = {}
): GitHub {
  let calls = 0;
  let graphqlCalls = 0;
  let graphqlCost = 0;
  let graphqlRemaining: number | null = null;
  let remaining: string | null = null;
  async function attempt(url: string, post: unknown): Promise<Attempt> {
    let response: Response;
    let body: unknown;
    try {
      response = await fetch(url, {
        body: post === undefined ? undefined : JSON.stringify(post),
        method: post === undefined ? "GET" : "POST",
        headers: {
          accept: "application/vnd.github+json",
          authorization: `Bearer ${token}`,
          "x-github-api-version": "2022-11-28",
        },
        signal: AbortSignal.timeout(30_000),
      });
      if (response.headers.get("x-ratelimit-resource") !== "graphql") {
        remaining = response.headers.get("x-ratelimit-remaining") ?? remaining;
      }
      if (response.status === 404 || response.status === 422) {
        return { body: undefined, next: undefined };
      }
      if (!response.ok) {
        const text = await response.text();
        const refusal = `HTTP ${response.status}: ${text.slice(0, 200)}`;
        if (response.status !== 403 && response.status !== 429) return { refusal };
        return {
          final: response.status === 403,
          refusal,
          wait: rateLimitWait(response.headers) ?? secondaryLimitWait(text),
          what: `HTTP ${response.status}`,
        };
      }
      body = await response.json();
    } catch (error) {
      return { refusal: error instanceof Error ? error.message : String(error) };
    }
    if (post !== undefined) {
      const { data, errors } = (body ?? {}) as {
        data?: { rateLimit?: { cost?: unknown; remaining?: unknown } } | null;
        errors?: unknown;
      };
      const limited = Array.isArray(errors)
        ? (errors as { type?: unknown; message?: unknown }[]).find(
            (error) => error?.type === "RATE_LIMITED"
          )
        : undefined;
      if (limited !== undefined) {
        return {
          final: true,
          refusal: `HTTP 200 with GraphQL's RATE_LIMITED error: ${String(limited.message).slice(0, 200)}`,
          wait: rateLimitWait(response.headers),
          what: "GraphQL's RATE_LIMITED error",
        };
      }
      if (typeof data?.rateLimit?.cost === "number") graphqlCost += data.rateLimit.cost;
      if (typeof data?.rateLimit?.remaining === "number") {
        graphqlRemaining = data.rateLimit.remaining;
      }
    }
    const next = /<([^>]+)>;\s*rel="next"/.exec(response.headers.get("link") ?? "")?.[1];
    return { body, next };
  }
  async function page(
    url: string,
    post?: unknown
  ): Promise<{ body: unknown; next: string | undefined }> {
    let refusals = 0;
    for (;;) {
      if (post === undefined) calls += 1;
      else graphqlCalls += 1;
      const answer = await attempt(url, post);
      if (!("refusal" in answer)) return answer;
      if (answer.wait !== undefined && answer.wait.at > Date.now()) {
        const until = new Date(answer.wait.at).toISOString();
        const named =
          `GitHub refused ${url} with ${answer.refusal}, and the time its rate limit names, ` +
          `${until} by its ${answer.wait.named}`;
        if ("none" in deadline) {
          throw new Error(`${named}, was not waited for: ${deadline.none}. ${failedRead}`);
        }
        const limit = `this ${deadline.of}'s limit at ${new Date(deadline.at).toISOString()}`;
        if (answer.wait.at + waitMarginSeconds * 1000 > deadline.at) {
          throw new Error(
            `${named}, falls past ${limit} less the ${waitMarginSeconds} s the call after the ` +
              `wait needs, so it was not waited for. ${failedRead}`
          );
        }
        log(
          `quarantine-check: GitHub refused ${url} with ${answer.what}, so it waits until ` +
            `${until}, by its ${answer.wait.named}, within ${limit}, and asks again.`
        );
        await sleep(Math.max(0, answer.wait.at - Date.now()));
        continue;
      }
      if (answer.wait !== undefined) {
        log(
          `quarantine-check: GitHub refused ${url} with ${answer.what}, and the time its rate ` +
            `limit names, ${new Date(answer.wait.at).toISOString()} by its ` +
            `${answer.wait.named}, has already passed, so it names no time to wait for, and ` +
            `the call takes the ladder's own three attempts, ${retrySeconds} s and then ` +
            `${2 * retrySeconds} s apart.`
        );
      } else if (answer.final) {
        throw new Error(
          `GitHub refused ${url} with ${answer.refusal}, which names no time it may be asked ` +
            "again (no retry-after, x-ratelimit-remaining not 0, and no secondary rate limit in " +
            `its body), so it was not asked again. ${failedRead}`
        );
      }
      refusals += 1;
      if (refusals === 3) {
        throw new Error(
          `GitHub refused ${url} three times, the last with ${answer.refusal}. ${failedRead}`
        );
      }
      await sleep(refusals * retrySeconds * 1000);
    }
  }
  async function everyPage<Item>(
    path: string,
    key: string
  ): Promise<{ items: Item[]; total: number }> {
    const items: Item[] = [];
    let total = 0;
    let url: string | undefined = `${api}${path}`;
    while (url !== undefined) {
      const { body, next }: { body: unknown; next: string | undefined } = await page(url);
      if (body === undefined) throw new Error(`GitHub has no ${url}`);
      const object = body as Record<string, unknown>;
      total = typeof object.total_count === "number" ? object.total_count : total;
      items.push(...((Array.isArray(body) ? body : object[key]) as Item[]));
      url = next;
    }
    return { items, total };
  }
  return {
    everyPage,
    graphql: async (query, variables) => (await page(`${api}/graphql`, { query, variables })).body,
    one: async (path) => (await page(`${api}${path}`)).body,
    spent: () => ({ calls, graphqlCalls, graphqlCost, graphqlRemaining, remaining }),
  };
}

interface ListedRun {
  readonly check_suite_node_id: string;
  readonly created_at: string;
  readonly event: string;
  readonly head_sha: string;
  readonly html_url: string;
  readonly id: number;
}

/** Each of `items` through `work`, `width` at a time, in order. GitHub asks a client not to
 *  make its calls all at once (its secondary rate limits), and four is a few. */
async function inTurn<Item, Result>(
  items: readonly Item[],
  width: number,
  work: (item: Item) => Promise<Result>
): Promise<Result[]> {
  const results: Result[] = new Array(items.length);
  let next = 0;
  await Promise.all(
    Array.from({ length: width }, async () => {
      while (next < items.length) {
        const index = next++;
        results[index] = await work(items[index]);
      }
    })
  );
  return results;
}

/** How many check suites one GraphQL call reads, `nodes(ids:)`'s own limit; how many check runs
 *  it reads of each suite; and how many notices of each check run. A suite holding more check
 *  runs, or a check run more notices, is read on, a page a call, until GitHub says there is no
 *  next page. */
export const suitesPerCall = 100;
export const checkRunsPerPage = 10;
export const annotationsPerPage = 50;

/** One page of a GraphQL connection. */
interface GraphPage<Node> {
  readonly pageInfo: { readonly hasNextPage: boolean; readonly endCursor: string | null };
  readonly nodes: readonly Node[];
}

/** One notice of a check run, as GraphQL's `CheckAnnotation` gives it. */
interface Note {
  readonly title: string | null;
  readonly message: string;
}

/** A check run and a page of its notices. */
interface CheckRunPage {
  readonly id: string;
  readonly annotations: GraphPage<Note>;
}

/** A check suite and a page of its merge job's check runs. */
interface SuitePage {
  readonly id: string;
  readonly checkRuns: GraphPage<CheckRunPage>;
}

function isPageOf<Node>(
  value: unknown,
  isNode: (node: unknown) => node is Node
): value is GraphPage<Node> {
  const page = value as {
    nodes?: unknown;
    pageInfo?: { hasNextPage?: unknown; endCursor?: unknown };
  };
  return (
    typeof value === "object" &&
    value !== null &&
    Array.isArray(page.nodes) &&
    page.nodes.every(isNode) &&
    typeof page.pageInfo?.hasNextPage === "boolean" &&
    (page.pageInfo.endCursor === null || typeof page.pageInfo.endCursor === "string")
  );
}

function isNote(node: unknown): node is Note {
  const note = node as { title?: unknown; message?: unknown } | null;
  return (
    typeof note?.message === "string" && (note.title === null || typeof note.title === "string")
  );
}

function isCheckRunPage(node: unknown): node is CheckRunPage {
  const run = node as { id?: unknown; annotations?: unknown } | null;
  return typeof run?.id === "string" && isPageOf(run.annotations, isNote);
}

function isSuitePage(node: unknown): node is SuitePage {
  const suite = node as { id?: unknown; checkRuns?: unknown } | null;
  return typeof suite?.id === "string" && isPageOf(suite.checkRuns, isCheckRunPage);
}

const rateLimitField = "rateLimit { cost remaining resetAt }";
const notesFields = "pageInfo { hasNextPage endCursor } nodes { title message }";
const checkRunsFields =
  "pageInfo { hasNextPage endCursor } nodes { id " +
  `annotations(first: ${annotationsPerPage}) { ${notesFields} } }`;
const checkRunsFilter = "filterBy: { checkName: $checkName, checkType: ALL }";
const suitesQuery =
  `query($ids: [ID!]!, $checkName: String!) { ${rateLimitField} nodes(ids: $ids) { ` +
  `... on CheckSuite { id checkRuns(first: ${checkRunsPerPage}, ${checkRunsFilter}) { ` +
  `${checkRunsFields} } } } }`;
const moreCheckRunsQuery =
  `query($id: ID!, $checkName: String!, $after: String!) { ${rateLimitField} node(id: $id) { ` +
  `... on CheckSuite { id checkRuns(first: ${checkRunsPerPage}, after: $after, ` +
  `${checkRunsFilter}) { ${checkRunsFields} } } } }`;
const moreNotesQuery =
  `query($id: ID!, $after: String!) { ${rateLimitField} node(id: $id) { ... on CheckRun { id ` +
  `annotations(first: ${annotationsPerPage}, after: $after) { ${notesFields} } } } }`;

/** The `data` GitHub's GraphQL endpoint answers `query` with, or a throw naming `what` where it
 *  answers errors or no data at all: a read that answered part of what was asked is not read
 *  as though it answered the whole. */
async function graphqlData(
  github: Pick<GitHub, "graphql">,
  query: string,
  variables: Record<string, unknown>,
  what: string
): Promise<{ node?: unknown; nodes?: unknown }> {
  const answer = (await github.graphql(query, variables)) as
    | { data?: { node?: unknown; nodes?: unknown } | null; errors?: unknown }
    | undefined;
  if (answer?.data == null || answer.errors !== undefined) {
    throw new Error(
      `GitHub's GraphQL answered no ${what} (${JSON.stringify(answer?.errors ?? answer ?? null).slice(0, 200)}). ` +
        failedRead
    );
  }
  return answer.data;
}

/** The notices of each check run named `checkName` in each suite of `ids` (at most
 *  `suitesPerCall`), by suite, each check run's in GitHub's order and the check runs in theirs:
 *  one GraphQL call for them all, and one call more for each further page of a suite's check runs
 *  or of a check run's notices. A suite GitHub does not answer, or answers in a shape this does
 *  not know, throws, naming it. */
async function readSuites(
  github: Pick<GitHub, "graphql">,
  ids: readonly string[],
  checkName: string
): Promise<Map<string, Note[][]>> {
  const shape = (what: string) =>
    new Error(
      `GitHub's GraphQL answered ${what} in a shape this reader does not know. ${failedRead}`
    );
  const { nodes } = await graphqlData(
    github,
    suitesQuery,
    { checkName, ids },
    `check runs of ${ids.length} check suites`
  );
  if (!Array.isArray(nodes) || nodes.length !== ids.length) {
    throw shape(`the check runs of ${ids.length} check suites`);
  }
  const suites = new Map<string, Note[][]>();
  for (const [index, node] of nodes.entries()) {
    const id = ids[index];
    if (node === null) throw new Error(`GitHub has no check suite ${id}. ${failedRead}`);
    if (!isSuitePage(node) || node.id !== id) throw shape(`check suite ${id}`);
    const checkRuns = [...node.checkRuns.nodes];
    let { pageInfo } = node.checkRuns;
    while (pageInfo.hasNextPage) {
      const more = await graphqlData(
        github,
        moreCheckRunsQuery,
        { after: pageInfo.endCursor, checkName, id },
        `check runs of check suite ${id}`
      );
      if (!isSuitePage(more.node)) throw shape(`the check runs of check suite ${id}`);
      checkRuns.push(...more.node.checkRuns.nodes);
      pageInfo = more.node.checkRuns.pageInfo;
    }
    const notes: Note[][] = [];
    for (const checkRun of checkRuns) {
      const runNotes = [...checkRun.annotations.nodes];
      let notesInfo = checkRun.annotations.pageInfo;
      while (notesInfo.hasNextPage) {
        const more = await graphqlData(
          github,
          moreNotesQuery,
          { after: notesInfo.endCursor, id: checkRun.id },
          `notices of check run ${checkRun.id}`
        );
        if (!isCheckRunPage(more.node)) throw shape(`the notices of check run ${checkRun.id}`);
        runNotes.push(...more.node.annotations.nodes);
        notesInfo = more.node.annotations.pageInfo;
      }
      notes.push(runNotes);
    }
    suites.set(id, notes);
  }
  return suites;
}

/** The readings of every run of `countedRuns` created in the `windowDays` days before `now`
 *  that has completed. A filtered run listing answers at most 1,000 runs (GitHub's REST
 *  reference, "List workflow runs for a workflow"), and ci.yaml makes more merge-group runs than
 *  that in 14 days, so the window is read a day at a time, and a day that holds more is refused
 *  rather than read short. The days of both workflows are listed `inTurn`, and each workflow's
 *  runs are then taken a day at a time, oldest first, which is the order its readings keep.
 *
 *  Each run's merge jobs are read from its check suite through GraphQL, `suitesPerCall` suites a
 *  call (`readSuites`), each call under one name of the merge job's check run, the name its runs
 *  carry (`mergeJobOf`): on 2026-11-22 at 03:15Z, from this issue's pod, this read 1,195 runs in
 *  34 REST calls and 13 GraphQL calls costing 132 points. */
export async function readHistory(
  github: GitHub,
  repository: string,
  now: Date
): Promise<RunReading[]> {
  const slices = countedRuns.flatMap((counted) =>
    Array.from({ length: windowDays }, (_, index) => ({ counted, day: windowDays - index }))
  );
  const listed = await inTurn(slices, 4, async ({ counted, day }) => {
    const from = new Date(now.getTime() - day * dayMs).toISOString();
    const to = new Date(now.getTime() - (day - 1) * dayMs).toISOString();
    const { items, total } = await github.everyPage<ListedRun>(
      `/repos/${repository}/actions/workflows/${counted.workflow}/runs?event=${counted.event}` +
        `&status=completed&created=${from}..${to}&per_page=100`,
      "workflow_runs"
    );
    if (total > 1000) {
      throw new Error(
        `${counted.workflow} made ${total} ${counted.event} runs between ${from} and ${to}, ` +
          "and GitHub lists 1,000 of a filtered listing at most; read a shorter slice"
      );
    }
    return items;
  });
  const readings: RunReading[] = [];
  for (const counted of countedRuns) {
    const runs = new Map<number, ListedRun>();
    slices.forEach((slice, index) => {
      if (slice.counted !== counted) return;
      for (const run of listed[index]) runs.set(run.id, run);
    });
    const ordered = [...runs.values()];
    const named = new Map<string, ListedRun[]>();
    for (const run of ordered) {
      const name = mergeJobOf(counted, run.created_at);
      const group = named.get(name);
      if (group === undefined) named.set(name, [run]);
      else group.push(run);
    }
    const batches = [...named].flatMap(([name, group]) =>
      Array.from({ length: Math.ceil(group.length / suitesPerCall) }, (_, index) => ({
        group: group.slice(index * suitesPerCall, (index + 1) * suitesPerCall),
        name,
      }))
    );
    const suites = new Map(
      (
        await inTurn(batches, 4, ({ group, name }) =>
          readSuites(
            github,
            group.map((run) => run.check_suite_node_id),
            name
          )
        )
      ).flatMap((read) => [...read])
    );
    for (const run of ordered) {
      const flakyMessages: string[] = [];
      let unread = 0;
      // A check run with no notices adds nothing: no row, and a flake rate of 0.
      for (const notes of suites.get(run.check_suite_node_id) ?? []) {
        const rows = notes
          .filter((note) => note.title === flakyRowTitle)
          .map((note) => note.message);
        const rate = notes.find((note) => note.title === flakeRateTitle);
        const flaky = Number(/^(\d+) flaky rows of /.exec(rate?.message ?? "")?.[1] ?? rows.length);
        flakyMessages.push(...rows);
        unread += Math.max(0, flaky - rows.length);
      }
      readings.push({
        createdAt: run.created_at,
        event: run.event,
        flakyRows: flakyMessages,
        headSha: run.head_sha,
        id: run.id,
        unread,
        url: run.html_url,
      });
    }
  }
  return readings;
}

type ListingNode = {
  readonly specs?: readonly {
    readonly file: string;
    readonly line: number;
    readonly title: string;
    readonly tests?: readonly { readonly projectName?: string }[];
  }[];
  readonly suites?: readonly ListingNode[];
  readonly title?: string;
};

/** A listing as this reads it: its files' suites, each walked to its specs. */
type ListingReport = { readonly suites?: readonly ListingNode[] };

/** Every row of `report` by `rowName` with its file and line, every row line of each file, and
 *  every row. */
function rowsWithLines(report: ListingReport): {
  lines: Map<string, { file: string; line: number }>;
  fileRows: Map<string, number[]>;
  rows: QuarantineRow[];
} {
  const lines = new Map<string, { file: string; line: number }>();
  const fileRows = new Map<string, number[]>();
  const rows: QuarantineRow[] = [];
  const walk = (node: ListingNode, path: readonly string[]): void => {
    for (const spec of node.specs ?? []) {
      const specLines = fileRows.get(spec.file) ?? [];
      fileRows.set(spec.file, specLines);
      specLines.push(spec.line);
      for (const test of spec.tests ?? []) {
        const title = [...path, spec.title].join(titleSeparator);
        const row = { file: spec.file, project: test.projectName ?? "", title };
        lines.set(rowName(row), { file: spec.file, line: spec.line });
        rows.push(row);
      }
    }
    for (const child of node.suites ?? []) walk(child, [...path, child.title ?? ""]);
  };
  for (const file of report.suites ?? []) walk(file, []);
  return { fileRows, lines, rows };
}

/** The blame of `path` at `expression`: one GraphQL call, `Commit.blame`, on the ladder of the
 *  other reads. A refused call, or an answer that holds no blame - a 404 or a 422 is no answer at
 *  all - throws, naming the file: the rows of a file whose blame is unread cannot be read as they
 *  stand, and nothing stands in for it. */
export async function blame(
  github: Pick<GitHub, "graphql">,
  repository: string,
  expression: string,
  path: string
): Promise<BlameRange[]> {
  const [owner, name] = repository.split("/");
  const query =
    "query($owner: String!, $name: String!, $expression: String!, $path: String!) { " +
    `${rateLimitField} ` +
    "repository(owner: $owner, name: $name) { object(expression: $expression) { ... on Commit { " +
    "blame(path: $path) { ranges { startingLine endingLine commit { oid committedDate } } } } } } }";
  let answer: unknown;
  try {
    answer = await github.graphql(query, { expression, name, owner, path });
  } catch (error) {
    throw new Error(
      `the blame of ${path} at ${expression} was refused: ${(error as Error).message}`
    );
  }
  // The answer is GitHub's GraphQL JSON; only `ranges` is read of it, and it is checked to be a
  // list before it is used.
  const ranges = (
    answer as { data?: { repository?: { object?: { blame?: { ranges?: unknown } } } } }
  )?.data?.repository?.object?.blame?.ranges;
  if (!Array.isArray(ranges)) {
    throw new Error(
      `GitHub answered no blame of ${path} at ${expression} (${JSON.stringify(answer ?? null).slice(0, 200)}), ` +
        "so the rows of that file cannot be read as they stand: the quarantine was not checked."
    );
  }
  return ranges as BlameRange[];
}

/** What the checkout says of the candidate rows, each a row flaky in `flakyRunsToQuarantine`
 *  runs: what `decide` and `readHolds` read of them (`readings`) and what `report` does
 *  (`unquarantinable`). */
export interface Checkout {
  /** One reading of each candidate, by `rowName`. */
  readonly readings: ReadonlyMap<string, CandidateReading>;
  readonly unquarantinable: UnquarantinableFiles;
}

/** What the checkout at `expression` says of `candidates`: one reading of each. Their rows are
 *  listed as the jobs run them, in four listings at once. The first is of the candidates' files
 *  alone, in one process, every job's way but the two-process job's: a file filter is a pattern
 *  over a spec's path, which may take a file more but never leaves out a row of a file it names,
 *  and `--pass-with-no-tests` keeps a listing of a deleted file from failing. The other three
 *  are `listUnquarantinableFiles`'s: the whole suite under each of `twoProcessRuns`, and the
 *  embedder job's command. They give the files the quarantine cannot take. The two-process
 *  listings also give the two-process job's rows: one process ignores every
 *  `*.multiprocess.e2e.ts`, so a row of those files is in no other listing. A row is read by
 *  its name in the one-process and two-process listings, and its lines by its own file's rows
 *  there. A candidate they do not name is `gone` where the checkout does not hold its file,
 *  `unlisted` where they hold no row of its file, and `ownTitle` or else `unmatched` where they
 *  hold other rows of it. With no candidate it lists nothing, and there is no reading and no
 *  file the quarantine cannot take: no row then reaches what `decide` and `report` read. */
export async function readCheckout(
  github: Pick<GitHub, "graphql">,
  candidates: readonly QuarantineRow[],
  repository: string,
  expression: string
): Promise<Checkout> {
  const readings = new Map<string, CandidateReading>();
  if (candidates.length === 0) {
    return { readings, unquarantinable: noUnquarantinableFiles };
  }
  const files = [...new Set(candidates.map((row) => row.file))];
  const [oneProcess, unquarantinable] = await Promise.all([
    listing(import.meta.url, "e2e/playwright.config.ts", {}, [...files, "--pass-with-no-tests"]),
    listUnquarantinableFiles(import.meta.url),
  ]);
  // `Listing` (e2e/configurations.ts) declares the fields its own readers take; a spec's `line`
  // is in the same JSON, which Playwright's JSON reporter writes for every spec.
  const reports = [oneProcess, ...unquarantinable.twoProcess] as ListingReport[];
  const { fileRows, lines, rows } = rowsWithLines({
    suites: reports.flatMap((report) => report.suites ?? []),
  });
  // Each file's blame and its line count, read once for every candidate of the file.
  const read = new Map<string, { length: number; ranges: readonly BlameRange[] }>();
  // Every way out of it is a reading, so no candidate is left out of `readings`.
  const readRow = async (row: QuarantineRow): Promise<CandidateReading> => {
    const at = lines.get(rowName(row));
    if (at === undefined) {
      if (!existsSync(fileURLToPath(new URL(`../${row.file}`, import.meta.url)))) {
        return { kind: "gone" };
      }
      if (!fileRows.has(row.file)) return { kind: "unlisted" };
      const meant = ownTitleRows(row, rows);
      return meant.length > 0 ? { kind: "ownTitle", listed: meant } : { kind: "unmatched" };
    }
    let file = read.get(at.file);
    if (file === undefined) {
      const ranges = await blame(
        github,
        repository,
        expression,
        `packages/dispatch/e2e/${at.file}`
      );
      const source = readFileSync(fileURLToPath(new URL(`../${at.file}`, import.meta.url)), "utf8");
      file = { length: source.split("\n").length, ranges };
      read.set(at.file, file);
    }
    const { end, start } = rowLines(at.line, fileRows.get(at.file) ?? [], file.length);
    const commit = lastChange(file.ranges, start, end);
    if (commit === undefined) {
      throw new Error(
        `the blame of ${at.file} at ${expression} holds none of lines ${start}-${end}`
      );
    }
    return { change: { ...commit, lines: `${at.file}:${start}-${end}` }, kind: "listed" };
  };
  for (const row of candidates) readings.set(rowName(row), await readRow(row));
  return { readings, unquarantinable: unquarantinable.files };
}

/** Each fix with its commit's time where `expression` holds it: GitHub's compare of the two
 *  answers the fix as the merge base exactly when the fix is an ancestor of the checkout. */
async function readFixes(
  github: GitHub,
  repository: string,
  expression: string
): Promise<FixReading[]> {
  return Promise.all(
    fixes.map(async (entry): Promise<FixReading> => {
      const compare = (await github.one(
        `/repos/${repository}/compare/${expression}...${entry.fixed}`
      )) as
        | { merge_base_commit?: { sha?: string; commit?: { committer?: { date?: string } } } }
        | undefined;
      const base = compare?.merge_base_commit;
      return {
        committedDate: base?.sha === entry.fixed ? base.commit?.committer?.date : undefined,
        entry,
      };
    })
  );
}

/** Whether the head of each run that named a candidate row - a row flaky in
 *  `flakyRunsToQuarantine` runs whose reading in `readings` is `listed` - holds the row's last
 *  change, and its fix where `fixEntries` names one, by `pairKey`. GitHub's compare of the commit
 *  with the head answers the commit as the merge base exactly when the head holds it, as
 *  `readFixes` reads it. Each pair is read once, a REST call, four at a time (`inTurn`), and none
 *  is read on a day with no candidate. A compare GitHub refuses, does not have (a 404 or a 422:
 *  one of the two is a commit it does not have) or answers with no merge base tells nothing: the
 *  pair holds why, and `decide` counts the run and names it. */
export async function readHolds(
  github: Pick<GitHub, "one">,
  repository: string,
  runs: readonly RunReading[],
  readings: ReadonlyMap<string, CandidateReading>,
  fixEntries: readonly FixEntry[],
  now: Date
): Promise<Holds> {
  const pairs = new Map<string, { commit: string; head: string }>();
  for (const [name, { runs: named }] of flakyRows(runs, now).rows) {
    const reading = readings.get(name);
    if (named.length < flakyRunsToQuarantine || reading?.kind !== "listed") continue;
    const fix = fixEntries.find((entry) => rowName(entry) === name);
    for (const { oid: commit } of rowCommits(reading.change, fix)) {
      for (const run of named) {
        pairs.set(pairKey(commit, run.headSha), { commit, head: run.headSha });
      }
    }
  }
  return new Map(
    await inTurn(
      [...pairs],
      4,
      async ([key, { commit, head }]): Promise<[string, boolean | string]> => {
        let compare: unknown;
        try {
          compare = await github.one(`/repos/${repository}/compare/${commit}...${head}`);
        } catch (error) {
          // `gitHub`'s refusal says the whole reading failed; a refused compare fails only its
          // pair, whose run counts.
          const refusal = (error as Error).message.replace(` ${failedRead}`, "");
          return [key, refusal.replace(/\.$/u, "")];
        }
        if (compare === undefined) {
          return [
            key,
            "GitHub has no compare of the two (a 404 or a 422), so one of them is a commit it " +
              "does not have",
          ];
        }
        const base = (compare as { merge_base_commit?: { sha?: unknown } } | null)
          ?.merge_base_commit?.sha;
        if (typeof base !== "string") {
          return [key, "GitHub's compare of the two names no merge base"];
        }
        return [key, base === commit];
      }
    )
  );
}

if (import.meta.main) {
  const repository = process.env.GITHUB_REPOSITORY ?? "";
  const token = process.env.GH_TOKEN || process.env.GITHUB_TOKEN || "";
  if (process.argv.length > 2 || !/^[^/\s]+\/[^/\s]+$/.test(repository) || token === "") {
    console.error(
      "usage: GITHUB_REPOSITORY=<owner>/<name> GH_TOKEN=<token> [JOB_TIMEOUT_MINUTES=<m>] " +
        "[STEP_TIMEOUT_MINUTES=<m>] bun e2e/tools/quarantine-check.ts\n" +
        "  reads the merge jobs' flaky-row notices of the last 14 days and the quarantine's dates"
    );
    process.exit(2);
  }
  const now = new Date();
  const started = performance.now();
  const expression = process.env.GITHUB_SHA || "main";
  const github = gitHub(process.env.GITHUB_API_URL ?? "https://api.github.com", token, {
    deadline: readDeadline(process.env),
  });
  let decision: Decision;
  let checkout: Checkout;
  try {
    // The fixes depend on neither the history nor the blames, so they are read beside the first.
    const [runs, fixReadings] = await Promise.all([
      readHistory(github, repository, now),
      readFixes(github, repository, expression),
    ]);
    const candidates = [...flakyRows(runs, now).rows.values()]
      .filter((seen) => seen.runs.length >= flakyRunsToQuarantine)
      .map((seen) => seen.row);
    checkout = await readCheckout(github, candidates, repository, expression);
    const holds = await readHolds(github, repository, runs, checkout.readings, fixes, now);
    decision = decide(runs, quarantine, fixReadings, checkout.readings, now, holds);
  } catch (error) {
    console.error(`::error title=dispatch-browser quarantine::${(error as Error).message}`);
    process.exit(1);
  }
  const { annotations, failed, summary } = report(decision, now, checkout.unquarantinable);
  console.log(summary.join("\n"));
  if (process.env.GITHUB_STEP_SUMMARY) {
    appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${summary.join("\n")}\n`);
  }
  for (const annotation of annotations) console.log(annotation);
  const spent = github.spent();
  console.error(
    `read ${decision.counted.length} runs in ${spent.calls} REST calls and ` +
      `${spent.graphqlCalls} GraphQL calls costing ${spent.graphqlCost} points, and ` +
      `${Math.round(performance.now() - started)} ms at ${expression}; the token has ` +
      `${spent.remaining ?? "an unknown number of"} REST calls and ` +
      `${spent.graphqlRemaining ?? "an unknown number of"} GraphQL points left this hour`
  );
  process.exit(failed ? 1 : 0);
}
