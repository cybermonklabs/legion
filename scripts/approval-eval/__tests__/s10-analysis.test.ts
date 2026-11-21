import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { main } from "../analyze";
import type { CaseRow } from "../build-corpus";
import type { DispatchEvent } from "../dispatch-read";
import type { FinalTestTree } from "../draw-io";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE, type Registration, runDigestOf } from "../reserve";
import { ANALYSIS_FILE, RANKS_STATEMENT, type S10Analysis } from "../s10-analysis";
import { INPUTS_VERSION, inventoryLinesOf } from "../s10-inputs-io";
import {
  HOLD_VERDICT,
  JUDGED_VERSION,
  judgedStageOf,
  RULE_VERSIONS,
  type RuleVersion,
  reportedOutcomesOf,
  ruleVerdictOf,
} from "../s10-judged-io";
import { clopperPearsonUpper } from "../stats";

/**
 * The claim's analysis of S10 (`analyze.ts --claim-s10`, `s10-analysis.ts`), run as the command
 * over a claim directory written here: a build, a draw, an inputs stage and a judged stage in the
 * shapes each stage writes and its parser reads back, chained by the sha256 each stage records of
 * the one before, and the event log the build read, which a stand-in Dispatch serves. Every case
 * of a claim written here is one the reserve holds, as the claim's own are. The registration is
 * one made for this test's own digest over a bundle root of its own, as `s10-inputs.test.ts`
 * makes one. `s10-judge.test.ts` holds the analysis's reading of the answers to every stage the
 * judging driver's own suite writes.
 */

const REFERENCE = "registration-comment-1";
const START = "start-1";
const PERSON = "person@example.test";
const OTHER = "other@example.test";
const TOKEN = "fake-bearer";
const OPTIONS = [
  { id: null, label: "Approve", description: null },
  { id: null, label: "Request changes", description: null },
  { id: null, label: "Something else", description: null },
];

const sha256 = (text: string): string => createHash("sha256").update(text, "utf8").digest("hex");
const ask = (tail: string): string => `00000000-0000-4000-8000-${tail.padStart(12, "0")}`;

type Verdict = "approve" | "request_changes";
type Chosen = "Approve" | "Request changes" | "Something else";

/** One request of a claim written here, as its stages record it. */
interface RequestSpec {
  tail: string;
  tree: string;
  /** The option of his first answer. */
  chose: Chosen;
  /** The option of each later answer of the same ask, which are its provenance. */
  later?: Chosen[];
  /** What the judged stage holds of it: an outcome, a started request, or nothing at all. */
  outcome: "verdict" | "hold" | "deviation" | "started" | "unjudged";
  /** The five answers the sandbox kept, each a verdict and a `never_decide`. */
  answers?: Array<[Verdict, string]>;
  /** The verdict the stage recorded, where it is not the rule's over `answers`. */
  recorded?: string;
  /** The stage's deviation kind, for a deviation that is no refused tool call. */
  deviation?: string;
  /** The versions of point 5's rule that refuse a call, with the tools of each call refused. */
  refused?: Partial<Record<RuleVersion, string[]>>;
  /** The (document, version) pairs his answer answered, where they are not the judged ones. */
  answered?: Array<{ artifactId: string; version: number }>;
  /** A settlement of his before his first `ask.answered`: its verdict, and its version if not 2. */
  settled?: { verdict: Verdict; version?: number };
  /** Whether an `ask.answered` of his that holds another user's answer comes first. */
  notOwnFirst?: boolean;
  /** The judges a fact-check hold's judgement records, where not none. */
  holdJudges?: number;
  /** Whether the inputs stage made the request a deviation, whatever the judged stage holds. */
  lineDeviation?: boolean;
  /** The inputs stage's candidates dropped for a later event, where not one seen in the log. */
  dropped?: { log: number; "no-event-through-pin": number; "after-pin": number };
  /** The drop scan's end its candidates were dropped by, where not event 90. */
  dropScan?: number;
}

/** Point 2's rule over the five answers, as the stand-in of a stage's recorded verdict. */
function ruled(answers: ReadonlyArray<[Verdict, string]>): string {
  const found = ruleVerdictOf(answers.map(([verdict, kind]) => ({ verdict, never_decide: kind })));
  if (!("verdict" in found)) throw new Error(found.problem);
  return found.verdict;
}

const FIVE_APPROVE: Array<[Verdict, string]> = Array(5).fill(["approve", "none"]);

let scratch: string;
let root: string;
let registration: Registration;
let server: { stop(closeActiveConnections?: boolean): void | Promise<void> } | undefined;
/** The event log of each claim written here, by its build's directory; and the one served now. */
const logs = new Map<string, DispatchEvent[]>();
let served: DispatchEvent[] = [];
const before = {
  reserve: process.env[RESERVE_VARIABLE],
  claimRun: process.env[CLAIM_RUN_VARIABLE],
  url: process.env.DISPATCH_URL,
  tokenFile: process.env.DISPATCH_TOKEN_FILE,
};

beforeAll(async () => {
  const started = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      if (request.headers.get("authorization") !== `Bearer ${TOKEN}`) {
        return new Response("no bearer", { status: 401 });
      }
      if (url.pathname !== "/api/v1/events/log") {
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
      }
      const after = Number(url.searchParams.get("after"));
      const limit = Number(url.searchParams.get("limit"));
      return Response.json(served.filter((one) => one.id > after).slice(0, limit));
    },
  });
  server = started;
  scratch = await mkdtemp(join(tmpdir(), "s10-analysis-"));
  await writeFile(join(scratch, "token"), `${TOKEN}\n`, "utf8");
  process.env.DISPATCH_URL = `http://127.0.0.1:${started.port}`;
  process.env.DISPATCH_TOKEN_FILE = join(scratch, "token");
  process.env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
  process.env[CLAIM_RUN_VARIABLE] = START;
  root = join(scratch, "bundle");
  await mkdir(join(root, "scripts/approval-eval"), { recursive: true });
  await writeFile(join(root, "scripts/approval-eval/analyze.ts"), "the analysis\n", "utf8");
  const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
  registration = { reads: [{ reference: REFERENCE, run }], ended: null };
});

afterAll(async () => {
  await server?.stop(true);
  for (const [name, value] of [
    [RESERVE_VARIABLE, before.reserve],
    [CLAIM_RUN_VARIABLE, before.claimRun],
    ["DISPATCH_URL", before.url],
    ["DISPATCH_TOKEN_FILE", before.tokenFile],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  await rm(scratch, { recursive: true, force: true });
});

/** The time of event `id` in a log written here: a minute an event, from 09:00Z. */
function timeOf(id: number): string {
  return new Date(Date.UTC(2026, 10, 20, 9) + id * 60_000).toISOString();
}

/** One event of a log written here. */
function event(
  id: number,
  type: string,
  actor: DispatchEvent["actor"],
  payload: Record<string, unknown>
): DispatchEvent {
  return {
    id,
    issue_key: null,
    project: "ACME",
    seq: id,
    type,
    actor,
    created_at: timeOf(id),
    payload,
  };
}

/** One case of the corpus: an answer of the person at event `eventId`. */
function caseRow(
  askId: string,
  issueKey: string,
  eventId: number,
  kind: CaseRow["kind"],
  chose: Chosen,
  documents: Array<{ artifactId: string; version: number }>
): CaseRow {
  const answeredAt = timeOf(eventId);
  return {
    caseId: `${askId}@${answeredAt}`,
    eventId,
    askId,
    issueKey,
    project: "ACME",
    kind,
    urgency: "med",
    multiple: false,
    question: "The question",
    options: OPTIONS,
    selected: [chose],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-20T09:00:00Z",
    askTreeRoot: issueKey,
    issueTreeRoot: issueKey,
    // Held by the reserve, as every case of the claim's final test is.
    askRootListed: false,
    issueRootListed: false,
    answeredAt,
    eventCreatedAt: answeredAt,
    approval:
      kind === "approval"
        ? {
            documents: documents.map((one) => ({
              artifactId: one.artifactId,
              name: "spec.md",
              version: one.version,
              requestedVersion: one.version,
            })),
            artifactSlug: null,
            artifactPrimary: null,
          }
        : null,
    verdict:
      kind === "approval"
        ? chose === "Approve"
          ? "approve"
          : chose === "Request changes"
            ? "request_changes"
            : "other"
        : null,
    confounds: {
      version: null,
      answersThatDayBefore: 0,
      hourUtc: 10,
      hourPacific: 2,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

/** What a claim written here is changed by, to test one refusal. */
interface ClaimChange {
  /** The claim run the judged stage's record names. */
  judgedClaimRun?: string;
  /** The read the judged stage's record names. */
  judgedReference?: string;
  /** Whether the inputs stage's record is one that never finished. */
  inputsStarted?: boolean;
  /** The inventory's lines as written, from the lines the inventory's rule gives. */
  lines?: (lines: Array<Record<string, unknown>>) => Array<Record<string, unknown>>;
}

/** Where a claim written here is, and what its stages hold. */
interface Claim {
  directory: string;
  build: string;
  draw: string;
  inputs: string;
  judged: string;
}

/** Writes `files`, by their paths under `directory`, each as text. */
async function writeAll(directory: string, files: Record<string, string>): Promise<void> {
  for (const [name, text] of Object.entries(files)) {
    await mkdir(dirname(join(directory, name)), { recursive: true });
    await writeFile(join(directory, name), text, "utf8");
  }
}

/** A JSON file's text, as the stages write theirs. */
function jsonText(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

/** One request's outcome under one version of the rule, as the judged stage records it. */
function ruleOutcome(spec: RequestSpec, version: RuleVersion): Record<string, unknown> {
  const tools = spec.refused?.[version];
  if (tools !== undefined) {
    return {
      outcome: "deviation",
      verdict: null,
      deviation: "tool-call",
      toolCallDeviations: tools.map((tool, at) => ({
        judge: `judge ${spec.tail} ${at + 1}`,
        callId: `call-${tool}-${at}`,
        tools: [tool],
        reason: `no ${tool} the rule allows`,
      })),
    };
  }
  if (spec.outcome === "hold") {
    return { outcome: "hold", verdict: HOLD_VERDICT, deviation: null, toolCallDeviations: [] };
  }
  if (spec.outcome === "deviation" && spec.deviation !== undefined) {
    return {
      outcome: "deviation",
      verdict: null,
      deviation: spec.deviation,
      toolCallDeviations: [],
    };
  }
  const verdict = spec.recorded ?? ruled(spec.answers ?? FIVE_APPROVE);
  return { outcome: "verdict", verdict, deviation: null, toolCallDeviations: [] };
}

/** The judged stage's files of one request: its outcome and, where judges ran, their answers. */
function judgedFiles(spec: RequestSpec, group: string, cases: number): Record<string, string> {
  const askId = ask(spec.tail);
  const document = `doc-${spec.tail}`;
  const byRule = Object.fromEntries(
    RULE_VERSIONS.map((version) => [version, ruleOutcome(spec, version)])
  ) as Record<RuleVersion, Record<string, unknown>>;
  const own = byRule.v6;
  const judged =
    spec.outcome !== "hold" && (spec.deviation === undefined || spec.refused?.v6 !== undefined);
  const answers = spec.answers ?? FIVE_APPROVE;
  const count = (pick: (answer: [Verdict, string]) => string): Record<string, number> => {
    const counts: Record<string, number> = {};
    for (const answer of answers) counts[pick(answer)] = (counts[pick(answer)] ?? 0) + 1;
    return counts;
  };
  const judgement =
    spec.outcome === "hold"
      ? {
          verdict: HOLD_VERDICT,
          documents: [
            { artifact_id: document, name: "spec.md", version: 2, sha256: "f".repeat(64) },
          ],
          judged_at: "2026-11-21T00:00:00Z",
          judges: spec.holdJudges ?? 0,
          votes: null,
          confidence: 1,
        }
      : judged
        ? {
            verdict: spec.recorded ?? ruled(answers),
            documents: [
              { artifact_id: document, name: "spec.md", version: 2, sha256: "f".repeat(64) },
            ],
            judged_at: "2026-11-21T00:00:00Z",
            judges: 5,
            votes: count(([verdict]) => verdict),
            never_decide_votes: count(([, kind]) => kind),
            confidence: 1,
            reason: "One sentence.",
            judge_models: Array(5).fill({
              asked: "anthropic/claude-opus-5-5:xhigh",
              served: null,
              fallback: false,
            }),
          }
        : null;
  const judges = judged
    ? answers.map((_, at) => ({
        label: `judge ${spec.tail} ${at + 1}`,
        transcript: `sessions/top/judge${at + 1}.jsonl`,
        bundle: `/work/shadow/bundles/${askId}/judge-${at + 1}.md`,
        agent: `judge${at + 1}`,
        asked: "anthropic/claude-opus-5-5:xhigh",
        served: null,
        fallback: false,
        toolCalls: [],
      }))
    : [];
  const deviationKind = own.deviation as string | null;
  const outcome = {
    askId,
    issueKey: `ACME-${spec.tail}`,
    group,
    cases,
    outcome: own.outcome,
    verdict: own.verdict,
    deviation:
      deviationKind === null
        ? null
        : { kind: deviationKind, reason: "a test's deviation", problems: ["a test's deviation"] },
    judgement,
    judges,
    sandbox: null,
    judgedAt: "2026-11-21T00:00:00Z",
    byRule,
  };
  const files: Record<string, string> = {
    [`requests/${askId}/outcome.json`]: jsonText(outcome),
  };
  if (judged) {
    files[`requests/${askId}/sandbox/results.json`] = jsonText(
      answers.map(([verdict, kind], at) => ({
        id: `judge${at + 1}`,
        // The first answer as wait() gives a text, the others as objects, as the driver reads both.
        result:
          at === 0
            ? JSON.stringify({ never_decide: kind, verdict, reason: "One sentence." })
            : { never_decide: kind, verdict, reason: "One sentence." },
      }))
    );
    for (let at = 1; at <= answers.length; at += 1) {
      files[`requests/${askId}/judge-${at}.md`] = `# The bundle of judge ${at}\n`;
      files[`requests/${askId}/sandbox/sessions/top/judge${at}.jsonl`] = `{"judge": ${at}}\n`;
    }
    files[`requests/${askId}/calls.json`] = jsonText([]);
  }
  return files;
}

/** The stage's counts of outcome lines, as `s10_judge.py`'s `counts_of` writes them. */
function countsOf(rows: ReadonlyArray<Record<string, unknown>>): Record<string, unknown> {
  const tally = (key: string): Record<string, number> => {
    const counts: Record<string, number> = {};
    for (const row of rows) {
      const value = row[key];
      if (typeof value === "string") counts[value] = (counts[value] ?? 0) + 1;
    }
    return counts;
  };
  return {
    requests: rows.length,
    holds: rows.filter((row) => row.outcome === "hold").length,
    verdicts: rows.filter((row) => row.outcome === "verdict").length,
    byVerdict: tally("verdict"),
    deviations: rows.filter((row) => row.outcome === "deviation").length,
    deviationsByKind: tally("deviation"),
  };
}

/**
 * A claim directory of `specs`, every stage chained to the one before it, with `questions` (each
 * a tree and a tail) as question cases of the eligible trees, and the log its build read: each
 * request's opening, then any settlement or answer of another user's its spec names, then each of
 * his answers its cases record.
 */
async function writeClaim(
  specs: readonly RequestSpec[],
  questions: ReadonlyArray<[string, string]> = [],
  change: ClaimChange = {}
): Promise<Claim> {
  const directory = await mkdtemp(join(scratch, "claim-"));
  const claim: Claim = {
    directory,
    build: join(directory, "build"),
    draw: join(directory, "draw"),
    inputs: join(directory, "s10-inputs"),
    judged: join(directory, "s10-judged"),
  };
  let id = 1;
  const events: DispatchEvent[] = [];
  const rows: CaseRow[] = [];
  const treeCases = new Map<string, string[]>();
  const casesOf = new Map<string, number>();
  const session = { kind: "session", id: "session-1" };
  const person = { kind: "user", id: PERSON };
  for (const spec of specs) {
    const askId = ask(spec.tail);
    const document = `doc-${spec.tail}`;
    events.push(
      event(id++, "ask.opened", session, {
        id: askId,
        kind: "approval",
        state: "open",
        issue_key: `ACME-${spec.tail}`,
        question: "Approve the spec?",
        approval: { artifact_id: document, name: "spec.md", version: 2 },
        created_at: timeOf(id),
      })
    );
    if (spec.settled !== undefined) {
      const type =
        spec.settled.verdict === "approve" ? "artifact.approved" : "artifact.changes_requested";
      events.push(
        event(id++, type, person, { artifact_id: document, version: spec.settled.version ?? 2 })
      );
    }
    if (spec.notOwnFirst === true) {
      events.push(
        event(id++, "ask.answered", person, {
          id: askId,
          answer: { user: OTHER, selected: ["Approve"], at: timeOf(id) },
        })
      );
    }
    const documents = spec.answered ?? [{ artifactId: document, version: 2 }];
    const chosen = [spec.chose, ...(spec.later ?? [])];
    for (const chose of chosen) {
      const row = caseRow(askId, `ACME-${spec.tail}`, id, "approval", chose, documents);
      events.push(
        event(id++, "ask.answered", person, {
          id: askId,
          answer: { user: PERSON, selected: [chose], at: row.answeredAt },
        })
      );
      rows.push(row);
      treeCases.set(spec.tree, [...(treeCases.get(spec.tree) ?? []), row.caseId]);
    }
    casesOf.set(askId, chosen.length);
  }
  for (const [tree, tail] of questions) {
    const row = caseRow(ask(tail), `ACME-${tail}`, id, "question", "Approve", []);
    events.push(
      event(id++, "ask.answered", person, {
        id: ask(tail),
        answer: { user: PERSON, selected: ["Approve"], at: row.answeredAt },
      })
    );
    rows.push(row);
    treeCases.set(tree, [...(treeCases.get(tree) ?? []), row.caseId]);
  }
  const through = id - 1;
  logs.set(claim.build, events);
  const snapshot = jsonText({
    person: PERSON,
    throughEventId: through,
    eventsRead: events.length,
    keptEvents: events.length,
    readStartedAt: "2026-11-20T00:00:00Z",
    readFinishedAt: "2026-11-20T00:01:00Z",
    reserve: {
      leftOut: false,
      cases: rows.length,
      issues: rows.length,
      readUnder: REFERENCE,
      claimRun: START,
    },
  });
  const corpus = rows.map((row) => `${JSON.stringify(row)}\n`).join("");
  const treesFile = jsonText({ version: 1, throughEventId: through, groups: [] });
  await writeAll(claim.build, {
    "snapshot.json": snapshot,
    "corpus.jsonl": corpus,
    "trees.json": treesFile,
  });
  const build = {
    id: START,
    snapshotSha256: sha256(snapshot),
    corpusSha256: sha256(corpus),
    treesSha256: sha256(treesFile),
  };
  const trees: FinalTestTree[] = [...treeCases]
    .sort(([one], [other]) => (one < other ? -1 : 1))
    .map(([group, cases]) => ({ group, slice: "report" as const, cases: [...cases].sort() }));
  const drawText = jsonText({
    corpus: claim.build,
    cut: "2026-11-16T00:00:00.000Z",
    seed: 7,
    backlogLegionRootsRead: null,
    backlogLegionRoots: null,
    rule: null,
    neverDecideApplied: false,
    claimRun: build,
    finalTest: { reference: REFERENCE, trees },
  });
  const slicesText = jsonText({
    fit: [],
    report: trees.flatMap((tree) => tree.cases),
    anchor: [],
    reserve: [],
  });
  await writeAll(claim.draw, { "draw.json": drawText, "slices.json": slicesText });
  const ordered = [...specs].sort((one, other) => (ask(one.tail) < ask(other.tail) ? -1 : 1));
  const lines = ordered.map((spec) => {
    const ready = spec.deviation !== "inputs" && spec.lineDeviation !== true;
    // The inputs stage counts the candidates of a request it searched for: none of a fact hold's.
    const searched = ready && spec.outcome !== "hold";
    return {
      askId: ask(spec.tail),
      issueKey: `ACME-${spec.tail}`,
      group: spec.tree,
      cases: casesOf.get(ask(spec.tail)),
      judged: ready ? { eventId: 50, type: "ask.opened", createdAt: "2026-11-20T09:00:00Z" } : null,
      state: ready ? "ready" : "deviation",
      reason: ready ? null : "the log gives no title as of the moment",
      requestSha256: ready ? sha256(`request ${spec.tail}`) : null,
      counts: searched
        ? {
            askHits: 4,
            candidates: 3,
            held: 1,
            reserveErrors: 0,
            droppedLaterEvent: spec.dropped ?? {
              log: 1,
              "no-event-through-pin": 0,
              "after-pin": 0,
            },
            kept: 1,
          }
        : null,
      dropScanThrough: searched ? (spec.dropScan ?? 90) : null,
    };
  });
  const written = change.lines?.(lines) ?? lines;
  const inventory = written.map((line) => `${JSON.stringify(line)}\n`).join("");
  const inputsRecord = jsonText({
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state: change.inputsStarted === true ? "started" : "written",
    claimRun: build,
    reference: REFERENCE,
    runDigest: "1".repeat(64),
    person: PERSON,
    throughEventId: through,
    draw: { drawSha256: sha256(drawText), slicesSha256: sha256(slicesText) },
    reserveAsk: "2".repeat(64),
    directories: { build: claim.build, draw: claim.draw },
    startedAt: "2026-11-20T01:00:00Z",
    finishedAt: "2026-11-20T02:00:00Z",
    dropScanThrough: null,
    dropScanPages: null,
    counts: null,
    inventorySha256: change.inputsStarted === true ? null : sha256(inventory),
    requests: {},
    pages: {},
  });
  await writeAll(claim.inputs, { "s10-inputs.json": inputsRecord, "inventory.jsonl": inventory });
  // The frozen code the stage keeps, a file outside every request's directory.
  const files: Record<string, string> = { "code/shadow.py": "# the kept frozen code\n" };
  const outcomeLines: Array<Record<string, unknown>> = [];
  const byRuleLines: Record<RuleVersion, Array<Record<string, unknown>>> = {
    v3: [],
    v4: [],
    v5: [],
    v6: [],
  };
  for (const spec of ordered) {
    const askId = ask(spec.tail);
    if (spec.outcome === "unjudged") continue;
    if (spec.outcome === "started") {
      files[`requests/${askId}/started.json`] = jsonText({
        askId,
        startedAt: "2026-11-21T00:00:00Z",
      });
      continue;
    }
    const own = judgedFiles(spec, spec.tree, casesOf.get(askId) ?? 1);
    Object.assign(files, own);
    const outcome = JSON.parse(own[`requests/${askId}/outcome.json`] as string);
    outcomeLines.push({
      askId,
      issueKey: outcome.issueKey,
      group: outcome.group,
      cases: outcome.cases,
      outcome: outcome.outcome,
      verdict: outcome.verdict,
      deviation: outcome.deviation?.kind ?? null,
      byRule: Object.fromEntries(
        RULE_VERSIONS.map((version) => {
          const { outcome: kind, verdict, deviation } = outcome.byRule[version];
          byRuleLines[version].push({ outcome: kind, verdict, deviation });
          return [version, { outcome: kind, verdict, deviation }];
        })
      ),
      outcomeSha256: sha256(own[`requests/${askId}/outcome.json`] as string),
    });
  }
  const whole = specs.every((spec) => spec.outcome !== "started" && spec.outcome !== "unjudged");
  if (whole) {
    files["outcomes.jsonl"] = outcomeLines.map((line) => `${JSON.stringify(line)}\n`).join("");
  }
  const record = {
    stage: "s10-judged",
    version: JUDGED_VERSION,
    state: whole ? "written" : "started",
    claimRun: change.judgedClaimRun ?? START,
    reference: change.judgedReference ?? REFERENCE,
    runDigest: "3".repeat(64),
    claimDirectory: directory,
    inputs: {
      directory: claim.inputs,
      stageSha256: sha256(inputsRecord),
      files: { "inventory.jsonl": sha256(inventory) },
    },
    frozen: {
      codeSha256: "4".repeat(64),
      files: { "shadow.py": "5".repeat(64) },
      promptSha256: "6".repeat(64),
      bundleCodeSha256: "7".repeat(64),
      skillSha256: { "SKILL.md": "8".repeat(64) },
      asked: "anthropic/claude-opus-5-5:xhigh",
      model: "anthropic/claude-opus-5-5",
    },
    image: `registry.example.test/sandbox@sha256:${"a".repeat(64)}`,
    serviceAccount: "s10-judge-sandbox",
    startedAt: "2026-11-21T00:00:00Z",
    finishedAt: whole ? "2026-11-21T01:00:00Z" : null,
    stops: whole ? [] : [{ at: "2026-11-21T00:30:00Z", reason: "the run received signal 15" }],
    counts: whole
      ? {
          ...countsOf(outcomeLines),
          byRule: Object.fromEntries(
            RULE_VERSIONS.map((version) => [version, countsOf(byRuleLines[version])])
          ),
        }
      : null,
    files: Object.fromEntries(Object.entries(files).map(([name, text]) => [name, sha256(text)])),
  };
  await writeAll(claim.judged, { ...files, "s10-judged.json": jsonText(record) });
  return claim;
}

/** The command's arguments over `claim`, as point 13 names them, its record written to `out`. */
function argsOf(claim: Claim, out = join(claim.directory, "analysis")): string[] {
  return [
    "--corpus",
    claim.build,
    "--claim-s10",
    claim.judged,
    "--draw",
    claim.draw,
    "--claim-threshold",
    "0.6",
    "--format",
    "json",
    "--out",
    out,
  ];
}

/** One run of the command, quiet: its exit status, what it printed, and what it said on stderr. */
async function run(
  argv: string[],
  read: Registration = registration
): Promise<{ status: number; printed: string; errors: string }> {
  // The stand-in Dispatch serves the log of the build the run reads.
  served = logs.get(argv[argv.indexOf("--corpus") + 1] ?? "") ?? [];
  const printed: string[] = [];
  const errors: string[] = [];
  const log = spyOn(console, "log").mockImplementation((line?: unknown) => {
    printed.push(String(line));
  });
  const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
    errors.push(String(line));
  });
  try {
    const status = await main(argv, { registration: read, root });
    return { status, printed: printed.join("\n"), errors: errors.join("\n") };
  } finally {
    log.mockRestore();
    error.mockRestore();
  }
}

/** The analysis of `claim`, run as the command and read back from the record it wrote. */
async function analysed(claim: Claim): Promise<S10Analysis> {
  const done = await run(argsOf(claim));
  if (done.status !== 0) throw new Error(done.errors);
  const record = await readFile(join(claim.directory, "analysis", ANALYSIS_FILE), "utf8");
  expect(JSON.parse(done.printed)).toEqual(JSON.parse(record));
  return JSON.parse(record) as S10Analysis;
}

describe("point 2's rule, recomputed from five answers", () => {
  const of = (...answers: Array<[Verdict, string]>) =>
    ruleVerdictOf(answers.map(([verdict, kind]) => ({ verdict, never_decide: kind })));

  test("is the majority of the five verdicts, with the share behind it", () => {
    expect(of(...FIVE_APPROVE)).toEqual({ verdict: "approve", share: 1 });
    expect(
      of(
        ["request_changes", "none"],
        ["request_changes", "none"],
        ["request_changes", "none"],
        ["approve", "none"],
        ["approve", "credential"]
      )
    ).toEqual({ verdict: "request_changes", share: 0.6 });
  });

  test("is never_decide where three of five name a kind, tied kinds in order joined by +", () => {
    expect(
      of(
        ["approve", "credential"],
        ["approve", "credential"],
        ["approve", "outside-message"],
        ["approve", "none"],
        ["approve", "none"]
      )
    ).toEqual({ verdict: "never_decide:credential", share: 0.4 });
    expect(
      of(
        ["approve", "outside-message"],
        ["approve", "outside-message"],
        ["approve", "credential"],
        ["approve", "credential"],
        ["approve", "none"]
      )
    ).toEqual({ verdict: "never_decide:credential+outside-message", share: 0.4 });
  });

  test("gives no verdict over other than five answers, or one the rule cannot read", () => {
    expect("problem" in of(["approve", "none"])).toBe(true);
    const unread = ruleVerdictOf([
      ...Array(4).fill({ verdict: "approve", never_decide: "none" }),
      {},
    ]);
    expect(unread).toEqual({
      problem: "answer 5 names no never_decide or no verdict the rule reads",
    });
  });
});

/**
 * The claim of the measure's hand count: eleven requests in eleven trees, a tree with a question
 * alone, and one more question beside a request.
 *
 * - a1 (T01): approve over five approves; he approved, then changed it (provenance only).
 * - a2, a3 (T02): request_changes he sent back; approve where he sent it back, which disagrees.
 * - a4 (T03): a fact-check hold. a5 (T04): never_decide:credential, three of five naming it.
 * - a6 (T05): the stage's own `model` deviation.
 * - a7 (T06): a recorded approve where three of the five answers request changes.
 * - a8 (T07): approve, his answer answering another document than the judged one.
 * - a9 (T08): approve under version 6, a refused call under versions 3 to 5.
 * - aa (T10): approve where he chose neither option, which disagrees.
 * - ab (T11): approve under versions 4 to 6, a refused `yield` under version 3.
 * - T09 holds a question alone, and T01 a question beside a1.
 */
const MEASURED: RequestSpec[] = [
  { tail: "a1", tree: "T01", chose: "Approve", later: ["Request changes"], outcome: "verdict" },
  {
    tail: "a2",
    tree: "T02",
    chose: "Request changes",
    outcome: "verdict",
    dropped: { log: 0, "no-event-through-pin": 0, "after-pin": 2 },
    dropScan: 95,
    answers: [
      ["request_changes", "none"],
      ["request_changes", "none"],
      ["request_changes", "none"],
      ["approve", "none"],
      ["approve", "none"],
    ],
  },
  { tail: "a3", tree: "T02", chose: "Request changes", outcome: "verdict" },
  { tail: "a4", tree: "T03", chose: "Approve", outcome: "hold" },
  {
    tail: "a5",
    tree: "T04",
    chose: "Approve",
    outcome: "verdict",
    answers: [
      ["approve", "credential"],
      ["approve", "credential"],
      ["approve", "credential"],
      ["approve", "none"],
      ["approve", "none"],
    ],
  },
  { tail: "a6", tree: "T05", chose: "Approve", outcome: "deviation", deviation: "model" },
  {
    tail: "a7",
    tree: "T06",
    chose: "Request changes",
    outcome: "verdict",
    recorded: "approve",
    answers: [
      ["request_changes", "none"],
      ["request_changes", "none"],
      ["request_changes", "none"],
      ["approve", "none"],
      ["approve", "none"],
    ],
  },
  {
    tail: "a8",
    tree: "T07",
    chose: "Approve",
    outcome: "verdict",
    answered: [{ artifactId: "doc-other", version: 2 }],
  },
  {
    tail: "a9",
    tree: "T08",
    chose: "Approve",
    outcome: "verdict",
    refused: { v3: ["yield"], v4: ["glob"], v5: ["glob"] },
  },
  { tail: "aa", tree: "T10", chose: "Something else", outcome: "verdict" },
  { tail: "ab", tree: "T11", chose: "Approve", outcome: "verdict", refused: { v3: ["yield"] } },
];
const QUESTIONS: Array<[string, string]> = [
  ["T09", "f1"],
  ["T01", "f2"],
];

describe("the claim's analysis over a claim of each kind of outcome", () => {
  let analysis: S10Analysis;
  let claim: Claim;

  beforeAll(async () => {
    claim = await writeClaim(MEASURED, QUESTIONS);
    analysis = await analysed(claim);
  });

  test("counts each request once by ask id, its later answers provenance, and the questions in no figure", () => {
    expect(analysis.inventory).toEqual({
      requests: 11,
      cases: 12,
      trees: 11,
      questionCases: 2,
      questionAsks: 2,
    });
    expect(analysis.requests.map((one) => one.askId)).toEqual(
      ["a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "aa", "ab"].map(ask)
    );
    const first = analysis.requests[0];
    expect(first?.cases.length).toBe(2);
    // His answer is the ask's first case: an approve, though he later asked for changes.
    expect(first?.answer.verdict).toBe("approve");
    expect(first?.byRule.v6.disagrees).toBe(false);
    expect(analysis.sendBack.requests).toBe(11);
  });

  test("counts each kind of outcome, never-decides apart from deviations", () => {
    expect(analysis.tally).toEqual({
      requests: 11,
      decided: { total: 6, approve: 5, requestChanges: 1 },
      neverDecides: {
        total: 2,
        byVerdict: { "never_decide:credential": 1, [HOLD_VERDICT]: 1 },
      },
      deviations: {
        total: 3,
        byKind: { documents: 1, model: 1, verdict: 1 },
        foundByAnalysis: { documents: 1, verdict: 1 },
      },
    });
    const of = (tail: string) => analysis.requests.find((one) => one.askId === ask(tail));
    expect(of("a4")?.byRule.v6).toMatchObject({ status: "never-decide", deviation: null });
    expect(of("a5")?.byRule.v6).toMatchObject({ status: "never-decide", disagrees: null });
  });

  test("refuses a recorded verdict unlike the rule's recomputation from the five kept answers", () => {
    const a7 = analysis.requests.find((one) => one.askId === ask("a7"));
    expect(a7?.recomputed).toEqual({ verdict: "request_changes", share: 0.6 });
    expect(a7?.verdictProblem).toBe(
      "S10 recorded approve, and point 2's rule gives request_changes"
    );
    for (const version of RULE_VERSIONS) {
      expect(a7?.byRule[version].deviation).toEqual({ kind: "verdict", foundBy: "analysis" });
    }
  });

  test("makes a request whose answered documents are not the judged ones a deviation", () => {
    const a8 = analysis.requests.find((one) => one.askId === ask("a8"));
    expect(a8?.documentsProblem).toContain('["doc-a8",2]');
    expect(a8?.byRule.v6).toMatchObject({
      status: "deviation",
      deviation: { kind: "documents", foundBy: "analysis" },
      disagrees: true,
    });
  });

  test("gives item 8's penalized bound of the hand count", () => {
    // n: T01, T02, T05, T06, T07, T08, T10, T11; disagreeing: T02 (a3), T05 (a6), T06 (a7),
    // T07 (a8), T10 (aa); never-decide: T03, T04 and T09, which holds no request.
    expect(analysis.measure).toMatchObject({
      trees: 11,
      withoutRequest: 1,
      n: 8,
      disagreeing: 5,
      neverDecide: { count: 3, share: 3 / 11, withRequest: { count: 2, share: 2 / 10 } },
      withinRate: false,
    });
    expect(analysis.measure.bound).toBe(clopperPearsonUpper(5, 8, 0.9));
  });

  test("puts every deviation in the decided share's denominator and none in its numerator", () => {
    expect(analysis.decidedShare).toEqual({
      decided: 6,
      requests: 11,
      share: 6 / 11,
      planningD: 0.85,
    });
    expect(analysis.leastDecidedShare).toBe(0.6);
    expect(analysis.threshold).toBe(0.6);
  });

  test("reports each version's penalized bound and the requests each version's rule changes", () => {
    expect(analysis.countingRule).toBe("v6");
    expect(analysis.byRule.v6).toEqual({ tally: analysis.tally, measure: analysis.measure });
    expect(analysis.byRule.v5.measure).toMatchObject({ n: 8, disagreeing: 6 });
    expect(analysis.byRule.v4.measure.bound).toBe(clopperPearsonUpper(6, 8, 0.9));
    expect(analysis.byRule.v3.measure).toMatchObject({ n: 8, disagreeing: 7 });
    expect(analysis.byRule.v3.tally.deviations.byKind["tool-call"]).toBe(2);
    expect(analysis.ruleChanges.map((one) => [one.from, one.to, one.requests.length])).toEqual([
      ["v3", "v4", 1],
      ["v4", "v5", 0],
      ["v5", "v6", 1],
    ]);
    const [fourth, , sixth] = analysis.ruleChanges;
    expect(fourth?.requests[0]).toMatchObject({
      askId: ask("ab"),
      before: "deviation tool-call",
      after: "decided approve",
      refusedOnlyAfter: [],
    });
    expect(fourth?.requests[0]?.refusedOnlyBefore.map((one) => one.tools)).toEqual([["yield"]]);
    expect(sixth?.requests[0]?.askId).toBe(ask("a9"));
    expect(sixth?.requests[0]?.refusedOnlyBefore.map((one) => one.tools)).toEqual([["glob"]]);
  });

  test("counts a never-decide and a deviation wrong in the send-back figures, with no verdict made up", () => {
    expect(analysis.sendBack).toMatchObject({
      requests: 11,
      otherAnswers: 1,
      caught: 1,
      missed: 2,
      cleared: 4,
      falseSendBacks: 4,
      notDecided: { missed: 1, falseSendBacks: 4 },
    });
    expect(analysis.sendBack.recall).toMatchObject({ count: 1, total: 3 });
    expect(analysis.sendBack.specificity).toMatchObject({ count: 4, total: 8 });
    expect(analysis.sendBack.alwaysApprove.agreement).toMatchObject({ count: 8, total: 11 });
  });

  test("prints the same figures as text", async () => {
    // A text run is another run than the json one (its digest takes --format as written), so it
    // writes its own --out.
    const out = join(claim.directory, "analysis-text");
    const text = await run([
      ...argsOf(claim, out).filter((one) => one !== "json" && one !== "--format"),
    ]);
    expect(text.status).toBe(0);
    expect(text.printed).toContain("# The claim's analysis of S10, over its judged stage");
    expect(text.printed).toContain(`at most ${clopperPearsonUpper(5, 8, 0.9).toFixed(3)}`);
    expect(text.printed).toContain("6 of 11 (0.545)");
    expect(text.printed).toContain("2 question cases on 2 asks");
    expect(text.printed).toContain("### The send-back figures beside item 8");
    expect(text.printed).toContain("## The neighbours' search (point 4)");
    expect(text.printed).toContain(
      "11 dropped for an event after the judged moment (seen in the log 9, with no event " +
        "through the pin 0, after the pin 2), by drop scans through 90, 95."
    );
    expect(text.printed).toContain(RANKS_STATEMENT);
  });

  test("discloses the candidates dropped for a later event and where the ranks come from (point 4)", () => {
    // Ten requests were searched (the hold a4 was not); a2's scan ran on to event 95.
    expect(analysis.neighbours).toEqual({
      requests: 10,
      askHits: 40,
      candidates: 30,
      held: 10,
      reserveErrors: 0,
      kept: 10,
      droppedLaterEvent: { log: 9, "no-event-through-pin": 0, "after-pin": 2, total: 11 },
      dropScanEnds: [90, 95],
      statement: RANKS_STATEMENT,
    });
    const a2 = analysis.requests.find((one) => one.askId === ask("a2"));
    expect(a2?.candidates?.droppedLaterEvent["after-pin"]).toBe(2);
  });

  test("keeps its own record on a run over the same stages, and refuses another run's or a stray file", async () => {
    const again = await run(argsOf(claim));
    expect(again.status).toBe(0);
    expect(again.errors).toContain(`kept ${join(claim.directory, "analysis", ANALYSIS_FILE)}`);
    const other = join(await mkdtemp(join(scratch, "other-")), "analysis");
    await mkdir(other, { recursive: true });
    const record = JSON.parse(
      await readFile(join(claim.directory, "analysis", ANALYSIS_FILE), "utf8")
    );
    await writeFile(
      join(other, ANALYSIS_FILE),
      jsonText({ ...record, runDigest: "9".repeat(64) }),
      "utf8"
    );
    const refused = await run(argsOf(claim, other));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("is not the record this run gives over the same stages");
    const stray = join(await mkdtemp(join(scratch, "stray-")), "analysis");
    await mkdir(stray, { recursive: true });
    await writeFile(join(stray, "notes.txt"), "not this run's\n", "utf8");
    const strayRun = await run(argsOf(claim, stray));
    expect(strayRun.status).toBe(2);
    expect(strayRun.errors).toContain("is no file this run writes");
  });
});

describe("a judging run that stopped", () => {
  test("counts each request a run started and never finished, and each no run reached, as a deviation", async () => {
    const claim = await writeClaim([
      { tail: "b1", tree: "T1", chose: "Approve", outcome: "verdict" },
      { tail: "b2", tree: "T2", chose: "Approve", outcome: "started" },
      { tail: "b3", tree: "T3", chose: "Approve", outcome: "unjudged" },
      { tail: "b4", tree: "T4", chose: "Approve", outcome: "deviation", deviation: "inputs" },
    ]);
    const analysis = await analysed(claim);
    expect(analysis.chain.judged.state).toBe("started");
    expect(analysis.tally.deviations.byKind).toEqual({ inputs: 1, started: 1, unjudged: 1 });
    expect(analysis.measure).toMatchObject({ n: 4, disagreeing: 3 });
    expect(analysis.decidedShare).toMatchObject({ decided: 1, requests: 4 });
  });
});

describe("the chain the analysis reads", () => {
  const SMALL: RequestSpec[] = [
    { tail: "c1", tree: "T1", chose: "Approve", outcome: "verdict" },
    { tail: "c2", tree: "T2", chose: "Request changes", outcome: "hold" },
  ];

  test("refuses a judged stage another claim run wrote", async () => {
    const claim = await writeClaim(SMALL, [], { judgedClaimRun: "start-2" });
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      "was written by the claim run start-2, and this run's is start-1"
    );
    expect(existsSync(join(claim.directory, "analysis"))).toBe(false);
  });

  test("refuses a judged stage written under another read", async () => {
    const claim = await writeClaim(SMALL, [], { judgedReference: "registration-comment-2" });
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      `was written under registration-comment-2, and this run reads under ${REFERENCE}`
    );
  });

  test("refuses an inputs stage that never finished", async () => {
    const claim = await writeClaim(SMALL, [], { inputsStarted: true });
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("records an inputs stage that never finished");
  });

  test("refuses an inventory changed after both records named its sha256", async () => {
    const claim = await writeClaim(SMALL);
    const path = join(claim.inputs, "inventory.jsonl");
    await writeFile(path, `${await readFile(path, "utf8")}\n`, "utf8");
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      `${path} is not the inventory the inputs stage wrote and the judging read`
    );
  });

  test("makes an outcome of a request the inputs stage made a deviation, other than its own, an inputs deviation", async () => {
    const claim = await writeClaim([
      { tail: "h1", tree: "T1", chose: "Approve", outcome: "verdict", lineDeviation: true },
      { tail: "h2", tree: "T2", chose: "Approve", outcome: "deviation", deviation: "inputs" },
    ]);
    const analysis = await analysed(claim);
    const [h1, h2] = analysis.requests;
    for (const version of RULE_VERSIONS) {
      expect(h1?.byRule[version].deviation).toEqual({ kind: "inputs", foundBy: "analysis" });
      expect(h2?.byRule[version].deviation).toEqual({ kind: "inputs", foundBy: "stage" });
    }
    expect(analysis.tally.decided.total).toBe(0);
  });

  test("refuses an inputs stage swapped for another under the judged stage", async () => {
    const claim = await writeClaim(SMALL);
    const path = join(claim.inputs, "s10-inputs.json");
    const record = JSON.parse(await readFile(path, "utf8"));
    await writeFile(path, jsonText({ ...record, finishedAt: "2026-11-20T03:00:00Z" }), "utf8");
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("is not the inputs stage");
  });

  test("refuses a draw other than the one the inputs stage read", async () => {
    const claim = await writeClaim(SMALL);
    const draw = join(claim.directory, "draw-2");
    await cp(claim.draw, draw, { recursive: true });
    const slices = join(draw, "slices.json");
    await writeFile(slices, `${(await readFile(slices, "utf8")).trimEnd()}\n\n`, "utf8");
    const argv = argsOf(claim);
    argv[argv.indexOf("--draw") + 1] = draw;
    const refused = await run(argv);
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      "is not this claim run's inputs stage over the build and the draw"
    );
    expect(refused.errors).toContain("its slices.json is");
  });

  test("refuses a read the registration does not admit, and no read at all, naming no case", async () => {
    const claim = await writeClaim(SMALL);
    // Every case of the claim is one the reserve holds; a reader that met one under no read would
    // name it, and the refusal comes before any reader.
    const named = [ask("c1"), ask("c2"), "@2026-"];
    const unregistered = await run(argsOf(claim), { reads: [], ended: null });
    expect(unregistered.status).toBe(2);
    expect(unregistered.errors).toContain(`claim.ts registers no read under ${REFERENCE}`);
    for (const one of named) expect(unregistered.errors).not.toContain(one);
    process.env[RESERVE_VARIABLE] = "";
    try {
      const unread = await run(argsOf(claim));
      expect(unread.status).toBe(2);
      expect(unread.errors).toContain("the claim run's stages run only under its registered read");
      for (const one of named) expect(unread.errors).not.toContain(one);
    } finally {
      process.env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
    }
    const unnamed = process.env[CLAIM_RUN_VARIABLE];
    process.env[CLAIM_RUN_VARIABLE] = "";
    try {
      const noRun = await run(argsOf(claim));
      expect(noRun.status).toBe(2);
      expect(noRun.errors).toContain(`${CLAIM_RUN_VARIABLE} names, and it names none`);
      for (const one of named) expect(noRun.errors).not.toContain(one);
    } finally {
      process.env[CLAIM_RUN_VARIABLE] = unnamed;
    }
    expect(existsSync(join(claim.directory, "analysis"))).toBe(false);
  });

  test("refuses an inventory that names a request twice", async () => {
    const claim = await writeClaim(SMALL, [], {
      lines: (lines) => [...lines, lines[0] as Record<string, unknown>],
    });
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(`names the request ${ask("c1")} twice`);
  });

  test("refuses an inventory that is not the one the draw's report trees give", async () => {
    const claim = await writeClaim(SMALL, [], {
      lines: (lines) => lines.map((line) => ({ ...line, group: "T9" })),
    });
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("and the draw's report trees give it T1 with 1");
  });

  test("takes --claim-s10 and --draw as paths in its digest", async () => {
    const one = await runDigestOf(
      { argv: ["bun", "/r/analyze.ts", ...argsOf(await writeClaim(SMALL))], env: {} },
      root
    );
    const two = await runDigestOf(
      { argv: ["bun", "/r/analyze.ts", ...argsOf(await writeClaim(SMALL))], env: {} },
      root
    );
    expect(one).toBe(two);
  });
});

describe("the claim path's flags", () => {
  const base = ["--corpus", "/c/build", "--claim-s10", "/c/s10-judged", "--draw", "/c/draw"];

  test("takes --claim-threshold 0.6 alone, never fitted or another number", async () => {
    for (const threshold of [[], ["--claim-threshold", "fitted"], ["--claim-threshold", "0.5"]]) {
      const refused = await run([...base, "--out", "/c/analysis", ...threshold]);
      expect(refused.status).toBe(2);
      expect(refused.errors).toContain("--claim-s10 takes --claim-threshold 0.6");
    }
  });

  test("refuses a round's flags beside it, and --draw without it", async () => {
    const withRound = await run([
      ...base,
      "--claim-threshold",
      "0.6",
      "--out",
      "/c/analysis",
      "--round",
      "/c/round",
    ]);
    expect(withRound.errors).toContain("and no --round: the claim reads no round");
    const drawAlone = await run(["--corpus", "/c/build", "--round", "/c/round", "--draw", "/d"]);
    expect(drawAlone.errors).toContain("--draw names the claim run's draw");
  });
});

describe("his answer, by the first qualifying event (point 7)", () => {
  /**
   * d1: S10 approves; a settlement of his requesting changes on the ask's document at its version
   * comes before the ask.answered approving it, so his answer requests changes.
   * d2: a settlement at another version than the ask names qualifies for nothing, so his answer
   * is the ask.answered, requesting changes.
   * d3: his first ask.answered holds another user's answer, so his answer holds no verdict.
   * d4: a settlement approving comes before an ask.answered requesting changes.
   */
  const SPECS: RequestSpec[] = [
    {
      tail: "d1",
      tree: "T1",
      chose: "Approve",
      outcome: "verdict",
      settled: { verdict: "request_changes" },
    },
    {
      tail: "d2",
      tree: "T2",
      chose: "Request changes",
      outcome: "verdict",
      settled: { verdict: "approve", version: 1 },
    },
    { tail: "d3", tree: "T3", chose: "Approve", outcome: "verdict", notOwnFirst: true },
    {
      tail: "d4",
      tree: "T4",
      chose: "Request changes",
      outcome: "verdict",
      settled: { verdict: "approve" },
    },
  ];
  let claim: Claim;
  let analysis: S10Analysis;
  const of = (tail: string) => analysis.requests.find((one) => one.askId === ask(tail));
  const eventOf = (type: string, tail: string): number | undefined =>
    logs
      .get(claim.build)
      ?.find(
        (one) =>
          one.type === type &&
          (one.payload.artifact_id === `doc-${tail}` || one.payload.id === ask(tail))
      )?.id;

  beforeAll(async () => {
    claim = await writeClaim(SPECS);
    analysis = await analysed(claim);
  });

  test("takes a settlement on the ask's document at its version, before his ask.answered", () => {
    const d1 = of("d1");
    expect(d1?.answer).toEqual({
      eventId: eventOf("artifact.changes_requested", "d1") ?? -1,
      type: "artifact.changes_requested",
      caseId: null,
      verdict: "request_changes",
      documents: [{ artifactId: "doc-d1", version: 2 }],
      problem: null,
    });
    expect(d1?.byRule.v6).toMatchObject({ status: "decided", verdict: "approve", disagrees: true });
    // The ask.answered after it is the request's provenance, and no row of any figure.
    expect(d1?.cases.length).toBe(1);
    const d4 = of("d4");
    expect(d4?.answer).toMatchObject({ type: "artifact.approved", verdict: "approve" });
    expect(d4?.answer.eventId).toBe(eventOf("artifact.approved", "d4") ?? -1);
    expect(d4?.byRule.v6).toMatchObject({ status: "decided", disagrees: false });
  });

  test("takes no settlement at another version than the ask names", () => {
    const d2 = of("d2");
    expect(d2?.answer).toMatchObject({
      eventId: eventOf("ask.answered", "d2") ?? -1,
      type: "ask.answered",
      verdict: "request_changes",
      problem: null,
    });
    expect(d2?.answer.caseId).toBe(d2?.cases[0] ?? "");
    expect(d2?.byRule.v6.disagrees).toBe(true);
  });

  test("makes a request whose first answer is not his own a deviation", () => {
    const d3 = of("d3");
    expect(d3?.answer.problem).toContain("is not his own, so it holds no verdict");
    for (const version of RULE_VERSIONS) {
      expect(d3?.byRule[version].deviation).toEqual({ kind: "answer", foundBy: "analysis" });
    }
    expect(analysis.measure).toMatchObject({ n: 4, disagreeing: 3 });
    expect(analysis.sendBack).toMatchObject({
      caught: 0,
      missed: 2,
      cleared: 1,
      falseSendBacks: 1,
    });
  });

  test("refuses a log that is not the one the build read", async () => {
    const other = await writeClaim(SPECS);
    logs.set(other.build, (logs.get(other.build) ?? []).slice(1));
    const refused = await run(argsOf(other));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("it is not the log the build read");
  });
});

describe("the judged stage's kept evidence", () => {
  const SPECS: RequestSpec[] = [
    { tail: "e1", tree: "T1", chose: "Approve", outcome: "verdict" },
    { tail: "e2", tree: "T2", chose: "Approve", outcome: "verdict" },
  ];
  const E1 = `requests/${ask("e1")}`;

  /** The analysis of a claim whose judged stage `change` altered after it was written. */
  async function changed(change: (judged: string) => Promise<void>): Promise<S10Analysis> {
    const claim = await writeClaim(SPECS);
    await change(claim.judged);
    return analysed(claim);
  }

  test("leaves a request whose every kept file is there a decided one", async () => {
    const analysis = await changed(async () => {});
    expect(analysis.tally.decided.total).toBe(2);
    expect(analysis.requests[0]?.lostEvidence).toEqual([]);
  });

  test("makes a lost transcript, changed answers or a changed bundle the request's deviation", async () => {
    const cases: Array<[string, (judged: string) => Promise<void>, string, string]> = [
      [
        "transcript",
        (judged) => rm(join(judged, E1, "sandbox/sessions/top/judge1.jsonl")),
        "sandbox/sessions/top/judge1.jsonl",
        "is not there",
      ],
      [
        "answers",
        (judged) => writeFile(join(judged, E1, "sandbox/results.json"), "[]\n", "utf8"),
        "sandbox/results.json",
        "is not the file the stage record names",
      ],
      [
        "sandbox",
        (judged) => writeFile(join(judged, E1, "judge-3.md"), "another bundle\n", "utf8"),
        "judge-3.md",
        "is not the file the stage record names",
      ],
    ];
    for (const [kind, change, file, problem] of cases) {
      const analysis = await changed(change);
      const e1 = analysis.requests.find((one) => one.askId === ask("e1"));
      expect(e1?.lostEvidence).toEqual([{ file, problem, kind }]);
      for (const version of RULE_VERSIONS) {
        expect(e1?.byRule[version].deviation).toEqual({ kind, foundBy: "analysis" });
      }
      expect(analysis.tally).toMatchObject({ decided: { total: 1 }, deviations: { total: 1 } });
      expect(analysis.measure).toMatchObject({ n: 2, disagreeing: 1 });
    }
  });

  test("makes a verdict whose record names no transcript of a judge a transcript deviation", async () => {
    const analysis = await changed(async (judged) => {
      const path = join(judged, "s10-judged.json");
      const record = JSON.parse(await readFile(path, "utf8"));
      delete record.files[`${E1}/sandbox/sessions/top/judge2.jsonl`];
      await writeFile(path, jsonText(record), "utf8");
    });
    const e1 = analysis.requests.find((one) => one.askId === ask("e1"));
    expect(e1?.lostEvidence).toEqual([
      {
        file: "sandbox/sessions/top/judge2.jsonl",
        problem: "is not named in the stage record",
        kind: "transcript",
      },
    ]);
    expect(e1?.byRule.v6.deviation).toEqual({ kind: "transcript", foundBy: "analysis" });
  });

  test("refuses the stage where a file it names outside every request is lost", async () => {
    const claim = await writeClaim(SPECS);
    await rm(join(claim.judged, "code/shadow.py"));
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      "code/shadow.py, which the stage record names, is not there: the stage is not what its " +
        "record says"
    );
  });

  /**
   * Writes `text` at `name` in the judged stage and names its new sha256 in the record (and, for
   * an outcome, in `outcomes.jsonl`, which the record names too), as a stage that wrote it so would.
   */
  async function rehashed(judged: string, name: string, text: string | null): Promise<void> {
    const path = join(judged, "s10-judged.json");
    const record = JSON.parse(await readFile(path, "utf8"));
    if (text === null) {
      delete record.files[name];
    } else {
      await writeFile(join(judged, name), text, "utf8");
      record.files[name] = sha256(text);
    }
    await writeFile(path, jsonText(record), "utf8");
  }

  test("makes answers the analysis cannot read, kept as named, a verdict deviation it found", async () => {
    const results = `${E1}/sandbox/results.json`;
    const one = (verdict: string) => ({
      never_decide: "none",
      verdict,
      reason: "One sentence.",
    });
    const cases: Array<[string | null, string, string]> = [
      [null, "answers", "is not named in the stage record"],
      ["not json\n", "verdict", `no recomputation: ${results} is not JSON`],
      ["{}\n", "verdict", `no recomputation: ${results} is no list of answers`],
      [
        jsonText([1, 2, 3, 4].map((at) => ({ id: `judge${at}`, result: one("approve") }))),
        "verdict",
        `no recomputation: ${results} holds no answer of judge e1 5 (judge5)`,
      ],
    ];
    for (const [text, kind, problem] of cases) {
      const analysis = await changed((judged) => rehashed(judged, results, text));
      const e1 = analysis.requests.find((reading) => reading.askId === ask("e1"));
      expect(e1?.byRule.v6.deviation).toEqual({ kind, foundBy: "analysis" });
      if (kind === "verdict") expect(e1?.verdictProblem).toBe(problem);
      else expect(e1?.lostEvidence).toEqual([{ file: "sandbox/results.json", problem, kind }]);
    }
  });

  test("makes a fact-check hold that records judges a verdict deviation it found", async () => {
    const claim = await writeClaim([
      { tail: "k1", tree: "T1", chose: "Approve", outcome: "hold", holdJudges: 1 },
    ]);
    const analysis = await analysed(claim);
    const k1 = analysis.requests[0];
    expect(k1?.verdictProblem).toBe(
      "no recomputation: a fact-check hold whose judgement records 1 judges and whose outcome " +
        "records 0, where a hold has none"
    );
    expect(k1?.byRule.v6.deviation).toEqual({ kind: "verdict", foundBy: "analysis" });
  });

  test("reads a started request's started.json with every file named, and says where it is lost", async () => {
    const claim = await writeClaim([
      { tail: "m1", tree: "T1", chose: "Approve", outcome: "verdict" },
      { tail: "m2", tree: "T2", chose: "Approve", outcome: "started" },
    ]);
    await writeFile(join(claim.judged, `requests/${ask("m2")}/started.json`), "{}\n", "utf8");
    const stage = await judgedStageOf(claim.judged);
    expect(stage.evidence.get(ask("m2"))).toEqual([
      { file: "started.json", problem: "is not the file the stage record names" },
    ]);
    const inventory = join(claim.inputs, "inventory.jsonl");
    const [, m2] = reportedOutcomesOf(
      inventoryLinesOf(await readFile(inventory, "utf8"), inventory),
      stage
    );
    expect(m2?.deviation?.kind).toBe("started");
    expect(m2?.deviation?.problems).toContain(
      "its started.json is not the file the stage record names"
    );
    const analysis = await analysed(claim);
    expect(analysis.requests[1]?.byRule.v6.deviation).toEqual({
      kind: "started",
      foundBy: "stage",
    });
  });
});

describe("the record's own directory", () => {
  test("removes this process's partial write of its record, and refuses another's or other bytes", async () => {
    const claim = await writeClaim([
      { tail: "g1", tree: "T1", chose: "Approve", outcome: "verdict" },
    ]);
    await analysed(claim);
    const text = await readFile(join(claim.directory, "analysis", ANALYSIS_FILE), "utf8");
    // This process's write cut short: the start of the record, beside its place.
    const own = join(claim.directory, "own");
    await mkdir(own, { recursive: true });
    const mine = join(own, `${ANALYSIS_FILE}.${process.pid}.partial`);
    await writeFile(mine, text.slice(0, 200), "utf8");
    const resumed = await run(argsOf(claim, own));
    expect(resumed.status).toBe(0);
    expect(existsSync(mine)).toBe(false);
    expect(await readFile(join(own, ANALYSIS_FILE), "utf8")).toBe(text);
    // Another process's partial is refused, though its bytes are the start of this run's record,
    // and so is this process's partial whose bytes are not.
    for (const [pid, bytes] of [
      [process.pid + 1, text.slice(0, 200)],
      [process.pid, `${text.slice(0, 199)}X`],
    ] as const) {
      const other = await mkdtemp(join(claim.directory, "other-"));
      const partial = join(other, `${ANALYSIS_FILE}.${pid}.partial`);
      await writeFile(partial, bytes, "utf8");
      const refused = await run(argsOf(claim, other));
      expect(refused.status).toBe(2);
      expect(refused.errors).toContain(`${partial} is no file this run writes`);
      expect(existsSync(partial)).toBe(true);
    }
  });

  test("refuses an own --out whose record is not byte for byte the one this run gives", async () => {
    const claim = await writeClaim([
      { tail: "g2", tree: "T1", chose: "Approve", outcome: "verdict" },
    ]);
    await analysed(claim);
    const path = join(claim.directory, "analysis", ANALYSIS_FILE);
    const text = await readFile(path, "utf8");
    await writeFile(path, text.replace('"requests": 1,', '"requests": 2,'), "utf8");
    const refused = await run(argsOf(claim));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain(
      `${path} is not the record this run gives over the same stages`
    );
  });
});
