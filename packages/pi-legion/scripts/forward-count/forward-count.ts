#!/usr/bin/env bun
// The count of returns from the retro and the merge (C7 of the forward-only design,
// dispatch://INFRA-5581), in two phases:
//
//   bun forward-count.ts collect --dispatch-url <url> --token-file <file> --github-api-url <url> \
//     --github-token-file <file> --projects <K1,K2,…> --from <YYYY-MM-DD> --to <YYYY-MM-DD> \
//     --transcripts tq|dir:<path>|none --reasons all|none --out <dir>
//   bun forward-count.ts resolve --out <dir> --readings <readings.jsonl>
//
// A return is a step record of kind `retro` or `merge` whose result is `failed`, for each task.
// `collect` reads Dispatch for the returns and their reasons, GitHub for the pull request's
// timeline, and the role's own transcript, and writes one line per return to `<out>/returns.jsonl`
// with the state of each stated cause's record. `resolve` adds the readers' readings and writes
// `<out>/resolved.jsonl`, `<out>/summary.txt` for the batch and `<out>/summary-<PROJECT>.txt` for
// each project.
//
// Two rules decide what the count may believe about a block by GitHub, and they are the reason
// this program is longer than a word count of reasons would be:
//
//   * an answer from GitHub is known by its channel, never by its content. Only a timeline item
//     this program read itself, and the result of a tool call that is a GitHub read in the role's
//     transcript, are answers. Text the role wrote is never one, and no delivery to the role is
//     one, whoever sent it: the store keeps a delivery's text and drops its separate fields, so a
//     rule that read the rendered `from:` line would judge a message by its words again.
//   * the classification of `merge-blocked`'s record runs in order, and the first step that gives
//     a state decides (classifyMergeBlocked). Read as unordered tests, the case of a last `clear`
//     answer with an unread timeline and no timeline item obtained would give `absent` where the
//     ordered steps give `unread`.
//
// Neither `collect` nor `resolve` writes anything outside `--out`, and neither calls any route
// that writes.
import { readFileSync, readdirSync, writeFileSync, mkdirSync, existsSync } from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";

// ---------------------------------------------------------------------------
// The words and the shapes
// ---------------------------------------------------------------------------

/** The stated causes of C7's table, in the order a summary and a class name them. */
export const CAUSES = ["merge-blocked", "ci", "failed-check", "news", "goal-check"] as const;
export type StatedCause = (typeof CAUSES)[number];

/** The state of the record that would establish one stated cause. */
export type RecordState = "established" | "absent" | "undetermined" | "unread";

/** The value of one answer about a head: what it says of the merge. */
export type AnswerValue = "blocked" | "clear" | "indeterminate";

/** One answer that counts: where it came from, when, what it says, and the head it is about. */
export interface Answer {
  source: "transcript" | "timeline";
  /** The call or the timeline item the answer came from, as the return's line prints it. */
  place: string;
  /** The answer's own time, null where it cannot be read. */
  at: string | null;
  value: AnswerValue;
  /** The head the answer names, null where it names none. */
  head: string | null;
}

/** Whether a source was read, and why it was not. */
export interface SourceRead {
  state: "read" | "unread";
  why?: string;
}

/** The two sources of one return, as `merge-blocked`'s classification reads them. */
export interface MergeBlockedRead {
  /** The head the role read, null where it cannot be determined. */
  head: string | null;
  /** The transcript's answers, in transcript order, up to the role's `request_backward_move`. */
  transcript: { state: "read" | "unread"; answers: Answer[]; why?: string };
  /** The timeline's items that stand before the return. */
  timeline: { state: "read" | "unread"; items: Answer[]; why?: string };
}

/** One stated cause with the state of its record, and the step that gave it for `merge-blocked`. */
export interface CauseRecord {
  cause: StatedCause;
  state: RecordState;
  step: number | null;
}

/** One check of the issue's Verification document, as the results read answers it at the head. */
export interface CheckRead {
  blockId: string;
  /** The name of the check run a CI job writes, null for every other check. */
  ciCheck: string | null;
  passStands: "yes" | "no" | "unknown";
}

/** Everything one return's records are read from. A null read is a read that did not answer. */
export interface ReturnFacts {
  kind: "retro" | "merge";
  /** The return's own time: the end of the run that failed. */
  at: string;
  /** The start of that run, null where the record holds none. */
  stepStartedAt: string | null;
  head: string | null;
  reason: string | null;
  reasonRead: boolean;
  checks: CheckRead[] | null;
  /** Each approval of a new version of the issue's spec or Verification document, by its time. */
  approvals: { at: string }[] | null;
  /** The step records of the return's own task. */
  taskSteps: { role: string; kind: string; type: string }[];
  requirementIds: string[] | null;
  mergeBlocked: MergeBlockedRead;
}

/** One return, as `collect` writes it to returns.jsonl. */
export interface ReturnLine {
  /** `<issue>/<task>/<kind>/<attempt>`, the name a reading names the return by. */
  id: string;
  project: string;
  issue: string;
  task: string;
  kind: "retro" | "merge";
  attempt: number;
  role: string;
  session: string;
  at: string;
  day: string;
  head: string | null;
  reason: string | null;
  reasonRead: boolean;
  /** The sources that hold the return itself: Dispatch's step records, the transcript store, or both. */
  heldBy: ("steps" | "transcript")[];
  sources: { transcript: "read" | "unread"; timeline: "read" | "unread" };
  causes: CauseRecord[];
  answers: Answer[];
}

/** One reading of one return, as readings.jsonl holds it. */
export interface Reading {
  return: string;
  reader: "1" | "2" | "resolver";
  model: string;
  /** Whether the role's own defect caused the return, alone or with another cause. */
  own: boolean;
  /** For each stated cause, whether it caused this return. */
  confirmed: Record<string, boolean>;
}

/** The class a return resolves to. */
export type ResolvedClass =
  | "against-R23"
  | "mixed"
  | "goal-check"
  | `exempt:${StatedCause}`
  | "unexplained"
  | "pending";

/** One resolved return, as resolve.jsonl holds it. */
export interface ResolvedLine extends ReturnLine {
  readings: Reading[];
  final: Reading | null;
  class: ResolvedClass;
  why: string;
}

// ---------------------------------------------------------------------------
// The classification of merge-blocked's record
// ---------------------------------------------------------------------------

/**
 * The state of `merge-blocked`'s record, and the step of C7 that gave it. The steps run in order
 * and the first that gives a state decides, so each case gets exactly one state. The order
 * matters: step 5 answers `unread` for a last `clear` answer with an unread timeline, which an
 * unordered reading would answer `absent` at step 7.
 */
export function classifyMergeBlocked(read: MergeBlockedRead): { state: RecordState; step: number } {
  if (read.head === null || read.head === "") return { state: "unread", step: 1 };
  if (read.transcript.state === "unread") return { state: "unread", step: 2 };
  const about = (answer: Answer) => answer.head === read.head;
  const eligible = read.transcript.answers.filter(
    (answer) => about(answer) && (answer.value === "blocked" || answer.value === "clear")
  );
  const last = eligible.at(-1);
  if (last !== undefined && last.value === "blocked") return { state: "established", step: 3 };
  const counted = read.timeline.items.filter(about);
  if (last === undefined) {
    if (read.timeline.state === "unread") return { state: "unread", step: 4 };
    return counted.length > 0 ? { state: "established", step: 4 } : { state: "undetermined", step: 4 };
  }
  if (read.timeline.state === "unread") return { state: "unread", step: 5 };
  const lastAt = timeOf(last.at);
  const after = counted.filter((item) => {
    const at = timeOf(item.at);
    return lastAt !== null && at !== null && at > lastAt;
  });
  if (after.length > 0) return { state: "established", step: 6 };
  const unordered = counted.filter((item) => {
    const at = timeOf(item.at);
    return lastAt === null || at === null || at === lastAt;
  });
  if (unordered.length === 0) return { state: "absent", step: 7 };
  return { state: "undetermined", step: 8 };
}

/** The milliseconds of a time, null where it cannot be read. */
function timeOf(at: string | null | undefined): number | null {
  if (at === null || at === undefined || at === "") return null;
  const parsed = Date.parse(at);
  return Number.isNaN(parsed) ? null : parsed;
}

// ---------------------------------------------------------------------------
// The transcript: what a session holds, and which of it is an answer
// ---------------------------------------------------------------------------

/** One entry of a session, in the form the transcript store imports. */
export interface TranscriptEntry {
  index: number;
  at: string | null;
  kind: "call" | "result" | "assistant" | "delivery" | "user" | "system";
  tool?: string;
  callId?: string;
  /** A call's command, or the text of its arguments for a tool that takes no command. */
  command?: string;
  args?: Record<string, unknown>;
  text?: string;
  isError?: boolean;
}

/** The text of a `content` array of parts, as the store's parser reads one. */
function partsText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => (isObject(part) && typeof part.text === "string" ? part.text : ""))
    .join("");
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * One session file, parsed: the session the header names, and every entry, in order. The form is
 * the one the transcript store imports (`transcript_store/parsers.py`'s OmpParser): a `session`
 * header, `message` entries whose role is assistant, toolResult, user, developer or system, and
 * `custom_message` entries, which are deliveries.
 */
export function parseSession(text: string): { session: string | null; entries: TranscriptEntry[] } {
  let session: string | null = null;
  const entries: TranscriptEntry[] = [];
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(parsed)) continue;
    const at = typeof parsed.timestamp === "string" ? parsed.timestamp : null;
    const add = (entry: Omit<TranscriptEntry, "index">) => entries.push({ ...entry, index: entries.length });
    if (parsed.type === "session") {
      if (typeof parsed.id === "string") session = parsed.id;
      continue;
    }
    if (parsed.type === "custom_message") {
      add({ at, kind: "delivery", text: partsText(parsed.content) });
      continue;
    }
    if (parsed.type === "compaction") {
      add({ at, kind: "system", text: typeof parsed.summary === "string" ? parsed.summary : "" });
      continue;
    }
    if (parsed.type !== "message" || !isObject(parsed.message)) continue;
    const message = parsed.message;
    const role = typeof message.role === "string" ? message.role : "";
    if (role === "assistant") {
      const content = Array.isArray(message.content) ? message.content : [];
      for (const part of content) {
        if (!isObject(part)) continue;
        if (part.type === "toolCall") {
          const args = isObject(part.arguments) ? part.arguments : {};
          add({
            at,
            kind: "call",
            tool: typeof part.name === "string" ? part.name : "",
            callId: typeof part.id === "string" ? part.id : "",
            args,
            command: commandOf(args),
          });
        } else if (part.type === "text" && typeof part.text === "string") {
          add({ at, kind: "assistant", text: part.text });
        }
      }
      continue;
    }
    if (role === "toolResult") {
      add({
        at,
        kind: "result",
        tool: typeof message.toolName === "string" ? message.toolName : "",
        callId: typeof message.toolCallId === "string" ? message.toolCallId : "",
        text: partsText(message.content),
        isError: message.isError === true,
      });
      continue;
    }
    add({ at, kind: role === "user" ? "user" : "system", text: partsText(message.content) });
  }
  return { session, entries };
}

/** A call's command: its `command` argument, else every string of its arguments, joined. */
function commandOf(args: Record<string, unknown>): string {
  if (typeof args.command === "string") return args.command;
  const strings: string[] = [];
  const walk = (value: unknown, depth: number) => {
    if (depth > 4) return;
    if (typeof value === "string") strings.push(value);
    else if (Array.isArray(value)) for (const item of value) walk(item, depth + 1);
    else if (isObject(value)) for (const item of Object.values(value)) walk(item, depth + 1);
  };
  walk(args, 0);
  return strings.join(" ");
}

/** The words that open a shell command, so `gh` at a command's start is found and `… | gh` too. */
const COMMAND_START = /(?:^|[;&|(]|&&|\|\||\$\(|\n)\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*([^\s;&|()]+)/g;

/** Whether a call is a GitHub read: a `gh` command, or a request to a host of GitHub's API. */
function githubRead(entry: TranscriptEntry, apiHosts: string[]): { place: string; merge: string | null } | null {
  const command = entry.command ?? "";
  if (command === "") return null;
  const tool = entry.tool ?? "";
  if (tool === "envoy_publish" || tool === "envoy_send" || tool === "legion") return null;
  for (const match of command.matchAll(COMMAND_START)) {
    const program = (match[1] ?? "").split("/").pop() ?? "";
    if (program !== "gh") continue;
    const after = command.slice((match.index ?? 0) + match[0].length);
    const words = after.trim().split(/\s+/);
    if (words[0] === "api") return { place: "gh api", merge: null };
    if (words[0] === "pr" && words[1] === "merge") {
      const head = /--match-head-commit[= ]+([0-9a-f]{40})/.exec(command)?.[1] ?? null;
      return { place: "gh pr merge --match-head-commit", merge: head };
    }
    if (words[0] === "pr" && words[1] !== undefined) return { place: `gh ${words[0]} ${words[1]}`, merge: null };
    return { place: "gh", merge: null };
  }
  for (const url of command.matchAll(/https?:\/\/([A-Za-z0-9.:-]+)(\/[^\s"'\\]*)?/g)) {
    const host = (url[1] ?? "").toLowerCase();
    if (!apiHosts.includes(host)) continue;
    const first = command.trim().split(/\s+/)[0] ?? "";
    return { place: `${first.split("/").pop() ?? first} ${url[0]}`, merge: null };
  }
  return null;
}

/** GitHub's answer about a head, read from a payload: its value and the head it names. */
export function answerOfPayload(text: string): { value: AnswerValue; head: string | null } | null {
  const mergeable = /"mergeable"\s*:\s*"?(MERGEABLE|CONFLICTING|UNKNOWN|true|false|null)"?/i.exec(text)?.[1];
  const status =
    /"mergeStateStatus"\s*:\s*"([A-Za-z_]+)"/.exec(text)?.[1] ?? /"mergeable_state"\s*:\s*"([A-Za-z_]+)"/.exec(text)?.[1];
  if (mergeable === undefined && status === undefined) return null;
  const head =
    /"headRefOid"\s*:\s*"([0-9a-f]{40})"/.exec(text)?.[1] ??
    /"head"\s*:\s*\{[^}]*"sha"\s*:\s*"([0-9a-f]{40})"/.exec(text)?.[1] ??
    null;
  const state = (status ?? "").toUpperCase();
  const merge = (mergeable ?? "").toUpperCase();
  const blocked = merge === "CONFLICTING" || merge === "FALSE" || ["DIRTY", "BLOCKED", "BEHIND", "DRAFT"].includes(state);
  if (blocked) return { value: "blocked", head };
  const clear = (merge === "MERGEABLE" || merge === "TRUE") && (state === "" || ["CLEAN", "HAS_HOOKS", "UNSTABLE"].includes(state));
  return { value: clear ? "clear" : "indeterminate", head };
}

/** Whether a call is the role's own `request_backward_move`, the cut every answer stands before. */
function backwardMoveCall(entry: TranscriptEntry): boolean {
  if (entry.kind !== "call") return false;
  if (isObject(entry.args) && entry.args.op === "request_backward_move") return true;
  return (entry.command ?? "").includes("request_backward_move");
}

/**
 * The answers a session holds about the merge, in transcript order: the result of each GitHub read
 * that stands before the role's own `request_backward_move` call. A result that holds no payload
 * about the merge gives no answer, and a refused `gh pr merge` gives a `blocked` answer about the
 * head its own `--match-head-commit` names, which the transcript joins to the result by the call's
 * id. Nothing else is an answer: no assistant message, no tool call's arguments, no delivery, and
 * no result of a command that is not a GitHub read.
 */
export function transcriptAnswers(entries: TranscriptEntry[], options: { apiHosts: string[] }): Answer[] {
  const cut = entries.findIndex(backwardMoveCall);
  const upTo = cut === -1 ? entries : entries.slice(0, cut);
  const calls = new Map<string, TranscriptEntry>();
  for (const entry of upTo) if (entry.kind === "call" && entry.callId !== undefined) calls.set(entry.callId, entry);
  const answers: Answer[] = [];
  for (const entry of upTo) {
    if (entry.kind !== "result" || entry.callId === undefined) continue;
    const call = calls.get(entry.callId);
    if (call === undefined) continue;
    const read = githubRead(call, options.apiHosts);
    if (read === null) continue;
    if (read.merge !== null && entry.isError === true) {
      answers.push({ source: "transcript", place: read.place, at: entry.at, value: "blocked", head: read.merge });
      continue;
    }
    const payload = answerOfPayload(entry.text ?? "");
    if (payload === null) continue;
    answers.push({ source: "transcript", place: read.place, at: entry.at, value: payload.value, head: payload.head });
  }
  return answers;
}

// ---------------------------------------------------------------------------
// The stated causes of a reason, and the record of each
// ---------------------------------------------------------------------------

/** What the stated causes are read from. */
export interface ReasonFacts {
  kind: "retro" | "merge";
  reason: string | null;
  /** The block ids of the issue's Verification document. */
  checkIds: string[];
  /** The names of the check runs a CI job writes for this issue. */
  ciJobNames: string[];
  /** The block ids of the issue's spec's requirements. */
  requirementIds: string[];
}

/** The words of `merge-blocked`, matched at the start of a word, in any case. */
const BLOCK_WORDS = ["CONFLICTING", "DIRTY", "BLOCKED", "conflict", "CHANGES_REQUESTED", "requested changes", "merge queue"];

/** The words of `ci`, matched anywhere, in any case. */
const CI_WORDS = ["failed_checks", "merge_group", "required check"];

/** A new version of the design, as a reason names one: `spec v4`, `Verification document version 9`. */
const NEWS = /\b(spec|verification)\b[^.\n]{0,60}?\bv(?:ersion)?\s?(\d+)/i;

/** Whether text holds name as a name of its own, with neither a letter, a digit nor a hyphen beside it. */
function namesWord(text: string, name: string): boolean {
  if (name === "") return false;
  const escaped = name.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
  return new RegExp(`(^|[^A-Za-z0-9_-])${escaped}([^A-Za-z0-9_-]|$)`, "i").test(text);
}

/**
 * The causes a reason states, in the order of CAUSES. Each rule stands by itself, so one reason
 * can state several causes. No rule says what a reason does not hold, and nothing here decides
 * that the role's own defect caused the return. A reason that was not read states nothing.
 */
export function statedCauses(facts: ReasonFacts): StatedCause[] {
  const reason = facts.reason;
  if (reason === null || reason === "") return [];
  const stated: StatedCause[] = [];
  const lower = reason.toLowerCase();
  if (BLOCK_WORDS.some((word) => new RegExp(`\\b${word.toLowerCase()}`).test(lower))) stated.push("merge-blocked");
  const ci =
    CI_WORDS.some((word) => lower.includes(word.toLowerCase())) ||
    facts.ciJobNames.some((name) => namesWord(reason, name));
  if (ci) stated.push("ci");
  if (facts.kind === "retro" && facts.checkIds.some((id) => namesWord(reason, id))) stated.push("failed-check");
  if (NEWS.test(reason)) stated.push("news");
  if (facts.kind === "retro" && facts.requirementIds.some((id) => namesWord(reason, id))) stated.push("goal-check");
  return CAUSES.filter((cause) => stated.includes(cause));
}

/**
 * The record of each stated cause, with its state. `merge-blocked`'s comes from the classification
 * above, with the step that gave it. Each other cause reads one source of Dispatch — its results,
 * its events or its step records — and its record is `unread` when the head or that read failed,
 * `established` when the read finds the record, and `absent` when it does not: those sources keep
 * their history, so a read that finds nothing is `absent`.
 */
export function causeRecords(facts: ReturnFacts): CauseRecord[] {
  const stated = statedCauses({
    kind: facts.kind,
    reason: facts.reasonRead ? facts.reason : null,
    checkIds: (facts.checks ?? []).map((check) => check.blockId),
    ciJobNames: (facts.checks ?? []).flatMap((check) => (check.ciCheck === null ? [] : [check.ciCheck])),
    requirementIds: facts.requirementIds ?? [],
  });
  const records: CauseRecord[] = [];
  for (const cause of stated) {
    if (cause === "merge-blocked") {
      const { state, step } = classifyMergeBlocked(facts.mergeBlocked);
      records.push({ cause, state, step });
      continue;
    }
    records.push({ cause, state: otherRecord(cause, facts), step: null });
  }
  return records;
}

/** The state of the record of a cause that is not `merge-blocked`. */
function otherRecord(cause: Exclude<StatedCause, "merge-blocked">, facts: ReturnFacts): RecordState {
  if (facts.head === null || facts.head === "") return "unread";
  const reason = facts.reason ?? "";
  switch (cause) {
    case "ci": {
      if (facts.checks === null) return "unread";
      const ciChecks = facts.checks.filter((check) => check.ciCheck !== null && check.ciCheck !== "");
      return ciChecks.some((check) => check.passStands !== "yes") ? "established" : "absent";
    }
    case "failed-check": {
      if (facts.checks === null) return "unread";
      const named = facts.checks.filter((check) => namesWord(reason, check.blockId));
      return named.some((check) => check.passStands !== "yes") ? "established" : "absent";
    }
    case "news": {
      if (facts.approvals === null) return "unread";
      const start = timeOf(facts.stepStartedAt);
      const end = timeOf(facts.at);
      const inRun = facts.approvals.some((approval) => {
        const at = timeOf(approval.at);
        if (at === null || end === null) return false;
        return (start === null || at >= start) && at <= end;
      });
      return inRun ? "established" : "absent";
    }
    case "goal-check": {
      const reviewed = facts.taskSteps.some((step) => step.role === "reviewer" || step.role === "acceptance");
      return reviewed ? "absent" : "established";
    }
  }
}

// ---------------------------------------------------------------------------
// The resolution of a return, and the summary of a batch
// ---------------------------------------------------------------------------

/**
 * Whether a stated cause holds, from its record's state and the final reading: `true` where it
 * holds, `false` where it does not, and `"pending"` where the pair leaves the return pending — an
 * `undetermined` or `unread` record that the reading confirms. A reading that a cause did not
 * cause the return needs no record.
 */
function causeHolds(record: CauseRecord, confirmed: boolean): boolean | "pending" {
  if (!confirmed) return false;
  if (record.state === "established") return true;
  if (record.state === "absent") return false;
  return "pending";
}

/** The reading that decides a return, or null where none does. */
function finalReading(ret: ReturnLine, readings: Reading[]): Reading | null {
  const mine = readings.filter((reading) => reading.return === ret.id);
  const resolver = mine.find((reading) => reading.reader === "resolver");
  if (resolver !== undefined) return resolver;
  const one = mine.find((reading) => reading.reader === "1");
  if (one === undefined) return null;
  if (classWith(ret, one) === "against-R23") return one;
  const two = mine.find((reading) => reading.reader === "2");
  if (two === undefined) return null;
  return readingsAgree(ret, one, two) ? one : null;
}

/** Whether two readings of one return say the same thing of its own defect and of each cause. */
function readingsAgree(ret: ReturnLine, one: Reading, two: Reading): boolean {
  if (one.own !== two.own) return false;
  return ret.causes.every((record) => (one.confirmed[record.cause] === true) === (two.confirmed[record.cause] === true));
}

/** The class one reading alone gives a return. */
function classWith(ret: ReturnLine, reading: Reading): ResolvedClass {
  const outcomes = ret.causes.map((record) => ({ cause: record.cause, holds: causeHolds(record, reading.confirmed[record.cause] === true) }));
  if (outcomes.some((outcome) => outcome.holds === "pending")) return "pending";
  const holds = outcomes.filter((outcome) => outcome.holds === true).map((outcome) => outcome.cause);
  if (reading.own) return holds.length === 0 ? "against-R23" : "mixed";
  if (holds.includes("goal-check")) return "goal-check";
  const first = CAUSES.find((cause) => holds.includes(cause));
  return first === undefined ? "unexplained" : `exempt:${first}`;
}

/**
 * The class a return resolves to, with the reading that decided it and why. A return whose reason
 * was not read is `pending`: it rests on no claim about any cause, and the role's own defect
 * cannot be judged without the reason.
 */
export function resolveReturn(ret: ReturnLine, readings: Reading[]): { class: ResolvedClass; final: Reading | null; why: string } {
  if (!ret.reasonRead) return { class: "pending", final: null, why: "the reason was not read" };
  const final = finalReading(ret, readings);
  if (final === null) {
    const mine = readings.filter((reading) => reading.return === ret.id);
    const why =
      mine.length === 0
        ? "no reader read it"
        : mine.some((reading) => reading.reader === "2")
          ? "the two readers disagree and no resolver read it"
          : "reader 1 leaves it out of the count and reader 2 did not read it";
    return { class: "pending", final: null, why };
  }
  const name = classWith(ret, final);
  const why = name === "pending" ? "a cause the reading confirms has no record that decides it" : `the reading of reader ${final.reader}`;
  return { class: name, final, why };
}

/**
 * Each reading of reader 2 that breaks the rule: reader 2 reads each return that reader 1's
 * reading leaves out of the count, and no other.
 */
export function readerTwoFaults(returns: ReturnLine[], readings: Reading[]): string[] {
  const faults: string[] = [];
  for (const ret of returns) {
    const mine = readings.filter((reading) => reading.return === ret.id);
    const one = mine.find((reading) => reading.reader === "1");
    const two = mine.find((reading) => reading.reader === "2");
    if (one === undefined || two === undefined) continue;
    if (classWith(ret, one) === "against-R23") {
      faults.push(
        `reader 2 read ${ret.id}, which reader 1's reading counts against R23: reader 2 reads each return that reader 1 leaves out of the count, and no other`
      );
    }
  }
  return faults;
}

/** One return of a summary: the issue it is of, and the class it resolved to. */
export interface SummaryReturn {
  issue: string;
  class: ResolvedClass;
}

/** A day or a project the batch could not read. */
export interface Unknown {
  project: string;
  day: string | null;
}

/**
 * The summary of one batch: the count of each class, how many last steps passed, what could not
 * be read, the verdict, and one line for each issue with a return. A day or a project that could
 * not be read is `unknown`, never a zero.
 */
export function summaryOf(input: { returns: SummaryReturn[]; lastStepsPassed: number; unknown: Unknown[] }): string {
  const count = (name: ResolvedClass) => input.returns.filter((ret) => ret.class === name).length;
  const lines = [
    `against R23: ${count("against-R23")}`,
    `mixed: ${count("mixed")}`,
    `goal-check: ${count("goal-check")}`,
    ...CAUSES.map((cause) => `exempt ${cause}: ${count(`exempt:${cause}`)}`),
    `unexplained: ${count("unexplained")}`,
    `pending: ${count("pending")}`,
    `last steps passed: ${input.lastStepsPassed}`,
    `unknown: ${
      input.unknown.length === 0
        ? "none"
        : input.unknown
            .map((entry) => (entry.day === null ? `${entry.project} (every day of the window)` : `${entry.project} ${entry.day}`))
            .join(", ")
    }`,
  ];
  const failed = count("against-R23") > 0;
  const pass = !failed && count("pending") === 0 && input.unknown.length === 0 && input.lastStepsPassed > 0;
  lines.push(`verdict: ${failed ? "fail" : pass ? "pass" : "unknown"}`);
  for (const ret of input.returns) lines.push(`${ret.issue} ${ret.class}`);
  return `${lines.join("\n")}\n`;
}

// ---------------------------------------------------------------------------
// The readers: Dispatch, GitHub and the transcripts
// ---------------------------------------------------------------------------

/** A read that did not answer, with the words a return's source keeps. */
class ReadFailed extends Error {}

/** Dispatch, read with the reader's own bearer. Nothing here calls a route that writes. */
class Dispatch {
  constructor(
    private readonly url: string,
    private readonly token: string
  ) {}

  async get<T>(route: string): Promise<T> {
    let last = "";
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        const answer = await fetch(`${this.url}${route}`, { headers: { Authorization: `Bearer ${this.token}` } });
        if (answer.ok) return (await answer.json()) as T;
        last = `${answer.status} ${(await answer.text()).slice(0, 200)}`;
        if (answer.status < 500 && answer.status !== 429) break;
      } catch (error) {
        last = error instanceof Error ? error.message : String(error);
      }
      await Bun.sleep(500 * (attempt + 1));
    }
    throw new ReadFailed(`GET ${route}: ${last}`);
  }
}

/** GitHub, read at the API the run names. */
class GitHub {
  constructor(
    private readonly api: string,
    private readonly token: string
  ) {}

  /** The hosts a transcript's request must name for its result to be an answer. */
  hosts(): string[] {
    const hosts = ["api.github.com"];
    try {
      hosts.push(new URL(this.api).host.toLowerCase());
    } catch {
      // An API the run names badly reaches nothing, and its reads say so on their own.
    }
    return [...new Set(hosts)];
  }

  async graphql(query: string, variables: Record<string, unknown>): Promise<unknown> {
    const answer = await fetch(`${this.api.replace(/\/+$/, "")}/graphql`, {
      method: "POST",
      headers: { Authorization: `Bearer ${this.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ query, variables }),
    });
    if (!answer.ok) throw new ReadFailed(`POST /graphql: ${answer.status} ${(await answer.text()).slice(0, 200)}`);
    const body = (await answer.json()) as { data?: unknown; errors?: unknown };
    if (body.errors !== undefined) throw new ReadFailed(`POST /graphql: ${JSON.stringify(body.errors).slice(0, 200)}`);
    return body.data;
  }

  async rest<T>(route: string): Promise<T> {
    const answer = await fetch(`${this.api.replace(/\/+$/, "")}${route}`, {
      headers: { Authorization: `Bearer ${this.token}`, Accept: "application/vnd.github+json" },
    });
    if (!answer.ok) throw new ReadFailed(`GET ${route}: ${answer.status} ${(await answer.text()).slice(0, 200)}`);
    return (await answer.json()) as T;
  }
}

/** The query of the pull request's timeline, as `P/roles/merger.md` asks for it. */
const TIMELINE_QUERY = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      timelineItems(itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT,PULL_REQUEST_REVIEW],last:50){
        nodes{
          __typename
          ... on RemovedFromMergeQueueEvent{createdAt beforeCommit{oid}}
          ... on PullRequestReview{createdAt submittedAt state commit{oid}}
        }
      }
    }
  }
}`;

/** `tq sql` over the transcript store, as JSON lines. */
function tqSql(sql: string): Record<string, unknown>[] {
  const run = spawnSync("tq", ["sql", "--format", "JSONEachRow", sql], {
    encoding: "utf8",
    maxBuffer: 512 * 1024 * 1024,
  });
  if (run.error !== undefined && run.error !== null) throw new ReadFailed(`tq sql: ${run.error.message}`);
  if (run.status !== 0) throw new ReadFailed(`tq sql exited ${run.status}: ${(run.stderr ?? "").trim().slice(0, 300)}`);
  return (run.stdout ?? "")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as Record<string, unknown>);
}

/** A string of a row, "" where the row holds none. */
function text(row: Record<string, unknown>, key: string): string {
  const value = row[key];
  return typeof value === "string" ? value : value === null || value === undefined ? "" : String(value);
}

/** The entries of each session the store holds, built from its calls and their results. */
function storeSessions(sessions: string[], from: string): Map<string, TranscriptEntry[]> {
  const held = new Map<string, TranscriptEntry[]>();
  if (sessions.length === 0) return held;
  const list = sessions.map((session) => `'${session.replace(/'/g, "")}'`).join(",");
  const rows = tqSql(
    `SELECT c.session_id AS session_id, c.call_id AS call_id, c.tool AS tool,
            toString(c.started_at) AS started_at, toString(c.ended_at) AS ended_at,
            c.is_error AS is_error, c.command AS command, substring(m.text, 1, 40000) AS result
     FROM calls c
     LEFT JOIN (SELECT session_id, host, tool_call_id, any(text) AS text FROM messages
                WHERE kind = 'tool_result' AND ts >= '${from}' GROUP BY session_id, host, tool_call_id) m
       ON m.session_id = c.session_id AND m.host = c.host AND m.tool_call_id = c.call_id
     WHERE c.session_id IN (${list}) AND c.started_at >= '${from}'
     ORDER BY c.session_id, c.started_at`
  );
  for (const row of rows) {
    const session = text(row, "session_id");
    const entries = held.get(session) ?? [];
    const command = text(row, "command");
    const at = text(row, "started_at");
    const ended = text(row, "ended_at");
    entries.push({
      index: entries.length,
      at: at === "" ? null : `${at.replace(" ", "T")}Z`,
      kind: "call",
      tool: text(row, "tool"),
      callId: text(row, "call_id"),
      args: {},
      command,
    });
    entries.push({
      index: entries.length,
      at: ended === "" ? null : `${ended.replace(" ", "T")}Z`,
      kind: "result",
      tool: text(row, "tool"),
      callId: text(row, "call_id"),
      text: text(row, "result"),
      isError: row.is_error === true || row.is_error === 1 || row.is_error === "1",
    });
    held.set(session, entries);
  }
  return held;
}

/** Every `request_backward_move` call the store holds in the window, by session and time. */
function storeBackwardMoves(from: string, to: string): { session: string; at: string; issue: string | null }[] {
  const rows = tqSql(
    `SELECT c.session_id AS session_id, toString(c.started_at) AS started_at, c.command AS command
     FROM calls c
     WHERE c.tool = 'legion' AND c.command ILIKE '%request_backward_move%'
       AND c.started_at >= '${from}' AND c.started_at < '${to}'
     ORDER BY c.started_at`
  );
  return rows.map((row) => {
    const command = text(row, "command");
    const issue = /"issue"\s*:\s*"([A-Z][A-Z0-9]*-\d+)"/.exec(command)?.[1] ?? null;
    return { session: text(row, "session_id"), at: `${text(row, "started_at").replace(" ", "T")}Z`, issue };
  });
}

/** The entries of each session file of a directory, in the form the store imports. */
function directorySessions(dir: string): Map<string, TranscriptEntry[]> {
  const held = new Map<string, TranscriptEntry[]>();
  for (const name of readdirSync(dir)) {
    if (!name.endsWith(".jsonl")) continue;
    const parsed = parseSession(readFileSync(path.join(dir, name), "utf8"));
    const session = parsed.session ?? name.replace(/\.jsonl$/, "");
    held.set(session, parsed.entries);
  }
  return held;
}

// ---------------------------------------------------------------------------
// collect
// ---------------------------------------------------------------------------

/** Dispatch's answer for one issue's step records. */
interface StepsAnswer {
  issue: string;
  tasks: { task: string; steps: StepRecord[] }[];
}

interface StepRecord {
  id: string;
  task: string;
  step: number;
  kind: string;
  role: string;
  type: string;
  agent: string;
  session: string;
  attempt: number;
  started_at: string | null;
  ended_at: string | null;
  result: string;
  report: string | null;
}

interface MessageRead {
  message: { id: string; body: string; created_at: string; author: { kind: string; id: string } };
}

interface EventRead {
  seq: number;
  type: string;
  created_at: string;
  payload: Record<string, unknown>;
}

interface ResultsRead {
  checks: {
    block_id: string;
    ci_check?: string;
    pass_stands: { answer: string };
  }[];
}

interface CoverageRead {
  requirements: { block_id: string }[];
}

/** The batch `collect` wrote, which `resolve` reads beside returns.jsonl. */
interface Batch {
  from: string;
  to: string;
  projects: string[];
  transcripts: string;
  reasons: string;
  collectedAt: string;
  lastStepsPassed: { project: string; count: number }[];
  unknown: Unknown[];
}

/** Runs `run` over `items`, at most `limit` at a time, keeping the answers in order. */
async function mapLimit<T, R>(items: T[], limit: number, run: (item: T) => Promise<R>): Promise<R[]> {
  const answers: R[] = new Array(items.length);
  let next = 0;
  const workers = Array.from({ length: Math.min(limit, items.length) }, async () => {
    for (;;) {
      const place = next++;
      if (place >= items.length) return;
      answers[place] = await run(items[place] as T);
    }
  });
  await Promise.all(workers);
  return answers;
}

/** Each day of the window, as the unknown list names them. */
function daysOf(from: string, to: string): string[] {
  const days: string[] = [];
  for (let at = Date.parse(`${from}T00:00:00Z`); at <= Date.parse(`${to}T00:00:00Z`); at += 86_400_000) {
    days.push(new Date(at).toISOString().slice(0, 10));
  }
  return days;
}

interface CollectArgs {
  dispatchUrl: string;
  token: string;
  githubApiUrl: string;
  githubToken: string;
  projects: string[];
  from: string;
  to: string;
  transcripts: { kind: "tq" } | { kind: "dir"; path: string } | { kind: "none" };
  reasons: "all" | "none";
  out: string;
}

async function collect(args: CollectArgs): Promise<void> {
  const dispatch = new Dispatch(args.dispatchUrl, args.token);
  const github = new GitHub(args.githubApiUrl, args.githubToken);
  const unknown: Unknown[] = [];
  const lastStepsPassed: { project: string; count: number }[] = [];
  const lines: ReturnLine[] = [];
  const windowFrom = Date.parse(`${args.from}T00:00:00Z`);
  const windowTo = Date.parse(`${args.to}T00:00:00Z`) + 86_400_000;

  /** Every day of a project is unknown once one of its reads did not answer. */
  const cannotRead = (project: string, why: string) => {
    process.stderr.write(`unknown: ${project}: ${why}\n`);
    for (const day of daysOf(args.from, args.to)) {
      if (!unknown.some((entry) => entry.project === project && entry.day === day)) unknown.push({ project, day });
    }
  };

  for (const project of args.projects) {
    let issues: { key: string }[] = [];
    try {
      issues = await listIssues(dispatch, project, args.from);
    } catch (error) {
      cannotRead(project, `the issue listing did not answer: ${messageOf(error)}`);
      lastStepsPassed.push({ project, count: 0 });
      continue;
    }
    process.stderr.write(`${project}: ${issues.length} issues updated since ${args.from}\n`);
    let passed = 0;
    const found: { issue: string; record: StepRecord; task: string; steps: StepRecord[] }[] = [];
    const reads = await mapLimit(issues, 8, async (issue) => {
      try {
        return await dispatch.get<StepsAnswer>(`/api/v1/issues/${issue.key}/steps`);
      } catch (error) {
        cannotRead(project, `the steps of ${issue.key} did not answer: ${messageOf(error)}`);
        return null;
      }
    });
    for (const read of reads) {
      if (read === null) continue;
      for (const task of read.tasks) {
        for (const record of task.steps) {
          if (record.kind !== "retro" && record.kind !== "merge") continue;
          const ended = timeOf(record.ended_at);
          if (ended === null || ended < windowFrom || ended >= windowTo) continue;
          if (record.result === "passed") passed++;
          if (record.result !== "failed") continue;
          found.push({ issue: read.issue, record, task: task.task, steps: task.steps });
        }
      }
    }
    lastStepsPassed.push({ project, count: passed });
    process.stderr.write(`${project}: ${found.length} returns of the retro and the merge in the window\n`);

    const sessions = [...new Set(found.map((row) => row.record.session).filter((session) => session !== ""))];
    let transcripts = new Map<string, TranscriptEntry[]>();
    let transcriptWhy = "the run read no transcript (--transcripts none)";
    if (args.transcripts.kind === "tq") {
      try {
        transcripts = storeSessions(sessions, args.from);
        transcriptWhy = "the transcript store holds no entry of this session";
      } catch (error) {
        transcriptWhy = `the transcript store did not answer: ${messageOf(error)}`;
        process.stderr.write(`${project}: ${transcriptWhy}\n`);
      }
    } else if (args.transcripts.kind === "dir") {
      transcripts = directorySessions(args.transcripts.path);
      transcriptWhy = "this directory holds no session of that id";
    }

    const collected = await mapLimit(found, 4, async (row) =>
      lineOf({ dispatch, github, args, project, row, transcripts, transcriptWhy })
    );
    for (const line of collected) if (line !== null) lines.push(line);

    if (args.transcripts.kind === "tq") {
      try {
        addStoreOnly(lines, found, project, storeBackwardMoves(args.from, args.to), args);
      } catch (error) {
        process.stderr.write(`${project}: the store's backward moves did not answer: ${messageOf(error)}\n`);
      }
    }
  }

  mkdirSync(args.out, { recursive: true });
  writeFileSync(path.join(args.out, "returns.jsonl"), lines.map((line) => `${JSON.stringify(line)}\n`).join(""), { mode: 0o644 });
  const batch: Batch = {
    from: args.from,
    to: args.to,
    projects: args.projects,
    transcripts: args.transcripts.kind === "dir" ? `dir:${args.transcripts.path}` : args.transcripts.kind,
    reasons: args.reasons,
    collectedAt: new Date().toISOString(),
    lastStepsPassed,
    unknown,
  };
  writeFileSync(path.join(args.out, "batch.json"), `${JSON.stringify(batch, null, 2)}\n`, { mode: 0o644 });
  process.stdout.write(`collected ${lines.length} returns of ${args.projects.join(", ")} into ${args.out}\n`);
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Every issue of a project that changed since the window's first day. */
async function listIssues(dispatch: Dispatch, project: string, from: string): Promise<{ key: string }[]> {
  const issues: { key: string }[] = [];
  const limit = 250;
  for (let offset = 0; ; offset += limit) {
    const page = await dispatch.get<{ issues: { key: string }[]; total: number }>(
      `/api/v1/issues?project=${project}&updated_since=${from}T00:00:00Z&limit=${limit}&offset=${offset}`
    );
    issues.push(...page.issues);
    if (page.issues.length < limit || issues.length >= page.total) return issues;
  }
}

/** One return's line, read from Dispatch, GitHub and the role's transcript. */
async function lineOf(input: {
  dispatch: Dispatch;
  github: GitHub;
  args: CollectArgs;
  project: string;
  row: { issue: string; record: StepRecord; task: string; steps: StepRecord[] };
  transcripts: Map<string, TranscriptEntry[]>;
  transcriptWhy: string;
}): Promise<ReturnLine | null> {
  const { dispatch, github, args, project, row, transcripts, transcriptWhy } = input;
  const record = row.record;
  const at = record.ended_at ?? record.started_at ?? "";
  const kind = record.kind === "retro" ? "retro" : "merge";
  const id = `${row.issue}/${row.task}/${kind}/${record.attempt}`;

  let events: EventRead[] | null = null;
  try {
    events = await issueEvents(dispatch, row.issue);
  } catch (error) {
    process.stderr.write(`${row.issue}: the events did not answer: ${messageOf(error)}\n`);
  }

  const pull = pullRequestOf(events, at);
  const head = await headOf(dispatch, github, record, pull, at);

  let reason: string | null = null;
  let reasonRead = args.reasons === "none" ? true : false;
  if (args.reasons === "all") {
    try {
      reason = await reasonOf(dispatch, row.issue, record, at);
      reasonRead = true;
    } catch (error) {
      process.stderr.write(`${row.issue}: the messages did not answer: ${messageOf(error)}\n`);
      reasonRead = false;
    }
  }
  if (args.reasons === "none") reason = null;

  let checks: CheckRead[] | null = null;
  if (head !== null) {
    try {
      const read = await dispatch.get<ResultsRead>(`/api/v1/issues/${row.issue}/results?at=${head}`);
      checks = read.checks.map((check) => ({
        blockId: check.block_id,
        ciCheck: check.ci_check === undefined || check.ci_check.trim() === "" ? null : check.ci_check.trim(),
        passStands: (check.pass_stands.answer === "yes" ? "yes" : check.pass_stands.answer === "no" ? "no" : "unknown") as CheckRead["passStands"],
      }));
    } catch (error) {
      process.stderr.write(`${row.issue}: the results did not answer: ${messageOf(error)}\n`);
    }
  }

  let requirementIds: string[] | null = null;
  if (kind === "retro") {
    try {
      const read = await dispatch.get<CoverageRead>(`/api/v1/issues/${row.issue}/coverage`);
      requirementIds = read.requirements.map((requirement) => requirement.block_id);
    } catch (error) {
      process.stderr.write(`${row.issue}: the coverage did not answer: ${messageOf(error)}\n`);
    }
  } else {
    requirementIds = [];
  }

  const approvals = events === null ? null : approvalsOf(events);

  const entries = transcripts.get(record.session);
  const transcriptRead: MergeBlockedRead["transcript"] =
    args.transcripts.kind === "none"
      ? { state: "unread", answers: [], why: "the run read no transcript (--transcripts none)" }
      : entries === undefined
        ? { state: "unread", answers: [], why: transcriptWhy }
        : { state: "read", answers: transcriptAnswers(entries, { apiHosts: github.hosts() }) };

  const stated = statedCauses({
    kind,
    reason: reasonRead ? reason : null,
    checkIds: (checks ?? []).map((check) => check.blockId),
    ciJobNames: (checks ?? []).flatMap((check) => (check.ciCheck === null ? [] : [check.ciCheck])),
    requirementIds: requirementIds ?? [],
  });
  let timeline: MergeBlockedRead["timeline"] = { state: "read", items: [] };
  if (stated.includes("merge-blocked")) {
    timeline = pull === null ? { state: "unread", items: [], why: "the issue holds no pull request" } : await timelineOf(github, pull, at);
  }

  const facts: ReturnFacts = {
    kind,
    at,
    stepStartedAt: record.started_at,
    head,
    reason,
    reasonRead,
    checks,
    approvals,
    taskSteps: row.steps.map((step) => ({ role: step.role, kind: step.kind, type: step.type })),
    requirementIds,
    mergeBlocked: { head, transcript: transcriptRead, timeline },
  };
  return {
    id,
    project,
    issue: row.issue,
    task: row.task,
    kind,
    attempt: record.attempt,
    role: record.role,
    session: record.session,
    at,
    day: at.slice(0, 10),
    head,
    reason,
    reasonRead,
    heldBy: ["steps"],
    sources: { transcript: transcriptRead.state, timeline: timeline.state },
    causes: causeRecords(facts),
    answers: [...transcriptRead.answers, ...timeline.items],
  };
}

/** Every event of an issue, oldest first. */
async function issueEvents(dispatch: Dispatch, issue: string): Promise<EventRead[]> {
  const events: EventRead[] = [];
  const limit = 200;
  for (let after = 0; ; ) {
    const page = await dispatch.get<EventRead[]>(`/api/v1/issues/${issue}/events?after=${after}&limit=${limit}`);
    events.push(...page);
    if (page.length < limit) return events;
    after = page[page.length - 1]?.seq ?? after + limit;
  }
}

/** Each approval of a new version of the issue's spec or Verification document, by its time. */
function approvalsOf(events: EventRead[]): { at: string }[] {
  return events
    .filter((event) => event.type === "artifact.approved")
    .filter((event) => {
      const name = typeof event.payload.name === "string" ? event.payload.name : "";
      return name !== "plan.md";
    })
    .map((event) => ({ at: event.created_at }));
}

/** The pull request the issue's events attribute to it, newest at or before the return. */
function pullRequestOf(events: EventRead[] | null, at: string): { repo: string; number: number } | null {
  if (events === null) return null;
  const moment = timeOf(at);
  let found: { repo: string; number: number } | null = null;
  for (const event of events) {
    if (event.type !== "issue.pull_request_attribution_changed") continue;
    const when = timeOf(event.created_at);
    if (moment !== null && when !== null && when > moment && found !== null) continue;
    const repo = typeof event.payload.repo === "string" ? event.payload.repo : "";
    const number = typeof event.payload.number === "number" ? event.payload.number : 0;
    if (repo === "" || number === 0) continue;
    found = { repo, number };
  }
  return found;
}

/**
 * The head the role read: the commit the step's own handoff record names, where it names one, else
 * the newest commit of the pull request whose committer time is before the return. A step record
 * holds no commit of its own in this release, so the handoff record it names is the first source.
 */
async function headOf(
  dispatch: Dispatch,
  github: GitHub,
  record: StepRecord,
  pull: { repo: string; number: number } | null,
  at: string
): Promise<string | null> {
  if (record.report !== null && record.report !== "") {
    try {
      const handoff = await dispatch.get<{ body: string }>(`/api/v1/handoffs/${record.report}`);
      const head = /"head"\s*:\s*"([0-9a-f]{40})"/.exec(handoff.body)?.[1];
      if (head !== undefined) return head;
    } catch {
      // The pull request's commits answer the head where the handoff does not.
    }
  }
  if (pull === null) return null;
  try {
    const commits = await github.rest<{ sha: string; commit: { committer: { date: string } } }[]>(
      `/repos/${pull.repo}/pulls/${pull.number}/commits?per_page=100`
    );
    const moment = timeOf(at);
    const before = commits.filter((commit) => {
      const when = timeOf(commit.commit?.committer?.date ?? null);
      return when !== null && moment !== null && when <= moment;
    });
    return before.at(-1)?.sha ?? null;
  } catch {
    return null;
  }
}

/** The reason of a return: the daemon's own message of the move, as backwardMoveReason writes it. */
async function reasonOf(dispatch: Dispatch, issue: string, record: StepRecord, at: string): Promise<string | null> {
  const messages = await dispatch.get<MessageRead[]>(`/api/v1/issues/${issue}/messages`);
  const moment = timeOf(at);
  const form = /^Legion moved this issue from (\S+) back to (\S+) at the ([a-z]+)'s request(?::\s*|,\s*)?([\s\S]*)$/;
  let found: { at: number; reason: string } | null = null;
  for (const read of messages) {
    const body = read.message.body ?? "";
    const match = form.exec(body.trim());
    if (match === null) continue;
    if (match[3] !== record.role) continue;
    const when = timeOf(read.message.created_at);
    if (when === null || moment === null) continue;
    if (when < moment - 120_000 || when > moment + 900_000) continue;
    const reason = (match[4] ?? "").split("\n\n<!--")[0]?.trim() ?? "";
    if (found === null || Math.abs(when - moment) < Math.abs(found.at - moment)) found = { at: when, reason };
  }
  return found === null ? null : found.reason;
}

/** The timeline items of a pull request that count: each about a head, before the return. */
async function timelineOf(
  github: GitHub,
  pull: { repo: string; number: number },
  at: string
): Promise<MergeBlockedRead["timeline"]> {
  const [owner, name] = pull.repo.split("/");
  try {
    const data = (await github.graphql(TIMELINE_QUERY, { owner, name, number: pull.number })) as {
      repository?: { pullRequest?: { timelineItems?: { nodes?: unknown[] } } };
    };
    const nodes = data.repository?.pullRequest?.timelineItems?.nodes ?? [];
    const moment = timeOf(at);
    const items: Answer[] = [];
    for (const node of nodes) {
      if (!isObject(node)) continue;
      const kind = typeof node.__typename === "string" ? node.__typename : "";
      const createdAt = typeof node.createdAt === "string" ? node.createdAt : null;
      const when = timeOf(createdAt);
      if (moment !== null && when !== null && when >= moment) continue;
      if (kind === "RemovedFromMergeQueueEvent") {
        const before = isObject(node.beforeCommit) && typeof node.beforeCommit.oid === "string" ? node.beforeCommit.oid : null;
        items.push({ source: "timeline", place: "REMOVED_FROM_MERGE_QUEUE_EVENT", at: createdAt, value: "blocked", head: before });
        continue;
      }
      if (kind === "PullRequestReview" && node.state === "CHANGES_REQUESTED") {
        const commit = isObject(node.commit) && typeof node.commit.oid === "string" ? node.commit.oid : null;
        const submitted = typeof node.submittedAt === "string" ? node.submittedAt : createdAt;
        items.push({ source: "timeline", place: "PULL_REQUEST_REVIEW CHANGES_REQUESTED", at: submitted, value: "blocked", head: commit });
      }
    }
    return { state: "read", items };
  } catch (error) {
    return { state: "unread", items: [], why: `GitHub's timeline did not answer: ${messageOf(error)}` };
  }
}

/** Each `request_backward_move` the store holds that Dispatch's step records do not: a return of
 * that source alone, listed with it. */
function addStoreOnly(
  lines: ReturnLine[],
  found: { issue: string; record: StepRecord; task: string; steps: StepRecord[] }[],
  project: string,
  calls: { session: string; at: string; issue: string | null }[],
  args: CollectArgs
): void {
  const byId = new Map(lines.map((line) => [line.id, line]));
  const collected = new Set(found.map((row) => row.record.session));
  const windowFrom = Date.parse(`${args.from}T00:00:00Z`);
  const windowTo = Date.parse(`${args.to}T00:00:00Z`) + 86_400_000;
  for (const call of calls) {
    if (collected.has(call.session)) {
      for (const line of byId.values()) {
        if (line.session === call.session && !line.heldBy.includes("transcript")) line.heldBy.push("transcript");
      }
      continue;
    }
    const when = timeOf(call.at);
    if (when === null || when < windowFrom || when >= windowTo) continue;
    if (call.issue === null || !call.issue.startsWith(`${project}-`)) continue;
    const id = `${call.issue}/unknown/unknown/1`;
    if (byId.has(id)) continue;
    const line: ReturnLine = {
      id,
      project,
      issue: call.issue,
      task: "unknown",
      kind: "retro",
      attempt: 1,
      role: "unknown",
      session: call.session,
      at: call.at,
      day: call.at.slice(0, 10),
      head: null,
      reason: null,
      reasonRead: false,
      heldBy: ["transcript"],
      sources: { transcript: "read", timeline: "unread" },
      causes: [],
      answers: [],
    };
    byId.set(id, line);
    lines.push(line);
  }
}

// ---------------------------------------------------------------------------
// resolve
// ---------------------------------------------------------------------------

function resolve(out: string, readingsFile: string): void {
  const returns = readFileSync(path.join(out, "returns.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as ReturnLine);
  const batch = JSON.parse(readFileSync(path.join(out, "batch.json"), "utf8")) as Batch;
  const readings =
    readingsFile === ""
      ? []
      : readFileSync(readingsFile, "utf8")
          .split("\n")
          .filter((line) => line.trim() !== "")
          .map((line) => JSON.parse(line) as Reading);

  const faults = readerTwoFaults(returns, readings);
  if (faults.length > 0) {
    for (const fault of faults) process.stderr.write(`readings refused: ${fault}\n`);
    process.exit(1);
  }

  const resolved: ResolvedLine[] = returns.map((ret) => {
    const mine = readings.filter((reading) => reading.return === ret.id);
    const answer = resolveReturn(ret, readings);
    return { ...ret, readings: mine, final: answer.final, class: answer.class, why: answer.why };
  });
  writeFileSync(path.join(out, "resolved.jsonl"), resolved.map((line) => `${JSON.stringify(line)}\n`).join(""), { mode: 0o644 });

  const everyProject = [...new Set([...batch.projects, ...resolved.map((ret) => ret.project)])];
  for (const project of everyProject) {
    const text = summaryOf({
      returns: resolved.filter((ret) => ret.project === project),
      lastStepsPassed: batch.lastStepsPassed.find((entry) => entry.project === project)?.count ?? 0,
      unknown: batch.unknown.filter((entry) => entry.project === project),
    });
    writeFileSync(path.join(out, `summary-${project}.txt`), text, { mode: 0o644 });
  }
  const whole = summaryOf({
    returns: resolved,
    lastStepsPassed: batch.lastStepsPassed.reduce((sum, entry) => sum + entry.count, 0),
    unknown: batch.unknown,
  });
  writeFileSync(path.join(out, "summary.txt"), whole, { mode: 0o644 });
  process.stdout.write(whole);
}

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

const USAGE = `usage:
  bun forward-count.ts collect --dispatch-url <url> --token-file <file> --github-api-url <url> \\
    --github-token-file <file> --projects <K1,K2,…> --from <YYYY-MM-DD> --to <YYYY-MM-DD> \\
    --transcripts tq|dir:<path>|none --reasons all|none --out <dir>
  bun forward-count.ts resolve --out <dir> [--readings <readings.jsonl>]`;

function flags(argv: string[]): Map<string, string> {
  const given = new Map<string, string>();
  for (let place = 0; place < argv.length; place++) {
    const word = argv[place] ?? "";
    if (!word.startsWith("--")) continue;
    const [name, inline] = word.slice(2).split("=", 2);
    if (name === undefined) continue;
    if (inline !== undefined) {
      given.set(name, inline);
      continue;
    }
    const next = argv[place + 1];
    given.set(name, next !== undefined && !next.startsWith("--") ? next : "true");
    if (next !== undefined && !next.startsWith("--")) place++;
  }
  return given;
}

function fileText(file: string, what: string): string {
  if (!existsSync(file)) {
    process.stderr.write(`${what} names ${file}, which is no file\n`);
    process.exit(2);
  }
  return readFileSync(file, "utf8").trim();
}

async function main(argv: string[]): Promise<void> {
  const command = argv[0];
  const given = flags(argv.slice(1));
  if (command === "resolve") {
    const out = given.get("out") ?? "";
    if (out === "") {
      process.stderr.write(`${USAGE}\n`);
      process.exit(2);
    }
    resolve(out, given.get("readings") ?? "");
    return;
  }
  if (command !== "collect") {
    process.stderr.write(`${USAGE}\n`);
    process.exit(2);
  }
  const transcripts = given.get("transcripts") ?? "none";
  const required = ["dispatch-url", "token-file", "projects", "from", "to", "out"];
  const missing = required.filter((name) => (given.get(name) ?? "") === "");
  if (missing.length > 0) {
    process.stderr.write(`collect needs ${missing.join(", ")}\n${USAGE}\n`);
    process.exit(2);
  }
  await collect({
    dispatchUrl: (given.get("dispatch-url") ?? "").replace(/\/+$/, ""),
    token: fileText(given.get("token-file") ?? "", "--token-file"),
    githubApiUrl: given.get("github-api-url") ?? "https://api.github.com",
    githubToken: given.get("github-token-file") === undefined ? "" : fileText(given.get("github-token-file") ?? "", "--github-token-file"),
    projects: (given.get("projects") ?? "").split(",").map((project) => project.trim()).filter((project) => project !== ""),
    from: given.get("from") ?? "",
    to: given.get("to") ?? "",
    transcripts: transcripts.startsWith("dir:")
      ? { kind: "dir", path: transcripts.slice(4) }
      : transcripts === "tq"
        ? { kind: "tq" }
        : { kind: "none" },
    reasons: given.get("reasons") === "none" ? "none" : "all",
    out: given.get("out") ?? "",
  });
}

if (import.meta.main) {
  await main(process.argv.slice(2));
}
