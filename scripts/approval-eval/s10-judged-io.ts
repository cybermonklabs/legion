import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import {
  CorpusError,
  countAt,
  listOf,
  numberOf,
  parsed,
  recordOf,
  sha256At,
  textAt,
} from "./corpus-io";
import type { InventoryLine } from "./s10-inputs-io";

/**
 * The files the claim's judging stage (`s10_judge.py`) writes under its `--out`, which the
 * analysis (`analyze.ts --claim-s10`) reads back: their shapes, and the parsers that read each back
 * and refuse any other shape, as `s10-inputs-io.ts` is to the inputs stage.
 *
 * `s10-judged.json` (`JudgedRecord`, `judgedRecordOf`) chains the stage to its inputs and names the
 * sha256 of every file written; `requests/<ask id>/outcome.json` (`JudgedOutcome`,
 * `judgedOutcomeOf`) is one request's one outcome, every key present, with its outcome under the
 * tool rule of each version of point 5 (`byRule`, version 6's the one that counts) and the calls
 * each refuses; and `outcomes.jsonl` (`OutcomeLine`, `outcomeLinesOf`), which only a run that gave
 * every request its outcome writes, lists them in the inventory's order. `judgedStageOf` reads a
 * directory whole, each file held to the sha256 its record names. A stage that stopped stays
 * `started`, and `reportedOutcomesOf` gives each request of the inventory its outcome: `started`
 * where a run kept its `started.json` and stopped before its outcome, `unjudged` where no run
 * reached it, the one place that kind is made. `recordedAnswersOf` reads the five answers a
 * request's sandbox result kept (`requests/<ask id>/sandbox/results.json`), which the analysis
 * recomputes point 2's verdict from.
 */

export const JUDGED_FILE = "s10-judged.json";
export const OUTCOMES_FILE = "outcomes.jsonl";
export const OUTCOME_FILE = "outcome.json";
/** The file that marks a request started (`s10_store.py`'s `STARTED_FILE`). */
export const STARTED_FILE = "started.json";
/** Why a request started and not finished is a `started` deviation (`s10_store.py`'s `STARTED_REASON`). */
export const STARTED_REASON =
  "a run started the request and stopped before its outcome; it is never judged again";
/** Where a request's sandbox result is kept, under the stage's directory (`kept_result`). */
export const SANDBOX_DIRECTORY = "sandbox";
/** The answers the judges gave, as `wait()` returned them (`s10_sandbox.py`'s `RESULTS_FILE`). */
export const RESULTS_FILE = "results.json";

/** The version of the stage's shapes, which every reader checks. */
export const JUDGED_VERSION = 4;

/** The deviation kinds the stage writes, in `s10_judge.py`'s order (its `DEVIATION_KINDS`). */
export const DEVIATION_KINDS = [
  "inputs",
  "fact",
  "sandbox",
  "manifest",
  "key",
  "session",
  "transcript",
  "tool-call",
  "model",
  "answers",
  "verdict",
  "shape",
  "started",
] as const;

export type DeviationKind = (typeof DEVIATION_KINDS)[number];

/** A request of the inventory no run of the stage reached: made by `reportedOutcomesOf` alone. */
export const UNJUDGED = "unjudged";

/** The verdict a fact hold gives, with no judge. */
export const HOLD_VERDICT = "never_decide:waiting-legion-root";

/** The judges of one request (point 2). */
export const JUDGES = 5;

/** One request's deviation: its kind, the reason, and every problem found. */
export interface Deviation {
  kind: DeviationKind | typeof UNJUDGED;
  reason: string;
  problems: string[];
}

/**
 * The versions of point 5's tool rule each outcome is recorded under, over the same transcripts,
 * and the one that counts (`s10_transcripts.py`'s `RULE_VERSIONS` and `COUNTING_RULE`).
 */
export const RULE_VERSIONS = ["v3", "v4", "v5", "v6"] as const;
export type RuleVersion = (typeof RULE_VERSIONS)[number];
export const COUNTING_RULE: RuleVersion = "v6";

/** One tool call a version of the rule makes a deviation. */
export interface ToolCallDeviation {
  judge: string;
  callId: string;
  tools: string[];
  reason: string;
}

/** A request's outcome under one version of the rule: a hold or a verdict, or a deviation. */
export type RuleOutcome =
  | { outcome: "hold" | "verdict"; verdict: string; deviation: null }
  | { outcome: "deviation"; verdict: null; deviation: DeviationKind | typeof UNJUDGED };

/** A request's outcome under one version of the rule, with the calls that version refuses. */
export type JudgedRuleOutcome = RuleOutcome & { toolCallDeviations: ToolCallDeviation[] };

/**
 * A document S10 judged, at its version, as its `finish_judgement` records it: point 7 holds his
 * answered (document, version) pairs to these.
 */
export interface JudgedDocument {
  artifact_id: string;
  name: string | null;
  version: number;
  sha256: string;
}

/** The model one judge was asked, the one omp reports serving it, and whether a fallback served. */
export interface JudgeModel {
  asked: string;
  served: string | null;
  fallback: boolean;
}

/**
 * S10's own judgement record (`finish_judgement`, with this stage's `judge_models`): the fields the
 * analysis reads, typed; every other field as S10 wrote it. A hold has no vote counts by kind and
 * no judge models, which read as null.
 */
export interface Judgement {
  verdict: string;
  documents: JudgedDocument[];
  judged_at: string;
  judges: number;
  votes: Record<string, number> | null;
  never_decide_votes: Record<string, number> | null;
  confidence: number;
  reason: string | null;
  judge_models: JudgeModel[] | null;
  [field: string]: unknown;
}

/**
 * One judge as the stage records it: its call's label and transcript, and, where a transcript was
 * read, its bundle, its agent, its models and its tool calls (null and none where it was not).
 */
export interface JudgeRecord {
  label: string;
  transcript: string | null;
  bundle: string | null;
  agent: string | null;
  asked: string | null;
  served: string | null;
  fallback: boolean | null;
  toolCalls: Array<{ id: string; names: string[] }>;
}

/** One metadata server's answer to the sandbox's probe: no body, ever. */
export interface MetadataAnswer {
  url: string;
  connected: boolean | null;
  status: number | null;
}

/**
 * What the sandbox recorded of its checks and its pod, as one request's outcome keeps it: the
 * fields the analysis reads, typed (null where the sandbox recorded none); every other as written.
 */
export interface SandboxFacts {
  job: string;
  pod: string | null;
  imageId: string | null;
  refused: string | null;
  listing: { lines: number; sha256: string } | null;
  metadata: MetadataAnswer[] | null;
  runtime: Record<string, string | null> | null;
  manifests: Record<string, unknown> | null;
  keyFound: string[] | null;
  [field: string]: unknown;
}

/** `requests/<ask id>/outcome.json`: one request's one outcome. */
export interface JudgedOutcome {
  askId: string;
  issueKey: string | null;
  group: string;
  cases: number;
  outcome: RuleOutcome["outcome"];
  /** The verdict of a hold or of five valid answers; null for a deviation. */
  verdict: string | null;
  deviation: Deviation | null;
  /** S10's own `finish_judgement` record, with `judge_models`; null where none was made. */
  judgement: Judgement | null;
  judges: JudgeRecord[];
  /** What the sandbox recorded of its checks and its pod; null where no sandbox ran. */
  sandbox: SandboxFacts | null;
  judgedAt: string | null;
  /** The outcome under each version of the rule; `COUNTING_RULE`'s is the outcome above. */
  byRule: Record<RuleVersion, JudgedRuleOutcome>;
}

/** One line of `outcomes.jsonl`. */
export interface OutcomeLine {
  askId: string;
  issueKey: string | null;
  group: string;
  cases: number;
  outcome: RuleOutcome["outcome"];
  verdict: string | null;
  deviation: DeviationKind | null;
  byRule: Record<RuleVersion, RuleOutcome>;
  outcomeSha256: string;
}

/** The frozen code and skill the stage judged with, as point 2 pins them. */
export interface FrozenCode {
  codeSha256: string;
  /** Each of the code-txt's files, by name, with its sha256. */
  files: Record<string, string>;
  promptSha256: string;
  bundleCodeSha256: string;
  skillSha256: Record<string, string>;
  asked: string;
  model: string;
}

/** A count of outcomes: by kind of outcome, by verdict and by deviation kind. */
export interface OutcomeCounts {
  requests: number;
  holds: number;
  verdicts: number;
  byVerdict: Record<string, number>;
  deviations: number;
  deviationsByKind: Record<string, number>;
}

/** The written stage's counts, by version 6's rule and under each version (`byRule`). */
export interface JudgedCounts extends OutcomeCounts {
  byRule: Record<RuleVersion, OutcomeCounts>;
}

/** `s10-judged.json`: the stage's record. */
export interface JudgedRecord {
  stage: "s10-judged";
  version: number;
  state: "started" | "written";
  claimRun: string;
  reference: string;
  runDigest: string;
  claimDirectory: string;
  inputs: { directory: string; stageSha256: string; files: Record<string, string> };
  frozen: FrozenCode;
  image: string;
  serviceAccount: string;
  startedAt: string;
  finishedAt: string | null;
  /** Each stop of a run, which left the stage to be resumed. */
  stops: Array<{ at: string; reason: string }>;
  counts: JudgedCounts | null;
  /** The sha256 of every file written under the stage's directory, by its path there. */
  files: Record<string, string>;
}

/** A stage read whole: its record, each request's outcome by ask id, and its outcome lines. */
export interface JudgedStage {
  record: JudgedRecord;
  /** The sha256 of the record's bytes, which a later stage names the judged stage by. */
  recordSha256: string;
  outcomes: Map<string, JudgedOutcome>;
  /** `outcomes.jsonl`'s lines, null while the stage is started. */
  lines: OutcomeLine[] | null;
  /**
   * By ask id, each file the record names under the request's directory, but its outcome, that is
   * not there with the bytes the record names (`judgedStageOf`).
   */
  evidence: Map<string, LostEvidence[]>;
}

function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** A JSON object read back, refused where it is no JSON or no object. */
function parsedRecord(text: string, where: string): Record<string, unknown> {
  const record = recordOf(parsed(text, where));
  if (record === null) throw new CorpusError(`${where} is no object`);
  return record;
}

function textOrNull(value: unknown, where: string, field: string): string | null {
  return value === null ? null : textAt(value, where, field);
}

function objectOrNull(
  value: unknown,
  where: string,
  field: string
): Record<string, unknown> | null {
  if (value === null) return null;
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${where} holds a ${field} that is no object`);
  return record;
}

function filesAt(value: unknown, where: string, field: string): Record<string, string> {
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${where} holds ${field} that are no object`);
  const files: Record<string, string> = {};
  for (const [path, sha256] of Object.entries(record))
    files[path] = sha256At(sha256, where, `${field} ${path}`);
  return files;
}

function outcomeKindAt(value: unknown, where: string): JudgedOutcome["outcome"] {
  if (value !== "hold" && value !== "verdict" && value !== "deviation") {
    throw new CorpusError(`${where} is neither a hold, a verdict nor a deviation`);
  }
  return value;
}

function deviationKindAt(value: unknown, where: string): DeviationKind {
  const kind = DEVIATION_KINDS.find((one) => one === value);
  if (kind === undefined)
    throw new CorpusError(
      `${where} holds the deviation kind ${String(value)}, which the stage never writes`
    );
  return kind;
}

/**
 * An outcome's verdict and deviation held to its kind of outcome, as the one `RuleOutcome` it is:
 * a hold's verdict is the fact check's and a verdict's is the rule's, with no deviation; a
 * deviation has a kind and no verdict.
 */
function heldToKind(
  outcome: JudgedOutcome["outcome"],
  verdict: string | null,
  deviation: DeviationKind | typeof UNJUDGED | null,
  where: string
): RuleOutcome {
  if (outcome === "deviation") {
    if (verdict === null && deviation !== null) return { outcome, verdict, deviation };
  } else if (
    deviation === null &&
    verdict !== null &&
    (outcome === "hold"
      ? verdict === HOLD_VERDICT
      : verdict === "approve" ||
        verdict === "request_changes" ||
        verdict.startsWith("never_decide:"))
  ) {
    return { outcome, verdict, deviation: null };
  }
  throw new CorpusError(
    `${where} is a ${outcome} whose verdict and deviation are not a ${outcome}'s`
  );
}

/** One version's outcome read back, held to its kind of outcome. */
function ruleOutcomeOf(value: unknown, where: string): RuleOutcome {
  const record = objectOrNull(value, where, "rule outcome");
  if (record === null) throw new CorpusError(`${where} holds no outcome`);
  const outcome = outcomeKindAt(record.outcome, where);
  const verdict = textOrNull(record.verdict, where, "verdict");
  const deviation = record.deviation === null ? null : deviationKindAt(record.deviation, where);
  return heldToKind(outcome, verdict, deviation, where);
}

/** A tool call one version refuses, read back. */
function toolCallDeviationOf(value: unknown, where: string): ToolCallDeviation {
  const record = objectOrNull(value, where, "tool call") ?? {};
  if (!Array.isArray(record.tools))
    throw new CorpusError(`${where} holds a tool call with no tools`);
  return {
    judge: textAt(record.judge, where, "tool call judge"),
    callId: textAt(record.callId, where, "tool call id"),
    tools: record.tools.map((one) => textAt(one, where, "tool call tool")),
    reason: textAt(record.reason, where, "tool call reason"),
  };
}

/**
 * One version's outcome in `outcome.json`, with the calls that version refuses: a version that
 * refuses a call gives no verdict, and a `tool-call` deviation names at least one call.
 */
function judgedRuleOf(value: unknown, where: string): JudgedRuleOutcome {
  const outcome = ruleOutcomeOf(value, where);
  const calls = recordOf(value)?.toolCallDeviations;
  if (!Array.isArray(calls)) throw new CorpusError(`${where} holds no list of refused tool calls`);
  const toolCallDeviations = calls.map((call) => toolCallDeviationOf(call, where));
  if (
    (toolCallDeviations.length > 0 && outcome.outcome !== "deviation") ||
    (outcome.deviation === "tool-call" && toolCallDeviations.length === 0)
  ) {
    throw new CorpusError(`${where} holds refused tool calls its outcome does not reflect`);
  }
  return { ...outcome, toolCallDeviations };
}

/** One value for each version of the rule, made by `make`. */
function eachRule<T>(make: (version: RuleVersion) => T): Record<RuleVersion, T> {
  const byRule = {} as Record<RuleVersion, T>;
  for (const version of RULE_VERSIONS) byRule[version] = make(version);
  return byRule;
}

/**
 * `byRule` read back: exactly the outcome under each of `RULE_VERSIONS`, each read by `read`, and
 * `COUNTING_RULE`'s equal to the outcome itself.
 */
function byRuleOf<T extends RuleOutcome>(
  value: unknown,
  where: string,
  counted: RuleOutcome,
  read: (one: unknown, at: string) => T
): Record<RuleVersion, T> {
  const record = objectOrNull(value, where, "byRule");
  if (record === null || Object.keys(record).sort().join() !== [...RULE_VERSIONS].sort().join()) {
    throw new CorpusError(`${where} records no outcome under exactly ${RULE_VERSIONS.join(", ")}`);
  }
  const byRule = eachRule((version) => read(record[version], `${where} byRule.${version}`));
  const counting = byRule[COUNTING_RULE];
  if (
    counting.outcome !== counted.outcome ||
    counting.verdict !== counted.verdict ||
    counting.deviation !== counted.deviation
  ) {
    throw new CorpusError(`${where} records under ${COUNTING_RULE} another outcome than its own`);
  }
  return byRule;
}

function numberAt(value: unknown, where: string, field: string): number {
  const found = numberOf(value);
  if (found === null) throw new CorpusError(`${where} holds a ${field} that is no number`);
  return found;
}

function booleanOrNull(value: unknown, where: string, field: string): boolean | null {
  if (value === undefined || value === null) return null;
  if (typeof value !== "boolean")
    throw new CorpusError(`${where} holds a ${field} that is no boolean`);
  return value;
}

/** An optional text: absent or null reads as null. */
function optionalText(value: unknown, where: string, field: string): string | null {
  return value === undefined ? null : textOrNull(value, where, field);
}

function countsByNameAt(value: unknown, where: string, field: string): Record<string, number> {
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${where} holds ${field} that are no object`);
  return Object.fromEntries(
    Object.entries(record).map(([name, count]) => [name, countAt(count, where, `${field} ${name}`)])
  );
}

function textsAt(value: unknown, where: string, field: string): string[] {
  if (!Array.isArray(value)) throw new CorpusError(`${where} holds ${field} that are no list`);
  return value.map((one) => textAt(one, where, field));
}

/** S10's judgement read back: the fields the analysis reads held to their types. */
function judgementOf(value: unknown, where: string): Judgement | null {
  const record = objectOrNull(value, where, "judgement");
  if (record === null) return null;
  if (!Array.isArray(record.documents)) {
    throw new CorpusError(`${where} holds a judgement with no list of documents`);
  }
  const models = record.judge_models;
  if (models !== undefined && !Array.isArray(models)) {
    throw new CorpusError(`${where} holds judge models that are no list`);
  }
  return {
    ...record,
    verdict: textAt(record.verdict, where, "judgement verdict"),
    documents: record.documents.map((one) => {
      const document = objectOrNull(one, where, "judged document") ?? {};
      return {
        artifact_id: textAt(document.artifact_id, where, "judged document id"),
        name: textOrNull(document.name ?? null, where, "judged document name"),
        version: countAt(document.version, where, "judged document version"),
        sha256: sha256At(document.sha256, where, "judged document sha256"),
      };
    }),
    judged_at: textAt(record.judged_at, where, "judgement time"),
    judges: countAt(record.judges, where, "judgement's judges"),
    votes: record.votes === null ? null : countsByNameAt(record.votes, where, "votes"),
    never_decide_votes:
      record.never_decide_votes === undefined || record.never_decide_votes === null
        ? null
        : countsByNameAt(record.never_decide_votes, where, "never-decide votes"),
    confidence: numberAt(record.confidence, where, "confidence"),
    reason: optionalText(record.reason, where, "judgement reason"),
    judge_models:
      models === undefined
        ? null
        : models.map((one) => {
            const model = objectOrNull(one, where, "judge model") ?? {};
            const fallback = booleanOrNull(model.fallback, where, "judge model fallback");
            if (fallback === null)
              throw new CorpusError(`${where} holds a judge model with no fallback`);
            return {
              asked: textAt(model.asked, where, "judge model asked"),
              served: optionalText(model.served, where, "judge model served"),
              fallback,
            };
          }),
  };
}

/** One judge's record read back. */
function judgeRecordOf(value: unknown, where: string): JudgeRecord {
  const record = objectOrNull(value, where, "judge") ?? {};
  const calls = record.toolCalls === undefined ? [] : record.toolCalls;
  if (!Array.isArray(calls)) throw new CorpusError(`${where} holds tool calls that are no list`);
  return {
    label: textAt(record.label, where, "judge label"),
    transcript: optionalText(record.transcript, where, "judge transcript"),
    bundle: optionalText(record.bundle, where, "judge bundle"),
    agent: optionalText(record.agent, where, "judge agent"),
    asked: optionalText(record.asked, where, "judge asked"),
    served: optionalText(record.served, where, "judge served"),
    fallback: booleanOrNull(record.fallback, where, "judge fallback"),
    toolCalls: calls.map((one) => {
      const call = objectOrNull(one, where, "tool call") ?? {};
      return {
        id: textAt(call.id, where, "tool call id"),
        names: textsAt(call.names, where, "tool names"),
      };
    }),
  };
}

/** What the sandbox recorded, read back: the fields the analysis reads held to their types. */
function sandboxFactsOf(value: unknown, where: string): SandboxFacts | null {
  const record = objectOrNull(value, where, "sandbox");
  if (record === null) return null;
  const listing = objectOrNull(record.listing ?? null, where, "listing");
  const metadata = record.metadata ?? null;
  if (metadata !== null && !Array.isArray(metadata)) {
    throw new CorpusError(`${where} holds metadata answers that are no list`);
  }
  const runtime = objectOrNull(record.runtime ?? null, where, "runtime");
  const keyFound = record.keyFound ?? null;
  return {
    ...record,
    job: textAt(record.job, where, "sandbox job"),
    pod: optionalText(record.pod, where, "sandbox pod"),
    imageId: optionalText(record.imageId, where, "sandbox image"),
    refused: optionalText(record.refused, where, "sandbox refusal"),
    listing:
      listing === null
        ? null
        : {
            lines: countAt(listing.lines, where, "listing lines"),
            sha256: sha256At(listing.sha256, where, "listing sha256"),
          },
    metadata:
      metadata === null
        ? null
        : metadata.map((one) => {
            const answer = objectOrNull(one, where, "metadata answer") ?? {};
            return {
              url: textAt(answer.url, where, "metadata url"),
              connected: booleanOrNull(answer.connected, where, "metadata connection"),
              status:
                answer.status === null ? null : countAt(answer.status, where, "metadata status"),
            };
          }),
    runtime:
      runtime === null
        ? null
        : Object.fromEntries(
            Object.entries(runtime).map(([name, one]) => [
              name,
              textOrNull(one ?? null, where, `runtime ${name}`),
            ])
          ),
    manifests: objectOrNull(record.manifests ?? null, where, "manifests"),
    keyFound: keyFound === null ? null : textsAt(keyFound, where, "files that held the key"),
  };
}

/** The frozen code the record names, read back. */
function frozenOf(value: unknown, where: string): FrozenCode {
  const record = objectOrNull(value, where, "frozen");
  if (record === null) throw new CorpusError(`${where} names no frozen code`);
  return {
    codeSha256: sha256At(record.codeSha256, where, "codeSha256"),
    files: filesAt(record.files, where, "frozen files"),
    promptSha256: sha256At(record.promptSha256, where, "promptSha256"),
    bundleCodeSha256: sha256At(record.bundleCodeSha256, where, "bundleCodeSha256"),
    skillSha256: filesAt(record.skillSha256, where, "skillSha256"),
    asked: textAt(record.asked, where, "asked"),
    model: textAt(record.model, where, "model"),
  };
}

function outcomeCountsOf(value: unknown, where: string): OutcomeCounts {
  const record = objectOrNull(value, where, "counts");
  if (record === null) throw new CorpusError(`${where} holds no counts`);
  return {
    requests: countAt(record.requests, where, "requests"),
    holds: countAt(record.holds, where, "holds"),
    verdicts: countAt(record.verdicts, where, "verdicts"),
    byVerdict: countsByNameAt(record.byVerdict, where, "byVerdict"),
    deviations: countAt(record.deviations, where, "deviations"),
    deviationsByKind: countsByNameAt(record.deviationsByKind, where, "deviationsByKind"),
  };
}

/** The written stage's counts read back, under each version of the rule too. */
function countsOf(value: unknown, where: string): JudgedCounts | null {
  if (value === null) return null;
  const record = objectOrNull(value, where, "counts") ?? {};
  const byRule = objectOrNull(record.byRule ?? null, where, "counts byRule");
  if (byRule === null) throw new CorpusError(`${where} holds counts under no rule version`);
  return {
    ...outcomeCountsOf(record, where),
    byRule: eachRule((version) =>
      outcomeCountsOf(byRule[version] ?? null, `${where} byRule.${version}`)
    ),
  };
}

/** `requests/<ask id>/outcome.json` read back, refused in any other shape. */
export function judgedOutcomeOf(text: string, path: string): JudgedOutcome {
  const record = parsedRecord(text, path);
  const keys = [
    "askId",
    "issueKey",
    "group",
    "cases",
    "outcome",
    "verdict",
    "deviation",
    "judgement",
    "judges",
    "sandbox",
    "judgedAt",
    "byRule",
  ];
  for (const key of keys) {
    if (!Object.hasOwn(record, key)) throw new CorpusError(`${path} has no ${key}`);
  }
  const outcome = outcomeKindAt(record.outcome, path);
  const verdict = textOrNull(record.verdict, path, "verdict");
  let deviation: Deviation | null = null;
  if (record.deviation !== null) {
    const found = objectOrNull(record.deviation, path, "deviation") as Record<string, unknown>;
    deviation = {
      kind: deviationKindAt(found.kind, path),
      reason: textAt(found.reason, path, "deviation reason"),
      problems: listOf(found.problems).map((one) => textAt(one, path, "deviation problem")),
    };
  }
  const counted = heldToKind(outcome, verdict, deviation?.kind ?? null, path);
  if (!Array.isArray(record.judges)) throw new CorpusError(`${path} holds judges that are no list`);
  const byRule = byRuleOf(record.byRule, path, counted, judgedRuleOf);
  return {
    askId: textAt(record.askId, path, "askId"),
    issueKey: textOrNull(record.issueKey, path, "issueKey"),
    group: textAt(record.group, path, "group"),
    cases: countAt(record.cases, path, "cases"),
    outcome,
    verdict,
    deviation,
    judgement: judgementOf(record.judgement, path),
    judges: record.judges.map((one) => judgeRecordOf(one, path)),
    sandbox: sandboxFactsOf(record.sandbox, path),
    judgedAt: textOrNull(record.judgedAt, path, "judgedAt"),
    byRule,
  };
}

/** `outcomes.jsonl` read back, one line a request, refused in any other shape. */
export function outcomeLinesOf(text: string, path: string): OutcomeLine[] {
  const lines = text.split("\n");
  if (lines.pop() !== "") throw new CorpusError(`${path} does not end its last line`);
  return lines.map((line, at) => {
    const where = `${path} line ${at + 1}`;
    const record = parsedRecord(line, where);
    const outcome = outcomeKindAt(record.outcome, where);
    const verdict = textOrNull(record.verdict, where, "verdict");
    const deviation = record.deviation === null ? null : deviationKindAt(record.deviation, where);
    const counted = heldToKind(outcome, verdict, deviation, where);
    return {
      askId: textAt(record.askId, where, "askId"),
      issueKey: textOrNull(record.issueKey, where, "issueKey"),
      group: textAt(record.group, where, "group"),
      cases: countAt(record.cases, where, "cases"),
      outcome,
      verdict,
      deviation,
      byRule: byRuleOf(record.byRule, where, counted, ruleOutcomeOf),
      outcomeSha256: sha256At(record.outcomeSha256, where, "outcomeSha256"),
    };
  });
}

/** `s10-judged.json` read back, refused unless it is a record of this version in the stage's shape. */
export function judgedRecordOf(text: string, path: string): JudgedRecord {
  const record = parsedRecord(text, path);
  if (record.stage !== "s10-judged" || record.version !== JUDGED_VERSION) {
    throw new CorpusError(`${path} is no record of version ${JUDGED_VERSION} of the judged stage`);
  }
  if (record.state !== "started" && record.state !== "written") {
    throw new CorpusError(`${path} is neither started nor written`);
  }
  const inputs = objectOrNull(record.inputs, path, "inputs");
  if (inputs === null) throw new CorpusError(`${path} names no inputs`);
  return {
    stage: "s10-judged",
    version: JUDGED_VERSION,
    state: record.state,
    claimRun: textAt(record.claimRun, path, "claimRun"),
    reference: textAt(record.reference, path, "reference"),
    runDigest: sha256At(record.runDigest, path, "runDigest"),
    claimDirectory: textAt(record.claimDirectory, path, "claimDirectory"),
    inputs: {
      directory: textAt(inputs.directory, path, "inputs directory"),
      stageSha256: sha256At(inputs.stageSha256, path, "inputs stageSha256"),
      files: filesAt(inputs.files, path, "inputs files"),
    },
    frozen: frozenOf(record.frozen, path),
    image: textAt(record.image, path, "image"),
    serviceAccount: textAt(record.serviceAccount, path, "serviceAccount"),
    startedAt: textAt(record.startedAt, path, "startedAt"),
    finishedAt: textOrNull(record.finishedAt, path, "finishedAt"),
    stops: listOf(record.stops).map((one) => {
      const stop = objectOrNull(one, path, "stop") ?? {};
      return {
        at: textAt(stop.at, path, "stop time"),
        reason: textAt(stop.reason, path, "stop reason"),
      };
    }),
    counts: countsOf(record.counts, path),
    files: filesAt(record.files, path, "files"),
  };
}

/** One file the record names under a request's directory that is not there with its bytes. */
export interface LostEvidence {
  /** Its path under `requests/<ask id>/`. */
  file: string;
  problem: string;
}

/** One file the record names, read and held to the sha256 it names. */
async function heldBytes(directory: string, record: JudgedRecord, name: string): Promise<Buffer> {
  const path = join(directory, name);
  const bytes = await readFile(path).catch(() => null);
  if (bytes === null) throw new CorpusError(`${path}, which the stage record names, is not there`);
  if (sha256Of(bytes) !== record.files[name]) {
    throw new CorpusError(`${path} is not the file the stage record names`);
  }
  return bytes;
}

/**
 * A judged stage read whole: its record, with the sha256 of the bytes it was read from, every
 * `outcome.json` it names, and `outcomes.jsonl` wherever the record names it (a written stage's
 * always), each held to the sha256 the record names. A missing or changed one of these refuses
 * the stage, since the outcome it holds cannot be read; so does a written stage whose
 * `outcomes.jsonl` and outcomes disagree, and one ask with two outcomes. Every other file the
 * record names is read and held to its sha256 too: one under `requests/<ask id>/` that is not
 * there with those bytes (a transcript, a bundle, the answers, the calls, the sandbox's record,
 * `started.json`) is that request's lost evidence (`JudgedStage.evidence`), which the analysis
 * makes the request's deviation; one outside every request's directory (the kept frozen code, a
 * preflight's result) refuses the stage, which is then not what its record says.
 */
export async function judgedStageOf(directory: string): Promise<JudgedStage> {
  const recordPath = join(directory, JUDGED_FILE);
  const recordBytes = await readFile(recordPath).catch(() => null);
  if (recordBytes === null)
    throw new CorpusError(`${recordPath} could not be read: it takes the judged stage`);
  const record = judgedRecordOf(recordBytes.toString("utf8"), recordPath);
  const outcomes = new Map<string, JudgedOutcome>();
  const evidence = new Map<string, LostEvidence[]>();
  let outcomesText: string | null = null;
  for (const name of Object.keys(record.files).sort()) {
    if (name === OUTCOMES_FILE) {
      outcomesText = (await heldBytes(directory, record, name)).toString("utf8");
      continue;
    }
    if (!/^requests\/[^/]+\/outcome\.json$/.test(name)) {
      const bytes = await readFile(join(directory, name)).catch(() => null);
      if (bytes !== null && sha256Of(bytes) === record.files[name]) continue;
      const problem = bytes === null ? "is not there" : "is not the file the stage record names";
      const askId = /^requests\/([^/]+)\/./.exec(name)?.[1];
      if (askId === undefined) {
        throw new CorpusError(
          `${join(directory, name)}, which the stage record names, ${problem}: the stage is not ` +
            "what its record says"
        );
      }
      const lost = evidence.get(askId) ?? [];
      lost.push({ file: name.slice(`requests/${askId}/`.length), problem });
      evidence.set(askId, lost);
      continue;
    }
    const outcome = judgedOutcomeOf(
      (await heldBytes(directory, record, name)).toString("utf8"),
      join(directory, name)
    );
    if (name !== `requests/${outcome.askId}/${OUTCOME_FILE}` || outcomes.has(outcome.askId)) {
      throw new CorpusError(
        `${join(directory, name)} is the outcome of another request than its place`
      );
    }
    outcomes.set(outcome.askId, outcome);
  }
  let lines: OutcomeLine[] | null = null;
  if (record.state === "written") {
    const text =
      outcomesText ?? (await heldBytes(directory, record, OUTCOMES_FILE)).toString("utf8");
    lines = outcomeLinesOf(text, join(directory, OUTCOMES_FILE));
    const named = new Set(lines.map((line) => line.askId));
    if (named.size !== lines.length || named.size !== outcomes.size) {
      throw new CorpusError(`${join(directory, OUTCOMES_FILE)} does not list each outcome once`);
    }
    for (const line of lines) {
      const file = `requests/${line.askId}/${OUTCOME_FILE}`;
      const outcome = outcomes.get(line.askId);
      if (
        outcome === undefined ||
        record.files[file] !== line.outcomeSha256 ||
        outcome.outcome !== line.outcome ||
        RULE_VERSIONS.some(
          (version) =>
            outcome.byRule[version].outcome !== line.byRule[version].outcome ||
            outcome.byRule[version].verdict !== line.byRule[version].verdict ||
            outcome.byRule[version].deviation !== line.byRule[version].deviation
        )
      ) {
        throw new CorpusError(
          `${join(directory, OUTCOMES_FILE)} lists ${line.askId} otherwise than its outcome`
        );
      }
    }
  }
  return { record, recordSha256: sha256Of(recordBytes), outcomes, lines, evidence };
}

/**
 * Each request of the inventory with its one outcome, in the inventory's order: the stage's own;
 * else a `started` deviation where a run kept its `started.json` and stopped before its outcome
 * (point 6: it is never judged again); else an `unjudged` deviation, where no run of the stage
 * reached the request. An outcome of an ask the inventory does not hold is refused.
 */
export function reportedOutcomesOf(
  inventory: readonly InventoryLine[],
  stage: JudgedStage
): JudgedOutcome[] {
  const asks = new Set(inventory.map((line) => line.askId));
  for (const askId of stage.outcomes.keys()) {
    if (!asks.has(askId))
      throw new CorpusError(
        `the judged stage holds an outcome of ${askId}, which the inventory does not`
      );
  }
  return inventory.map((line) => {
    const found = stage.outcomes.get(line.askId);
    if (found !== undefined) return found;
    // The record names the request's `started.json`, read and held to its sha256 with every file
    // it names (`judgedStageOf`): a lost one is said beside the reason.
    const started = Object.hasOwn(stage.record.files, `requests/${line.askId}/${STARTED_FILE}`);
    const lost = stage.evidence.get(line.askId)?.find((one) => one.file === STARTED_FILE);
    const kind = started ? "started" : UNJUDGED;
    const reason = started ? STARTED_REASON : "no run of the judging stage reached the request";
    const problems =
      lost === undefined ? [reason] : [reason, `its ${STARTED_FILE} ${lost.problem}`];
    return {
      askId: line.askId,
      issueKey: line.issueKey,
      group: line.group,
      cases: line.cases,
      outcome: "deviation",
      verdict: null,
      deviation: { kind, reason, problems },
      judgement: null,
      judges: [],
      sandbox: null,
      judgedAt: null,
      byRule: eachRule(
        (): JudgedRuleOutcome => ({
          outcome: "deviation",
          verdict: null,
          deviation: kind,
          toolCallDeviations: [],
        })
      ),
    };
  });
}

/**
 * The answers one request's judges gave, in the order of its judges (its calls' order), as its
 * kept sandbox result holds them (`requests/<ask id>/sandbox/results.json`, held to the sha256 the
 * record names): each judge's answer is the entry of that list whose `id` is the judge's agent, a
 * text read as JSON where it is one, as `s10_transcripts.py`'s `answers_by_agent` reads them, a
 * later entry of one id taking the earlier's place there too. Where they cannot be read, why: no
 * kept result, a result that is no list of answers, a judge with no agent, or an agent with no
 * answer. A file the record names that is not there with its bytes is refused (`CorpusError`), as
 * `judgedStageOf` refuses one.
 */
export async function recordedAnswersOf(
  directory: string,
  stage: JudgedStage,
  outcome: JudgedOutcome
): Promise<{ answers: unknown[] } | { problem: string }> {
  const name = `requests/${outcome.askId}/${SANDBOX_DIRECTORY}/${RESULTS_FILE}`;
  if (!Object.hasOwn(stage.record.files, name)) {
    return { problem: `the stage keeps no ${RESULTS_FILE} of the request` };
  }
  const text = (await heldBytes(directory, stage.record, name)).toString("utf8");
  let listed: unknown;
  try {
    listed = JSON.parse(text);
  } catch {
    return { problem: `${name} is not JSON` };
  }
  if (!Array.isArray(listed) || !listed.every((one) => recordOf(one) !== null)) {
    return { problem: `${name} is no list of answers` };
  }
  const byAgent = new Map<string, unknown>();
  for (const one of listed as Array<Record<string, unknown>>) {
    let answer = one.result;
    if (typeof answer === "string") {
      try {
        answer = JSON.parse(answer);
      } catch {
        // A text that is no JSON is the answer as it is, as `answers_by_agent` keeps it.
      }
    }
    byAgent.set(String(one.id), answer);
  }
  const answers: unknown[] = [];
  for (const judge of outcome.judges) {
    if (judge.agent === null) return { problem: `${judge.label} has no agent, so no answer` };
    if (!byAgent.has(judge.agent)) {
      return { problem: `${name} holds no answer of ${judge.label} (${judge.agent})` };
    }
    answers.push(byAgent.get(judge.agent));
  }
  return { answers };
}
