// The Oh My Pi session transcript shapes score.ts, seed.ts and the skill-cuts runner read, so a
// run's own JSONL has one parser and one notion of what a tool result says, never two that drift;
// and the reading of a `bash` call's shell text that score.ts and the skill-cuts runner share.
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** An Oh My Pi message, as a transcript line holds it: its assistant content holds each tool call
 * whole (`toolCall`: id, tool name, arguments) and its tool results name the call they answer,
 * whether the tool refused it (`isError`) and the tool's own record (`details`; a `bash` result's
 * holds its `exitCode`). Fields it does not name are kept as they are. */
export const SessionMessage = z.looseObject({
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
});
export type SessionMessage = z.infer<typeof SessionMessage>;

/** An Oh My Pi session transcript line: a message (SessionMessage), or an entry of its own. Its
 * `tool_execution_start` entries keep only the first 200 characters of the arguments, so nothing
 * here reads them. The first line is the header, `{"type":"session", "id":"<session id>"}`. A
 * background task's report arrives as a line of its own (`"type":"custom_message"`,
 * `"customType":"async-result"`) whose `content` is its text. */
export const SessionEntry = z.looseObject({
  type: z.string().optional(),
  id: z.unknown().optional(),
  timestamp: z.string().optional(),
  customType: z.string().optional(),
  content: z.unknown().optional(),
  message: SessionMessage.optional(),
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
export function subagentSessions(runDir: string): { name: string; entries: SessionEntry[] }[] {
  const full = sessionFile(runDir);
  if (full === undefined) return [];
  const dir = full.replace(/\.jsonl$/, "");
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".jsonl"))
    .map((entry) => entry.name)
    .sort()
    .map((name) => ({
      name: name.replace(/\.jsonl$/, ""),
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

/** Every tool call the session made, in order: its id, its tool, and its whole arguments as JSON
 * and their `path` and `command`. */
export function toolCalls(entries: SessionEntry[]) {
  return entries.flatMap((entry) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => {
            const args = part.arguments;
            const fields: Record<string, unknown> =
              typeof args === "object" && args !== null ? { ...args } : {};
            return {
              id: part.id ?? "",
              tool: part.name ?? "",
              path: typeof fields.path === "string" ? fields.path : "",
              command: typeof fields.command === "string" ? fields.command : "",
              args: JSON.stringify(args ?? null),
            };
          })
  );
}

/** Every `read` call the session made whose `path` is a string, in order: its id and the path or
 * URL it read. */
export function readCalls(entries: SessionEntry[]): { id: string; target: string }[] {
  const reads: { id: string; target: string }[] = [];
  for (const entry of entries) {
    if (entry.message?.role !== "assistant") continue;
    for (const part of entry.message.content ?? []) {
      if (part.type !== "toolCall" || part.name !== "read") continue;
      const args = part.arguments;
      const target =
        typeof args === "object" && args !== null ? Reflect.get(args, "path") : undefined;
      if (typeof target === "string") reads.push({ id: part.id ?? "", target });
    }
  }
  return reads;
}

/** The `read` calls of `messages` (`readCalls`), by call id: the path or URL each one read. */
export function readTargets(messages: readonly SessionMessage[]): Map<string, string> {
  return new Map(
    readCalls(messages.map((message) => ({ message }))).map((call) => [call.id, call.target])
  );
}

/** The line the pinned Oh My Pi puts before the text of a file it read by a `skill://` URL,
 * `[Skill file: <absolute path>]`, then a newline. That line is not the file's. */
export function skillFileHeader(file: string): string {
  return `[Skill file: ${file}]`;
}

/** The `[Skill file: <absolute path>]` line a read result's text starts with, then a newline. */
const SKILL_HEADER = /^\[Skill file: (\/[^\n]*)\]\n/;

/** A read result's text less its `[Skill file: <absolute path>]` line. Any other first line stays. */
export function stripSkillHeader(text: string): string {
  return text.replace(SKILL_HEADER, "");
}

/** The absolute path a read result's `[Skill file: <absolute path>]` line names, or undefined when
 * its text starts with no such line. */
export function skillHeaderPath(text: string): string | undefined {
  return SKILL_HEADER.exec(text)?.[1];
}

/** Dispatch's answer when `dispatch issue` creates an issue, whatever reached it: the command in a
 * `bash` call, the same run from inside an `eval` cell, or a host that calls the tool by name. A
 * refusal reads `Not created:`, which the capital and the key after it keep from matching. */
export const CREATED = /\bCreated ([A-Z][A-Z0-9]*-\d+):/;

/** Where a command starts in shell text: at a line's start, or after `;`, `&`, `|`, `(`, `$(` or a
 * backquote, past any variable assignments, the words that open a command inside a compound one
 * (`then`, `do`, `else`, `elif`, `!`, `{`), and `command`, `exec` or `env`. */
export const AT_COMMAND = String.raw`(?:^|[;&|(\x60]|\$\()[ \t]*(?:[A-Za-z_]\w*=\S*[ \t]+)*(?:(?:then|do|else|elif|!|\{)[ \t]+)*(?:(?:command|exec|env)[ \t]+)?`;
/** A here-document: the rest of the line that opens it, then its body up to the line that ends it. */
const HEREDOC = /(<<-?[ \t]*(['"]?)(\w+)\2[^\n]*\n)[\s\S]*?^[ \t]*\3[ \t]*$/gm;

/** Shell text with its lines joined where a backslash continues them, and its here-document
 * bodies taken out (a body is data for the command whose `<<` opens it), with each body and the
 * place of its `<<` in that text. */
export function withoutBodies(source: string): {
  code: string;
  bodies: { at: number; text: string }[];
} {
  const joined = source.replace(/\\\r?\n/g, " ");
  const bodies: { at: number; text: string }[] = [];
  let code = "";
  let last = 0;
  for (const hit of joined.matchAll(HEREDOC)) {
    const opener = hit[1] ?? "";
    code += joined.slice(last, hit.index) + opener;
    bodies.push({ at: code.length - opener.length, text: hit[0].slice(opener.length) });
    last = (hit.index ?? 0) + hit[0].length;
  }
  return { code: code + joined.slice(last), bodies };
}
