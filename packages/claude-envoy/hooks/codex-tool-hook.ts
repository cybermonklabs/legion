// The Codex CLI's `PostToolUse` hook for the Envoy plugin (INFRA-3558): `codex/hooks.json` runs
// `dist/codex-tool-hook.js` after every tool call of a Codex session.
//
// It takes every message waiting in the session's inbox (`../src/codex/codex-inbox`) and hands
// them to Codex as the hook's `additionalContext`, which reaches the model in its very next
// request, inside the running turn (measured on Codex CLI 0.154.0). With nothing waiting it
// prints nothing. It runs after every tool call, so it is a bundle of its own and imports only the
// inbox, which needs nothing but the filesystem; and it exits 0 whatever happens, because a
// failure here must never fail the tool call it follows.

import { codexInboxDirectory, inboxContext, takeInbox } from "../src/codex/codex-inbox"

async function postToolUse(text: string): Promise<void> {
  const input: unknown = JSON.parse(text)
  const sessionId =
    typeof input === "object" && input !== null
      ? (input as { session_id?: unknown }).session_id
      : undefined
  const pluginData = process.env["PLUGIN_DATA"] || process.env["CLAUDE_PLUGIN_DATA"]
  if (typeof sessionId !== "string" || sessionId === "" || !pluginData) return
  const taken = await takeInbox(codexInboxDirectory(pluginData, sessionId), "hook")
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
process.exit(0)
