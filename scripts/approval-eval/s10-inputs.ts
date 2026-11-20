import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
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
import { TREES_FILE } from "./trees";

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
 * answer, which is placed as S10's own `first_answer` places it (point 7): the first event after
 * the ask's opening, in the log's order, whose actor is the person and that is either an
 * `ask.answered` of the ask, whatever answer it carries, or, for an ask that names one document,
 * an `artifact.approved` or `artifact.changes_requested` on that document at the `version` the ask
 * names. An ask that names several documents is answered by its `ask.answered` only. Where that
 * first event is an `ask.answered` whose answer is not the person's own (a restored answer of
 * another user, or none), S10's first answer holds no verdict, and the request is a deviation,
 * judged at no moment.
 *
 * The payload of the judged event is the request, as the log keeps it, with what the log's route
 * adds when it is read taken out. `GET /api/v1/events/log` serves each ask event's payload as the
 * write stored it (its `id`, `kind`, `state`, `issue_key`, `question`, `options`, `multiple`,
 * `urgency`, `approval`, `answer`, `asked_of`, `created_at`, `edited_at`, `previous` and the rest
 * of what the write recorded) and adds, at read time, facts of the ask as it stands now
 * (`attachAskEventFields` in `packages/envoy/internal/dispatch/api/events.go`): `opened_event_id`,
 * `referenced_by_count`, `revision`, the part `id` of each option and of the answer, and
 * `anchor_artifact` and `approval_artifact`, the document's current slug, name and primary flag.
 * Each of those is taken out of the judged payload and of every candidate's fields; S10 reads none
 * of them but `approval_artifact.primary`, which its `legion_fact` reads, so `approval_artifact`
 * is written again as `{ primary }` for the approval's own `artifact_id`, from the document's
 * creation record in the log: an issue's primary document is made with the issue, and its
 * `issue.created` names it as `primary_artifact_id`, and every other document's `artifact.created`
 * records `primary: false`. A document's primary flag is set when it is made and never changed
 * (`store/migrations/0140_issue_embedding_spec.up.sql`), so its creation record is the flag as of
 * any later moment; a request whose document the log records no creation of by the judged moment
 * is a deviation. The issue is its `title`, `status`, `labels`, `parent` and `primary_artifact_id`
 * as the log's `issue.` events set them up to that moment (`issue.created`, `issue.updated` and
 * `issue.closed` carry the whole issue, `issue.claimed` and `issue.released` its status), with the
 * id of the last of them applied; and each document the payload names, as S10's `ask_docs` names
 * them (`approval.documents` whenever Python takes that list for true, every entry of it, or the
 * approval's own document where it is absent or empty: `askDocsOf`), is the `markdown` of its
 * immutable `version`, checked against the sha256 the version route answers, with the
 * `artifact_id`, `name` and `version` the request's own payload gives. A field S10 reads of the
 * request (its question, its first opening, its issue, each document's id, name and version, an
 * entry of `approval.documents` that is no document, and each issue field `legion_fact` reads)
 * that the log cannot give as of the moment makes the request a deviation, naming the field. The
 * answer rule's one-document test reads the same list.
 *
 * The fact check. S10's `prepare` reads `legion_fact` over the request and its issue before any
 * neighbour, and a fact that holds (the primary document of a root issue carrying the `legion`
 * label, waiting on the backlog or in icebox) ends the request there: S10 decides it
 * never-decide, reading each document's version and no neighbour. So the fact is read here as
 * `legion_fact` reads it (`legionFactOf`), over the request as written and its issue as of the
 * moment, before any search: every request file carries it (`fact`, its five fields, and
 * `factHold`), and a request it holds is searched for nothing, none of its hits is decided, and
 * its file has its documents and no `search`, `candidates` or `counts`; no neighbour can make it a
 * deviation. `s10_judge.py` runs the frozen `legion_fact` over the same file, and a fact it reads
 * otherwise than `factHold` is a deviation.
 *
 * His answer is placed and never written: no verdict, option, words or answer time of it, and no
 * id of its event, is in any file this writes. The analysis places it again over its own walk of
 * the log with the rule here (`InputsLog`), so the two stages read one answer. A payload at the
 * judged moment that carries an `answer`, or an `approval.approved` that is not before the
 * moment, makes the request a deviation, and every request file is held, before it is written and
 * again whenever a resume takes it, to `assertNoAnswer`: no case id of the ask, no event id that
 * is the answer's, no `answered_at` at or after the moment, and no `selected` of the person's in
 * the request's own part. Inputs that break it are not the request as it stood, so the request is
 * a deviation and no file is written.
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
 * not ask about and which the reserve keeps. Until then no field of a candidate has been read:
 * the first walk of the log keeps, of an ask outside the inventory, only the id of its last event,
 * and the candidates' fields are read by a second walk that reads them only for the asks the
 * reserve kept, so a held candidate's fields are never read, kept or written. Any event of a
 * kept candidate's ask after the moment drops it (`later-event`), and the next hit is S10's to
 * take: an event of an ask is an `ask.` event whose payload's `id` is the ask, or any event whose
 * payload's `ask_id` is (its comments, deliveries, followers and document settlements). An ask
 * with no event in the log through the pin opened after it, so after the moment, and is dropped
 * too, and so is one with an event after the pin (below). Otherwise its fields are its last whole
 * payload's, which no event after the moment changed, and its issue's title as of the moment.
 * S10's `neighbours` reads a candidate's fields one after another, each only where the ones
 * before it let the candidate through (its state and its answer's user; its issue; its answer's
 * time; its kind; its documents' ids, names and versions, its answer's selected options and
 * words, its question and its issue's title), taking the hits in its own order, by rank, and
 * stopping at its eighth neighbour (`neighbourFault`); the first of those reads the log cannot
 * give as of the moment makes the request a deviation, naming the candidate and the field. The
 * filters themselves are S10's own code's, run over these fields. Each inventory line carries its
 * request's counts: the asks held, the asks `reserve-ask.ts` decided nothing on, those dropped by
 * an event after the moment, by where the event was seen, and those kept.
 *
 * The walks. The log is walked twice, each time through the build's pin, and each walk is refused
 * unless it reads as many events as the build read. The first keeps each ask's last event id, each
 * issue's five fields at each event that set one, each document's creation record, and each
 * inventory ask's statements up to its answer: not the whole log, and no field of any other ask.
 * The second, after the reserve's decisions, keeps the last whole payload of each candidate the
 * reserve kept. Every input is as of the judged moment from those walks alone. The searches run
 * after the pin, and an event between the pin and them can move a rank too, so once the searches
 * are done the log is read on from the pin to its newest event at that time, the drop scan, and
 * of each of those events nothing is read but the ask it is an event of: a candidate with any is
 * dropped (`later-event`, seen `after-pin`). The scan's last event id is recorded
 * (`dropScanThrough`) before any request file is written, and a resume scans through that id and
 * never further, so it drops the same candidates its first run did.
 *
 * The stage record. `s10-inputs.json` records the claim run, the read it is under, this run's
 * digest, the person and the pin, the sha256 of each of the build's `snapshot.json`,
 * `corpus.jsonl` and `trees.json` and of the draw's `draw.json` and `slices.json`, so the build,
 * the draw and these inputs are one chain and no directory, and no file of one, can be put in
 * another's place between stages: the draw's own record names the claim run and the sha256 of the
 * build's three files, and the draw is refused unless they are this run's and those files' bytes
 * now. It names each request file and each page record with the sha256 of its bytes: a file is
 * written beside its place, the record naming it is written, and only then is the file moved into
 * its place, so the record is what makes a file this run's. It is written again once the drop
 * scan is done, with `dropScanThrough`, and when the run ends, with the counts, which are the sums
 * of the inventory's lines, recomputed on every run, and the sha256 of `inventory.jsonl`. A run
 * into an `--out` holding another run's record, or files of no record, is refused; a run into its
 * own claim run's directory resumes it, and takes only what its record names: every file the
 * record names must be there with the bytes it names, and every request file must still be the
 * request this run judges at that moment and pass `assertNoAnswer` again, or the run is refused,
 * and so is any file the record does not name. A file written beside its place whose bytes the
 * record names is moved into it, and any other is removed unread. A resume takes each page record
 * rather than ask that page again, takes the recorded `dropScanThrough`, never writes a request
 * file again, and works anew only on the requests with no file, the deviations among them.
 */

const USAGE = `usage: bun scripts/approval-eval/s10-inputs.ts --corpus <build dir> \\
  --draw <draw dir> --out <dir>

Writes the inputs S10 judges each approval request of the claim's final test on, each read as it
stood at the request's last opening or edit before the person's answer, and no part of that
answer: the inventory, one file of inputs per request, and a record of each search page S10's
neighbours read. Runs only under the claim run's registered read, over the build and the draw
that claim run made.

Options:
  --corpus <dir>  Required. The claim run's build: snapshot.json, corpus.jsonl and trees.json.
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

/** How many neighbours S10's `neighbours` keeps before it stops reading candidates. */
const NEIGHBOURS_KEPT = 8;

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
export const INPUTS_VERSION = 2;

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

/**
 * The fields `GET /api/v1/events/log` adds to an ask event's payload when it is read, as the ask
 * stands then (`attachAskEventFields`): none is the ask as the write recorded it. The part `id`
 * it adds to each option and to the answer is taken out by `writeTimeAsk` too.
 */
export const READ_TIME_FIELDS = [
  "opened_event_id",
  "referenced_by_count",
  "revision",
  "anchor_artifact",
  "approval_artifact",
] as const;

/** Where the event that drops a candidate was seen. */
export type DropSeen = "log" | "no-event-through-pin" | "after-pin";

/** Those places, in the order a count of them is written. */
export const DROP_SEEN: readonly DropSeen[] = ["log", "no-event-through-pin", "after-pin"];

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

/** What a request's candidates came to. */
export interface CandidateCounts {
  /** Ask hits over both queries, a hit both answered counted twice. */
  askHits: number;
  /** The distinct asks among them. */
  candidates: number;
  held: number;
  /** Of the held, the asks `reserve-ask.ts` decided nothing on. */
  reserveErrors: number;
  /** Dropped by point 4 (b), an event of the ask after the judged moment, by where it was seen. */
  droppedLaterEvent: Record<DropSeen, number>;
  /** Written with their fields as of the judged moment. */
  kept: number;
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
  /**
   * The request's candidates, a deviation's too; null for one that stopped before its search, and
   * for one its fact holds, which S10 searches for nothing.
   */
  counts: CandidateCounts | null;
}

/** The issue of a request as of its judged moment, in Dispatch's own field names. */
export type IssueAsOf = { key: string } & Record<IssueField, unknown>;

/** One document an approval names, as S10's `ask_docs` reads it. */
export interface AskDocument {
  artifact_id: string;
  name: string;
  version: number;
}

/** One document a request names, at the version it names, read from that version's route. */
export interface RequestDocument extends AskDocument {
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

/** A candidate's ask, in the field names of `GET /api/v1/asks/{id}`'s `ask`, as the write was. */
export type CandidateAsk = Partial<Record<(typeof CANDIDATE_FIELDS)[number], unknown>>;

/** A candidate as of the judged moment. */
export interface KeptCandidate {
  /** The event whose payload the fields are, and null where the log holds no whole payload. */
  asOfEventId: number | null;
  ask: CandidateAsk;
  /** Its issue, with the title as of the moment, null where the log gives none. */
  issue: { key: string; title: string | null } | null;
}

/** What one ask among the hits is to S10's neighbours, as of the judged moment. */
export type CandidateInputs =
  | { held: true; reserve: "held" | "error" }
  | { dropped: "later-event"; seen: DropSeen }
  | KeptCandidate;

/** `requests/<ask id>.json`: one request's inputs as of its judged moment. */
export interface RequestInputs {
  version: number;
  askId: string;
  issueKey: string;
  group: string;
  cases: number;
  judged: JudgedMoment;
  /**
   * The payload of the judged event as the write stored it (`writeTimeAsk`), with
   * `approval_artifact` written as `{ primary }` from the document's creation record.
   */
  ask: Record<string, unknown>;
  issue: IssueAsOf;
  /** The last `issue.` event applied to `issue`. */
  issueAsOfEventId: number;
  documents: RequestDocument[];
  /** S10's `legion_fact` over `ask` and `issue` (`legionFactOf`); `s10_judge.py` must agree. */
  fact: LegionFact;
  /**
   * Whether the fact holds: S10 then decides the request never-decide before any neighbour is
   * read, so a held request has no `search`, `candidates` or `counts` (each null).
   */
  factHold: boolean;
  search: { title: QueryInputs; question: QueryInputs } | null;
  candidates: Record<string, CandidateInputs> | null;
  counts: CandidateCounts | null;
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
  build: {
    directory: string;
    snapshotSha256: string;
    corpusSha256: string;
    treesSha256: string;
  };
  draw: { directory: string; drawSha256: string; slicesSha256: string };
  /** The sha256 of the `reserve-ask.ts` that decided, and null where a test decided. */
  reserveAsk: string | null;
}

/** The stage's counts: the inventory's, and the sums of its lines' candidate counts. */
export interface StageCounts extends CandidateCounts {
  requests: number;
  ready: number;
  deviations: number;
  cases: number;
  /** The page records `pages/` holds. */
  pages: number;
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
  counts: StageCounts | null;
  inventorySha256: string | null;
  /** The sha256 of each request file's bytes, by ask id. */
  requests: Record<string, string>;
  /** The sha256 of each page record file's bytes, by the sha256 of the page it records. */
  pages: Record<string, string>;
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
 * Whether Python reads a value as false, which is what its `x or y` passes over: none, false, a
 * zero, and an empty text, list or object.
 */
function falsy(value: unknown): boolean {
  if (value === undefined || value === null || value === false || value === 0 || value === "") {
    return true;
  }
  if (Array.isArray(value)) return value.length === 0;
  const record = recordOf(value);
  return record !== null && Object.keys(record).length === 0;
}

/**
 * The documents an approval names as S10's `ask_docs` reads them, each with an `artifact_id`, a
 * `name` and a `version`, or the first place where it reads one the approval does not give.
 * `ask_docs` reads `approval.documents` whenever Python takes it for true, every entry of it,
 * and falls back to the approval's own document only where that list is absent or empty, and to
 * none where there is no approval: `ap.get("documents") or ([...] if ap else [])`. So a list
 * holding an entry that is no document, or naming no document field, gives no documents at all,
 * the place of that entry named, and never the approval's own document in their stead.
 */
export function askDocsOf(approval: unknown): { documents: AskDocument[] } | { missing: string } {
  if (falsy(approval)) return { documents: [] };
  const record = recordOf(approval);
  if (record === null) return { missing: "approval" };
  const listed = record.documents;
  if (!falsy(listed) && !Array.isArray(listed)) return { missing: "approval.documents" };
  const entries: Array<[string, unknown]> =
    Array.isArray(listed) && listed.length > 0
      ? listed.map((entry, at): [string, unknown] => [`approval.documents[${at}]`, entry])
      : [["approval", record]];
  const documents: AskDocument[] = [];
  for (const [where, entry] of entries) {
    const document = recordOf(entry);
    if (document === null) return { missing: where };
    const artifactId = stringOf(document.artifact_id);
    const name = stringOf(document.name);
    const version = numberOf(document.version);
    if (artifactId === null) return { missing: `${where}.artifact_id` };
    if (name === null) return { missing: `${where}.name` };
    if (version === null || !Number.isSafeInteger(version)) return { missing: `${where}.version` };
    documents.push({ artifact_id: artifactId, name, version });
  }
  return { documents };
}

/** S10's `legion_fact` over a request and its issue, field for field as it returns them. */
export interface LegionFact {
  holds: boolean;
  legion: boolean;
  primary: boolean;
  root: boolean;
  status: unknown;
}

/**
 * An issue's parent as S10's `parent_key` reads it, and null where it reads none: a key written
 * as text, or an object's `key`, `issue_key` or `id`, the first Python takes for true, or else
 * its `id`, whatever that holds. `undefined` where `parent_key` cannot read the value at all.
 */
export function parentKeyOf(parent: unknown): unknown {
  if (falsy(parent)) return null;
  if (typeof parent === "string") return parent;
  const record = recordOf(parent);
  if (record === null) return undefined;
  for (const field of ["key", "issue_key"]) {
    if (!falsy(record[field])) return record[field];
  }
  return record.id ?? null;
}

/**
 * S10's `legion_fact` over the request as this stage writes it and its issue as of the judged
 * moment, mirrored exactly, or null where Python would stop on a value it cannot read (a parent
 * `parent_key` cannot read, labels `in` cannot search). It holds for the primary document of a
 * root issue carrying the `legion` label that waits on the backlog or in icebox: S10 decides
 * that request never-decide without its judges or its neighbours.
 */
export function legionFactOf(
  ask: Readonly<Record<string, unknown>>,
  issue: Readonly<Record<string, unknown>>
): LegionFact | null {
  const artifact = recordOf(ask.approval_artifact) ?? {};
  const approved = recordOf(ask.approval)?.artifact_id ?? null;
  const primary = !falsy(artifact.primary) || approved === (issue.primary_artifact_id ?? null);
  const parent = parentKeyOf(issue.parent);
  if (parent === undefined) return null;
  const labels = falsy(issue.labels) ? [] : issue.labels;
  let legion: boolean;
  if (Array.isArray(labels)) legion = labels.includes("legion");
  else if (typeof labels === "string") legion = labels.includes("legion");
  else if (recordOf(labels) !== null) legion = Object.hasOwn(labels as object, "legion");
  else return null;
  const root = parent === null;
  const status = issue.status ?? null;
  const holds = root && legion && primary && (status === "backlog" || status === "icebox");
  return { holds, legion, primary, root, status };
}

/**
 * An ask event's payload as the write stored it: the fields `GET /api/v1/events/log` adds when it
 * is read (`READ_TIME_FIELDS`) taken out, and the part `id` it adds to each option and to the
 * answer taken out of them.
 */
export function writeTimeAsk(payload: Record<string, unknown>): Record<string, unknown> {
  const readTime: readonly string[] = READ_TIME_FIELDS;
  const withoutId = (value: unknown): unknown => {
    const record = recordOf(value);
    if (record === null) return value;
    return Object.fromEntries(Object.entries(record).filter(([field]) => field !== "id"));
  };
  const ask: Record<string, unknown> = {};
  for (const [field, value] of Object.entries(payload)) {
    if (readTime.includes(field)) continue;
    if (field === "options" && Array.isArray(value)) ask.options = value.map(withoutId);
    else if (field === "answer") ask.answer = withoutId(value);
    else ask[field] = value;
  }
  return ask;
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

/** An issue's fields as one event left them. */
interface IssueStep {
  eventId: number;
  fields: Readonly<Partial<Record<IssueField, unknown>>>;
}

/** The person's first answer event on an ask, and whether it carries the person's own answer. */
export interface FirstAnswer {
  eventId: number;
  own: boolean;
}

/** Where one request of the inventory stands in the walk. */
export interface Judging {
  /** The id of the ask's first `ask.opened`. */
  firstOpened: number | null;
  /** Its last `ask.opened` or `ask.edited` so far, while no answer of the person has come. */
  statement: DispatchEvent | null;
  /** The person's first answer event on the ask, as S10's `first_answer` places it. */
  answer: FirstAnswer | null;
  /** The statement that stood when that answer came: the judged moment. */
  judged: DispatchEvent | null;
}

/**
 * The first walk of the log, read for the inputs: each ask's last event id and nothing else of
 * an ask outside the inventory, each issue's fields at each event that set one, each document's
 * creation record, and the statements and first answer of each request of the inventory
 * (`Judging`). The walk hands the events in id order.
 */
export class InputsLog {
  private readonly person: string;
  private readonly lastEvents = new Map<string, number>();
  private readonly issues = new Map<string, IssueStep[]>();
  private readonly primaries = new Map<string, { eventId: number; primary: boolean }>();
  private readonly judging = new Map<string, Judging>();

  constructor(person: string, inventory: Iterable<string>) {
    this.person = person;
    for (const askId of inventory) {
      this.judging.set(askId, { firstOpened: null, statement: null, answer: null, judged: null });
    }
  }

  /** Reads one event. */
  note(event: DispatchEvent): void {
    if (event.type.startsWith("issue.")) this.noteIssue(event);
    this.noteCreation(event);
    for (const askId of askIdsOf(event)) this.lastEvents.set(askId, event.id);
    const own = event.type.startsWith("ask.") ? stringOf(event.payload.id) : null;
    if (own !== null) this.noteStatement(own, event);
    if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
      this.noteSettlement(event);
    }
  }

  /** Where one request of the inventory stands. */
  judgingOf(askId: string): Judging | undefined {
    return this.judging.get(askId);
  }

  /** The id of an ask's last event through the pin, and none where the log holds no event of it. */
  lastEventOf(askId: string): number | undefined {
    return this.lastEvents.get(askId);
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

  /**
   * Whether a document is its issue's primary one, from its creation record at or before an
   * event, and null where the log records no creation of it by then.
   */
  primaryAsOf(artifactId: string, eventId: number): boolean | null {
    const made = this.primaries.get(artifactId);
    return made === undefined || made.eventId > eventId ? null : made.primary;
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

  /**
   * A document's creation record: an issue's primary document, made with the issue and named by
   * its `issue.created`, or any other document, whose `artifact.created` records its flag.
   */
  private noteCreation(event: DispatchEvent): void {
    let made: { id: string | null; primary: boolean } | null = null;
    if (event.type === "issue.created") {
      made = { id: stringOf(event.payload.primary_artifact_id), primary: true };
    } else if (event.type === "artifact.created") {
      const artifact = recordOf(event.payload.artifact);
      const primary = artifact?.primary;
      if (typeof primary === "boolean") made = { id: stringOf(artifact?.id), primary };
    }
    if (made === null || made.id === null || made.id === "" || this.primaries.has(made.id)) return;
    this.primaries.set(made.id, { eventId: event.id, primary: made.primary });
  }

  private noteStatement(askId: string, event: DispatchEvent): void {
    const judging = this.judging.get(askId);
    if (judging === undefined || judging.answer !== null) return;
    if (event.type === "ask.opened" || event.type === "ask.edited") {
      if (event.type === "ask.opened" && judging.firstOpened === null) {
        judging.firstOpened = event.id;
      }
      judging.statement = event;
      return;
    }
    if (event.type !== "ask.answered" || event.actor?.id !== this.person) return;
    if (judging.firstOpened === null || event.id <= judging.firstOpened) return;
    // S10's `first_answer` takes the person's first `ask.answered` whatever it carries, and its
    // `ask_answer` reads a verdict only from an answer the person gave.
    const answer = recordOf(event.payload.answer);
    const own = answer !== null && stringOf(answer.user) === this.person;
    judging.answer = { eventId: event.id, own };
    judging.judged = judging.statement;
  }

  /**
   * A document settlement by the person, which answers each request still waiting for its answer
   * that names that one document at that version and first opened before it: one document as
   * `ask_docs` reads them (`askDocsOf`), so a list `ask_docs` cannot read names none.
   */
  private noteSettlement(event: DispatchEvent): void {
    if (event.actor?.id !== this.person) return;
    const artifactId = stringOf(event.payload.artifact_id);
    const version = numberOf(event.payload.version);
    if (artifactId === null || version === null) return;
    for (const judging of this.judging.values()) {
      if (judging.answer !== null || judging.firstOpened === null) continue;
      if (event.id <= judging.firstOpened) continue;
      const read = askDocsOf(judging.statement?.payload.approval);
      const only = "documents" in read && read.documents.length === 1 ? read.documents[0] : null;
      if (only?.artifact_id !== artifactId || only.version !== version) continue;
      judging.answer = { eventId: event.id, own: true };
      judging.judged = judging.statement;
    }
  }
}

/**
 * The second walk of the log, after the reserve's decisions: the last whole payload of each ask
 * the reserve kept, as the write stored it, and nothing of any other ask. The walk hands the
 * events in id order.
 */
export class CandidateLog {
  private readonly kept: ReadonlySet<string>;
  private readonly latest = new Map<string, { eventId: number; fields: CandidateAsk }>();

  constructor(kept: ReadonlySet<string>) {
    this.kept = kept;
  }

  /** Reads one event. */
  note(event: DispatchEvent): void {
    if (!event.type.startsWith("ask.")) return;
    const askId = stringOf(event.payload.id);
    if (askId === null || !this.kept.has(askId)) return;
    // A whole ask, as its opening, edits, answers and resolution carry it; a follower names the
    // ask and carries none of it.
    if (!Object.hasOwn(event.payload, "state") || !Object.hasOwn(event.payload, "kind")) return;
    const ask = writeTimeAsk(event.payload);
    const fields: CandidateAsk = {};
    for (const field of CANDIDATE_FIELDS) {
      if (Object.hasOwn(ask, field)) fields[field] = ask[field];
    }
    this.latest.set(askId, { eventId: event.id, fields });
  }

  /** A kept ask's last whole payload through the pin, and none where the log holds none. */
  latestOf(askId: string): { eventId: number; fields: CandidateAsk } | undefined {
    return this.latest.get(askId);
  }
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
  const answer = recordOf(inputs.ask)?.answer;
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
  for (const document of listOf(inputs.documents)) {
    const markdown = stringOf(recordOf(document)?.markdown) ?? "";
    for (const line of markdown.split("\n")) {
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

/** The bytes of a file a stage before this one wrote, refused where it is not there. */
async function stageBytesOf(path: string, flag: string): Promise<Buffer> {
  try {
    return await readFile(path);
  } catch {
    throw new S10InputsError(`${path} could not be read: ${flag} takes the directory it wrote`);
  }
}

/** The claim's draw, as this run reads it, with the sha256 of each of its two files. */
interface ClaimDraw {
  directory: string;
  trees: readonly FinalTestTree[];
  report: readonly string[];
  drawSha256: string;
  slicesSha256: string;
  /** The sha256 of the build's `trees.json` now, which the draw's record names too. */
  treesSha256: string;
}

/**
 * The draw at `directory`, refused unless the claim run made it under this read from this build,
 * as its record says: the claim run it names, the sha256 of each of the build's `snapshot.json`,
 * `corpus.jsonl` and `trees.json`, each the bytes the build directory holds now, the read its
 * final test was drawn under, and that it applied no never-decide rule.
 */
async function claimDrawOf(
  directory: string,
  corpus: Corpus,
  reference: string,
  claimRun: string
): Promise<ClaimDraw> {
  const drawFile = drawPath(directory);
  const slicesFile = slicesPath(directory);
  const drawBytes = await stageBytesOf(drawFile, "--draw");
  const slicesBytes = await stageBytesOf(slicesFile, "--draw");
  const treesSha256 = sha256Of(await stageBytesOf(join(corpus.directory, TREES_FILE), "--corpus"));
  const facts = drawFactsOf(drawBytes.toString("utf8"), drawFile);
  if (facts.claimRun?.id !== claimRun) {
    throw new S10InputsError(
      `${drawFile} was drawn in the claim run ${facts.claimRun?.id ?? "of no record"}, and this ` +
        `run is ${claimRun}: a stage takes only its own claim run's draw`
    );
  }
  const build: Array<[string, string, string]> = [
    ["snapshot.json", facts.claimRun.snapshotSha256, corpus.snapshotSha256],
    ["corpus.jsonl", facts.claimRun.corpusSha256, corpus.corpusSha256],
    ["trees.json", facts.claimRun.treesSha256, treesSha256],
  ];
  for (const [file, drawn, now] of build) {
    if (drawn === now) continue;
    throw new S10InputsError(
      `${drawFile} was drawn from the build whose ${file} has sha256 ${drawn}, and ` +
        `${join(corpus.directory, file)} has ${now}: a stage takes only the build its draw read`
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
    treesSha256,
  };
}

/** The chain's fields, in the order a refusal names the first that differs. */
function chainDifference(found: Record<string, unknown>, chain: StageChain): string | null {
  const build = recordOf(found.build);
  const draw = recordOf(found.draw);
  const wanted: Array<[string, unknown, unknown]> = [
    ["claim run", found.claimRun, chain.claimRun],
    ["read", found.reference, chain.reference],
    ["run digest", found.runDigest, chain.runDigest],
    ["person", found.person, chain.person],
    ["pin", found.throughEventId, chain.throughEventId],
    ["snapshot.json", build?.snapshotSha256, chain.build.snapshotSha256],
    ["corpus.jsonl", build?.corpusSha256, chain.build.corpusSha256],
    ["trees.json", build?.treesSha256, chain.build.treesSha256],
    ["draw", draw?.drawSha256, chain.draw.drawSha256],
    ["slices", draw?.slicesSha256, chain.draw.slicesSha256],
    ["reserve-ask.ts", found.reserveAsk, chain.reserveAsk],
  ];
  for (const [what, was, is] of wanted) {
    if (was !== is) return `its ${what} is ${String(was)}, and this run's is ${String(is)}`;
  }
  return null;
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
    pages: {},
  };
}

/** The name a request file or a page record file is kept under: an ask id or a sha256. */
const FILE_KEY = /^[A-Za-z0-9-]+$/;

/** The files a stage record names under one of its two directories, refused in another shape. */
function recordedFiles(value: unknown, path: string): Record<string, string> {
  const record = recordOf(value);
  const files: Record<string, string> = {};
  for (const [key, sha256] of Object.entries(record ?? {})) {
    if (!FILE_KEY.test(key) || typeof sha256 !== "string" || !/^[0-9a-f]{64}$/.test(sha256)) {
      throw new S10InputsError(`${path} records its files in a shape this command never writes`);
    }
    files[key] = sha256;
  }
  if (record === null) {
    throw new S10InputsError(`${path} records its files in a shape this command never writes`);
  }
  return files;
}

/**
 * Holds one directory of the stage to its record, before a resume takes anything from it: a file
 * written beside its place whose bytes the record names is moved into its place, every other is
 * removed unread, and then every file there must be one the record names, with the bytes it
 * names, and every file it names must be there.
 */
async function holdToRecord(
  directory: string,
  recorded: Readonly<Record<string, string>>
): Promise<void> {
  const names = (await readdir(directory).catch((): string[] => [])).sort();
  for (const name of names) {
    if (!name.endsWith(".json.partial")) continue;
    const key = name.slice(0, -".json.partial".length);
    const partial = join(directory, name);
    const bytes = await readFile(partial);
    const committed = Object.hasOwn(recorded, key) && recorded[key] === sha256Of(bytes);
    if (committed) await rename(partial, join(directory, `${key}.json`));
    else await rm(partial, { force: true });
  }
  for (const name of await readdir(directory).catch((): string[] => [])) {
    const key = /^([A-Za-z0-9-]+)\.json$/.exec(name)?.[1];
    if (key === undefined || !Object.hasOwn(recorded, key)) {
      throw new S10InputsError(
        `${join(directory, name)} is a file the stage record does not name, so no run here can ` +
          "show it is its own"
      );
    }
  }
  for (const [key, sha256] of Object.entries(recorded)) {
    const path = join(directory, `${key}.json`);
    const bytes = await readFile(path).catch(() => null);
    if (bytes === null) {
      throw new S10InputsError(
        `the stage record names ${path}, which is not there: a resume never writes a file its ` +
          "record names again"
      );
    }
    if (sha256Of(bytes) !== sha256) {
      throw new S10InputsError(
        `${path} is not the file the stage record names: its sha256 is ${sha256Of(bytes)}, and ` +
          `the record names ${sha256}`
      );
    }
  }
}

/**
 * The stage record and the files it names, written through one writer, so each write of the
 * record holds every file committed before it. A file is committed by writing it beside its
 * place, writing the record that names it, and moving it into its place.
 */
class Stage {
  readonly directory: string;
  readonly record: StageRecord;
  private saving: Promise<void> = Promise.resolve();

  private constructor(directory: string, record: StageRecord) {
    this.directory = directory;
    this.record = record;
  }

  /**
   * Takes `directory` for this run: a directory holding nothing is started, with a stage record
   * naming the chain; one holding this chain's record is resumed, held to its record
   * (`holdToRecord`); anything else is refused.
   */
  static async open(directory: string, chain: StageChain): Promise<Stage> {
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
      const stage = new Stage(directory, emptyStage(chain, new Date().toISOString()));
      await stage.save();
      return stage;
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
    // A scan the earlier run recorded is the one this run takes: it reads through it, no further.
    const through = numberOf(found.dropScanThrough);
    if (found.dropScanThrough !== null && (through === null || through < chain.throughEventId)) {
      throw new S10InputsError(`${recordPath} records a drop scan through no event from the pin`);
    }
    const requests = recordedFiles(found.requests, recordPath);
    const pages = recordedFiles(found.pages, recordPath);
    const own = [STAGE_FILE, INVENTORY_FILE, REQUESTS_DIRECTORY, PAGES_DIRECTORY];
    for (const name of await readdir(directory)) {
      if (name.endsWith(".partial")) await rm(join(directory, name), { force: true });
      else if (!own.includes(name)) {
        throw new S10InputsError(
          `${join(directory, name)} is no file this command writes, so no run here can show it ` +
            "is its own"
        );
      }
    }
    await holdToRecord(join(directory, REQUESTS_DIRECTORY), requests);
    await holdToRecord(join(directory, PAGES_DIRECTORY), pages);
    const startedAt = stringOf(found.startedAt) ?? new Date().toISOString();
    return new Stage(directory, {
      ...emptyStage(chain, startedAt),
      dropScanThrough: through,
      requests,
      pages,
    });
  }

  /** Writes the record as it stands, after any write of it already under way. */
  save(): Promise<void> {
    this.saving = this.saving.then(() =>
      writeWhole(join(this.directory, STAGE_FILE), `${JSON.stringify(this.record, null, 2)}\n`)
    );
    return this.saving;
  }

  /** Commits one request file or page record file, and gives back the sha256 of its bytes. */
  async commit(kind: "requests" | "pages", key: string, text: string): Promise<string> {
    if (!FILE_KEY.test(key)) {
      throw new S10InputsError(`the ask id or page ${key} cannot name a file of its own`);
    }
    const under = kind === "requests" ? REQUESTS_DIRECTORY : PAGES_DIRECTORY;
    const directory = join(this.directory, under);
    await mkdir(directory, { recursive: true });
    const path = join(directory, `${key}.json`);
    const partial = `${path}.partial`;
    await writeFile(partial, text, "utf8");
    const sha256 = sha256Of(text);
    this.record[kind][key] = sha256;
    await this.save();
    await rename(partial, path);
    return sha256;
  }
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
 * The search, as S10 asks it and once for each query and offset: every page its record names is
 * taken, and every other page is read now, named by the sha256 of its bytes and committed to the
 * stage before it is used. A body that is no search answer stops the run.
 */
class Searches {
  private readonly stage: Stage;
  private readonly config: DispatchConfig;
  private readonly pages = new Map<string, PageRecord>();
  private readonly byRead = new Map<string, PageRecord>();
  private readonly writing = new Map<string, Promise<unknown>>();
  /** How many pages this run read, beside the ones it took from an earlier run. */
  read = 0;

  constructor(stage: Stage, config: DispatchConfig, pages: readonly PageRecord[]) {
    this.stage = stage;
    this.config = config;
    for (const page of pages) {
      this.pages.set(page.sha256, page);
      for (const read of page.reads) this.byRead.set(`${read.offset}\n${read.query}`, page);
    }
  }

  /** The page records `--out` holds, each held to the stage record already. */
  static async archived(stage: Stage): Promise<PageRecord[]> {
    const pages: PageRecord[] = [];
    for (const key of Object.keys(stage.record.pages).sort()) {
      const path = join(stage.directory, PAGES_DIRECTORY, `${key}.json`);
      const record = pageRecordOf(await readFile(path, "utf8"), path);
      if (record.sha256 !== key) {
        throw new S10InputsError(`${path} records the page ${record.sha256}, not the one it names`);
      }
      pages.push(record);
    }
    return pages;
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
    // One commit of a page at a time, so two reads answered by the same bytes are both recorded.
    const before = this.writing.get(sha256) ?? Promise.resolve();
    const next = before.then(() =>
      this.stage.commit("pages", sha256, `${JSON.stringify(page, null, 2)}\n`)
    );
    this.writing.set(sha256, next);
    await next;
    return page;
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
 * The ask hits of a request in the order S10's `neighbours` reads them: each ask once, at the
 * highest rank either query gave it, kept where it was first met, the title's pages before the
 * question's; then by rank, highest first, the order met breaking a tie.
 */
export function neighbourOrder(search: { title: QueryInputs; question: QueryInputs }): string[] {
  const best = new Map<string, number>();
  for (const hit of [...search.title.hits, ...search.question.hits]) {
    const rank = hit.rank ?? 0;
    const known = best.get(hit.id);
    if (known === undefined || known < rank) best.set(hit.id, rank);
  }
  return [...best.entries()].sort((one, other) => other[1] - one[1]).map(([id]) => id);
}

/** What `neighbourFault` reads of the judged request. */
export interface JudgedForNeighbours {
  person: string;
  issueKey: string;
  /** The request's first opening, in microseconds: S10's `opened_at`. */
  openedAt: number;
  /** The documents the request names, by artifact id. */
  documents: ReadonlySet<string>;
}

/**
 * The first field S10's `neighbours` reads of a candidate that the log cannot give as of the
 * judged moment, and the candidate, or null where it reads none such. It takes the candidates in
 * `order`, passing over the held and the dropped, and stops at its eighth neighbour, as S10 does;
 * each field is read only where the ones before it let the candidate through.
 */
export function neighbourFault(
  order: readonly string[],
  candidates: Readonly<Record<string, CandidateInputs>>,
  judged: JudgedForNeighbours
): { askId: string; field: string } | null {
  let kept = 0;
  for (const askId of order) {
    if (kept >= NEIGHBOURS_KEPT) break;
    const candidate = candidates[askId];
    if (candidate === undefined || "held" in candidate || "dropped" in candidate) continue;
    const fault = (field: string): { askId: string; field: string } => ({ askId, field });
    const ask = candidate.ask;
    if (typeof ask.state !== "string") return fault("state");
    if (!Object.hasOwn(ask, "answer")) return fault("answer");
    const answer = recordOf(ask.answer);
    if (ask.state !== "answered" || answer === null) continue;
    if (!Object.hasOwn(answer, "user")) return fault("answer.user");
    if (answer.user !== judged.person) continue;
    if (!Object.hasOwn(ask, "issue_key")) return fault("issue_key");
    if (ask.issue_key === judged.issueKey) continue;
    const at = microsecondsOf(stringOf(answer.at) ?? "");
    if (at === null) return fault("answer.at");
    if (at >= judged.openedAt) continue;
    if (typeof ask.kind !== "string") return fault("kind");
    if (ask.kind !== "approval") continue;
    if (!Object.hasOwn(ask, "approval")) return fault("approval");
    const documents = askDocsOf(ask.approval);
    if ("missing" in documents) return fault(documents.missing);
    if (documents.documents.some((one) => judged.documents.has(one.artifact_id))) continue;
    if (!Array.isArray(answer.selected)) return fault("answer.selected");
    if (!Object.hasOwn(answer, "text")) return fault("answer.text");
    if (typeof ask.question !== "string") return fault("question");
    if (candidate.issue !== null && candidate.issue.title === null) return fault("issue title");
    kept += 1;
  }
  return null;
}

/**
 * A candidate the reserve kept, as of the judged moment, from the two walks: dropped where its
 * ask has an event after the moment, else its fields as its last whole payload left them.
 * `afterPin` holds the asks the drop scan found an event of after the pin.
 */
function candidateOf(
  askId: string,
  log: InputsLog,
  candidates: CandidateLog,
  moment: number,
  afterPin: ReadonlySet<string>
): Exclude<CandidateInputs, { held: true }> {
  const last = log.lastEventOf(askId);
  if (last === undefined) return { dropped: "later-event", seen: "no-event-through-pin" };
  if (last > moment) return { dropped: "later-event", seen: "log" };
  if (afterPin.has(askId)) return { dropped: "later-event", seen: "after-pin" };
  const latest = candidates.latestOf(askId);
  if (latest === undefined) return { asOfEventId: null, ask: {}, issue: null };
  const issueKey = stringOf(latest.fields.issue_key);
  const title = issueKey === null ? null : stringOf(log.issueAsOf(issueKey, moment)?.fields.title);
  return {
    asOfEventId: latest.eventId,
    ask: latest.fields,
    issue: issueKey === null ? null : { key: issueKey, title },
  };
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

/** Walks the log through the pin into `note`, refused unless it reads the events the build read. */
async function walkThroughPin(
  corpus: Corpus,
  note: (event: DispatchEvent) => void
): Promise<number> {
  const walk = await pageEventLog({
    through: corpus.snapshot.throughEventId,
    onPage: (events) => {
      for (const event of events) note(event);
    },
  });
  if (corpus.snapshot.eventsRead > 0 && walk.eventsRead !== corpus.snapshot.eventsRead) {
    throw new S10InputsError(
      `the log through event ${corpus.snapshot.throughEventId} reads ${walk.eventsRead} events, ` +
        `and the build read ${corpus.snapshot.eventsRead} there: it is not the log the build read`
    );
  }
  return walk.eventsRead;
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
  openedAt: number;
  answerEventId: number;
  /** The judged payload as the write stored it, with `approval_artifact` written again. */
  ask: Record<string, unknown>;
  issue: IssueAsOf;
  issueAsOfEventId: number;
  documents: AskDocument[];
  /** S10's `legion_fact`; where it holds, no search is run and no neighbour read. */
  fact: LegionFact;
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
  if (judging === undefined || judging.answer === null) {
    return deviation("the log through the pin holds no answer of the build's person to the ask");
  }
  if (!judging.answer.own) {
    return deviation(
      "the person's first answer event on the ask carries no answer of the person's own (a " +
        "restored answer of another, or none), so S10's first answer holds no verdict"
    );
  }
  const event = judging.judged;
  if (event === null) {
    return deviation("no ask.opened or ask.edited of the ask comes before the person's answer");
  }
  const judged = { eventId: event.id, type: event.type, createdAt: event.created_at };
  const judgedAt = microsecondsOf(event.created_at);
  if (judgedAt === null) return deviation("the judged event's time is no time", judged);
  const payload = event.payload;
  const missing = (field: string): Deviation =>
    deviation(`the log gives no ${field} of the request as of event ${event.id}`, judged);
  if (payload.answer !== null && payload.answer !== undefined) {
    return deviation("the payload at the judged moment carries an answer", judged);
  }
  const approval = recordOf(payload.approval);
  const approved = approval?.approved;
  if (approved !== null && approved !== undefined) {
    const at = microsecondsOf(stringOf(recordOf(approved)?.at) ?? "");
    if (at === null || at >= judgedAt) {
      return deviation(
        "the payload at the judged moment carries an approval.approved not before the moment",
        judged
      );
    }
  }
  const issueKey = stringOf(payload.issue_key);
  if (issueKey === null) return missing("issue_key");
  const question = stringOf(payload.question);
  if (question === null || question.trim() === "") return missing("question");
  const openedAt = microsecondsOf(stringOf(payload.created_at) ?? "");
  if (openedAt === null) return missing("created_at");
  const step = log.issueAsOf(issueKey, event.id);
  const absent = ISSUE_FIELDS.filter((field) => !Object.hasOwn(step?.fields ?? {}, field));
  if (step === null || absent.length > 0) {
    return deviation(
      `the log gives no ${absent.join(", ")} of ${issueKey} as of event ${event.id}`,
      judged
    );
  }
  if (approval === null) return missing("approval");
  const documents = askDocsOf(approval);
  if ("missing" in documents) return missing(documents.missing);
  const artifactId = stringOf(approval.artifact_id);
  if (artifactId === null) return missing("approval.artifact_id");
  const primary = log.primaryAsOf(artifactId, event.id);
  if (primary === null) {
    return deviation(
      `the log records no creation of the approval's document ${artifactId} by event ` +
        `${event.id}, so its primary flag, which legion_fact reads, cannot be given`,
      judged
    );
  }
  const ask = { ...writeTimeAsk(payload), approval_artifact: { primary } };
  const issue = { key: issueKey, ...step.fields } as IssueAsOf;
  // S10's `prepare` reads the fact before its neighbours, and a fact that holds reads none.
  const fact = legionFactOf(ask, issue);
  if (fact === null) {
    return deviation(
      `S10's legion_fact cannot read the parent or the labels of ${issueKey} as of event ` +
        `${event.id}`,
      judged
    );
  }
  const title = stringOf(step.fields.title) ?? "";
  if (!fact.holds && title.trim() === "") {
    return deviation(`${issueKey} has no title as of event ${event.id} for S10 to search`, judged);
  }
  return {
    state: "ready",
    request,
    judged,
    judgedAt,
    openedAt,
    answerEventId: judging.answer.eventId,
    ask,
    issue,
    issueAsOfEventId: step.eventId,
    documents: documents.documents,
    fact,
    queries: { title: firstCharacters(title), question: firstCharacters(question) },
  };
}

/** What one document version's route answered: its markdown and its sha256, or why none. */
type VersionRead = { markdown: string; sha256: string } | string;

/**
 * One document version, as `replay.ts` reads it: `GET /api/v1/artifacts/{id}/versions/{v}`, whose
 * markdown must hash to the sha256 the route answers. A refusal or a mismatch is why the request
 * is a deviation. Only the version's bytes are read here; its name is the request's own.
 */
async function versionOf(document: AskDocument, config: DispatchConfig): Promise<VersionRead> {
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
  return { markdown, sha256 };
}

/** Counts with nothing counted yet. */
function noCounts(): CandidateCounts {
  return {
    askHits: 0,
    candidates: 0,
    held: 0,
    reserveErrors: 0,
    droppedLaterEvent: { log: 0, "no-event-through-pin": 0, "after-pin": 0 },
    kept: 0,
  };
}

/** A request file's counts, read back from a file the stage record names. */
function countsOf(value: unknown): CandidateCounts {
  const record = recordOf(value);
  const dropped = recordOf(record?.droppedLaterEvent);
  const counts = noCounts();
  for (const field of ["askHits", "candidates", "held", "reserveErrors", "kept"] as const) {
    counts[field] = numberOf(record?.[field]) ?? 0;
  }
  for (const seen of DROP_SEEN) counts.droppedLaterEvent[seen] = numberOf(dropped?.[seen]) ?? 0;
  return counts;
}

/** The guard a request's file is held to, from its plan. */
function guardOf(plan: Ready): AnswerGuard {
  return {
    askId: plan.request.askId,
    answerEventId: plan.answerEventId,
    selected: plan.request.rows.map((row) => row.selected),
    judgedAt: plan.judgedAt,
  };
}

/**
 * A request file a resume takes, already held to the bytes its record names, held to this run's
 * reading too: the request this run judges at that same moment, under the same fact, and no part
 * of its answer. Gives back its counts, null for a request the fact holds.
 */
async function takenRequest(
  stage: Stage,
  askId: string,
  plan: Ready | Deviation | undefined
): Promise<CandidateCounts | null> {
  const path = join(stage.directory, REQUESTS_DIRECTORY, `${askId}.json`);
  if (plan === undefined || plan.state !== "ready") {
    const reads = plan === undefined ? "holds no request" : "reads a deviation";
    throw new S10InputsError(`the stage record names ${path}, and this run ${reads} of that ask`);
  }
  const inputs = JSON.parse(await readFile(path, "utf8")) as RequestInputs;
  if (inputs.askId !== askId || recordOf(inputs.judged)?.eventId !== plan.judged.eventId) {
    throw new S10InputsError(
      `${path} is not the request this run judges at event ${plan.judged.eventId}`
    );
  }
  if (inputs.factHold !== plan.fact.holds) {
    throw new S10InputsError(`${path} records another legion_fact than this run reads`);
  }
  try {
    assertNoAnswer(inputs, guardOf(plan));
  } catch (error) {
    if (!(error instanceof LeakError)) throw error;
    throw new S10InputsError(`${path} carries the person's answer to it: ${error.message}`);
  }
  return inputs.counts === null ? null : countsOf(inputs.counts);
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
    build: {
      directory: corpus.directory,
      snapshotSha256: corpus.snapshotSha256,
      corpusSha256: corpus.corpusSha256,
      treesSha256: draw.treesSha256,
    },
    draw: {
      directory: draw.directory,
      drawSha256: draw.drawSha256,
      slicesSha256: draw.slicesSha256,
    },
    reserveAsk: options.decide === undefined ? RESERVE_ASK_SHA256 : null,
  };
  const inventory = inventoryOf(corpus.byCaseId, draw.trees, draw.report);
  const stage = await Stage.open(directory, chain);
  console.log(`inputs            ${directory}`);
  console.log(`claim run         ${claimRun}, under ${reference}`);
  console.log(`requests          ${inventory.length} in the inventory`);
  const finish = async (lines: readonly InventoryLine[]): Promise<string> => {
    const text = lines.map((line) => `${JSON.stringify(line)}\n`).join("");
    await writeWhole(join(directory, INVENTORY_FILE), text);
    const counts: StageCounts = {
      ...noCounts(),
      requests: lines.length,
      ready: lines.filter((line) => line.state === "ready").length,
      deviations: lines.filter((line) => line.state === "deviation").length,
      cases: lines.reduce((total, line) => total + line.cases, 0),
      pages: Object.keys(stage.record.pages).length,
    };
    for (const line of lines) {
      if (line.counts === null) continue;
      for (const field of ["askHits", "candidates", "held", "reserveErrors", "kept"] as const) {
        counts[field] += line.counts[field];
      }
      for (const seen of DROP_SEEN) {
        counts.droppedLaterEvent[seen] += line.counts.droppedLaterEvent[seen];
      }
    }
    stage.record.state = "written";
    stage.record.finishedAt = new Date().toISOString();
    stage.record.counts = counts;
    stage.record.inventorySha256 = sha256Of(text);
    await stage.save();
    return text;
  };
  if (inventory.length === 0) {
    await finish([]);
    console.error("no request: the draw's report trees hold no approval case");
    return 1;
  }

  // The first walk: nothing of any ask outside the inventory but the id of its last event.
  const log = new InputsLog(
    corpus.snapshot.person,
    inventory.map((request) => request.askId)
  );
  const eventsRead = await walkThroughPin(corpus, (event) => log.note(event));
  console.log(`log               ${eventsRead} events through ${corpus.snapshot.throughEventId}`);

  const plans = new Map(inventory.map((request) => [request.askId, planOf(request, log)]));
  const lines = new Map<string, InventoryLine>();
  // A request's line: ready with its file's sha256, or a deviation with its reason, which a
  // request whose own inputs stood takes from a document or a candidate read after them.
  const lineOf = (
    plan: Ready | Deviation,
    sha256: string | null,
    counts: CandidateCounts | null,
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
    counts,
  });
  for (const [askId, sha256] of Object.entries(stage.record.requests)) {
    // Written by an earlier run of this claim run, held to its record: never written again.
    const plan = plans.get(askId);
    const counts = await takenRequest(stage, askId, plan);
    lines.set(askId, lineOf(plan as Ready, sha256, counts));
  }
  const pending: Ready[] = [];
  for (const plan of plans.values()) {
    if (lines.has(plan.request.askId)) continue;
    if (plan.state === "deviation") lines.set(plan.request.askId, lineOf(plan, null, null));
    else pending.push(plan);
  }

  // Each document version the pending requests name, read once; each request names it itself.
  const config = await dispatchConfig();
  const versions = new Map<string, VersionRead>();
  const wanted = new Map<string, AskDocument>();
  for (const plan of pending) {
    for (const document of plan.documents) {
      wanted.set(`${document.artifact_id}@${document.version}`, document);
    }
  }
  await pool([...wanted.entries()], DOCUMENT_CONCURRENCY, async ([key, document]) => {
    versions.set(key, await versionOf(document, config));
  });
  const readable: Array<{ plan: Ready; documents: RequestDocument[] }> = [];
  for (const plan of pending) {
    const documents: RequestDocument[] = [];
    let unread: string | null = null;
    for (const document of plan.documents) {
      const read = versions.get(`${document.artifact_id}@${document.version}`) ?? "not read";
      if (typeof read === "string") {
        unread = read;
        break;
      }
      documents.push({ ...document, ...read });
    }
    if (unread === null) readable.push({ plan, documents });
    else lines.set(plan.request.askId, lineOf(plan, null, null, `a document: ${unread}`));
  }
  // S10 reads no neighbour of a request its fact holds (`prepare` returns the hold first), so
  // such a request is searched for nothing and none of its hits is decided.
  const searchable = readable.filter(({ plan }) => !plan.fact.holds);

  // S10's two queries of each request, each query once.
  const searches = new Searches(stage, config, await Searches.archived(stage));
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
  const searchesOf = (plan: Ready): { title: QueryInputs; question: QueryInputs } => ({
    title: queryInputsOf(plan.queries.title, answered.get(plan.queries.title) ?? []),
    question: queryInputsOf(plan.queries.question, answered.get(plan.queries.question) ?? []),
  });

  // The drop scan, once the searches are done: an event of a candidate's ask between the pin and
  // the searches drops it too. Its last event is recorded before any request file is written, so
  // a resume reads through the same event and drops the same candidates.
  let afterPin = new Set<string>();
  if (searchable.length > 0) {
    const recorded = stage.record.dropScanThrough;
    const scan = await dropScanOf(corpus.snapshot.throughEventId, recorded);
    afterPin = scan.asks;
    if (recorded === null) {
      stage.record.dropScanThrough = scan.through;
      await stage.save();
    }
    console.log(
      `drop scan         events ${corpus.snapshot.throughEventId} to ${scan.through}: ` +
        `${counted(afterPin.size, "ask")} with an event after the pin`
    );
  }

  // Every ask among the hits, decided under the reserve before any field of it is read.
  const candidates = new Set<string>();
  for (const { plan } of searchable) {
    const search = searchesOf(plan);
    for (const hit of [...search.title.hits, ...search.question.hits]) candidates.add(hit.id);
  }
  const verdicts = new Map<string, ReserveVerdict>();
  await pool([...candidates], DECISION_CONCURRENCY, async (askId) => {
    verdicts.set(askId, await decide(askId));
  });
  console.log(`candidates        ${candidates.size} decided by reserve-ask.ts`);

  // The second walk: the fields of the candidates the reserve kept, and of no other ask.
  const kept = new Set(
    [...candidates].filter(
      (askId) =>
        verdicts.get(askId) === "kept" &&
        log.lastEventOf(askId) !== undefined &&
        !afterPin.has(askId)
    )
  );
  const candidateLog = new CandidateLog(kept);
  if (kept.size > 0) await walkThroughPin(corpus, (event) => candidateLog.note(event));

  // One request's neighbours: each hit as of the judged moment, the counts, and the first field
  // S10's `neighbours` reads that the log cannot give.
  const neighboursOf = (plan: Ready) => {
    const search = searchesOf(plan);
    const outcomes: Record<string, CandidateInputs> = {};
    const counts = noCounts();
    counts.askHits = search.title.hits.length + search.question.hits.length;
    for (const hit of [...search.title.hits, ...search.question.hits]) {
      if (Object.hasOwn(outcomes, hit.id)) continue;
      const verdict = verdicts.get(hit.id) as ReserveVerdict;
      counts.candidates += 1;
      if (verdict !== "kept") {
        outcomes[hit.id] = { held: true, reserve: verdict };
        counts.held += 1;
        if (verdict === "error") counts.reserveErrors += 1;
        continue;
      }
      const outcome = candidateOf(hit.id, log, candidateLog, plan.judged.eventId, afterPin);
      outcomes[hit.id] = outcome;
      if ("dropped" in outcome) counts.droppedLaterEvent[outcome.seen] += 1;
      else counts.kept += 1;
    }
    const fault = neighbourFault(neighbourOrder(search), outcomes, {
      person: corpus.snapshot.person,
      issueKey: plan.issue.key,
      openedAt: plan.openedAt,
      documents: new Set(plan.documents.map((document) => document.artifact_id)),
    });
    return { search, outcomes, counts, fault };
  };

  for (const { plan, documents } of readable) {
    const neighbours = plan.fact.holds ? null : neighboursOf(plan);
    const counts = neighbours?.counts ?? null;
    const fault = neighbours?.fault ?? null;
    if (fault !== null) {
      const reason =
        `a candidate: the log gives no ${fault.field} of the ask ${fault.askId} as of event ` +
        `${plan.judged.eventId}, which S10's neighbours reads`;
      lines.set(plan.request.askId, lineOf(plan, null, counts, reason));
      continue;
    }
    const request: RequestInputs = {
      version: INPUTS_VERSION,
      askId: plan.request.askId,
      issueKey: plan.issue.key,
      group: plan.request.group,
      cases: plan.request.rows.length,
      judged: plan.judged,
      ask: plan.ask,
      issue: plan.issue,
      issueAsOfEventId: plan.issueAsOfEventId,
      documents,
      fact: plan.fact,
      factHold: plan.fact.holds,
      search: neighbours?.search ?? null,
      candidates: neighbours?.outcomes ?? null,
      counts,
    };
    let text: string;
    try {
      text = assertNoAnswer(request, guardOf(plan));
    } catch (error) {
      if (!(error instanceof LeakError)) throw error;
      // Inputs that would carry the answer are not the request as it stood: the file is not
      // written, and the reason names the rule and none of the text that broke it.
      const reason = "its inputs would carry the answer";
      lines.set(plan.request.askId, lineOf(plan, null, counts, reason));
      continue;
    }
    const sha256 = await stage.commit("requests", plan.request.askId, text);
    lines.set(plan.request.askId, lineOf(plan, sha256, counts));
  }

  const ordered = inventory.map((request) => lines.get(request.askId) as InventoryLine);
  await finish(ordered);
  const totals = stage.record.counts as StageCounts;
  const dropped = DROP_SEEN.reduce((total, seen) => total + totals.droppedLaterEvent[seen], 0);
  console.log(
    `written           ${counted(totals.ready, "request")} ready, ` +
      `${counted(totals.deviations, "deviation")}; ${dropped} candidates dropped by an event ` +
      `after the judged moment, ${totals.held} held (${totals.reserveErrors} undecided)`
  );
  for (const line of ordered) {
    if (line.state === "deviation") console.log(`  deviation ${line.askId}: ${line.reason}`);
  }
  console.log(`                  ${join(directory, INVENTORY_FILE)}`);
  console.log(`                  ${join(directory, STAGE_FILE)}`);
  return 0;
}

/** The command: the flags, the inputs, and the exit status it takes. */
export async function main(argv: readonly string[], options: InputsOptions = {}): Promise<number> {
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
