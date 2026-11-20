import { join } from "node:path";
import type { CaseRow } from "./build-corpus";
import {
  CorpusError,
  countAt,
  listOf,
  numberOf,
  parsed,
  recordOf,
  sha256At,
  stringOf,
  textAt,
} from "./corpus-io";
import type { DispatchEvent } from "./dispatch-read";
import type { BuildChain, FinalTestTree } from "./draw-io";
import { type AskDocument, askDocsOf, type LegionFact } from "./s10-mirror";

/**
 * The files the claim's S10 inputs stage (`s10-inputs.ts`) writes that a later stage reads back,
 * with one owner: their shapes, the parsers that read each back and refuse any other shape, the
 * inventory's rule (`inventoryOf`), which the stage writes its inventory by and the analysis
 * holds that inventory to, and the rule that places the person's answer to a request in a walk
 * of the log, by which the stage finds each request's judged moment.
 *
 * `s10-inputs.ts` writes the files under its `--out`, and `s10_judge.py` and `analyze.ts` read
 * them back: `inventory.jsonl` (`InventoryLine`, `inventoryLinesOf`), `requests/<ask id>.json`
 * (`RequestInputs`, `requestInputsOf`), `pages/<sha256>.json` (`PageRecord`, `pageRecordOf`) and
 * `s10-inputs.json` (`StageRecord`, `stageRecordOf`). A reader written in TypeScript imports this
 * module and never the command, as `draw-io.ts` is to `draw.ts`; `s10_judge.py` reads the same
 * shapes, as these types and parsers state them. Every shape carries `INPUTS_VERSION` where its
 * file does, and a parser refuses a file of another version, or one that is not this shape, with
 * a `CorpusError` naming the file.
 *
 * Nothing here reads Dispatch or writes a file. The answer rule (`AnswerLog`) holds, of each ask
 * it is given, the statements and the first answer event in memory, and nothing of either is in
 * any file the stage writes.
 */

/** The files the stage writes, under its `--out`. */
export const INVENTORY_FILE = "inventory.jsonl";
export const STAGE_FILE = "s10-inputs.json";
export const REQUESTS_DIRECTORY = "requests";
export const PAGES_DIRECTORY = "pages";

/** The version of the shapes the stage writes, which every reader checks. */
export const INPUTS_VERSION = 4;

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

/** Where the event that drops a candidate was seen. */
export type DropSeen = "log" | "no-event-through-pin" | "after-pin";

/** Those places, in the order a count of them is written. */
export const DROP_SEEN: readonly DropSeen[] = ["log", "no-event-through-pin", "after-pin"];

/** The name a request file or a page record file is kept under: an ask id or a sha256. */
export const FILE_KEY = /^[A-Za-z0-9-]+$/;

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

/** The fields of `CandidateCounts` that are one count each. */
export const COUNT_FIELDS = ["askHits", "candidates", "held", "reserveErrors", "kept"] as const;

/** Counts with nothing counted yet. */
export function noCounts(): CandidateCounts {
  return {
    askHits: 0,
    candidates: 0,
    held: 0,
    reserveErrors: 0,
    droppedLaterEvent: { log: 0, "no-event-through-pin": 0, "after-pin": 0 },
    kept: 0,
  };
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
  /** Why the request is a deviation; null for a ready one. */
  reason: string | null;
  /** The sha256 of `requests/<ask id>.json`, written for a ready request only. */
  requestSha256: string | null;
  /**
   * The request's candidates, a deviation's too; null for one that stopped before its search, and
   * for one its fact holds, which S10 searches for nothing.
   */
  counts: CandidateCounts | null;
  /**
   * The end of the drop scan the request's candidates were dropped by, as its file records it;
   * null with `counts`.
   */
  dropScanThrough: number | null;
}

/** The issue of a request as of its judged moment, in Dispatch's own field names. */
export type IssueAsOf = { key: string } & Record<IssueField, unknown>;

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

/** A request's two queries, as S10 sends them: its issue's title and its question. */
export interface RequestSearch {
  title: QueryInputs;
  question: QueryInputs;
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
   * read, so a held request has no `search`, `candidates` or `counts` (each null), and every
   * other request has all three.
   */
  factHold: boolean;
  search: RequestSearch | null;
  candidates: Record<string, CandidateInputs> | null;
  counts: CandidateCounts | null;
  /**
   * The end of the drop scan the candidates were dropped by (`StageRecord.dropScanThrough` when
   * this file was written); null with `search`.
   */
  dropScanThrough: number | null;
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

/**
 * The chain a stage record holds, which a resume must match whole: the claim run and the build,
 * as the draw's own record names them (`BuildChain`, `draw.ts`), the read, this run's digest, the
 * person and the pin, the sha256 of the draw's two files, and the digest of the decision's
 * closure. The directories they were read from are recorded and not compared, since their bytes
 * are.
 */
export interface StageChain {
  claimRun: BuildChain;
  reference: string;
  runDigest: string;
  person: string;
  throughEventId: number;
  draw: { drawSha256: string; slicesSha256: string };
  /** The digest of the decision's pinned closure (`s10-decision.ts`), or of a test's own. */
  reserveAsk: string;
  directories: { build: string; draw: string };
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
  /** When the run that last wrote anything new ended; null until a run ends. */
  finishedAt: string | null;
  /**
   * The last event of the drop scan, which read on from the pin once the searches were done; a
   * run reads through it again, and on from it when a page its searches use is not one the scan
   * covers. Null before the scan.
   */
  dropScanThrough: number | null;
  /**
   * The page records the recorded scan covers: each page then archived, by the sha256 of its
   * page, with the sha256 of its file as it stood when the scan ran, so every read it holds came
   * before the scan. Written in the same record write as `dropScanThrough`; null with it.
   */
  dropScanPages: Record<string, string> | null;
  counts: StageCounts | null;
  inventorySha256: string | null;
  /** The sha256 of each request file's bytes, by ask id. */
  requests: Record<string, string>;
  /** The sha256 of each page record file's bytes, by the sha256 of the page it records. */
  pages: Record<string, string>;
}

/** Where a request's file is, under the stage's directory. */
export function requestPath(directory: string, askId: string): string {
  return join(directory, REQUESTS_DIRECTORY, `${askId}.json`);
}

/** Where a page's record is, under the stage's directory. */
export function pagePath(directory: string, sha256: string): string {
  return join(directory, PAGES_DIRECTORY, `${sha256}.json`);
}

/** The person's first answer event on an ask, and whether it carries the person's own answer. */
export interface FirstAnswer {
  eventId: number;
  own: boolean;
}

/** Where one request stands in a walk of the log, under the answer rule. */
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
 * The answer rule (point 7), over a walk of the log handed in id order: for each ask it is given,
 * the person's first answer as S10's own `first_answer` places it, the first event after the
 * ask's opening, in the log's order, whose actor is the person and that is either an
 * `ask.answered` of the ask, whatever answer it carries, or, for an ask that names one document
 * (`askDocsOf`), an `artifact.approved` or `artifact.changes_requested` on that document at the
 * `version` the ask names; and the statement that stood then, the judged moment. An answer is the
 * person's own (`FirstAnswer.own`) where it is a settlement, or an `ask.answered` whose answer's
 * user is the person: a restored answer of another user, or none, is not.
 */
export class AnswerLog {
  private readonly person: string;
  private readonly judging = new Map<string, Judging>();

  constructor(person: string, asks: Iterable<string>) {
    this.person = person;
    for (const askId of asks) {
      this.judging.set(askId, { firstOpened: null, statement: null, answer: null, judged: null });
    }
  }

  /** Reads one event. */
  note(event: DispatchEvent): void {
    const own = event.type.startsWith("ask.") ? stringOf(event.payload.id) : null;
    if (own !== null) this.noteStatement(own, event);
    if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
      this.noteSettlement(event);
    }
  }

  /** Where one ask the rule was given stands. */
  judgingOf(askId: string): Judging | undefined {
    return this.judging.get(askId);
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
   * A document settlement by the person, which answers each ask still waiting for its answer
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

/** One request of the inventory, with the corpus's cases of its ask, which are never written. */
export interface InventoryRequest {
  askId: string;
  issueKey: string | null;
  group: string;
  rows: readonly CaseRow[];
}

/**
 * The inventory (point 6): one request per ask among the approval cases of the eligible trees the
 * draw put in report, sorted by ask id, with the corpus's cases of each ask, which are its
 * provenance and never a second request. Every way the draw's two files could make a report case
 * map to no request or to two is refused, and so is a case either names twice. The inputs stage
 * writes its inventory by this rule, and the analysis holds that inventory to it.
 */
export function inventoryOf(
  byCaseId: ReadonlyMap<string, CaseRow>,
  trees: readonly FinalTestTree[],
  report: readonly string[]
): InventoryRequest[] {
  const inReport = new Set<string>();
  for (const caseId of report) {
    if (inReport.has(caseId)) {
      throw new CorpusError(`the draw's report slice names the case ${caseId} twice`);
    }
    inReport.add(caseId);
  }
  const requests = new Map<string, InventoryRequest & { rows: CaseRow[] }>();
  const placed = new Set<string>();
  for (const tree of trees) {
    for (const caseId of tree.cases) {
      const row = byCaseId.get(caseId);
      if (row === undefined) {
        throw new CorpusError(
          `the draw's tree ${tree.group} names the case ${caseId}, which the build does not hold`
        );
      }
      if (placed.has(caseId)) {
        throw new CorpusError(`the draw's trees name the case ${caseId} twice`);
      }
      placed.add(caseId);
      if (row.kind !== "approval") continue;
      if (tree.slice !== "report" || !inReport.has(caseId)) {
        throw new CorpusError(
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
        throw new CorpusError(
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
      throw new CorpusError(
        `the draw's report slice names the case ${caseId}, which the build does not hold`
      );
    }
    if (!placed.has(caseId)) {
      throw new CorpusError(
        `the draw's report slice holds the case ${caseId}, which no eligible tree of its ` +
          "record holds"
      );
    }
  }
  return [...requests.values()].sort((one, other) =>
    one.askId < other.askId ? -1 : one.askId > other.askId ? 1 : 0
  );
}

/** An event id or another whole number a file names, refused where it is none. */
function wholeAt(value: unknown, where: string, field: string): number {
  const whole = numberOf(value);
  if (whole === null || !Number.isSafeInteger(whole)) {
    throw new CorpusError(`${where} holds a ${field} that is no whole number`);
  }
  return whole;
}

/** A record a file holds, refused where it is none. */
function recordAt(value: unknown, where: string, field: string): Record<string, unknown> {
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${where} holds a ${field} that is no object`);
  return record;
}

/** A request's candidate counts read back. */
export function countsOf(value: unknown, where: string): CandidateCounts {
  const record = recordAt(value, where, "counts");
  const dropped = recordAt(record.droppedLaterEvent, where, "counts.droppedLaterEvent");
  const counts = noCounts();
  for (const field of COUNT_FIELDS) counts[field] = countAt(record[field], where, field);
  for (const seen of DROP_SEEN) {
    counts.droppedLaterEvent[seen] = countAt(dropped[seen], where, `droppedLaterEvent.${seen}`);
  }
  return counts;
}

/** A judged moment read back. */
function judgedOf(value: unknown, where: string): JudgedMoment {
  const record = recordAt(value, where, "judged");
  const type = textAt(record.type, where, "judged.type");
  if (type !== "ask.opened" && type !== "ask.edited") {
    throw new CorpusError(`${where} holds a judged moment of a ${type}, not an opening or edit`);
  }
  return {
    eventId: wholeAt(record.eventId, where, "judged.eventId"),
    type,
    createdAt: textAt(record.createdAt, where, "judged.createdAt"),
  };
}

/** Each line of `inventory.jsonl` read back, in its order; `path` names the file in a refusal. */
export function inventoryLinesOf(text: string, path: string): InventoryLine[] {
  const lines = text.split("\n");
  if (lines.pop() !== "") throw new CorpusError(`${path} does not end its last line`);
  return lines.map((line, at) => {
    const where = `${path} line ${at + 1}`;
    const record = recordAt(parsed(line, where), where, "line");
    const state = record.state;
    if (state !== "ready" && state !== "deviation") {
      throw new CorpusError(`${where} is neither ready nor a deviation`);
    }
    const issueKey = record.issueKey === null ? null : textAt(record.issueKey, where, "issueKey");
    const reason = record.reason === null ? null : textAt(record.reason, where, "reason");
    const requestSha256 =
      record.requestSha256 === null ? null : sha256At(record.requestSha256, where, "requestSha256");
    const ready = state === "ready";
    if (ready !== (reason === null) || ready !== (requestSha256 !== null)) {
      throw new CorpusError(
        `${where} is ${state} with ${reason === null ? "no" : "a"} reason and ` +
          `${requestSha256 === null ? "no" : "a"} request file`
      );
    }
    const counts = record.counts === null ? null : countsOf(record.counts, where);
    const dropScanThrough =
      record.dropScanThrough === null
        ? null
        : wholeAt(record.dropScanThrough, where, "dropScanThrough");
    if ((counts === null) !== (dropScanThrough === null)) {
      throw new CorpusError(`${where} records candidate counts and a drop scan's end apart`);
    }
    return {
      askId: textAt(record.askId, where, "askId"),
      issueKey,
      group: textAt(record.group, where, "group"),
      cases: countAt(record.cases, where, "cases"),
      judged: record.judged === null ? null : judgedOf(record.judged, where),
      state,
      reason,
      requestSha256,
      counts,
      dropScanThrough,
    };
  });
}

/** One of a request's queries read back. */
function queryOf(value: unknown, where: string): QueryInputs {
  const record = recordAt(value, where, "query");
  return {
    query: textAt(record.query, where, "query"),
    pages: listOf(record.pages).map((one) => {
      const page = recordAt(one, where, "page");
      return {
        sha256: sha256At(page.sha256, where, "page sha256"),
        offset: countAt(page.offset, where, "page offset"),
      };
    }),
    hits: listOf(record.hits).map((one) => {
      const hit = recordAt(one, where, "hit");
      return {
        id: textAt(hit.id, where, "hit id"),
        rank: hit.rank === null ? null : rankAt(hit.rank, where),
        page: sha256At(hit.page, where, "hit page"),
        position: countAt(hit.position, where, "hit position"),
      };
    }),
  };
}

/** A rank as the search gives it: any finite number. */
function rankAt(value: unknown, where: string): number {
  const rank = numberOf(value);
  if (rank === null) throw new CorpusError(`${where} holds a hit rank that is no number`);
  return rank;
}

/** One candidate read back: held, dropped, or kept with its fields. */
function candidateInputsOf(value: unknown, where: string): CandidateInputs {
  const record = recordAt(value, where, "candidate");
  if (Object.hasOwn(record, "held")) {
    if (record.held !== true || (record.reserve !== "held" && record.reserve !== "error")) {
      throw new CorpusError(`${where} holds a held candidate in a shape the stage never writes`);
    }
    return { held: true, reserve: record.reserve };
  }
  if (Object.hasOwn(record, "dropped")) {
    const seen = DROP_SEEN.find((one) => one === record.seen);
    if (record.dropped !== "later-event" || seen === undefined) {
      throw new CorpusError(`${where} holds a dropped candidate in a shape the stage never writes`);
    }
    return { dropped: "later-event", seen };
  }
  const issue = record.issue === null ? null : recordAt(record.issue, where, "candidate issue");
  return {
    asOfEventId:
      record.asOfEventId === null ? null : wholeAt(record.asOfEventId, where, "asOfEventId"),
    ask: recordAt(record.ask, where, "candidate ask"),
    issue:
      issue === null
        ? null
        : {
            key: textAt(issue.key, where, "candidate issue key"),
            title: issue.title === null ? null : textAt(issue.title, where, "candidate title"),
          },
  };
}

/** S10's `legion_fact` as a request file records it. */
function factOf(value: unknown, where: string): LegionFact {
  const record = recordAt(value, where, "fact");
  const flags = ["holds", "legion", "primary", "root"] as const;
  for (const flag of flags) {
    if (typeof record[flag] !== "boolean") {
      throw new CorpusError(`${where} holds a fact whose ${flag} is no boolean`);
    }
  }
  if (!Object.hasOwn(record, "status")) {
    throw new CorpusError(`${where} holds a fact with no status`);
  }
  return {
    holds: record.holds as boolean,
    legion: record.legion as boolean,
    primary: record.primary as boolean,
    root: record.root as boolean,
    status: record.status,
  };
}

/**
 * `requests/<ask id>.json` read back, refused unless it is a request of this version in the shape
 * the stage writes: a request its fact holds has no search, candidates or counts, and every other
 * has all three. `path` names the file in a refusal.
 */
export function requestInputsOf(text: string, path: string): RequestInputs {
  const record = recordAt(parsed(text, path), path, "request");
  if (record.version !== INPUTS_VERSION) {
    throw new CorpusError(`${path} is no request of version ${INPUTS_VERSION} of these inputs`);
  }
  const issue = recordAt(record.issue, path, "issue");
  const key = textAt(issue.key, path, "issue key");
  for (const field of ISSUE_FIELDS) {
    if (!Object.hasOwn(issue, field)) throw new CorpusError(`${path} gives its issue no ${field}`);
  }
  const fact = factOf(record.fact, path);
  if (record.factHold !== fact.holds) {
    throw new CorpusError(`${path} records a factHold that is not its fact's`);
  }
  const searched = [record.search, record.candidates, record.counts, record.dropScanThrough].map(
    (one) => one !== null
  );
  if (searched.some((one) => one === fact.holds)) {
    throw new CorpusError(
      `${path} is a request whose fact ${fact.holds ? "holds" : "does not hold"}, and its ` +
        "search, candidates, counts and drop scan's end are not what such a request carries"
    );
  }
  const search = record.search === null ? null : recordAt(record.search, path, "search");
  const candidates =
    record.candidates === null ? null : recordAt(record.candidates, path, "candidates");
  return {
    version: INPUTS_VERSION,
    askId: textAt(record.askId, path, "askId"),
    issueKey: textAt(record.issueKey, path, "issueKey"),
    group: textAt(record.group, path, "group"),
    cases: countAt(record.cases, path, "cases"),
    judged: judgedOf(record.judged, path),
    ask: recordAt(record.ask, path, "ask"),
    issue: { ...issue, key } as IssueAsOf,
    issueAsOfEventId: wholeAt(record.issueAsOfEventId, path, "issueAsOfEventId"),
    documents: listOf(record.documents).map((one) => {
      const document = recordAt(one, path, "document");
      return {
        artifact_id: textAt(document.artifact_id, path, "document artifact_id"),
        name: textAt(document.name, path, "document name"),
        version: wholeAt(document.version, path, "document version"),
        markdown: textAt(document.markdown, path, "document markdown"),
        sha256: sha256At(document.sha256, path, "document sha256"),
      };
    }),
    fact,
    factHold: fact.holds,
    search:
      search === null
        ? null
        : {
            title: queryOf(search.title, `${path} search.title`),
            question: queryOf(search.question, `${path} search.question`),
          },
    candidates:
      candidates === null
        ? null
        : Object.fromEntries(
            Object.entries(candidates).map(([askId, one]) => [
              askId,
              candidateInputsOf(one, `${path} candidate ${askId}`),
            ])
          ),
    counts: record.counts === null ? null : countsOf(record.counts, path),
    dropScanThrough:
      record.dropScanThrough === null
        ? null
        : wholeAt(record.dropScanThrough, path, "dropScanThrough"),
  };
}

/** One hit of a search answer: its id, its kind and its rank. Any other shape is refused. */
export function pageHitOf(value: unknown, where: string): PageHit {
  const hit = recordOf(value);
  const id = stringOf(hit?.id);
  const kind = stringOf(hit?.kind);
  if (id === null || kind === null) {
    throw new CorpusError(`${where} holds a hit with no id or no kind`);
  }
  return { id, kind, rank: numberOf(hit?.rank) };
}

/** `pages/<sha256>.json` read back, refused where it is not the shape the stage writes. */
export function pageRecordOf(text: string, path: string): PageRecord {
  const record = recordOf(parsed(text, path));
  const sha256 = stringOf(record?.sha256);
  const hits = listOf(record?.hits).map((one) => pageHitOf(one, path));
  const reads = listOf(record?.reads).map((one) => {
    const read = recordOf(one);
    const query = stringOf(read?.query);
    const offset = numberOf(read?.offset);
    const readAt = stringOf(read?.readAt);
    if (query === null || offset === null || readAt === null) {
      throw new CorpusError(`${path} records a read with no query, offset or time`);
    }
    return { query, offset, readAt };
  });
  if (sha256 === null || reads.length === 0 || !Array.isArray(record?.hits)) {
    throw new CorpusError(`${path} is no page record the stage writes`);
  }
  return { sha256, hits, reads };
}

/** The files a stage record names under one of its two directories, refused in another shape. */
function recordedFiles(value: unknown, path: string): Record<string, string> {
  const record = recordOf(value);
  if (record === null) {
    throw new CorpusError(`${path} records its files in a shape the stage never writes`);
  }
  const files: Record<string, string> = {};
  for (const [key, sha256] of Object.entries(record)) {
    if (!FILE_KEY.test(key) || typeof sha256 !== "string" || !/^[0-9a-f]{64}$/.test(sha256)) {
      throw new CorpusError(`${path} records its files in a shape the stage never writes`);
    }
    files[key] = sha256;
  }
  return files;
}

/** The stage's counts read back. */
function stageCountsOf(value: unknown, where: string): StageCounts {
  const record = recordAt(value, where, "counts");
  return {
    ...countsOf(record, where),
    requests: countAt(record.requests, where, "requests"),
    ready: countAt(record.ready, where, "ready"),
    deviations: countAt(record.deviations, where, "deviations"),
    cases: countAt(record.cases, where, "cases"),
    pages: countAt(record.pages, where, "pages"),
  };
}

/**
 * `s10-inputs.json` read back, refused unless it is a record of this version in the shape the
 * stage writes, its chain whole. `path` names the file in a refusal.
 */
export function stageRecordOf(text: string, path: string): StageRecord {
  const record = recordOf(parsed(text, path));
  if (record === null || record.stage !== "s10-inputs" || record.version !== INPUTS_VERSION) {
    throw new CorpusError(`${path} is no record of version ${INPUTS_VERSION} of the S10 inputs`);
  }
  const state = record.state;
  if (state !== "started" && state !== "written") {
    throw new CorpusError(`${path} is neither started nor written`);
  }
  const claimRun = recordAt(record.claimRun, path, "claimRun");
  const draw = recordAt(record.draw, path, "draw");
  const directories = recordAt(record.directories, path, "directories");
  const optional = <T>(value: unknown, read: (one: unknown) => T): T | null =>
    value === null ? null : read(value);
  const dropScanThrough = optional(record.dropScanThrough, (one) =>
    wholeAt(one, path, "dropScanThrough")
  );
  const dropScanPages = optional(record.dropScanPages, (one) => recordedFiles(one, path));
  const pages = recordedFiles(record.pages, path);
  if ((dropScanThrough === null) !== (dropScanPages === null)) {
    throw new CorpusError(
      `${path} records a drop scan's end without the pages it covers, or those without an end`
    );
  }
  for (const page of Object.keys(dropScanPages ?? {})) {
    if (!Object.hasOwn(pages, page)) {
      throw new CorpusError(`${path} records a drop scan over a page ${page} it does not name`);
    }
  }
  return {
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state,
    claimRun: {
      id: textAt(claimRun.id, path, "claimRun.id"),
      snapshotSha256: sha256At(claimRun.snapshotSha256, path, "claimRun.snapshotSha256"),
      corpusSha256: sha256At(claimRun.corpusSha256, path, "claimRun.corpusSha256"),
      treesSha256: sha256At(claimRun.treesSha256, path, "claimRun.treesSha256"),
    },
    reference: textAt(record.reference, path, "reference"),
    runDigest: sha256At(record.runDigest, path, "runDigest"),
    person: textAt(record.person, path, "person"),
    throughEventId: wholeAt(record.throughEventId, path, "throughEventId"),
    draw: {
      drawSha256: sha256At(draw.drawSha256, path, "draw.drawSha256"),
      slicesSha256: sha256At(draw.slicesSha256, path, "draw.slicesSha256"),
    },
    reserveAsk: sha256At(record.reserveAsk, path, "reserveAsk"),
    directories: {
      build: textAt(directories.build, path, "directories.build"),
      draw: textAt(directories.draw, path, "directories.draw"),
    },
    startedAt: textAt(record.startedAt, path, "startedAt"),
    finishedAt: optional(record.finishedAt, (one) => textAt(one, path, "finishedAt")),
    dropScanThrough,
    dropScanPages,
    counts: optional(record.counts, (one) => stageCountsOf(one, path)),
    inventorySha256: optional(record.inventorySha256, (one) =>
      sha256At(one, path, "inventorySha256")
    ),
    requests: recordedFiles(record.requests, path),
    pages,
  };
}
