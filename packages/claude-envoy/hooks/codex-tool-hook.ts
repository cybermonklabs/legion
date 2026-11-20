// The Codex CLI's `PostToolUse` hook for the Envoy plugin: `codex/hooks/hooks.json`
// runs `dist/codex-tool-hook.js` after every tool call of a Codex session.
//
// It takes the messages waiting in the session's inbox (`../src/codex/codex-inbox`), oldest first
// and as many as fit under Codex's cap on a hook's text (`HOOK_CONTEXT_MAX_BYTES`), and hands them
// to Codex as the hook's `additionalContext`, which reaches the model in its very next request,
// inside the running turn (measured on Codex CLI 0.154.0). A message that does not fit, and every
// one behind it, waits for `codex queue` at the turn's end. With nothing waiting it prints nothing.
// It runs after every tool call, so it is a bundle of its own and imports only the inbox, which
// needs nothing but the filesystem, and the stream write below; and it exits 0 whatever happens,
// because a failure here must never fail the tool call it follows.
//
// Codex reads the hook's stdout through a pipe, and Bun drops what a full pipe has not taken when
// a program calls `process.exit`: 4,096 of 7,974 bytes reached a one-page pipe read 2 s late, with
// exit 0, while the messages had already left the inbox. So the hook holds what it took until its
// write is done (`claimInbox`), moves it into `taken/` only once Codex has every byte, puts it back
// to wait when the write failed, and ends by itself, never by `process.exit`.

import { writeOut } from "@legion/envoy-client/process-exit"
import {
  claimInbox,
  codexInboxDirectory,
  HOOK_CONTEXT_MAX_BYTES,
  inboxContext,
} from "../src/codex/codex-inbox"

async function postToolUse(text: string): Promise<void> {
  const input: unknown = JSON.parse(text)
  const sessionId =
    typeof input === "object" && input !== null
      ? (input as { session_id?: unknown }).session_id
      : undefined
  const pluginData = process.env["PLUGIN_DATA"] || process.env["CLAUDE_PLUGIN_DATA"]
  if (typeof sessionId !== "string" || sessionId === "" || !pluginData) return
  // Only what fits under Codex's cap on a hook's text; a longer message waits for `codex queue`.
  const claim = await claimInbox(codexInboxDirectory(pluginData, sessionId), HOOK_CONTEXT_MAX_BYTES)
  if (claim.taken.length === 0) return
  let delivered = false
  try {
    delivered = await writeOut(
      process.stdout,
      JSON.stringify({
        hookSpecificOutput: {
          additionalContext: inboxContext(claim.taken.map(({ item }) => item)),
          hookEventName: "PostToolUse",
        },
      }),
    )
  } finally {
    await claim.settle(delivered)
  }
}

try {
  await postToolUse(await Bun.stdin.text())
} catch (error) {
  process.stderr.write(
    `envoy: codex-tool-hook: ${error instanceof Error ? error.message : String(error)}\n`,
  )
}
