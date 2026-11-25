import { readFileSync } from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

// The comparison that scripts/e2e/step-models-check.sh runs once it has read each issue's step
// records with `dispatch steps`. It joins each record to the daemon's log lines of the reads the
// record used. Between those reads it counts the responses of the record's agent in the agent's
// own transcript, with the response that holds the step's completion call. The section
// "step-models-check.sh" of scripts/e2e/README.md says what it prints.
//
//   bun scripts/e2e/lib/step-models-check.ts --steps <KEY> <file> [--steps <KEY> <file> …] \
//     --daemon-log <file> --agents <file> [--lower-one]
//
// Each `--steps` file is what `dispatch steps --issue <KEY>` printed, in `--issue` order.

/** The two harnesses an agents file names: Oh My Pi and Claude Code. */
export type Harness = "omp" | "claude";

/** One response of the agent's own conversation, as its transcript holds it. */
export interface Response {
  /** The model's name as a step record writes it, `<provider>/<model>`. */
  model: string;
  /** When the response ended, as the transcript wrote it. */
  time: string;
  /** The same moment, in nanoseconds since the epoch. */
  at: bigint;
  /** False for a response that ended in an error, and for one that no model wrote. */
  counts: boolean;
  /** True where the response holds a completion call: the call with which an agent ends a step. */
  completes: boolean;
}

/**
 * The ops of a completion call. The daemon's end line says `endedBy` `agent` only for the facts
 * these two make (`endedBy` in packages/daemon/internal/workflow/step_models.go).
 */
const COMPLETION_OPS: readonly unknown[] = ["handoff_complete", "sign_off"];

/**
 * Whether a message's content holds a completion call: a block of `type` that calls `tool` with
 * one of the completion ops in its arguments, `field`.
 */
function holdsCompletion(content: unknown, type: string, tool: string, field: string): boolean {
  if (!Array.isArray(content)) return false;
  return content.some((value) => {
    const block = object(value);
    return (
      block !== null &&
      block.type === type &&
      block.name === tool &&
      COMPLETION_OPS.includes(object(block[field])?.op)
    );
  });
}

/** The message of the daemon's log line for each read that a step's start or end used. */
export const READ_LINE = "workflow: a step used a read of its agent's calls";

/** Evidence the comparison cannot read. Each reason prints as `UNKNOWN: <reason>`, exit 2. */
class Unknown extends Error {
  constructor(readonly reasons: string[]) {
    super(reasons.join("\n"));
  }
}

type Json = Record<string, unknown>;

function object(value: unknown): Json | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Json)
    : null;
}

function readText(path: string, what: string): string {
  try {
    return readFileSync(path, "utf8");
  } catch (error) {
    throw new Unknown([`cannot read ${what} ${path}: ${(error as Error).message}`]);
  }
}

const INSTANT = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/;

/** An RFC 3339 time in nanoseconds since the epoch, or null for any other value. */
function instant(value: unknown): bigint | null {
  if (typeof value !== "string") return null;
  const match = INSTANT.exec(value);
  if (match === null) return null;
  const milliseconds = Date.parse(`${match[1]}${match[3]}`);
  if (Number.isNaN(milliseconds)) return null;
  return BigInt(milliseconds) * 1_000_000n + BigInt((match[2] ?? "").padEnd(9, "0"));
}

/** What a transcript holds: its responses in the order of their times, and each model it holds. */
interface Transcript {
  responses: Response[];
  held: Set<string>;
}

/** Each line of a transcript as a JSON object, with its line number. Any other line is unknown. */
function transcriptLines(path: string, what: string): Array<[number, Json]> {
  const lines: Array<[number, Json]> = [];
  const texts = readText(path, what).split("\n");
  for (let index = 0; index < texts.length; index++) {
    if (texts[index].trim() === "") continue;
    let entry: Json | null;
    try {
      entry = object(JSON.parse(texts[index]));
    } catch {
      entry = null;
    }
    if (entry === null) {
      throw new Unknown([`line ${index + 1} of ${what} ${path} is no JSON object`]);
    }
    lines.push([index + 1, entry]);
  }
  return lines;
}

/**
 * An Oh My Pi session file. Each `message` entry of the role `assistant` is a response, named
 * `<provider>/<model>`, at the entry's `timestamp`. It counts unless its stop reason is `error`.
 * It holds a completion call where a `toolCall` block of its content calls `legion`, as the
 * session names the tool of packages/pi-legion, with a completion op in its `arguments`.
 * A `model_change` entry holds its model too. A `model_usage` entry is an upkeep call and holds none.
 */
function ompTranscript(path: string): Transcript {
  const what = "the Oh My Pi session";
  const responses: Response[] = [];
  const held = new Set<string>();
  for (const [line, entry] of transcriptLines(path, what)) {
    if (entry.type === "model_change") {
      if (typeof entry.model !== "string") {
        throw new Unknown([
          `line ${line} of ${what} ${path} is a model change that names no model`,
        ]);
      }
      held.add(entry.model);
      continue;
    }
    const message = object(entry.message);
    if (entry.type !== "message" || message === null || message.role !== "assistant") continue;
    const at = instant(entry.timestamp);
    if (typeof message.provider !== "string" || typeof message.model !== "string" || at === null) {
      throw new Unknown([
        `line ${line} of ${what} ${path} is an assistant message with no provider, model or time`,
      ]);
    }
    const model = `${message.provider}/${message.model}`;
    responses.push({
      model,
      time: String(entry.timestamp),
      at,
      counts: message.stopReason !== "error",
      completes: holdsCompletion(message.content, "toolCall", "legion", "arguments"),
    });
    held.add(model);
  }
  return { responses, held };
}

/**
 * A Claude Code transcript or `stream-json` output. Each assistant line belongs to one response,
 * once by its `message.id`, named `anthropic/<model>`, at the time of its last line. It counts
 * unless its model is `<synthetic>`. It holds a completion call where a `tool_use` block of any of
 * its lines calls `mcp__legion__legion` with a completion op in its `input`: the `legion` tool of
 * packages/claude-envoy under its MCP server `legion`. A `system` `init` line holds its model too.
 * A line whose `parent_tool_use_id` is a string, or whose `isSidechain` is true, is a subagent's
 * and holds none.
 */
function claudeTranscript(path: string): Transcript {
  const what = "the Claude Code transcript";
  const responses = new Map<string, Response>();
  const held = new Set<string>();
  for (const [line, entry] of transcriptLines(path, what)) {
    if (typeof entry.parent_tool_use_id === "string" || entry.isSidechain === true) continue;
    if (entry.type === "system" && entry.subtype === "init") {
      if (typeof entry.model !== "string") {
        throw new Unknown([`line ${line} of ${what} ${path} is an init line that names no model`]);
      }
      held.add(`anthropic/${entry.model}`);
      continue;
    }
    if (entry.type !== "assistant") continue;
    const message = object(entry.message);
    const at = instant(entry.timestamp);
    if (
      message === null ||
      typeof message.id !== "string" ||
      typeof message.model !== "string" ||
      at === null
    ) {
      throw new Unknown([
        `line ${line} of ${what} ${path} is an assistant line with no id, model or time`,
      ]);
    }
    const model = `anthropic/${message.model}`;
    const seen = responses.get(message.id);
    if (seen !== undefined && seen.model !== model) {
      throw new Unknown([
        `line ${line} of ${what} ${path} gives response ${message.id} the model ${model}, and an earlier line gives it ${seen.model}`,
      ]);
    }
    const completes = holdsCompletion(message.content, "tool_use", "mcp__legion__legion", "input");
    responses.set(message.id, {
      model,
      time: String(entry.timestamp),
      at,
      counts: message.model !== "<synthetic>",
      completes: completes || seen?.completes === true,
    });
    held.add(model);
  }
  return { responses: [...responses.values()], held };
}

/** The transcript of one harness, its responses in the order of their times. */
function transcriptOf(path: string, harness: Harness): Transcript {
  const { responses, held } = harness === "omp" ? ompTranscript(path) : claudeTranscript(path);
  responses.sort((a, b) => (a.at < b.at ? -1 : a.at > b.at ? 1 : 0));
  return { responses, held };
}

/** Each response of the agent's own conversation in the transcript at `path`, counted or not. */
export function transcriptResponses(path: string, harness: Harness): Response[] {
  return transcriptOf(path, harness).responses;
}

/** Each model the transcript at `path` holds: each response's, and each model change's or init line's. */
export function heldModels(path: string, harness: Harness): Set<string> {
  return transcriptOf(path, harness).held;
}

/** One model that answered a step, and how many of its calls. */
interface Answer {
  model: string;
  calls: number;
}

/** One run line of `dispatch steps`, with the issue that was read. */
interface StepLine {
  issue: string;
  line: string;
  task: string;
  place: number;
  kind: string;
  attempt: string;
  agent: string;
  /** The model that answered last, or null for `-`. */
  lastModel: string | null;
  startedAt: bigint | null;
  setModel: string | null;
  answered: string;
  /** The counts, or null where the record does not know them (`unknown`, `running`). */
  answers: Answer[] | null;
}

function stepName(step: StepLine): string {
  return `${step.issue} ${step.task} ${step.place} ${step.kind} attempt ${step.attempt}`;
}

const ANSWER = /^(.+?)=([0-9]+)\([0-9]+%\)(,|$)/;

/** The counts that `answered` prints, null where they are not known, undefined for other text. */
function answersOf(text: string): Answer[] | null | undefined {
  if (text === "unknown" || text === "running") return null;
  if (text === "none") return [];
  const answers: Answer[] = [];
  let rest = text;
  while (rest !== "") {
    const match = ANSWER.exec(rest);
    if (match === null || (match[3] === "," && match[0].length === rest.length)) return undefined;
    answers.push({ model: match[1], calls: Number(match[2]) });
    rest = rest.slice(match[0].length);
  }
  return answers;
}

/**
 * One run line of `dispatch steps` (packages/envoy-client/src/dispatch-commands/steps.ts). It has
 * 16 words: `<task> <step> <kind> <role> <type> attempt <a> <agent> <model> <started> <ended>
 * <result> set <set model> answered <answers>`. A line of the release before has 12 words, and is
 * unknown.
 */
function stepLine(issue: string, line: string): StepLine {
  const words = line.trim().split(/\s+/);
  if (words.length !== 16) {
    throw new Unknown([
      `a line of ${issue} has ${words.length} words, not the 16 that name a step's models: ${line}`,
    ]);
  }
  const answers = answersOf(words[15]);
  const startedAt = words[9] === "-" ? null : instant(words[9]);
  if (
    words[5] !== "attempt" ||
    words[12] !== "set" ||
    words[14] !== "answered" ||
    !/^[0-9]+$/.test(words[1]) ||
    answers === undefined ||
    (words[9] !== "-" && startedAt === null)
  ) {
    throw new Unknown([`a line of ${issue} is no run of dispatch steps: ${line}`]);
  }
  return {
    issue,
    line,
    task: words[0],
    place: Number(words[1]),
    kind: words[2],
    attempt: words[6],
    agent: words[7],
    lastModel: words[8] === "-" ? null : words[8],
    startedAt,
    setModel: words[13] === "-" ? null : words[13],
    answered: words[15],
    answers,
  };
}

/** The run lines of one `dispatch steps --issue <issue>` answer, after its header. */
function stepLines(issue: string, path: string): StepLine[] {
  const lines = readText(path, `the steps answer of ${issue}`).split("\n");
  const header = /^(.+): ([0-9]+) steps in [0-9]+ tasks$/.exec(lines[0]);
  if (header === null || header[1] !== issue) {
    throw new Unknown([`the first line of the steps answer is no header of ${issue}: ${lines[0]}`]);
  }
  const steps = lines
    .slice(1)
    .filter((line) => line.trim() !== "")
    .map((line) => stepLine(issue, line));
  if (steps.length !== Number(header[2])) {
    throw new Unknown([
      `the header of ${issue} says ${header[2]} records, and ${steps.length} lines of runs follow it`,
    ]);
  }
  return steps;
}

/** One claim of the agents file: its harness, its transcript and the model its settings give. */
interface Agent {
  harness: Harness;
  transcript: string;
  setModel: string;
}

/** The agents file. A relative transcript path is read from the file's own directory. */
function agentsOf(path: string): Map<string, Agent> {
  const text = readText(path, "the agents file");
  let file: Json | null;
  try {
    file = object(JSON.parse(text));
  } catch {
    file = null;
  }
  if (file === null) throw new Unknown([`the agents file ${path} is no JSON object`]);
  const agents = new Map<string, Agent>();
  for (const [claim, value] of Object.entries(file)) {
    const agent = object(value);
    if (
      agent === null ||
      (agent.harness !== "omp" && agent.harness !== "claude") ||
      typeof agent.transcript !== "string" ||
      agent.transcript === "" ||
      typeof agent.setModel !== "string" ||
      agent.setModel === ""
    ) {
      throw new Unknown([
        `the agents file ${path} does not give ${claim} a harness of omp or claude, a transcript and a setModel`,
      ]);
    }
    agents.set(claim, {
      harness: agent.harness,
      transcript: isAbsolute(agent.transcript)
        ? agent.transcript
        : resolve(dirname(path), agent.transcript),
      setModel: agent.setModel,
    });
  }
  if (agents.size === 0) throw new Unknown([`the agents file ${path} names no claim`]);
  return agents;
}

/** One daemon log line of a read that a step's start or end used. */
interface ReadLine {
  issue: string;
  task: string;
  kind: string;
  place: number;
  startedAt: bigint;
  side: "start" | "end";
  claim: string;
  generation: number;
  final: boolean;
  readAt: bigint;
  readTime: string;
  /** On an end line only: `agent` where the run's own agent ended the step. */
  endedBy: string | null;
}

/**
 * Each read line of the daemon's JSON log, in log order. A line that is no JSON is no read line.
 * A line that holds the read line's message and is no JSON object, such as a cut line, is unknown.
 */
function readLines(path: string): ReadLine[] {
  const reads: ReadLine[] = [];
  const texts = readText(path, "the daemon log").split("\n");
  for (let index = 0; index < texts.length; index++) {
    let entry: Json | null;
    try {
      entry = object(JSON.parse(texts[index]));
    } catch {
      entry = null;
    }
    if (entry === null && texts[index].includes(READ_LINE)) {
      throw new Unknown([
        `line ${index + 1} of the daemon log ${path} holds "${READ_LINE}" and is no JSON object`,
      ]);
    }
    if (entry === null || entry.msg !== READ_LINE) continue;
    const startedAt = instant(entry.startedAt);
    const readAt = instant(entry.readAt);
    const side = entry.side;
    if (
      typeof entry.issue !== "string" ||
      typeof entry.task !== "string" ||
      typeof entry.kind !== "string" ||
      typeof entry.place !== "number" ||
      startedAt === null ||
      (side !== "start" && side !== "end") ||
      typeof entry.claim !== "string" ||
      typeof entry.generation !== "number" ||
      typeof entry.final !== "boolean" ||
      readAt === null ||
      (side === "end" && typeof entry.endedBy !== "string")
    ) {
      throw new Unknown([
        `line ${index + 1} of the daemon log ${path} is a read line without the keys this check reads`,
      ]);
    }
    reads.push({
      issue: entry.issue,
      task: entry.task,
      kind: entry.kind,
      place: entry.place,
      startedAt,
      side,
      claim: entry.claim,
      generation: entry.generation,
      final: entry.final,
      readAt,
      readTime: String(entry.readAt),
      endedBy: side === "end" ? String(entry.endedBy) : null,
    });
  }
  if (reads.length === 0) {
    throw new Unknown([`the daemon log ${path} holds no line "${READ_LINE}"`]);
  }
  return reads;
}

/** `dispatch steps` prints a start with six fraction digits at most, and the log keeps nanoseconds. */
const SAME_START = 1_000n;

/**
 * The read lines of a record: those of its issue, task, place, kind and start. Two starts are one
 * where they differ by less than 1 µs.
 */
function joinedReads(step: StepLine, reads: ReadLine[]): { starts: ReadLine[]; ends: ReadLine[] } {
  const { startedAt } = step;
  const joined =
    startedAt === null
      ? []
      : reads.filter(
          (read) =>
            read.issue === step.issue &&
            read.task === step.task &&
            read.place === step.place &&
            read.kind === step.kind &&
            read.startedAt - startedAt > -SAME_START &&
            read.startedAt - startedAt < SAME_START
        );
  return {
    starts: joined.filter((read) => read.side === "start"),
    ends: joined.filter((read) => read.side === "end"),
  };
}

/** A read is within 2 s of a response where either of its gaps is under 2 s. */
const NEAR = 2_000_000_000n;

/**
 * The nanoseconds since the last response that ended at or before `at`, and to the next one. Each
 * leaves out the response `leftOut`, where one is given.
 */
function gapsOf(
  at: bigint,
  responses: Response[],
  leftOut: Response | null
): { before: bigint | null; after: bigint | null } {
  let before: bigint | null = null;
  let after: bigint | null = null;
  for (const response of responses) {
    if (response === leftOut) continue;
    if (response.at <= at) before = at - response.at;
    else if (after === null) after = response.at - at;
  }
  return { before, after };
}

function seconds(nanoseconds: bigint): string {
  return (Number(nanoseconds) / 1e9).toFixed(3);
}

/** The two reads a comparison can take a step on. */
interface Window {
  start: ReadLine;
  end: ReadLine;
  /** On a step that its agent ended, the response that holds its completion call, else null. */
  completion: Response | null;
}

/** A record of a claim the agents file names: its reads, and why a comparison cannot take it. */
interface Judged {
  step: StepLine;
  agent: Agent;
  transcript: Transcript;
  ends: ReadLine[];
  window: Window | null;
  /** Each read with its gaps, or why it is missing or final, for the record's `GAP:` line. */
  gaps: string;
  why: string;
}

function readGaps(
  side: string,
  reads: ReadLine[],
  responses: Response[],
  leftOut: Response | null
): string {
  if (reads.length === 0) return `no ${side} read`;
  if (reads.length > 1) return `${reads.length} ${side} reads`;
  const [read] = reads;
  if (read.final) return `${side} read ${read.readTime}: final totals`;
  const { before, after } = gapsOf(read.readAt, responses, leftOut);
  const last =
    before === null
      ? "no response ended before it"
      : `the last response ended ${seconds(before)} s before it`;
  const next = after === null ? "none after it" : `the next ${seconds(after)} s after it`;
  return `${side} read ${read.readTime}: ${last}, ${next}`;
}

/**
 * The response that holds the step's completion call. It is found by that call, never by how near
 * its time lies to a read: the last such response after the start read and up to the end read.
 * Where there is none, it is the next response after the end read, where that one holds the call,
 * since the clock of the agent's machine may run ahead of the daemon's. Null where neither holds
 * it.
 */
function completionOf(responses: Response[], start: ReadLine, end: ReadLine): Response | null {
  let found: Response | null = null;
  for (const response of responses) {
    if (response.at <= start.readAt) continue;
    if (response.at > end.readAt) return found ?? (response.completes ? response : null);
    if (response.completes) found = response;
  }
  return found;
}

/**
 * Why no comparison can take the step on its reads, or "" where one can. On a step that its agent
 * ended, with one start read and one end read of one launch and neither final, it also gives the
 * response that holds the completion call, or null. The 2 s test of the end read leaves that
 * response out, and every other response keeps the 2 s test at both reads. Where no response holds
 * the call, the end read leaves out nothing.
 */
function whyNot(
  starts: ReadLine[],
  ends: ReadLine[],
  responses: Response[]
): { why: string; completion?: Response | null } {
  if (starts.length !== 1) return { why: `it has ${starts.length} start reads, not one` };
  if (ends.length !== 1) return { why: `it has ${ends.length} end reads, not one` };
  const [start] = starts;
  const [end] = ends;
  if (start.final) return { why: "its start read is final totals" };
  if (end.final) return { why: "its end read is final totals" };
  if (start.claim !== end.claim || start.generation !== end.generation) {
    return { why: "its two reads name two launches" };
  }
  const completion = end.endedBy === "agent" ? completionOf(responses, start, end) : undefined;
  const leftOut = completion ?? null;
  for (const [read, without] of [
    [start, null],
    [end, leftOut],
  ] as const) {
    const { before, after } = gapsOf(read.readAt, responses, without);
    if ((before !== null && before < NEAR) || (after !== null && after < NEAR)) {
      return { why: `a response ended within 2 s of its ${read.side} read`, completion };
    }
  }
  return { why: "", completion };
}

/**
 * What a step's `GAP:` line says of the completion's response, where `whyNot` looked for one: then
 * `ends` holds the one end read.
 */
function leftOutText(completion: Response | null | undefined, ends: ReadLine[]): string {
  if (completion === undefined) return "";
  if (completion === null) {
    return " No response holds the completion call, so the end read leaves out none.";
  }
  const [end] = ends;
  const [gap, side] =
    completion.at <= end.readAt
      ? [end.readAt - completion.at, "before"]
      : [completion.at - end.readAt, "after"];
  return ` The end read leaves out the response of ${completion.time}, which holds the completion call and ended ${seconds(gap)} s ${side} it.`;
}

function countsText(answers: Answer[]): string {
  return answers.length === 0
    ? "none"
    : answers.map((answer) => `${answer.model}=${answer.calls}`).join(",");
}

function totals(answers: Answer[]): Map<string, number> {
  const by = new Map<string, number>();
  for (const answer of answers) by.set(answer.model, (by.get(answer.model) ?? 0) + answer.calls);
  return by;
}

/**
 * The transcript's counted responses by model: those after the start read and up to the end read,
 * and the completion's response wherever its time lies.
 */
function countedBetween(responses: Response[], window: Window): Answer[] {
  const counted: Answer[] = [];
  for (const response of responses) {
    const between = response.at > window.start.readAt && response.at <= window.end.readAt;
    if (!response.counts || !(between || response === window.completion)) continue;
    const answer = counted.find((entry) => entry.model === response.model);
    if (answer === undefined) counted.push({ model: response.model, calls: 1 });
    else answer.calls++;
  }
  return counted;
}

function sameCounts(a: Answer[], b: Answer[]): boolean {
  const left = totals(a);
  const right = totals(b);
  return (
    left.size === right.size && [...left].every(([model, calls]) => right.get(model) === calls)
  );
}

type Verdict = "PASS" | "FAIL" | "UNKNOWN";

/** The four things a taken step must show, on the counts `--lower-one` may have lowered. */
function judge(comparison: string, taken: Judged, window: Window, lowerOne: boolean): string[] {
  const { step, agent } = taken;
  const faults: string[] = [];
  if (step.setModel !== agent.setModel) {
    faults.push(
      `its set model is ${step.setModel ?? "-"}, and the settings give its role ${agent.setModel}`
    );
  }
  let answers = step.answers;
  if (answers !== null && lowerOne && answers.length > 0) {
    answers = [{ ...answers[0], calls: answers[0].calls - 1 }, ...answers.slice(1)];
  }
  if (answers === null) {
    faults.push(`its counts are ${step.answered}, not known`);
  } else {
    const total = answers.reduce((sum, answer) => sum + answer.calls, 0);
    if (total < 1) faults.push(`its counts total ${total}`);
    const counted = countedBetween(taken.transcript.responses, window);
    if (!sameCounts(answers, counted)) {
      faults.push(
        `its counts are ${countsText(answers)}, and its transcript holds ${countsText(counted)} between the two reads`
      );
    }
  }
  // Each line of a taken step names its harness, so a pass shows each harness it covered.
  const head = `${comparison}: harness ${agent.harness}: ${stepName(step)}`;
  if (faults.length > 0) return faults.map((fault) => `FAIL: ${head}: ${fault}`);
  return [`PASS: ${head}: ${countsText(answers ?? [])}`];
}

/**
 * One comparison. For each harness, it takes the first step that `takes` accepts on its end read,
 * in `--issue` order and then in printed order. It holds that step to the four things. A harness
 * with no such step is UNKNOWN.
 */
function compare(
  comparison: string,
  harnesses: Harness[],
  judged: Judged[],
  takes: (end: ReadLine) => boolean,
  lowerOne: boolean
): { lines: string[]; verdict: Verdict } {
  const lines: string[] = [];
  let unknown = false;
  for (const harness of harnesses) {
    const taken = judged.find(
      (entry) => entry.agent.harness === harness && entry.window !== null && takes(entry.window.end)
    );
    if (taken === undefined || taken.window === null) {
      lines.push(`${comparison}: UNKNOWN: took no step of harness ${harness}`);
      unknown = true;
      continue;
    }
    lines.push(...judge(comparison, taken, taken.window, lowerOne));
  }
  const failed = lines.some((line) => line.startsWith("FAIL: "));
  return { lines, verdict: failed ? "FAIL" : unknown ? "UNKNOWN" : "PASS" };
}

/** No record may name a model that its agent's transcript does not hold: set, answering or last. */
function unheldModels(judged: Judged[]): string[] {
  const lines: string[] = [];
  for (const { step, transcript } of judged) {
    const named: Array<[string, string | null]> = [
      ["set", step.setModel],
      ...(step.answers ?? []).map((answer): [string, string] => ["answering", answer.model]),
      ["last", step.lastModel],
    ];
    for (const [which, model] of named) {
      if (model === null || transcript.held.has(model)) continue;
      lines.push(
        `FAIL: c-real: ${stepName(step)}: its ${which} model ${model} is held by no entry of the transcript of ${step.agent}`
      );
    }
  }
  return lines;
}

interface Inputs {
  steps: Array<[string, string]>;
  daemonLog: string;
  agents: string;
  lowerOne: boolean;
}

function inputsOf(argv: string[]): Inputs {
  const inputs: Inputs = { steps: [], daemonLog: "", agents: "", lowerOne: false };
  for (let index = 0; index < argv.length; index++) {
    const flag = argv[index];
    if (flag === "--steps" && index + 2 < argv.length) {
      inputs.steps.push([argv[index + 1], argv[index + 2]]);
      index += 2;
    } else if (flag === "--daemon-log" && index + 1 < argv.length) {
      inputs.daemonLog = argv[++index];
    } else if (flag === "--agents" && index + 1 < argv.length) {
      inputs.agents = argv[++index];
    } else if (flag === "--lower-one") {
      inputs.lowerOne = true;
    } else {
      throw new Unknown([`lib/step-models-check.ts takes no argument ${flag}`]);
    }
  }
  if (inputs.steps.length === 0 || inputs.daemonLog === "" || inputs.agents === "") {
    throw new Unknown(["lib/step-models-check.ts needs a --steps, a --daemon-log and an --agents"]);
  }
  return inputs;
}

/** The lines the comparison prints, the two verdicts last, and its exit: the `c-real` verdict's. */
function stepModelsCheck(argv: string[]): { lines: string[]; code: number } {
  const inputs = inputsOf(argv);
  const steps = inputs.steps.flatMap(([issue, path]) => stepLines(issue, path));
  const reads = readLines(inputs.daemonLog);
  const agents = agentsOf(inputs.agents);
  const missing = [
    ...new Set(
      steps
        .filter(
          (step) =>
            step.setModel !== null || step.lastModel !== null || (step.answers ?? []).length > 0
        )
        .flatMap((step) => (agents.has(step.agent) ? [] : [step.agent]))
    ),
  ];
  if (missing.length > 0) throw new Unknown(missing.map((claim) => `no transcript of ${claim}`));

  const transcripts = new Map<string, Transcript>();
  const judged: Judged[] = [];
  for (const step of steps) {
    const agent = agents.get(step.agent);
    if (agent === undefined) continue;
    let transcript = transcripts.get(step.agent);
    if (transcript === undefined) {
      transcript = transcriptOf(agent.transcript, agent.harness);
      transcripts.set(step.agent, transcript);
    }
    const { starts, ends } = joinedReads(step, reads);
    const { responses } = transcript;
    const { why, completion } = whyNot(starts, ends, responses);
    const leftOut = completion ?? null;
    const startGaps = readGaps("start", starts, responses, null);
    const endGaps = readGaps("end", ends, responses, leftOut);
    judged.push({
      step,
      agent,
      transcript,
      ends,
      window: why === "" ? { start: starts[0], end: ends[0], completion: leftOut } : null,
      gaps: `${startGaps}. ${endGaps}.${leftOutText(completion, ends)}`,
      why,
    });
  }

  const harnesses = [...new Set([...agents.values()].map((agent) => agent.harness))];
  const lines: string[] = [];
  for (const harness of harnesses) {
    const ended = judged.filter(
      (entry) =>
        entry.agent.harness === harness && entry.ends.some((end) => end.endedBy === "agent")
    );
    for (const entry of ended) {
      const taken = entry.why === "" ? "" : ` Not taken: ${entry.why}.`;
      lines.push(`GAP: ${stepName(entry.step)} (${harness}): ${entry.gaps}${taken}`);
    }
    if (ended.length === 0) lines.push(`GAP: harness ${harness}: saw no step that its agent ended`);
  }
  const real = compare(
    "c-real",
    harnesses,
    judged,
    (end) => end.endedBy === "agent",
    inputs.lowerOne
  );
  const unheld = unheldModels(judged);
  const live = compare(
    "live-at-both-reads",
    harnesses,
    judged,
    (end) => end.endedBy !== "agent",
    inputs.lowerOne
  );
  const realVerdict: Verdict = unheld.length > 0 ? "FAIL" : real.verdict;
  lines.push(...real.lines, ...unheld, ...live.lines);
  lines.push(`c-real: ${realVerdict}`, `live-at-both-reads: ${live.verdict}`);
  return { lines, code: realVerdict === "PASS" ? 0 : realVerdict === "FAIL" ? 1 : 2 };
}

if (import.meta.main) {
  let result: { lines: string[]; code: number };
  try {
    result = stepModelsCheck(process.argv.slice(2));
  } catch (error) {
    const reasons =
      error instanceof Unknown ? error.reasons : [`the comparison stopped: ${String(error)}`];
    result = {
      lines: [
        ...reasons.map((reason) => `UNKNOWN: ${reason}`),
        "c-real: UNKNOWN",
        "live-at-both-reads: UNKNOWN",
      ],
      code: 2,
    };
  }
  process.stdout.write(`${result.lines.join("\n")}\n`);
  process.exitCode = result.code;
}
