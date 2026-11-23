/**
 * The census the three readings of this work's checks run: `texts`, which reads the documents and
 * plans that still hold the old rule on who runs a check; `rounds`, which reads each testing round
 * since a merge and says which of the testing step's checks it has no run of; and `main-read`,
 * which reads main after a merge, runs the greps and the pin tests of the readings, and writes the
 * files their readers read.
 *
 * It is written in the form of `ask-census.ts`: Dispatch resolved with `sessionDispatchSettings`,
 * eight issues read at a time, and `--json` printing one object and nothing else.
 *
 * Exit 0: the run is complete and nothing is listed. Exit 1: the run is complete and holds rows
 * for a reader. Exit 3: UNKNOWN, with the reason — a list below its total, a text or handoff that
 * did not load, an issue Dispatch does not know, or no issue at all. A run that enumerated nothing
 * says so rather than reading clean.
 */

import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { parseArgs } from "node:util";
import type {
  Artifact,
  ArtifactText,
  CheckBlock,
  CheckResult,
  GraphEdge,
  GraphReferences,
  Handoff,
  HandoffSummary,
  IssueCheckResults,
  IssueDetails,
  IssueSummary,
} from "../../packages/contracts/src/dispatch-api";
import { MAX_ISSUE_PAGE_LIMIT } from "../../packages/contracts/src/dispatch-api";
import { ISSUE_STATUSES } from "../../packages/contracts/src/dispatch-tools";
import type { ActiveDispatchConfig } from "../../packages/envoy-client/src/dispatch-config";
import { DISPATCH_TOOL_DEADLINE_MS } from "../../packages/envoy-client/src/dispatch-http";
import { parseDispatchRef } from "../../packages/envoy-client/src/dispatch-owner";
import {
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";

/** Issues read at once, as the ask census reads them: a few requests' latency each, well inside
 *  one request's deadline, while the run as a whole has no deadline at all. */
export const ISSUE_CONCURRENCY = 8;

/** The two statuses whose texts the first reading leaves: a closed issue's document keeps its
 *  rule, and an issue in `retro` starts no testing round until its work goes back to testing. */
const LEFT_STATUSES = ["done", "retro"] as const;
const OPEN_STATUSES = ISSUE_STATUSES.filter(
  (status) => !(LEFT_STATUSES as readonly string[]).includes(status)
);

const VERIFICATION_SLUG = "verification-md";
const PLAN_SLUG = "plan-md";

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "credential">;

/**
 * One listed row. Each mode fills the members of its own reading, so one shape carries the whole
 * report: `texts` names the issue, the document, its version and token, the match and its
 * paragraph; `rounds` names the round, its head and session, the check and its words; `main-read`
 * names the step and what it printed.
 */
export interface CensusRow {
  readonly issue: string;
  readonly document?: string;
  readonly version?: number | null;
  readonly token?: string;
  /** Which matcher found it: `p1` to `p6`, `old-rule`, `ci-recording`. */
  readonly rules?: string[];
  readonly match?: string;
  readonly paragraph?: string;
  readonly round?: number;
  readonly head?: string;
  readonly session?: string;
  readonly check?: string;
  readonly words?: string;
  readonly step?: string;
  readonly reason?: string;
  readonly detail?: string;
}

/** What the run could not read. A row here means the run is not complete, whatever else it found. */
export interface CensusUnknown {
  readonly issue: string;
  readonly what: string;
  readonly reason: string;
}

/** A text the run did not read, with why: its issue is done, or in `retro`. */
export interface CensusNotRead {
  readonly issue: string;
  readonly reason: string;
}

/** A check a round leaves out, with the reason and what shows it. */
export interface CensusExcluded {
  readonly issue: string;
  readonly round: number;
  readonly check: string;
  readonly reason: string;
  readonly detail?: string;
}

/** A check whose command a job of an ordinary pull request runs: CI's, not the round's. */
export interface CensusCICheck {
  readonly issue: string;
  readonly round: number;
  readonly check: string;
  readonly job: string;
  readonly command: string;
}

/** One step of the reading of main, and what it did. */
export interface CensusStep {
  readonly name: string;
  readonly verdict: "pass" | "fail" | "not run";
  readonly reason?: string;
  readonly detail?: string;
}

export interface CensusReport {
  readonly mode: "texts" | "rounds" | "main-read";
  /** Whether every read the mode names answered. False where `unknown` holds a row. */
  readonly complete: boolean;
  readonly rows: CensusRow[];
  readonly unknown: CensusUnknown[];
  readonly read?: { readonly issues: number; readonly texts: number };
  readonly notRead?: CensusNotRead[];
  readonly excluded?: CensusExcluded[];
  readonly ci?: CensusCICheck[];
  /** A job a label or the merge queue gates, which is no job of an ordinary pull request. */
  readonly gated?: { readonly job: string; readonly gate: string }[];
  /** Each result the run took whose actor does not carry `proven`. */
  readonly unproven?: string[];
  readonly issues?: string[];
  readonly steps?: CensusStep[];
  /** The files the reading of main wrote for its reader, and the ones the checkout lacks. */
  readonly files?: { readonly written: string[]; readonly missing: string[] };
}

// ---------------------------------------------------------------------------
// The matchers

/** The six wordings of the old rule, p1 to p6 of the measurements this work's spec rests on. Each
 *  is matched as a fixed string over the flattened text, so bold marks and a line break inside it
 *  hide none of them. */
const FIXED: readonly (readonly [string, string])[] = [
  ["p1", "The tester repeats no check"],
  ["p2", "the tester does not repeat what CI or the implementer already ran"],
  ["p3", "does not run again a check that holds a current result"],
  ["p4", "No role repeats another role's check"],
  [
    "p5",
    "Each check is recorded once at a head, by the implementer, unless the check names another role",
  ],
  ["p6", "records a check from the CI run at the head"],
];

/** A pattern that matches nothing, for a run whose matchers are broken on purpose. */
const NEVER = /(?!x)x/;

/** The old rule in other words, and the CI-recording rule in other words: the two patterns of the
 *  measurements' section on plans that give a check one run (`v3ocounts.py`, its lines 51 to 60). */
const OLD =
  /records? (?:each|every) check once|(?:each|every) check is recorded once|recorded once,? at|tester (?:does not|doesn't|never|need not|will not) (?:run|re-?run|repeat)|repeats? no check|implementer records (?:each|every) check|no role (?:repeats|re-?runs)|(?:each|every) check (?:is )?(?:run|recorded) (?:only )?by the implementer|runs once at (?:that|the) head|(?:does not|doesn't|never) (?:run|re-?run) (?:it |a check |the row )?again/i;
const CIREC =
  /record\w*[^.]{0,60}?from (?:the |a |CI's |its )?(?:CI|GitHub)(?:'s)? (?:run|job|check run)|record\w*[^.]{0,40}?(?:result|check)[^.]{0,40}?from (?:the |a )?CI run/i;

/** A plan line that gives a check one run at a head, as the measurements' `ONCE` reads it. */
const ONCE = /\bonce,? at (?:the|that|each) head|\bits only run\b|\bruns? once\b|\bonly once\b/i;

/** A check's words that name the tester, and words that give its run to another runner. The two
 *  patterns of the measurements' runner count, which reads a check that names no tester and gives
 *  its run to a role, a person or an operator. */
const TESTER = /\btesters?\b|\btester's\b/i;
const ROLE =
  "(?:the\\s+)?(?:implementer|architect|reviewer|merger|planner|a\\s+person|owner|an?\\s+operator|controller|author)";
const RUNNER = new RegExp(
  `runner\\s*:\\s*${ROLE}|run by ${ROLE}|${ROLE}(?:\\s+or\\s+${ROLE})?\\s+(?:runs|records|reads|makes|walks|starts)\\s+(?:it|this|the\\s+check)\\b`,
  "i"
);

/** A reading that carries an earlier run rather than reporting one at this head. */
const CARRIED = /carried from\s+(\S+)/i;

/** Text as the matchers read it: bold marks gone, every run of whitespace one space, so a wording
 *  broken across lines or emphasised inside is the same string. */
function flat(text: string): string {
  return text.replace(/\*\*|\s+/g, (match) => (match === "**" ? "" : " ")).trim();
}

function paragraphsOf(markdown: string): string[] {
  return markdown
    .split(/\n\s*\n/)
    .map(flat)
    .filter((paragraph) => paragraph !== "");
}

function sentencesOf(paragraph: string): string[] {
  return paragraph.split(/(?<=[.;])\s+/);
}

/** Whether a sentence names one of the document's own checks, which makes it an owner's rule for
 *  that check rather than a rule on every check. */
function namesOwnCheck(sentence: string, ownChecks: ReadonlySet<string>): boolean {
  for (const id of ownChecks) {
    const named = new RegExp(`(^|[^\\w-])${id.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}([^\\w-]|$)`);
    if (named.test(sentence)) return true;
  }
  return false;
}

/** The ids of the checks a document declares, which its own paragraphs may narrow. */
function checkIdsOf(markdown: string): Set<string> {
  const ids = new Set<string>();
  for (const match of markdown.matchAll(/:::check\{#([^\s}]+)/g)) {
    if (match[1] !== undefined) ids.add(match[1]);
  }
  return ids;
}

interface TextMatch {
  readonly rules: string[];
  readonly match: string;
  readonly paragraph: string;
}

/**
 * Each sentence of a text that holds the old rule: first the fixed strings p1 to p6, then the two
 * patterns over the sentences that hold none of them. A pattern hit whose sentence names one of
 * the document's own checks is the owner's narrower rule for that check, and is not listed.
 */
export function matchesIn(
  markdown: string,
  ownChecks: ReadonlySet<string>,
  patterns: { readonly old: RegExp; readonly cirec: RegExp }
): TextMatch[] {
  const found: TextMatch[] = [];
  for (const paragraph of paragraphsOf(markdown)) {
    for (const sentence of sentencesOf(paragraph)) {
      const fixed = FIXED.filter(([, wording]) =>
        sentence.toLowerCase().includes(wording.toLowerCase())
      );
      if (fixed.length > 0) {
        const [, first] = fixed[0] as readonly [string, string];
        const at = sentence.toLowerCase().indexOf(first.toLowerCase());
        found.push({
          rules: fixed.map(([id]) => id),
          match: sentence.slice(at, at + first.length),
          paragraph,
        });
        continue;
      }
      const old = patterns.old.exec(sentence);
      const cirec = patterns.cirec.exec(sentence);
      if (old === null && cirec === null) continue;
      if (namesOwnCheck(sentence, ownChecks)) continue;
      found.push({
        rules: [...(old === null ? [] : ["old-rule"]), ...(cirec === null ? [] : ["ci-recording"])],
        match: (old ?? cirec)?.[0] ?? sentence,
        paragraph,
      });
    }
  }
  return found;
}

/**
 * Each line of a plan that names a check and gives it one run at a head: a table row whose first
 * cell is the id, or a paragraph, flagged where it holds a one-run wording, or where it is a row
 * `The same` under a first row that does.
 */
export function oneRunLines(markdown: string, id: string): string[] {
  const named = new RegExp(`(^|[^\\w-])${id.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}([^\\w-]|$)`);
  const flagged: string[] = [];
  let firstRowRuns: boolean | null = false;
  for (const line of markdown.split("\n")) {
    if (line.startsWith("| ---")) {
      firstRowRuns = null;
      continue;
    }
    if (line.startsWith("|") && firstRowRuns === null) firstRowRuns = ONCE.test(line);
    else if (!line.startsWith("|")) firstRowRuns = false;
    if (!named.test(line)) continue;
    if (line.startsWith("|")) {
      const cells = (line.replace(/^\||\|$/g, "").split("|")[0] ?? "")
        .split(",")
        .map((cell) => cell.trim().replace(/`/g, ""));
      if (!cells.includes(id)) continue;
      const same = line.trimEnd().replace(/\|$/, "").trimEnd().endsWith("The same");
      if (ONCE.test(line) || (same && firstRowRuns === true)) flagged.push(flat(line));
      continue;
    }
    if (ONCE.test(line)) flagged.push(flat(line));
  }
  return flagged;
}

// ---------------------------------------------------------------------------
// The reads of Dispatch

/** One read of Dispatch, with this machine's own credential and one deadline of its own. */
async function get<T>(config: DispatchConfig, path: string): Promise<T> {
  const url = `${config.url}${path}`;
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  const response = await fetch(url, {
    headers: await config.credential.headers("GET", url, undefined, deadline),
    signal: deadline,
  });
  if (!response.ok) {
    throw new Error(`GET ${path}: ${response.status} ${(await response.text()).slice(0, 200)}`);
  }
  return (await response.json()) as T;
}

function why(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

type DocumentRow = Pick<Artifact, "slug" | "name"> & { readonly role?: string };
type IssueRead = Pick<IssueDetails, "key" | "status"> & {
  readonly artifacts?: readonly DocumentRow[] | null;
};
type IssueKeyRow = Pick<IssueSummary, "key">;
interface IssuePage {
  readonly issues: IssueKeyRow[] | null;
  readonly total: number;
}

/** An event as the rounds reading takes it: its place in the issue's order, its time, and the
 *  status an `issue.*` event carries. A history older than a field carries none, so each is read
 *  as optional. */
interface RecordedEvent {
  readonly seq: number;
  readonly created_at: string;
  readonly type: string;
  readonly payload?: { readonly status?: string };
}

const EVENT_PAGE_SIZE = 200;

async function issueEvents(config: DispatchConfig, issue: string): Promise<RecordedEvent[]> {
  const events: RecordedEvent[] = [];
  let after = 0;
  while (true) {
    const page = await get<RecordedEvent[]>(
      config,
      `/api/v1/issues/${encodeURIComponent(issue)}/events?after=${after}&limit=${EVENT_PAGE_SIZE}`
    );
    events.push(...page);
    if (page.length < EVENT_PAGE_SIZE) return events;
    const last = page.at(-1);
    if (last === undefined || last.seq <= after) {
      throw new Error(`the events of ${issue} answered no event after ${after}`);
    }
    after = last.seq;
  }
}

/** Every issue of one status, page by page, with the total the listing reports. A run that read
 *  fewer rows than the total has not read the status, and its caller reports UNKNOWN. */
async function issuesOfStatus(
  config: DispatchConfig,
  project: string,
  status: string,
  onePageOf?: number
): Promise<{ keys: string[]; total: number }> {
  const keys: string[] = [];
  const limit = onePageOf ?? MAX_ISSUE_PAGE_LIMIT;
  let offset = 0;
  let total = 0;
  while (true) {
    const page = await get<IssuePage>(
      config,
      `/api/v1/issues?project=${encodeURIComponent(project)}&status=${encodeURIComponent(status)}` +
        `&limit=${limit}&offset=${offset}`
    );
    total = page.total;
    const rows = page.issues ?? [];
    keys.push(...rows.map((row) => row.key));
    offset += rows.length;
    if (onePageOf !== undefined || rows.length === 0 || keys.length >= total) {
      return { keys, total };
    }
  }
}

async function issueRead(config: DispatchConfig, issue: string): Promise<IssueRead> {
  return get<IssueRead>(config, `/api/v1/issues/${encodeURIComponent(issue)}`);
}

async function documentText(
  config: DispatchConfig,
  issue: string,
  slug: string
): Promise<ArtifactText> {
  return get<ArtifactText>(
    config,
    `/api/v1/issues/${encodeURIComponent(issue)}/artifacts/${encodeURIComponent(slug)}/text`
  );
}

/** Reads every key, `ISSUE_CONCURRENCY` at a time, in the order given. */
async function eachIssue<T>(
  keys: readonly string[],
  read: (key: string) => Promise<T>
): Promise<T[]> {
  const answers: T[] = [];
  let next = 0;
  const reader = async (): Promise<void> => {
    while (next < keys.length) {
      const index = next++;
      answers[index] = await read(keys[index] as string);
    }
  };
  await Promise.all(Array.from({ length: Math.min(ISSUE_CONCURRENCY, keys.length) }, reader));
  return answers;
}

/** The document a reference edge names: an artifact node is its own document, and a block node is
 *  the document it belongs to. */
function documentOfEdge(edge: GraphEdge): { issue: string; slug: string } | undefined {
  const { node } = edge;
  const reference = node.kind === "artifact" ? node.ref : node.document;
  if (reference === undefined) return undefined;
  const parsed = parseDispatchRef(reference);
  if (parsed === null || parsed.kind !== "artifact" || parsed.owner.kind !== "issue") {
    return undefined;
  }
  return { issue: parsed.owner.issue, slug: parsed.id };
}

// ---------------------------------------------------------------------------
// `texts`

interface TextsOptions {
  readonly mode: "texts";
  readonly project: string;
  readonly keysPath?: string;
  readonly referencedBy?: string;
  readonly onePageOf?: number;
  readonly breakPatterns: boolean;
  readonly json: boolean;
}

/** What one issue's texts are read for: the slugs this run wants of it. */
interface TextWork {
  readonly issue: string;
  readonly slugs: Set<string>;
}

function wants(work: Map<string, TextWork>, issue: string, slug: string): void {
  const held = work.get(issue);
  if (held === undefined) work.set(issue, { issue, slugs: new Set([slug]) });
  else held.slugs.add(slug);
}

/** The document of an issue's list under a slug: the slug itself, else the document whose role is
 *  the one that slug names. A list that names neither has no such document. */
function documentNamed(issue: IssueRead, slug: string): DocumentRow | undefined {
  const documents = issue.artifacts ?? [];
  const bySlug = documents.find((document) => document.slug === slug);
  if (bySlug !== undefined) return bySlug;
  const role =
    slug === VERIFICATION_SLUG ? "verification" : slug === PLAN_SLUG ? "plan" : undefined;
  return role === undefined ? undefined : documents.find((document) => document.role === role);
}

async function runTexts(config: DispatchConfig, options: TextsOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const notRead: CensusNotRead[] = [];
  const patterns = options.breakPatterns
    ? { old: NEVER, cirec: NEVER }
    : { old: OLD, cirec: CIREC };
  const work = new Map<string, TextWork>();
  const fromReferences: { issue: string; slug: string }[] = [];

  if (options.keysPath === undefined) {
    const reference = options.referencedBy;
    if (reference === undefined) {
      throw new Error(
        "texts without --keys needs --referenced-by: the reference of the comment " +
          "whose Referenced by list names the documents of the first source"
      );
    }
    const references = await get<Pick<GraphReferences, "edges">>(
      config,
      `/api/v1/references?to=${encodeURIComponent(reference)}`
    );
    const edges = references.edges ?? [];
    if (edges.length === 0) {
      unknown.push({
        issue: "",
        what: reference,
        reason: "the Referenced by list of that comment named no document",
      });
    }
    for (const edge of edges) {
      const document = documentOfEdge(edge);
      if (document === undefined) continue;
      if (!fromReferences.some((held) => held.issue === document.issue)) {
        fromReferences.push(document);
      }
    }
    for (const status of OPEN_STATUSES) {
      const listing = await issuesOfStatus(config, options.project, status, options.onePageOf);
      if (listing.keys.length < listing.total) {
        unknown.push({
          issue: "",
          what: `the ${status} listing of ${options.project}`,
          reason: `it holds ${listing.keys.length} of ${listing.total} issues`,
        });
      }
      for (const key of listing.keys) wants(work, key, VERIFICATION_SLUG);
    }
    for (const document of fromReferences) {
      wants(work, document.issue, document.slug);
      wants(work, document.issue, PLAN_SLUG);
    }
  } else {
    const listed = (await readFile(options.keysPath, "utf8"))
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line !== "" && !line.startsWith("#"));
    if (listed.length === 0) {
      unknown.push({ issue: "", what: options.keysPath, reason: "the file names no issue" });
    }
    for (const key of listed) {
      wants(work, key, VERIFICATION_SLUG);
      wants(work, key, PLAN_SLUG);
    }
  }

  const keys = [...work.keys()].sort();
  let issuesRead = 0;
  let textsRead = 0;

  const readIssue = async (key: string): Promise<void> => {
    const wanted = work.get(key);
    if (wanted === undefined) return;
    let issue: IssueRead;
    try {
      issue = await issueRead(config, key);
    } catch (error) {
      unknown.push({ issue: key, what: "the issue", reason: why(error) });
      return;
    }
    issuesRead += 1;
    // A listed issue is read whether it is done or not; the two sources of a whole run leave a
    // done or `retro` issue's texts, and the reading names it.
    if (
      options.keysPath === undefined &&
      (LEFT_STATUSES as readonly string[]).includes(issue.status)
    ) {
      notRead.push({ issue: key, reason: issue.status });
      return;
    }
    for (const slug of [...wanted.slugs].sort()) {
      const named = documentNamed(issue, slug);
      // A document the issue's list does not name counts as read, with no checks.
      if (named === undefined) continue;
      let text: ArtifactText;
      try {
        text = await documentText(config, key, named.slug);
      } catch (error) {
        unknown.push({ issue: key, what: named.slug, reason: why(error) });
        continue;
      }
      textsRead += 1;
      for (const found of matchesIn(text.markdown, checkIdsOf(text.markdown), patterns)) {
        rows.push({
          issue: key,
          document: named.slug,
          version: text.version,
          token: text.token,
          ...found,
        });
      }
    }
  };

  await eachIssue(keys, readIssue);
  if (keys.length === 0) {
    unknown.push({ issue: "", what: "the list of issues", reason: "it holds no issue" });
  }
  rows.sort((left, right) => left.issue.localeCompare(right.issue));
  notRead.sort((left, right) => left.issue.localeCompare(right.issue));
  return {
    mode: "texts",
    complete: unknown.length === 0,
    rows,
    unknown,
    read: { issues: issuesRead, texts: textsRead },
    notRead,
  };
}

// ---------------------------------------------------------------------------
// The reads of the checkout

interface GitRun {
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;
}

function git(repo: string, args: readonly string[]): GitRun {
  const run = Bun.spawnSync(["git", "-C", repo, ...args], { stdout: "pipe", stderr: "pipe" });
  return {
    exitCode: run.exitCode,
    stdout: run.stdout.toString(),
    stderr: run.stderr.toString(),
  };
}

/** A command as the comparison reads it: every run of whitespace one space. */
function command(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** A job of a workflow, as the reading of a parsed workflow narrows it: a workflow file is
 *  whatever the checkout holds, so each member is read and checked rather than assumed. */
interface WorkflowJob {
  readonly gate?: string;
  readonly uses?: string;
  readonly runs: string[];
}

/** A job's gate, where a label or the merge queue decides whether it runs on an ordinary pull
 *  request. Such a job proves no check at the head of one. */
function gateOf(value: object): string | undefined {
  const condition = "if" in value && typeof value.if === "string" ? value.if : "";
  const inputs = "with" in value ? JSON.stringify(value.with) : "";
  const words = `${condition} ${inputs}`;
  return /full-ci|merge_group/.test(words) ? command(words) : undefined;
}

function jobOf(value: unknown): WorkflowJob {
  if (value === null || typeof value !== "object") return { runs: [] };
  const gate = gateOf(value);
  const uses = "uses" in value && typeof value.uses === "string" ? value.uses : undefined;
  const steps = "steps" in value && Array.isArray(value.steps) ? value.steps : [];
  const runs: string[] = [];
  for (const step of steps) {
    if (
      step !== null &&
      typeof step === "object" &&
      "run" in step &&
      typeof step.run === "string"
    ) {
      runs.push(step.run);
    }
  }
  return { runs, ...(gate === undefined ? {} : { gate }), ...(uses === undefined ? {} : { uses }) };
}

/** The jobs of a parsed workflow, by their ids. */
function jobsOf(workflow: unknown): Map<string, WorkflowJob> {
  if (workflow === null || typeof workflow !== "object" || !("jobs" in workflow)) return new Map();
  const jobs = workflow.jobs;
  if (jobs === null || typeof jobs !== "object") return new Map();
  return new Map(Object.entries(jobs).map(([id, job]) => [id, jobOf(job)]));
}

/** Whether a parsed workflow runs on an ordinary pull request. */
function runsOnPullRequest(workflow: unknown): boolean {
  if (workflow === null || typeof workflow !== "object" || !("on" in workflow)) return false;
  const on = workflow.on;
  if (typeof on === "string") return on === "pull_request";
  if (Array.isArray(on)) return on.includes("pull_request");
  return on !== null && typeof on === "object" && "pull_request" in on;
}

/** A workflow file as YAML, or nothing where it is no YAML object. */
function parseWorkflow(body: string): unknown {
  try {
    return Bun.YAML.parse(body);
  } catch {
    return undefined;
  }
}

/** What the workflows at one head say: each step that records a result, each command a job of an
 *  ordinary pull request runs, and each job a label or the merge queue gates. */
interface WorkflowReads {
  readonly recorders: { file: string; line: number; text: string }[];
  readonly commands: { job: string; command: string }[];
  readonly gated: { job: string; gate: string }[];
}

function readWorkflows(repo: string, head: string): WorkflowReads {
  const recorders: WorkflowReads["recorders"] = [];
  const commands: WorkflowReads["commands"] = [];
  const gated: WorkflowReads["gated"] = [];
  const bodies = new Map<string, string>();
  const bodyOf = (path: string): string | undefined => {
    if (bodies.has(path)) return bodies.get(path);
    const shown = git(repo, ["show", `${head}:${path}`]);
    if (shown.exitCode !== 0) return undefined;
    bodies.set(path, shown.stdout);
    return shown.stdout;
  };

  // A check that a workflow step records is left out of the round, so the recording steps are
  // read first, from the search the reading names.
  const search = git(repo, ["grep", "-n", "-E", "dispatch result( |$)", head, "--", ".github/"]);
  for (const line of search.stdout.split("\n")) {
    const matched = line.match(/^[^:]*:([^:]+):(\d+):(.*)$/);
    if (matched === null) continue;
    const [, file = "", number = "0", text = ""] = matched;
    const body = bodyOf(file);
    const lines = body === undefined ? [text] : body.split("\n");
    const at = Number(number) - 1;
    const whole = body === undefined ? text : lines.slice(at, at + 3).join("\n");
    recorders.push({ file, line: Number(number), text: whole });
  }

  const listed = git(repo, ["ls-tree", "-r", "--name-only", head, "--", ".github/workflows"]);
  const stepCommands = (file: string, jobs: Map<string, WorkflowJob>): void => {
    for (const [id, job] of jobs) {
      if (job.gate !== undefined) {
        gated.push({ job: `${file} ${id}`, gate: job.gate });
        continue;
      }
      for (const run of job.runs) commands.push({ job: `${file} ${id}`, command: run });
      const called = job.uses?.match(/^\.\/(\.github\/workflows\/[^@]+)$/);
      if (called?.[1] === undefined) continue;
      const body = bodyOf(called[1]);
      if (body === undefined) continue;
      stepCommands(called[1], jobsOf(parseWorkflow(body)));
    }
  };
  for (const path of listed.stdout.split("\n").filter((line) => line !== "")) {
    const body = bodyOf(path);
    if (body === undefined) continue;
    const parsed = parseWorkflow(body);
    if (!runsOnPullRequest(parsed)) continue;
    stepCommands(path, jobsOf(parsed));
  }
  return { recorders, commands, gated };
}

// ---------------------------------------------------------------------------
// `rounds`

interface RoundsOptions {
  readonly mode: "rounds";
  readonly project: string;
  readonly since: string;
  readonly repo: string;
  readonly out: string;
  readonly json: boolean;
}

/** One testing round: the handoff that ends it, the head it tested, the session that wrote it,
 *  and the move into testing that started it. */
interface Round {
  readonly version: number;
  readonly head: string;
  readonly session: string;
  readonly from: number;
  readonly to: number;
}

function time(value: string): number {
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) throw new Error(`not a time: ${value}`);
  return parsed;
}

/** Every link a result names, which the reading of its run reads. */
function linksOf(result: Pick<CheckResult, "run" | "observed">): string[] {
  const text = `${result.run} ${result.observed}`;
  return [...text.matchAll(/(https?:\/\/[^\s)"']+|dispatch:\/\/[^\s)"']+)/g)].map(
    (match) => match[0]
  );
}

async function readLink(config: DispatchConfig, link: string): Promise<string> {
  if (link.startsWith("dispatch://")) {
    const parsed = parseDispatchRef(link);
    if (parsed === null || parsed.kind !== "artifact" || parsed.owner.kind !== "issue") {
      throw new Error(`the census reads no output at ${link}`);
    }
    return (await documentText(config, parsed.owner.issue, parsed.id)).markdown;
  }
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  const response = await fetch(link, { signal: deadline });
  if (!response.ok) throw new Error(`GET ${link}: ${response.status}`);
  return response.text();
}

/** Whether an output names the head, as a word: a run at another commit does not. */
function namesHead(output: string, head: string): boolean {
  const named = new RegExp(`(^|[^0-9a-zA-Z])${head.replace(/[^0-9a-zA-Z]/g, "")}([^0-9a-zA-Z]|$)`);
  return named.test(output);
}

async function runRounds(config: DispatchConfig, options: RoundsOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const excluded: CensusExcluded[] = [];
  const ci: CensusCICheck[] = [];
  const gated: { job: string; gate: string }[] = [];
  const unproven: string[] = [];
  const listed: string[] = [];
  const since = time(options.since);

  const updated = await get<IssueKeyRow[]>(
    config,
    `/api/v1/issues?project=${encodeURIComponent(options.project)}` +
      `&updated_since=${encodeURIComponent(options.since)}`
  );
  if (updated.length === 0) {
    unknown.push({
      issue: "",
      what: `the issues of ${options.project} updated since ${options.since}`,
      reason: "the listing holds no issue",
    });
  }

  const readIssue = async (key: string): Promise<void> => {
    let events: RecordedEvent[];
    try {
      events = await issueEvents(config, key);
    } catch (error) {
      unknown.push({ issue: key, what: "the events", reason: why(error) });
      return;
    }
    const moves = events
      .filter(
        (event) =>
          event.type.startsWith("issue.") &&
          event.payload?.status === "testing" &&
          time(event.created_at) >= since
      )
      .map((event) => time(event.created_at));
    if (moves.length === 0) return;
    listed.push(key);

    let handoffs: HandoffSummary[];
    try {
      handoffs = await get<HandoffSummary[]>(
        config,
        `/api/v1/issues/${encodeURIComponent(key)}/handoffs`
      );
    } catch (error) {
      unknown.push({ issue: key, what: "the handoffs", reason: why(error) });
      return;
    }
    const tests = handoffs
      .filter((handoff) => handoff.phase === "test")
      .sort((left, right) => left.version - right.version);
    if (tests.length === 0) {
      unknown.push({
        issue: key,
        what: "the test handoffs",
        reason: "the issue moved into testing and holds no test handoff, so no round can be read",
      });
      return;
    }

    const rounds: Round[] = [];
    for (const summary of tests) {
      let handoff: Handoff;
      try {
        handoff = await get<Handoff>(config, `/api/v1/handoffs/${encodeURIComponent(summary.id)}`);
      } catch (error) {
        unknown.push({ issue: key, what: `test handoff ${summary.version}`, reason: why(error) });
        continue;
      }
      let head: unknown;
      try {
        const body: unknown = JSON.parse(handoff.body);
        head = body !== null && typeof body === "object" && "head" in body ? body.head : undefined;
      } catch (error) {
        unknown.push({
          issue: key,
          what: `test handoff ${summary.version}`,
          reason: `its body is no JSON object: ${why(error)}`,
        });
        continue;
      }
      if (typeof head !== "string" || head === "") {
        unknown.push({
          issue: key,
          what: `test handoff ${summary.version}`,
          reason: "its body names no head",
        });
        continue;
      }
      const to = time(handoff.created_at);
      const starts = moves.filter((move) => move <= to);
      rounds.push({
        version: summary.version,
        head,
        session: handoff.author.id,
        from: starts.length === 0 ? since : Math.max(...starts),
        to,
      });
    }

    let results: IssueCheckResults;
    try {
      results = await get<IssueCheckResults>(
        config,
        `/api/v1/issues/${encodeURIComponent(key)}/results`
      );
    } catch (error) {
      unknown.push({ issue: key, what: "the results", reason: why(error) });
      return;
    }

    let plan = "";
    try {
      const issue = await issueRead(config, key);
      const named = documentNamed(issue, PLAN_SLUG);
      if (named !== undefined) plan = (await documentText(config, key, named.slug)).markdown;
    } catch (error) {
      unknown.push({ issue: key, what: PLAN_SLUG, reason: why(error) });
      return;
    }

    for (const round of rounds) {
      const workflows = readWorkflows(options.repo, round.head);
      for (const job of workflows.gated) gated.push(job);
      for (const check of results.checks ?? []) {
        const left = await judgeCheck({
          config,
          issue: key,
          round,
          check,
          plan,
          workflows,
        });
        if (left.excluded !== undefined) excluded.push(left.excluded);
        if (left.ci !== undefined) ci.push(left.ci);
        if (left.row !== undefined) rows.push(left.row);
        if (left.unknown !== undefined) unknown.push(left.unknown);
        if (left.unproven !== undefined) unproven.push(left.unproven);
      }
    }
  };

  await eachIssue(
    updated.map((row) => row.key),
    readIssue
  );

  listed.sort();
  await mkdir(options.out, { recursive: true });
  await writeFile(join(options.out, "keys.txt"), listed.map((key) => `${key}\n`).join(""));
  return {
    mode: "rounds",
    complete: unknown.length === 0,
    rows,
    unknown,
    excluded,
    ci,
    gated,
    unproven,
    issues: listed,
  };
}

/** What one check of one round is: left out, CI's, listed, or a run the reading could read. */
interface CheckVerdict {
  readonly excluded?: CensusExcluded;
  readonly ci?: CensusCICheck;
  readonly row?: CensusRow;
  readonly unknown?: CensusUnknown;
  readonly unproven?: string;
}

async function judgeCheck(input: {
  config: DispatchConfig;
  issue: string;
  round: Round;
  check: CheckBlock;
  plan: string;
  workflows: WorkflowReads;
}): Promise<CheckVerdict> {
  const { config, issue, round, check, plan, workflows } = input;
  const id = check.block_id;
  const place = { issue, round: round.version, check: id };
  if (check.when !== "before_merge") {
    return { excluded: { ...place, reason: `its when is ${check.when}` } };
  }
  if (check.ci_check !== undefined && check.ci_check !== "") {
    return { excluded: { ...place, reason: "it carries ci_check" } };
  }
  const recorder = workflows.recorders.find((step) => namesOwnCheck(step.text, new Set([id])));
  if (recorder !== undefined) {
    return {
      excluded: {
        ...place,
        reason: "a workflow step records it",
        detail: `${recorder.file}:${recorder.line}: ${command(recorder.text)}`,
      },
    };
  }
  const words = `${check.run}\n${check.text}`;
  if (!TESTER.test(words) && RUNNER.test(words)) {
    return { excluded: { ...place, reason: "its words give its run only to another runner" } };
  }
  const oneRun = oneRunLines(plan, id);
  if (oneRun.length > 0) {
    return {
      excluded: {
        ...place,
        reason: "its plan gives it one run at a head",
        detail: oneRun.join(" "),
      },
    };
  }
  const job = workflows.commands.find(
    (entry) => check.run !== "" && command(entry.command).includes(command(check.run))
  );
  if (job !== undefined) {
    return { ci: { ...place, job: job.job, command: command(check.run) } };
  }

  const taken = (check.results ?? [])
    .filter(
      (result) =>
        result.session === round.session &&
        result.commit_sha === round.head &&
        time(result.created_at) > round.from &&
        time(result.created_at) <= round.to
    )
    .sort((left, right) => time(left.created_at) - time(right.created_at))
    .at(-1);
  const row = {
    ...place,
    head: round.head,
    session: round.session,
    words: flat(check.text),
  };
  if (taken === undefined) {
    return { row: { ...row, reason: "no result of the round's session at the head" } };
  }
  const unproven = taken.actor.proven === true ? undefined : taken.id;
  const carried = CARRIED.exec(`${taken.run} ${taken.observed}`);
  if (carried !== null) {
    return {
      row: { ...row, reason: `its reading carries an earlier run: ${carried[0]}` },
      ...(unproven === undefined ? {} : { unproven }),
    };
  }
  const links = linksOf(taken);
  if (links.length === 0) {
    return {
      unknown: {
        issue,
        what: `${id} at ${round.head}`,
        reason: "its result links no output, so the reading cannot see a run",
      },
      ...(unproven === undefined ? {} : { unproven }),
    };
  }
  const outputs: string[] = [];
  for (const link of links) {
    try {
      outputs.push(await readLink(config, link));
    } catch (error) {
      return {
        unknown: {
          issue,
          what: `${id} at ${round.head}`,
          reason: `its output did not load: ${why(error)}`,
        },
        ...(unproven === undefined ? {} : { unproven }),
      };
    }
  }
  if (!outputs.some((output) => namesHead(output, round.head))) {
    return {
      row: { ...row, reason: "its output shows no run at the head" },
      ...(unproven === undefined ? {} : { unproven }),
    };
  }
  return unproven === undefined ? {} : { unproven };
}

// ---------------------------------------------------------------------------
// `main-read`

interface ReadingOptions {
  readonly mode: "main-read";
  readonly merge: string;
  readonly after: number;
  readonly repo: string;
  readonly out: string;
}

const PROMPTS = "packages/daemon/internal/prompts";

/** The three files of the tester whose words the failure grep reads. */
const TESTER_FILES = [
  `${PROMPTS}/roles/tester.md`,
  `${PROMPTS}/roles/core/tester.md`,
  `${PROMPTS}/go/tester.md`,
];

/** The eight files whose readers the failure grep reads. */
const READER_FILES = [
  `${PROMPTS}/go/architect-common.md`,
  `${PROMPTS}/roles/architect.md`,
  `${PROMPTS}/roles/architect-root.md`,
  `${PROMPTS}/roles/implementer.md`,
  `${PROMPTS}/roles/core/implementer.md`,
  `${PROMPTS}/roles/reviewer.md`,
  `${PROMPTS}/roles/core/reviewer.md`,
  `${PROMPTS}/roles/tester.md`,
];

const WORKER_COMMON = `${PROMPTS}/go/worker-common.md`;

/** Each file a reader of these steps reads at the head, which the run writes out for it. */
const READING_FILES = [
  ...TESTER_FILES,
  ...READER_FILES,
  WORKER_COMMON,
  `${PROMPTS}/roles/core/common.md`,
  `${PROMPTS}/mechanics/headless.md`,
  `${PROMPTS}/roles/mechanics/headless.md`,
  `${PROMPTS}/go/reviewer.md`,
  `${PROMPTS}/roles_test.go`,
  "packages/pi-legion/scripts/skill-scenarios/gate-search.ts",
  "packages/pi-legion/scripts/skill-scenarios/label-rules.ts",
  "packages/pi-legion/scripts/skill-scenarios/rule-search.test.ts",
  "packages/daemon/.changes/unreleased/pr4230-ci-job-results.yaml",
  "packages/claude-envoy/.changes/unreleased/pr4230-ci-job-results.yaml",
];

/** The three pin tests the reading runs where the packages hold them. */
const PIN_TESTS = [
  "TestJudgeStates",
  "TestCIResultRulesLiveInRolePrompts",
  "TestNoRolePartAsksAPassOfNamedRoles",
];

/** One grep of the reading: the pattern, the files, and whether a line printed is a failure. */
interface GrepStep {
  readonly name: string;
  readonly pattern: string;
  readonly files: readonly string[];
  readonly wants: "nothing" | "a line";
}

const GREPS: readonly GrepStep[] = [
  {
    name: "v-tester-failures step 1",
    pattern:
      "implementerProof|`failures`|`failed`|no pass standing from the implementer|does not read yes from the implementer",
    files: TESTER_FILES,
    wants: "nothing",
  },
  {
    name: "v-prompt-readers step 1",
    pattern: "implementerProof|`failures`|`failed`|tester.s .?failures",
    files: READER_FILES,
    wants: "nothing",
  },
  {
    name: "v-retract step 1",
    pattern: "dispatch result --retract",
    files: [WORKER_COMMON],
    wants: "a line",
  },
];

function orderStep(options: ReadingOptions): { step: CensusStep; row?: CensusRow } {
  const view = Bun.spawnSync(["gh", "pr", "view", String(options.after), "--json", "mergeCommit"], {
    cwd: options.repo,
    stdout: "pipe",
    stderr: "pipe",
  });
  if (view.exitCode !== 0) {
    const reason = `gh pr view ${options.after} answered ${view.exitCode}: ${command(view.stderr.toString()).slice(0, 200)}`;
    return {
      step: { name: "order", verdict: "fail", reason },
      row: { issue: "", step: "order", reason },
    };
  }
  let oid: unknown;
  try {
    const answer: unknown = JSON.parse(view.stdout.toString());
    const merge =
      answer !== null && typeof answer === "object" && "mergeCommit" in answer
        ? answer.mergeCommit
        : undefined;
    oid = merge !== null && typeof merge === "object" && "oid" in merge ? merge.oid : undefined;
  } catch (error) {
    const reason = `gh pr view ${options.after} answered no JSON: ${why(error)}`;
    return {
      step: { name: "order", verdict: "fail", reason },
      row: { issue: "", step: "order", reason },
    };
  }
  if (typeof oid !== "string" || oid === "") {
    const reason = `pull request ${options.after} has no merge commit, so the order step fails`;
    return {
      step: { name: "order", verdict: "fail", reason },
      row: { issue: "", step: "order", reason },
    };
  }
  const ancestor = git(options.repo, ["merge-base", "--is-ancestor", oid, options.merge]);
  if (ancestor.exitCode === 0) return { step: { name: "order", verdict: "pass", detail: oid } };
  if (ancestor.exitCode === 1) {
    const reason = `the merge commit of pull request ${options.after} (${oid}) is no ancestor of the merge read (${options.merge})`;
    return {
      step: { name: "order", verdict: "fail", reason },
      row: { issue: "", step: "order", reason },
    };
  }
  const reason = `git merge-base --is-ancestor answered ${ancestor.exitCode}: ${command(ancestor.stderr)}`;
  return {
    step: { name: "order", verdict: "fail", reason },
    row: { issue: "", step: "order", reason },
  };
}

function grepStep(
  repo: string,
  step: GrepStep
): { step: CensusStep; row?: CensusRow; unknown?: CensusUnknown } {
  const run = git(repo, ["grep", "-n", "-E", step.pattern, "--", ...step.files]);
  if (run.exitCode > 1) {
    const reason = `git grep answered ${run.exitCode}: ${command(run.stderr).slice(0, 300)}`;
    return {
      step: { name: step.name, verdict: "not run", reason },
      unknown: { issue: "", what: step.name, reason },
    };
  }
  const printed = run.stdout.split("\n").filter((line) => line !== "");
  if (step.wants === "nothing") {
    if (printed.length === 0) return { step: { name: step.name, verdict: "pass" } };
    const reason = `it printed ${printed.length} line(s) and must print none`;
    return {
      step: { name: step.name, verdict: "fail", reason, detail: printed.join("\n") },
      row: { issue: "", step: step.name, reason, detail: printed.join("\n") },
    };
  }
  if (printed.length > 0) return { step: { name: step.name, verdict: "pass" } };
  const reason = "it printed nothing and must print at least one line";
  return {
    step: { name: step.name, verdict: "fail", reason },
    row: { issue: "", step: step.name, reason },
  };
}

function pinTestStep(repo: string, name: string): { step: CensusStep; row?: CensusRow } {
  const held = git(repo, ["grep", "-l", "-E", `^func ${name}\\(`, "--", "packages/daemon"]);
  const files = held.stdout.split("\n").filter((line) => line !== "");
  const file = files[0];
  if (held.exitCode > 1 || file === undefined) {
    return {
      step: {
        name,
        verdict: "not run",
        reason: `no package under packages/daemon holds it (git grep answered ${held.exitCode})`,
      },
    };
  }
  const relative = `./${dirname(file).replace(/^packages\/daemon\//, "")}/`;
  const run = Bun.spawnSync(
    [
      "go",
      "-C",
      join(repo, "packages/daemon"),
      "test",
      "-count=1",
      "-v",
      "-run",
      `^${name}$`,
      relative,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  const output = `${run.stdout.toString()}${run.stderr.toString()}`;
  // A run that selected no test, or that skipped, proves nothing and fails its step.
  if (output.includes("--- SKIP") || output.includes("no tests to run")) {
    const reason = "the run selected no test, or skipped it";
    return {
      step: { name, verdict: "fail", reason, detail: output.slice(-2000) },
      row: { issue: "", step: name, reason },
    };
  }
  if (run.exitCode === 0) return { step: { name, verdict: "pass", detail: relative } };
  const reason = `go test answered ${run.exitCode} in ${relative}`;
  return {
    step: { name, verdict: "fail", reason, detail: output.slice(-2000) },
    row: { issue: "", step: name, reason, detail: output.slice(-2000) },
  };
}

async function runMainRead(options: ReadingOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const steps: CensusStep[] = [];

  const inside = git(options.repo, ["rev-parse", "--git-dir"]);
  if (inside.exitCode !== 0) {
    unknown.push({
      issue: "",
      what: options.repo,
      reason: `it is no git checkout: ${command(inside.stderr)}`,
    });
  }

  const order = orderStep(options);
  steps.push(order.step);
  if (order.row !== undefined) rows.push(order.row);

  for (const step of GREPS) {
    const run = grepStep(options.repo, step);
    steps.push(run.step);
    if (run.row !== undefined) rows.push(run.row);
    if (run.unknown !== undefined) unknown.push(run.unknown);
  }

  for (const name of PIN_TESTS) {
    const run = pinTestStep(options.repo, name);
    steps.push(run.step);
    if (run.row !== undefined) rows.push(run.row);
  }

  const written: string[] = [];
  const missing: string[] = [];
  for (const path of READING_FILES) {
    if (written.includes(path) || missing.includes(path)) continue;
    const shown = git(options.repo, ["show", `${options.merge}:${path}`]);
    if (shown.exitCode !== 0) {
      missing.push(path);
      continue;
    }
    const target = join(options.out, "files", path);
    await mkdir(dirname(target), { recursive: true });
    await writeFile(target, shown.stdout);
    written.push(path);
  }
  await writeFile(
    join(options.out, "files.txt"),
    [...written.map((path) => `read ${path}`), ...missing.map((path) => `absent ${path}`)]
      .map((line) => `${line}\n`)
      .join("")
  );

  return {
    mode: "main-read",
    complete: unknown.length === 0,
    rows,
    unknown,
    steps,
    files: { written, missing },
  };
}

// ---------------------------------------------------------------------------
// The run

function printReport(report: CensusReport): void {
  if (report.steps !== undefined) {
    console.log("Steps");
    console.table(report.steps.map(({ name, verdict, reason }) => ({ name, verdict, reason })));
  }
  if (report.read !== undefined) {
    console.log(`Read: ${report.read.issues} issues, ${report.read.texts} texts`);
  }
  if (report.notRead !== undefined && report.notRead.length > 0) {
    console.log("Not read");
    console.table(report.notRead);
  }
  if (report.excluded !== undefined && report.excluded.length > 0) {
    console.log("Left out of the round");
    console.table(report.excluded);
  }
  if (report.ci !== undefined && report.ci.length > 0) {
    console.log("CI's checks");
    console.table(report.ci);
  }
  if (report.gated !== undefined && report.gated.length > 0) {
    console.log("Jobs a label or the merge queue gates, which prove no check at a head");
    console.table(report.gated);
  }
  if (report.unproven !== undefined && report.unproven.length > 0) {
    console.log(`Results taken whose actor does not carry proven: ${report.unproven.join(", ")}`);
  }
  if (report.files !== undefined) {
    console.log(
      `Files written: ${report.files.written.length}; the checkout lacks ${report.files.missing.length}`
    );
  }
  console.log("Listed");
  console.table(report.rows);
  if (report.unknown.length > 0) {
    console.log("UNKNOWN");
    console.table(report.unknown);
  }
  console.log(
    report.unknown.length > 0
      ? "UNKNOWN: the run is not complete."
      : report.rows.length > 0
        ? `Complete, with ${report.rows.length} row(s) for a reader.`
        : "Complete, and nothing is listed."
  );
}

type Options = TextsOptions | RoundsOptions | ReadingOptions;

function parseArguments(argv: readonly string[]): Options {
  const { values, positionals } = parseArgs({
    args: [...argv],
    options: {
      project: { type: "string" },
      keys: { type: "string" },
      "referenced-by": { type: "string" },
      limit: { type: "string" },
      "break-patterns": { type: "boolean" },
      since: { type: "string" },
      repo: { type: "string" },
      out: { type: "string" },
      merge: { type: "string" },
      after: { type: "string" },
      json: { type: "boolean" },
    },
    allowPositionals: true,
    strict: true,
  });
  const mode = positionals[0];
  if (mode === undefined) {
    throw new Error("name one mode: texts, rounds or main-read");
  }
  if (positionals.length > 1) throw new Error(`one mode only, got ${positionals.join(" ")}`);
  const json = values.json === true;
  if (mode === "texts") {
    const { project, keys, limit } = values;
    if (project === undefined || project === "") throw new Error("--project is required");
    if (keys === undefined && values["referenced-by"] === undefined) {
      throw new Error(
        "--referenced-by is required without --keys: the reference of the comment whose " +
          "Referenced by list names the documents of the first source"
      );
    }
    const onePageOf = limit === undefined ? undefined : Number(limit);
    if (onePageOf !== undefined && (!Number.isInteger(onePageOf) || onePageOf < 1)) {
      throw new Error("--limit takes one whole number of issues a page");
    }
    return {
      mode,
      project,
      breakPatterns: values["break-patterns"] === true,
      json,
      ...(keys === undefined ? {} : { keysPath: keys }),
      ...(values["referenced-by"] === undefined ? {} : { referencedBy: values["referenced-by"] }),
      ...(onePageOf === undefined ? {} : { onePageOf }),
    };
  }
  if (mode === "rounds") {
    const { project, since, repo, out } = values;
    if (project === undefined || project === "") throw new Error("--project is required");
    if (since === undefined) throw new Error("--since is required: an RFC 3339 time");
    if (Number.isNaN(Date.parse(since))) throw new Error(`--since is no time: ${since}`);
    if (repo === undefined) throw new Error("--repo is required: a checkout at the round's head");
    if (out === undefined) throw new Error("--out is required: where the run writes keys.txt");
    return { mode, project, since, repo, out, json };
  }
  if (mode === "main-read") {
    const { merge, after, repo, out } = values;
    if (merge === undefined) throw new Error("--merge is required: the merge commit read");
    if (after === undefined) {
      throw new Error("--after is required: the pull request whose merge must come first");
    }
    const number = Number(after);
    if (!Number.isInteger(number) || number < 1) {
      throw new Error("--after takes one pull request number");
    }
    if (repo === undefined) throw new Error("--repo is required: a checkout at main");
    if (out === undefined) throw new Error("--out is required: where the run writes the files");
    return { mode, merge, after: number, repo, out };
  }
  throw new Error(`no such mode: ${mode}. The modes are texts, rounds and main-read.`);
}

function dispatchConfig(): DispatchConfig {
  const { dispatch: resolution, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  // A checkout's OpenCode-era file, which is not read, is said on every run while it is there.
  for (const warning of warnings) console.error(`dispatch: ${warning}`);
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `Dispatch is not configured: ${resolution.notice}`
        : `dispatch config: ${resolution.error}`
    );
  }
  return resolution;
}

async function run(options: Options): Promise<number> {
  const report =
    options.mode === "main-read"
      ? await runMainRead(options)
      : options.mode === "texts"
        ? await runTexts(dispatchConfig(), options)
        : await runRounds(dispatchConfig(), options);
  if (options.mode === "main-read") {
    await mkdir(options.out, { recursive: true });
    await writeFile(join(options.out, "report.json"), `${JSON.stringify(report, null, 1)}\n`);
    printReport(report);
  } else if (options.json) {
    // One object and nothing else on stdout, so a program can read the whole of it.
    console.log(JSON.stringify(report));
  } else {
    printReport(report);
  }
  if (report.unknown.length > 0) return 3;
  return report.rows.length > 0 ? 1 : 0;
}

if (import.meta.main) process.exit(await run(parseArguments(Bun.argv.slice(2))));
