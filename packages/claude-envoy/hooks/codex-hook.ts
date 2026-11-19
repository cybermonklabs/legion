// The Codex CLI's `SessionStart` and `SessionEnd` hooks for the Envoy plugin:
// `codex/hooks/hooks.json` runs `dist/codex-hook.js session-start` at a session's first prompt and
// again after `/new`, and `dist/codex-hook.js session-end` when Codex unloads a thread.
//
// At SessionStart it records which session the `codex` process now runs, where the plugin's MCP
// servers read it (`../src/codex/codex-session-record`): they are that process's children too, but
// Codex hands them no session id. It writes the record Claude Code's hook writes as well
// (`sessions/<pid>/session-id`), so the server that owns the process's session
// (`../src/codex/codex-owner`) follows `/new` to the new id the way it follows a Claude Code
// `/clear`. Then it tells the model this session's open Dispatch asks and how to run `dispatch`
// here, as plain output, which Codex adds to the model's context (measured on 0.154.0). The
// dispatch-first skill and the answers list come from `dist/session-hook.js`, which
// `codex/hooks/hooks.json` runs beside this. At SessionEnd it leaves the session's ended marker.
// It always exits 0: a session must start without Envoy.

import { rm } from "node:fs/promises"
import { formatOpenAsksSummary } from "@legion/envoy-client/dispatch-execute"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionDispatchSettings } from "@legion/envoy-client/session-settings"
import { z } from "zod"
import { configuredValue } from "../src/claude-session"
import {
  codexEndedFile,
  codexExecutable,
  codexHome,
  codexProcessId,
  codexSessionRecordFile,
  markSessionEnded,
  processStarted,
  pruneCodexSessionRecords,
  readCodexSessionRecord,
  recordOfProcess,
  SHARED_APP_SERVER_NOTE,
  sharedAppServer,
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

/** What a session that keeps no transcript is told about Envoy. */
const NO_TRANSCRIPT_NOTE =
  "This Codex session keeps no transcript (a fork such as /side or /btw, or an ephemeral session), so the Envoy plugin does not register it with Envoy: it receives no Envoy messages and does not show on Dispatch's Agents page. Its `dispatch` tool works, acting as this session."

const SessionEndInput = z.object({ session_id: z.string().min(1) })

/**
 * `SessionEnd`, which Codex runs when it unloads a thread (`thread_unload_delay`, 60 s by default,
 * after the thread is idle with no subscriber) or at exit: leaves the session's ended marker when the
 * process's record names it, so the server delivers nothing more to it until the next
 * `SessionStart` names the session that follows. It writes a file of its own and never the record,
 * which a `SessionStart` may be writing at that moment. The end of a fork leaves nothing.
 */
async function sessionEnd(raw: unknown): Promise<void> {
  const input = SessionEndInput.parse(raw)
  const pluginData = configuredValue(
    process.env["PLUGIN_DATA"] ?? process.env["CLAUDE_PLUGIN_DATA"],
  )
  if (pluginData === undefined) return
  const pid = codexProcessId()
  const file = codexSessionRecordFile(pluginData, pid)
  const record = await readCodexSessionRecord(file)
  if (record === undefined || record.session_id !== input.session_id) return
  if (!recordOfProcess(record, pid)) return
  await markSessionEnded(pluginData, pid, input.session_id)
}

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
      await pruneCodexSessionRecords(pluginData)
    } catch (error) {
      process.stderr.write(`envoy: could not prune old session records — ${messageFor(error)}\n`)
    }
    try {
      const started = processStarted(pid)
      if (sharedAppServer(pid)) {
        // No record: two sessions of one process would share it (`sharedAppServer`).
        process.stdout.write(`${SHARED_APP_SERVER_NOTE}\n\n`)
      } else if (input.transcript_path === null || input.transcript_path === undefined) {
        // A fork (`/side`, `/btw`) or an ephemeral session (`codex exec --ephemeral`): it keeps no
        // transcript, recording a fork would move its session's registration to it, and Codex
        // refuses `codex queue` for an ephemeral thread, so nothing could reach it.
        process.stdout.write(`${NO_TRANSCRIPT_NOTE}\n\n`)
      } else if (started === undefined) {
        process.stderr.write(
          `envoy: this session was not recorded, because the start of codex process ${pid} cannot be read; it gets no Envoy messages\n`,
        )
      } else {
        // The session again (`/resume`): it has not ended. The marker goes before the record is
        // written, so a server that reads the new record never reads the old marker beside it.
        await rm(codexEndedFile(pluginData, pid, input.session_id), { force: true })
        await writeCodexSessionRecord(codexSessionRecordFile(pluginData, pid), {
          codex_executable: codexExecutable(pid) ?? null,
          codex_home: codexHome(process.env),
          codex_started: started,
          cwd: input.cwd,
          session_id: input.session_id,
          source: input.source ?? null,
          transcript_path: input.transcript_path ?? null,
        })
        await writeSessionHandoff(sessionHandoffFile(pluginData, pid), input.session_id)
      }
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
    const snapshot = await new DispatchClient(dispatch.url, dispatch.credential, fetch, {
      signal: AbortSignal.timeout(OPEN_ASKS_TIMEOUT_MS),
    }).openAsks(input.session_id)
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
    if (mode === "session-start") await sessionStart(JSON.parse(await Bun.stdin.text()))
    else if (mode === "session-end") await sessionEnd(JSON.parse(await Bun.stdin.text()))
    else {
      throw new Error(
        `unknown mode ${JSON.stringify(mode)}; codex/hooks/hooks.json names session-start and session-end`,
      )
    }
  } catch (error) {
    process.stderr.write(`envoy: codex-hook: ${messageFor(error)}\n`)
  }
  process.exit(0)
}
