/**
 * The hook Oh My Pi loads for a capture (`omp --hook <this file>`, capture.ts), built from the
 * scratch replay hook of the fixtures' captures (testdata/CAPTURE.md, "The scratch tools"):
 *
 * - Its `context` handler gives the model exactly the arm's messages: the JSON list of Oh My Pi
 *   message objects in the file SKILL_CUTS_MESSAGES names. Every message the run itself holds is
 *   dropped: the placeholder prompt, and what a plugin added to a run outside a real pane (a
 *   notice that its endpoints do not answer). The arm's messages come from the real transcript,
 *   which already holds every block the real session's plugins added. When SKILL_CUTS_HOOK_LOG
 *   names a file, each request appends one JSON line to it, `{"dropped": <messages dropped>}`.
 * - Its `tool_call` handler blocks every tool call, so no tool runs in a capture.
 * - When SKILL_CUTS_SYSTEM_PROMPT names a file, a JSON list of the system prompt's blocks, its
 *   `before_agent_start` handler returns them as the turn's system prompt, which Oh My Pi sends in
 *   place of the prompt it built (an extension's override, applied whole): a subagent case's
 *   recorded prompt (capture.ts). With the variable unset the hook adds no such handler.
 *
 * It imports nothing beyond Node's own modules, since Oh My Pi loads it from this directory alone.
 */
import { appendFileSync, readFileSync } from "node:fs";

/** The variable that names the file of the arm's messages. */
export const MESSAGES_VARIABLE = "SKILL_CUTS_MESSAGES";

/** The variable that names the file each request's count of dropped messages is appended to. */
export const HOOK_LOG_VARIABLE = "SKILL_CUTS_HOOK_LOG";

/** The variable that names the file of the system prompt's blocks the turn is sent with. */
export const SYSTEM_PROMPT_VARIABLE = "SKILL_CUTS_SYSTEM_PROMPT";

/** Why a call was blocked, as the model reads it in the call's result. */
export const BLOCK_REASON = "blocked by the replay hook: no tool runs in a capture";

/** The part of Oh My Pi's hook API this hook uses. */
interface HookApi {
  on(
    event: "context",
    handler: (event: { readonly messages?: readonly unknown[] }) => Promise<{ messages: unknown[] }>
  ): void;
  on(event: "tool_call", handler: () => Promise<{ block: true; reason: string }>): void;
  on(event: "before_agent_start", handler: () => Promise<{ systemPrompt: string[] }>): void;
}

export default function replayHook(pi: HookApi): void {
  const file = process.env[MESSAGES_VARIABLE];
  if (file === undefined || file === "") {
    throw new Error(`replay-hook: ${MESSAGES_VARIABLE} names no file of messages`);
  }
  const replayed: unknown = JSON.parse(readFileSync(file, "utf8"));
  if (!Array.isArray(replayed)) throw new Error(`replay-hook: ${file} holds no list of messages`);
  const log = process.env[HOOK_LOG_VARIABLE];
  pi.on("context", async (event) => {
    if (log !== undefined && log !== "") {
      appendFileSync(log, `${JSON.stringify({ dropped: event.messages?.length ?? 0 })}\n`);
    }
    return { messages: [...replayed] };
  });
  pi.on("tool_call", async () => ({ block: true, reason: BLOCK_REASON }));
  const promptFile = process.env[SYSTEM_PROMPT_VARIABLE];
  if (promptFile === undefined || promptFile === "") return;
  const blocks: unknown = JSON.parse(readFileSync(promptFile, "utf8"));
  if (!Array.isArray(blocks) || !blocks.every((block) => typeof block === "string")) {
    throw new Error(`replay-hook: ${promptFile} holds no list of system prompt blocks`);
  }
  pi.on("before_agent_start", async () => ({ systemPrompt: [...blocks] }));
}
