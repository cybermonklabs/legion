import { parseArgs } from "node:util";
import type { AskOption, CaseKind, CaseRow, CaseVerdict } from "./build-corpus";
import {
  CannotRunError,
  type Corpus,
  listOf,
  numberOf,
  optionsOf,
  pool,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
  verdictOf,
} from "./corpus-io";
import {
  type DispatchConfig,
  DispatchRefusedError,
  dispatchConfig,
  readJson,
} from "./dispatch-read";

/**
 * Counts a built corpus a second time, by another route, and says whether the two readings agree.
 *
 * The build walked the global event log once. This reads the same cases from the two routes that
 * answer an ask and an owner's own log: `GET /api/v1/asks/{id}` for each ask the corpus names,
 * and `GET /api/v1/issues/{key}/events` for each issue those asks hang on. Nothing here reads
 * `corpus.jsonl` for anything but the ask ids and the cases to compare against, so a case the
 * build invented or dropped shows up as a difference and not as agreement.
 *
 * Both readings must be of one snapshot, not of two clock times. How the pin is applied to each
 * route was decided from what each route answers, measured against this server:
 *
 *   - `GET /api/v1/asks/{id}` answers the ask as it stands now. Its `answers` carry the user,
 *     what was selected, the words and `at`, and no event id: the only event id in the body is
 *     `opened_event_id`, the ask's opening. So this route alone cannot say whether an answer was
 *     written before or after the pin, and an answer's time is no substitute, since `at` is the
 *     moment the answer was taken and not the moment the log recorded it.
 *   - `GET /api/v1/issues/{key}/events` answers that issue's own log. Every row carries the
 *     global event `id` the pinned walk counted by, and a per-issue `seq` the page is read by
 *     (`?after=<seq>`, `?limit=` at most 200: `maxEventPage` in the handler). Its `ask.answered`
 *     rows carry the same `answer.at` the ask route reports, so an answer is placed in the log
 *     by its ask id and its `at`.
 *
 * An answer is therefore counted only when that log holds an `ask.answered` row for it whose id
 * is at or below `snapshot.throughEventId`. An answer with no such row was written after the pin
 * (or is not in the log the second route walks at all), and it is left out and named in the
 * differences. A settlement can write one answer twice, so the earliest row for an (ask, `at`)
 * is the one the pin is read against: the same collapse the build makes.
 *
 * The few asks that hang on a document rather than an issue (`issue_key` null, `artifact_id`
 * set) are dated the same way from `GET /api/v1/artifacts/{id}/events`, which is the same
 * handler over the other owner and answers rows of the same shape.
 */

const USAGE = `usage: bun scripts/approval-eval/recount.ts --corpus <dir>

Counts a built corpus a second time, from the ask and event-log routes, and compares.

Options:
  --corpus <dir>  Required. A directory holding corpus.jsonl and snapshot.json.

Exit status: 0 when the two readings agree, 1 when they differ, 2 when the recount could not
run.`;

/** How many reads run at once, which is what the shared client does for a batch. */
const READ_CONCURRENCY = 8;
/** One page of an owner's event log, which is that route's own maximum. */
const EVENT_PAGE_SIZE = 200;
/** How many differences are printed before the rest are only counted. */
const DIFFERENCES_PRINTED = 50;

interface Flags {
  corpus: string;
}

/** One answer of one ask, as the ask's own route reports it. */
interface AskAnswer {
  user: string | null;
  selected: string[];
  text: string | null;
  at: string | null;
}

/** One ask, as the ask's own route reports it. */
interface AskRead {
  askId: string;
  issueKey: string | null;
  artifactId: string | null;
  kind: CaseKind;
  options: AskOption[];
  answers: AskAnswer[];
}

/** Whose event log one walk reads: an issue, or the document an ask hangs on instead. */
interface Owner {
  kind: "issue" | "document";
  id: string;
}

/** One case of the second reading: what the two routes together say the person decided. */
interface SecondCase {
  caseId: string;
  kind: CaseKind;
  verdict: CaseVerdict | null;
  hasWords: boolean;
  eventId: number;
}

/** What the second reading saw, and what it left out and why. */
interface SecondReading {
  cases: Map<string, SecondCase>;
  /** By case id: why an answer the ask route reported is not a case of this snapshot. */
  leftOut: Map<string, string>;
}

/** The shape of one reading, counted the way the build counts it. */
interface Counts {
  total: number;
  byKind: Record<CaseKind, number>;
  approvalByVerdict: Record<CaseVerdict, number>;
  withWordsByKind: Record<CaseKind, number>;
}

/** One case the two readings do not say the same thing about. */
interface Difference {
  caseId: string;
  build: string;
  second: string;
}

function readFlags(argv: readonly string[]): Flags {
  let values: { corpus?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: { corpus: { type: "string" } },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  if (!corpus) throw new UsageError(USAGE);
  return { corpus };
}

function answersOf(value: unknown): AskAnswer[] {
  return listOf(value).map((entry) => {
    const answer = recordOf(entry);
    return {
      user: stringOf(answer?.user),
      selected: listOf(answer?.selected)
        .map(stringOf)
        .filter((selection): selection is string => selection !== null),
      text: stringOf(answer?.text),
      at: stringOf(answer?.at),
    };
  });
}

function askOf(askId: string, body: unknown): AskRead {
  const ask = recordOf(recordOf(body)?.ask);
  return {
    askId,
    issueKey: stringOf(ask?.issue_key),
    artifactId: stringOf(ask?.artifact_id),
    kind: stringOf(ask?.kind) === "approval" ? "approval" : "question",
    options: optionsOf(ask?.options),
    answers: answersOf(recordOf(body)?.answers),
  };
}

/** Every ask the corpus names, read one by one. An ask the server refuses is kept as its status. */
async function readAsks(
  askIds: readonly string[],
  config: DispatchConfig
): Promise<{ asks: Map<string, AskRead>; refused: Map<string, number> }> {
  const asks = new Map<string, AskRead>();
  const refused = new Map<string, number>();
  await pool(askIds, READ_CONCURRENCY, async (askId) => {
    try {
      const body = await readJson<unknown>(`/api/v1/asks/${encodeURIComponent(askId)}`, config);
      asks.set(askId, askOf(askId, body));
    } catch (error) {
      if (!(error instanceof DispatchRefusedError)) throw error;
      refused.set(askId, error.status);
    }
  });
  return { asks, refused };
}

/** Whose logs must be walked to date the answers of these asks, each owner once. */
function ownersOf(asks: ReadonlyMap<string, AskRead>): Owner[] {
  const issues = new Set<string>();
  const documents = new Set<string>();
  for (const ask of asks.values()) {
    if (ask.issueKey !== null) issues.add(ask.issueKey);
    else if (ask.artifactId !== null) documents.add(ask.artifactId);
  }
  return [
    ...[...issues].sort().map((id): Owner => ({ kind: "issue", id })),
    ...[...documents].sort().map((id): Owner => ({ kind: "document", id })),
  ];
}

function ownerPath(owner: Owner): string {
  const route = owner.kind === "issue" ? "issues" : "artifacts";
  return `/api/v1/${route}/${encodeURIComponent(owner.id)}/events`;
}

/**
 * The event id that recorded each answer, by case id, read from the owners' own logs: the
 * earliest `ask.answered` row carrying that ask and that `at`.
 */
async function readAnsweredEvents(
  owners: readonly Owner[],
  askIds: ReadonlySet<string>,
  config: DispatchConfig
): Promise<{ eventIds: Map<string, number>; pages: number; refused: Map<string, number> }> {
  const eventIds = new Map<string, number>();
  const refused = new Map<string, number>();
  let pages = 0;
  await pool(owners, READ_CONCURRENCY, async (owner) => {
    let after = 0;
    for (;;) {
      const path = `${ownerPath(owner)}?after=${after}&limit=${EVENT_PAGE_SIZE}`;
      let page: unknown[];
      try {
        page = listOf(await readJson<unknown>(path, config));
      } catch (error) {
        if (!(error instanceof DispatchRefusedError)) throw error;
        refused.set(owner.id, error.status);
        return;
      }
      pages += 1;
      let lastSeq: number | null = null;
      for (const entry of page) {
        const event = recordOf(entry);
        if (event === null) continue;
        lastSeq = numberOf(event.seq) ?? lastSeq;
        if (stringOf(event.type) !== "ask.answered") continue;
        const eventId = numberOf(event.id);
        const payload = recordOf(event.payload);
        const askId = stringOf(payload?.id);
        const at = stringOf(recordOf(payload?.answer)?.at);
        if (eventId === null || askId === null || at === null) continue;
        if (!askIds.has(askId)) continue;
        const caseId = `${askId}@${at}`;
        const earlier = eventIds.get(caseId);
        if (earlier === undefined || eventId < earlier) eventIds.set(caseId, eventId);
      }
      if (page.length < EVENT_PAGE_SIZE || lastSeq === null) return;
      after = lastSeq;
    }
  });
  return { eventIds, pages, refused };
}

/** The cases the two routes together hold, under the build's own rule and the build's own pin. */
function secondReading(
  corpus: Corpus,
  asks: ReadonlyMap<string, AskRead>,
  eventIds: ReadonlyMap<string, number>
): SecondReading {
  const { person, throughEventId } = corpus.snapshot;
  const cases = new Map<string, SecondCase>();
  const leftOut = new Map<string, string>();
  for (const ask of asks.values()) {
    for (const answer of ask.answers) {
      if (answer.user !== person || answer.at === null) continue;
      const caseId = `${ask.askId}@${answer.at}`;
      const eventId = eventIds.get(caseId);
      if (eventId === undefined) {
        leftOut.set(caseId, "the owner's log holds no ask.answered row for this answer");
        continue;
      }
      if (eventId > throughEventId) {
        leftOut.set(caseId, `written as event ${eventId}, above the pin ${throughEventId}`);
        continue;
      }
      const words = answer.text;
      cases.set(caseId, {
        caseId,
        kind: ask.kind,
        verdict: ask.kind === "approval" ? verdictOf(answer.selected, ask.options) : null,
        hasWords: words !== null && words.trim() !== "",
        eventId,
      });
    }
  }
  return { cases, leftOut };
}

function countsOfBuild(rows: readonly CaseRow[]): Counts {
  return countsOf(
    rows.map((row) => ({ kind: row.kind, verdict: row.verdict, hasWords: row.hasWords }))
  );
}

function countsOf(
  cases: readonly { kind: CaseKind; verdict: CaseVerdict | null; hasWords: boolean }[]
): Counts {
  const counts: Counts = {
    total: cases.length,
    byKind: { approval: 0, question: 0 },
    approvalByVerdict: { approve: 0, request_changes: 0, other: 0 },
    withWordsByKind: { approval: 0, question: 0 },
  };
  for (const one of cases) {
    counts.byKind[one.kind] += 1;
    if (one.kind === "approval" && one.verdict !== null) counts.approvalByVerdict[one.verdict] += 1;
    if (one.hasWords) counts.withWordsByKind[one.kind] += 1;
  }
  return counts;
}

function describeBuild(row: CaseRow): string {
  const verdict = row.verdict === null ? "" : ` verdict=${row.verdict}`;
  return `kind=${row.kind}${verdict} words=${row.hasWords ? "yes" : "no"}`;
}

function describeSecond(one: SecondCase): string {
  const verdict = one.verdict === null ? "" : ` verdict=${one.verdict}`;
  return `kind=${one.kind}${verdict} words=${one.hasWords ? "yes" : "no"} (event ${one.eventId})`;
}

function differencesOf(
  corpus: Corpus,
  reading: SecondReading,
  refusedAsks: ReadonlyMap<string, number>
): Difference[] {
  const differences: Difference[] = [];
  for (const row of corpus.rows) {
    const one = reading.cases.get(row.caseId);
    if (one === undefined) {
      const refusal = refusedAsks.get(row.askId);
      const why =
        refusal !== undefined
          ? `the ask's own route refuses it with ${refusal}`
          : (reading.leftOut.get(row.caseId) ?? "this person answers no such ask by this route");
      differences.push({ caseId: row.caseId, build: describeBuild(row), second: why });
      continue;
    }
    if (one.kind !== row.kind || one.verdict !== row.verdict || one.hasWords !== row.hasWords) {
      differences.push({
        caseId: row.caseId,
        build: describeBuild(row),
        second: describeSecond(one),
      });
    }
  }
  for (const [caseId, one] of [...reading.cases].sort(([left], [right]) =>
    left < right ? -1 : left > right ? 1 : 0
  )) {
    if (corpus.byCaseId.has(caseId)) continue;
    differences.push({ caseId, build: "no such case", second: describeSecond(one) });
  }
  return differences;
}

function sideBySide(build: Counts, second: Counts): string {
  const row = (label: string, left: number, right: number): string =>
    `  ${label.padEnd(28)}${String(left).padStart(8)}${String(right).padStart(16)}`;
  return [
    `  ${"".padEnd(28)}${"build".padStart(8)}${"second reading".padStart(16)}`,
    row("cases", build.total, second.total),
    row("  approval", build.byKind.approval, second.byKind.approval),
    row("    approve", build.approvalByVerdict.approve, second.approvalByVerdict.approve),
    row(
      "    request_changes",
      build.approvalByVerdict.request_changes,
      second.approvalByVerdict.request_changes
    ),
    row("    other", build.approvalByVerdict.other, second.approvalByVerdict.other),
    row("  question", build.byKind.question, second.byKind.question),
    "  cases with their words",
    row("  approval", build.withWordsByKind.approval, second.withWordsByKind.approval),
    row("  question", build.withWordsByKind.question, second.withWordsByKind.question),
  ].join("\n");
}

async function recount(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const config = await dispatchConfig();
  const askIds = [...new Set(corpus.rows.map((row) => row.askId))].sort();
  console.log(`corpus                 ${corpus.directory}`);
  console.log(`person                 ${corpus.snapshot.person}`);
  console.log(`pinned through event   ${corpus.snapshot.throughEventId}`);
  console.log(`cases to recount       ${corpus.rows.length} over ${askIds.length} asks`);
  const { asks, refused } = await readAsks(askIds, config);
  const owners = ownersOf(asks);
  const issues = owners.filter((owner) => owner.kind === "issue").length;
  const walk = await readAnsweredEvents(owners, new Set(askIds), config);
  const refusedAsks = refused.size === 0 ? "" : ` (${refused.size} refused)`;
  const refusedLogs = walk.refused.size === 0 ? "" : `, ${walk.refused.size} refused`;
  console.log(`asks read              ${asks.size}${refusedAsks}`);
  console.log(
    `event logs walked      ${issues} issues and ${owners.length - issues} documents,` +
      ` ${walk.pages} pages${refusedLogs}`
  );
  const reading = secondReading(corpus, asks, walk.eventIds);
  const afterPin = [...reading.leftOut.values()].filter((why) => why.startsWith("written as"));
  console.log(`answers above the pin  ${afterPin.length} (left out: one snapshot, not two times)`);
  console.log("");
  console.log(sideBySide(countsOfBuild(corpus.rows), countsOf([...reading.cases.values()])));
  console.log("");
  const differences = differencesOf(corpus, reading, refused);
  const cases = (count: number): string => `${count} case${count === 1 ? "" : "s"}`;
  if (differences.length === 0) {
    console.log(`agree: ${cases(corpus.rows.length)}, each of the same kind, verdict and words`);
    return 0;
  }
  console.log(`differ: ${cases(differences.length)}`);
  for (const difference of differences.slice(0, DIFFERENCES_PRINTED)) {
    console.log(`  ${difference.caseId}`);
    console.log(`    build           ${difference.build}`);
    console.log(`    second reading  ${difference.second}`);
  }
  const rest = differences.length - DIFFERENCES_PRINTED;
  if (rest > 0) console.log(`  and ${rest} more`);
  return 1;
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
    return await recount(flags);
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

process.exit(await main(Bun.argv.slice(2)));
