import { createHash } from "node:crypto";
import { appendFile, copyFile, cp, mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { labelKey } from "./analyze";
import type { CaseRow } from "./build-corpus";
import {
  type Corpus,
  CorpusError,
  listOf,
  OutputPathError,
  outDirectory,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
  verdictOf,
} from "./corpus-io";
import { labelsOf } from "./models";
import { renderPrompt } from "./recipes";
import { assertNoLeak, LeakError, renderCase } from "./render";
import type { ReplayedDocument } from "./replay";
import {
  clopperPearsonInterval,
  type Interval,
  mcnemarExact,
  type PairedComparison,
} from "./stats";

/**
 * Scores a fresh agent given this checkout's `skills/approval-review` against the same agent
 * given no skill at all, over the same cases of the report slice, in one run.
 *
 * This is check C6 of the plan (dispatch://INFRA-2897): the skills carry the answer, and another
 * agent reproduces the number. The skill records what the round measured over the 250 approval
 * cases of the report slice - `many-shot-opus-5-5` 0.772 against `bare-opus-5-5` 0.656, a gain of
 * 0.116 for the retrieval recipe - and this asks whether an agent handed the skill, and nothing
 * else, beats an agent handed nothing.
 *
 * **The two arms differ in the skill and in nothing else.** Each arm is the same binary, the same
 * model, the same flags and the same one message. What differs is the profile the agent starts
 * under: one carries `skills/approval-review` as its only skill, so `skill://approval-review`
 * resolves for it, and one carries no skill, so that read fails. Both profiles are built under
 * `--out` from this checkout, and both carry the models file of the profile this command runs
 * under, which is how either arm reaches a model at all.
 *
 * **What the agent may read, which is what `--neighbours` chooses.** The prompt is the round's
 * own `bare` prompt over the round's own evidence: the documents of the request at the version
 * it named, read back from the evidence cache of `--round`, rendered by `render.ts` and held to
 * `assertNoLeak`. Nothing is read live, and the agent's environment carries no Dispatch
 * credential, so no arm can reach the record. In the `with` shape, `past-decisions.md` sits
 * beside each case holding the neighbours the round's own retrieval found for it, which is what
 * the round's many-shot arms were shown, and both arms are told of the file in the same words
 * and neither is told to read it. In the `without` shape no such file is written and neither
 * arm is told of one.
 *
 * **Neither shape alone answers the question, which is why both are kept.** With the file, the
 * control reads it unprompted, so the comparison measures the skill's text on top of a recipe
 * both arms run. Without it, the skill arm can read the skill but cannot run the retrieval the
 * skill records, so the comparison measures the skill's text alone against nothing. The gain
 * the skill records is retrieval over nothing, and no shape reachable from here separates that:
 * `fresh-agent.json` holds both shapes so a reader sees the pair and not one number.
 *
 * **One judgement per case and arm.** The round took five and read the mode; this takes one, so
 * its numbers carry a wider interval than the round's over the same count of cases.
 *
 * The readout is the two agreements with their intervals, the two error kinds apart, the paired
 * difference with its exact McNemar test, and the verdict: the check passes when the skill arm
 * beats the control by more than the interval of the difference, and fails when it does not. The
 * control's number is reported whatever it is.
 */

/** The model both arms run on, which is the model the numbers the skill records were measured on. */
const MODEL = "claude-opus-5-5";

/** The profile name both arms use, each under its own home, so neither sees the other's state. */
const PROFILE = "fresh";

/** How many cases both arms are scored over where no `--cases` says otherwise. */
const DEFAULT_CASES = 100;

/** How many agent runs are in flight at once where no `--concurrency` says otherwise. */
const DEFAULT_CONCURRENCY = 4;

/**
 * Which shape a run is: whether the neighbours the round's retrieval found sit beside the case
 * as `past-decisions.md`, with both arms told of the file, or whether no such file is written
 * and neither arm is told of one.
 */
export type Shape = "with" | "without";

/** Both shapes, which is what `fresh-agent.json` is keyed by. */
export const SHAPES: readonly Shape[] = ["with", "without"];

/** The shape a run takes where no `--neighbours` says otherwise, which is the first one run. */
const DEFAULT_SHAPE: Shape = "with";

/** The agent this scores where no `--omp` names another. */
const DEFAULT_OMP = "/opt/omp/bin/omp";

/** How long one agent run may take before it is killed and counted as a fault. */
const RUN_TIMEOUT_MS = 300_000;

/** How many characters of an unreadable answer a row and a printed line carry. */
const DETAIL_CHARS = 400;

/** The shape of the evidence cache `run-round.ts` writes, which this reads and never writes. */
const CACHE_VERSION = 1;

/** The slice and the kind of case this is scored over, which is what the skill's numbers name. */
const SLICE = "report";
const KIND = "approval";

/** What the round measured over those same cases, which the skill records and a reader compares. */
const ROUND = {
  skillArm: "many-shot-opus-5-5",
  skillAgreed: 193,
  controlArm: "bare-opus-5-5",
  controlAgreed: 164,
  cases: 250,
} as const;

/** The check passed: the skill arm beat the control by more than the interval of the difference. */
const EXIT_PASSES = 0;

/** The check failed, which is a finished run and a reading, not an error. */
const EXIT_FAILS = 1;

/** The run could not be made at all. */
const EXIT_CANNOT_RUN = 2;

const USAGE = `usage: bun scripts/approval-eval/fresh-agent.ts --corpus <dir> --round <dir> \\
  --out <dir> [--cases <n>] [--neighbours <with|without>] [--omp <path>] [--concurrency <n>]

Scores a fresh agent given this checkout's skills/approval-review against the same agent given
no skill, over the same cases of the report slice, and prints the paired difference.

Options:
  --corpus <dir>       Required. The corpus directory the cases are read from.
  --round <dir>        Required. A directory run-round.ts wrote: its rows.jsonl names the report
                       slice's approval cases, and its evidence cache is what the agent reads.
  --out <dir>          Required. An absolute path outside this workspace. Holds the two profiles,
                       the case directories, answers-<shape>.jsonl and fresh-agent.json.
  --cases <n>          How many cases, from the head of the report slice's approval cases sorted
                       by case id. Default ${DEFAULT_CASES}.
  --neighbours <shape> with: past-decisions.md sits beside each case and both arms are told of
                       it. without: no such file, and neither arm is told of one. Default
                       ${DEFAULT_SHAPE}. Each shape is kept in fresh-agent.json under its name.
  --omp <path>         The agent binary. Default ${DEFAULT_OMP}.
  --concurrency <n>    How many agent runs at once. Default ${DEFAULT_CONCURRENCY}.

Exits ${EXIT_PASSES} when the check passes, ${EXIT_FAILS} when it fails, and ${EXIT_CANNOT_RUN} when it could not be run.`;

/** The run cannot be made over these cases, or what it read is not what it needs. */
export class FreshError extends Error {
  override readonly name = "FreshError";
}

/** The two arms: the agent with this checkout's skill, and the same agent with no skill at all. */
export type ArmName = "with-skill" | "no-skill";

/** Both arms, in the order they are printed. The first is the one the check is about. */
export const ARMS: readonly ArmName[] = ["with-skill", "no-skill"];

/** One case as both arms are given it. */
export interface FreshCase {
  row: CaseRow;
  /** The one message both arms are sent. */
  prompt: string;
  /** The option labels an answer names, which is what a verdict is read from. */
  labels: string[];
  /** The directory the agent is started in, which holds the past decisions it may read. */
  directory: string;
}

/** One run of the agent, as the deps are asked for it. */
export interface AgentRequest {
  arm: ArmName;
  caseId: string;
  /** The home the arm's profile lives under: the skill is in one of them and in no other. */
  home: string;
  directory: string;
  prompt: string;
}

/** What one agent run came back with. */
export interface AgentAnswer {
  /** The last text the agent printed, which is where its verdict is read from. */
  text: string;
  /** The model the agent reports it ran on, and null where it reported none. */
  model: string | null;
  latencyMs: number;
  /** Why the run gave nothing, and null where it ran. */
  failure: string | null;
}

/** What the run reads the world through, so a test can hand it written answers. */
export interface FreshDeps {
  ask(request: AgentRequest): Promise<AgentAnswer>;
  now(): number;
  log(line: string): void;
}

/** One arm's answer on one case, as one line of `answers.jsonl`. */
export interface AnswerRow {
  arm: ArmName;
  caseId: string;
  model: string | null;
  /** The labels the answer chose, and empty where none could be read. */
  chosen: string[];
  agreed: boolean;
  /** Why no verdict was read: the agent gave nothing, or what it gave was no label. */
  fault: "agent" | "unreadable" | null;
  faultDetail: string | null;
  latencyMs: number;
  at: string;
}

/** One arm's reading over the cases both arms saw. */
export interface ArmReading {
  arm: ArmName;
  /** Every model the agent reported for this arm, which is one where the arm ran as asked. */
  models: string[];
  cases: number;
  agreed: number;
  agreement: number;
  interval: Interval;
  /** The cases a verdict was read for. A fault is never one of them. */
  decided: number;
  faults: number;
  /** The arm approved where the person asked for changes. */
  falseApprovals: number;
  /** The arm asked for changes where the person approved. */
  falseRequests: number;
  /** Disagreements that are neither, one side having chosen some other option. */
  otherErrors: number;
}

/** What this command answers for one shape: printed, and written as `fresh-agent.json`. */
export interface FreshReport {
  /** Which shape this run was, which is what the reading is only comparable within. */
  neighbours: Shape;
  startedAt: string;
  finishedAt: string;
  wallMs: number;
  /** The model both arms were asked for, beside the model each reports it ran on. */
  model: string;
  caseIds: string[];
  arms: ArmReading[];
  /** The skill arm against the control, paired case by case. */
  paired: PairedComparison;
  /** What the round measured over its 250 cases, for the reader to compare against. */
  round: {
    skillArm: string;
    skillAgreement: number;
    controlArm: string;
    controlAgreement: number;
    gain: number;
    cases: number;
  };
  verdict: { passes: boolean; line: string };
}

/** Both shapes as `fresh-agent.json` holds them, each written by the run that made it. */
export type FreshShapes = { [shape in Shape]?: FreshReport };

/** What one run is asked to score. */
export interface FreshPlan {
  neighbours: Shape;
  cases: readonly FreshCase[];
  /** Where `answers-<shape>.jsonl` and `fresh-agent.json` are written. */
  out: string;
  /** The home each arm's profile lives under. */
  homes: Readonly<Record<ArmName, string>>;
  concurrency: number;
}

/** The flags, as the command reads them. */
interface Flags {
  corpus: string;
  round: string;
  out: string;
  cases: number;
  neighbours: Shape;
  omp: string;
  concurrency: number;
}

/**
 * What both arms are told about the record, in the same words, in either shape: no live
 * document and no way to reach one.
 */
const NO_RECORD_NOTE = [
  "Nothing of the record is reachable from this machine: there is no live document, no search and",
  "no `dispatch` command.",
].join(" ");

/**
 * What both arms are told in the `with` shape, and neither arm in the `without` shape, which is
 * the whole of the difference between the two prompts. Nothing here says to read the file: the
 * skill is what names the retrieval recipe, and whether the arm given the skill uses it is the
 * thing being measured.
 */
const FILE_NOTE = [
  "One file sits in the directory you are started in, `past-decisions.md`,",
  "holding approval decisions this person answered before this case that a search of the record",
  "found for it. Whether you read it is yours to decide.",
].join(" ");

/** The key one answer row is held under: one row per arm and case, never two. */
function rowKey(arm: ArmName, caseId: string): string {
  return `${arm}\u0000${caseId}`;
}

/** One detail on one line, bounded, as a row and a printed line carry it. */
function oneLine(detail: string): string {
  const line = detail.replace(/\s+/g, " ").trim();
  return line.length > DETAIL_CHARS ? `${line.slice(0, DETAIL_CHARS)}…` : line;
}

/** An agreement as the readout prints it. */
function rate(value: number): string {
  return value.toFixed(3);
}

/** A difference as the readout prints it, which always carries its sign. */
function signed(value: number): string {
  return `${value < 0 ? "−" : "+"}${Math.abs(value).toFixed(3)}`;
}

/** An interval as the readout prints it. */
function span(interval: Interval): string {
  return `[${signed(interval.lower)}, ${signed(interval.upper)}]`;
}

/**
 * The last text the agent printed and the model it says it ran on, read from its JSON event
 * stream. A later assistant message replaces an earlier one, so what is left is the answer.
 */
export function answerOf(stdout: string): { text: string; model: string | null } {
  let text = "";
  let model: string | null = null;
  for (const line of stdout.split("\n")) {
    if (line.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    const message = recordOf(recordOf(parsed)?.message);
    if (message === null || message.role !== "assistant") continue;
    const said = textOfContent(message.content);
    if (said !== "") text = said;
    model = stringOf(message.model) ?? model;
  }
  return { text, model };
}

/** The text blocks of one assistant message, joined as the agent printed them. */
function textOfContent(content: unknown): string {
  const parts: string[] = [];
  for (const value of listOf(content)) {
    const block = recordOf(value);
    if (block?.type !== "text") continue;
    const text = stringOf(block.text);
    if (text !== null) parts.push(text);
  }
  return parts.join("\n").trim();
}

/**
 * The neighbours of one case as the agent may read them: the question, the verdict and the
 * person's own words, and never the neighbour's document, which is what `recipes.ts` puts in
 * front of the round's many-shot arms.
 */
export function pastDecisions(neighbours: readonly CaseRow[]): string {
  const lines = [
    `# ${neighbours.length} decisions this person made before this one`,
    "",
    "Each is a past case with what they decided. None is the case you are judging.",
    "",
  ];
  neighbours.forEach((row, at) => {
    lines.push(`## Decision ${at + 1}`, "", `Question: ${row.question ?? "none"}`);
    if (row.kind === "approval") lines.push(`Their verdict: ${row.verdict ?? "none"}`);
    if (row.selected.length > 0) lines.push(`They chose: ${row.selected.join(", ")}`);
    if (row.hasWords && row.words !== null) lines.push("", "Their words:", row.words.trim());
    lines.push("");
  });
  return lines.join("\n");
}

/** The option labels one case offered, which is what an answer names. */
export function labelsOfCase(row: CaseRow): string[] {
  const labels: string[] = [];
  for (const option of row.options) {
    const label = option.label?.trim();
    if (label && !labels.includes(label)) labels.push(label);
  }
  return labels;
}

/**
 * The one message both arms are sent over one case: the round's own `bare` prompt over the
 * round's own evidence, with the note that says what else is readable. The shape decides that
 * note and nothing else, so the two shapes differ by the sentence about the file and by no
 * other word.
 */
export async function promptOf(
  row: CaseRow,
  documents: readonly ReplayedDocument[],
  issueTitle: string | null,
  neighbours: Shape
): Promise<string> {
  const evidence = renderCase(row, documents, { issueTitle });
  assertNoLeak(row, evidence);
  const prompt = await renderPrompt("bare", row, evidence);
  const note = neighbours === "with" ? `${NO_RECORD_NOTE} ${FILE_NOTE}` : NO_RECORD_NOTE;
  return `${prompt.system}\n\n${note}\n\n${prompt.user}`;
}

/** The rows of one arm, by case, which is what a reading and a pairing are taken over. */
function byCase(rows: readonly AnswerRow[], arm: ArmName): Map<string, AnswerRow> {
  const held = new Map<string, AnswerRow>();
  for (const row of rows) if (row.arm === arm) held.set(row.caseId, row);
  return held;
}

/** One arm's row for one case, or a refusal naming what is missing. */
function rowOfCase(held: ReadonlyMap<string, AnswerRow>, arm: ArmName, caseId: string): AnswerRow {
  const row = held.get(caseId);
  if (row === undefined) {
    throw new FreshError(`the ${arm} arm has no answer for the case ${caseId}`);
  }
  return row;
}

/**
 * One arm's reading. The agreement is over every case the arm was given, so a case it answered
 * in no readable shape counts against it and is never an agreement; the two error kinds are
 * counted over the cases it reached a verdict on, which is what `analyze.ts` counts them over.
 */
export function readingOf(
  arm: ArmName,
  rows: readonly AnswerRow[],
  cases: readonly FreshCase[]
): ArmReading {
  const held = byCase(rows, arm);
  const models = new Set<string>();
  let agreed = 0;
  let decided = 0;
  let faults = 0;
  let falseApprovals = 0;
  let falseRequests = 0;
  let otherErrors = 0;
  for (const one of cases) {
    const row = rowOfCase(held, arm, one.row.caseId);
    if (row.model !== null) models.add(row.model);
    if (row.fault !== null) {
      faults += 1;
      continue;
    }
    decided += 1;
    if (row.agreed) {
      agreed += 1;
      continue;
    }
    const chose = verdictOf(row.chosen, one.row.options);
    const person = one.row.verdict;
    if (person === "request_changes" && chose === "approve") falseApprovals += 1;
    else if (person === "approve" && chose === "request_changes") falseRequests += 1;
    else otherErrors += 1;
  }
  return {
    arm,
    models: [...models].sort(),
    cases: cases.length,
    agreed,
    agreement: cases.length === 0 ? 0 : agreed / cases.length,
    interval:
      cases.length === 0 ? { lower: 0, upper: 0 } : clopperPearsonInterval(agreed, cases.length),
    decided,
    faults,
    falseApprovals,
    falseRequests,
    otherErrors,
  };
}

/**
 * The skill arm against the control, paired case by case over the cases both arms saw, which is
 * the only comparison two readings of one set of cases carry.
 */
export function pairedOf(
  rows: readonly AnswerRow[],
  cases: readonly FreshCase[]
): PairedComparison {
  const skill = byCase(rows, "with-skill");
  const control = byCase(rows, "no-skill");
  return mcnemarExact(
    cases.map((one) => ({
      armAgreed: rowOfCase(skill, "with-skill", one.row.caseId).agreed,
      otherAgreed: rowOfCase(control, "no-skill", one.row.caseId).agreed,
    }))
  );
}

/**
 * The verdict: the check passes when the skill arm beats the control by more than the interval
 * of the difference, which is the interval holding every difference the pairs are consistent
 * with, and fails when it does not.
 */
export function verdictLineOf(paired: PairedComparison): { passes: boolean; line: string } {
  const passes = paired.interval.lower > 0;
  const difference = `${signed(paired.difference)} in ${span(paired.interval)}`;
  return {
    passes,
    line: passes
      ? `the check passes: the skill arm beats the control by more than the interval of the ` +
        `difference (${difference})`
      : `the check fails: the skill arm does not beat the control by more than the interval of ` +
        `the difference (${difference})`,
  };
}

/**
 * The answer rows already written under `--out`, by arm and case, the last row of each key
 * winning. A key whose row answered is never paid for twice, and a key whose row carries a
 * fault is asked again, which is the resume the round keeps.
 */
async function heldAnswers(path: string): Promise<Map<string, AnswerRow>> {
  const held = new Map<string, AnswerRow>();
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return held;
  }
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    // The file is this command's own, so a line that parses is a row it wrote.
    const row = parsed as AnswerRow;
    if (typeof row.arm === "string" && typeof row.caseId === "string") {
      held.set(rowKey(row.arm, row.caseId), row);
    }
  }
  return held;
}

/** Runs `run` over `items`, `concurrency` of them at once. */
async function pool<T>(
  items: readonly T[],
  concurrency: number,
  run: (item: T) => Promise<void>
): Promise<void> {
  let next = 0;
  const workers = Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    for (;;) {
      const at = next;
      next += 1;
      const item = items[at];
      if (item === undefined) return;
      await run(item);
    }
  });
  await Promise.all(workers);
}

/** One arm's judgement of one case: one run of the agent, read as a verdict. */
async function askOne(
  arm: ArmName,
  one: FreshCase,
  home: string,
  deps: FreshDeps
): Promise<AnswerRow> {
  const answer = await deps.ask({
    arm,
    caseId: one.row.caseId,
    home,
    directory: one.directory,
    prompt: one.prompt,
  });
  const chosen = answer.failure === null ? labelsOf(answer.text, one.labels) : [];
  const fault = answer.failure !== null ? "agent" : chosen.length === 0 ? "unreadable" : null;
  const detail =
    answer.failure !== null
      ? oneLine(answer.failure)
      : fault === null
        ? null
        : oneLine(answer.text);
  return {
    arm,
    caseId: one.row.caseId,
    model: answer.model,
    chosen,
    agreed: fault === null && labelKey(chosen) === labelKey(one.row.selected),
    fault,
    faultDetail: detail,
    latencyMs: answer.latencyMs,
    at: new Date(deps.now()).toISOString(),
  };
}

/** Where one shape's answers are held, so one shape never reads another shape's rows. */
export function answersPath(out: string, neighbours: Shape): string {
  return join(out, `answers-${neighbours}.jsonl`);
}

/** What each shape is, in the words the readout prints. */
const SHAPE_SAYS: Readonly<Record<Shape, string>> = {
  with: "past-decisions.md sits beside each case, and both arms are told of it",
  without: "no past-decisions.md is written, and neither arm is told of one",
};

/**
 * The run itself: every case put to both arms, one judgement each, with the answers appended as
 * they come so a run that stopped resumes rather than paying for what it already has.
 */
export async function runFresh(plan: FreshPlan, deps: FreshDeps): Promise<FreshReport> {
  const startedAt = deps.now();
  const path = answersPath(plan.out, plan.neighbours);
  const held = await heldAnswers(path);
  const tasks = plan.cases.flatMap((one) =>
    ARMS.map((arm) => ({ arm, one })).filter((task) => {
      const row = held.get(rowKey(task.arm, task.one.row.caseId));
      return row === undefined || row.fault !== null;
    })
  );
  const rows: AnswerRow[] = [...held.values()];
  let asked = 0;
  await pool(tasks, plan.concurrency, async (task) => {
    const row = await askOne(task.arm, task.one, plan.homes[task.arm], deps);
    rows.push(row);
    await appendFile(path, `${JSON.stringify(row)}\n`, "utf8");
    asked += 1;
    deps.log(
      `${String(asked).padStart(4)}/${tasks.length} ${task.arm.padEnd(10)} ` +
        `${row.fault ?? (row.agreed ? "agreed" : "differed")} ${row.caseId}` +
        `${row.faultDetail === null ? "" : `: ${row.faultDetail}`}`
    );
  });
  const finishedAt = deps.now();
  const paired = pairedOf(rows, plan.cases);
  return {
    neighbours: plan.neighbours,
    startedAt: new Date(startedAt).toISOString(),
    finishedAt: new Date(finishedAt).toISOString(),
    wallMs: finishedAt - startedAt,
    model: MODEL,
    caseIds: plan.cases.map((one) => one.row.caseId),
    arms: ARMS.map((arm) => readingOf(arm, rows, plan.cases)),
    paired,
    round: {
      skillArm: ROUND.skillArm,
      skillAgreement: ROUND.skillAgreed / ROUND.cases,
      controlArm: ROUND.controlArm,
      controlAgreement: ROUND.controlAgreed / ROUND.cases,
      gain: (ROUND.skillAgreed - ROUND.controlAgreed) / ROUND.cases,
      cases: ROUND.cases,
    },
    verdict: verdictLineOf(paired),
  };
}

/** One shape's reading, as the readout prints it. */
function shapeLines(report: FreshReport): string[] {
  const lines = [
    `shape ${report.neighbours.padEnd(12)}${SHAPE_SAYS[report.neighbours]}`,
    `                  wall ${Math.round(report.wallMs / 1000)}s`,
  ];
  for (const arm of report.arms) {
    lines.push(
      `  arm ${arm.arm.padEnd(12)}agreement ${rate(arm.agreement)} (${arm.agreed} of ` +
        `${arm.cases}) in [${rate(arm.interval.lower)}, ${rate(arm.interval.upper)}]`,
      `                  decided ${arm.decided}, faults ${arm.faults}, false approvals ` +
        `${arm.falseApprovals}, false requests ${arm.falseRequests}, other ${arm.otherErrors}`,
      `                  model as the agent reports it: ${arm.models.join(", ") || "none"}`
    );
  }
  const paired = report.paired;
  lines.push(
    `  paired          with-skill − no-skill = ${signed(paired.difference)} in ` +
      `${span(paired.interval)}, McNemar exact p ${paired.pValue.toFixed(3)}`,
    `                  over ${paired.counted} cases: both ${paired.bothAgreed}, skill alone ` +
      `${paired.armOnly}, control alone ${paired.otherOnly}, neither ${paired.neither}`,
    `  verdict         ${report.verdict.line}`
  );
  return lines;
}

/**
 * The report as the command prints it: the shape this run made, every other shape the output
 * directory already holds, and the round's own numbers beneath both. Neither shape alone
 * answers the check, so neither is printed alone where the other has been run.
 */
export function reportLines(report: FreshReport, held: FreshShapes = {}): string[] {
  const lines = [
    `cases             ${report.caseIds.length}, the head of the ${SLICE} slice's ${KIND} ` +
      "cases by case id",
    `model asked       ${report.model}`,
    ...shapeLines(report),
  ];
  for (const shape of SHAPES) {
    const other = held[shape];
    if (shape !== report.neighbours && other !== undefined) lines.push(...shapeLines(other));
  }
  lines.push(
    `round             ${report.round.skillArm} ${rate(report.round.skillAgreement)} against ` +
      `${report.round.controlArm} ${rate(report.round.controlAgreement)}, a gain of ` +
      `${rate(report.round.gain)} over ${report.round.cases} cases`,
    "case ids"
  );
  for (const caseId of report.caseIds) lines.push(`  ${caseId}`);
  return lines;
}

/** Where `run-round.ts` cached one case's evidence, keyed by the digest of its case id. */
function cachePath(round: string, caseId: string): string {
  const digest = createHash("sha256").update(caseId, "utf8").digest("hex").slice(0, 16);
  return join(round, "evidence", `${digest}.json`);
}

/** What one case's evidence cache holds, of the fields this reads. */
interface CachedEvidence {
  version: number;
  caseId: string;
  issueTitle: string | null;
  documents: ReplayedDocument[];
  nearest: { neighbours: { caseId: string }[] } | null;
}

/** One case's evidence, from the round's own cache. A case with no cache stops the run. */
async function evidenceOf(
  round: string,
  caseId: string,
  corpus: Corpus
): Promise<{ documents: ReplayedDocument[]; issueTitle: string | null; neighbours: CaseRow[] }> {
  const path = cachePath(round, caseId);
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    throw new FreshError(
      `${path} could not be read, so the case ${caseId} has no cached evidence: this reads the ` +
        "round's own cache and never a live document"
    );
  }
  // The cache is `run-round.ts`'s own file, so its shape is `CachedEvidence`.
  const cache = JSON.parse(text) as CachedEvidence;
  if (cache.version !== CACHE_VERSION || cache.caseId !== caseId) {
    throw new FreshError(
      `${path} is not the cache of the case ${caseId} at version ${CACHE_VERSION}`
    );
  }
  const neighbours: CaseRow[] = [];
  for (const one of cache.nearest?.neighbours ?? []) {
    const row = corpus.byCaseId.get(one.caseId);
    if (row === undefined) {
      throw new FreshError(
        `${path} names the neighbour ${one.caseId}, which this corpus does not hold`
      );
    }
    neighbours.push(row);
  }
  return { documents: cache.documents, issueTitle: cache.issueTitle ?? null, neighbours };
}

/**
 * The report slice's approval cases, taken from the rows the round wrote and sorted by case id,
 * which is an order a tester drives again from the same round.
 */
async function reportCaseIds(round: string): Promise<string[]> {
  const path = join(round, "rows.jsonl");
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    throw new FreshError(`${path} could not be read: --round takes a directory run-round.ts wrote`);
  }
  const ids = new Set<string>();
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    const row = recordOf(parsed);
    if (row?.slice !== SLICE || row.kind !== KIND) continue;
    const caseId = stringOf(row.caseId);
    if (caseId !== null) ids.add(caseId);
  }
  if (ids.size === 0) throw new FreshError(`${path} names no ${KIND} case of the ${SLICE} slice`);
  return [...ids].sort();
}

/**
 * The two profiles, built under `--out`: one carrying this checkout's `skills/approval-review`
 * and no other skill, one carrying no skill at all. Both carry the models file of the profile
 * this command runs under, which is the provider and the credential, never a skill.
 */
async function profilesOf(out: string, omp: string): Promise<Record<ArmName, string>> {
  const skills = join(out, "skills");
  await mkdir(skills, { recursive: true });
  await cp(
    join(import.meta.dir, "..", "..", "skills", "approval-review"),
    join(skills, "approval-review"),
    {
      recursive: true,
    }
  );
  const models = await modelsFileOf(omp);
  const homes: Record<ArmName, string> = {
    "with-skill": join(out, "home-with-skill"),
    "no-skill": join(out, "home-no-skill"),
  };
  for (const arm of ARMS) {
    const agent = join(homes[arm], ".omp", "profiles", PROFILE, "agent");
    await mkdir(agent, { recursive: true });
    const directories = arm === "with-skill" ? `\n    - ${skills}` : " []";
    await writeFile(
      join(agent, "config.yml"),
      `skills:\n  customDirectories:${directories}\n`,
      "utf8"
    );
    if (models !== null) await copyFile(models, join(agent, "models.yml"));
  }
  return homes;
}

/** The models file of the profile this command runs under, or null where it has none. */
async function modelsFileOf(omp: string): Promise<string | null> {
  const child = Bun.spawn({ cmd: [omp, "config", "path"], stdout: "pipe", stderr: "ignore" });
  const [printed, code] = await Promise.all([new Response(child.stdout).text(), child.exited]);
  if (code !== 0)
    throw new FreshError(`${omp} config path exited ${code}: the agent cannot be run`);
  const path = join(printed.trim(), "models.yml");
  return (await Bun.file(path).exists()) ? path : null;
}

/** The environment one agent run is given: its own home, and no credential of this session's. */
function environmentOf(home: string): Record<string, string> {
  const kept: Record<string, string> = {
    HOME: home,
    PATH: process.env.PATH ?? "/usr/bin:/bin",
  };
  for (const name of ["TMPDIR", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"]) {
    const value = process.env[name];
    if (value !== undefined) kept[name] = value;
  }
  return kept;
}

/** One run of the real agent: its own profile, its own case directory, and one message. */
async function askAgent(omp: string, request: AgentRequest): Promise<AgentAnswer> {
  const startedAt = Date.now();
  const child = Bun.spawn({
    cmd: [
      omp,
      "--profile",
      PROFILE,
      "--cwd",
      request.directory,
      "--model",
      MODEL,
      "--tools",
      "read",
      "--no-context-files",
      "--no-session",
      "--mode",
      "json",
      "-p",
      request.prompt,
    ],
    cwd: request.directory,
    env: environmentOf(request.home),
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: RUN_TIMEOUT_MS,
  });
  const [out, error, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  const latencyMs = Date.now() - startedAt;
  const answer = answerOf(out);
  if (answer.text === "") {
    return {
      text: "",
      model: answer.model,
      latencyMs,
      failure: `the agent exited ${code} with no answer: ${error.trim() || out.trim() || "nothing"}`,
    };
  }
  return { text: answer.text, model: answer.model, latencyMs, failure: null };
}

/** One whole-number flag, at or above `least`. */
function whole(value: string | undefined, flag: string, fallback: number, least: number): number {
  if (value === undefined) return fallback;
  const number = Number(value.trim());
  if (!Number.isSafeInteger(number) || number < least) {
    throw new UsageError(`${flag} takes a whole number of ${least} or above\n\n${USAGE}`);
  }
  return number;
}

/** The shape a `--neighbours` names, or a refusal naming the two it may be. */
function shapeOf(value: string | undefined): Shape {
  if (value === undefined) return DEFAULT_SHAPE;
  const named = value.trim();
  if (named === "with" || named === "without") return named;
  throw new UsageError(`--neighbours takes with or without, and not ${named}\n\n${USAGE}`);
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    round?: string;
    out?: string;
    cases?: string;
    neighbours?: string;
    omp?: string;
    concurrency?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        round: { type: "string" },
        out: { type: "string" },
        cases: { type: "string" },
        neighbours: { type: "string" },
        omp: { type: "string" },
        concurrency: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const round = values.round?.trim();
  const out = values.out?.trim();
  if (!corpus || !round || !out) throw new UsageError(USAGE);
  return {
    corpus,
    round,
    out,
    cases: whole(values.cases, "--cases", DEFAULT_CASES, 1),
    neighbours: shapeOf(values.neighbours),
    omp: values.omp?.trim() || DEFAULT_OMP,
    concurrency: whole(values.concurrency, "--concurrency", DEFAULT_CONCURRENCY, 1),
  };
}

/**
 * Every case of the run, built once: the prompt, the labels, and the directory it is judged in.
 * The `with` shape writes the round's neighbours beside each case; the `without` shape writes
 * no such file, and its prompt names none.
 */
async function casesOf(flags: Flags, out: string, corpus: Corpus): Promise<FreshCase[]> {
  const ids = await reportCaseIds(flags.round);
  if (ids.length < flags.cases) {
    throw new FreshError(
      `--cases asks for ${flags.cases} cases and the ${SLICE} slice holds ${ids.length}`
    );
  }
  const cases: FreshCase[] = [];
  for (const caseId of ids.slice(0, flags.cases)) {
    const row = corpus.byCaseId.get(caseId);
    if (row === undefined) {
      throw new FreshError(`the round names the case ${caseId}, which this corpus does not hold`);
    }
    const evidence = await evidenceOf(flags.round, caseId, corpus);
    const directory = join(
      out,
      "cases",
      flags.neighbours,
      createHash("sha256").update(caseId).digest("hex").slice(0, 16)
    );
    await mkdir(directory, { recursive: true });
    if (flags.neighbours === "with") {
      await writeFile(
        join(directory, "past-decisions.md"),
        pastDecisions(evidence.neighbours),
        "utf8"
      );
    }
    cases.push({
      row,
      prompt: await promptOf(row, evidence.documents, evidence.issueTitle, flags.neighbours),
      labels: labelsOfCase(row),
      directory,
    });
  }
  return cases;
}

/** The shapes `fresh-agent.json` already holds, so a new shape is added and never replaces one. */
async function heldShapes(path: string): Promise<FreshShapes> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return {};
  }
  try {
    // The file is this command's own, so what it holds is what it wrote.
    return JSON.parse(text) as FreshShapes;
  } catch {
    return {};
  }
}

/** The run as the command makes it: the real corpus, the real round and the real agent. */
async function fresh(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const out = await outDirectory(flags.out);
  await mkdir(out, { recursive: true });
  const cases = await casesOf(flags, out, corpus);
  const homes = await profilesOf(out, flags.omp);
  const report = await runFresh(
    { neighbours: flags.neighbours, cases, out, homes, concurrency: flags.concurrency },
    {
      ask: (request) => askAgent(flags.omp, request),
      now: () => Date.now(),
      log: (line) => {
        process.stderr.write(`${line}\n`);
      },
    }
  );
  const path = join(out, "fresh-agent.json");
  const held = await heldShapes(path);
  const shapes: FreshShapes = { ...held, [flags.neighbours]: report };
  await writeFile(path, `${JSON.stringify(shapes, null, 2)}\n`, "utf8");
  console.log(reportLines(report, shapes).join("\n"));
  return report.verdict.passes ? EXIT_PASSES : EXIT_FAILS;
}

async function main(argv: readonly string[]): Promise<number> {
  try {
    return await fresh(readFlags(argv));
  } catch (error) {
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof CorpusError ||
      error instanceof FreshError ||
      error instanceof LeakError
    ) {
      console.error(error.message);
      return EXIT_CANNOT_RUN;
    }
    throw error;
  }
}

// Only when run: a test that imports `runFresh` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
