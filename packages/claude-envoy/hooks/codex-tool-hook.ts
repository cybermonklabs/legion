// The Codex CLI's `PostToolUse` hook for the Envoy plugin: `codex/hooks/hooks.json`
// runs `dist/codex-tool-hook.js` after every tool call of a Codex session.
//
// It takes the messages waiting in the session's inbox (`../src/codex/codex-inbox`), oldest first
// and as many as fit under Codex's cap on a hook's text (`HOOK_CONTEXT_MAX_BYTES`), and hands them
// to Codex as the hook's `additionalContext`, which reaches the model in its very next request,
// inside the running turn (measured on Codex CLI 0.154.0). A message that does not fit, and every
// one behind it, waits for `codex queue` at the turn's end. With nothing waiting it prints nothing. It runs after every tool call, so it is a bundle of its own and imports only the
// inbox, which needs nothing but the filesystem, and the exit flush; and it exits 0 whatever
// happens, because a failure here must never fail the tool call it follows.

import { exitProcess } from "@legion/envoy-client/process-exit"
import {
  codexInboxDirectory,
  HOOK_CONTEXT_MAX_BYTES,
  inboxContext,
  takeInbox,
} from "../src/codex/codex-inbox"

/** How long the exit gives stdout to take the messages the hook took out of the inbox before it
 *  exits whatever is still queued (`exitProcess`): Codex reads a hook's output as it comes, so
 *  only a reader that takes nothing keeps the hook this long. Codex kills the hook at 10 s
 *  (`codex/hooks/hooks.json`'s PostToolUse `timeout`); the take before the exit is file renames,
 *  and half of those 10 s is left to it. */
const EXIT_FLUSH_MS = 5_000

async function postToolUse(text: string): Promise<void> {
  const input: unknown = JSON.parse(text)
  const sessionId =
    typeof input === "object" && input !== null
      ? (input as { session_id?: unknown }).session_id
      : undefined
  const pluginData = process.env["PLUGIN_DATA"] || process.env["CLAUDE_PLUGIN_DATA"]
  if (typeof sessionId !== "string" || sessionId === "" || !pluginData) return
  // Only what fits under Codex's cap on a hook's text; a longer message waits for `codex queue`.
  const taken = await takeInbox(
    codexInboxDirectory(pluginData, sessionId),
    "hook",
    HOOK_CONTEXT_MAX_BYTES,
  )
  if (taken.length === 0) return
  process.stdout.write(
    JSON.stringify({
      hookSpecificOutput: {
        additionalContext: inboxContext(taken.map(({ item }) => item)),
        hookEventName: "PostToolUse",
      },
    }),
  )
}

try {
  await postToolUse(await Bun.stdin.text())
} catch (error) {
  process.stderr.write(
    `envoy: codex-tool-hook: ${error instanceof Error ? error.message : String(error)}\n`,
  )
}
// Exits rather than returns, once stdout has taken the messages taken out of the inbox: they are
// no longer waiting, so an exit that dropped them would lose them with nothing said.
await exitProcess(0, EXIT_FLUSH_MS)
