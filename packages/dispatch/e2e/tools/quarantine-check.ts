/** `bun e2e/tools/quarantine-check.ts`: the half of the browser suite's quarantine that depends on
 *  the date (R4 of dispatch://INFRA-983), which `.github/workflows/dispatch-browser-nightly.yaml`
 *  runs and no pull request does. `e2e/quarantine.ts` holds the list and says what reads it.
 *
 *  It exits 1 when it names any of these:
 *
 *  - a row flaky in `flakyRunsToQuarantine` or more runs of the last `windowDays` days that the
 *    quarantine does not hold, with the runs and the lines to add to `quarantine`;
 *  - an entry of `quarantine` held longer than `windowDays` days, with its issue and its lines;
 *  - an entry of `fixes` whose commit is not on main.
 *
 *  A fix entry older than `windowDays` days, by its commit's time, is named too, for its owner
 *  to delete, and fails nothing: every flake the window holds is after it.
 *
 *  R4's "flaky twice" counts the row as it stands. A flake counts only from a run created after
 *  the row's own lines last changed on main - the row's lines run from its line in the listing
 *  of the checkout to the line before the next row of its file there, or to the file's end, and
 *  their last change is the newest commit of the file's blame at the checkout that overlaps them
 *  - and after its fix, where `fixes` names one. A merge-group or scheduled run created after a
 *  commit landed holds that commit, so time is the comparison and no history is read. A change
 *  elsewhere in the file moves nothing. The summary names each run it left out, and why.
 *
 *  The runs it reads are the scheduled runs of the nightly and the merge-group runs of ci.yaml,
 *  and no other: a pull request's head can carry its own defect. What it reads of each is the
 *  `dispatch-browser flaky row` notices of its merge job (the "Reading two" step of
 *  .github/workflows/dispatch-browser.yaml's merge job prints one per flaky row), through the
 *  check runs API: the run listing (`actions: read`), the merge job's check runs in the run's
 *  check suite and their annotations (`checks: read`). For a row flaky in two runs it also reads
 *  a listing of its file in this checkout and the file's blame, one GraphQL request per file,
 *  which it sends itself with the job's token, through the client and the retry ladder of its
 *  REST reads; a refused blame fails the check, naming the file. For each fix it reads GitHub's
 *  compare of the checkout with the fix's commit.
 *
 *  Those reads are the adapter's, below `readHistory`, and they decide nothing; `flakyRows`,
 *  `rowLines`, `lastChange`, `decide` and `report` are pure over what was read and the moment
 *  given them, which is how `e2e/__tests__/quarantine-check.test.ts` drives them, and it drives
 *  `blame` against a stand-in for GitHub's answer. Only the command line reads the clock.
 *
 *  Environment: GITHUB_REPOSITORY (`owner/name`), GH_TOKEN or GITHUB_TOKEN, GITHUB_SHA (the
 *  commit checked out; `main` when unset), GITHUB_API_URL (https://api.github.com when unset)
 *  and, where it is set, GITHUB_STEP_SUMMARY, which the summary is appended to as well as
 *  printed. It writes nothing to GitHub. A read GitHub refuses three times exits 1 too, and says
 *  it is a failed read rather than a finding. Usage errors exit 2. From a workstation, with gh
 *  signed in, in a checkout of the commit GITHUB_SHA names:
 *
 *    GITHUB_REPOSITORY=cybermonklabs/legion GH_TOKEN="$(gh auth token)" GITHUB_SHA=<sha> \
 *      bun e2e/tools/quarantine-check.ts
 */
import { appendFileSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { listing } from "../__tests__/playwright-listing";
import {
  entryLines,
  type FixEntry,
  fixes,
  type QuarantineEntry,
  type QuarantineRow,
  quarantine,
  rowName,
  titleSeparator,
} from "../quarantine";

/** How far back a flaky row counts, and how long an entry may be held: R4's 14 days. */
export const windowDays = 14;

/** How many runs a row must be flaky in for the quarantine to owe it an entry: R4's "twice". */
export const flakyRunsToQuarantine = 2;

/** The runs whose flaky rows count, each with the workflow that holds them and the name of its
 *  merge job's check run: a called workflow's job is named `<the caller's job> / <its job>`, and
 *  the nightly calls dispatch-browser.yaml from its `full` job, ci.yaml from `dispatch-browser`. */
export const countedRuns = [
  { event: "schedule", mergeJob: "full / merge", workflow: "dispatch-browser-nightly.yaml" },
  { event: "merge_group", mergeJob: "dispatch-browser / merge", workflow: "ci.yaml" },
] as const;

/** What one run's merge jobs said. */
export interface RunReading {
  readonly id: number;
  /** The run's event: `schedule`, `merge_group`, `pull_request`. */
  readonly event: string;
  /** When the run was created, as GitHub gives it. */
  readonly createdAt: string;
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
  /** Each row flaky in enough runs after its last change that the quarantine does not hold. */
  readonly toQuarantine: readonly {
    readonly row: QuarantineRow;
    readonly runs: readonly RunReading[];
    readonly change: RowChange;
  }[];
  /** Each run a row flaky in enough runs was not counted in, and why. */
  readonly leftOut: readonly {
    readonly row: QuarantineRow;
    readonly run: RunReading;
    readonly why: string;
  }[];
  /** A row flaky in enough runs that the listing of the checkout does not hold. */
  readonly absent: readonly QuarantineRow[];
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

/** What `runs` say of `entries` and `fixReadings` at `now`, with `changes` the last change of
 *  each row flaky in `flakyRunsToQuarantine` runs, by `rowName`; a row it holds no change for is
 *  one the checkout's listing does not hold. Such a row counts a run created after its change
 *  and, where a fix on main names it, after that fix too. A quarantine entry is overdue when more
 *  than `windowDays` calendar days (UTC) lie between the day it was added and the day of `now`. */
export function decide(
  runs: readonly RunReading[],
  entries: readonly QuarantineEntry[],
  fixReadings: readonly FixReading[],
  changes: ReadonlyMap<string, RowChange>,
  now: Date
): Decision {
  const flaky = flakyRows(runs, now);
  const held = new Set(entries.map(rowName));
  const toQuarantine: { row: QuarantineRow; runs: RunReading[]; change: RowChange }[] = [];
  const leftOut: { row: QuarantineRow; run: RunReading; why: string }[] = [];
  const absent: QuarantineRow[] = [];
  for (const [name, { row, runs: named }] of [...flaky.rows].sort(([a], [b]) =>
    a.localeCompare(b)
  )) {
    if (named.length < flakyRunsToQuarantine) continue;
    const change = changes.get(name);
    if (change === undefined) {
      absent.push(row);
      continue;
    }
    const fix = fixReadings.find(
      (reading) => rowName(reading.entry) === name && reading.committedDate !== undefined
    );
    const changed = Date.parse(change.committedDate);
    const fixedAt =
      fix?.committedDate === undefined ? Number.NEGATIVE_INFINITY : Date.parse(fix.committedDate);
    const counting: RunReading[] = [];
    for (const run of named) {
      const created = Date.parse(run.createdAt);
      if (created <= changed) {
        leftOut.push({
          row,
          run,
          why: `created before its own lines last changed, in ${change.oid} at ${change.committedDate}`,
        });
      } else if (created <= fixedAt) {
        leftOut.push({
          row,
          run,
          why: `created before its fix, ${fix?.entry.fixed} at ${fix?.committedDate}`,
        });
      } else {
        counting.push(run);
      }
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
    spent,
    toQuarantine,
  };
}

/** `entry`'s lines as `entryLines` prints them, fenced as TypeScript under a list item of the
 *  summary, with a blank line before and after. */
function fencedEntry(entry: QuarantineEntry | FixEntry): string[] {
  return ["", "  ```ts", ...entryLines(entry).map((line) => `  ${line}`), "  ```", ""];
}

/** The decision for a person and for the log: `summary` is Markdown, the step summary's;
 *  `annotations` are workflow commands, one per finding; `failed` is whether the run must fail. */
export function report(
  decision: Decision,
  now: Date
): { annotations: string[]; failed: boolean; summary: string[] } {
  const today = now.toISOString().slice(0, 10);
  const runLink = (run: RunReading) =>
    `[run ${run.id}](${run.url}) of ${run.createdAt.slice(0, 10)} (${run.event})`;
  const flaky = decision.counted.filter((run) => run.flakyRows.length > 0);
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
      `. ${flaky.length} of them named a flaky row. A pull request's runs are not read: its ` +
      "head can carry its own defect.",
    "",
    `#### Flaky in ${flakyRunsToQuarantine} or more runs, and not quarantined`,
    "",
  ];
  const annotations: string[] = [];
  if (decision.toQuarantine.length === 0) {
    summary.push(
      `No row was flaky in ${flakyRunsToQuarantine} runs after its last change that the ` +
        "quarantine does not hold."
    );
  }
  for (const { change, row, runs } of decision.toQuarantine) {
    summary.push(
      `- \`${rowName(row)}\`, flaky in ${runs.length} runs after its own lines ` +
        `(${change.lines}) last changed, in ${change.oid} at ${change.committedDate}: ` +
        `${runs.map(runLink).join(", ")}.`,
      "",
      `  The controller files the issue that owns it. Add these lines to \`quarantine\` in ` +
        `\`${quarantinePath}\`, with that issue's key for \`<KEY>\`:`,
      ...fencedEntry({ ...row, added: today, issue: "dispatch://<KEY>" })
    );
    annotations.push(
      `::error title=dispatch-browser quarantine::${rowName(row)} was flaky in ${runs.length} ` +
        `runs of ${windowDays} days and is not quarantined: ${runs.map((run) => run.url).join(" ")}`
    );
  }
  if (decision.leftOut.length > 0 || decision.absent.length > 0) {
    summary.push("", "#### Flakes left out", "");
  }
  for (const { row, run, why } of decision.leftOut) {
    summary.push(`- \`${rowName(row)}\` in ${runLink(run)}: ${why}.`);
  }
  for (const row of decision.absent) {
    summary.push(`- \`${rowName(row)}\`: no row of the checkout's listing, so it does not stand.`);
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
      decision.overdue.length > 0 ||
      decision.offMain.length > 0,
    summary,
  };
}

/** The reads `readHistory` and the rest make, and what they cost. */
interface GitHub {
  /** Every page of `path`, the list of each page taken from `key` of its body (or the body itself
   *  where it is a list), with the `total_count` the last page gave. */
  everyPage<Item>(path: string, key: string): Promise<{ items: Item[]; total: number }>;
  /** The body of `path`, or `undefined` where GitHub has no such thing (404 or 422). */
  one(path: string): Promise<unknown>;
  /** The body GitHub's GraphQL endpoint answers `query` with, on the same ladder. */
  graphql(query: string, variables: Record<string, string>): Promise<unknown>;
  /** The calls made so far, retries included, and the rate limit's remaining calls last read. */
  spent(): { calls: number; remaining: string | null };
}

/** The GitHub REST API at `api`, read with `token`: one call is three attempts at most, 10 s and
 *  then 20 s apart, each bounded at 30 s, the ladder `.github/scripts/gh-read.sh` gives the
 *  scripts beside it, since GitHub refuses a call now and then (a 502, a secondary rate limit)
 *  and one refusal must decide nothing. A call refused three times throws, naming the call. */
function gitHub(api: string, token: string): GitHub {
  let calls = 0;
  let remaining: string | null = null;
  async function page(
    url: string,
    post?: unknown
  ): Promise<{ body: unknown; next: string | undefined }> {
    let last = "";
    for (const attempt of [1, 2, 3]) {
      calls += 1;
      try {
        const response = await fetch(url, {
          body: post === undefined ? undefined : JSON.stringify(post),
          method: post === undefined ? "GET" : "POST",
          headers: {
            accept: "application/vnd.github+json",
            authorization: `Bearer ${token}`,
            "x-github-api-version": "2022-11-28",
          },
          signal: AbortSignal.timeout(30_000),
        });
        remaining = response.headers.get("x-ratelimit-remaining") ?? remaining;
        if (response.ok) {
          const next = /<([^>]+)>;\s*rel="next"/.exec(response.headers.get("link") ?? "")?.[1];
          return { body: await response.json(), next };
        }
        if (response.status === 404 || response.status === 422) {
          return { body: undefined, next: undefined };
        }
        last = `HTTP ${response.status}: ${(await response.text()).slice(0, 200)}`;
      } catch (error) {
        last = error instanceof Error ? error.message : String(error);
      }
      if (attempt < 3) await Bun.sleep(attempt * 10_000);
    }
    throw new Error(
      `GitHub refused ${url} three times, the last with ${last}. This is a failed read of the ` +
        "runs and not a finding: the quarantine was not checked."
    );
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
    spent: () => ({ calls, remaining }),
  };
}

interface ListedRun {
  readonly check_suite_id: number;
  readonly created_at: string;
  readonly event: string;
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

/** The readings of every run of `countedRuns` created in the `windowDays` days before `now`
 *  that has completed. A filtered run listing answers at most 1,000 runs (GitHub's REST
 *  reference, "List workflow runs for a workflow"), and ci.yaml makes more merge-group runs than
 *  that in 14 days, so the window is read a day at a time, and a day that holds more is refused
 *  rather than read short. The days of both workflows are listed `inTurn`, and each workflow's
 *  runs are then taken a day at a time, oldest first, which is the order its readings keep. */
async function readHistory(github: GitHub, repository: string, now: Date): Promise<RunReading[]> {
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
    const read = await inTurn([...runs.values()], 4, async (run): Promise<RunReading> => {
      const { items: checkRuns } = await github.everyPage<{
        id: number;
        output: { annotations_count: number };
      }>(
        `/repos/${repository}/check-suites/${run.check_suite_id}/check-runs` +
          `?check_name=${encodeURIComponent(counted.mergeJob)}&filter=all&per_page=100`,
        "check_runs"
      );
      const flakyMessages: string[] = [];
      let unread = 0;
      for (const checkRun of checkRuns.filter((check) => check.output.annotations_count > 0)) {
        const { items: notes } = await github.everyPage<{ message: string; title: string | null }>(
          `/repos/${repository}/check-runs/${checkRun.id}/annotations?per_page=100`,
          "annotations"
        );
        const rows = notes
          .filter((note) => note.title === flakyRowTitle)
          .map((note) => note.message);
        const rate = notes.find((note) => note.title === flakeRateTitle);
        const flaky = Number(/^(\d+) flaky rows of /.exec(rate?.message ?? "")?.[1] ?? rows.length);
        flakyMessages.push(...rows);
        unread += Math.max(0, flaky - rows.length);
      }
      return {
        createdAt: run.created_at,
        event: run.event,
        flakyRows: flakyMessages,
        id: run.id,
        unread,
        url: run.html_url,
      };
    });
    readings.push(...read);
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

/** Every row of `report` by `rowName` with its file and line, and every row line of each file. */
function rowsWithLines(report: { readonly suites?: readonly ListingNode[] }): {
  lines: Map<string, { file: string; line: number }>;
  fileRows: Map<string, number[]>;
} {
  const lines = new Map<string, { file: string; line: number }>();
  const fileRows = new Map<string, number[]>();
  const walk = (node: ListingNode, path: readonly string[]): void => {
    for (const spec of node.specs ?? []) {
      const specLines = fileRows.get(spec.file) ?? [];
      fileRows.set(spec.file, specLines);
      specLines.push(spec.line);
      for (const test of spec.tests ?? []) {
        const title = [...path, spec.title].join(titleSeparator);
        const row = { file: spec.file, project: test.projectName ?? "", title };
        lines.set(rowName(row), { file: spec.file, line: spec.line });
      }
    }
    for (const child of node.suites ?? []) walk(child, [...path, child.title ?? ""]);
  };
  for (const file of report.suites ?? []) walk(file, []);
  return { fileRows, lines };
}

/** The blame of `path` at `expression`: one GraphQL call, `Commit.blame`, on the REST reads'
 *  ladder. A refused call, or an answer that holds no blame - a 404 or a 422 is no answer at
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

/** The last change of each row of `candidates` that the checkout's listing holds: its lines
 *  from that listing, and their newest commit from its file's blame at `expression`. */
async function readChanges(
  github: GitHub,
  candidates: readonly QuarantineRow[],
  repository: string,
  expression: string
): Promise<Map<string, RowChange>> {
  const changes = new Map<string, RowChange>();
  if (candidates.length === 0) return changes;
  // `Listing` (e2e/configurations.ts) declares the fields its own readers take; a spec's `line`
  // is in the same JSON, which Playwright's JSON reporter writes for every spec. It lists the
  // candidates' files alone: a file filter is a pattern over a spec's path, which may take a file
  // more but never leaves out a row of a file it names, and a row is read here by its name and
  // its lines by its own file's rows. `--pass-with-no-tests` keeps a listing that a deleted file
  // leaves empty from failing, so the deleted file's rows land in `absent`.
  const files = [...new Set(candidates.map((row) => row.file))];
  const report = (await listing(import.meta.url, "e2e/playwright.config.ts", {}, [
    ...files,
    "--pass-with-no-tests",
  ])) as { readonly suites?: readonly ListingNode[] };
  const { fileRows, lines } = rowsWithLines(report);
  // Each file's blame and its line count, read once for every candidate of the file.
  const read = new Map<string, { length: number; ranges: readonly BlameRange[] }>();
  for (const row of candidates) {
    const name = rowName(row);
    const at = lines.get(name);
    if (at === undefined) continue;
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
    changes.set(name, { ...commit, lines: `${at.file}:${start}-${end}` });
  }
  return changes;
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

if (import.meta.main) {
  const repository = process.env.GITHUB_REPOSITORY ?? "";
  const token = process.env.GH_TOKEN || process.env.GITHUB_TOKEN || "";
  if (process.argv.length > 2 || !/^[^/\s]+\/[^/\s]+$/.test(repository) || token === "") {
    console.error(
      "usage: GITHUB_REPOSITORY=<owner>/<name> GH_TOKEN=<token> bun e2e/tools/quarantine-check.ts\n" +
        "  reads the merge jobs' flaky-row notices of the last 14 days and the quarantine's dates"
    );
    process.exit(2);
  }
  const now = new Date();
  const started = performance.now();
  const expression = process.env.GITHUB_SHA || "main";
  const github = gitHub(process.env.GITHUB_API_URL ?? "https://api.github.com", token);
  let decision: Decision;
  try {
    // The fixes depend on neither the history nor the blames, so they are read beside the first.
    const [runs, fixReadings] = await Promise.all([
      readHistory(github, repository, now),
      readFixes(github, repository, expression),
    ]);
    const candidates = [...flakyRows(runs, now).rows.values()]
      .filter((seen) => seen.runs.length >= flakyRunsToQuarantine)
      .map((seen) => seen.row);
    const changes = await readChanges(github, candidates, repository, expression);
    decision = decide(runs, quarantine, fixReadings, changes, now);
  } catch (error) {
    console.error(`::error title=dispatch-browser quarantine::${(error as Error).message}`);
    process.exit(1);
  }
  const { annotations, failed, summary } = report(decision, now);
  console.log(summary.join("\n"));
  if (process.env.GITHUB_STEP_SUMMARY) {
    appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${summary.join("\n")}\n`);
  }
  for (const annotation of annotations) console.log(annotation);
  const { calls, remaining } = github.spent();
  console.error(
    `read ${decision.counted.length} runs in ${calls} REST calls and ` +
      `${Math.round(performance.now() - started)} ms at ${expression}; ` +
      `the token has ${remaining ?? "an unknown number of"} REST calls left this hour`
  );
  process.exit(failed ? 1 : 0);
}
