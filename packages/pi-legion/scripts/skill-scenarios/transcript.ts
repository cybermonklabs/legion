// The Oh My Pi session transcript shapes score.ts and seed.ts both read, so a run's own JSONL has
// one parser and one notion of what a tool result says, never two that can drift.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import { z } from "zod";
import type { OutcomeRecord } from "./heldout-case";
import { parseInstant } from "./production-dispatch";

/** An Oh My Pi session transcript line: a message, whose assistant content holds each tool call
 * whole (`toolCall`: id, tool name, arguments) and whose tool results name the call they answer,
 * whether the tool refused it (`isError`) and the tool's own record (`details`; a `bash` result's
 * holds its `exitCode`). Its `tool_execution_start` entries keep only the first 200 characters of
 * the arguments, so nothing here reads them. The first line is the header, `{"type":"session",
 * "id":"<session id>"}`. A background task's report arrives as a line of its own
 * (`"type":"custom_message"`, `"customType":"async-result"`) whose `content` is its text. */
export const SessionEntry = z.looseObject({
  type: z.string().optional(),
  id: z.unknown().optional(),
  timestamp: z.string().optional(),
  customType: z.string().optional(),
  content: z.unknown().optional(),
  message: z
    .looseObject({
      role: z.string().optional(),
      toolName: z.string().optional(),
      toolCallId: z.string().optional(),
      isError: z.boolean().optional(),
      stopReason: z.string().optional(),
      errorMessage: z.string().optional(),
      details: z.unknown().optional(),
      content: z
        .array(
          z.looseObject({
            type: z.string(),
            text: z.string().optional(),
            id: z.string().optional(),
            name: z.string().optional(),
            arguments: z.unknown().optional(),
          })
        )
        .optional(),
    })
    .optional(),
});
export type SessionEntry = z.infer<typeof SessionEntry>;

/** The session id the transcript's header names, or undefined when it has no header. */
export function sessionId(entries: SessionEntry[]): string | undefined {
  const id = entries.find((entry) => entry.type === "session")?.id;
  return typeof id === "string" ? id : undefined;
}

/** The file of the run's own session transcript, or undefined when it never got a model turn. */
function sessionFile(runDir: string): string | undefined {
  const dir = path.join(runDir, "sessions");
  const file = existsSync(dir)
    ? readdirSync(dir).find((name) => name.endsWith(".jsonl"))
    : undefined;
  return file === undefined ? undefined : path.join(dir, file);
}
/** A transcript file, parsed whole. A line that does not parse is the rig's fault. Every reader of
 * a transcript file parses it through this. */
export function parsedSession(full: string): SessionEntry[] {
  return readFileSync(full, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line, index) => {
      const result = SessionEntry.safeParse(JSON.parse(line));
      if (!result.success)
        throw new Error(`${full}:${index + 1} is not a record: ${result.error.message}`);
      return result.data;
    });
}

/** The run's own session transcript, parsed whole, or empty when it never got a model turn. A line
 * that does not parse is the rig's fault. */
export function session(runDir: string): SessionEntry[] {
  const full = sessionFile(runDir);
  return full === undefined ? [] : parsedSession(full);
}

/** Each subagent the run's session started, by its name, with its transcript parsed whole. Oh My Pi
 * writes every subagent's transcript to `<session file less .jsonl>/<name>.jsonl` beside the
 * session's own, whatever started it: a `task` tool call, or `tool.task(…)` or the `agent(…)`
 * helper inside an eval cell. A subagent's own subagents, one directory deeper, are left out. */
export function subagentSessions(runDir: string): Transcript[] {
  const full = sessionFile(runDir);
  return full === undefined ? [] : childSessions(full);
}

/** A transcript file, its name (the file's name less `.jsonl`) and its entries parsed whole. */
export interface Transcript {
  readonly name: string;
  readonly file: string;
  readonly entries: SessionEntry[];
}

/** The subagents the session whose transcript is `file` started, by name: each transcript of
 * `<file less .jsonl>/`, sorted by name. */
export function childSessions(file: string): Transcript[] {
  const dir = file.replace(/\.jsonl$/, "");
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".jsonl"))
    .map((entry) => entry.name)
    .sort()
    .map((name) => ({
      name: name.replace(/\.jsonl$/, ""),
      file: path.join(dir, name),
      entries: parsedSession(path.join(dir, name)),
    }));
}

/** Every tool result's text, in order: a bare `env`/`printenv` dump reaches the model only here, in
 * the `toolResult` message a tool call's own arguments never carry. */
export function toolResults(entries: SessionEntry[]): string[] {
  return entries.flatMap((entry) => {
    const message = entry.message;
    if (message?.role !== "toolResult") return [];
    return [(message.content ?? []).map((part) => part.text ?? "").join("")];
  });
}

/** Dispatch's answer when `dispatch issue` creates an issue, whatever reached it: the command in a
 * `bash` call, the same run from inside an `eval` cell, or a host that calls the tool by name. A
 * refusal reads `Not created:`, which the capital and the key after it keep from matching. */
export const CREATED = /\bCreated ([A-Z][A-Z0-9]*-\d+):/;

/** Every assistant tool call in transcript order, with the entry that carried it. */
export function sessionCalls(entries: readonly SessionEntry[]) {
  return entries.flatMap((entry) =>
    entry.message?.role === "assistant"
      ? (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((call) => ({ entry, call }))
      : []
  );
}

export function resultText(message: SessionEntry["message"]): string {
  return (message?.content ?? []).map((part) => part.text ?? "").join("");
}

/** Each `skill://` read, in call order, as heldout-case.ts's SkillRead records it. Count the bytes
 * delivered, not the path banner or the text of a failure. Match by call id: tool results in a
 * parallel batch can finish in a different order than their calls. `beforeFirstAction` is whether
 * the read came before the run's first scored action of a T0 kind (heldout-case.ts
 * T0_ACTION_KINDS), whose time `firstAction` is in nanoseconds on the host clock that stamps the
 * transcript's entries (undefined where the run took no such action): the read's place in the
 * transcript, the time of the entry that carries its call, is earlier than that action's. A tool
 * call that makes no such action, such as a `bash`, a `grep`, an Envoy message, a Dispatch read or
 * a task agent's start, is no first action, and where the run took none every read came before
 * it. */
export function skillReads(
  entries: readonly SessionEntry[],
  firstAction?: bigint
): OutcomeRecord["skillReads"] {
  const results = new Map(
    entries
      .map((entry) => entry.message)
      .filter((message) => message?.role === "toolResult" && message.toolCallId !== undefined)
      .map((message) => [message?.toolCallId, message])
  );
  const reads: OutcomeRecord["skillReads"] = [];
  for (const { entry, call } of sessionCalls(entries)) {
    if (call.name !== "read") continue;
    const args = call.arguments;
    const uri = typeof args === "object" && args !== null ? Reflect.get(args, "path") : undefined;
    if (typeof uri !== "string") continue;
    const match = /^skill:\/\/([A-Za-z0-9][A-Za-z0-9._-]*)(?:\/(.*))?$/.exec(uri);
    if (!match?.[1]) continue;
    const result = results.get(call.id);
    const text = resultText(result);
    const resolved = result !== undefined && !result.isError && !/^Unknown skill\b/i.test(text);
    let beforeFirstAction = true;
    if (firstAction !== undefined) {
      const calledAt = parseInstant(entry.timestamp ?? "");
      if (calledAt === undefined)
        throw new Error(
          `the read of ${uri} is in a transcript entry with no RFC 3339 timestamp, so whether it came before the first scored action is unknown`
        );
      beforeFirstAction = calledAt < firstAction;
    }
    reads.push({
      skill: match[1],
      path: match[2] || "SKILL.md",
      resolved,
      bytes: resolved ? Buffer.byteLength(text.replace(/^\[Skill file: \/[^\n]*\]\r?\n/, "")) : 0,
      beforeFirstAction,
    });
  }
  return reads;
}

type Action = OutcomeRecord["actions"][number];
/** An action of `kind`, as a transcript shows it: at the host clock's time of the entry that carries
 * its call, and with no `seq`, which the outcome gives every action once they are in time order. */
export type TranscriptAction<K extends Action["kind"]> = Omit<Extract<Action, { kind: K }>, "seq">;

const Arguments = z.record(z.string(), z.json());
type Arguments = z.infer<typeof Arguments>;

/** A tool call as its tool takes it: the tool's name, and its arguments without Oh My Pi's intent
 * (`i`), which every tool takes beside its own. A `write` to a tool's `xd://<name>` device, whose
 * content is the arguments' JSON, is a call of that tool; a device write whose content is no JSON
 * object, or a call whose arguments are none, is no call of it. A host can prefix a tool's name
 * with `_`, which names the same tool. */
function toolCall(call: {
  name?: string;
  arguments?: unknown;
}): { name: string; args: Arguments } | undefined {
  const name = call.name?.replace(/^_+/, "") ?? "";
  const own = Arguments.safeParse(call.arguments).data;
  if (own === undefined) return undefined;
  const device = typeof own.path === "string" ? /^xd:\/\/(\w+)$/.exec(own.path.trim()) : null;
  if (name === "write" && device?.[1] !== undefined) {
    let content: unknown;
    try {
      content = JSON.parse(typeof own.content === "string" ? own.content : "");
    } catch {
      return undefined;
    }
    const args = Arguments.safeParse(content).data;
    if (args === undefined) return undefined;
    const { i: _intent, ...rest } = args;
    return { name: device[1], args: rest };
  }
  const { i: _intent, ...rest } = own;
  return { name, args: rest };
}

/** Each tool result, by the id of the call it answers. */
function resultsById(entries: readonly SessionEntry[]) {
  return new Map(
    entries.flatMap((entry) =>
      entry.message?.role === "toolResult" && entry.message.toolCallId !== undefined
        ? [[entry.message.toolCallId, entry.message] as const]
        : []
    )
  );
}

/** The host clock's time of the entry that carries a call of `what`; a transcript entry with none
 * is the rig's fault, since the call's place among the run's actions is then unknown. */
function calledAt(entry: SessionEntry, what: string): string {
  if (entry.timestamp === undefined || parseInstant(entry.timestamp) === undefined)
    throw new Error(
      `${what} is in a transcript entry with no RFC 3339 timestamp, so its place among the run's actions is unknown`
    );
  return entry.timestamp;
}

/** The Envoy tools whose messages an outcome records, by the word it names each one. */
const ENVOY_TOOLS: Readonly<Record<string, "publish" | "send">> = {
  envoy_publish: "publish",
  envoy_send: "send",
};

/** Each Envoy message the session sent, in call order: a call of envoy_publish or envoy_send, by
 * the tool's name or as a `write` to its `xd://` device, that the tool answered without error. A
 * call the tool refused, or one it never answered, sent nothing the run can show. `target` is the
 * topic of a publish and the session of a send, and `fields` the call's other arguments, named as
 * the tool names them. */
export function envoySends(entries: readonly SessionEntry[]): TranscriptAction<"envoy">[] {
  const results = resultsById(entries);
  return sessionCalls(entries).flatMap(({ entry, call }): TranscriptAction<"envoy">[] => {
    const made = toolCall(call);
    const tool = made === undefined ? undefined : ENVOY_TOOLS[made.name];
    if (made === undefined || tool === undefined) return [];
    const result = call.id === undefined ? undefined : results.get(call.id);
    if (result === undefined || result.isError === true) return [];
    const key = tool === "publish" ? "topic" : "session_id";
    const { [key]: target, ...fields } = made.args;
    if (typeof target !== "string" || target.trim() === "") return [];
    return [{ kind: "envoy", at: calledAt(entry, `the ${made.name} call`), tool, target, fields }];
  });
}

/** Each task agent the session started, in call order: one for each item of a `task` call's
 * `tasks` (the call itself where it names none, as an older host's call does), by the tool's name
 * or as a `write` to its `xd://task` device. `agent` is the item's (`task` where it names none),
 * `name` its name, and `fields` its other members. `endedAt` is the time of the last entry of the
 * transcript of that name among `children`, the transcripts of the agents the session started
 * (childSessions), absent where it wrote none. A call the tool refused started no agent: one it
 * answered with an error, none of whose items wrote a transcript. An item with no name is no agent
 * the run can name. */
export function taskAgents(
  entries: readonly SessionEntry[],
  children: readonly Transcript[]
): TranscriptAction<"agent">[] {
  const results = resultsById(entries);
  const ended = new Map(
    children.flatMap(({ name, entries: own }) => {
      const last = own.findLast((each) => parseInstant(each.timestamp ?? "") !== undefined);
      return last?.timestamp === undefined ? [] : [[name, last.timestamp] as const];
    })
  );
  return sessionCalls(entries).flatMap(({ entry, call }): TranscriptAction<"agent">[] => {
    const made = toolCall(call);
    if (made?.name !== "task") return [];
    const listed = made.args.tasks;
    const items = (Array.isArray(listed) ? listed : [made.args]).flatMap((item) => {
      const fields = Arguments.safeParse(item).data;
      return fields === undefined ? [] : [fields];
    });
    const refused =
      call.id !== undefined &&
      results.get(call.id)?.isError === true &&
      !items.some((item) => typeof item.name === "string" && ended.has(item.name));
    if (refused) return [];
    return items.flatMap(({ agent, name, ...fields }): TranscriptAction<"agent">[] => {
      if (typeof name !== "string" || name.trim() === "") return [];
      const endedAt = ended.get(name);
      return [
        {
          kind: "agent",
          at: calledAt(entry, `the task call of ${name}`),
          agent: typeof agent === "string" ? agent : "task",
          name,
          fields,
          ...(endedAt === undefined ? {} : { endedAt }),
        },
      ];
    });
  });
}

/** Recognize the actual context message, not a tool result quoting the marker: a user message
 * with a text block that opens with the marker, at any place in the message, since a host can put
 * a block of its own (Oh My Pi's dated system reminder) ahead of it. Some hosts inject this on
 * the provider request only; pass positive wire evidence in that case. */
export function dispatchFirstInjected(
  entries: readonly SessionEntry[],
  onProviderRequest = false
): boolean {
  return (
    onProviderRequest ||
    entries.some(
      ({ message }) =>
        message?.role === "user" &&
        (message.content ?? []).some(
          (part) => part.type === "text" && part.text?.startsWith(DISPATCH_FIRST_MARKER) === true
        )
    )
  );
}
