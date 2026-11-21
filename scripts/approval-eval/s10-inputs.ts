import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { claimStartOf } from "./claim-start";
import {
  CannotRunError,
  type Corpus,
  counted,
  listOf,
  outDirectory,
  pool,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import {
  type DispatchConfig,
  type DispatchEvent,
  dispatchConfig,
  pageEventLog,
} from "./dispatch-read";
import { LeakError } from "./render";
import { readVersion } from "./replay";
import { microsecondsOf, type Registration } from "./reserve";
import { type Decision, type ReserveVerdict, reserveAskDecision } from "./s10-decision";
import {
  AnswerLog,
  CANDIDATE_FIELDS,
  type CandidateAsk,
  type CandidateCounts,
  type CandidateInputs,
  COUNT_FIELDS,
  DROP_SEEN,
  INPUTS_VERSION,
  INVENTORY_FILE,
  type InventoryLine,
  type InventoryRequest,
  ISSUE_FIELDS,
  type IssueAsOf,
  type IssueField,
  inventoryOf,
  type JudgedMoment,
  noCounts,
  type QueryHit,
  type RequestDocument,
  type RequestInputs,
  type RequestSearch,
  requestInputsOf,
  requestPath,
  STAGE_FILE,
  type StageChain,
  type StageCounts,
} from "./s10-inputs-io";
import {
  type AskDocument,
  askDocsOf,
  firstCharacters,
  type LegionFact,
  legionFactOf,
  neighbourFault,
  neighbourOrder,
} from "./s10-mirror";
import {
  type QueryPages,
  queryInputsOf,
  S10InputsError,
  Searches,
  Stage,
  writeWhole,
} from "./s10-stage";

/**
 * Writes the inputs the claim's S10 reviewer judges each request of the final test on: the third
 * stage of the claim run, after its build (`build-corpus.ts`) and its draw (`draw.ts`), and
 * before the judging (`s10_judge.py`) and the analysis (`analyze.ts`), each stage run under the
 * registered read and the claim run (`APPROVAL_EVAL_RESERVE`, `APPROVAL_EVAL_CLAIM_RUN`).
 *
 * The files. The shapes of the four files written here, the parsers that read each back, and the
 * rule that places the person's answer are in `s10-inputs-io.ts`, which every later stage written
 * in TypeScript imports in place of this command; S10's own reads of a request, mirrored from its
 * Python code, are in `s10-mirror.ts`; the reserve's decision and its pin are in
 * `s10-decision.ts`; and the stage record, the files it names and the search pages are kept by
 * `s10-stage.ts`.
 *
 * The inventory. Before any judging, `inventory.jsonl` names every answered approval request of
 * the person among the draw's report cases, and no other (the claim's point 6): the approval cases
 * of each eligible tree the draw put in report, keyed by ask id. The cases of one ask are one
 * request, and are its provenance only: never a second judgement or a second row of any figure.
 * A report case the build does not hold, an approval case of a tree the draw left out of report,
 * a case `slices.json` and the trees disagree on, a case named twice, and an ask whose cases sit
 * in two trees are each refused (`inventoryOf`, in `s10-inputs-io.ts`). A case id is
 * `<ask id>@<answer time>`, so no file here names one: a line carries the count of its cases, and
 * the analysis (`analyze.ts --claim-s10`) holds this inventory to the same rule over the same
 * build and draw, the draw pinned by its sha256 in `s10-inputs.json`.
 *
 * What is read, and as of when (point 3). Live, S10 judged a request at its last opening or edit,
 * before the person's answer existed. Here each request is read as it stood at the judged moment:
 * the last `ask.opened` or `ask.edited` of the ask, by event id, strictly before the person's
 * answer, which is placed as S10's own `first_answer` places it (point 7) by the rule `AnswerLog`
 * holds; where that answer is not the person's own, S10's first answer holds no verdict, and the
 * request is a deviation, judged at no moment.
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
 * immutable `version`, read as `replay.ts` reads a version (`readVersion`) and checked against
 * the sha256 the version route answers, with the `artifact_id`, `name` and `version` the
 * request's own payload gives. A field S10 reads of the request (its question, its first opening,
 * its issue, each document's id, name and version, an entry of `approval.documents` that is no
 * document, and each issue field `legion_fact` reads) that the log cannot give as of the moment
 * makes the request a deviation, naming the field. The answer rule's one-document test reads the
 * same list.
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
 * id of its event, is in any file this writes. The analysis compares each request with his
 * answer as the build's corpus records it, which this stage never writes. A payload at the
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
 * Every ask among the hits is first decided by `reserve-ask.ts` as commit 651db8d5 decides it,
 * outside the claim's read, by the decision `s10-decision.ts` pins and spawns, its closure
 * checked before anything is read and again right before the first child is spawned. An ask it
 * holds, or decides nothing on, is held, and the second counted as an error, as S10 counts it.
 * Every ask hit is decided, those answered before the reserve's cut included, which S10 would not
 * ask about and which the reserve keeps. Until then no field of a candidate has been read: the
 * first walk of the log keeps, of an ask outside the inventory, only the id of its last event,
 * and the candidates' fields are read by a second walk that reads them only for the asks the
 * reserve kept, so a held candidate's fields are never read, kept or written. Any event of a kept
 * candidate's ask after the moment drops it (`later-event`), and the next hit is S10's to take:
 * an event of an ask is an `ask.` event whose payload's `id` is the ask, or any event whose
 * payload's `ask_id` is (its comments, deliveries, followers and document settlements). An ask
 * with no event in the log through the pin opened after it, so after the moment, and is dropped
 * too, and so is one with an event after the pin (below). Otherwise its fields are its last whole
 * payload's, which no event after the moment changed, and its issue's title as of the moment.
 * S10's `neighbours` reads a candidate's fields one after another, in the order `neighbourFault`
 * in `s10-mirror.ts` states, taking the hits in its own order, by rank, and stopping at its eighth
 * neighbour; the first of those reads the log cannot give as of the moment makes the request a
 * deviation, naming the candidate and the field. The filters themselves are S10's own code's,
 * run over these fields. Each inventory line carries its request's counts (the asks held, the
 * asks `reserve-ask.ts` decided nothing on, those dropped by an event after the moment, by where
 * the event was seen, and those kept) and the end of the drop scan its candidates were dropped by
 * (`dropScanThrough`), as its request file does.
 *
 * The walks. The log is walked twice, each time through the build's pin, and each walk is refused
 * unless it reads as many events as the build read, which a build in the claim run records (a
 * build that records none is refused). The first keeps each ask's last event id, each issue's
 * five fields at each event that set one, each document's creation record, and each inventory
 * ask's statements up to its answer: not the whole log, and no field of any other ask. The second,
 * after the reserve's decisions, keeps the last whole payload of each candidate the reserve kept.
 * Every input is as of the judged moment from those walks alone. The searches run after the pin,
 * and an event between the pin and them can move a rank too, so once the searches are done the
 * log is read on from the pin to its newest event at that time, the drop scan, and of each of
 * those events nothing is read but the ask it is an event of: a candidate with any is dropped
 * (`later-event`, seen `after-pin`). The scan's last event id is recorded (`dropScanThrough`),
 * in the same write of the stage record as the page records it covers (`dropScanPages`, each by
 * the sha256 of its file as it stood when the scan ran, so every read it holds came before the
 * scan), before any request file is written. Every run, a resume included, reads the scan
 * through the recorded id again; where any page its searches use is not covered (a page read
 * after the recorded scan, by a resume that searched a request whose document read was refused
 * before, or by a run that stopped before its scan was recorded), it scans on from that id to the
 * log's newest event, drops the candidates with an event there too, and records the new end with
 * every page then archived covered, before any request file is written. A run whose every page is
 * covered keeps the recorded end. A request file and its inventory line record the end of the scan
 * their candidates were dropped by, so a file an earlier run wrote under an earlier end says so.
 *
 * The stage record. `s10-inputs.json` records the claim run and the sha256 of each of the build's
 * `snapshot.json`, `corpus.jsonl` and `trees.json` (one `BuildChain`, as the draw's own record
 * holds it), the read it is under, this run's digest, the person and the pin, the sha256 of the
 * draw's `draw.json` and `slices.json` and the digest of the decision's closure, so the build, the
 * draw and these inputs are one chain and no directory, and no file of one, can be put in
 * another's place between stages: the draw is refused unless its record names this run's chain
 * and those files' bytes now (`chainRefusal`). It names each request file and each page record
 * with the sha256 of its bytes: a file is written beside its place, the record naming it is
 * written, and only then is the file moved into its place, so the record is what makes a file
 * this run's. It is written again once the drop scan is done, with `dropScanThrough` and
 * `dropScanPages`, and when the run ends, with the counts, which are the sums of the inventory's
 * lines, recomputed on every run, and the sha256 of `inventory.jsonl`. A run into an `--out`
 * holding another run's record, or files of no record, is refused; a run into its own claim
 * run's directory resumes it, and takes only what its record names: every file the record names
 * must be there with the bytes it names, and every request file must still be the request this
 * run judges, at the same moment and under the same fact, or the run is refused, and so is any
 * file the record does not name. Those bytes are the ones its first run held to `assertNoAnswer`
 * before it wrote them, under the same chain, so they are not held to it again. A file written
 * beside its place whose bytes the record names is moved into it, and any other is removed
 * unread. A resume takes each page record rather than ask that page again, never writes a request
 * file again, and works anew only on the requests with no file, the deviations among them; one
 * that writes nothing new keeps the record's `finishedAt`, so its `s10-inputs.json` is byte for
 * byte the one it resumed.
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

/** How many searches, document reads and decisions run at once. */
const SEARCH_CONCURRENCY = 4;
const DOCUMENT_CONCURRENCY = 8;
const DECISION_CONCURRENCY = 8;

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

/** What a caller may hand the command beside its flags: a test's registration, root, decision. */
export interface InputsOptions {
  registration?: Registration;
  root?: string;
  decision?: Decision;
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

/**
 * The first walk of the log, read for the inputs: each ask's last event id and nothing else of
 * an ask outside the inventory, each issue's fields at each event that set one, each document's
 * creation record, and, under the answer rule it extends (`AnswerLog`), the statements and first
 * answer of each request of the inventory. The walk hands the events in id order.
 */
export class InputsLog extends AnswerLog {
  private readonly lastEvents = new Map<string, number>();
  private readonly issues = new Map<string, IssueStep[]>();
  private readonly primaries = new Map<string, { eventId: number; primary: boolean }>();

  /** Reads one event. */
  override note(event: DispatchEvent): void {
    if (event.type.startsWith("issue.")) this.noteIssue(event);
    this.noteCreation(event);
    for (const askId of askIdsOf(event)) this.lastEvents.set(askId, event.id);
    super.note(event);
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

/** A request's ask hits, the title's before the question's, in the order the pages give them. */
function hitsOf(search: RequestSearch): QueryHit[] {
  return [...search.title.hits, ...search.question.hits];
}

/**
 * A candidate the reserve kept, as of the judged moment, from the two walks: dropped where its
 * ask has an event after the moment, else its fields as its last whole payload left them and its
 * issue's title as of the moment. `afterPin` holds the asks the drop scan found an event of after
 * the pin.
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
 * The drop scan: the log read on from the pin, and of each event nothing but the ask it is an
 * event of (`askIdsOf`). It reads through `recorded`, the end an earlier run of this claim run
 * recorded, where there is one, and on from there to the log's newest event where `onward` (a
 * first scan, or a resume that searched anything its first run did not). Returns those asks and
 * the scan's last event id.
 */
async function dropScanOf(
  pin: number,
  recorded: number | null,
  onward: boolean
): Promise<{ asks: Set<string>; through: number }> {
  const asks = new Set<string>();
  const onPage = (events: readonly DispatchEvent[]): void => {
    for (const event of events) for (const askId of askIdsOf(event)) asks.add(askId);
  };
  let through = recorded ?? pin;
  if (through > pin) await pageEventLog({ after: pin, through, onPage });
  if (onward) {
    const walk = await pageEventLog({ after: through, onPage });
    through = Math.max(through, walk.lastEventId);
  }
  return { asks, through };
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
  if (walk.eventsRead !== corpus.snapshot.eventsRead) {
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

/**
 * One document version as `replay.ts` reads it (`readVersion`), whose markdown must hash to the
 * sha256 the route answers: its markdown and that sha256, or why the request is a deviation. Only
 * the version's bytes are read here; its name is the request's own.
 */
async function versionOf(
  document: AskDocument,
  config: DispatchConfig
): Promise<{ markdown: string; sha256: string } | string> {
  const read = await readVersion(document.artifact_id, document.version, config);
  if ("refusal" in read) return read.refusal;
  const sha256 = stringOf(read.version.sha256);
  if (sha256 === null) return `GET ${read.path} answered no sha256`;
  if (sha256Of(read.markdown) !== sha256) {
    return `GET ${read.path} answered markdown whose sha256 is not the ${sha256} it names`;
  }
  return { markdown: read.markdown, sha256 };
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

/** A request's candidate counts and the end of the drop scan its candidates were dropped by. */
interface Scanned {
  counts: CandidateCounts;
  dropScanThrough: number;
}

/**
 * A request's line: ready with its file's sha256, or a deviation with its reason, which a request
 * whose own inputs stood takes from a document or a candidate read after them; with its counts
 * and scan end where it was searched.
 */
function lineOf(
  plan: Ready | Deviation,
  sha256: string | null,
  scanned: Scanned | null,
  reason: string | null = plan.state === "deviation" ? plan.reason : null
): InventoryLine {
  return {
    askId: plan.request.askId,
    issueKey: plan.request.issueKey,
    group: plan.request.group,
    cases: plan.request.rows.length,
    judged: plan.judged,
    state: reason === null ? "ready" : "deviation",
    reason,
    requestSha256: sha256,
    counts: scanned?.counts ?? null,
    dropScanThrough: scanned?.dropScanThrough ?? null,
  };
}

/** What every phase of one run reads: the build, its chain, the inventory and the stage. */
interface Run {
  corpus: Corpus;
  chain: StageChain;
  inventory: InventoryRequest[];
  stage: Stage;
  decision: Decision;
}

/**
 * The run's start: the decision checked first, so a checkout whose `reserve-ask.ts` closure is not
 * the registered one reads nothing; then the claim run's start every later stage shares
 * (`claimStartOf`: the admission before any file is read, the build under that read, the draw held
 * to the build), whose snapshot must record the events it read; the chain; the inventory; and the
 * stage directory, started or resumed.
 */
async function openRun(flags: Flags, options: InputsOptions): Promise<Run> {
  const decision = options.decision ?? (await reserveAskDecision());
  if (!/^[0-9a-f]{64}$/.test(decision.sha256)) {
    throw new S10InputsError("the decision handed in names itself by no sha256");
  }
  const directory = await outDirectory(flags.out);
  const { corpus, reference, build, draw, runDigest } = await claimStartOf(
    flags.corpus,
    flags.draw,
    { registration: options.registration, root: options.root }
  );
  if (corpus.snapshot.eventsRead <= 0) {
    // `readCorpus` reads a snapshot that records none as having read none; a build of the claim
    // run always records it, so no walk here can show it reads the build's log without it.
    throw new S10InputsError(
      `${join(corpus.directory, "snapshot.json")} records no events read, so no walk of the log ` +
        "here can show it is the log the build read"
    );
  }
  const chain: StageChain = {
    claimRun: build,
    reference,
    runDigest,
    person: corpus.snapshot.person,
    throughEventId: corpus.snapshot.throughEventId,
    draw: { drawSha256: draw.drawSha256, slicesSha256: draw.slicesSha256 },
    reserveAsk: decision.sha256,
    directories: { build: corpus.directory, draw: draw.directory },
  };
  const inventory = inventoryOf(corpus.byCaseId, draw.trees, draw.report);
  const stage = await Stage.open(directory, chain);
  return { corpus, chain, inventory, stage, decision };
}

/** The first walk: nothing of any ask outside the inventory but the id of its last event. */
async function firstWalkOf(run: Run): Promise<InputsLog> {
  const log = new InputsLog(
    run.corpus.snapshot.person,
    run.inventory.map((request) => request.askId)
  );
  const eventsRead = await walkThroughPin(run.corpus, (event) => log.note(event));
  console.log(`log               ${eventsRead} events through ${run.chain.throughEventId}`);
  return log;
}

/**
 * A request file a resume takes, already held to the bytes its record names, held to this run's
 * reading too: the request this run judges, at that same moment and under the same fact. Those
 * bytes passed `assertNoAnswer` before its first run wrote them, under the same chain, so they
 * are not held to it again. Gives back the plan and the file's counts and scan end, null for a
 * request the fact holds.
 */
async function takenRequest(
  stage: Stage,
  askId: string,
  plan: Ready | Deviation | undefined
): Promise<{ plan: Ready; scanned: Scanned | null }> {
  const path = requestPath(stage.directory, askId);
  if (plan === undefined || plan.state !== "ready") {
    const reads = plan === undefined ? "holds no request" : "reads a deviation";
    throw new S10InputsError(`the stage record names ${path}, and this run ${reads} of that ask`);
  }
  const inputs = requestInputsOf(await readFile(path, "utf8"), path);
  if (inputs.askId !== askId || inputs.judged.eventId !== plan.judged.eventId) {
    throw new S10InputsError(
      `${path} is not the request this run judges at event ${plan.judged.eventId}`
    );
  }
  if (inputs.factHold !== plan.fact.holds) {
    throw new S10InputsError(`${path} records another legion_fact than this run reads`);
  }
  const { counts, dropScanThrough } = inputs;
  const scanned = counts === null || dropScanThrough === null ? null : { counts, dropScanThrough };
  return { plan, scanned };
}

/**
 * The requests an earlier run of this claim run wrote, each taken (`takenRequest`) and never
 * written again, and the deviations, each lined into `lines`; gives back the ready requests left
 * to work on.
 */
async function pendingOf(
  stage: Stage,
  plans: ReadonlyMap<string, Ready | Deviation>,
  lines: Map<string, InventoryLine>
): Promise<Ready[]> {
  for (const [askId, sha256] of Object.entries(stage.record.requests)) {
    const taken = await takenRequest(stage, askId, plans.get(askId));
    lines.set(askId, lineOf(taken.plan, sha256, taken.scanned));
  }
  const pending: Ready[] = [];
  for (const plan of plans.values()) {
    if (lines.has(plan.request.askId)) continue;
    if (plan.state === "deviation") lines.set(plan.request.askId, lineOf(plan, null, null));
    else pending.push(plan);
  }
  return pending;
}

/** A ready request with every document it names read. */
interface Readable {
  plan: Ready;
  documents: RequestDocument[];
}

/**
 * Each document version the pending requests name, read once, each request naming it itself; a
 * request one of whose versions could not be read is lined into `lines` as a deviation.
 */
async function readableOf(
  pending: readonly Ready[],
  config: DispatchConfig,
  lines: Map<string, InventoryLine>
): Promise<Readable[]> {
  const versions = new Map<string, { markdown: string; sha256: string } | string>();
  const wanted = new Map<string, AskDocument>();
  for (const plan of pending) {
    for (const document of plan.documents) {
      wanted.set(`${document.artifact_id}@${document.version}`, document);
    }
  }
  await pool([...wanted.entries()], DOCUMENT_CONCURRENCY, async ([key, document]) => {
    versions.set(key, await versionOf(document, config));
  });
  const readable: Readable[] = [];
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
  return readable;
}

/** The searches of the requests searched for neighbours, by ask id, and the pages they use. */
interface Searched {
  byAsk: Map<string, RequestSearch>;
  /** The sha256 of each page the searches use, read in this run or taken from an earlier one. */
  pages: Set<string>;
}

/** S10's two queries of each request searched for neighbours, each query once. */
async function searchedOf(
  stage: Stage,
  config: DispatchConfig,
  searchable: readonly Readable[]
): Promise<Searched> {
  const searches = new Searches(stage, config, await Searches.archived(stage));
  const queries = new Set<string>();
  for (const { plan } of searchable) {
    queries.add(plan.queries.title);
    queries.add(plan.queries.question);
  }
  const answered = new Map<string, QueryPages>();
  await pool([...queries], SEARCH_CONCURRENCY, async (query) => {
    answered.set(query, await searches.pagesOf(query));
  });
  console.log(`queries           ${queries.size}, ${searches.read} pages read in this run`);
  const byAsk = new Map<string, RequestSearch>();
  const pages = new Set<string>();
  for (const { plan } of searchable) {
    const { title, question } = plan.queries;
    const search = {
      title: queryInputsOf(title, answered.get(title) ?? []),
      question: queryInputsOf(question, answered.get(question) ?? []),
    };
    byAsk.set(plan.request.askId, search);
    for (const page of [...search.title.pages, ...search.question.pages]) pages.add(page.sha256);
  }
  return { byAsk, pages };
}

/**
 * The drop scan, once the searches are done: an event of a candidate's ask between the pin and
 * the searches drops it too. It reads through the recorded end, and on from it to the log's
 * newest event where there is none, or where a page the searches use (`used`) is not one the
 * recorded scan covers, as the page record's file stood then: a page read after that scan, in
 * this run or in one that stopped before it recorded its scan. The new end is recorded with every
 * page now archived covered, in one write of the stage record, before any request file is
 * written. Gives back the asks with an event after the pin, and the scan's end.
 */
async function afterPinOf(
  stage: Stage,
  pin: number,
  used: ReadonlySet<string>
): Promise<{ asks: Set<string>; through: number }> {
  const recorded = stage.record.dropScanThrough;
  const covered = stage.record.dropScanPages ?? {};
  const uncovered = [...used].filter(
    (page) => !Object.hasOwn(covered, page) || covered[page] !== stage.record.pages[page]
  );
  const onward = recorded === null || uncovered.length > 0;
  const scan = await dropScanOf(pin, recorded, onward);
  if (onward) {
    const archived = Object.entries(stage.record.pages).sort(([one], [other]) =>
      one < other ? -1 : 1
    );
    stage.record.dropScanThrough = scan.through;
    stage.record.dropScanPages = Object.fromEntries(archived);
    await stage.save();
  }
  console.log(
    `drop scan         events ${pin} to ${scan.through}: ` +
      `${counted(scan.asks.size, "ask")} with an event after the pin` +
      (recorded !== null && onward ? `, on from ${recorded} for ${uncovered.length} pages` : "")
  );
  return scan;
}

/**
 * Every ask among the hits, decided under the reserve before any field of it is read, the
 * decision confirmed (`Decision.confirm`) right before its first child is spawned.
 */
async function verdictsOf(
  searches: Iterable<RequestSearch>,
  decision: Decision
): Promise<Map<string, ReserveVerdict>> {
  const candidates = new Set<string>();
  for (const search of searches) for (const hit of hitsOf(search)) candidates.add(hit.id);
  const verdicts = new Map<string, ReserveVerdict>();
  if (candidates.size > 0) await decision.confirm();
  await pool([...candidates], DECISION_CONCURRENCY, async (askId) => {
    verdicts.set(askId, await decision.decide(askId));
  });
  console.log(`candidates        ${candidates.size} decided by reserve-ask.ts`);
  return verdicts;
}

/** What a request's neighbours are read from: the searches, the decisions and the two walks. */
interface Neighbourhood {
  person: string;
  searches: ReadonlyMap<string, RequestSearch>;
  verdicts: ReadonlyMap<string, ReserveVerdict>;
  log: InputsLog;
  candidates: CandidateLog;
  afterPin: ReadonlySet<string>;
  /** The drop scan's end, null where no request was searched and no scan ran. */
  scanThrough: number | null;
}

/**
 * The searches of the requests searched for neighbours, the drop scan after them, the reserve's
 * decisions on every hit, and the second walk, which reads the fields of the candidates the
 * reserve kept and of no other ask.
 */
async function neighbourhoodOf(
  run: Run,
  log: InputsLog,
  config: DispatchConfig,
  searchable: readonly Readable[]
): Promise<Neighbourhood> {
  const searched = await searchedOf(run.stage, config, searchable);
  const scan =
    searchable.length === 0
      ? { asks: new Set<string>(), through: null }
      : await afterPinOf(run.stage, run.chain.throughEventId, searched.pages);
  const afterPin = scan.asks;
  const verdicts = await verdictsOf(searched.byAsk.values(), run.decision);
  const kept = new Set(
    [...verdicts.entries()]
      .filter(([askId, verdict]) => verdict === "kept" && log.lastEventOf(askId) !== undefined)
      .map(([askId]) => askId)
      .filter((askId) => !afterPin.has(askId))
  );
  const candidates = new CandidateLog(kept);
  if (kept.size > 0) await walkThroughPin(run.corpus, (event) => candidates.note(event));
  const person = run.chain.person;
  const searches = searched.byAsk;
  return { person, searches, verdicts, log, candidates, afterPin, scanThrough: scan.through };
}

/** One request's neighbours: each hit as of the judged moment, the counts, and the first fault. */
interface Neighbours extends Scanned {
  search: RequestSearch;
  outcomes: Record<string, CandidateInputs>;
  /** The first field S10's `neighbours` reads that the log cannot give, and its candidate. */
  fault: { askId: string; field: string } | null;
}

/** One searched request's neighbours, from its neighbourhood. */
function neighboursOf(plan: Ready, hood: Neighbourhood): Neighbours {
  const search = hood.searches.get(plan.request.askId);
  if (search === undefined || hood.scanThrough === null) {
    throw new S10InputsError(`the request ${plan.request.askId} was never searched`);
  }
  const hits = hitsOf(search);
  const outcomes: Record<string, CandidateInputs> = {};
  const counts = noCounts();
  counts.askHits = hits.length;
  for (const hit of hits) {
    if (Object.hasOwn(outcomes, hit.id)) continue;
    const verdict = hood.verdicts.get(hit.id);
    if (verdict === undefined) {
      throw new S10InputsError(`the ask ${hit.id} among the hits was never decided`);
    }
    counts.candidates += 1;
    if (verdict !== "kept") {
      outcomes[hit.id] = { held: true, reserve: verdict };
      counts.held += 1;
      if (verdict === "error") counts.reserveErrors += 1;
      continue;
    }
    const moment = plan.judged.eventId;
    const outcome = candidateOf(hit.id, hood.log, hood.candidates, moment, hood.afterPin);
    outcomes[hit.id] = outcome;
    if ("dropped" in outcome) counts.droppedLaterEvent[outcome.seen] += 1;
    else counts.kept += 1;
  }
  const fault = neighbourFault(neighbourOrder(hits), outcomes, {
    person: hood.person,
    issueKey: plan.issue.key,
    openedAt: plan.openedAt,
    documents: new Set(plan.documents.map((document) => document.artifact_id)),
  });
  return { search, outcomes, counts, dropScanThrough: hood.scanThrough, fault };
}

/**
 * Each readable request's file, held to `assertNoAnswer` and committed, or its deviation, lined
 * into `lines`: a request its fact holds has no neighbours; another whose neighbours fault, or
 * whose inputs would carry the answer, is a deviation and no file is written.
 */
async function writeRequests(
  stage: Stage,
  readable: readonly Readable[],
  hood: Neighbourhood,
  lines: Map<string, InventoryLine>
): Promise<void> {
  for (const { plan, documents } of readable) {
    const neighbours = plan.fact.holds ? null : neighboursOf(plan, hood);
    const fault = neighbours?.fault ?? null;
    if (fault !== null) {
      const reason =
        `a candidate: the log gives no ${fault.field} of the ask ${fault.askId} as of event ` +
        `${plan.judged.eventId}, which S10's neighbours reads`;
      lines.set(plan.request.askId, lineOf(plan, null, neighbours, reason));
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
      counts: neighbours?.counts ?? null,
      dropScanThrough: neighbours?.dropScanThrough ?? null,
    };
    let text: string;
    try {
      text = assertNoAnswer(request, guardOf(plan));
    } catch (error) {
      if (!(error instanceof LeakError)) throw error;
      // Inputs that would carry the answer are not the request as it stood: the file is not
      // written, and the reason names the rule and none of the text that broke it.
      const reason = "its inputs would carry the answer";
      lines.set(plan.request.askId, lineOf(plan, null, neighbours, reason));
      continue;
    }
    const sha256 = await stage.commit("requests", plan.request.askId, text);
    lines.set(plan.request.askId, lineOf(plan, sha256, neighbours));
  }
}

/** The inventory's lines in its order, every request lined by one phase or another. */
function orderedLines(
  inventory: readonly InventoryRequest[],
  lines: ReadonlyMap<string, InventoryLine>
): InventoryLine[] {
  return inventory.map((request) => {
    const line = lines.get(request.askId);
    if (line === undefined) {
      throw new S10InputsError(`the request ${request.askId} was never lined`);
    }
    return line;
  });
}

/**
 * The run's end: `inventory.jsonl` written whole, and the stage record with the counts, the sums
 * of the inventory's lines, and the inventory's sha256. Gives back the counts.
 */
async function finishRun(stage: Stage, lines: readonly InventoryLine[]): Promise<StageCounts> {
  const text = lines.map((line) => `${JSON.stringify(line)}\n`).join("");
  await writeWhole(join(stage.directory, INVENTORY_FILE), text);
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
    for (const field of COUNT_FIELDS) counts[field] += line.counts[field];
    for (const seen of DROP_SEEN) {
      counts.droppedLaterEvent[seen] += line.counts.droppedLaterEvent[seen];
    }
  }
  await stage.finish(counts, sha256Of(text));
  return counts;
}

async function inputs(flags: Flags, options: InputsOptions): Promise<number> {
  const run = await openRun(flags, options);
  console.log(`inputs            ${run.stage.directory}`);
  console.log(`claim run         ${run.chain.claimRun.id}, under ${run.chain.reference}`);
  console.log(`requests          ${run.inventory.length} in the inventory`);
  if (run.inventory.length === 0) {
    await finishRun(run.stage, []);
    console.error("no request: the draw's report trees hold no approval case");
    return 1;
  }
  const log = await firstWalkOf(run);
  const plans = new Map(run.inventory.map((request) => [request.askId, planOf(request, log)]));
  const lines = new Map<string, InventoryLine>();
  const pending = await pendingOf(run.stage, plans, lines);
  const config = await dispatchConfig();
  const readable = await readableOf(pending, config, lines);
  // S10 reads no neighbour of a request its fact holds (`prepare` returns the hold first), so
  // such a request is searched for nothing and none of its hits is decided.
  const searchable = readable.filter(({ plan }) => !plan.fact.holds);
  const hood = await neighbourhoodOf(run, log, config, searchable);
  await writeRequests(run.stage, readable, hood, lines);
  const ordered = orderedLines(run.inventory, lines);
  const totals = await finishRun(run.stage, ordered);
  const dropped = DROP_SEEN.reduce((total, seen) => total + totals.droppedLaterEvent[seen], 0);
  console.log(
    `written           ${counted(totals.ready, "request")} ready, ` +
      `${counted(totals.deviations, "deviation")}; ${dropped} candidates dropped by an event ` +
      `after the judged moment, ${totals.held} held (${totals.reserveErrors} undecided)`
  );
  for (const line of ordered) {
    if (line.state === "deviation") console.log(`  deviation ${line.askId}: ${line.reason}`);
  }
  console.log(`                  ${join(run.stage.directory, INVENTORY_FILE)}`);
  console.log(`                  ${join(run.stage.directory, STAGE_FILE)}`);
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

// Only when run: a test that imports `assertNoAnswer` or the logs must not write any inputs.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
