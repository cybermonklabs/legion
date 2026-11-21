// The channel server's process ends with its session: a start that fails (here a hub that refuses
// its NATS credential, which the session's own broker token makes a start can meet) closes the mod
// link's socket and exits non-zero, and a server whose parent is gone (its stdin at end of file)
// exits too, rather than living on with its socket bound for a session nobody runs.
import { afterEach, expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment"
import { completeLines } from "@legion/test-fixtures/transcript-lines"
import { settled, span, until } from "@legion/test-fixtures/wait"
import type { Subprocess } from "bun"
import { resolveModSocketPath } from "../hooks/mod-protocol"
import { CHANNEL_RECORD_FILE, type ChannelRecordLine } from "../src/channel-record"
import { SHUTDOWN_EXIT_MS } from "../src/way-out"
import { dialTimerPreload, lastDialAtExit, readDialTimers } from "./dial-timer"
import { runTeardown } from "./teardown"

/** Each session's home, which is also its `XDG_RUNTIME_DIR`; `mkdtemp` adds six characters. */
const HOME_PREFIX = "claude-envoy-fatal-"

/** The root the session homes go in, once it is chosen (`homeRoot`). */
let chosenRoot: string | undefined

/**
 * The root each session's home is made in: `tmpdir()`, as for any other scratch directory, when
 * the session's mod socket, `<home>/claude-envoy/ce-<hash>.sock`, fits the 100 bytes
 * `resolveModSocketPath` allows (`MOD_SOCKET_PATH_MAX_BYTES`) under a home made there, and `/tmp`
 * otherwise, which it says once on stderr. The product's own resolver measures the path, so the
 * bound is the one the server keeps. A `TMPDIR` over 36 bytes, such as a dev box's data disk or a
 * Legion pod's, leaves the socket no room and takes `/tmp`.
 */
function homeRoot(): string {
  if (chosenRoot !== undefined) return chosenRoot
  const refusals: string[] = []
  for (const root of new Set([tmpdir(), "/tmp"])) {
    const { refusal } = resolveModSocketPath("ses_fatal", {
      XDG_RUNTIME_DIR: join(root, `${HOME_PREFIX}XXXXXX`),
    })
    if (refusal !== undefined) {
      refusals.push(refusal)
      continue
    }
    if (refusals.length > 0) {
      console.error(
        `channel-fatal-exit.test.ts: ${refusals.join("; ")}; the homes go under ${root}`,
      )
    }
    chosenRoot = root
    return root
  }
  throw new Error(`no root leaves a session's mod socket room: ${refusals.join("; ")}`)
}

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

const cleanups: (() => Promise<void> | void)[] = []
afterEach(() => runTeardown(cleanups))

/** A NATS server that refuses every client: the INFO asking for a credential, then nats-server's
 * own refusal of the CONNECT, which is what a hub's auth service answers a token it does not admit. */
function refusingHub(): string {
  const server = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        const info = {
          server_id: "refusing-hub",
          server_name: "refusing-hub",
          version: "2.10.22",
          proto: 1,
          host: "127.0.0.1",
          port: 0,
          headers: true,
          auth_required: true,
          max_payload: 1_048_576,
          nonce: "refusing-hub-nonce",
        }
        socket.write(`INFO ${JSON.stringify(info)}\r\n`)
      },
      data(socket, data) {
        if (data.toString().includes("CONNECT")) {
          socket.write("-ERR 'Authorization Violation'\r\n")
          socket.end()
        }
      },
    },
  })
  cleanups.push(() => server.stop(true))
  return `nats://127.0.0.1:${server.port}`
}

/** A NATS server that admits a client with no credential, answers the first PING (which a client's
 * connect waits for) and then goes silent: every later PING, a flush's, is left unanswered, as on
 * a path to the hub that dropped after admitting the session. `pings()` counts the PINGs seen. */
function silentHub(): { url: string; pings(): number } {
  let pings = 0
  const server = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        const info = {
          server_id: "silent-hub",
          server_name: "silent-hub",
          version: "2.10.22",
          proto: 1,
          host: "127.0.0.1",
          port: 0,
          headers: true,
          max_payload: 1_048_576,
        }
        socket.write(`INFO ${JSON.stringify(info)}\r\n`)
      },
      data(socket, data) {
        for (const line of data.toString().split("\r\n")) {
          if (line !== "PING") continue
          pings += 1
          if (pings === 1) socket.write("PONG\r\n")
        }
      },
    },
  })
  cleanups.push(() => server.stop(true))
  return { url: `nats://127.0.0.1:${server.port}`, pings: () => pings }
}

/** A NATS server that takes the connection and never sends its INFO, so a client's dial waits on
 *  it until nats.js's own dial timeout. `connections()` counts the clients that reached it. */
function muteHub(): { url: string; connections(): number } {
  let connections = 0
  const server = Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open() {
        connections += 1
      },
      data() {},
    },
  })
  cleanups.push(() => server.stop(true))
  return { url: `nats://127.0.0.1:${server.port}`, connections: () => connections }
}

interface Started {
  /** Where the server's dial-timer record goes (`./dial-timer`), when the row asked for the
   *  records (`records`). */
  readonly dialTimerRecord: string | undefined
  /** Where the server's own record of its way out goes (`../src/channel-record`), likewise. */
  readonly wayOutRecord: string | undefined
  readonly server: Subprocess<"pipe", "ignore", "pipe">
  readonly socketPath: string
  stderr(): string
}

/** Starts the channel server from this checkout as Claude Code would, with `natsUrls` its Envoy's
 *  NATS servers (none: inbound off), its stdin held open as a live parent holds it, and sends the
 *  MCP initialize handshake. `records` preloads the server with `dialTimerPreload` and has it
 *  write its own record of its way out, for the one row that reads which way its NATS dial ended
 *  and which steps its way out took. */
async function startServer(
  natsUrls: readonly string[],
  options: { readonly records?: boolean } = {},
): Promise<Started> {
  const home = await mkdtemp(join(homeRoot(), HOME_PREFIX))
  cleanups.push(() => rm(home, { recursive: true, force: true }))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({
      envoy: {
        enabled: true,
        url: "http://127.0.0.1:9",
        ...(natsUrls.length > 0 ? { natsUrls } : {}),
      },
      dispatch: { enabled: false },
    }),
  )
  const sessionID = `ses_fatal_${crypto.randomUUID().replaceAll("-", "")}`
  const dialTimer = options.records === true ? await dialTimerPreload(home) : undefined
  const wayOutRecord = options.records === true ? join(home, "channel-record.jsonl") : undefined
  // This process's own Bun, by its path: a `bun` looked up on PATH can be a version manager's
  // shim, which under this session's HOME finds no configuration and exits before the server runs.
  const server = Bun.spawn(
    [
      process.execPath,
      ...(dialTimer === undefined ? [] : ["--preload", dialTimer.preload]),
      channelEntry,
    ],
    {
      cwd: home,
      env: {
        // Bun keeps its transpiler cache under HOME, and this HOME starts empty: the server writes a
        // cache file into it for each large module it loads, before its own code runs, and on a
        // loaded machine its socket then came up to 7.6 s after the spawn rather than under half a
        // second. `bunCacheOff` turns the cache off; the server transpiles as before and writes
        // nothing there.
        ...bunCacheOff,
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: sessionID,
        CLAUDE_PLUGIN_DATA: home,
        XDG_RUNTIME_DIR: home,
        ENVOY_HEARTBEAT_MS: "240000",
        ...(wayOutRecord === undefined ? {} : { [CHANNEL_RECORD_FILE]: wayOutRecord }),
      },
      stdin: "pipe",
      stdout: "ignore",
      stderr: "pipe",
    },
  )
  cleanups.push(async () => {
    server.kill("SIGKILL")
    await server.exited
  })
  let stderr = ""
  void (async () => {
    for await (const chunk of server.stderr.pipeThrough(new TextDecoderStream())) stderr += chunk
  })()
  server.stdin.write(
    `${JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "test", version: "0" },
      },
    })}\n`,
  )
  server.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`)
  server.stdin.flush()
  const socket = resolveModSocketPath(sessionID, { XDG_RUNTIME_DIR: home })
  if (socket.path === undefined)
    throw new Error(`the session names no mod socket: ${socket.refusal}`)
  const socketPath = socket.path
  return {
    server,
    socketPath,
    dialTimerRecord: dialTimer?.record,
    wayOutRecord,
    stderr: () => stderr,
  }
}

/** The server's exit code once it exits; past the row's deadline the wait fails, naming `what`
 *  and what the server's stderr said by then. */
async function exitOf(started: Started, what: string): Promise<number> {
  try {
    return await settled(what, started.server.exited)
  } catch (error) {
    // Read when the wait gave up, not when it began: the line that says why comes late.
    throw new Error(
      `${error instanceof Error ? error.message : String(error)}; its stderr said: ${started.stderr()}`,
      { cause: error },
    )
  }
}

/** Holds the row while the server is expected to run on: a child process that does not exit
 *  leaves no state to read but its exit code, its stdout is not read here, and no later action's
 *  answer is ordered after a spontaneous exit, so a span it is, after which it must still run. */
async function stillRunning(started: Started): Promise<void> {
  await span(500, "a child process that does not exit leaves nothing to read but its exit code")
  expect(started.server.exitCode, `exited: ${started.stderr()}`).toBeNull()
}

/** How an ended server left: its own exit, possibly after the entry's start-failure line, or a
 * signal death. */
function exitEvent(started: Started): string {
  const { exitCode, signalCode } = started.server
  if (signalCode !== null) return `killed by ${signalCode}`
  return started.stderr().includes("channel server failed")
    ? `exit ${exitCode} after the start failed`
    : `exit ${exitCode}`
}

/**
 * Waits until `ready` holds, through the shared wait, which takes no bound of its own and ends at
 * the row's deadline naming `what`, so a start that a loaded machine slows takes what it takes. A
 * server that exits first ends the wait at once, naming its exit code and what it wrote to stderr.
 */
async function startedTo(started: Started, ready: () => boolean, what: string): Promise<void> {
  let exited: number | undefined
  void started.server.exited.then((code) => {
    exited = code
  })
  await until(what, () => {
    if (ready()) return true
    if (exited !== undefined) {
      throw new Error(`the server exited ${exited} before ${what}: ${started.stderr()}`)
    }
    return false
  })
}

test("a start the hub refuses closes the mod socket and exits non-zero, naming the setting", async () => {
  const hub = refusingHub()
  const started = await startServer([hub])
  // Its stdin stays open: the parent is alive, and the server still ends with its session.
  const code = await exitOf(started, "the exit of a start the hub refused")
  // The failure says which setting named the hub, which credential it refused, and why; it names
  // no part of the hub's address, which the setting holds.
  expect(started.stderr()).toMatch(
    /could not connect to NATS at the server envoy\.natsUrls in \S+envoy\.json names presenting no credential: Authorization Violation/,
  )
  expect(started.stderr()).not.toContain(hub)
  expect(code).not.toBe(0)
  expect(existsSync(started.socketPath)).toBe(false)
})

test("a server whose parent is gone exits on its stdin's end of file, its socket removed", async () => {
  // Inbound off: the session runs with no NATS, so nothing but the parent ends it.
  const started = await startServer([])
  await startedTo(started, () => existsSync(started.socketPath), "its mod socket was bound")
  await stillRunning(started)
  started.server.stdin.end()
  await exitOf(started, "the exit after stdin closed")
  expect(existsSync(started.socketPath)).toBe(false)
})

test("a stop asked for while the start waits on a silent hub exits 0, its socket removed", async () => {
  // Admitted, then the hub answers no flush: the start waits, as on a path that dropped after
  // admitting the session. A stop then is an asked-for stop, not a failure.
  const hub = silentHub()
  const started = await startServer([hub.url])
  await startedTo(
    started,
    () => hub.pings() >= 2 && existsSync(started.socketPath),
    "the hub's second PING and its mod socket",
  )
  await stillRunning(started)
  started.server.kill("SIGTERM")
  const code = await exitOf(started, "the exit after SIGTERM")
  expect(code, started.stderr()).toBe(0)
  expect(started.stderr()).not.toContain("channel server failed")
  expect(existsSync(started.socketPath)).toBe(false)
})

test("a stop asked for while the dial waits on a mute hub exits 0, its socket removed", async () => {
  const hub = muteHub()
  const started = await startServer([hub.url], { records: true })
  await startedTo(
    started,
    () => hub.connections() >= 1 && existsSync(started.socketPath),
    "the hub's connection and its mod socket",
  )
  started.server.kill("SIGTERM")
  await exitOf(started, "the exit after SIGTERM")
  // What decides the row is how the server ended, not when. A correct stop ends the start's wait
  // on the dial, and the process exits 0 through its own SIGTERM handler while nats.js's dial
  // timer is still armed. A server with no handler is killed by the signal. A stop that leaves the
  // dial's wait alone also exits 0, but only after that timer fired. A server started without the
  // preload records no timer at all. The wait for the exit ends at the row's deadline, naming it,
  // against a process that never ends.
  expect(exitEvent(started), started.stderr()).toBe("exit 0")
  const timers =
    started.dialTimerRecord === undefined ? [] : await readDialTimers(started.dialTimerRecord)
  expect(lastDialAtExit(timers), `${JSON.stringify(timers)}; ${started.stderr()}`).toBe(
    "armed and not fired",
  )
  // The way out, from the server's own record: no session came up, so it is the mod link's bind
  // and close and the MCP server's close, each named while it runs and ended by its fact, and it
  // completed before the process exited.
  const wayOut =
    started.wayOutRecord === undefined
      ? []
      : completeLines(await readFile(started.wayOutRecord, "utf8").catch(() => "")).map(
          (line) => JSON.parse(line) as ChannelRecordLine,
        )
  expect(wayOut, started.stderr()).toEqual([
    { shutdown: "began", exitBoundMs: SHUTDOWN_EXIT_MS },
    { wait: "bind", endedBy: "fact" },
    { wait: "mod", endedBy: "fact" },
    { wait: "server", endedBy: "fact" },
    { shutdown: "completed" },
    { shutdown: "exited" },
  ])
  expect(existsSync(started.socketPath)).toBe(false)
})
