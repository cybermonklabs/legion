import { spawn } from "node:child_process"
import { unwatchFile, watchFile } from "node:fs"
import { homedir } from "node:os"
import { join, resolve } from "node:path"
import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import { Server } from "@modelcontextprotocol/sdk/server/index.js"
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js"
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { z } from "zod"
import { configuredValue } from "../claude-session"
import {
  channelToolDefinitions,
  executeEnvoyTool,
  MCP_SERVER_INFO,
  mcpResult,
} from "../envoy-channel-server"
import { type CodexEnvoySession, createCodexEnvoySession } from "./codex-envoy-session"
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
  codexThreadRecordFile,
  processStarted,
  readCodexSessionRecord,
  recordOfProcess,
  sessionEnded,
  sharedAppServer,
  watchRecord,
  watchThreads,
} from "./codex-session-record"
import { type CodexThreads, createCodexThreads } from "./codex-threads"
import {
  forwardToolCall,
  serveToolRoute,
  type ToolCall,
  type ToolRouteServer,
} from "./codex-tool-route"
import { findTranscript, latestTurnCwd, threadIdShape } from "./codex-transcript"

/**
 * The Envoy plugin's MCP server under the Codex CLI: `codex/.codex-plugin/plugin.json`
 * starts `dist/envoy-channel.js --host codex` in the plugin's own directory, as a child of the
 * `codex` process.
 *
 * It serves the Envoy tools and the `dispatch` tool at once, and starts the session's Envoy side
 * (`./codex-envoy-session`) when the plugin's `SessionStart` hook records which session the
 * `codex` process runs (`./codex-session-record`), which Codex does at the session's first prompt.
 * A `/new` moves it to the new session's id through the same hand-off file a Claude Code `/clear`
 * writes.
 *
 * Codex starts one such server per thread, so a `codex` process can have several (a `/new` starts
 * one beside the old thread's, and `/side` and `/btw` one each). Only the one that owns the
 * process's session (`./codex-owner`) runs the Envoy side; the others serve their thread's
 * `dispatch` calls and take the session over once the owner is gone.
 *
 * Codex's shared app server (`sharedAppServer`) runs one session per thread in one process. There
 * the hook records each thread, and the server that owns the process runs an Envoy session for each
 * recorded thread that has not ended (`./codex-threads`). Every Envoy tool call there acts as the
 * thread Codex names on it: the owner runs it through that thread's session, and a server standing
 * by sends it to the owner (`./codex-tool-route`).
 */

const CODEX_INSTRUCTIONS =
  "Envoy delivers trusted, internal session and Dispatch events into this conversation. Use the Envoy tools for Envoy actions, and reach Dispatch through this server's `dispatch` tool (the `dispatch` skill teaches its commands). Dispatch asks stay on Dispatch."

/** How often the session record is looked at while the server waits for it, and after. */
const RECORD_POLL_MS = 250

/** What an Envoy tool call under the shared app server is told when Codex names no thread on it. */
const NO_THREAD_NOTE =
  "Codex named no thread on this call, and this session runs in Codex's shared app server, which runs several sessions in one process, so the Envoy plugin cannot tell which of them made it, and acts as none of them"

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
 * `threadId` on every MCP tool call, beside `x-codex-turn-metadata` naming the same thread, and so
 * do 0.170.0 and 0.171.0 under the shared app server (measured). Undefined when the call names none.
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
  /** Codex's shared app server runs one session per thread in this one process (`sharedAppServer`):
   *  the server that owns it registers each recorded thread (`./codex-threads`), and every Envoy
   *  tool call acts as the thread Codex names on it. */
  const shared = sharedAppServer(codexPid)
  const recordFile = codexSessionRecordFile(stateDirectory, codexPid)
  const serversDirectory = codexServersDirectory(stateDirectory, codexPid)
  /** This server's entry, once `waitForOwnership` published it. */
  let self: CodexServerEntry | undefined
  let stopWatching: () => void = () => undefined
  let recheck: ReturnType<typeof setInterval> | undefined
  /** Whether session `sessionId` of this process has ended (`SessionEnd`'s marker). */
  const endedNow = (sessionId: string): Promise<boolean> =>
    sessionEnded(codexEndedFile(stateDirectory, codexPid, sessionId))
  const configFile = sessionConfigFile("codex", process.env)
  const warn = (text: string): void => void process.stderr.write(`envoy-channel: ${text}\n`)
  const bundlePath = resolve(pluginRoot, process.argv[1] ?? "")

  /** The record of the session a `codex` process of its own runs, once the hook wrote it. */
  let record: CodexSessionRecord | undefined
  /** That session's Envoy side, once its record is bound. */
  let processSession: CodexEnvoySession | undefined
  /** The Envoy sessions of the shared app server's threads, once this server owns that process. */
  let threads: CodexThreads | undefined
  /** Where the other servers of the shared app server send their threads' Envoy tool calls. */
  let route: ToolRouteServer | undefined
  let notReady = "this session's Envoy side starts at its first prompt"
  const stopping = new AbortController()

  const server = new Server(MCP_SERVER_INFO, {
    capabilities: { tools: {} },
    instructions: CODEX_INSTRUCTIONS,
  })
  const tools = [...channelToolDefinitions(), dispatchToolDefinition()]
  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }))
  server.setRequestHandler(CallToolRequestSchema, async (request) => {
    const thread = callingThread(request.params._meta)
    if (request.params.name === "dispatch") {
      return {
        content: [
          { text: await runDispatchTool(request.params.arguments, thread), type: "text" as const },
        ],
      }
    }
    if (shared) {
      // Never answered as another thread: a call that names none is refused.
      if (thread === undefined) throw new Error(`Envoy is not ready: ${NO_THREAD_NOTE}`)
      const call = { arguments: request.params.arguments, name: request.params.name, thread }
      return mcpResult(
        threads === undefined
          ? await forwardToolCall(serversDirectory, call)
          : await threadTool(call),
      )
    }
    const runtime = processSession?.runtime()
    if (runtime === undefined) {
      throw new Error(`Envoy is not ready: ${processSession?.notReady() ?? notReady}`)
    }
    return mcpResult(await executeEnvoyTool(runtime, request.params.name, request.params.arguments))
  })

  /** Runs an Envoy tool call of the shared app server through the session of the thread it names,
   *  which this server runs as the process's owner; a call another server sent here
   *  (`./codex-tool-route`) comes through this too. */
  const threadTool = async (call: ToolCall): Promise<unknown> => {
    const runtime = threads?.runtime(call.thread) ?? "this server does not own the process"
    if (typeof runtime === "string") throw new Error(`Envoy is not ready: ${runtime}`)
    return executeEnvoyTool(runtime, call.name, call.arguments)
  }

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
        ? `dispatch: ${NO_THREAD_NOTE}\n[exit 2]`
        : `dispatch: this session has no id yet; Codex records it at the session's first prompt\n[exit 2]`
    }
    // The calling thread's own record: under the shared app server each thread has one.
    const recorded = shared
      ? await readCodexSessionRecord(codexThreadRecordFile(stateDirectory, codexPid, sessionId))
      : current
    const own =
      recorded?.session_id === sessionId && recordOfProcess(recorded, codexPid)
        ? recorded
        : undefined
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

  const stopped = Promise.withResolvers<void>()
  let isStopping = false
  const stop = (): void => {
    if (isStopping) return
    isStopping = true
    stopping.abort()
    stopWatching()
    clearInterval(recheck)
    // The entry goes once the deliveries' waiting lines are withdrawn, so a server that takes over
    // never discards a line this one still answers for, and once the route file no longer names
    // this server, so that server's own route file is never the one removed.
    const withdrawn = Promise.all([
      processSession?.withdraw(),
      threads?.withdraw(),
      route?.stop().catch(() => undefined),
    ]).finally(() =>
      self === undefined
        ? undefined
        : unregisterServer(serversDirectory, self).catch(() => undefined),
    )
    void Promise.all([withdrawn, processSession?.shutdown(), threads?.shutdown()]).finally(
      stopped.resolve,
    )
  }
  process.stdin.once("end", stop)
  process.once("SIGTERM", stop)
  process.once("SIGINT", stop)
  process.once("SIGHUP", stop)
  await server.connect(new StdioServerTransport())

  /** Runs the shared app server's threads as the process's owner: serves the other servers' Envoy
   *  tool calls, and keeps one Envoy session for each recorded thread that has not ended, until
   *  this server stops. */
  const ownSharedAppServer = async (): Promise<void> => {
    const owned = createCodexThreads({
      session: (bound) =>
        createCodexEnvoySession({
          bound,
          bundlePath,
          codexPid,
          configFile,
          followsNew: false,
          stateDirectory,
          // The directory's name and the short id: the shared app server's `TMUX_PANE` is the
          // terminal's that started it, and names no other thread's (measured on 0.171.0).
          titleEnvironment: {},
          warn: (text) => warn(`thread ${bound.session_id}: ${text}`),
        }),
      warn,
    })
    threads = owned
    try {
      route = await serveToolRoute(serversDirectory, threadTool)
    } catch (error) {
      warn(
        `the other plugin servers of this shared app server cannot reach this one, so their threads' Envoy tools are refused: ${messageFor(error)}`,
      )
    }
    if (isStopping) {
      // A stop that came while the route started stopped neither it nor the threads.
      await route?.stop().catch(() => undefined)
      await owned.shutdown()
      await stopped.promise
      return
    }
    stopWatching = watchThreads(
      stateDirectory,
      codexPid,
      (live) => owned.update(live),
      RECORD_POLL_MS,
      warn,
    )
    await stopped.promise
  }

  try {
    // One server per codex process owns its Envoy side (`./codex-owner`): Codex starts one per
    // thread, and the others stand by, serving their thread's `dispatch` calls, until every server
    // older than them is gone. Under the shared app server, a server standing by sends its thread's
    // Envoy tool calls to the owner.
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
      return
    }
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

    if (shared) {
      await ownSharedAppServer()
      return
    }
    notReady =
      "this session's Envoy side starts once Codex records the session at its first prompt; a fork or an ephemeral session, which keeps no transcript, is never registered"
    record = await waitForRecord(recordFile, codexPid, stopping.signal, endedNow)
    if (record === undefined || isStopping) {
      await stopped.promise
      return
    }
    const session = createCodexEnvoySession({
      bound: record,
      bundlePath,
      codexPid,
      configFile,
      followsNew: true,
      stateDirectory,
      titleEnvironment: {
        ENVOY_SESSION_TITLE: process.env["ENVOY_SESSION_TITLE"],
        TMUX: process.env["TMUX"],
        TMUX_PANE: process.env["TMUX_PANE"],
      },
      warn,
    })
    processSession = session
    // Watched from now, so a `/new` while the session starts is seen, and read once at once, so an
    // end marked between the bind's own read and this watch is seen too.
    stopWatching = watchRecord(
      recordFile,
      codexPid,
      endedNow,
      (next, ended) => {
        // The same session ended (`SessionEnd`), or runs again (`/resume`); or a `/new` moved the
        // process to another.
        if (next.session_id === record?.session_id) {
          session.ended(ended)
          return
        }
        record = next
        session.moveTo(next, ended)
      },
      RECORD_POLL_MS,
    )
    await session.start()
    await stopped.promise
  } finally {
    await server.close().catch(() => undefined)
  }
}
