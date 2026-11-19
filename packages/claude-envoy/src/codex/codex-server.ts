import { spawn } from "node:child_process"
import { unwatchFile, watchFile } from "node:fs"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
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
import type { ChannelConnection } from "../channel-topics"
import { configuredValue } from "../claude-session"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  channelToolDefinitions,
  dialChannelNats,
  executeEnvoyTool,
  MCP_SERVER_INFO,
  mcpResult,
  postDispatchReply,
  sessionAdmission,
  startChannelSession,
} from "../envoy-channel-server"
import { inboundOffChannelSession } from "../envoy-off"
import { installedPluginVersion } from "../installed-build"
import { SessionIdentity } from "../session-identity"
import { sessionTitle } from "../session-title"
import { type CodexAgentStream, startCodexAgentStream } from "./codex-agent-stream"
import { askSideQuestion, type CodexCommand, codexProcessEnv, queueMessage } from "./codex-cli"
import { createCodexDelivery } from "./codex-delivery"
import { codexInboxDirectory, discardWaiting } from "./codex-inbox"
import {
  type CodexServerEntry,
  codexServersDirectory,
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
  recordOfProcess,
  SHARED_APP_SERVER_NOTE,
  sessionEnded,
  sharedAppServer,
  watchRecord,
} from "./codex-session-record"
import {
  findTranscript,
  followTranscript,
  latestTurnCwd,
  type TranscriptFollower,
  threadIdShape,
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
 * process's session (`./codex-owner`) runs the Envoy side; the others serve their thread's
 * `dispatch` calls and take the session over once the owner is gone.
 */

const CODEX_INSTRUCTIONS =
  "Envoy delivers trusted, internal session and Dispatch events into this conversation. Use the Envoy tools for Envoy actions, and reach Dispatch through this server's `dispatch` tool (the `dispatch` skill teaches its commands). Dispatch asks stay on Dispatch."

/** How often the session record is looked at while the server waits for it, and after. */
const RECORD_POLL_MS = 250

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
      'Runs the `dispatch` command as this session, outside Codex\'s sandbox. Pass the words after `dispatch` as `args` (for example ["read", "--issue", "LEGION-3"]) and what the command would read on standard input as `stdin` (the text of a here-document given as `--body-file -`). `["--help"]` lists the commands. Answers with what the command printed and its exit code: 0 written or read, 1 refused by Dispatch, 2 a usage error.',
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
  /** Codex's shared app server runs several sessions in this one process, whose Envoy traffic this
   *  plugin cannot tell apart, so it registers none of them (`sharedAppServer`); `dispatch` still
   *  acts as the thread that calls it. */
  const shared = sharedAppServer(codexPid)
  const recordFile = codexSessionRecordFile(stateDirectory, codexPid)
  const serversDirectory = codexServersDirectory(stateDirectory, codexPid)
  /** This server's entry, once `waitForOwnership` published it. */
  let self: CodexServerEntry | undefined
  let stopWatchingRecord: () => void = () => undefined
  let recheck: ReturnType<typeof setInterval> | undefined
  /** Whether session `sessionId` of this process has ended (`SessionEnd`'s marker). */
  const endedNow = (sessionId: string): Promise<boolean> =>
    sessionEnded(codexEndedFile(stateDirectory, codexPid, sessionId))
  const configFile = sessionConfigFile("codex", process.env)
  const warn = (text: string): void => void process.stderr.write(`envoy-channel: ${text}\n`)
  const bundlePath = resolve(pluginRoot, process.argv[1] ?? "")

  let record: CodexSessionRecord | undefined
  let runtime: ChannelToolRuntime | undefined
  let notReady = "this session's Envoy side starts at its first prompt"
  const stopping = new AbortController()

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {} },
    instructions: CODEX_INSTRUCTIONS,
  })
  const tools = [...channelToolDefinitions(), dispatchToolDefinition()]
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }))
  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    if (request.params.name === "dispatch") {
      const thread = callingThread(request.params._meta)
      return {
        content: [
          { text: await runDispatchTool(request.params.arguments, thread), type: "text" as const },
        ],
      }
    }
    if (runtime === undefined) throw new Error(`Envoy is not ready: ${notReady}`)
    return mcpResult(await executeEnvoyTool(runtime, request.params.name, request.params.arguments))
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
    const read = shared ? undefined : (record ?? (await readCodexSessionRecord(recordFile)))
    const current = read !== undefined && recordOfProcess(read, codexPid) ? read : undefined
    // A Codex that names no thread on the call (older than 0.154.0) acts as the record's session,
    // which is the one session of a process this plugin registers; under the shared app server
    // there is no such session to fall back on.
    const sessionId = thread ?? current?.session_id
    if (sessionId === undefined) {
      return shared
        ? `dispatch: ${SHARED_APP_SERVER_NOTE}\n[exit 2]`
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

  let session: ChannelSession | undefined
  let connection: ChannelConnection | undefined
  let stream: CodexAgentStream | undefined
  /** Stops the keeper of the connection's broker token, once one is dialled. */
  let stopNatsKeeper: () => void = () => undefined
  let follower: TranscriptFollower | undefined
  let delivery: ReturnType<typeof createCodexDelivery> | undefined

  const stopped = Promise.withResolvers<void>()
  let isStopping = false
  const stop = (): void => {
    if (isStopping) return
    isStopping = true
    stopping.abort()
    stopWatchingRecord()
    clearInterval(recheck)
    follower?.stop()
    stream?.stop()
    stopNatsKeeper()
    // The entry goes once the delivery's waiting lines are withdrawn, so a server that takes over
    // never discards a line this one still answers for.
    const withdrawn = (delivery?.stop() ?? Promise.resolve()).finally(() =>
      self === undefined
        ? undefined
        : unregisterServer(serversDirectory, self).catch(() => undefined),
    )
    if (session === undefined) {
      void withdrawn.finally(stopped.resolve)
      return
    }
    void Promise.all([withdrawn, session.shutdown()]).finally(stopped.resolve)
  }
  process.stdin.once("end", stop)
  process.once("SIGTERM", stop)
  process.once("SIGINT", stop)
  process.once("SIGHUP", stop)
  await server.connect(new StdioServerTransport())

  if (shared) {
    notReady = SHARED_APP_SERVER_NOTE
    warn(SHARED_APP_SERVER_NOTE)
    await stopped.promise
    await server.close().catch(() => undefined)
    return
  }
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
  if (owning === undefined) {
    await stopped.promise
    await server.close().catch(() => undefined)
    return
  }
  notReady =
    "this session's Envoy side starts once Codex records the session at its first prompt; a fork or an ephemeral session, which keeps no transcript, is never registered"
  // The owner looks again every few seconds and warns if it finds another owner, which the order
  // of publication rules out while every entry is readable.
  const owned = owning
  recheck = setInterval(() => {
    void stillOwner(serversDirectory, owned).then(
      (still) => {
        if (!still)
          warn(`another plugin server of codex process ${codexPid} owns its Envoy session as well`)
      },
      () => undefined,
    )
  }, OWNER_RECHECK_MS)
  recheck.unref?.()
  record = await waitForRecord(recordFile, codexPid, stopping.signal, endedNow)
  if (record === undefined) {
    await stopped.promise
    await server.close().catch(() => undefined)
    return
  }
  const bound = record
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
  stopWatchingRecord = watchRecord(
    recordFile,
    codexPid,
    endedNow,
    (next, ended) => {
      if (next.session_id === record?.session_id) {
        // The same session ended (`SessionEnd`), or runs again (`/resume`). Before the delivery
        // exists, its creation reads `recordEnded` instead.
        if (ended !== recordEnded) {
          recordEnded = ended
          if (ended) delivery?.sessionEnded()
          else void delivery?.sessionResumed().catch((error: unknown) => warn(messageFor(error)))
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
  const { dispatch, envoy, notice } = sessionSettings(configFile, process.env, {
    cwd: bound.cwd,
    session: () => id.id,
  })
  if (notice !== null) warn(notice)
  const title = await sessionTitle(
    {
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
    await server.close().catch(() => undefined)
    return
  }

  const command = (): CodexCommand => ({
    codexHome: record?.codex_home ?? bound.codex_home,
    // Codex's own environment, not this server's: Codex gave this server only `env_vars`.
    env: codexProcessEnv(codexPid, process.env),
    executable: record?.codex_executable ?? bound.codex_executable ?? "codex",
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
  const runningSession = (): string => record?.session_id ?? bound.session_id
  const codexDelivery = createCodexDelivery({
    inboxDirectory: (sessionId) => codexInboxDirectory(stateDirectory, sessionId),
    queue: (text) =>
      queueMessage(command(), runningSession(), text, join(stateDirectory, "codex-long-messages")),
    sessionId: runningSession,
    taken: (item) => stream?.delivered(item.content, item.dispatchMessageId),
    warn,
  })
  delivery = codexDelivery
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

  try {
    const { inbound } = envoy
    if ("off" in inbound) {
      session = inboundOffChannelSession({ identity: id, stateDirectory, why: inbound.off })
    } else {
      // Dialled as the Claude Code session's connection is (`dialChannelNats`): with a seed, no
      // credential, or the session's broker token, which is then kept renewed and, at a `/new`,
      // moved to the new session's (`followSession`) before that session's subjects are taken.
      const dialled = await dialChannelNats(
        inbound,
        `codex-envoy-channel-${id.id}`,
        warn,
        connect,
        stopping.signal,
      )
      connection = dialled.connection
      stopNatsKeeper = dialled.stop
      // Which id the connection is admitted as, where its broker token admits one session at a
      // time: the dial's, then each `/new`'s once its move lands. The live view takes a new id's
      // control subject only then (`CodexAgentStreamOptions.admitted`).
      const admission = dialled.followSession === undefined ? undefined : sessionAdmission(id.id)
      const follow = dialled.followSession
      const followSession =
        follow === undefined || admission === undefined
          ? undefined
          : async (): Promise<void> => {
              const forSession = id.id
              await follow()
              admission.admit(forSession)
            }
      session = await startChannelSession({
        capabilities: () => CODEX_CAPABILITIES,
        client,
        clientInstanceID,
        configFile,
        connection,
        handoffPid: codexPid,
        heartbeatMs: envoy.heartbeatMs,
        identity: id,
        ...(followSession === undefined ? {} : { followSession }),
        notifier: {
          notification: ({ content, eventId }) =>
            codexDelivery.deliver({ content, ...(eventId === undefined ? {} : { eventId }) }),
        },
        pluginVersion: installedPluginVersion(bundlePath) ?? "source",
        pluginName: CODEX_PACKAGE_NAME,
        router: (state) =>
          createCodexUserTurnRouter({
            ask: (question) => askSideQuestion(command(), id.id, question),
            deliver: (item) => codexDelivery.deliver(item),
            dispatch: (ms) => {
              const active = activeDispatchSettings(
                sessionDispatchSettings(configFile, process.env, { cwd: bound.cwd }).dispatch,
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
        stateDirectory,
        title,
        ...(dispatch.enabled ? { stampTitle } : {}),
      })
      stream = startCodexAgentStream({
        connection,
        sessionId: id.id,
        warn,
        ...(admission === undefined
          ? {}
          : { admitted: (next: string) => admission.admitted(next) }),
      })
    }
    runtime = { client, clientInstanceID, identity: id, session }
    follow(bound.transcript_path)

    ready = true
    // A `/new` the record watcher saw while the session was starting.
    if (record !== undefined && record.session_id !== bound.session_id) moveTo(record)
    if (isStopping) await session.shutdown()
    await stopped.promise
  } finally {
    follower?.stop()
    stopNatsKeeper()
    if (session === undefined && connection !== undefined) {
      await connection.drain().catch(() => undefined)
      await connection.close().catch(() => undefined)
    }
    await server.close().catch(() => undefined)
  }
}
