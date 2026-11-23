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
 * for a reader. Exit 3: UNKNOWN, with the reason — a list below its total, a text, page or
 * handoff that did not load, an issue Dispatch does not know, or no issue or round at all. A run
 * that enumerated nothing says so rather than reading clean, and a source it could not read is
 * a row of the report's `unknown`, never a bare error.
 */

import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, relative } from "node:path";
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
 * paragraph; `rounds` names the round, its head and session, the check, its words and the step
 * of the reading that lists it; `main-read` names the step and what it printed.
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

/** A check a job of an ordinary pull request proves at the round's head: CI's, not the round's. */
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
  /** A job or step a label, another event or the merge queue gates: no proof on an ordinary
   *  pull request. */
  readonly gated?: { readonly job: string; readonly gate: string }[];
  /** A job or step whose condition is false: it never runs, so it proves nothing. */
  readonly disabled?: { readonly job: string; readonly condition: string }[];
  /** Each result the run took whose actor does not carry `proven`. */
  readonly unproven?: string[];
  readonly issues?: string[];
  readonly steps?: CensusStep[];
  /** The one commit the reading of main read, and the merges its order step held it to. */
  readonly subject?: {
    readonly read: string;
    readonly merge: string;
    readonly after: number;
    readonly afterMerge?: string;
  };
  /** The files the reading of main wrote for its reader, and the ones the commit lacks. `tester`
   *  is the tester's parts as `Compose` returns them when a Go test runs it at the read commit,
   *  in that order, and empty where the run could not get them. */
  readonly files?: {
    readonly tester: string[];
    readonly written: string[];
    readonly missing: string[];
  };
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
export const DEFAULT_PATTERNS = {
  old: /records? (?:each|every) check once|(?:each|every) check is recorded once|recorded once,? at|tester (?:does not|doesn't|never|need not|will not) (?:run|re-?run|repeat)|repeats? no check|implementer records (?:each|every) check|no role (?:repeats|re-?runs)|(?:each|every) check (?:is )?(?:run|recorded) (?:only )?by the implementer|runs once at (?:that|the) head|(?:does not|doesn't|never) (?:run|re-?run) (?:it |a check |the row )?again/i,
  cirec:
    /record\w*[^.]{0,60}?from (?:the |a |CI's |its )?(?:CI|GitHub)(?:'s)? (?:run|job|check run)|record\w*[^.]{0,40}?(?:result|check)[^.]{0,40}?from (?:the |a )?CI run/i,
} as const;

/** Words that give a sentence's rule to checks in general: a quantifier on `check`, or `checks`.
 *  A sentence that holds one is a rule on every check, even where it also names one of its own
 *  checks as an example. */
const ON_EVERY_CHECK = /\b(?:any|each|every|all|no|other)\b[^.;:]{0,40}?\bchecks?\b|\bchecks\b/i;

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

function escaped(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** Whether a text names one of `ids` as a word of its own. */
function namesCheck(text: string, ids: ReadonlySet<string>): boolean {
  for (const id of ids) {
    if (new RegExp(`(^|[^\\w-])${escaped(id)}([^\\w-]|$)`).test(text)) return true;
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

export interface TextMatch {
  readonly rules: string[];
  readonly match: string;
  readonly paragraph: string;
}

/**
 * Each sentence of a text that holds the old rule: first the fixed strings p1 to p6, then the two
 * patterns over the sentences that hold none of them. A pattern hit whose sentence restricts its
 * rule to one of the document's own checks — it names the check and holds no words on checks in
 * general — is the owner's narrower rule for that check, and is not listed.
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
      if (namesCheck(sentence, ownChecks) && !ON_EVERY_CHECK.test(sentence)) continue;
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
  const named = new Set([id]);
  const flagged: string[] = [];
  let firstRowRuns: boolean | null = false;
  for (const line of markdown.split("\n")) {
    if (line.startsWith("| ---")) {
      firstRowRuns = null;
      continue;
    }
    if (line.startsWith("|") && firstRowRuns === null) firstRowRuns = ONCE.test(line);
    else if (!line.startsWith("|")) firstRowRuns = false;
    if (!namesCheck(line, named)) continue;
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

/** One status's listing: its unique issues, the total the listing reports, and how many rows
 *  repeated an issue already read. */
interface StatusListing {
  readonly keys: string[];
  readonly total: number;
  readonly repeated: number;
}

/**
 * Every issue of one status, page by page, with the total the listing reports and how many rows
 * repeated an issue already read. The walk ends at the total, at an empty page, or at a page that
 * adds no new issue, so a listing that repeats itself ends the walk short rather than forever; its
 * caller then holds the unique count against the total.
 */
async function issuesOfStatus(
  config: DispatchConfig,
  project: string,
  status: string,
  onePageOf?: number
): Promise<StatusListing> {
  const seen = new Set<string>();
  const limit = onePageOf ?? MAX_ISSUE_PAGE_LIMIT;
  let repeated = 0;
  let offset = 0;
  while (true) {
    const page = await get<IssuePage>(
      config,
      `/api/v1/issues?project=${encodeURIComponent(project)}&status=${encodeURIComponent(status)}` +
        `&limit=${limit}&offset=${offset}`
    );
    const rows = page.issues ?? [];
    const before = seen.size;
    for (const row of rows) {
      if (seen.has(row.key)) repeated += 1;
      else seen.add(row.key);
    }
    offset += rows.length;
    const added = seen.size - before;
    if (onePageOf !== undefined || added === 0 || seen.size >= page.total) {
      return { keys: [...seen], total: page.total, repeated };
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
 *  the document it belongs to. Any other edge names no document. */
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

/** The first source: each document the comment's Referenced by list names, by issue and slug. */
async function referencedDocuments(
  config: DispatchConfig,
  reference: string,
  unknown: CensusUnknown[]
): Promise<{ issue: string; slug: string }[]> {
  let edges: GraphEdge[];
  try {
    const references = await get<Pick<GraphReferences, "edges">>(
      config,
      `/api/v1/references?to=${encodeURIComponent(reference)}`
    );
    edges = references.edges ?? [];
  } catch (error) {
    unknown.push({ issue: "", what: reference, reason: why(error) });
    return [];
  }
  const documents = new Map<string, { issue: string; slug: string }>();
  for (const edge of edges) {
    const document = documentOfEdge(edge);
    if (document !== undefined) documents.set(`${document.issue}/${document.slug}`, document);
  }
  if (documents.size === 0) {
    unknown.push({
      issue: "",
      what: reference,
      reason: `its Referenced by list of ${edges.length} edge(s) named no document`,
    });
  }
  return [...documents.values()];
}

/** The second source: each issue of the project neither done nor in `retro`, every page of each
 *  status, with the unique issues held against each status's total. */
async function openIssues(
  config: DispatchConfig,
  options: TextsOptions,
  unknown: CensusUnknown[]
): Promise<string[]> {
  const keys: string[] = [];
  for (const status of OPEN_STATUSES) {
    const what = `the ${status} listing of ${options.project}`;
    let listing: StatusListing;
    try {
      listing = await issuesOfStatus(config, options.project, status, options.onePageOf);
    } catch (error) {
      unknown.push({ issue: "", what, reason: why(error) });
      continue;
    }
    if (listing.keys.length < listing.total) {
      const repeated =
        listing.repeated === 0 ? "" : `; ${listing.repeated} row(s) repeated an issue already read`;
      unknown.push({
        issue: "",
        what,
        reason: `it holds ${listing.keys.length} of ${listing.total} issues${repeated}`,
      });
    }
    keys.push(...listing.keys);
  }
  return keys;
}

async function runTexts(config: DispatchConfig, options: TextsOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const notRead: CensusNotRead[] = [];
  const patterns = options.breakPatterns ? { old: NEVER, cirec: NEVER } : DEFAULT_PATTERNS;
  const work = new Map<string, TextWork>();

  if (options.keysPath === undefined) {
    const reference = options.referencedBy;
    if (reference === undefined) {
      throw new Error("texts without --keys needs --referenced-by");
    }
    for (const document of await referencedDocuments(config, reference, unknown)) {
      wants(work, document.issue, document.slug);
      wants(work, document.issue, PLAN_SLUG);
    }
    for (const key of await openIssues(config, options, unknown)) {
      wants(work, key, VERIFICATION_SLUG);
    }
  } else {
    let listed: string[] = [];
    try {
      listed = (await readFile(options.keysPath, "utf8"))
        .split("\n")
        .map((line) => line.trim())
        .filter((line) => line !== "" && !line.startsWith("#"));
    } catch (error) {
      unknown.push({ issue: "", what: options.keysPath, reason: why(error) });
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

/**
 * The commands a shell script runs, one each: lines joined across a trailing backslash, then split
 * at each unquoted newline, `;`, `&&`, `||` and `|`, with any leading `NAME=value` assignments and
 * comments left out. A command quoted inside another, as `echo 'bun test x'` holds one, stays a
 * part of that other command.
 */
export function commandsOf(script: string): string[] {
  const joined = script.replace(/\\\r?\n/g, " ");
  const parts: string[] = [];
  let current = "";
  let quote: string | null = null;
  for (let index = 0; index < joined.length; index++) {
    const character = joined[index] as string;
    if (quote !== null) {
      current += character;
      if (character === quote) quote = null;
      continue;
    }
    if (character === "'" || character === '"') {
      quote = character;
      current += character;
      continue;
    }
    const pair = joined.slice(index, index + 2);
    if (pair === "&&" || pair === "||") {
      parts.push(current);
      current = "";
      index += 1;
      continue;
    }
    if (character === "\n" || character === ";" || character === "|") {
      parts.push(current);
      current = "";
      continue;
    }
    current += character;
  }
  parts.push(current);
  return parts
    .map((part) => command(part.replace(/^\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)+/, "")))
    .filter((part) => part !== "" && !part.startsWith("#"));
}

/** Whether a step's commands run a check's commands, in order: each as written, the last one
 *  with arguments of its own after it (a redirection, a reporter). */
function executesCommand(ran: readonly string[], wanted: readonly string[]): boolean {
  if (wanted.length === 0) return false;
  for (let start = 0; start + wanted.length <= ran.length; start++) {
    const matches = wanted.every((part, offset) => {
      const step = ran[start + offset] as string;
      return step === part || (offset === wanted.length - 1 && step.startsWith(`${part} `));
    });
    if (matches) return true;
  }
  return false;
}

/** What a job's or step's condition says of an ordinary pull request: it runs there, it never
 *  runs (`off`), it runs only on another event, a label or the merge queue (`gated`), or the
 *  census cannot tell (`unsure`). */
type Condition =
  | { readonly kind: "runs" }
  | { readonly kind: "off" | "gated" | "unsure"; readonly text: string };

const RUNS: Condition = { kind: "runs" };

/** The terms of a condition that hold on an ordinary pull request that is ready for review. */
const TRUE_ON_A_PULL_REQUEST: Record<string, true> = {
  true: true,
  "!github.event.pull_request.draft": true,
  "github.event_name == 'pull_request'": true,
  "github.event_name != 'merge_group'": true,
  "github.event_name != 'push'": true,
  "github.event_name != 'workflow_dispatch'": true,
  "success()": true,
  "!cancelled()": true,
  "always()": true,
};

/**
 * A job's or step's `if`, read as far as it can be read without a run: no condition, or a chain of
 * `&&` terms each in `TRUE_ON_A_PULL_REQUEST`, runs; `false` never runs; a term on another event
 * or on a label gates it; anything else — an `||`, a `needs` output, a path filter — is a question
 * for a reader, never proof.
 */
function conditionOf(value: unknown): Condition {
  if (value === undefined || value === null) return RUNS;
  if (typeof value === "boolean") return value ? RUNS : { kind: "off", text: "false" };
  if (typeof value !== "string") return { kind: "unsure", text: JSON.stringify(value) };
  const text = command(value);
  const bare = text.replace(/^\$\{\{\s*([\s\S]*?)\s*\}\}$/, "$1").trim();
  if (bare.includes("||")) return { kind: "unsure", text };
  let verdict: Condition = RUNS;
  for (const raw of bare.split("&&")) {
    const term = command(raw).replace(/"/g, "'");
    if (term === "false") return { kind: "off", text };
    if (TRUE_ON_A_PULL_REQUEST[term] === true) continue;
    const gates =
      /^github\.event_name == '[^']+'$/.test(term) ||
      /^contains\(github\.event\.pull_request\.labels\.\*\.name, '[^']+'\)$/.test(term);
    if (gates) verdict = { kind: "gated", text };
    else if (verdict.kind === "runs") verdict = { kind: "unsure", text };
  }
  return verdict;
}

const CONDITION_RANK: Record<Condition["kind"], number> = { runs: 0, unsure: 1, gated: 2, off: 3 };

/** Two conditions that both apply: the stronger bar of the two. */
function both(outer: Condition, inner: Condition): Condition {
  return CONDITION_RANK[inner.kind] > CONDITION_RANK[outer.kind] ? inner : outer;
}

/** A parsed workflow's trigger on an ordinary pull request, or nothing where it has none. A
 *  `pull_request` trigger that filters by path or branch runs only for some pull requests, which
 *  the census cannot tell, so it is a reader's question. */
function pullRequestTrigger(workflow: object): Condition | undefined {
  if (!("on" in workflow)) return undefined;
  const on = workflow.on;
  if (on === "pull_request") return RUNS;
  if (Array.isArray(on)) return on.includes("pull_request") ? RUNS : undefined;
  if (on === null || typeof on !== "object" || !("pull_request" in on)) return undefined;
  const trigger = on.pull_request;
  if (trigger === null || typeof trigger !== "object") return RUNS;
  for (const filter of ["paths", "paths-ignore", "branches", "branches-ignore"]) {
    if (filter in trigger) {
      return { kind: "unsure", text: `its pull_request trigger filters by ${filter}` };
    }
  }
  if ("types" in trigger && Array.isArray(trigger.types)) {
    const types: unknown[] = trigger.types;
    if (!types.includes("opened") && !types.includes("synchronize")) {
      return { kind: "gated", text: `it runs on pull request events ${types.join(", ")}` };
    }
  }
  return RUNS;
}

/** A step of a workflow job that runs a shell script. */
interface WorkflowStep {
  readonly condition: unknown;
  readonly run: string;
}

/** A job of a workflow, as the reading of a parsed workflow narrows it: a workflow file is
 *  whatever the checkout holds, so each member is read and checked rather than assumed. */
interface WorkflowJob {
  readonly condition: unknown;
  readonly name?: string;
  readonly uses?: string;
  /** Where the inputs a called workflow is given name a label or the merge queue. */
  readonly gate?: string;
  readonly steps: WorkflowStep[];
}

function jobOf(value: unknown): WorkflowJob {
  if (value === null || typeof value !== "object") return { condition: undefined, steps: [] };
  const inputs = "with" in value ? JSON.stringify(value.with) : "";
  const name = "name" in value && typeof value.name === "string" ? value.name : undefined;
  const uses = "uses" in value && typeof value.uses === "string" ? value.uses : undefined;
  const steps: WorkflowStep[] = [];
  const listed = "steps" in value && Array.isArray(value.steps) ? value.steps : [];
  for (const step of listed) {
    if (step === null || typeof step !== "object") continue;
    if (!("run" in step) || typeof step.run !== "string") continue;
    steps.push({ condition: "if" in step ? step.if : undefined, run: step.run });
  }
  return {
    condition: "if" in value ? value.if : undefined,
    steps,
    ...(name === undefined ? {} : { name }),
    ...(uses === undefined ? {} : { uses }),
    ...(/full-ci|merge_group/.test(inputs) ? { gate: command(inputs) } : {}),
  };
}

/** The jobs of a parsed workflow, by their ids. */
function jobsOf(workflow: object): Map<string, WorkflowJob> {
  if (!("jobs" in workflow)) return new Map();
  const jobs = workflow.jobs;
  if (jobs === null || typeof jobs !== "object") return new Map();
  return new Map(Object.entries(jobs).map(([id, job]) => [id, jobOf(job)]));
}

/** A workflow file as a YAML object, or the reason it is none. */
function parseWorkflow(path: string, body: string): object | string {
  let parsed: unknown;
  try {
    parsed = Bun.YAML.parse(body);
  } catch (error) {
    return `${path} does not parse as YAML: ${why(error)}`;
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return `${path} is no workflow: its YAML is no mapping`;
  }
  return parsed;
}

/** One step of an ordinary pull request's job that runs at the head: the commands it runs, the
 *  names a `ci_check` may give its job, and the condition the census could not read, if any. */
interface Execution {
  readonly file: string;
  readonly job: string;
  readonly names: readonly string[];
  readonly script: string;
  readonly commands: readonly string[];
  readonly unsure?: string;
}

/** What the workflows at one head say, and what of them the census could not read. */
interface WorkflowReads {
  readonly executions: Execution[];
  /** The lines the reading's search for `dispatch result` found, with the two after each. */
  readonly hits: { file: string; line: number; text: string; first: string }[];
  readonly gated: { job: string; gate: string }[];
  readonly disabled: { job: string; condition: string }[];
  readonly failures: string[];
}

function readWorkflows(repo: string, head: string): WorkflowReads {
  const reads: WorkflowReads = { executions: [], hits: [], gated: [], disabled: [], failures: [] };
  const resolved = git(repo, ["rev-parse", "--verify", "--quiet", `${head}^{commit}`]);
  if (resolved.exitCode !== 0) {
    reads.failures.push(`the checkout holds no commit ${head}`);
    return reads;
  }
  const bodies = new Map<string, string>();
  const bodyOf = (path: string): string | undefined => {
    const held = bodies.get(path);
    if (held !== undefined) return held;
    const shown = git(repo, ["show", `${head}:${path}`]);
    if (shown.exitCode !== 0) {
      reads.failures.push(
        `git show ${head}:${path} answered ${shown.exitCode}: ${command(shown.stderr)}`
      );
      return undefined;
    }
    bodies.set(path, shown.stdout);
    return shown.stdout;
  };

  // The search the reading names, for the steps that record a result themselves.
  const search = git(repo, ["grep", "-n", "-E", "dispatch result( |$)", head, "--", ".github/"]);
  if (search.exitCode > 1) {
    reads.failures.push(
      `git grep for dispatch result answered ${search.exitCode}: ${command(search.stderr)}`
    );
  }
  for (const line of search.stdout.split("\n")) {
    const matched = line.match(/^[^:]*:([^:]+):(\d+):(.*)$/);
    if (matched === null) continue;
    const [, file = "", number = "0", first = ""] = matched;
    const lines = (bodyOf(file) ?? first).split("\n");
    const at = Number(number) - 1;
    reads.hits.push({
      file,
      line: Number(number),
      first,
      text: lines.slice(at, at + 3).join("\n"),
    });
  }

  const walk = (
    file: string,
    jobs: Map<string, WorkflowJob>,
    above: Condition,
    caller?: { readonly id: string; readonly name?: string }
  ): void => {
    for (const [id, job] of jobs) {
      const label = `${file} ${id}`;
      let condition = both(above, conditionOf(job.condition));
      if (job.gate !== undefined) condition = both(condition, { kind: "gated", text: job.gate });
      if (condition.kind === "off") {
        reads.disabled.push({ job: label, condition: condition.text });
        continue;
      }
      if (condition.kind === "gated") {
        reads.gated.push({ job: label, gate: condition.text });
        continue;
      }
      const own = [id, ...(job.name === undefined ? [] : [job.name])];
      const callers =
        caller === undefined
          ? []
          : [caller.id, ...(caller.name === undefined ? [] : [caller.name])];
      const names =
        callers.length === 0
          ? own
          : [...own, ...callers.flatMap((outer) => own.map((inner) => `${outer} / ${inner}`))];
      for (const step of job.steps) {
        const stepCondition = both(condition, conditionOf(step.condition));
        if (stepCondition.kind === "off") {
          reads.disabled.push({ job: `${label} step`, condition: stepCondition.text });
          continue;
        }
        if (stepCondition.kind === "gated") {
          reads.gated.push({ job: `${label} step`, gate: stepCondition.text });
          continue;
        }
        reads.executions.push({
          file,
          job: label,
          names,
          script: step.run,
          commands: commandsOf(step.run),
          ...(stepCondition.kind === "unsure" ? { unsure: stepCondition.text } : {}),
        });
      }
      const called = job.uses?.match(/^\.\/(\.github\/workflows\/[^@]+)$/)?.[1];
      if (called === undefined) continue;
      const body = bodyOf(called);
      if (body === undefined) continue;
      const parsed = parseWorkflow(called, body);
      if (typeof parsed === "string") {
        reads.failures.push(parsed);
        continue;
      }
      walk(called, jobsOf(parsed), condition, {
        id,
        ...(job.name === undefined ? {} : { name: job.name }),
      });
    }
  };

  const listed = git(repo, ["ls-tree", "-r", "--name-only", head, "--", ".github/workflows"]);
  if (listed.exitCode !== 0) {
    reads.failures.push(
      `git ls-tree of .github/workflows answered ${listed.exitCode}: ${command(listed.stderr)}`
    );
    return reads;
  }
  for (const path of listed.stdout.split("\n").filter((line) => /\.ya?ml$/.test(line))) {
    const body = bodyOf(path);
    if (body === undefined) continue;
    const parsed = parseWorkflow(path, body);
    if (typeof parsed === "string") {
      reads.failures.push(parsed);
      continue;
    }
    const trigger = pullRequestTrigger(parsed);
    if (trigger === undefined) continue;
    if (trigger.kind === "gated" || trigger.kind === "off") {
      reads.gated.push({ job: path, gate: trigger.text });
      continue;
    }
    walk(path, jobsOf(parsed), trigger);
  }
  return reads;
}

/** How a workflow step records a check itself: a step that runs at the head and whose command
 *  names the check, as the reading's search finds it. A search line in a step that never runs
 *  records nothing there; one outside a workflow file, or in a step under a condition the census
 *  cannot read, is a question for a reader. */
function recorderOf(
  reads: WorkflowReads,
  id: string
): { kind: "none" } | { kind: "records"; detail: string } | { kind: "unsure"; reason: string } {
  const named = new Set([id]);
  for (const hit of reads.hits) {
    if (!namesCheck(hit.text, named)) continue;
    if (!/^\.github\/workflows\/[^/]+\.ya?ml$/.test(hit.file)) {
      return {
        kind: "unsure",
        reason: `the search found dispatch result naming it in ${hit.file}, which is no workflow step`,
      };
    }
    const step = reads.executions.find(
      (execution) => execution.file === hit.file && execution.script.includes(hit.first.trim())
    );
    if (step === undefined) continue;
    if (step.unsure !== undefined) {
      return {
        kind: "unsure",
        reason: `the step that records it runs under a condition the census cannot read: ${step.unsure}`,
      };
    }
    return { kind: "records", detail: `${hit.file}:${hit.line}: ${command(hit.text)}` };
  }
  return { kind: "none" };
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

/** Whether a result was recorded during the round: after the move that started it, by the
 *  handoff that ended it. */
function duringRound(result: Pick<CheckResult, "created_at">, round: Round): boolean {
  const at = time(result.created_at);
  return at > round.from && at <= round.to;
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

/** The rounds of one issue since `--since`: one for each `test` handoff, its head and session from
 *  the handoff, its start the last move into testing before the handoff. */
async function roundsOf(
  config: DispatchConfig,
  key: string,
  moves: readonly number[],
  since: number,
  unknown: CensusUnknown[]
): Promise<Round[]> {
  let handoffs: HandoffSummary[];
  try {
    handoffs = await get<HandoffSummary[]>(
      config,
      `/api/v1/issues/${encodeURIComponent(key)}/handoffs`
    );
  } catch (error) {
    unknown.push({ issue: key, what: "the handoffs", reason: why(error) });
    return [];
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
  }
  const rounds: Round[] = [];
  for (const summary of tests) {
    const what = `test handoff ${summary.version}`;
    let handoff: Handoff;
    try {
      handoff = await get<Handoff>(config, `/api/v1/handoffs/${encodeURIComponent(summary.id)}`);
    } catch (error) {
      unknown.push({ issue: key, what, reason: why(error) });
      continue;
    }
    let head: unknown;
    try {
      const body: unknown = JSON.parse(handoff.body);
      head = body !== null && typeof body === "object" && "head" in body ? body.head : undefined;
    } catch (error) {
      unknown.push({ issue: key, what, reason: `its body is no JSON object: ${why(error)}` });
      continue;
    }
    if (typeof head !== "string" || head === "") {
      unknown.push({ issue: key, what, reason: "its body names no head" });
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
  return rounds;
}

async function runRounds(config: DispatchConfig, options: RoundsOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const excluded: CensusExcluded[] = [];
  const ci: CensusCICheck[] = [];
  const gated: { job: string; gate: string }[] = [];
  const disabled: { job: string; condition: string }[] = [];
  const unproven: string[] = [];
  const listed: string[] = [];
  const since = time(options.since);
  const source = `the issues of ${options.project} updated since ${options.since}`;
  let roundsRead = 0;

  let updated: IssueKeyRow[] | undefined;
  try {
    updated = await get<IssueKeyRow[]>(
      config,
      `/api/v1/issues?project=${encodeURIComponent(options.project)}` +
        `&updated_since=${encodeURIComponent(options.since)}`
    );
  } catch (error) {
    unknown.push({ issue: "", what: source, reason: why(error) });
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

    const rounds = await roundsOf(config, key, moves, since, unknown);
    let results: IssueCheckResults;
    let plan = "";
    try {
      results = await get<IssueCheckResults>(
        config,
        `/api/v1/issues/${encodeURIComponent(key)}/results`
      );
      const issue = await issueRead(config, key);
      const named = documentNamed(issue, PLAN_SLUG);
      if (named !== undefined) plan = (await documentText(config, key, named.slug)).markdown;
    } catch (error) {
      unknown.push({ issue: key, what: "the results and the plan", reason: why(error) });
      return;
    }

    for (const round of rounds) {
      roundsRead += 1;
      const workflows = readWorkflows(options.repo, round.head);
      for (const failure of workflows.failures) {
        unknown.push({ issue: key, what: `the workflows at ${round.head}`, reason: failure });
      }
      gated.push(...workflows.gated);
      disabled.push(...workflows.disabled);
      for (const check of results.checks ?? []) {
        const verdict = await judgeCheck({ config, issue: key, round, check, plan, workflows });
        if (verdict.excluded !== undefined) excluded.push(verdict.excluded);
        if (verdict.ci !== undefined) ci.push(verdict.ci);
        if (verdict.unproven !== undefined) unproven.push(verdict.unproven);
        rows.push(...verdict.rows);
        unknown.push(...verdict.unknown);
      }
    }
  };

  await eachIssue(
    (updated ?? []).map((row) => row.key),
    readIssue
  );

  // A population the run enumerated nothing of is no clean reading.
  if (updated !== undefined && roundsRead === 0) {
    unknown.push({
      issue: "",
      what: "the testing rounds",
      reason:
        listed.length === 0
          ? `none of the ${updated.length} issue(s) of ${source} moved into testing after ${options.since}, so no round was read`
          : "no round of the issues that moved into testing could be read",
    });
  }

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
    disabled,
    unproven,
    issues: listed,
  };
}

/** What one check of one round is: left out, CI's, listed, or a run the reading could read. A
 *  check can be listed by step 2 and still be read by step 3, so its rows and its unknowns are
 *  lists. */
interface CheckVerdict {
  excluded?: CensusExcluded;
  ci?: CensusCICheck;
  unproven?: string;
  readonly rows: CensusRow[];
  readonly unknown: CensusUnknown[];
}

/**
 * One `before_merge` check of one round, by the reading's steps. A check a workflow step records
 * is left out of both. Step 2 reads the check against the ordinary pull request's jobs at the
 * head: a check with no `ci_check` whose command such a job runs is listed, and so is a `ci_check`
 * whose named job does not run its command, and a result a role recorded in the round on a check
 * that carries `ci_check` now. A check such a job proves is CI's, and step 3 reads no run of it.
 * Step 3 then looks for the round session's own run at the head, unless the check's words or the
 * issue's plan give its run to another runner or one run at a head.
 */
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
  const what = `${id} at ${round.head}`;
  const verdict: CheckVerdict = { rows: [], unknown: [] };
  if (check.when !== "before_merge") {
    verdict.excluded = { ...place, reason: `its when is ${check.when}` };
    return verdict;
  }
  const recorder = recorderOf(workflows, id);
  if (recorder.kind === "unsure") {
    verdict.unknown.push({ issue, what, reason: recorder.reason });
    return verdict;
  }
  if (recorder.kind === "records") {
    verdict.excluded = { ...place, reason: "a workflow step records it", detail: recorder.detail };
    return verdict;
  }

  const row = { ...place, head: round.head, session: round.session, words: flat(check.text) };
  const wanted = commandsOf(check.run);
  const running = workflows.executions.filter((execution) =>
    executesCommand(execution.commands, wanted)
  );
  const sure = running.filter((execution) => execution.unsure === undefined);
  const unsure = running.filter((execution) => execution.unsure !== undefined);
  const attribute = check.ci_check ?? "";

  // Step 2: the check against the ordinary pull request's jobs at the head.
  let proved: Execution | undefined;
  if (attribute === "") {
    proved = sure[0];
    if (proved !== undefined) {
      verdict.rows.push({
        ...row,
        step: "v-open-work step 2",
        reason: "a job of an ordinary pull request runs its command, and it carries no ci_check",
        detail: sure.map((execution) => execution.job).join(", "),
      });
    } else if (unsure[0] !== undefined) {
      verdict.unknown.push({
        issue,
        what,
        reason: `a job runs its command under a condition the census cannot read: ${unsure[0].unsure}`,
      });
      return verdict;
    }
  } else {
    proved = sure.find((execution) => execution.names.includes(attribute));
    const named = unsure.find((execution) => execution.names.includes(attribute));
    if (proved === undefined && named !== undefined) {
      verdict.unknown.push({
        issue,
        what,
        reason: `the job its ci_check names runs under a condition the census cannot read: ${named.unsure}`,
      });
      return verdict;
    }
    if (proved === undefined) {
      verdict.rows.push({
        ...row,
        step: "v-open-work step 2",
        reason: `its ci_check names ${attribute}, and no job of that name runs its command on an ordinary pull request`,
      });
    }
    const recorded = (check.results ?? []).filter((result) => duringRound(result, round));
    if (recorded.length > 0) {
      verdict.rows.push({
        ...row,
        step: "v-open-work step 2",
        reason:
          "a role recorded a result on it during the round, so it carried no ci_check when that result was written",
        detail: recorded.map((result) => result.id).join(", "),
      });
    }
  }
  if (proved !== undefined) {
    verdict.ci = { ...place, job: proved.job, command: command(check.run) };
    return verdict;
  }

  // Step 3: the round session's own run at the head.
  const words = `${check.run}\n${check.text}`;
  if (!TESTER.test(words) && RUNNER.test(words)) {
    verdict.excluded = { ...place, reason: "its words give its run only to another runner" };
    return verdict;
  }
  const oneRun = oneRunLines(plan, id);
  if (oneRun.length > 0) {
    verdict.excluded = {
      ...place,
      reason: "its plan gives it one run at a head",
      detail: oneRun.join(" "),
    };
    return verdict;
  }
  const taken = (check.results ?? [])
    .filter(
      (result) =>
        result.session === round.session &&
        result.commit_sha === round.head &&
        duringRound(result, round)
    )
    .sort((left, right) => time(left.created_at) - time(right.created_at))
    .at(-1);
  const audit = { ...row, step: "v-open-work step 3" };
  if (taken === undefined) {
    verdict.rows.push({ ...audit, reason: "no result of the round's session at the head" });
    return verdict;
  }
  if (taken.actor.proven !== true) verdict.unproven = taken.id;
  const carried = CARRIED.exec(`${taken.run} ${taken.observed}`);
  if (carried !== null) {
    verdict.rows.push({ ...audit, reason: `its reading carries an earlier run: ${carried[0]}` });
    return verdict;
  }
  const links = linksOf(taken);
  if (links.length === 0) {
    verdict.unknown.push({
      issue,
      what,
      reason: "its result links no output, so the reading cannot see a run",
    });
    return verdict;
  }
  const outputs: string[] = [];
  for (const link of links) {
    try {
      outputs.push(await readLink(config, link));
    } catch (error) {
      verdict.unknown.push({ issue, what, reason: `its output did not load: ${why(error)}` });
      return verdict;
    }
  }
  const atHead = outputs.filter((output) => namesHead(output, round.head));
  if (atHead.length === 0) {
    verdict.rows.push({ ...audit, reason: "its output shows no run at the head" });
    return verdict;
  }
  // The output must show this check's own procedure at the head, not only the head.
  const procedure = command(check.run);
  if (procedure === "" || !atHead.some((output) => command(output).includes(procedure))) {
    verdict.unknown.push({
      issue,
      what,
      reason:
        "its output does not show the check's procedure, so the reading cannot tie the run to this check",
    });
  }
  return verdict;
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
const PROMPTS_GO = `${PROMPTS}/prompts.go`;

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

/** The files the reader of the other steps reads beside the tester's parts. A base that lacks one
 *  names it as absent, and that part of the reading is not run there. */
const OTHER_READING_FILES = [
  PROMPTS_GO,
  ...TESTER_FILES,
  ...READER_FILES,
  WORKER_COMMON,
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

/** The role package whose `RoleTester` the census gives `Compose`. */
const CLAIM = "packages/daemon/internal/claim";

/** The Go test the census adds to the prompts package of its own export of the read commit. */
const PARTS_TEST = "TestCheckRunsCensusTesterParts";
const PARTS_TEST_FILE = "check_runs_census_parts_test.go";

/** The marks of the lines that test prints: one per part `Compose` returned, one for a path
 *  outside the Composer's two directories, and the count of the parts. */
const PART_LINE = "check-runs-census part ";
const OUTSIDE_LINE = "check-runs-census outside ";
const COUNT_LINE = /^check-runs-census parts (\d+)$/;

/** One part of the tester's prompt: its file in the repository, and whether the prompts package
 *  embeds that file, which is the file `New` copies to the path `Compose` returns. */
interface TesterPart {
  readonly path: string;
  readonly embedded: boolean;
}

/**
 * The Go test the census runs at the read commit. It calls `New` and `Compose` for the tester, as
 * the daemon does, and prints each path `Compose` returns as the embedded folder `New` copies to
 * that directory (`roles` to the shared one, `go` to its own) and the name below it, with whether
 * that folder embeds the file. A path outside both directories, or a copy that is not the
 * embedded file, fails the test: the census cannot then tell which file of the repository the
 * tester reads.
 */
function partsTestSource(claimImport: string): string {
  return `package prompts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"${claimImport}"
)

// ${PARTS_TEST} is no test of this package: the check runs census writes it into its own
// export of one commit, runs it there, and removes the export.
func ${PARTS_TEST}(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parts, err := composer.Compose(claim.RoleTester, false)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	for _, path := range parts.RolePromptPaths {
		root, files := "roles", roleParts
		name, below := checkRunsCensusBelow(composer.sharedDir, path)
		if !below {
			root, files = "go", goParts
			name, below = checkRunsCensusBelow(composer.goDir, path)
		}
		if !below {
			line, _ := json.Marshal(path)
			fmt.Printf("${OUTSIDE_LINE}%s\\n", line)
			t.Fatalf("Compose returned %s, in neither of the Composer's two directories", path)
		}
		embedded, missing := fs.ReadFile(files, root+"/"+name)
		if missing == nil {
			copied, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(copied, embedded) {
				t.Fatalf("%s is not the embedded %s/%s that New copies there: %v", path, root, name, err)
			}
		}
		line, _ := json.Marshal(struct {
			Root, Name string
			Embedded   bool
		}{root, name, missing == nil})
		fmt.Printf("${PART_LINE}%s\\n", line)
	}
	fmt.Printf("check-runs-census parts %d\\n", len(parts.RolePromptPaths))
}

func checkRunsCensusBelow(dir, path string) (string, bool) {
	name, err := filepath.Rel(dir, path)
	if err != nil || name == "." || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(name), true
}
`;
}

/** Writes the read commit's tree into `into` from the repository's objects alone, so nothing of
 *  the checkout's own files reaches it, or says why it could not. */
async function exportCommit(repo: string, read: string, into: string): Promise<string | undefined> {
  const archive = Bun.spawn(["git", "-C", repo, "archive", "--format=tar", read], {
    stdout: "pipe",
    stderr: "pipe",
  });
  const tar = Bun.spawn(["tar", "-x", "-C", into], {
    stdin: archive.stdout,
    stdout: "ignore",
    stderr: "pipe",
  });
  const [archived, extracted, archiveError, tarError] = await Promise.all([
    archive.exited,
    tar.exited,
    new Response(archive.stderr).text(),
    new Response(tar.stderr).text(),
  ]);
  if (archived !== 0) {
    return `git archive ${read} answered ${archived}: ${command(archiveError).slice(0, 300)}`;
  }
  if (extracted !== 0) return `tar answered ${extracted}: ${command(tarError).slice(0, 300)}`;
  return undefined;
}

/** The import path of the role package in the export: the module the nearest `go.mod` above it
 *  names, and the package's place below that file. */
async function claimImportOf(root: string): Promise<string | undefined> {
  for (let at = dirname(CLAIM); ; at = dirname(at)) {
    const text = await readFile(join(root, at, "go.mod"), "utf8").catch(() => undefined);
    if (text !== undefined) {
      const module = text.match(/^module\s+"?([^\s"]+)"?\s*$/m)?.[1];
      return module === undefined ? undefined : `${module}/${relative(at, CLAIM)}`;
    }
    if (at === ".") return undefined;
  }
}

/** A part line's JSON, or nothing where it is not one part of the two folders. */
function partOf(text: string): TesterPart | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (parsed === null || typeof parsed !== "object") return undefined;
  const root = "Root" in parsed ? parsed.Root : undefined;
  const name = "Name" in parsed ? parsed.Name : undefined;
  const embedded = "Embedded" in parsed ? parsed.Embedded : undefined;
  if (root !== "roles" && root !== "go") return undefined;
  if (typeof name !== "string" || name === "" || typeof embedded !== "boolean") return undefined;
  return { path: `${PROMPTS}/${root}/${name}`, embedded };
}

/** The parts the Go test printed, or why its output gives none: a list is taken only from a run
 *  that passed its one test and printed as many part lines as its count. */
function partsOf(output: string, exitCode: number | null): TesterPart[] | string {
  const lines = output.split("\n");
  const outside = lines.find((line) => line.startsWith(OUTSIDE_LINE));
  if (outside !== undefined) {
    return `Compose returned a path outside the Composer's two directories: ${outside.slice(OUTSIDE_LINE.length)}`;
  }
  if (output.includes("--- SKIP") || output.includes("no tests to run")) {
    return "the go test selected no test, or skipped it";
  }
  if (exitCode !== 0) return `go test answered ${exitCode}: ${command(output).slice(-600)}`;
  if (!output.includes(`--- PASS: ${PARTS_TEST} `)) {
    return "the go test printed no pass of its test";
  }
  const parts: TesterPart[] = [];
  for (const line of lines) {
    if (!line.startsWith(PART_LINE)) continue;
    const part = partOf(line.slice(PART_LINE.length));
    if (part === undefined) return `the go test printed a part the census cannot read: ${line}`;
    parts.push(part);
  }
  const count = lines.map((line) => line.match(COUNT_LINE)?.[1]).find((n) => n !== undefined);
  if (count === undefined || Number(count) !== parts.length) {
    return `the go test printed ${parts.length} part line(s) and the count ${count ?? "nowhere"}`;
  }
  if (parts.length === 0) return "Compose returned no part for the tester";
  return parts;
}

async function testerPartsIn(
  repo: string,
  read: string,
  scratch: string
): Promise<TesterPart[] | string> {
  if (!holds(repo, read, PROMPTS)) return `${read} lacks ${PROMPTS}`;
  const exported = await exportCommit(repo, read, scratch);
  if (exported !== undefined) return exported;
  const claimImport = await claimImportOf(scratch);
  if (claimImport === undefined) return `${read} holds no go.mod with a module above ${CLAIM}`;
  // `wx`: a file of that name at the commit is the commit's own, and the census writes none over it.
  await writeFile(join(scratch, PROMPTS, PARTS_TEST_FILE), partsTestSource(claimImport), {
    flag: "wx",
  });
  const workspace = join(scratch, "go.work");
  const run = Bun.spawnSync(
    [
      "go",
      "-C",
      join(scratch, PROMPTS),
      "test",
      "-count=1",
      "-v",
      "-vet=off",
      "-run",
      `^${PARTS_TEST}$`,
      ".",
    ],
    {
      stdout: "pipe",
      stderr: "pipe",
      // The export's own workspace, or none: never a go.work of a folder above the export.
      env: { ...process.env, GOWORK: (await Bun.file(workspace).exists()) ? workspace : "off" },
    }
  );
  return partsOf(`${run.stdout.toString()}${run.stderr.toString()}`, run.exitCode);
}

/**
 * The tester's parts as `Compose` returns them at the read commit: the commit exported into a
 * folder of the census's own, a Go test added to its prompts package that calls `New` and
 * `Compose(claim.RoleTester, false)`, and its printed paths taken back to their files. A run that
 * cannot get the list — no `go`, a package that does not compile there, a `Compose` that errs —
 * returns the reason, which the reading reports UNKNOWN, never a list of its own.
 */
async function testerParts(repo: string, read: string): Promise<TesterPart[] | string> {
  const scratch = await mkdtemp(join(tmpdir(), "check-runs-census-"));
  try {
    return await testerPartsIn(repo, read, scratch);
  } catch (error) {
    return why(error);
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
}

interface StepRun {
  readonly step: CensusStep;
  readonly row?: CensusRow;
  readonly unknown?: CensusUnknown;
}

function failed(name: string, reason: string, detail?: string): StepRun {
  const extra = detail === undefined ? {} : { detail };
  return {
    step: { name, verdict: "fail", reason, ...extra },
    row: { issue: "", step: name, reason, ...extra },
  };
}

function unreadable(name: string, reason: string): StepRun {
  return {
    step: { name, verdict: "not run", reason },
    unknown: { issue: "", what: name, reason },
  };
}

/** The order step: the merge commit of the pull request `--after` names must be an ancestor of
 *  `--merge`. A pull request with no merge commit fails it; a `gh` that cannot answer is UNKNOWN. */
function orderStep(options: ReadingOptions): { run: StepRun; afterMerge?: string } {
  const view = Bun.spawnSync(["gh", "pr", "view", String(options.after), "--json", "mergeCommit"], {
    cwd: options.repo,
    stdout: "pipe",
    stderr: "pipe",
  });
  if (view.exitCode !== 0) {
    return {
      run: unreadable(
        "order",
        `gh pr view ${options.after} answered ${view.exitCode}: ${command(view.stderr.toString()).slice(0, 200)}`
      ),
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
    return {
      run: unreadable("order", `gh pr view ${options.after} answered no JSON: ${why(error)}`),
    };
  }
  if (typeof oid !== "string" || oid === "") {
    return {
      run: failed(
        "order",
        `pull request ${options.after} has no merge commit, so the order step fails`
      ),
    };
  }
  const ancestor = git(options.repo, ["merge-base", "--is-ancestor", oid, options.merge]);
  if (ancestor.exitCode === 0) {
    return { run: { step: { name: "order", verdict: "pass", detail: oid } }, afterMerge: oid };
  }
  if (ancestor.exitCode === 1) {
    return {
      run: failed(
        "order",
        `the merge commit of pull request ${options.after} (${oid}) is no ancestor of the merge read (${options.merge})`
      ),
      afterMerge: oid,
    };
  }
  return {
    run: unreadable(
      "order",
      `git merge-base --is-ancestor answered ${ancestor.exitCode}: ${command(ancestor.stderr)}`
    ),
    afterMerge: oid,
  };
}

function holdsMergeStep(options: ReadingOptions, read: string): StepRun {
  const name = "the read commit holds the merge";
  const held = git(options.repo, ["merge-base", "--is-ancestor", options.merge, read]);
  if (held.exitCode === 0) return { step: { name, verdict: "pass" } };
  if (held.exitCode === 1) {
    return failed(name, `the read commit ${read} does not hold the merge ${options.merge}`);
  }
  return unreadable(
    name,
    `git merge-base --is-ancestor answered ${held.exitCode}: ${command(held.stderr)}`
  );
}

function holds(repo: string, read: string, path: string): boolean {
  return git(repo, ["cat-file", "-e", `${read}:${path}`]).exitCode === 0;
}

function grepStep(repo: string, read: string, step: GrepStep): StepRun {
  // A grep over a file the commit lacks prints nothing, which would read as a pass.
  const absent = step.files.filter((path) => !holds(repo, read, path));
  if (absent.length > 0) {
    return unreadable(
      step.name,
      `the read commit lacks ${absent.join(", ")}, so the grep cannot read it`
    );
  }
  const run = git(repo, ["grep", "-n", "-E", step.pattern, read, "--", ...step.files]);
  if (run.exitCode > 1) {
    return unreadable(
      step.name,
      `git grep answered ${run.exitCode}: ${command(run.stderr).slice(0, 300)}`
    );
  }
  const printed = run.stdout.split("\n").filter((line) => line !== "");
  if (step.wants === "nothing") {
    if (printed.length === 0) return { step: { name: step.name, verdict: "pass" } };
    return failed(
      step.name,
      `it printed ${printed.length} line(s) and must print none`,
      printed.join("\n")
    );
  }
  if (printed.length > 0) return { step: { name: step.name, verdict: "pass" } };
  return failed(step.name, "it printed nothing and must print at least one line");
}

function pinTestStep(repo: string, read: string, name: string, coherent: boolean): StepRun {
  const held = git(repo, ["grep", "-l", "-E", `^func ${name}\\(`, read, "--", "packages/daemon"]);
  if (held.exitCode > 1) {
    return unreadable(name, `git grep answered ${held.exitCode}: ${command(held.stderr)}`);
  }
  const file = held.stdout
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => line.slice(line.indexOf(":") + 1))[0];
  if (file === undefined) {
    return {
      step: {
        name,
        verdict: "not run",
        reason: `no package under packages/daemon holds it at ${read}`,
      },
    };
  }
  if (!coherent) {
    return {
      step: {
        name,
        verdict: "not run",
        reason: "the checkout's tracked files differ from the read commit, so no test runs on it",
      },
    };
  }
  const relative = `./${dirname(file).replace(/^packages\/daemon\//, "")}/`;
  let output: string;
  let exitCode: number | null;
  try {
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
    output = `${run.stdout.toString()}${run.stderr.toString()}`;
    exitCode = run.exitCode;
  } catch (error) {
    // A machine with no `go` cannot run the test, which is UNKNOWN, never a failed step.
    return unreadable(name, `go did not start: ${why(error)}`);
  }
  // A run that selected no test, or that skipped, proves nothing and fails its step.
  if (output.includes("--- SKIP") || output.includes("no tests to run")) {
    return failed(name, "the run selected no test, or skipped it", output.slice(-2000));
  }
  if (exitCode === 0) return { step: { name, verdict: "pass", detail: relative } };
  return failed(name, `go test answered ${exitCode} in ${relative}`, output.slice(-2000));
}

async function runMainRead(options: ReadingOptions): Promise<CensusReport> {
  const rows: CensusRow[] = [];
  const unknown: CensusUnknown[] = [];
  const steps: CensusStep[] = [];
  const record = (run: StepRun): void => {
    steps.push(run.step);
    if (run.row !== undefined) rows.push(run.row);
    if (run.unknown !== undefined) unknown.push(run.unknown);
  };

  // The one commit every step of the reading reads.
  const head = git(options.repo, ["rev-parse", "--verify", "HEAD^{commit}"]);
  if (head.exitCode !== 0) {
    unknown.push({
      issue: "",
      what: options.repo,
      reason: `it is no git checkout with a commit: ${command(head.stderr)}`,
    });
    return { mode: "main-read", complete: false, rows, unknown, steps };
  }
  const read = head.stdout.trim();
  const status = git(options.repo, ["status", "--porcelain", "--untracked-files=no"]);
  const coherent = status.exitCode === 0 && status.stdout.trim() === "";
  if (!coherent) {
    unknown.push({
      issue: "",
      what: "the checkout",
      reason: `its tracked files differ from ${read}: ${command(status.stdout || status.stderr).slice(0, 300)}`,
    });
  }

  const order = orderStep(options);
  record(order.run);
  record(holdsMergeStep(options, read));
  for (const step of GREPS) record(grepStep(options.repo, read, step));
  for (const name of PIN_TESTS) record(pinTestStep(options.repo, read, name, coherent));

  // The tester's parts as `Compose` returns them at the read commit, and the other files.
  const composed = await testerParts(options.repo, read);
  const tester = typeof composed === "string" ? [] : composed.map((part) => part.path);
  if (typeof composed === "string") {
    unknown.push({
      issue: "",
      what: "the tester's parts",
      reason: `cannot tell the tester's parts: ${composed}`,
    });
  } else {
    for (const part of composed) {
      // A part the commit lacks is the loop's below; one it holds but does not embed is this.
      if (!part.embedded && holds(options.repo, read, part.path)) {
        unknown.push({
          issue: "",
          what: part.path,
          reason: `Compose names it, and the prompts package does not embed it at ${read}`,
        });
      }
    }
  }
  const written: string[] = [];
  const missing: string[] = [];
  for (const path of [...new Set([...tester, ...OTHER_READING_FILES])]) {
    const shown = git(options.repo, ["show", `${read}:${path}`]);
    if (shown.exitCode !== 0) {
      missing.push(path);
      if (tester.includes(path) || path === PROMPTS_GO) {
        unknown.push({ issue: "", what: path, reason: `Compose names it, and ${read} lacks it` });
      }
      continue;
    }
    const target = join(options.out, "files", path);
    await mkdir(dirname(target), { recursive: true });
    await writeFile(target, shown.stdout);
    written.push(path);
  }
  await writeFile(
    join(options.out, "files.txt"),
    [
      `read ${read}`,
      ...written.map((path) => `written ${path}`),
      ...missing.map((path) => `absent ${path}`),
    ]
      .map((line) => `${line}\n`)
      .join("")
  );

  return {
    mode: "main-read",
    complete: unknown.length === 0,
    rows,
    unknown,
    steps,
    subject: {
      read,
      merge: options.merge,
      after: options.after,
      ...(order.afterMerge === undefined ? {} : { afterMerge: order.afterMerge }),
    },
    files: { tester, written, missing },
  };
}

// ---------------------------------------------------------------------------
// The run

function printReport(report: CensusReport): void {
  if (report.subject !== undefined) {
    console.log(`Read commit ${report.subject.read}; merge ${report.subject.merge}`);
  }
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
    console.log("Jobs another event, a label or the merge queue gates, which prove no check here");
    console.table(report.gated);
  }
  if (report.disabled !== undefined && report.disabled.length > 0) {
    console.log("Jobs and steps that never run, which prove nothing");
    console.table(report.disabled);
  }
  if (report.unproven !== undefined && report.unproven.length > 0) {
    console.log(`Results taken whose actor does not carry proven: ${report.unproven.join(", ")}`);
  }
  if (report.files !== undefined) {
    console.log(
      `Files written: ${report.files.written.length}; the read commit lacks ${report.files.missing.length}`
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
    if (merge === undefined) throw new Error("--merge is required: the merge commit of this work");
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
