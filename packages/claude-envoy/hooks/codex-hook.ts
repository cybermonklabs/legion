// The Codex CLI's `SessionStart` hook for the Envoy plugin (INFRA-3558): `codex/hooks.json` runs
// `dist/codex-hook.js session-start` at a session's first prompt and again after `/new`.
//
// It records which session the `codex` process now runs, where the plugin's MCP server reads it
// (`../src/codex/codex-session-record`): the server is that process's child too, but Codex hands
// it no session id. It writes the record Claude Code's hook writes as well
// (`sessions/<pid>/session-id`), so the server's session code follows `/new` to the new id the way
// it follows a Claude Code `/clear`. Then it tells the model this session's open Dispatch asks and
// how to run `dispatch` here, as plain output, which Codex adds to the model's context (measured on
// 0.154.0). The dispatch-first skill and the answers list come from `dist/session-hook.js`, which
// `codex/hooks.json` runs beside this. It always exits 0: a session must start without Envoy.

import { formatOpenAsksSummary } from "@legion/envoy-client/dispatch-execute"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionDispatchSettings } from "@legion/envoy-client/session-settings"
import { z } from "zod"
import { configuredValue } from "../src/claude-session"
import {
  codexExecutable,
  codexHome,
  codexProcessId,
  codexSessionRecordFile,
  processStarted,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"

/** How long the open-asks read may take before the summary says it is unavailable. */
const OPEN_ASKS_TIMEOUT_MS = 3_000

const SessionStartInput = z.object({
  session_id: z.string().min(1),
  transcript_path: z.string().nullable().optional(),
  cwd: z.string().min(1),
  source: z.string().nullable().optional(),
})

/** What the model is told about running `dispatch` in a Codex session. */
export const CODEX_DISPATCH_NOTE =
  "This Codex session reaches Dispatch through the `dispatch` tool of the `envoy` MCP server, not a shell command: Codex puts no plugin command on the shell's PATH, and its sandbox keeps shell commands off the network. Wherever a skill shows `dispatch <command> <flags>`, call the `dispatch` tool with `args` set to the words after `dispatch`, as a list, and `stdin` set to what the command would read on standard input (a here-document's text). It runs the same command, as this session, and answers with what the command prints and its exit code."

async function sessionStart(raw: unknown): Promise<void> {
  const input = SessionStartInput.parse(raw)
  const pluginData = configuredValue(
    process.env["PLUGIN_DATA"] ?? process.env["CLAUDE_PLUGIN_DATA"],
  )
  if (pluginData === undefined) {
    process.stderr.write("envoy: PLUGIN_DATA is unset, so this session's id was not recorded\n")
  } else {
    const pid = codexProcessId()
    try {
      await writeCodexSessionRecord(codexSessionRecordFile(pluginData, pid), {
        codex_executable: codexExecutable(pid) ?? null,
        codex_home: codexHome(process.env),
        codex_started: processStarted(pid) ?? null,
        cwd: input.cwd,
        session_id: input.session_id,
        source: input.source ?? null,
        transcript_path: input.transcript_path ?? null,
      })
      await writeSessionHandoff(sessionHandoffFile(pluginData, pid), input.session_id)
    } catch (error) {
      process.stderr.write(`envoy: could not record the session id — ${messageFor(error)}\n`)
    }
  }

  const chosen = sessionConfigFile("codex", process.env)
  const { dispatch, warnings } = sessionDispatchSettings(chosen, process.env, {
    cwd: input.cwd,
    session: input.session_id,
  })
  for (const warning of warnings) process.stdout.write(`envoy: ${warning}\n`)
  if (!dispatch.enabled) {
    if (dispatch.error !== null) {
      process.stdout.write(`Dispatch authored-ask summary unavailable: ${dispatch.error}\n`)
    }
    return
  }
  process.stdout.write(`${CODEX_DISPATCH_NOTE}\n\n`)
  try {
    const snapshot = await new DispatchClient(
      dispatch.url,
      dispatch.credential,
      fetch,
      AbortSignal.timeout(OPEN_ASKS_TIMEOUT_MS),
    ).openAsks(input.session_id)
    process.stdout.write(
      `Dispatch authored-ask summary:\n${formatOpenAsksSummary(snapshot, dispatch.publicUrl ?? dispatch.url)}\n`,
    )
  } catch (error) {
    process.stdout.write(`Dispatch authored-ask summary unavailable: ${messageFor(error)}\n`)
  }
}

if (import.meta.main) {
  const [mode] = process.argv.slice(2)
  try {
    if (mode !== "session-start") {
      throw new Error(`unknown mode ${JSON.stringify(mode)}; codex/hooks.json names session-start`)
    }
    await sessionStart(JSON.parse(await Bun.stdin.text()))
  } catch (error) {
    process.stderr.write(`envoy: codex-hook: ${messageFor(error)}\n`)
  }
  process.exit(0)
}
