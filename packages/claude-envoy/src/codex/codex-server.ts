import { spawn } from "node:child_process"
import { unwatchFile, watchFile } from "node:fs"
import { rm } from "node:fs/promises"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { DELIVERY_CAPABILITIES } from "@legion/contracts"
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { sessionDirectory as dispatchSessionDirectory } from "@legion/envoy-client/dispatch-session-directory"
import { writeSessionTitle } from "@legion/envoy-client/dispatch-session-state"
import { messageFor } from "@legion/envoy-client/errors"
import {
  sessionConfigFile,
  sessionDispatchSettings,
  sessionSettings,
} from "@legion/envoy-client/session-settings"
import { createEnvoyClient, newClientInstanceID } from "@legion/envoy-client/transport"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { connect } from "nats"
import { z } from "zod"
/** The npm package a Codex session runs, which its registrations name. */
import { name as CODEX_PACKAGE_NAME } from "../../codex/package.json" with { type: "json" }
import {
  admittingMoves,
  channelReopener,
  closeUnstartedConnection,
  dialChannelInbound,
  sessionAdmission,
} from "../channel-connection"
import type { ChannelConnection } from "../channel-topics"
import { configuredValue } from "../claude-session"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  channelToolDefinitions,
  executeEnvoyTool,
  MCP_SERVER_INFO,
  mcpResult,
  postDispatchReply,
  startChannelSession,
} from "../envoy-channel-server"
import { inboundOffChannelSession } from "../envoy-off"
import { channelConnectionName, installedPluginVersion } from "../installed-build"
import { SessionIdentity } from "../session-identity"
import { sessionTitle } from "../session-title"
import { type CodexAgentStream, startCodexAgentStream } from "./codex-agent-stream"
import { askSideQuestion, type CodexCommand, codexProcessEnv, queueMessage } from "./codex-cli"
import { createCodexDelivery } from "./codex-delivery"
import { codexInboxDirectory, discardWaiting } from "./codex-inbox"
import {
  type CodexServerEntry,
  codexServersDirectory,
  hasStandbyServer,
  stillOwner,
  unregisterServer,
  waitForOwnership,
} from "./codex-owner"
import {
  type CodexSessionRecord,
  codexEndedFile,
  codexHome,
  codexPluginDataDirectory,
  codexSessionRecordFile,
  processStarted,
  readCodexSessionRecord,
  readSharedCodexState,
  recordOfProcess,
  recordSharedCodexRegistration,
  type SharedCodexState,
  sessionEnded,
  sharedAppServer,
  sharedCodexRecordActive,
  sharedCodexRecordFile,
  watchRecord,
} from "./codex-session-record"
import { threadIdShape } from "./codex-thread-id"
import { callSharedCodexTool, serveSharedCodexTools } from "./codex-tool-link"
import {
  findTranscript,
  followTranscript,
  latestTurnCwd,
  type TranscriptFollower,
  transcriptEvents,
} from "./codex-transcript"
import { createCodexUserTurnRouter } from "./codex-user-turns"

/**
 * The Envoy plugin's MCP server under the Codex CLI: `codex/.codex-plugin/plugin.json`
 * starts `dist/envoy-channel.js --host codex` in the plugin's own directory, as a child of the
 * `codex` process.
 *
 * It serves the Envoy tools and the `dispatch` tool at once, and starts the session's Envoy side
 * when the plugin's `SessionStart` hook records which session the `codex` process runs
 * (`./codex-session-record`), which Codex does at the session's first prompt. From then on it is
 * the Claude Code plugin's session code (`startChannelSession`) with Codex's parts: a line goes
 * into the session through `codex queue` or the `PostToolUse` hook (`./codex-delivery`), a
 * person's message through `./codex-user-turns`, and the live view comes from the session's
 * transcript (`./codex-agent-stream`). A `/new` moves it to the new session's id through the same
 * hand-off file a Claude Code `/clear` writes.
 *
 * Codex starts one such server per thread, so a `codex` process can have several (a `/new` starts
 * one beside the old thread's, and `/side` and `/btw` one each). Only the one that owns the
 * process's sessions (`./codex-owner`) runs the Envoy side. On a shared app server it keeps one
 * runtime per recorded thread, and the other servers forward Envoy tool calls to it.
 */

const CODEX_INSTRUCTIONS =
  "Envoy delivers trusted, internal session and Dispatch events into this conversation. Use the Envoy tools for Envoy actions, and reach Dispatch through this server's `dispatch` tool (the `dispatch` skill teaches its commands). Dispatch asks stay on Dispatch."

/** How often the session record is looked at while the server waits for it, and after. */
const RECORD_POLL_MS = 250

// Give the new follower two 500 ms polls to find a transcript created after SessionStart.
const TRANSCRIPT_CATCH_UP_MS = 1_000

/** The capabilities a Codex session advertises: all three, since every one has a way in. */
const CODEX_CAPABILITIES = DELIVERY_CAPABILITIES

const DispatchToolArguments = z.object({
  args: z.array(z.string()),
  stdin: z.string().optional(),
})

/** Why a standby server's Envoy tools do not run (`./codex-owner`). */
const STANDBY_NOTE =
  "another of this codex process's plugin servers serves its Envoy session, because Codex starts one server per thread and one of them owns the session; after /new, this thread's Envoy tools start once every server started before this one has exited, which for the previous thread's server is thread_unload_delay (60 s by default) after that thread went idle with no subscriber, and messages to this session arrive meanwhile; the dispatch tool works now"

/** How often the owning server checks it still owns the session. */
const OWNER_RECHECK_MS = 5_000

/**
 * The Codex thread whose turn made a tool call, from the request's `_meta`: Codex 0.154.0 sends
 * `threadId` on every MCP tool call, beside `x-codex-turn-metadata` naming the same thread
 * (measured). Undefined when the call names none.
 */
export function callingThread(meta: unknown): string | undefined {
  if (typeof meta !== "object" || meta === null) return undefined
  const named = (meta as { threadId?: unknown }).threadId
  const turn = (meta as { "x-codex-turn-metadata"?: { thread_id?: unknown } })[
    "x-codex-turn-metadata"
  ]
  const thread = typeof named === "string" && named !== "" ? named : turn?.thread_id
  // Checked once here: everything downstream (CODEX_THREAD_ID, the transcript search) takes it.
  return typeof thread === "string" && threadIdShape(thread) ? thread : undefined
}

/** The `dispatch` tool's definition: the command, as a tool that runs outside Codex's sandbox. */
export function dispatchToolDefinition() {
  return {
    description:
      'Runs the `dispatch` command as this session, outside Codex\'s sandbox. Pass the words after `dispatch` as `args` (for example ["read", "--issue", "LEGION-3"]) and what the command would read on standard input as `stdin` (the text of a here-document given as `--body-file -`). `["--help"]` lists the commands. Answers with what the command printed and its exit code. A command that a Dispatch spec defines exits 0 when it wrote or read. It exits 1 when Dispatch or AskGate refused it, Dispatch failed, the command could not reach Dispatch, its trade for a token at the secrets broker failed, or Dispatch kept no answer for a call that writes nothing (`ANSWER_NOT_KEPT`, which you run again). It exits 2 on a usage error or a session or configuration it cannot use. `dispatch proxy` and `dispatch login` give their own exit codes in their help.',
    inputSchema: {
      properties: {
        args: {
          description: "The command's words after `dispatch`.",
          items: { type: "string" },
          type: "array",
        },
        stdin: {
          description: "What the command reads on standard input, where a flag says `-`.",
          type: "string",
        },
      },
      required: ["args"],
      type: "object",
    },
    name: "dispatch",
  }
}

/** How long one `dispatch` tool call may run. */
const DISPATCH_TOOL_TIMEOUT_MS = 120_000

/** Runs the plugin's own `dispatch` command (`dist/dispatch.js`) with this server's Bun, outside
 *  Codex's sandbox, and answers its exit code and everything it printed, in order. */
export function runDispatchCommand(
  script: string,
  args: readonly string[],
  stdin: string,
  options: { readonly cwd: string; readonly env: Record<string, string> },
): Promise<{ readonly code: number; readonly output: string }> {
  return new Promise((done) => {
    const child = spawn(process.execPath, [script, ...args], {
      cwd: options.cwd,
      env: options.env,
      stdio: ["pipe", "pipe", "pipe"],
    })
    let output = ""
    const take = (chunk: Buffer): void => {
      output += chunk.toString()
    }
    child.stdout.on("data", take)
    child.stderr.on("data", take)
    const timer = setTimeout(() => child.kill("SIGKILL"), DISPATCH_TOOL_TIMEOUT_MS)
    child.on("error", (error) => {
      clearTimeout(timer)
      done({ code: 2, output: `dispatch: could not run ${script}: ${error.message}\n` })
    })
    child.on("close", (code, signal) => {
      clearTimeout(timer)
      done({
        code: code ?? 2,
        output: signal === null ? output : `${output}dispatch: stopped by ${signal}\n`,
      })
    })
    // A command that exits without reading all of a long stdin raises EPIPE here, which
    // would otherwise end this server; the command's own exit code is the answer.
    child.stdin.on("error", () => undefined)
    child.stdin.end(stdin)
  })
}

/** The plugin's state directory: the one Codex hands its hooks, worked out from where it
 *  installed the plugin, else a directory of the plugin's own under the person's state home. */
function stateDirectoryFor(pluginRoot: string): string {
  return (
    configuredValue(process.env["PLUGIN_DATA"]) ??
    codexPluginDataDirectory(pluginRoot) ??
    join(
      configuredValue(process.env["XDG_STATE_HOME"]) ?? join(homedir(), ".local", "state"),
      "claude-envoy",
      "codex",
    )
  )
}

/** Resolves with the record once the `SessionStart` hook has written it for a session that has not
 *  ended, polling the file. */
function waitForRecord(
  file: string,
  pid: number,
  signal: AbortSignal,
  ended: (sessionId: string) => Promise<boolean>,
): Promise<CodexSessionRecord | undefined> {
  return new Promise((done) => {
    const look = (): void => {
      void readCodexSessionRecord(file).then(async (record) => {
        // A record an earlier process with the same id left behind names no session of this one,
        // and a session that has ended is not registered again (a takeover after `/new`, before
        // the new thread's first prompt).
        if (record === undefined || !recordOfProcess(record, pid)) return
        if (await ended(record.session_id)) return
        unwatchFile(file, look)
        done(record)
      })
    }
    watchFile(file, { interval: RECORD_POLL_MS }, look)
    signal.addEventListener("abort", () => {
      unwatchFile(file, look)
      done(undefined)
    })
    look()
  })
}

export async function runCodexEnvoyServer(): Promise<void> {
  const pluginRoot = process.cwd()
  const stateDirectory = stateDirectoryFor(pluginRoot)
  const codexPid = process.ppid
  const shared = sharedAppServer(codexPid)
  const recordFile = codexSessionRecordFile(stateDirectory, codexPid)
  const serversDirectory = codexServersDirectory(stateDirectory, codexPid)
  /** This server's entry, once `waitForOwnership` published it. */
  let self: CodexServerEntry | undefined
  let recheck: ReturnType<typeof setInterval> | undefined
  /** Whether session `sessionId` of this process has ended (`SessionEnd`'s marker). */
  const endedNow = (sessionId: string): Promise<boolean> =>
    sessionEnded(codexEndedFile(stateDirectory, codexPid, sessionId))
  const configFile = sessionConfigFile("codex", process.env)
  const warn = (text: string): void => void process.stderr.write(`envoy-channel: ${text}\n`)
  const bundlePath = resolve(pluginRoot, process.argv[1] ?? "")

  let legacy: CodexThreadRuntime | undefined
  let notReady = "this session's Envoy side starts at its first prompt"
  const stopping = new AbortController()

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {} },
    instructions: CODEX_INSTRUCTIONS,
  })
  const tools = [...channelToolDefinitions(), dispatchToolDefinition()]
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }))
  server.setRequestHandler(CallToolRequestSchema, async (request, { signal }) => {
    const thread = callingThread(request.params._meta)
    if (request.params.name === "dispatch") {
      return {
        content: [
          { text: await runDispatchTool(request.params.arguments, thread), type: "text" as const },
        ],
      }
    }
    const { name, arguments: args } = request.params
    if (shared) {
      if (thread === undefined) throw new Error("Codex did not identify the calling thread")
      return mcpResult(
        await callSharedCodexTool(
          serversDirectory,
          { thread, name, args },
          AbortSignal.any([stopping.signal, signal]),
        ),
      )
    }
    if (legacy === undefined) throw new Error(`Envoy is not ready: ${notReady}`)
    return mcpResult(await legacy.call(name, args))
  })

  /** Runs the `dispatch` command in this process, as `thread`, the Codex thread whose turn made the
   *  call, in the directory that thread's newest turn ran in, and answers what it printed and its
   *  exit code. Codex names the thread on every tool call (`callingThread`), so a fork or a second
   *  session of the same process acts as itself and never as the session Envoy registered. */
  const runDispatchTool = async (raw: unknown, thread: string | undefined): Promise<string> => {
    const parsed = DispatchToolArguments.safeParse(raw)
    if (!parsed.success)
      return `dispatch: the tool takes args (a list of strings) and an optional stdin\n[exit 2]`
    // The record, read now when the server has not seen it yet: the hook writes it before the turn's
    // first request, and the server reads it on a poll.
    const read = shared
      ? thread === undefined
        ? undefined
        : await readCodexSessionRecord(sharedCodexRecordFile(stateDirectory, codexPid, thread))
      : await readCodexSessionRecord(recordFile)
    const current = read !== undefined && recordOfProcess(read, codexPid) ? read : undefined
    // A Codex that names no thread on the call (older than 0.154.0) acts as the record's session,
    // which is the one session of a process this plugin registers; under the shared app server
    // there is no such session to fall back on.
    const sessionId = thread ?? current?.session_id
    if (sessionId === undefined) {
      return shared
        ? "dispatch: Codex did not identify the calling thread\n[exit 2]"
        : `dispatch: this session has no id yet; Codex records it at the session's first prompt\n[exit 2]`
    }
    const own = current?.session_id === sessionId ? current : undefined
    const home = own?.codex_home ?? codexHome(process.env)
    const transcript = own?.transcript_path ?? (await findTranscript(home, sessionId))
    const env: Record<string, string> = {}
    for (const [name, value] of Object.entries(process.env)) {
      if (value !== undefined) env[name] = value
    }
    env["CODEX_HOME"] = home
    env["CODEX_THREAD_ID"] = sessionId
    env["DISPATCH_HOST"] = "codex"
    if (configFile.path !== null) env["ENVOY_CONFIG_FILE"] = configFile.path
    const { code, output } = await runDispatchCommand(
      join(pluginRoot, "dist", "dispatch.js"),
      parsed.data.args,
      parsed.data.stdin ?? "",
      { cwd: (await latestTurnCwd(transcript)) ?? own?.cwd ?? current?.cwd ?? homedir(), env },
    )
    return `${output}${output.endsWith("\n") || output === "" ? "" : "\n"}[exit ${code}]`
  }

  const stop = (): void => stopping.abort()
  const signals = ["SIGTERM", "SIGINT", "SIGHUP"] as const
  process.stdin.once("end", stop)
  for (const signal of signals) process.once(signal, stop)

  try {
    await server.connect(new StdioServerTransport())
    // One server per codex process owns its Envoy session (`./codex-owner`): Codex starts one per
    // thread, and the others stand by, serving their thread's `dispatch` calls, until every server
    // older than them is gone.
    notReady = STANDBY_NOTE
    const owning = await waitForOwnership(
      serversDirectory,
      process.pid,
      processStarted(process.pid) ?? null,
      stopping.signal,
      (entry) => {
        self = entry
      },
    )
    if (owning === undefined) return
    notReady =
      "this session's Envoy side starts once Codex records the session at its first prompt; a fork or an ephemeral session, which keeps no transcript, is never registered"
    // The owner looks again every few seconds and warns if it finds another owner, which the order
    // of publication rules out while every entry is readable.
    const owned = owning
    recheck = setInterval(() => {
      void stillOwner(serversDirectory, owned).then(
        (still) => {
          if (!still)
            warn(
              `another plugin server of codex process ${codexPid} owns its Envoy session as well`,
            )
        },
        () => undefined,
      )
    }, OWNER_RECHECK_MS)
    recheck.unref?.()
    const options = { stateDirectory, codexPid, configFile, bundlePath, warn }
    if (shared) {
      await runSharedCodexOwner(serversDirectory, owning, options, stopping.signal)
    } else {
      const record = await waitForRecord(recordFile, codexPid, stopping.signal, endedNow)
      if (record === undefined) return
      legacy = startCodexThreadRuntime(record, options, { recordFile, endedNow })
      const abort = (): void => legacy?.stop()
      stopping.signal.addEventListener("abort", abort, { once: true })
      if (stopping.signal.aborted) abort()
      await legacy.done
    }
  } finally {
    clearInterval(recheck)
    process.stdin.off("end", stop)
    for (const signal of signals) process.off(signal, stop)
    if (self !== undefined) await unregisterServer(serversDirectory, self).catch(() => undefined)
    await server.close().catch(() => undefined)
  }
}

interface CodexRuntimeOptions {
  readonly stateDirectory: string
  readonly codexPid: number
  readonly configFile: ReturnType<typeof sessionConfigFile>
  readonly bundlePath: string
  readonly warn: (text: string) => void
}

interface CodexThreadRuntime {
  readonly done: Promise<void>
  readonly stop: (retainRegistration?: () => Promise<boolean>) => void
  readonly call: (tool: string, args: unknown) => Promise<unknown>
}

async function runSharedCodexOwner(
  serversDirectory: string,
  owner: CodexServerEntry,
  options: CodexRuntimeOptions,
  signal: AbortSignal,
): Promise<void> {
  const { stateDirectory, codexPid, warn } = options
  const started = processStarted(codexPid)
  if (started === undefined) {
    warn(`the start of codex process ${codexPid} cannot be read; its threads cannot register`)
    return
  }
  const threads = new Map<string, { generation: string; runtime: CodexThreadRuntime }>()
  const closeTools = await serveSharedCodexTools(
    serversDirectory,
    owner,
    async ({ thread, name, args }) => {
      const slot = threads.get(thread)
      if (slot !== undefined) return slot.runtime.call(name, args)
      throw new Error("Envoy is not ready: the calling Codex thread has no active session")
    },
  )
  const stopThreads = (): void => {
    for (const [thread, { generation, runtime }] of threads) {
      runtime.stop(async () => {
        // An unloaded thread must still leave the registry. A loaded sibling keeps its route,
        // consumer and role until a live standby re-registers it with a fresh client instance.
        if (!signal.aborted || !(await hasStandbyServer(serversDirectory, owner))) return false
        if (processStarted(codexPid) !== started) return false
        return sharedCodexRecordActive(
          sharedCodexRecordFile(stateDirectory, codexPid, thread),
          generation,
        )
      })
    }
  }
  let cleanup: Promise<void> | undefined
  signal.addEventListener("abort", stopThreads, { once: true })
  try {
    while (!signal.aborted) {
      const state = await readSharedCodexState(stateDirectory, codexPid, started).catch(
        (error: unknown) => {
          warn(`could not read Codex thread records: ${messageFor(error)}`)
          return undefined
        },
      )
      if (signal.aborted) break
      if (state !== undefined) {
        for (const [thread, slot] of threads) {
          if (state.live.get(thread)?.generation !== slot.generation) slot.runtime.stop()
        }
        for (const [thread, record] of state.live) {
          // A new load of the same id waits until the old runtime releases its relay and inbox.
          if (threads.has(thread)) continue
          const runtime = startCodexThreadRuntime(record, options)
          const slot = { generation: record.generation, runtime }
          threads.set(thread, slot)
          void runtime.done
            .catch((error: unknown) => {
              warn(`Codex thread ${thread}: ${messageFor(error)}`)
            })
            .finally(() => threads.delete(thread))
        }
        if (cleanup === undefined) {
          const ended = state.endedRegistrations.filter(
            ({ record }) => !threads.has(record.session_id),
          )
          if (ended.length > 0) {
            cleanup = cleanEndedSharedRegistrations(ended, options).finally(() => {
              cleanup = undefined
            })
          }
        }
      }
      await delay(RECORD_POLL_MS, undefined, { signal }).catch((error: unknown) => {
        if (!signal.aborted) throw error
      })
    }
  } finally {
    signal.removeEventListener("abort", stopThreads)
    stopThreads()
    await Promise.allSettled([
      closeTools(),
      cleanup,
      ...[...threads.values()].map(({ runtime }) => runtime.done),
    ])
  }
}

/** A successor cleans registrations the previous owner left when a thread was still loaded.
 *  Each DELETE names the recorded instance, so a late cleanup cannot remove a resumed thread. */
async function cleanEndedSharedRegistrations(
  registrations: SharedCodexState["endedRegistrations"],
  { configFile, warn }: CodexRuntimeOptions,
): Promise<void> {
  await Promise.all(
    registrations.map(async ({ record, clientInstanceID, file }) => {
      try {
        const { envoy } = await sessionSettings(configFile, process.env, {
          cwd: record.cwd,
          session: () => record.session_id,
        })
        if (!envoy.enabled) return
        const client = createEnvoyClient({
          baseUrl: envoy.url,
          credential: envoy.credential,
          fetch,
        })
        // Allow a fresh credential while leaving time for the Agents roster's five-second cache.
        await client.unregisterSession({
          sessionID: record.session_id,
          clientInstanceID,
          timeoutMs: 1_000,
        })
        await rm(file, { force: true })
      } catch (error) {
        warn(`could not deregister ended Codex thread ${record.session_id}: ${messageFor(error)}`)
      }
    }),
  )
}

/** One registration, relay, transcript and queue target. Only the legacy process runtime can
 *  change thread id via a `/new` hand-off. */
function startCodexThreadRuntime(
  bound: CodexSessionRecord,
  { stateDirectory, codexPid, configFile, bundlePath, warn }: CodexRuntimeOptions,
  legacy?: { recordFile: string; endedNow: (thread: string) => Promise<boolean> },
): CodexThreadRuntime {
  let record = bound
  let runtime: ChannelToolRuntime | undefined
  let notReady = "the calling Codex thread is starting"
  let session: ChannelSession | undefined
  let connection: ChannelConnection | undefined
  let stream: CodexAgentStream | undefined
  let stopNatsKeeper: () => void = () => undefined
  let stopWatchingRecord: () => void = () => undefined
  let follower: TranscriptFollower | undefined
  let delivery: ReturnType<typeof createCodexDelivery> | undefined
  const stopping = new AbortController()
  const stopped = Promise.withResolvers<void>()
  let retainRegistration: (() => Promise<boolean>) | undefined
  const stop = (retain?: () => Promise<boolean>): void => {
    if (stopping.signal.aborted) return
    retainRegistration = retain
    stopping.abort()
    delivery?.sessionEnded()
    stopped.resolve()
  }
  const generation = bound.generation
  const requireActive = async (): Promise<void> => {
    stopping.signal.throwIfAborted()
    if (generation === undefined) return
    const file = sharedCodexRecordFile(stateDirectory, codexPid, bound.session_id)
    if (!(await sharedCodexRecordActive(file, generation))) {
      stop()
      throw new Error("the calling Codex thread has ended")
    }
    stopping.signal.throwIfAborted()
  }

  const run = async (): Promise<void> => {
    /** Whether the session the record names has ended, as its marker last said. */
    let recordEnded = false
    // Lines an earlier owner of this session left waiting: that server is gone, so no write waits on
    // them, and Envoy sends each one's frame again.
    await discardWaiting(codexInboxDirectory(stateDirectory, bound.session_id))
    /** Whether the session code, the live view and the transcript follower have started; a `/new`
     *  before then is applied once they have. */
    let ready = false
    /** Moves this server's Codex parts to the session `next` names (`/new`): the delivery's turn
     *  state and waiting lines, the live view, and the transcript followed. The Envoy session code
     *  follows the hand-off file the hook wrote beside the record on its own. */
    let moveTo: (next: CodexSessionRecord) => void = () => undefined
    // Watched from now, so a `/new` while the session starts is seen, and read once at once, so an
    // end marked between the bind's own read and this watch is seen too.
    if (legacy !== undefined)
      stopWatchingRecord = watchRecord(
        legacy.recordFile,
        codexPid,
        legacy.endedNow,
        (next, ended) => {
          if (next.session_id === record.session_id) {
            // The same session ended (`SessionEnd`), or runs again (`/resume`). Before the delivery
            // exists, its creation reads `recordEnded` instead.
            if (ended !== recordEnded) {
              recordEnded = ended
              if (ended) delivery?.sessionEnded()
              else
                void delivery?.sessionResumed().catch((error: unknown) => warn(messageFor(error)))
            }
            return
          }
          record = next
          recordEnded = ended
          if (ready) moveTo(next)
        },
        RECORD_POLL_MS,
      )
    const id = new SessionIdentity(bound.session_id, bound.cwd)
    const { dispatch, envoy, notice } = await sessionSettings(configFile, process.env, {
      cwd: bound.cwd,
      session: () => id.id,
    })
    if (notice !== null) warn(notice)
    const title = await sessionTitle(
      legacy === undefined
        ? {}
        : {
            ENVOY_SESSION_TITLE: process.env["ENVOY_SESSION_TITLE"],
            TMUX: process.env["TMUX"],
            TMUX_PANE: process.env["TMUX_PANE"],
          },
      bound.cwd,
    )
    const stampTitle = (sessionId: string, value: string): void => {
      writeSessionTitle(dispatchSessionDirectory(process.env, sessionId), value)
    }
    if (!envoy.enabled) {
      notReady = `Envoy is off for this session: ${envoy.error ?? envoy.notice}`
      if (dispatch.enabled) stampTitle(id.id, title(id.id))
      await stopped.promise
      return
    }

    const command = (): CodexCommand => ({
      codexHome: record.codex_home,
      // Codex's own environment, not this server's: Codex gave this server only `env_vars`.
      env: codexProcessEnv(codexPid, process.env),
      executable: record.codex_executable ?? "codex",
    })
    const client = createEnvoyClient({
      baseUrl: envoy.url,
      credential: envoy.credential,
      fetch: globalThis.fetch,
    })
    const clientInstanceID = newClientInstanceID()
    /** The session the `codex` process runs now, as its record names it: what `codex queue` and the
     *  inbox are addressed by, which moves at `/new` before the Envoy session code adopts the new
     *  id from the hand-off file. */
    const runningSession = (): string => record.session_id
    const codexDelivery = createCodexDelivery({
      inboxDirectory: (sessionId) => codexInboxDirectory(stateDirectory, sessionId),
      queue: async (text) => {
        if (legacy === undefined) await requireActive()
        await queueMessage(
          command(),
          runningSession(),
          text,
          join(stateDirectory, "codex-long-messages"),
        )
      },
      sessionId: runningSession,
      taken: (item) => stream?.delivered(item.content, item.dispatchMessageId),
      warn,
    })
    delivery = codexDelivery
    const deliver = async (item: Parameters<typeof codexDelivery.deliver>[0]): Promise<void> => {
      if (legacy === undefined) {
        // A new owner may register a thread that is already in a turn. Classify its first
        // delivery after replaying that turn, without delaying its registration.
        if (follower !== undefined) {
          const timer = new AbortController()
          try {
            const caughtUp = await Promise.race([
              follower.caughtUp.then(() => true),
              delay(TRANSCRIPT_CATCH_UP_MS, false, {
                signal: AbortSignal.any([stopping.signal, timer.signal]),
              }),
            ])
            if (!caughtUp) throw new Error("the Codex transcript is still being read")
          } finally {
            timer.abort()
          }
        }
        await requireActive()
      }
      await codexDelivery.deliver(item)
    }
    // An end the watcher saw before the delivery existed.
    if (recordEnded) codexDelivery.sessionEnded()

    /** Reads the transcript of the session now bound into the delivery's turn state and the live
     *  view, from its first line; a `/new` starts this again on the new session's transcript. */
    const follow = (path: string | null): void => {
      follower?.stop()
      follower = undefined
      if (path === null) return
      follower = followTranscript(
        path,
        (line) => {
          for (const event of transcriptEvents(line)) {
            if (event.kind === "turn-started") codexDelivery.turnStarted()
            else if (event.kind === "turn-ended") {
              void codexDelivery.turnEnded().catch((error: unknown) => warn(messageFor(error)))
            }
            stream?.event(event)
          }
        },
        { warn },
      )
    }

    moveTo = (next) => {
      void codexDelivery.rebind(recordEnded).catch((error: unknown) => warn(messageFor(error)))
      stream?.rebind(next.session_id)
      follow(next.transcript_path)
    }

    await requireActive()
    const { inbound } = envoy
    if ("off" in inbound) {
      session = inboundOffChannelSession({ identity: id, stateDirectory, why: inbound.off })
    } else {
      // Dialled as the Claude Code session's connection is (`dialChannelInbound`): NATS with a
      // seed, no credential, or the session's broker token, which is then kept renewed and, at a
      // `/new`, moved to the new session's (`followSession`) before that session's subjects are
      // taken; or the listener's relay socket, opened again for the new session at a `/new`. A
      // connection that closes for good is replaced as the Claude Code one is (`channelReopener`):
      // Codex starts no plugin server again for a thread whose server lost its connection.
      const natsName = channelConnectionName(
        `codex-envoy-channel-${id.id}`,
        bundlePath,
        CODEX_PACKAGE_NAME,
      )
      // A stop asked for while the token or the dial is awaited ends the start, and is no failure:
      // the process ends as an asked-for stop does, as the Claude Code server's does.
      const dialled = await dialChannelInbound(
        inbound,
        envoy.url,
        () => id.id,
        natsName,
        warn,
        connect,
        stopping.signal,
      )
      connection = dialled.connection
      stopNatsKeeper = dialled.stop
      await requireActive()
      // Which id the connection is admitted as, where its broker token admits one session at a
      // time: the dial's, then each `/new`'s once its move lands. The live view takes a new id's
      // control subject only then (`CodexAgentStreamOptions.admitted`).
      const admission = dialled.followSession === undefined ? undefined : sessionAdmission(id.id)
      const admitting = admittingMoves(admission, id)
      const followSession = admitting(dialled.followSession)
      const reopen = channelReopener({
        inbound,
        listenerUrl: envoy.url,
        session: () => id.id,
        name: natsName,
        signal: stopping.signal,
        swapKeeper: (stop) => {
          stopNatsKeeper()
          stopNatsKeeper = stop
        },
        admitting,
        useConnection: (next) => {
          connection = next
          stream?.useConnection(next)
        },
      })
      stream = startCodexAgentStream({
        connection,
        liveView: dialled.liveView,
        sessionId: id.id,
        warn,
        ...(admission === undefined
          ? {}
          : { admitted: (next: string) => admission.admitted(next) }),
      })
      // Learn the turn state before accepting forwarded deliveries through the listener.
      follow(bound.transcript_path)
      if (generation !== undefined) {
        await recordSharedCodexRegistration(
          sharedCodexRecordFile(stateDirectory, codexPid, bound.session_id),
          generation,
          clientInstanceID,
        )
        await requireActive()
      }
      const started = await startChannelSession({
        capabilities: () => CODEX_CAPABILITIES,
        client,
        clientInstanceID,
        configFile,
        connection,
        ...(legacy === undefined ? {} : { handoffPid: codexPid }),
        heartbeatMs: envoy.heartbeatMs,
        identity: id,
        ...(followSession === undefined ? {} : { followSession }),
        ...(dialled.reportOutagesTo === undefined
          ? {}
          : { reportOutagesTo: dialled.reportOutagesTo }),
        notifier: {
          notification: ({ content, eventId }) =>
            deliver({ content, ...(eventId === undefined ? {} : { eventId }) }),
        },
        pluginVersion: installedPluginVersion(bundlePath) ?? "source",
        pluginName: CODEX_PACKAGE_NAME,
        router: (state) =>
          createCodexUserTurnRouter({
            ask: async (question) => {
              if (legacy === undefined) await requireActive()
              return askSideQuestion(command(), id.id, question)
            },
            deliver,
            dispatch: async (ms) => {
              const active = activeDispatchSettings(
                (await sessionDispatchSettings(configFile, process.env, { cwd: bound.cwd }))
                  .dispatch,
              )
              return active === null
                ? undefined
                : new DispatchClient(active.url, active.credential, fetch, {
                    signal: AbortSignal.timeout(ms),
                  })
            },
            expect: (text, messageId) => stream?.expect(text, messageId),
            reply: (target, result) => postDispatchReply(id, configFile, target, result),
            sessionId: () => id.id,
            state,
            warn,
          }),
        reopen,
        // Ends a start still flushing through its connection's redials (`flushThroughRedials`):
        // the process is stopping.
        startSignal: stopping.signal,
        stateDirectory,
        title,
        ...(dispatch.enabled ? { stampTitle } : {}),
      })
      session = started
      await requireActive()
    }
    runtime = { client, clientInstanceID, identity: id, session }
    if (follower === undefined) follow(bound.transcript_path)

    ready = true
    // A `/new` the record watcher saw while the session was starting.
    if (record.session_id !== bound.session_id) moveTo(record)
    await stopped.promise
  }
  const done = run()
    .catch((error: unknown) => {
      if (!stopping.signal.aborted) throw error
    })
    .finally(async () => {
      stop()
      stopWatchingRecord()
      follower?.stop()
      stream?.stop()
      stopNatsKeeper()
      // Finish both teardowns before this owner's successor can drain the inbox.
      const settled = await Promise.allSettled([
        delivery?.stop(),
        session?.shutdown(retainRegistration),
      ])
      if (connection !== undefined) {
        if (session === undefined) await closeUnstartedConnection(connection)
        else await connection.close().catch(() => undefined)
      }
      for (const result of settled) {
        if (result.status === "rejected") throw result.reason
      }
    })
  return {
    done,
    stop,
    async call(tool, args) {
      await requireActive()
      if (runtime === undefined) throw new Error(`Envoy is not ready: ${notReady}`)
      return executeEnvoyTool(runtime, tool, args)
    },
  }
}
