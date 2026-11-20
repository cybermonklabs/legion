import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { main } from "../analyze";
import type { CaseRow } from "../build-corpus";
import type { FinalTestTree } from "../draw-io";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE, type Registration, runDigestOf } from "../reserve";
import { ANALYSIS_FILE, ruleVerdictOf, type S10Analysis } from "../s10-analysis";
import { INPUTS_VERSION } from "../s10-inputs-io";
import { HOLD_VERDICT, JUDGED_VERSION, RULE_VERSIONS, type RuleVersion } from "../s10-judged-io";
import { clopperPearsonUpper } from "../stats";

/**
 * The claim's analysis of S10 (`analyze.ts --claim-s10`, `s10-analysis.ts`), run as the command
 * over a claim directory written here: a build, a draw, an inputs stage and a judged stage in the
 * shapes each stage writes and its parser reads back, chained by the sha256 each stage records of
 * the one before. The registration is one made for this test's own digest over a bundle root of
 * its own, as `s10-inputs.test.ts` makes one. `s10-judge.test.ts` holds the analysis's reading of
 * the answers to every stage the judging driver's own suite writes.
 */

const REFERENCE = "registration-comment-1";
const START = "start-1";
const PERSON = "person@example.test";
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
const before = {
  reserve: process.env[RESERVE_VARIABLE],
  claimRun: process.env[CLAIM_RUN_VARIABLE],
};

beforeAll(async () => {
  scratch = await mkdtemp(join(tmpdir(), "s10-analysis-"));
  process.env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
  process.env[CLAIM_RUN_VARIABLE] = START;
  root = join(scratch, "bundle");
  await mkdir(join(root, "scripts/approval-eval"), { recursive: true });
  await writeFile(join(root, "scripts/approval-eval/analyze.ts"), "the analysis\n", "utf8");
  const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
  registration = { reads: [{ reference: REFERENCE, run }], ended: null };
});

afterAll(async () => {
  for (const [name, value] of [
    [RESERVE_VARIABLE, before.reserve],
    [CLAIM_RUN_VARIABLE, before.claimRun],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  await rm(scratch, { recursive: true, force: true });
});

/** One case of the corpus: an answer of the person at event `eventId`. */
function caseRow(
  askId: string,
  issueKey: string,
  eventId: number,
  kind: CaseRow["kind"],
  chose: Chosen,
  documents: Array<{ artifactId: string; version: number }>
): CaseRow {
  const answeredAt = `2026-11-20T10:${String(eventId).padStart(2, "0")}:00Z`;
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
    askRootListed: true,
    issueRootListed: true,
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
          judges: 0,
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
 * a tree and a tail) as question cases of the eligible trees and `empty` trees holding a question
 * alone.
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
  let event = 1;
  const rows: CaseRow[] = [];
  const treeCases = new Map<string, string[]>();
  const casesOf = new Map<string, number>();
  for (const spec of specs) {
    const askId = ask(spec.tail);
    const documents = spec.answered ?? [{ artifactId: `doc-${spec.tail}`, version: 2 }];
    const chosen = [spec.chose, ...(spec.later ?? [])];
    for (const chose of chosen) {
      const row = caseRow(askId, `ACME-${spec.tail}`, event, "approval", chose, documents);
      event += 1;
      rows.push(row);
      treeCases.set(spec.tree, [...(treeCases.get(spec.tree) ?? []), row.caseId]);
    }
    casesOf.set(askId, chosen.length);
  }
  for (const [tree, tail] of questions) {
    const row = caseRow(ask(tail), `ACME-${tail}`, event, "question", "Approve", []);
    event += 1;
    rows.push(row);
    treeCases.set(tree, [...(treeCases.get(tree) ?? []), row.caseId]);
  }
  const snapshot = jsonText({
    person: PERSON,
    throughEventId: 100,
    eventsRead: 100,
    keptEvents: 0,
    readStartedAt: "2026-11-20T00:00:00Z",
    readFinishedAt: "2026-11-20T00:01:00Z",
    reserve: { leftOut: false, cases: 0, issues: 0, readUnder: REFERENCE, claimRun: START },
  });
  const corpus = rows.map((row) => `${JSON.stringify(row)}\n`).join("");
  const treesFile = jsonText({ version: 1, throughEventId: 100, groups: [] });
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
    const ready = spec.deviation !== "inputs";
    return {
      askId: ask(spec.tail),
      issueKey: `ACME-${spec.tail}`,
      group: spec.tree,
      cases: casesOf.get(ask(spec.tail)),
      judged: ready ? { eventId: 50, type: "ask.opened", createdAt: "2026-11-20T09:00:00Z" } : null,
      state: ready ? "ready" : "deviation",
      reason: ready ? null : "the log gives no title as of the moment",
      requestSha256: ready ? sha256(`request ${spec.tail}`) : null,
      counts: null,
      dropScanThrough: null,
    };
  });
  const written = change.lines?.(lines) ?? lines;
  const inventory = written.map((line) => `${JSON.stringify(line)}\n`).join("");
  const inputsRecord = jsonText({
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state: "written",
    claimRun: build,
    reference: REFERENCE,
    runDigest: "1".repeat(64),
    person: PERSON,
    throughEventId: 100,
    draw: { drawSha256: sha256(drawText), slicesSha256: sha256(slicesText) },
    reserveAsk: "2".repeat(64),
    directories: { build: claim.build, draw: claim.draw },
    startedAt: "2026-11-20T01:00:00Z",
    finishedAt: "2026-11-20T02:00:00Z",
    dropScanThrough: null,
    dropScanPages: null,
    counts: null,
    inventorySha256: sha256(inventory),
    requests: {},
    pages: {},
  });
  await writeAll(claim.inputs, { "s10-inputs.json": inputsRecord, "inventory.jsonl": inventory });
  const files: Record<string, string> = {};
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
    reference: REFERENCE,
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
    expect(analysis.otherRules.v5?.measure).toMatchObject({ n: 8, disagreeing: 6 });
    expect(analysis.otherRules.v4?.measure.bound).toBe(clopperPearsonUpper(6, 8, 0.9));
    expect(analysis.otherRules.v3?.measure).toMatchObject({ n: 8, disagreeing: 7 });
    expect(analysis.otherRules.v3?.tally.deviations.byKind["tool-call"]).toBe(2);
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
    const text = await run([
      ...argsOf(claim).filter((one) => one !== "json" && one !== "--format"),
    ]);
    expect(text.status).toBe(0);
    expect(text.printed).toContain("# The claim's analysis of S10, over its judged stage");
    expect(text.printed).toContain(`at most ${clopperPearsonUpper(5, 8, 0.9).toFixed(3)}`);
    expect(text.printed).toContain("6 of 11 (0.545)");
    expect(text.printed).toContain("2 question cases on 2 asks");
    expect(text.printed).toContain("### The send-back figures beside item 8");
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
    expect(refused.errors).toContain("is another run's analysis: its run digest is");
    const stray = join(await mkdtemp(join(scratch, "stray-")), "analysis");
    await mkdir(stray, { recursive: true });
    await writeFile(join(stray, "notes.txt"), "not this run's\n", "utf8");
    const strayRun = await run(argsOf(claim, stray));
    expect(strayRun.status).toBe(2);
    expect(strayRun.errors).toContain("is no file this analysis writes");
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

  test("refuses a read the registration does not admit, and no read at all", async () => {
    const claim = await writeClaim(SMALL);
    const unregistered = await run(argsOf(claim), { reads: [], ended: null });
    expect(unregistered.status).toBe(2);
    expect(unregistered.errors).toContain(`claim.ts registers no read under ${REFERENCE}`);
    process.env[RESERVE_VARIABLE] = "";
    try {
      const unread = await run(argsOf(claim));
      expect(unread.status).toBe(2);
      expect(unread.errors).toContain("runs only under its registered read");
    } finally {
      process.env[RESERVE_VARIABLE] = `read:${REFERENCE}`;
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
    expect(withRound.errors).toContain("--claim-s10 reads no round, so it takes no --round");
    const drawAlone = await run(["--corpus", "/c/build", "--round", "/c/round", "--draw", "/d"]);
    expect(drawAlone.errors).toContain("--draw names the claim run's draw");
  });
});
