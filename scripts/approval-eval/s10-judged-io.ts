import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { CorpusError, listOf, numberOf, recordOf, stringOf } from "./corpus-io";
import type { InventoryLine } from "./s10-inputs-io";

/**
 * The files the claim's judging stage (`s10_judge.py`) writes under its `--out`, which the
 * analysis (`analyze.ts --claim-s10`) reads back: their shapes, and the parsers that read each back
 * and refuse any other shape, as `s10-inputs-io.ts` is to the inputs stage.
 *
 * `s10-judged.json` (`JudgedRecord`, `judgedRecordOf`) chains the stage to its inputs and names the
 * sha256 of every file written; `requests/<ask id>/outcome.json` (`JudgedOutcome`,
 * `judgedOutcomeOf`) is one request's one outcome, every key present, with its outcome under the
 * tool rule of each version of point 5 (`byRule`, version 5's the one that counts) and the calls
 * each refuses; and `outcomes.jsonl` (`OutcomeLine`, `outcomeLinesOf`), which only a run that gave
 * every request its outcome writes, lists them in the inventory's order. `judgedStageOf` reads a
 * directory whole, each file held to the sha256 its record names. A stage that stopped stays
 * `started`, and `reportedOutcomesOf` gives each request of the inventory its outcome, `unjudged`
 * where no run reached it: the one place that kind is made.
 */

export const JUDGED_FILE = "s10-judged.json";
export const OUTCOMES_FILE = "outcomes.jsonl";
export const OUTCOME_FILE = "outcome.json";

/** The version of the stage's shapes, which every reader checks. */
export const JUDGED_VERSION = 3;

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
  "started",
] as const;

export type DeviationKind = (typeof DEVIATION_KINDS)[number];

/** A request of the inventory no run of the stage reached: made by `reportedOutcomesOf` alone. */
export const UNJUDGED = "unjudged";

/** The verdict a fact hold gives, with no judge. */
export const HOLD_VERDICT = "never_decide:waiting-legion-root";

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
export const RULE_VERSIONS = ["v3", "v4", "v5"] as const;
export type RuleVersion = (typeof RULE_VERSIONS)[number];
export const COUNTING_RULE: RuleVersion = "v5";

/** One tool call a version of the rule makes a deviation. */
export interface ToolCallDeviation {
  judge: string;
  callId: string;
  tools: string[];
  reason: string;
}

/** A request's outcome under one version of the rule. */
export interface RuleOutcome {
  outcome: "hold" | "verdict" | "deviation";
  verdict: string | null;
  deviation: DeviationKind | typeof UNJUDGED | null;
}

/** A request's outcome under one version of the rule, with the calls that version refuses. */
export interface JudgedRuleOutcome extends RuleOutcome {
  toolCallDeviations: ToolCallDeviation[];
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
  judgement: Record<string, unknown> | null;
  judges: Array<Record<string, unknown>>;
  /** What the sandbox recorded of its checks and its pod; null where no sandbox ran. */
  sandbox: Record<string, unknown> | null;
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
  frozen: Record<string, unknown>;
  image: string;
  serviceAccount: string;
  startedAt: string;
  finishedAt: string | null;
  /** Each stop of a run, which left the stage to be resumed. */
  stops: Array<{ at: string; reason: string }>;
  counts: Record<string, unknown> | null;
  /** The sha256 of every file written under the stage's directory, by its path there. */
  files: Record<string, string>;
}

/** A stage read whole: its record, each request's outcome by ask id, and its outcome lines. */
export interface JudgedStage {
  record: JudgedRecord;
  outcomes: Map<string, JudgedOutcome>;
  /** `outcomes.jsonl`'s lines, null while the stage is started. */
  lines: OutcomeLine[] | null;
}

function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function parsed(text: string, where: string): Record<string, unknown> {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    throw new CorpusError(`${where} is not JSON`);
  }
  const record = recordOf(value);
  if (record === null) throw new CorpusError(`${where} is no object`);
  return record;
}

function textAt(value: unknown, where: string, field: string): string {
  const text = stringOf(value);
  if (text === null) throw new CorpusError(`${where} holds a ${field} that is no text`);
  return text;
}

function textOrNull(value: unknown, where: string, field: string): string | null {
  return value === null ? null : textAt(value, where, field);
}

function sha256At(value: unknown, where: string, field: string): string {
  const text = textAt(value, where, field);
  if (!/^[0-9a-f]{64}$/.test(text))
    throw new CorpusError(`${where} holds a ${field} that is no sha256`);
  return text;
}

function countAt(value: unknown, where: string, field: string): number {
  const count = numberOf(value);
  if (count === null || !Number.isSafeInteger(count) || count < 0) {
    throw new CorpusError(`${where} holds a ${field} that is no count`);
  }
  return count;
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

/** Holds an outcome's verdict and deviation to its kind of outcome. */
function heldToKind(
  outcome: JudgedOutcome["outcome"],
  verdict: string | null,
  deviation: unknown,
  where: string
): void {
  const fits =
    outcome === "deviation"
      ? verdict === null && deviation !== null
      : deviation === null &&
        (outcome === "hold"
          ? verdict === HOLD_VERDICT
          : verdict === "approve" ||
            verdict === "request_changes" ||
            verdict?.startsWith("never_decide:") === true);
  if (!fits)
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
  heldToKind(outcome, verdict, deviation, where);
  return { outcome, verdict, deviation };
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

/** `requests/<ask id>/outcome.json` read back, refused in any other shape. */
export function judgedOutcomeOf(text: string, path: string): JudgedOutcome {
  const record = parsed(text, path);
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
  heldToKind(outcome, verdict, deviation, path);
  if (!Array.isArray(record.judges)) throw new CorpusError(`${path} holds judges that are no list`);
  const byRule = byRuleOf(
    record.byRule,
    path,
    { outcome, verdict, deviation: deviation?.kind ?? null },
    judgedRuleOf
  );
  return {
    askId: textAt(record.askId, path, "askId"),
    issueKey: textOrNull(record.issueKey, path, "issueKey"),
    group: textAt(record.group, path, "group"),
    cases: countAt(record.cases, path, "cases"),
    outcome,
    verdict,
    deviation,
    judgement: objectOrNull(record.judgement, path, "judgement"),
    judges: record.judges.map((one) => objectOrNull(one, path, "judge") ?? {}),
    sandbox: objectOrNull(record.sandbox, path, "sandbox"),
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
    const record = parsed(line, where);
    const outcome = outcomeKindAt(record.outcome, where);
    const verdict = textOrNull(record.verdict, where, "verdict");
    const deviation = record.deviation === null ? null : deviationKindAt(record.deviation, where);
    heldToKind(outcome, verdict, deviation, where);
    return {
      askId: textAt(record.askId, where, "askId"),
      issueKey: textOrNull(record.issueKey, where, "issueKey"),
      group: textAt(record.group, where, "group"),
      cases: countAt(record.cases, where, "cases"),
      outcome,
      verdict,
      deviation,
      byRule: byRuleOf(record.byRule, where, { outcome, verdict, deviation }, ruleOutcomeOf),
      outcomeSha256: sha256At(record.outcomeSha256, where, "outcomeSha256"),
    };
  });
}

/** `s10-judged.json` read back, refused unless it is a record of this version in the stage's shape. */
export function judgedRecordOf(text: string, path: string): JudgedRecord {
  const record = parsed(text, path);
  if (record.stage !== "s10-judged" || record.version !== JUDGED_VERSION) {
    throw new CorpusError(`${path} is no record of version ${JUDGED_VERSION} of the judged stage`);
  }
  if (record.state !== "started" && record.state !== "written") {
    throw new CorpusError(`${path} is neither started nor written`);
  }
  const inputs = objectOrNull(record.inputs, path, "inputs");
  if (inputs === null) throw new CorpusError(`${path} names no inputs`);
  const frozen = objectOrNull(record.frozen, path, "frozen");
  if (frozen === null) throw new CorpusError(`${path} names no frozen code`);
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
    frozen,
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
    counts: objectOrNull(record.counts, path, "counts"),
    files: filesAt(record.files, path, "files"),
  };
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
 * A judged stage read whole: its record, every `outcome.json` it names, and `outcomes.jsonl` where
 * the stage is written, each held to the sha256 the record names; refused where `outcomes.jsonl`
 * and the outcomes disagree, or one ask has two outcomes.
 */
export async function judgedStageOf(directory: string): Promise<JudgedStage> {
  const recordPath = join(directory, JUDGED_FILE);
  const recordText = await readFile(recordPath, "utf8").catch(() => null);
  if (recordText === null)
    throw new CorpusError(`${recordPath} could not be read: it takes the judged stage`);
  const record = judgedRecordOf(recordText, recordPath);
  const outcomes = new Map<string, JudgedOutcome>();
  for (const name of Object.keys(record.files).sort()) {
    if (!/^requests\/[^/]+\/outcome\.json$/.test(name)) continue;
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
    const text = (await heldBytes(directory, record, OUTCOMES_FILE)).toString("utf8");
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
  return { record, outcomes, lines };
}

/**
 * Each request of the inventory with its one outcome, in the inventory's order: the stage's own,
 * or an `unjudged` deviation where no run of the stage reached the request. An outcome of an ask
 * the inventory does not hold is refused.
 */
export function reportedOutcomesOf(
  inventory: readonly InventoryLine[],
  outcomes: ReadonlyMap<string, JudgedOutcome>
): JudgedOutcome[] {
  const asks = new Set(inventory.map((line) => line.askId));
  for (const askId of outcomes.keys()) {
    if (!asks.has(askId))
      throw new CorpusError(
        `the judged stage holds an outcome of ${askId}, which the inventory does not`
      );
  }
  const unjudged = (): JudgedRuleOutcome => ({
    outcome: "deviation",
    verdict: null,
    deviation: UNJUDGED,
    toolCallDeviations: [],
  });
  return inventory.map(
    (line) =>
      outcomes.get(line.askId) ?? {
        askId: line.askId,
        issueKey: line.issueKey,
        group: line.group,
        cases: line.cases,
        outcome: "deviation",
        verdict: null,
        deviation: {
          kind: UNJUDGED,
          reason: "no run of the judging stage reached the request",
          problems: ["no run of the judging stage reached the request"],
        },
        judgement: null,
        judges: [],
        sandbox: null,
        judgedAt: null,
        byRule: eachRule(unjudged),
      }
  );
}
