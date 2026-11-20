import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rename, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { parseArgs } from "node:util";
import type { CaseRow } from "./build-corpus";
import {
  CannotRunError,
  type Corpus,
  counted,
  listOf,
  numberOf,
  outDirectory,
  pool,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import {
  type DispatchConfig,
  type DispatchEvent,
  DispatchRefusedError,
  dispatchConfig,
  pageEventLog,
  readBytes,
  readJson,
} from "./dispatch-read";
import { drawFactsOf, drawPath, type FinalTestTree, slicesOf, slicesPath } from "./draw-io";
import { LeakError } from "./render";
import {
  bytesDigestOf,
  CLAIM_RUN_VARIABLE,
  microsecondsOf,
  REPOSITORY_ROOT,
  RESERVE_VARIABLE,
  type Registration,
  runDigestOf,
} from "./reserve";

/**
 * Writes the inputs the claim's S10 reviewer judges each request of the final test on: the third
 * stage of the claim run, after its build (`build-corpus.ts`) and its draw (`draw.ts`), and
 * before the judging (`s10_judge.py`) and the analysis (`analyze.ts`), each stage run under the
 * registered read and the claim run (`APPROVAL_EVAL_RESERVE`, `APPROVAL_EVAL_CLAIM_RUN`).
 *
 * The inventory. Before any judging, `inventory.jsonl` names every answered approval request of
 * the person among the draw's report cases, and no other (the claim's point 6): the approval cases
 * of each eligible tree the draw put in report, keyed by ask id. The cases of one ask are one
 * request, and are its provenance only: never a second judgement or a second row of any figure.
 * A report case the build does not hold, an approval case of a tree the draw left out of report,
 * a case `slices.json` and the trees disagree on, a case named twice, and an ask whose cases sit
 * in two trees are each refused. A case id is `<ask id>@<answer time>`, so no file here names one:
 * a line carries the count of its cases, and `analyze.ts` maps the cases to the requests by ask id
 * over the draw's report trees, the draw pinned by its sha256 in `s10-inputs.json`.
 *
 * What is read, and as of when (point 3). Live, S10 judged a request at its last opening or edit,
 * before the person's answer existed. Here each request is read as it stood at the judged moment:
 * the last `ask.opened` or `ask.edited` of the ask, by event id, strictly before the person's
 * answer, which is the first answer event by the person on the ask in the log's order: its
 * `ask.answered` (a user actor who is the person and the user the answer names, as the build reads
 * an answer), or, for an ask that names one document, an `artifact.approved` or
 * `artifact.changes_requested` by the person on that document at the `version` the ask names, after
 * the ask first opened (point 7). An ask that names several documents is answered by its
 * `ask.answered` only. The request's inputs are the payload of the judged event whole, which is
 * what S10 read live; the issue's `title`, `status`, `labels`, `parent` and `primary_artifact_id`
 * as the log's `issue.` events set them up to that moment (`issue.created`, `issue.updated` and
 * `issue.closed` carry the whole issue, `issue.claimed` and `issue.released` its status), with the
 * id of the last of them applied; and each document the payload names, as S10 names them
 * (`approval.documents`, or the approval's own document where that list is absent), read at its
 * immutable `version` and checked against the sha256 the version route answers. S10's fact check
 * reads only those fields of the ask and the issue, and no document's text beyond these.
 *
 * His answer is placed and never written: no verdict, option, words or answer time of it, and no
 * id of its event, is in any file this writes. The analysis places it again over its own walk of
 * the log with the rule here (`InputsLog`), so the two stages read one answer. A payload at the
 * judged moment that carries an `answer`, or an `approval.approved` that is not before the
 * moment, makes the request a deviation, and every request file is held, before it is written, to
 * `assertNoAnswer`: no case id of the ask, no event id that is the answer's, no `answered_at` at
 * or after the moment, and no `selected` of the person's in the request's own part. Inputs that
 * break it are not the request as it stood, so the request is a deviation and no file is written.
 *
 * The neighbours (point 4). Dispatch has no search as of a past moment, so only the search hits
 * and their ranks come from the run: S10's two queries of each request, its as-of issue title and
 * the first 1000 characters of its as-of question, each cut to 1000 characters as S10 cuts them,
 * run here once each (a query two requests share runs once), at `offset=0` and at `offset=50`
 * only where the first page held 50 results. Each page is archived once, in `pages/`, with each
 * query and offset it answered and when, its hits in order (id, kind and rank), and the sha256 of
 * the bytes the server sent. The bytes pin what the server answered; the body itself is kept
 * nowhere, because it carries answers, the person's answer to the judged request itself among
 * them wherever that request is a hit of its own query.
 *
 * Every ask among the hits is first decided by `reserve-ask.ts`, the bytes of commit 651db8d5
 * (`RESERVE_ASK_SHA256`, checked before anything is read), run with neither `APPROVAL_EVAL_RESERVE`
 * nor `APPROVAL_EVAL_CLAIM_RUN` in its environment, so outside the claim's read: exit 0 keeps the
 * candidate, 1 holds it, and anything else holds it too and is counted as an error, as S10 counts
 * it. Every ask hit is decided, those answered before the reserve's cut included, which S10 would
 * not ask about and which the reserve keeps: one decision per ask, made before any field of it is
 * looked up in the walk or written, and a held candidate's fields are never written. Each kept
 * candidate is then read as it stood at the judged moment. Any event of its ask after the moment
 * drops it (`later-event`), and the next hit is S10's to take: an event of an ask is an `ask.`
 * event whose payload's `id` is the ask, or any event whose payload's `ask_id` is (its comments,
 * deliveries, followers and document settlements). An ask with no event in the log through the
 * pin opened after it, so after the moment, and is dropped too, and so is one with an event after
 * the pin (below). Otherwise its fields are its last whole payload's, which no event after the
 * moment changed, and its issue's title as of the moment; a field the log cannot give as of the
 * moment makes the request a deviation. The filters (decisions the person answered before the
 * judged request opened, none on its issue, none the reserve holds) are S10's own code's, run over
 * these fields. Beside each request are its counts, the ones dropped by an event after the moment
 * among them.
 *
 * The walk. The log is walked once, through the build's pin, and refused unless it reads as many
 * events as the build read. It keeps each ask's last event id and its last whole payload's fields,
 * each issue's five fields at each event that set one, and each inventory ask's statements up to
 * its answer: not the whole log. Every input is as of the judged moment from that walk alone. The
 * searches run after the pin, and an event between the pin and them can move a rank too, so once
 * the searches are done the log is read on from the pin to its newest event at that time, the
 * drop scan, and of each of those events nothing is read but the ask it is an event of: a
 * candidate with any is dropped (`later-event`, seen `after-pin`). The scan's last event id is
 * recorded (`dropScanThrough`) before any request file is written, and a resume scans through
 * that id and never further, so it drops the same candidates its first run did.
 *
 * The stage record. `s10-inputs.json` records the claim run, the read it is under, this run's
 * digest, the person and the pin, the sha256 of the build's `snapshot.json` and of the draw's
 * `draw.json` and `slices.json`, so the build, the draw and these inputs are one chain and no
 * directory can be put in another's place between stages; the draw's own record names the claim
 * run and the build's sha256, and the draw is refused unless they are this run's. It is written
 * when the run starts, again once the drop scan is done, with `dropScanThrough`, and again when
 * it ends, with the counts, the sha256 of `inventory.jsonl` and of each request file, and the
 * pages. A run into an `--out` holding another run's record, or files of no record, is refused; a
 * run into its own claim run's directory resumes it: it takes each page already archived rather
 * than ask that page again, takes the recorded `dropScanThrough`, and never writes again a
 * request file already written.
 */

const USAGE = `usage: bun scripts/approval-eval/s10-inputs.ts --corpus <build dir> \\
  --draw <draw dir> --out <dir>

Writes the inputs S10 judges each approval request of the claim's final test on, each read as it
stood at the request's last opening or edit before the person's answer, and no part of that
answer: the inventory, one file of inputs per request, and a record of each search page S10's
neighbours read. Runs only under the claim run's registered read, over the build and the draw
that claim run made.

Options:
  --corpus <dir>  Required. The claim run's build: snapshot.json and corpus.jsonl.
  --draw <dir>    Required. The claim run's draw of that build: draw.json and slices.json.
  --out <dir>     Required. Where inventory.jsonl, requests/, pages/ and s10-inputs.json are
                  written. An absolute path outside this repository's workspace. A directory this
                  command wrote in the same claim run, over the same build and draw, is resumed.

Environment:
  APPROVAL_EVAL_RESERVE=read:<reference>, APPROVAL_EVAL_CLAIM_RUN=<start reply id>
                  Required: the read claim.ts registers for this run's digest, and the claim run
                  the build recorded.

Exit status: 0 when the inputs were written, 1 when the inventory holds no request, 2 when the
command could not run.`;

/**
 * The sha256 of `reserve-ask.ts` at commit 651db8d5, the decision point 4 (d) names. A checkout
 * whose file differs is refused before anything is read.
 */
export const RESERVE_ASK_SHA256 =
  "604e2f943ed71f6d43761f594d333e083d9d2811f28df779673b32a2fefdde42";

/** That command in this checkout, which is run for every ask among the search hits. */
const RESERVE_ASK_PATH = join(import.meta.dir, "reserve-ask.ts");

/** How many hits one search page asks for, as S10 asks. */
const SEARCH_PAGE = 50;

/** How much of a query S10 sends, in characters (code points, as Python counts them). */
const QUERY_CHARACTERS = 1000;

/** How many searches, document reads and decisions run at once. */
const SEARCH_CONCURRENCY = 4;
const DOCUMENT_CONCURRENCY = 8;
const DECISION_CONCURRENCY = 8;

/** The files this writes, under `--out`. */
export const INVENTORY_FILE = "inventory.jsonl";
export const STAGE_FILE = "s10-inputs.json";
export const REQUESTS_DIRECTORY = "requests";
export const PAGES_DIRECTORY = "pages";

/** The version of the shapes this writes, which a reader checks. */
export const INPUTS_VERSION = 1;

/** The fields of an issue S10 reads, as Dispatch names them. */
export const ISSUE_FIELDS = ["title", "status", "labels", "parent", "primary_artifact_id"] as const;

/** One of those fields. */
export type IssueField = (typeof ISSUE_FIELDS)[number];

/** The fields of a candidate's ask that S10's neighbours read, as Dispatch names them. */
export const CANDIDATE_FIELDS = [
  "id",
  "kind",
  "state",
  "issue_key",
  "question",
  "options",
  "multiple",
  "approval",
  "answer",
  "created_at",
] as const;

/** The inputs cannot be written as the registered text fixes them, so the command stops. */
export class S10InputsError extends CannotRunError {
  override readonly name = "S10InputsError";
}

/** How `reserve-ask.ts` decided one ask: kept, held, or an error, which S10 holds and counts. */
export type ReserveVerdict = "kept" | "held" | "error";

/** Decides one ask under the reserve, outside the claim's read. A test hands in its own. */
export type ReserveDecision = (askId: string) => Promise<ReserveVerdict>;

/** What a caller may hand the command beside its flags: a test's registration, root, decision. */
export interface InputsOptions {
  registration?: Registration;
  root?: string;
  decide?: ReserveDecision;
}

/** The moment a request is judged at: its last opening or edit before the person's answer. */
export interface JudgedMoment {
  eventId: number;
  type: string;
  createdAt: string;
}

/** One line of `inventory.jsonl`: one request, and where it stands. */
export interface InventoryLine {
  askId: string;
  issueKey: string | null;
  /** The draw's tree group the request's cases are in. */
  group: string;
  /** How many cases of the corpus are this one request's provenance. */
  cases: number;
  /** Null where the log gives no such moment, which is the deviation's reason. */
  judged: JudgedMoment | null;
  state: "ready" | "deviation";
  reason: string | null;
  /** The sha256 of `requests/<ask id>.json`, written for a ready request only. */
  requestSha256: string | null;
}

/** The issue of a request as of its judged moment, in Dispatch's own field names. */
export type IssueAsOf = { key: string } & Record<IssueField, unknown>;

/** One document a request names, at the version it names, read from that version's route. */
export interface RequestDocument {
  artifact_id: string;
  name: unknown;
  version: number;
  markdown: string;
  sha256: string;
}

/** One ask among a query's hits, in the order the pages give them. */
export interface QueryHit {
  id: string;
  rank: number | null;
  /** The sha256 of the page it is on, and its place among that page's hits, from 0. */
  page: string;
  position: number;
}

/** One of a request's two queries: what was sent, the pages it was answered by, its ask hits. */
export interface QueryInputs {
  query: string;
  pages: Array<{ sha256: string; offset: number }>;
  hits: QueryHit[];
}

/** A candidate as of the judged moment, in the field names of `GET /api/v1/asks/{id}`'s `ask`. */
export interface KeptCandidate {
  /** The event whose payload the fields are: the candidate's last whole payload. */
  asOfEventId: number;
  ask: Partial<Record<(typeof CANDIDATE_FIELDS)[number], unknown>>;
  issue: { key: string; title: string } | null;
}

/** What one ask among the hits is to S10's neighbours, as of the judged moment. */
export type CandidateInputs =
  | { held: true; reserve: "held" | "error" }
  | { dropped: "later-event"; seen: "log" | "no-event-through-pin" | "after-pin" }
  | KeptCandidate;

/** What a request's candidates came to. */
export interface CandidateCounts {
  /** Ask hits over both queries, a hit both answered counted twice. */
  askHits: number;
  /** The distinct asks among them. */
  candidates: number;
  held: number;
  /** Of the held, the asks `reserve-ask.ts` decided nothing on. */
  reserveErrors: number;
  /** Dropped by point 4 (b): an event of the ask after the judged moment. */
  droppedLaterEvent: number;
  kept: number;
}

/** `requests/<ask id>.json`: one request's inputs as of its judged moment. */
export interface RequestInputs {
  version: number;
  askId: string;
  issueKey: string;
  group: string;
  cases: number;
  judged: JudgedMoment;
  /** The payload of the judged event, whole, as S10 read it live from the log. */
  ask: Record<string, unknown>;
  issue: IssueAsOf;
  /** The last `issue.` event applied to `issue`. */
  issueAsOfEventId: number;
  documents: RequestDocument[];
  search: { title: QueryInputs; question: QueryInputs };
  candidates: Record<string, CandidateInputs>;
  counts: CandidateCounts;
}

/** One read a page answered: the query, its offset, and when it was read. */
export interface PageRead {
  query: string;
  offset: number;
  readAt: string;
}

/** One hit of a page, as the page gives it. */
export interface PageHit {
  id: string;
  kind: string;
  rank: number | null;
}

/**
 * `pages/<sha256>.json`: one page the search answered, named by the sha256 of its bytes. Two reads
 * answered by the same bytes are one page, with both reads.
 */
export interface PageRecord {
  sha256: string;
  hits: PageHit[];
  reads: PageRead[];
}

/** The chain a stage record holds, which a resume must match whole. */
export interface StageChain {
  claimRun: string;
  reference: string;
  runDigest: string;
  person: string;
  throughEventId: number;
  build: { directory: string; snapshotSha256: string };
  draw: { directory: string; drawSha256: string; slicesSha256: string };
  /** The sha256 of the `reserve-ask.ts` that decided, and null where a test decided. */
  reserveAsk: string | null;
}

/** `s10-inputs.json`: the stage's record. */
export interface StageRecord extends StageChain {
  stage: "s10-inputs";
  version: number;
  state: "started" | "written";
  startedAt: string;
  finishedAt: string | null;
  /**
   * The last event of the drop scan, which read on from the pin once the searches were done and
   * which a resume scans through again; null before the scan.
   */
  dropScanThrough: number | null;
  counts: {
    requests: number;
    ready: number;
    deviations: number;
    cases: number;
    queries: number;
    pages: number;
    candidates: number;
    held: number;
    reserveErrors: number;
    droppedLaterEvent: number;
  } | null;
  inventorySha256: string | null;
  /** The sha256 of each request file, by ask id. */
  requests: Record<string, string>;
  /** The sha256 of each page, sorted. */
  pages: string[];
}

/** One request of the inventory, with the corpus's cases of its ask, which are never written. */
export interface InventoryRequest {
  askId: string;
  issueKey: string | null;
  group: string;
  rows: readonly CaseRow[];
}

interface Flags {
  corpus: string;
  draw: string;
  out: string;
}

function readFlags(argv: readonly string[]): Flags {
  let values: { corpus?: string; draw?: string; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        draw: { type: "string" },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const draw = values.draw?.trim();
  const out = values.out?.trim();
  if (!corpus || !draw || !out) throw new UsageError(USAGE);
  return { corpus, draw, out };
}

/** The sha256 of some bytes, as every record here names a file by. */
function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** The first `count` characters of a text, as Python's `text[:count]` takes them. */
export function firstCharacters(text: string, count: number = QUERY_CHARACTERS): string {
  return Array.from(text).slice(0, count).join("");
}

/**
 * The documents an ask names, as S10 reads them: `approval.documents`, or the approval's own
 * document where that list is absent or empty. Each field is as the payload holds it.
 */
export function documentsOf(
  approval: unknown
): Array<{ artifact_id: unknown; name: unknown; version: unknown }> {
  const record = recordOf(approval);
  if (record === null) return [];
  const listed = listOf(record.documents)
    .map(recordOf)
    .filter((entry): entry is Record<string, unknown> => entry !== null);
  const entries = listed.length > 0 ? listed : Object.hasOwn(record, "artifact_id") ? [record] : [];
  return entries.map((entry) => ({
    artifact_id: entry.artifact_id,
    name: entry.name,
    version: entry.version,
  }));
}

/**
 * The asks an event of the log is an event of: the ask an `ask.` event's payload is, and the ask
 * any event's payload names under `ask_id`.
 */
export function askIdsOf(event: DispatchEvent): string[] {
  const own = event.type.startsWith("ask.") ? stringOf(event.payload.id) : null;
  const named = stringOf(event.payload.ask_id);
  const ids: string[] = [];
  if (own !== null) ids.push(own);
  if (named !== null && named !== own) ids.push(named);
  return ids;
}

/** What the walk keeps of one ask through the pin. */
interface AskLog {
  /** The id of the last event of the ask. */
  lastEventId: number;
  /** Its last whole payload's fields that S10's neighbours read, with that event's id. */
  latest: { eventId: number; fields: KeptCandidate["ask"] } | null;
}

/** An issue's fields as one event left them. */
interface IssueStep {
  eventId: number;
  fields: Readonly<Partial<Record<IssueField, unknown>>>;
}

/** Where one request of the inventory stands in the walk. */
export interface Judging {
  /** The id of the ask's first `ask.opened`. */
  firstOpened: number | null;
  /** Its last `ask.opened` or `ask.edited` so far, while no answer of the person has come. */
  statement: DispatchEvent | null;
  /** The id of the person's first answer event on the ask. */
  answerEventId: number | null;
  /** The statement that stood when that answer came: the judged moment. */
  judged: DispatchEvent | null;
}

/**
 * One walk of the log, read for the inputs: each ask's last event and last whole payload, each
 * issue's fields at each event that set one, and the statements and first answer of each request
 * of the inventory (`Judging`). The walk hands the events in id order.
 */
export class InputsLog {
  private readonly person: string;
  private readonly asks = new Map<string, AskLog>();
  private readonly issues = new Map<string, IssueStep[]>();
  private readonly judging = new Map<string, Judging>();

  constructor(person: string, inventory: Iterable<string>) {
    this.person = person;
    for (const askId of inventory) {
      this.judging.set(askId, {
        firstOpened: null,
        statement: null,
        answerEventId: null,
        judged: null,
      });
    }
  }

  /** Reads one event. */
  note(event: DispatchEvent): void {
    if (event.type.startsWith("issue.")) this.noteIssue(event);
    const own = event.type.startsWith("ask.") ? stringOf(event.payload.id) : null;
    for (const askId of askIdsOf(event)) {
      const known: AskLog = this.asks.get(askId) ?? { lastEventId: 0, latest: null };
      known.lastEventId = event.id;
      // A whole ask, as its opening, edits, answers and resolution carry it; a follower or a
      // comment names the ask and carries none of it.
      const whole = Object.hasOwn(event.payload, "state") && Object.hasOwn(event.payload, "kind");
      if (askId === own && whole) {
        known.latest = { eventId: event.id, fields: candidateFieldsOf(event.payload) };
      }
      this.asks.set(askId, known);
    }
    if (own !== null) this.noteStatement(own, event);
    if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
      this.noteSettlement(event);
    }
  }

  /** Where one request of the inventory stands. */
  judgingOf(askId: string): Judging | undefined {
    return this.judging.get(askId);
  }

  /** What the walk kept of one ask, and none where the log through the pin holds no event of it. */
  askOf(askId: string): AskLog | undefined {
    return this.asks.get(askId);
  }

  /** An issue's fields as of an event: as the last `issue.` event at or before it left them. */
  issueAsOf(key: string, eventId: number): IssueStep | null {
    const steps = this.issues.get(key) ?? [];
    let low = 0;
    let high = steps.length;
    while (low < high) {
      const middle = (low + high) >>> 1;
      if ((steps[middle] as IssueStep).eventId <= eventId) low = middle + 1;
      else high = middle;
    }
    return steps[low - 1] ?? null;
  }

  private noteIssue(event: DispatchEvent): void {
    const key = stringOf(event.payload.key);
    if (key === null) return;
    const set: Partial<Record<IssueField, unknown>> = {};
    let any = false;
    for (const field of ISSUE_FIELDS) {
      if (!Object.hasOwn(event.payload, field)) continue;
      set[field] = event.payload[field];
      any = true;
    }
    if (!any) return;
    const steps = this.issues.get(key) ?? [];
    const before = steps[steps.length - 1]?.fields ?? {};
    steps.push({ eventId: event.id, fields: { ...before, ...set } });
    this.issues.set(key, steps);
  }

  private noteStatement(askId: string, event: DispatchEvent): void {
    const judging = this.judging.get(askId);
    if (judging === undefined || judging.answerEventId !== null) return;
    if (event.type === "ask.opened" || event.type === "ask.edited") {
      if (event.type === "ask.opened" && judging.firstOpened === null) {
        judging.firstOpened = event.id;
      }
      judging.statement = event;
      return;
    }
    if (event.type !== "ask.answered") return;
    // The person's own answer, by the rule the build reads one with (`caseOf`).
    const actor = event.actor;
    if (actor === null || actor.kind !== "user" || actor.id !== this.person) return;
    if (stringOf(recordOf(event.payload.answer)?.user) !== actor.id) return;
    judging.answerEventId = event.id;
    judging.judged = judging.statement;
  }

  /**
   * A document settlement by the person, which answers each request still waiting for its answer
   * that names that one document at that version and first opened before it.
   */
  private noteSettlement(event: DispatchEvent): void {
    const actor = event.actor;
    if (actor === null || actor.kind !== "user" || actor.id !== this.person) return;
    const artifactId = stringOf(event.payload.artifact_id);
    const version = numberOf(event.payload.version);
    if (artifactId === null || version === null) return;
    for (const [askId, judging] of this.judging) {
      if (judging.answerEventId !== null || judging.firstOpened === null) continue;
      if (event.id <= judging.firstOpened) continue;
      const documents = documentsOf(this.asks.get(askId)?.latest?.fields.approval);
      const only = documents.length === 1 ? documents[0] : undefined;
      if (only?.artifact_id !== artifactId || only.version !== version) continue;
      judging.answerEventId = event.id;
      judging.judged = judging.statement;
    }
  }
}

/** The fields of a whole payload that S10's neighbours read. */
function candidateFieldsOf(payload: Record<string, unknown>): KeptCandidate["ask"] {
  const fields: KeptCandidate["ask"] = {};
  for (const field of CANDIDATE_FIELDS) {
    if (Object.hasOwn(payload, field)) fields[field] = payload[field];
  }
  return fields;
}

/**
 * The inventory: one request per ask among the approval cases of the eligible trees the draw put
 * in report, sorted by ask id (point 6). Every way the draw's two files could make a report case
 * map to no request or to two is refused, and so is a case either names twice.
 */
export function inventoryOf(
  byCaseId: ReadonlyMap<string, CaseRow>,
  trees: readonly FinalTestTree[],
  report: readonly string[]
): InventoryRequest[] {
  const inReport = new Set<string>();
  for (const caseId of report) {
    if (inReport.has(caseId)) {
      throw new S10InputsError(`the draw's report slice names the case ${caseId} twice`);
    }
    inReport.add(caseId);
  }
  const requests = new Map<string, InventoryRequest & { rows: CaseRow[] }>();
  const placed = new Set<string>();
  for (const tree of trees) {
    for (const caseId of tree.cases) {
      const row = byCaseId.get(caseId);
      if (row === undefined) {
        throw new S10InputsError(
          `the draw's tree ${tree.group} names the case ${caseId}, which the build does not hold`
        );
      }
      if (placed.has(caseId)) {
        throw new S10InputsError(`the draw's trees name the case ${caseId} twice`);
      }
      placed.add(caseId);
      if (row.kind !== "approval") continue;
      if (tree.slice !== "report" || !inReport.has(caseId)) {
        throw new S10InputsError(
          `the draw left the approval case ${caseId} of the eligible tree ${tree.group} out of ` +
            "its report slice, where the claim's draw puts every one"
        );
      }
      const known = requests.get(row.askId);
      if (known === undefined) {
        requests.set(row.askId, {
          askId: row.askId,
          issueKey: row.issueKey,
          group: tree.group,
          rows: [row],
        });
      } else if (known.group !== tree.group) {
        throw new S10InputsError(
          `the ask ${row.askId} has cases in the trees ${known.group} and ${tree.group}, so its ` +
            "one request would be judged twice"
        );
      } else {
        known.rows.push(row);
      }
    }
  }
  for (const caseId of inReport) {
    const row = byCaseId.get(caseId);
    if (row === undefined) {
      throw new S10InputsError(
        `the draw's report slice names the case ${caseId}, which the build does not hold`
      );
    }
    if (!placed.has(caseId)) {
      throw new S10InputsError(
        `the draw's report slice holds the case ${caseId}, which no eligible tree of its ` +
          "record holds"
      );
    }
  }
  return [...requests.values()].sort((one, other) =>
    one.askId < other.askId ? -1 : one.askId > other.askId ? 1 : 0
  );
}

/** What `assertNoAnswer` holds a request file to: the person's answer to the judged request. */
export interface AnswerGuard {
  askId: string;
  answerEventId: number;
  /** Each `selected` the person's cases of the ask carry. */
  selected: readonly (readonly string[])[];
  /** The judged moment, in microseconds. */
  judgedAt: number;
}

/** An `answered_at`, as a fence's attribute inside a JSON string and as a JSON field. */
const ANSWERED_AT = [/answered_at=\\"([^"\\]*)\\"/g, /"answered_at":\s*"([^"\\]*)"/g];

/** A field that names an event, whose value is an event id. */
const EVENT_FIELD = /(?:^|_)event_id$|EventId$|^eventId$|^id$/;

/** An ask fence's attributes that only an answer writes. */
const ANSWER_ATTRIBUTES = /\b(?:answered_at|answered_by|selected|answer)="/;

/**
 * Holds one request's inputs to the rule that no part of the person's answer to it is written,
 * and gives back the text to write. It throws `LeakError` where the text names a case of the ask
 * (`<ask id>@<answer time>`), an `answered_at` at or after the judged moment, or a field naming an
 * event whose value is the answer's event; or where the request's own part (the judged payload,
 * the issue, the documents) holds an `answer`, a `selected` the person chose, or a fence of the
 * judged ask that carries an answer.
 */
export function assertNoAnswer(inputs: RequestInputs, guard: AnswerGuard): string {
  const text = `${JSON.stringify(inputs, null, 2)}\n`;
  const caseOfAsk = `${guard.askId}@`;
  if (text.includes(caseOfAsk)) {
    throw new LeakError(
      guard.askId,
      caseOfAsk,
      "a case id of the judged ask, which holds its answer time"
    );
  }
  for (const pattern of ANSWERED_AT) {
    for (const match of text.matchAll(pattern)) {
      const at = microsecondsOf(match[1] ?? "");
      if (at !== null && at >= guard.judgedAt) {
        throw new LeakError(guard.askId, match[0], "an answer dated at or after the judged moment");
      }
    }
  }
  walkValues(inputs, null, (key, value) => {
    if (key !== null && EVENT_FIELD.test(key) && value === guard.answerEventId) {
      throw new LeakError(guard.askId, `${key}: ${value}`, "the id of the person's answer event");
    }
  });
  const answer = inputs.ask.answer;
  if (answer !== null && answer !== undefined) {
    throw new LeakError(guard.askId, JSON.stringify(answer), "an answer in the judged payload");
  }
  const chosen = new Set(guard.selected.map((one) => JSON.stringify(one)));
  for (const own of [inputs.ask, inputs.issue, inputs.documents]) {
    walkValues(own, null, (key, value) => {
      if (key === "selected" && Array.isArray(value) && chosen.has(JSON.stringify(value))) {
        throw new LeakError(guard.askId, JSON.stringify(value), "the person's selected option");
      }
    });
  }
  const fence = `:::ask{#${guard.askId}`;
  for (const document of inputs.documents) {
    for (const line of document.markdown.split("\n")) {
      if (line.startsWith(fence) && ANSWER_ATTRIBUTES.test(line)) {
        throw new LeakError(guard.askId, line, "the judged ask's fence carries an answer");
      }
    }
  }
  return text;
}

/** Calls `visit` with every value under `value` and the key it sits under (null for none). */
function walkValues(
  value: unknown,
  key: string | null,
  visit: (key: string | null, value: unknown) => void
): void {
  visit(key, value);
  if (Array.isArray(value)) {
    for (const entry of value) walkValues(entry, key, visit);
    return;
  }
  const record = recordOf(value);
  if (record === null) return;
  for (const [name, entry] of Object.entries(record)) walkValues(entry, name, visit);
}

/**
 * The decision point 4 (d) names: `reserve-ask.ts` as commit 651db8d5 holds it, refused unless the
 * checkout's bytes are those, run for one ask with neither `APPROVAL_EVAL_RESERVE` nor
 * `APPROVAL_EVAL_CLAIM_RUN` in its environment. What it prints is not kept: a held ask's facts are
 * never written.
 */
export async function reserveAskDecision(
  path: string = RESERVE_ASK_PATH
): Promise<ReserveDecision> {
  const digest = await bytesDigestOf(path);
  if (digest !== RESERVE_ASK_SHA256) {
    throw new S10InputsError(
      `${path} has sha256 ${digest}, and the claim decides its neighbours by reserve-ask.ts at ` +
        `651db8d5, whose sha256 is ${RESERVE_ASK_SHA256}`
    );
  }
  const outside = new Set<string>([RESERVE_VARIABLE, CLAIM_RUN_VARIABLE]);
  const env = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !outside.has(name))
  );
  return async (askId) => {
    const child = Bun.spawn([process.execPath, path, "--ask", askId], {
      env,
      stdout: "ignore",
      stderr: "ignore",
    });
    const exit = await child.exited;
    return exit === 0 ? "kept" : exit === 1 ? "held" : "error";
  };
}

/** How many files this process has begun to write, which names each one's partial file. */
let begun = 0;

/** Writes a file whole or not at all, so a resume never takes a half-written one for written. */
async function writeWhole(path: string, text: string): Promise<void> {
  begun += 1;
  const partial = `${path}.${process.pid}.${begun}.partial`;
  await writeFile(partial, text, "utf8");
  await rename(partial, path);
}

/** The bytes of a file the claim's draw wrote, refused where it is not there. */
async function drawBytesOf(path: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new S10InputsError(`${path} could not be read: --draw takes the directory draw.ts wrote`);
  }
}

/** The claim's draw, as this run reads it, with the sha256 of each of its two files. */
interface ClaimDraw {
  directory: string;
  trees: readonly FinalTestTree[];
  report: readonly string[];
  drawSha256: string;
  slicesSha256: string;
}

/**
 * The draw at `directory`, refused unless the claim run made it under this read from this build,
 * as its record says: the claim run it names, the sha256 of the build's `snapshot.json` it read,
 * the read its final test was drawn under, and that it applied no never-decide rule.
 */
async function claimDrawOf(
  directory: string,
  corpus: Corpus,
  reference: string,
  claimRun: string
): Promise<ClaimDraw> {
  const drawFile = drawPath(directory);
  const slicesFile = slicesPath(directory);
  const drawBytes = await drawBytesOf(drawFile);
  const slicesBytes = await drawBytesOf(slicesFile);
  const facts = drawFactsOf(drawBytes.toString("utf8"), drawFile);
  if (facts.claimRun?.id !== claimRun) {
    throw new S10InputsError(
      `${drawFile} was drawn in the claim run ${facts.claimRun?.id ?? "of no record"}, and this ` +
        `run is ${claimRun}: a stage takes only its own claim run's draw`
    );
  }
  if (facts.claimRun.snapshotSha256 !== corpus.snapshotSha256) {
    throw new S10InputsError(
      `${drawFile} was drawn from the build whose snapshot.json has sha256 ` +
        `${facts.claimRun.snapshotSha256}, and ${corpus.directory}'s has ${corpus.snapshotSha256}`
    );
  }
  if (facts.finalTestReference !== reference) {
    throw new S10InputsError(
      `${drawFile} was drawn under ${facts.finalTestReference ?? "no read of the reserve"}, and ` +
        `this run reads under ${reference}`
    );
  }
  if (facts.neverDecideApplied !== false) {
    throw new S10InputsError(
      `${drawFile} does not record that it applied no never-decide rule, which the claim's draw ` +
        "does not apply (draw.ts)"
    );
  }
  if (facts.finalTestTrees === undefined) {
    throw new S10InputsError(`${drawFile} records no final test's trees`);
  }
  return {
    directory,
    trees: facts.finalTestTrees,
    report: slicesOf(slicesBytes.toString("utf8"), slicesFile).report,
    drawSha256: sha256Of(drawBytes),
    slicesSha256: sha256Of(slicesBytes),
  };
}

/** The chain's fields, in the order a refusal names the first that differs. */
function chainDifference(found: Record<string, unknown>, chain: StageChain): string | null {
  const wanted: Array<[string, unknown, unknown]> = [
    ["claim run", found.claimRun, chain.claimRun],
    ["read", found.reference, chain.reference],
    ["run digest", found.runDigest, chain.runDigest],
    ["person", found.person, chain.person],
    ["pin", found.throughEventId, chain.throughEventId],
    ["build", recordOf(found.build)?.snapshotSha256, chain.build.snapshotSha256],
    ["draw", recordOf(found.draw)?.drawSha256, chain.draw.drawSha256],
    ["slices", recordOf(found.draw)?.slicesSha256, chain.draw.slicesSha256],
    ["reserve-ask.ts", found.reserveAsk, chain.reserveAsk],
  ];
  for (const [what, was, is] of wanted) {
    if (was !== is) return `its ${what} is ${String(was)}, and this run's is ${String(is)}`;
  }
  return null;
}

/** Where a run in a directory stands: when it first started, and how far its drop scan read. */
interface RunStart {
  startedAt: string;
  dropScanThrough: number | null;
}

/**
 * Takes `directory` for this run: a directory holding nothing is started, with a stage record
 * naming the chain; one holding this chain's record is resumed; anything else is refused.
 */
async function claimDirectory(directory: string, chain: StageChain): Promise<RunStart> {
  await mkdir(directory, { recursive: true });
  const recordPath = join(directory, STAGE_FILE);
  const text = await readFile(recordPath, "utf8").catch(() => null);
  if (text === null) {
    const entries = await readdir(directory);
    if (entries.length > 0) {
      throw new S10InputsError(
        `${directory} holds files and no ${STAGE_FILE}, so no run here can show they are its own`
      );
    }
    const startedAt = new Date().toISOString();
    await writeStage(directory, emptyStage(chain, startedAt));
    return { startedAt, dropScanThrough: null };
  }
  let found: Record<string, unknown> | null;
  try {
    found = recordOf(JSON.parse(text));
  } catch {
    found = null;
  }
  if (found === null || found.stage !== "s10-inputs" || found.version !== INPUTS_VERSION) {
    throw new S10InputsError(`${recordPath} is no record this command writes`);
  }
  const difference = chainDifference(found, chain);
  if (difference !== null) {
    throw new S10InputsError(
      `${directory} holds another run's inputs: ${difference}. A resume takes only its own ` +
        "claim run's directory, over the same build and draw"
    );
  }
  // A scan the earlier run recorded is the one this run takes: it reads through it and no further.
  const through = numberOf(found.dropScanThrough);
  if (found.dropScanThrough !== null && (through === null || through < chain.throughEventId)) {
    throw new S10InputsError(`${recordPath} records a drop scan through no event at the pin or on`);
  }
  return {
    startedAt: stringOf(found.startedAt) ?? new Date().toISOString(),
    dropScanThrough: through,
  };
}

/** A stage record of a run that has written nothing yet. */
function emptyStage(chain: StageChain, startedAt: string): StageRecord {
  return {
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state: "started",
    ...chain,
    startedAt,
    finishedAt: null,
    dropScanThrough: null,
    counts: null,
    inventorySha256: null,
    requests: {},
    pages: [],
  };
}

async function writeStage(directory: string, record: StageRecord): Promise<void> {
  await writeWhole(join(directory, STAGE_FILE), `${JSON.stringify(record, null, 2)}\n`);
}

/** The pages already archived in `directory`, by their sha256. */
async function archivedPages(directory: string): Promise<Map<string, PageRecord>> {
  const pages = new Map<string, PageRecord>();
  let names: string[];
  try {
    names = await readdir(directory);
  } catch {
    return pages;
  }
  for (const name of names) {
    if (!name.endsWith(".json")) continue;
    const path = join(directory, name);
    const record = pageRecordOf(await readFile(path, "utf8"), path);
    if (`${record.sha256}.json` !== name) {
      throw new S10InputsError(`${path} records the page ${record.sha256}, not the one it names`);
    }
    pages.set(record.sha256, record);
  }
  return pages;
}

/** One page record read back, refused where it is not the shape this command writes. */
function pageRecordOf(text: string, path: string): PageRecord {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new S10InputsError(`${path} is not JSON`);
  }
  const record = recordOf(parsed);
  const sha256 = stringOf(record?.sha256);
  const hits = listOf(record?.hits).map((one) => hitOf(one, path));
  const reads = listOf(record?.reads).map((one) => {
    const read = recordOf(one);
    const query = stringOf(read?.query);
    const offset = numberOf(read?.offset);
    const readAt = stringOf(read?.readAt);
    if (query === null || offset === null || readAt === null) {
      throw new S10InputsError(`${path} records a read with no query, offset or time`);
    }
    return { query, offset, readAt };
  });
  if (sha256 === null || reads.length === 0 || !Array.isArray(record?.hits)) {
    throw new S10InputsError(`${path} is no page record this command writes`);
  }
  return { sha256, hits, reads };
}

/** One hit of a search answer: its id, its kind and its rank. Any other shape stops the run. */
function hitOf(value: unknown, where: string): PageHit {
  const hit = recordOf(value);
  const id = stringOf(hit?.id);
  const kind = stringOf(hit?.kind);
  if (id === null || kind === null) {
    throw new S10InputsError(`${where} holds a hit with no id or no kind`);
  }
  return { id, kind, rank: numberOf(hit?.rank) };
}

/**
 * The search, as S10 asks it and once for each query and offset: every page already archived is
 * taken, and every other page is read now, named by the sha256 of its bytes and archived before
 * it is used. A body that is no search answer stops the run.
 */
class Searches {
  private readonly directory: string;
  private readonly config: DispatchConfig;
  private readonly pages: Map<string, PageRecord>;
  private readonly byRead = new Map<string, PageRecord>();
  private readonly writing = new Map<string, Promise<void>>();
  /** How many pages this run read, beside the ones it took from an earlier run. */
  read = 0;

  constructor(directory: string, config: DispatchConfig, pages: Map<string, PageRecord>) {
    this.directory = directory;
    this.config = config;
    this.pages = pages;
    for (const page of pages.values()) {
      for (const read of page.reads) this.byRead.set(`${read.offset}\n${read.query}`, page);
    }
  }

  /** The pages of one query: its first, and its second where the first held a full page. */
  async pagesOf(query: string): Promise<Array<{ offset: number; page: PageRecord }>> {
    const first = await this.pageOf(query, 0);
    const pages = [{ offset: 0, page: first }];
    if (first.hits.length === SEARCH_PAGE) {
      pages.push({ offset: SEARCH_PAGE, page: await this.pageOf(query, SEARCH_PAGE) });
    }
    return pages;
  }

  /** Every page this run holds, sorted by sha256. */
  all(): string[] {
    return [...this.pages.keys()].sort();
  }

  private async pageOf(query: string, offset: number): Promise<PageRecord> {
    const key = `${offset}\n${query}`;
    const known = this.byRead.get(key);
    if (known !== undefined) return known;
    const path =
      `/api/v1/search/results?q=${encodeURIComponent(query)}` +
      `&limit=${SEARCH_PAGE}&offset=${offset}`;
    const bytes = await readBytes(path, this.config);
    const readAt = new Date().toISOString();
    const sha256 = sha256Of(bytes);
    let body: unknown;
    try {
      body = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    } catch {
      throw new S10InputsError(`GET ${path} answered no JSON`);
    }
    const results = recordOf(body)?.results;
    if (!Array.isArray(results)) throw new S10InputsError(`GET ${path} answered no results`);
    const hits = results.map((one) => hitOf(one, `GET ${path}`));
    const page = this.pages.get(sha256) ?? { sha256, hits, reads: [] };
    page.reads.push({ query, offset, readAt });
    this.pages.set(sha256, page);
    this.byRead.set(key, page);
    this.read += 1;
    await this.archive(page);
    return page;
  }

  /** Writes one page's record, after any write of it already under way. */
  private async archive(page: PageRecord): Promise<void> {
    const before = this.writing.get(page.sha256) ?? Promise.resolve();
    const next = before.then(() =>
      writeWhole(join(this.directory, `${page.sha256}.json`), `${JSON.stringify(page, null, 2)}\n`)
    );
    this.writing.set(page.sha256, next);
    await next;
  }
}

/** One of a request's queries, as its pages answered it. */
function queryInputsOf(
  query: string,
  pages: ReadonlyArray<{ offset: number; page: PageRecord }>
): QueryInputs {
  const hits: QueryHit[] = [];
  for (const { page } of pages) {
    page.hits.forEach((hit, position) => {
      if (hit.kind !== "ask") return;
      hits.push({ id: hit.id, rank: hit.rank, page: page.sha256, position });
    });
  }
  return {
    query,
    pages: pages.map(({ offset, page }) => ({ sha256: page.sha256, offset })),
    hits,
  };
}

/**
 * A candidate as of the judged moment, or why the log cannot give it as of that moment. `afterPin`
 * holds the asks the drop scan found an event of after the pin.
 */
function candidateOf(
  askId: string,
  log: InputsLog,
  moment: number,
  afterPin: ReadonlySet<string>
): Exclude<CandidateInputs, { held: true }> | { deviation: string } {
  const known = log.askOf(askId);
  if (known === undefined) return { dropped: "later-event", seen: "no-event-through-pin" };
  if (known.lastEventId > moment) return { dropped: "later-event", seen: "log" };
  if (afterPin.has(askId)) return { dropped: "later-event", seen: "after-pin" };
  const latest = known.latest;
  if (latest === null) {
    return { deviation: `the log gives no payload of the ask ${askId} as of event ${moment}` };
  }
  const fields = latest.fields;
  for (const field of ["kind", "state"] as const) {
    if (typeof fields[field] !== "string") {
      return { deviation: `the log gives no ${field} of the ask ${askId} as of event ${moment}` };
    }
  }
  if (!Object.hasOwn(fields, "answer")) {
    return { deviation: `the log gives no answer field of the ask ${askId} as of event ${moment}` };
  }
  const issueKey = fields.issue_key;
  if (issueKey === null || issueKey === undefined) {
    return { asOfEventId: latest.eventId, ask: fields, issue: null };
  }
  if (typeof issueKey !== "string") {
    return { deviation: `the ask ${askId} names an issue that is no key as of event ${moment}` };
  }
  const title = stringOf(log.issueAsOf(issueKey, moment)?.fields.title);
  if (title === null) {
    return { deviation: `the log gives no title of ${issueKey} as of event ${moment}` };
  }
  return { asOfEventId: latest.eventId, ask: fields, issue: { key: issueKey, title } };
}

/**
 * The drop scan: the log read on from the pin, through `through` where an earlier run of this
 * claim run recorded it and to the log's newest event otherwise, and of each event nothing but the
 * ask it is an event of (`askIdsOf`). Returns those asks and the scan's last event id.
 */
async function dropScanOf(
  pin: number,
  through: number | null
): Promise<{ asks: Set<string>; through: number }> {
  const asks = new Set<string>();
  if (through === pin) return { asks, through };
  const walk = await pageEventLog({
    after: pin,
    through: through ?? undefined,
    onPage: (events) => {
      for (const event of events) for (const askId of askIdsOf(event)) asks.add(askId);
    },
  });
  return { asks, through: through ?? Math.max(pin, walk.lastEventId) };
}

/** A request of the inventory that cannot be judged as the text fixes, and why. */
interface Deviation {
  state: "deviation";
  request: InventoryRequest;
  judged: JudgedMoment | null;
  reason: string;
}

/** A request whose own inputs the log gives as of its judged moment, before its searches. */
interface Ready {
  state: "ready";
  request: InventoryRequest;
  judged: JudgedMoment;
  judgedAt: number;
  answerEventId: number;
  payload: Record<string, unknown>;
  issue: IssueAsOf;
  issueAsOfEventId: number;
  documents: Array<{ artifact_id: string; name: unknown; version: number }>;
  queries: { title: string; question: string };
}

/** One request as of its judged moment, before any document or search is read. */
function planOf(request: InventoryRequest, log: InputsLog): Ready | Deviation {
  const deviation = (reason: string, judged: JudgedMoment | null = null): Deviation => ({
    state: "deviation",
    request,
    judged,
    reason,
  });
  const judging = log.judgingOf(request.askId);
  if (judging === undefined || judging.answerEventId === null) {
    return deviation("the log through the pin holds no answer of the build's person to the ask");
  }
  const event = judging.judged;
  if (event === null) {
    return deviation("no ask.opened or ask.edited of the ask comes before the person's answer");
  }
  const judged = { eventId: event.id, type: event.type, createdAt: event.created_at };
  const judgedAt = microsecondsOf(event.created_at);
  if (judgedAt === null) return deviation("the judged event's time is no time", judged);
  const payload = event.payload;
  if (payload.answer !== null && payload.answer !== undefined) {
    return deviation("the payload at the judged moment carries an answer", judged);
  }
  const approved = recordOf(payload.approval)?.approved;
  if (approved !== null && approved !== undefined) {
    const at = microsecondsOf(stringOf(recordOf(approved)?.at) ?? "");
    if (at === null || at >= judgedAt) {
      return deviation(
        "the payload at the judged moment carries an approval.approved not before the moment",
        judged
      );
    }
  }
  const issueKey = stringOf(payload.issue_key) ?? event.issue_key;
  if (issueKey === null) {
    return deviation("the request names no issue, whose title S10 searches by", judged);
  }
  const step = log.issueAsOf(issueKey, event.id);
  const missing = ISSUE_FIELDS.filter((field) => !Object.hasOwn(step?.fields ?? {}, field));
  if (step === null || missing.length > 0) {
    return deviation(
      `the log gives no ${missing.join(", ")} of ${issueKey} as of event ${event.id}`,
      judged
    );
  }
  const title = stringOf(step.fields.title);
  if (title === null || title.trim() === "") {
    return deviation(`${issueKey} has no title as of event ${event.id} for S10 to search`, judged);
  }
  const question = stringOf(payload.question);
  if (question === null || question.trim() === "") {
    return deviation("the request has no question as of its judged moment", judged);
  }
  const documents: Ready["documents"] = [];
  for (const document of documentsOf(payload.approval)) {
    const artifactId = stringOf(document.artifact_id);
    const version = numberOf(document.version);
    if (artifactId === null || version === null || !Number.isSafeInteger(version)) {
      return deviation("the request names a document with no artifact id or version", judged);
    }
    documents.push({ artifact_id: artifactId, name: document.name, version });
  }
  return {
    state: "ready",
    request,
    judged,
    judgedAt,
    answerEventId: judging.answerEventId,
    payload,
    issue: { key: issueKey, ...step.fields } as IssueAsOf,
    issueAsOfEventId: step.eventId,
    documents,
    queries: { title: firstCharacters(title), question: firstCharacters(question) },
  };
}

/**
 * One document version, as `replay.ts` reads it: `GET /api/v1/artifacts/{id}/versions/{v}`, whose
 * markdown must hash to the sha256 the route answers. A refusal or a mismatch is why the request
 * is a deviation.
 */
async function documentOf(
  document: Ready["documents"][number],
  config: DispatchConfig
): Promise<RequestDocument | string> {
  const path =
    `/api/v1/artifacts/${encodeURIComponent(document.artifact_id)}` +
    `/versions/${document.version}`;
  let body: unknown;
  try {
    body = await readJson<unknown>(path, config);
  } catch (error) {
    if (!(error instanceof DispatchRefusedError)) throw error;
    return `${error.status} on GET ${path}`;
  }
  const version = recordOf(body);
  const markdown = stringOf(version?.markdown);
  const sha256 = stringOf(version?.sha256);
  if (markdown === null || sha256 === null) return `GET ${path} answered no markdown or no sha256`;
  if (sha256Of(markdown) !== sha256) {
    return `GET ${path} answered markdown whose sha256 is not the ${sha256} it names`;
  }
  return { ...document, markdown, sha256 };
}

/**
 * A request file already written by an earlier run of this claim run, read back for its sha256
 * and the counts the stage record sums, and refused where it is not this request at this moment.
 */
async function writtenRequest(
  path: string,
  ready: Ready
): Promise<{ sha256: string; held: number; reserveErrors: number; dropped: number } | null> {
  let bytes: Buffer;
  try {
    bytes = await readFile(path);
  } catch {
    return null;
  }
  let parsed: Record<string, unknown> | null;
  try {
    parsed = recordOf(JSON.parse(bytes.toString("utf8")));
  } catch {
    parsed = null;
  }
  const judged = recordOf(parsed?.judged);
  if (parsed?.askId !== ready.request.askId || judged?.eventId !== ready.judged.eventId) {
    throw new S10InputsError(
      `${path} is not the request this run judges at event ${ready.judged.eventId}`
    );
  }
  const counts = recordOf(parsed.counts);
  return {
    sha256: sha256Of(bytes),
    held: numberOf(counts?.held) ?? 0,
    reserveErrors: numberOf(counts?.reserveErrors) ?? 0,
    dropped: numberOf(counts?.droppedLaterEvent) ?? 0,
  };
}

/** The file of one request, by its ask id, which must be a name and nothing more. */
function requestPath(directory: string, askId: string): string {
  if (!/^[A-Za-z0-9-]+$/.test(askId)) {
    throw new S10InputsError(`the ask id ${askId} cannot name a file of its own`);
  }
  return join(directory, REQUESTS_DIRECTORY, `${askId}.json`);
}

async function inputs(flags: Flags, options: InputsOptions): Promise<number> {
  // The decision point 4 (d) names is checked first, so a checkout whose reserve-ask.ts is not
  // the registered one reads nothing.
  const decide = options.decide ?? (await reserveAskDecision());
  const directory = await outDirectory(flags.out);
  const root = options.root ?? REPOSITORY_ROOT;
  const corpus = await readCorpus(flags.corpus, { registration: options.registration, root });
  const reference = corpus.reserve.readUnder;
  const claimRun = corpus.snapshot.reserve?.claimRun;
  if (reference === null || claimRun === undefined) {
    throw new S10InputsError(
      "the claim's inputs are written only under its registered read, by the claim run its " +
        `build recorded: set ${RESERVE_VARIABLE} and ${CLAIM_RUN_VARIABLE} as that build had them`
    );
  }
  const draw = await claimDrawOf(resolve(flags.draw), corpus, reference, claimRun);
  const chain: StageChain = {
    claimRun,
    reference,
    runDigest: await runDigestOf({ argv: process.argv, env: process.env }, root),
    person: corpus.snapshot.person,
    throughEventId: corpus.snapshot.throughEventId,
    build: { directory: corpus.directory, snapshotSha256: corpus.snapshotSha256 },
    draw: {
      directory: draw.directory,
      drawSha256: draw.drawSha256,
      slicesSha256: draw.slicesSha256,
    },
    reserveAsk: options.decide === undefined ? RESERVE_ASK_SHA256 : null,
  };
  const inventory = inventoryOf(corpus.byCaseId, draw.trees, draw.report);
  const { startedAt, dropScanThrough: recorded } = await claimDirectory(directory, chain);
  console.log(`inputs            ${directory}`);
  console.log(`claim run         ${claimRun}, under ${reference}`);
  console.log(`requests          ${inventory.length} in the inventory`);
  if (inventory.length === 0) {
    await writeWhole(join(directory, INVENTORY_FILE), "");
    await writeStage(directory, {
      ...emptyStage(chain, startedAt),
      dropScanThrough: recorded,
      state: "written",
      finishedAt: new Date().toISOString(),
      inventorySha256: sha256Of(""),
    });
    console.error("no request: the draw's report trees hold no approval case");
    return 1;
  }

  const log = new InputsLog(corpus.snapshot.person, inventory.map((request) => request.askId));
  const walk = await pageEventLog({
    through: corpus.snapshot.throughEventId,
    onPage: (events) => {
      for (const event of events) log.note(event);
    },
  });
  if (corpus.snapshot.eventsRead > 0 && walk.eventsRead !== corpus.snapshot.eventsRead) {
    throw new S10InputsError(
      `the log through event ${corpus.snapshot.throughEventId} reads ${walk.eventsRead} events, ` +
        `and the build read ${corpus.snapshot.eventsRead} there: it is not the log the build read`
    );
  }
  console.log(
    `log               ${walk.eventsRead} events through ${corpus.snapshot.throughEventId}`
  );

  const config = await dispatchConfig();
  const plans = inventory.map((request) => planOf(request, log));
  const lines = new Map<string, InventoryLine>();
  // A request's line: ready with its file's sha256, or a deviation with its reason, which a
  // request whose own inputs stood takes from a document or a candidate read after them.
  const lineOf = (
    plan: Ready | Deviation,
    sha256: string | null,
    reason: string | null = plan.state === "deviation" ? plan.reason : null
  ): InventoryLine => ({
    askId: plan.request.askId,
    issueKey: plan.request.issueKey,
    group: plan.request.group,
    cases: plan.request.rows.length,
    judged: plan.judged,
    state: reason === null ? "ready" : "deviation",
    reason,
    requestSha256: sha256,
  });
  const requests: Record<string, string> = {};
  const pending: Ready[] = [];
  let droppedLaterEvent = 0;
  let heldCount = 0;
  let reserveErrors = 0;
  for (const plan of plans) {
    if (plan.state === "deviation") {
      lines.set(plan.request.askId, lineOf(plan, null));
      continue;
    }
    const earlier = await writtenRequest(requestPath(directory, plan.request.askId), plan);
    if (earlier === null) {
      pending.push(plan);
      continue;
    }
    // Written by an earlier run of this claim run: never written again.
    requests[plan.request.askId] = earlier.sha256;
    lines.set(plan.request.askId, lineOf(plan, earlier.sha256));
    droppedLaterEvent += earlier.dropped;
    heldCount += earlier.held;
    reserveErrors += earlier.reserveErrors;
  }

  // Each document version each request names, read once.
  const versions = new Map<string, RequestDocument | string>();
  const wanted = new Map<string, Ready["documents"][number]>();
  for (const plan of pending) {
    for (const document of plan.documents) {
      wanted.set(`${document.artifact_id}@${document.version}`, document);
    }
  }
  await pool([...wanted.entries()], DOCUMENT_CONCURRENCY, async ([key, document]) => {
    versions.set(key, await documentOf(document, config));
  });

  // S10's two queries of each request, each query once, at this run's start.
  await mkdir(join(directory, PAGES_DIRECTORY), { recursive: true });
  await mkdir(join(directory, REQUESTS_DIRECTORY), { recursive: true });
  const searches = new Searches(
    join(directory, PAGES_DIRECTORY),
    config,
    await archivedPages(join(directory, PAGES_DIRECTORY))
  );
  const searchable: Array<{ plan: Ready; documents: RequestDocument[] }> = [];
  for (const plan of pending) {
    const documents: RequestDocument[] = [];
    let unread: string | null = null;
    for (const document of plan.documents) {
      const read = versions.get(`${document.artifact_id}@${document.version}`);
      if (read === undefined || typeof read === "string") {
        unread = read ?? "the document was not read";
        break;
      }
      documents.push(read);
    }
    if (unread !== null) {
      lines.set(plan.request.askId, lineOf(plan, null, `a document: ${unread}`));
      continue;
    }
    searchable.push({ plan, documents });
  }
  const queries = new Set<string>();
  for (const { plan } of searchable) {
    queries.add(plan.queries.title);
    queries.add(plan.queries.question);
  }
  const answered = new Map<string, Array<{ offset: number; page: PageRecord }>>();
  await pool([...queries], SEARCH_CONCURRENCY, async (query) => {
    answered.set(query, await searches.pagesOf(query));
  });
  console.log(`queries           ${queries.size}, ${searches.read} pages read in this run`);

  // The drop scan, once the searches are done: an event of a candidate's ask between the pin and
  // the searches drops it too. Its last event is recorded before any request file is written, so
  // a resume reads through the same event and drops the same candidates.
  let dropScanThrough = recorded;
  let afterPin = new Set<string>();
  if (searchable.length > 0) {
    const scan = await dropScanOf(corpus.snapshot.throughEventId, recorded);
    afterPin = scan.asks;
    dropScanThrough = scan.through;
    if (recorded === null) {
      await writeStage(directory, { ...emptyStage(chain, startedAt), dropScanThrough });
    }
    console.log(
      `drop scan         events ${corpus.snapshot.throughEventId} to ${dropScanThrough}: ` +
        `${counted(afterPin.size, "ask")} with an event after the pin`
    );
  }

  // Every ask among the hits, decided under the reserve before any field of it is looked up.
  const candidates = new Set<string>();
  const searchesOf = (plan: Ready): { title: QueryInputs; question: QueryInputs } => ({
    title: queryInputsOf(plan.queries.title, answered.get(plan.queries.title) ?? []),
    question: queryInputsOf(plan.queries.question, answered.get(plan.queries.question) ?? []),
  });
  for (const { plan } of searchable) {
    const search = searchesOf(plan);
    for (const hit of [...search.title.hits, ...search.question.hits]) candidates.add(hit.id);
  }
  const verdicts = new Map<string, ReserveVerdict>();
  await pool([...candidates], DECISION_CONCURRENCY, async (askId) => {
    verdicts.set(askId, await decide(askId));
  });
  console.log(`candidates        ${candidates.size} decided by reserve-ask.ts`);

  for (const { plan, documents } of searchable) {
    const search = searchesOf(plan);
    const outcomes = new Map<string, CandidateInputs>();
    const counts: CandidateCounts = {
      askHits: search.title.hits.length + search.question.hits.length,
      candidates: 0,
      held: 0,
      reserveErrors: 0,
      droppedLaterEvent: 0,
      kept: 0,
    };
    let deviation: string | null = null;
    for (const hit of [...search.title.hits, ...search.question.hits]) {
      if (outcomes.has(hit.id)) continue;
      const verdict = verdicts.get(hit.id) as ReserveVerdict;
      if (verdict !== "kept") {
        outcomes.set(hit.id, { held: true, reserve: verdict });
        counts.held += 1;
        if (verdict === "error") counts.reserveErrors += 1;
        continue;
      }
      const outcome = candidateOf(hit.id, log, plan.judged.eventId, afterPin);
      if ("deviation" in outcome) {
        deviation ??= outcome.deviation;
        continue;
      }
      outcomes.set(hit.id, outcome);
      if ("dropped" in outcome) counts.droppedLaterEvent += 1;
      else counts.kept += 1;
    }
    counts.candidates = outcomes.size;
    if (deviation !== null) {
      lines.set(plan.request.askId, lineOf(plan, null, `a candidate: ${deviation}`));
      continue;
    }
    const request: RequestInputs = {
      version: INPUTS_VERSION,
      askId: plan.request.askId,
      issueKey: plan.issue.key,
      group: plan.request.group,
      cases: plan.request.rows.length,
      judged: plan.judged,
      ask: plan.payload,
      issue: plan.issue,
      issueAsOfEventId: plan.issueAsOfEventId,
      documents,
      search,
      candidates: Object.fromEntries(outcomes),
      counts,
    };
    let text: string;
    try {
      text = assertNoAnswer(request, {
        askId: plan.request.askId,
        answerEventId: plan.answerEventId,
        selected: plan.request.rows.map((row) => row.selected),
        judgedAt: plan.judgedAt,
      });
    } catch (error) {
      if (!(error instanceof LeakError)) throw error;
      // Inputs that would carry the answer are not the request as it stood: the file is not
      // written, and the reason names the rule and none of the text that broke it.
      lines.set(plan.request.askId, lineOf(plan, null, "its inputs would carry the answer"));
      continue;
    }
    await writeWhole(requestPath(directory, plan.request.askId), text);
    const sha256 = sha256Of(text);
    requests[plan.request.askId] = sha256;
    lines.set(plan.request.askId, lineOf(plan, sha256));
    droppedLaterEvent += counts.droppedLaterEvent;
    heldCount += counts.held;
    reserveErrors += counts.reserveErrors;
  }

  const ordered = inventory.map((request) => lines.get(request.askId) as InventoryLine);
  const inventoryText = ordered.map((line) => `${JSON.stringify(line)}\n`).join("");
  await writeWhole(join(directory, INVENTORY_FILE), inventoryText);
  const ready = ordered.filter((line) => line.state === "ready").length;
  await writeStage(directory, {
    ...emptyStage(chain, startedAt),
    dropScanThrough,
    state: "written",
    finishedAt: new Date().toISOString(),
    counts: {
      requests: ordered.length,
      ready,
      deviations: ordered.length - ready,
      cases: inventory.reduce((total, request) => total + request.rows.length, 0),
      queries: queries.size,
      pages: searches.all().length,
      candidates: candidates.size,
      held: heldCount,
      reserveErrors,
      droppedLaterEvent,
    },
    inventorySha256: sha256Of(inventoryText),
    requests,
    pages: searches.all(),
  });
  console.log(
    `written           ${counted(ready, "request")} ready, ` +
      `${counted(ordered.length - ready, "deviation")}; ${droppedLaterEvent} candidates dropped ` +
      `by an event after the judged moment, ${heldCount} held (${reserveErrors} undecided)`
  );
  for (const line of ordered) {
    if (line.state === "deviation") console.log(`  deviation ${line.askId}: ${line.reason}`);
  }
  console.log(`                  ${join(directory, INVENTORY_FILE)}`);
  console.log(`                  ${join(directory, STAGE_FILE)}`);
  return 0;
}

/** The command: the flags, the inputs, and the exit status it takes. */
export async function main(
  argv: readonly string[],
  options: InputsOptions = {}
): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  try {
    return await inputs(flags, options);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports `inventoryOf` must not write any inputs.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
