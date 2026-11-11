import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import {
  listOf,
  numberOf,
  OutputPathError,
  optionsOf,
  outDirectory,
  recordOf,
  stringOf,
  UsageError,
  verdictOf,
} from "./corpus-io";
import {
  DispatchConfigError,
  type DispatchEvent,
  DispatchRefusedError,
  DispatchUnreachableError,
  pageEventLog,
} from "./dispatch-read";

/**
 * Builds the corpus of one person's approval decisions: every ask that person answered, up to a
 * pinned event id, with the facts the answer could have turned on.
 *
 * The source is Dispatch's own event log, walked once from the start through the pin, so a build
 * pinned to the same id reads the same events however much the log grew since. A person's own
 * answer is the rule Dispatch itself reads answers back with (`answerEventsQuery` in
 * `packages/envoy/internal/dispatch/api/asks.go`): an `ask.answered` event whose actor is a user,
 * whose actor is the user the answer names, and whose actor is this person. A document
 * settlement can write the same answer again under its editor, so the repeats of one answer (its
 * ask and its own `at`) collapse to the earliest event; an answer the person changed carries a
 * later `at` and is a second case.
 *
 * Nothing it writes goes into this repository: `--out` is an absolute path outside the workspace,
 * since a corpus of a production log is large and the workspace is snapshotted whole.
 */

const USAGE = `usage: bun scripts/approval-eval/build-corpus.ts --person <login> --out <dir> \\
  [--through <event id>]

Builds one person's approval corpus from Dispatch's event log.

Options:
  --person <login>     Required. The login whose own answers make the cases.
  --out <dir>          Required. Where corpus.jsonl, snapshot.json and counts.json are written.
                       An absolute path outside this repository's workspace.
  --through <event id> The last event of the walk, which pins the build. With none, the log is
                       walked once to learn its newest event id, and that id is the pin.

Exit status: 0 when the corpus was built, 1 when it holds no case, 2 when the build could not
run.`;

/** The event types a later script may read back from a kept walk. */
const KEPT_TYPES: Readonly<Record<string, true>> = {
  "ask.opened": true,
  "ask.edited": true,
  "ask.answered": true,
  "ask.resolved": true,
  "artifact.approved": true,
  "artifact.changes_requested": true,
};

/** The person's own working day, which is what an hour of the answer is read against. */
const PACIFIC = "America/Los_Angeles";
const PACIFIC_HOUR = new Intl.DateTimeFormat("en-US", {
  timeZone: PACIFIC,
  hour: "numeric",
  hourCycle: "h23",
});

/** What an ask asked, as the answered event holds it. */
export interface AskOption {
  id: string | null;
  label: string | null;
  description: string | null;
}

/** One document an approval request covered. A request of one document holds exactly one. */
export interface ApprovalDocument {
  artifactId: string | null;
  name: string | null;
  version: number | null;
  requestedVersion: number | null;
}

/** What an approval ask asked to approve: its documents, and the artifact the ask hangs on. */
export interface CaseApproval {
  documents: ApprovalDocument[];
  artifactSlug: string | null;
  artifactPrimary: boolean | null;
}

/** What a judge must not see: the facts of the case beside its words. */
export interface CaseConfounds {
  version: number | null;
  answersThatDayBefore: number;
  hourUtc: number;
  hourPacific: number;
  changesAlreadyRequested: boolean;
  priorCasesOnDocument: number;
  caseKind: CaseKind;
}

/** An approval ask asks to approve a version of a document; every other ask is a question. */
export type CaseKind = "approval" | "question";

/** What the person decided on an approval ask, read from the option they chose. */
export type CaseVerdict = "approve" | "request_changes" | "other";

/** One answer of the person: one line of `corpus.jsonl`. */
export interface CaseRow {
  caseId: string;
  eventId: number;
  askId: string;
  issueKey: string | null;
  project: string | null;
  kind: CaseKind;
  urgency: string | null;
  multiple: boolean;
  question: string | null;
  options: AskOption[];
  selected: string[];
  words: string | null;
  hasWords: boolean;
  askCreatedAt: string | null;
  answeredAt: string;
  eventCreatedAt: string;
  approval: CaseApproval | null;
  verdict: CaseVerdict | null;
  confounds: CaseConfounds;
}

/** A case before the whole corpus is known, which is what its confounds are counted over. */
type CaseSeed = Omit<CaseRow, "confounds">;

/** What the build read, so a later run can tell one corpus from another. */
interface Snapshot {
  person: string;
  throughEventId: number;
  eventsRead: number;
  keptEvents: number;
  readStartedAt: string;
  readFinishedAt: string;
}

/** The shape of the corpus, by kind and by verdict. */
interface Counts {
  total: number;
  byKind: Record<CaseKind, number>;
  approvalByVerdict: Record<CaseVerdict, number>;
  withWordsByKind: Record<CaseKind, number>;
}

/** The flags as `parseArgs` reads them, before their rules are checked. */
interface Flags {
  person: string;
  out: string;
  through: number | undefined;
}

function readFlags(argv: readonly string[]): Flags {
  let values: { person?: string; out?: string; through?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        person: { type: "string" },
        out: { type: "string" },
        through: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const person = values.person?.trim();
  const out = values.out?.trim();
  if (!person || !out) throw new UsageError(USAGE);
  let through: number | undefined;
  if (values.through !== undefined) {
    through = Number(values.through.trim());
    if (!Number.isSafeInteger(through) || through <= 0) {
      throw new UsageError(`--through takes an event id, a whole number above zero\n\n${USAGE}`);
    }
  }
  return { person, out, through };
}

function documentOf(value: Record<string, unknown>): ApprovalDocument {
  return {
    artifactId: stringOf(value.artifact_id),
    name: stringOf(value.name),
    version: numberOf(value.version),
    requestedVersion: numberOf(value.requested_version),
  };
}

/**
 * What the request asked to approve. A request covering more than one document lists them under
 * `documents`; a request of one document carries that one in the lead fields of `approval`.
 */
function approvalOf(payload: Record<string, unknown>): CaseApproval {
  const approval = recordOf(payload.approval);
  const listed = listOf(approval?.documents)
    .map(recordOf)
    .filter((entry): entry is Record<string, unknown> => entry !== null)
    .map(documentOf);
  const documents = listed.length > 0 ? listed : approval ? [documentOf(approval)] : [];
  const artifact = recordOf(payload.approval_artifact);
  return {
    documents,
    artifactSlug: stringOf(artifact?.slug),
    artifactPrimary: typeof artifact?.primary === "boolean" ? artifact.primary : null,
  };
}

/**
 * The person's own answer this event records, or null. The rule is Dispatch's own: a user actor,
 * who is the user the answer names, who is this person.
 */
function caseOf(event: DispatchEvent, person: string): CaseSeed | null {
  if (event.type !== "ask.answered") return null;
  const actor = event.actor;
  if (actor === null || actor.kind !== "user" || actor.id !== person) return null;
  const payload = event.payload;
  const answer = recordOf(payload.answer);
  if (answer === null || stringOf(answer.user) !== actor.id) return null;
  const askId = stringOf(payload.id);
  const answeredAt = stringOf(answer.at);
  if (askId === null || answeredAt === null) return null;
  const kind: CaseKind = stringOf(payload.kind) === "approval" ? "approval" : "question";
  const options = optionsOf(payload.options);
  const selected = listOf(answer.selected)
    .map(stringOf)
    .filter((entry): entry is string => entry !== null);
  const words = stringOf(answer.text);
  return {
    caseId: `${askId}@${answeredAt}`,
    eventId: event.id,
    askId,
    issueKey: stringOf(payload.issue_key) ?? event.issue_key,
    project: event.project,
    kind,
    urgency: stringOf(payload.urgency),
    multiple: payload.multiple === true,
    question: stringOf(payload.question),
    options,
    selected,
    words,
    hasWords: words !== null && words.trim() !== "",
    askCreatedAt: stringOf(payload.created_at),
    answeredAt,
    eventCreatedAt: event.created_at,
    approval: kind === "approval" ? approvalOf(payload) : null,
    verdict: kind === "approval" ? verdictOf(selected, options) : null,
  };
}

function hourPacificOf(at: Date): number {
  const part = PACIFIC_HOUR.formatToParts(at).find((entry) => entry.type === "hour");
  const hour = part === undefined ? Number.NaN : Number(part.value);
  return Number.isFinite(hour) ? hour % 24 : Number.NaN;
}

/**
 * The corpus in the order it is written, each case carrying what it could have turned on: how
 * much the person had already answered that day, the hour where they were, and what this
 * document had already been told.
 */
function withConfounds(seeds: readonly CaseSeed[]): CaseRow[] {
  const ordered = [...seeds].sort(
    (left, right) =>
      Date.parse(left.answeredAt) - Date.parse(right.answeredAt) || left.eventId - right.eventId
  );
  const dayTimes = new Map<string, number[]>();
  const casesOnDocument = new Map<string, number>();
  const changesRequestedAt = new Map<string, number>();
  const rows: CaseRow[] = [];
  for (const seed of ordered) {
    const at = new Date(seed.answeredAt);
    const time = at.getTime();
    const day = at.toISOString().slice(0, 10);
    const times = dayTimes.get(day) ?? [];
    const lead = seed.approval?.documents[0] ?? null;
    const document = lead?.artifactId ?? null;
    const changedAt = document === null ? undefined : changesRequestedAt.get(document);
    rows.push({
      ...seed,
      confounds: {
        version: lead?.requestedVersion ?? null,
        answersThatDayBefore: times.filter((earlier) => earlier < time).length,
        hourUtc: at.getUTCHours(),
        hourPacific: hourPacificOf(at),
        changesAlreadyRequested: changedAt !== undefined && changedAt < time,
        priorCasesOnDocument: document === null ? 0 : (casesOnDocument.get(document) ?? 0),
        caseKind: seed.kind,
      },
    });
    times.push(time);
    dayTimes.set(day, times);
    if (document !== null) {
      casesOnDocument.set(document, (casesOnDocument.get(document) ?? 0) + 1);
      if (seed.verdict === "request_changes" && changedAt === undefined) {
        changesRequestedAt.set(document, time);
      }
    }
  }
  return rows;
}

function countsOf(rows: readonly CaseRow[]): Counts {
  const counts: Counts = {
    total: rows.length,
    byKind: { approval: 0, question: 0 },
    approvalByVerdict: { approve: 0, request_changes: 0, other: 0 },
    withWordsByKind: { approval: 0, question: 0 },
  };
  for (const row of rows) {
    counts.byKind[row.kind] += 1;
    if (row.kind === "approval" && row.verdict !== null) counts.approvalByVerdict[row.verdict] += 1;
    if (row.hasWords) counts.withWordsByKind[row.kind] += 1;
  }
  return counts;
}

function report(counts: Counts, snapshot: Snapshot, directory: string): string {
  const verdicts = counts.approvalByVerdict;
  return [
    `corpus for ${snapshot.person} through event ${snapshot.throughEventId}`,
    `  events read            ${snapshot.eventsRead}`,
    `  events kept            ${snapshot.keptEvents}`,
    `  cases                  ${counts.total}`,
    `    approval             ${counts.byKind.approval}` +
      ` (approve ${verdicts.approve}, request_changes ${verdicts.request_changes},` +
      ` other ${verdicts.other})`,
    `    question             ${counts.byKind.question}`,
    `  cases with their words ${counts.withWordsByKind.approval} approval,` +
      ` ${counts.withWordsByKind.question} question`,
    `  written to             ${directory}`,
  ].join("\n");
}

async function build(flags: Flags): Promise<number> {
  const directory = await outDirectory(flags.out);
  const readStartedAt = new Date().toISOString();
  let through = flags.through;
  if (through === undefined) {
    // The log's own end pins a build the caller gave no id, so the walk that reads it repeats.
    through = (await pageEventLog({ onPage: () => {} })).lastEventId;
    if (through === 0) {
      console.error("the event log holds no event: there is nothing to pin a corpus to");
      return 1;
    }
    console.log(`no --through given: pinned to the log's newest event, ${through}`);
  }
  const seeds = new Map<string, CaseSeed>();
  let keptEvents = 0;
  const walk = await pageEventLog({
    through,
    onPage: (events) => {
      for (const event of events) {
        if (KEPT_TYPES[event.type] !== true) continue;
        keptEvents += 1;
        const seed = caseOf(event, flags.person);
        // The repeats of one answer collapse to the earliest event, and the walk is in id order.
        if (seed !== null && !seeds.has(seed.caseId)) seeds.set(seed.caseId, seed);
      }
    },
  });
  const readFinishedAt = new Date().toISOString();
  const rows = withConfounds([...seeds.values()]);
  const counts = countsOf(rows);
  const snapshot: Snapshot = {
    person: flags.person,
    throughEventId: through,
    eventsRead: walk.eventsRead,
    keptEvents,
    readStartedAt,
    readFinishedAt,
  };
  await mkdir(directory, { recursive: true });
  await writeFile(
    join(directory, "corpus.jsonl"),
    rows.map((row) => `${JSON.stringify(row)}\n`).join(""),
    "utf8"
  );
  await writeFile(join(directory, "snapshot.json"), `${JSON.stringify(snapshot, null, 2)}\n`);
  await writeFile(join(directory, "counts.json"), `${JSON.stringify(counts, null, 2)}\n`);
  console.log(report(counts, snapshot, directory));
  if (rows.length === 0) {
    console.error(`no case: ${flags.person} answered no ask through event ${through}`);
    return 1;
  }
  return 0;
}

async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return 2;
  }
  try {
    return await build(flags);
  } catch (error) {
    if (
      error instanceof OutputPathError ||
      error instanceof DispatchConfigError ||
      error instanceof DispatchRefusedError ||
      error instanceof DispatchUnreachableError
    ) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

process.exit(await main(Bun.argv.slice(2)));
