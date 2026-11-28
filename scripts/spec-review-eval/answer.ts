/**
 * The reviewer's answer in the line form of skills/spec-statement-review/SKILL.md, what its
 * transcript holds, and its comparison with the expectation of a fixture.
 */
import { recordOf, stringOf } from "../approval-eval/corpus-io";

/** The kinds of the skill's output form, in the order the skill prints them. */
export const KINDS = [
  "principle",
  "undefined-noun",
  "other-meaning",
  "repeated-meaning",
  "wide-claim",
  "agent-choice",
  "agent-where-program",
  "form-limit",
] as const;

export type Kind = (typeof KINDS)[number];

export function isKind(value: string): value is Kind {
  return (KINDS as readonly string[]).includes(value);
}

/** One `FINDING` line. `subject` is null where the line writes `-`. */
export interface Finding {
  readonly kind: Kind;
  readonly node: string;
  readonly subject: string | null;
}

/**
 * What an answer says. `not-reviewed` is a `NOT REVIEWED:` line, which says that a command failed
 * and no review was made. `unreadable` is an answer out of the skill's form. Neither is a verdict.
 */
export type Answer =
  | { readonly verdict: "findings"; readonly findings: readonly Finding[] }
  | { readonly verdict: "none" }
  | { readonly verdict: "not-reviewed"; readonly reason: string }
  | { readonly verdict: "unreadable"; readonly reason: string };

/** An item of expected.json. A field it leaves out matches any value. */
export interface Item {
  readonly kind?: Kind;
  readonly node: string;
  readonly subject?: string;
}

/** A planted item names its kind. */
export interface PlantedItem extends Item {
  readonly kind: Kind;
}

/** What a fixture expects of an answer: each planted item named, and no item of mustNotName. */
export interface Expectation {
  readonly planted: readonly PlantedItem[];
  readonly mustNotName: readonly Item[];
}

/** What compareFindings decides about one answer. */
export interface Comparison {
  readonly pass: boolean;
  readonly problems: readonly string[];
}

const FINDING_LINE = /^FINDING (\S+) (\S+) (.+?): .+$/;
const NOT_REVIEWED = "NOT REVIEWED:";
/** A verdict's word, which no line holds after its own opening: `FINDING`, `NO FINDING` or `NOT REVIEWED`. */
const VERDICT_WORD = /\b(?:FINDING|NOT REVIEWED)\b/;

/**
 * Reads an answer in the skill's output form. The skill prints a `NOT REVIEWED:` line "and stop",
 * so that line beside any other line is unreadable, and alone it is no verdict. The skill says "no
 * other text", so a line out of the form, and `NO FINDING` beside a finding, make the answer
 * unreadable, as an answer with neither a `FINDING` nor a `NO FINDING` line does. A verdict's word
 * after the opening of a line is a second verdict on that line, which makes it unreadable too.
 */
export function parseFindings(text: string): Answer {
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  const notReviewed = lines.find((line) => line.startsWith(NOT_REVIEWED));
  if (notReviewed !== undefined) {
    const reason = notReviewed.slice(NOT_REVIEWED.length).trim();
    if (lines.length > 1) {
      return {
        verdict: "unreadable",
        reason: `a NOT REVIEWED line beside another line: ${notReviewed}`,
      };
    }
    return VERDICT_WORD.test(reason)
      ? {
          verdict: "unreadable",
          reason: `a second verdict on the NOT REVIEWED line: ${notReviewed}`,
        }
      : { verdict: "not-reviewed", reason };
  }
  const findings: Finding[] = [];
  let none = false;
  for (const line of lines) {
    if (line === "NO FINDING") {
      none = true;
      continue;
    }
    const match = FINDING_LINE.exec(line);
    const [, kind = "", node = "", subject = ""] = match ?? [];
    if (match === null || !isKind(kind)) {
      return { verdict: "unreadable", reason: `a line out of the skill's form: ${line}` };
    }
    if (VERDICT_WORD.test(line.slice("FINDING".length))) {
      return { verdict: "unreadable", reason: `a second verdict on a FINDING line: ${line}` };
    }
    findings.push({ kind, node, subject: subject === "-" ? null : subject });
  }
  if (none && findings.length > 0) {
    return { verdict: "unreadable", reason: "NO FINDING beside a FINDING line" };
  }
  if (none) return { verdict: "none" };
  if (findings.length === 0) {
    return { verdict: "unreadable", reason: "no FINDING line and no NO FINDING line" };
  }
  return { verdict: "findings", findings };
}

/** A subject as it compares: no code quotes or quotation marks, one space, lower case. */
export function normal(subject: string): string {
  return subject
    .replace(/[`"'“”‘’]/g, "")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase();
}

function matches(finding: Finding, item: Item): boolean {
  return (
    finding.node === item.node &&
    (item.kind === undefined || finding.kind === item.kind) &&
    (item.subject === undefined ||
      (finding.subject !== null && normal(finding.subject) === normal(item.subject)))
  );
}

export function label(node: string, subject: string | null | undefined): string {
  return subject === null || subject === undefined ? node : `${node} ${subject}`;
}

/**
 * Judges one answer against its fixture's expectation. The answer passes when it names each
 * planted item and nothing else. An answer that is no verdict never passes.
 */
export function compareFindings(expectation: Expectation, answer: Answer): Comparison {
  if (answer.verdict === "not-reviewed") {
    return { pass: false, problems: [`gave no verdict: ${answer.reason}`] };
  }
  if (answer.verdict === "unreadable") {
    return { pass: false, problems: [`gave an unreadable answer: ${answer.reason}`] };
  }
  const named = answer.verdict === "none" ? [] : answer.findings;
  const problems: string[] = [];
  for (const item of expectation.planted) {
    if (!named.some((finding) => matches(finding, item))) {
      problems.push(`missed the planted ${item.kind} ${label(item.node, item.subject)}`);
    }
  }
  for (const finding of named) {
    if (expectation.planted.some((item) => matches(finding, item))) continue;
    const where = label(finding.node, finding.subject);
    problems.push(
      expectation.mustNotName.some((item) => matches(finding, item))
        ? `named ${where}, which it must not name (${finding.kind})`
        : `named ${finding.kind} ${where}, which is not planted`
    );
  }
  return { pass: problems.length === 0, problems };
}

/** One tool event of the agent's JSON event stream: a call with its arguments, or its result. */
interface ToolEvent {
  readonly type: "tool_execution_start" | "tool_execution_end";
  readonly toolName: string | null;
  readonly record: Record<string, unknown>;
}

function toolEventsOf(stdout: string): ToolEvent[] {
  const events: ToolEvent[] = [];
  for (const line of stdout.split("\n")) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    const record = recordOf(parsed);
    const type = record?.type;
    if (record === null || (type !== "tool_execution_start" && type !== "tool_execution_end")) {
      continue;
    }
    events.push({ type, toolName: stringOf(record.toolName), record });
  }
  return events;
}

/**
 * Whether the agent's JSON event stream holds a bash call of `dispatch spec-form` whose own result
 * holds `heading`, the line that the client prints before the texts that Dispatch read in the
 * fixture's spec (`textsHeading`): a call that echoes the words, or that failed, holds no such
 * line, so this finds the effect of the call and not its words.
 */
export function specFormAnswered(stdout: string, heading: string): boolean {
  const events = toolEventsOf(stdout);
  return events.some(({ type, toolName, record }) => {
    const command = stringOf(recordOf(record.args)?.command);
    if (
      type !== "tool_execution_start" ||
      toolName !== "bash" ||
      command === null ||
      !/\bdispatch\s+spec-form\b/.test(command)
    ) {
      return false;
    }
    const call = stringOf(record.toolCallId);
    return events.some(
      (result) =>
        result.type === "tool_execution_end" &&
        call !== null &&
        stringOf(result.record.toolCallId) === call &&
        textOfResult(result.record.result).split("\n").includes(heading)
    );
  });
}

/** The text blocks of a tool result, joined as the tool printed them. */
function textOfResult(result: unknown): string {
  const content = recordOf(result)?.content;
  return (Array.isArray(content) ? content : [])
    .map((block) => stringOf(recordOf(block)?.text) ?? "")
    .join("\n");
}

/**
 * The names of this command's own files that hold the expected answers: its directory and
 * expected.json. The agent's sandbox holds neither, and no file the agent's own `dispatch` writes
 * has either name, so a tool call or result that holds one reached a copy outside the sandbox,
 * such as one over the network, which the sandbox does not cut (agent.ts). This is a match of
 * names: a command that reads such a copy and never prints a name passes it.
 */
export const EVALUATION_NAMES = ["spec-review-eval", "expected.json"] as const;

/**
 * Each tool call or result of the agent's JSON event stream that names one of this command's own
 * files (EVALUATION_NAMES), as `<tool> <call or result> names <name>`: a run that read one is no
 * measure of the skill.
 */
export function evaluationNamed(stdout: string): string[] {
  const named: string[] = [];
  for (const { type, toolName, record } of toolEventsOf(stdout)) {
    const part = type === "tool_execution_start" ? record.args : record.result;
    const text = JSON.stringify(part ?? null);
    for (const name of EVALUATION_NAMES) {
      if (text.includes(name)) {
        named.push(
          `${toolName ?? "a tool"} ${type === "tool_execution_start" ? "call" : "result"} names ${name}`
        );
      }
    }
  }
  return named;
}
