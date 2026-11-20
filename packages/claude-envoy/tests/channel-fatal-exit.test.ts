// The channel server's process ends with its session: a start that fails (here a hub that refuses
// its NATS credential, which the session's own broker token makes a start can meet) closes the mod
// link's socket and exits non-zero, and a server whose parent is gone (its stdin at end of file)
// exits too, rather than living on with its socket bound for a session nobody runs.
import { afterEach, expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { createRequire } from "node:module"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { natsConnectionOptions } from "@legion/envoy-client/nats-connection"
import type { Subprocess } from "bun"
import { resolveModSocketPath } from "../hooks/mod-protocol"

const require = createRequire(import.meta.url)
const NATS_PROTOCOL = require.resolve("nats/lib/nats-base-client/protocol.js")
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
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup()
})

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

/** The timer nats.js's protocol arms for a dial this test's server starts. `natsConnectionOptions`
 * leaves `timeout` unset, so the installed protocol's `this.options.timeout || <number>` takes
 * it. Read that number from that very source, never from every timer that happens to have a large
 * delay. */
async function natsDialTimeoutMs(): Promise<number> {
  const options = natsConnectionOptions({
    servers: [],
    name: "channel-fatal-exit-timer-record",
    credential: { kind: "none" },
  })
  if (options.timeout !== undefined) return options.timeout
  const source = await readFile(NATS_PROTOCOL, "utf8")
  const defaultTimeout = /this\.options\.timeout \|\| (\d+)/.exec(source)?.[1]
  if (defaultTimeout === undefined)
    throw new Error(`could not read nats's dial timeout in ${NATS_PROTOCOL}`)
  return Number(defaultTimeout)
}

/** A preloaded child module records the nats dial timer this test's server arms: only a timer with
 * the exact timeout the server's connection options select, called by nats's protocol source. On
 * exit it writes `armed and not fired`, `fired`, or `none armed`. */
async function dialTimerPreload(
  home: string,
): Promise<{ readonly preload: string; readonly record: string }> {
  const directory = join(home, "dial-timer")
  const preload = join(directory, "preload.js")
  const record = join(directory, "record")
  await mkdir(directory)
  await writeFile(
    preload,
    [
      'import { writeFileSync } from "node:fs"',
      `const timeout = ${await natsDialTimeoutMs()}`,
      `const protocol = ${JSON.stringify(NATS_PROTOCOL)}`,
      `const record = ${JSON.stringify(record)}`,
      "const set = globalThis.setTimeout",
      "const clear = globalThis.clearTimeout",
      "let timer",
      "let fired = false",
      "globalThis.setTimeout = (callback, delay, ...args) => {",
      '  const dial = Number(delay) === timeout && (new Error().stack ?? "").includes(protocol)',
      "  let handle",
      "  handle = set((...callbackArgs) => {",
      "    if (dial && handle === timer) fired = true",
      "    return callback(...callbackArgs)",
      "  }, delay, ...args)",
      "  if (dial) timer = handle",
      "  return handle",
      "}",
      "globalThis.clearTimeout = (handle) => {",
      "  if (handle === timer && !fired) timer = undefined",
      "  return clear(handle)",
      "}",
      'process.on("exit", () => {',
      '  writeFileSync(record, timer === undefined ? "none armed\\n" : fired ? "fired\\n" : "armed and not fired\\n")',
      "})",
      "",
    ].join("\n"),
  )
  return { preload, record }
}

interface Started {
  /** Where the server's dial-timer record goes, when the row asked for one (`recordDialTimer`). */
  readonly dialTimerRecord: string | undefined
  readonly server: Subprocess<"pipe", "ignore", "pipe">
  readonly socketPath: string
  stderr(): string
}

/** Starts the channel server from this checkout as Claude Code would, with `natsUrls` its Envoy's
 *  NATS servers (none: inbound off), its stdin held open as a live parent holds it, and sends the
 *  MCP initialize handshake. `recordDialTimer` preloads the server with `dialTimerPreload`, for the
 *  one row that reads which way its NATS dial ended. */
async function startServer(
  natsUrls: readonly string[],
  options: { readonly recordDialTimer?: boolean } = {},
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
  const dialTimer = options.recordDialTimer === true ? await dialTimerPreload(home) : undefined
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
        // second. "0" turns the cache off; the server transpiles as before and writes nothing there.
        BUN_RUNTIME_TRANSPILER_CACHE_PATH: "0",
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: sessionID,
        CLAUDE_PLUGIN_DATA: home,
        XDG_RUNTIME_DIR: home,
        ENVOY_HEARTBEAT_MS: "240000",
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
  return { server, socketPath, dialTimerRecord: dialTimer?.record, stderr: () => stderr }
}

/** The server's exit code once it exits, or undefined when it is still running after `ms`. */
async function exitWithin(server: Subprocess, ms: number): Promise<number | undefined> {
  const late = Bun.sleep(ms).then(() => undefined)
  return Promise.race([server.exited, late])
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
 * Waits until `ready` holds, looking every 50 ms, with no deadline of its own: the row's timeout is
 * its only bound, so a start that a loaded machine slows takes what it takes. A server that exits
 * first ends the wait at once, naming its exit code and what it wrote to stderr.
 */
async function startedTo(started: Started, ready: () => boolean, what: string): Promise<void> {
  let exited: number | undefined
  void started.server.exited.then((code) => {
    exited = code
  })
  while (!ready()) {
    if (exited !== undefined) {
      throw new Error(`the server exited ${exited} before ${what}: ${started.stderr()}`)
    }
    await Bun.sleep(50)
  }
}

test("a start the hub refuses closes the mod socket and exits non-zero, naming the setting", async () => {
  const hub = refusingHub()
  const started = await startServer([hub])
  // Its stdin stays open: the parent is alive, and the server still ends with its session.
  const code = await started.server.exited
  // The failure says which setting named the hub, which credential it refused, and why; it names
  // no part of the hub's address, which the setting holds.
  expect(started.stderr()).toMatch(
    /could not connect to NATS at the server envoy\.natsUrls in \S+envoy\.json names presenting no credential: Authorization Violation/,
  )
  expect(started.stderr()).not.toContain(hub)
  expect(code).not.toBe(0)
  expect(existsSync(started.socketPath)).toBe(false)
}, 20_000)

test("a server whose parent is gone exits on its stdin's end of file, its socket removed", async () => {
  // Inbound off: the session runs with no NATS, so nothing but the parent ends it.
  const started = await startServer([])
  await startedTo(started, () => existsSync(started.socketPath), "its mod socket was bound")
  expect(await exitWithin(started.server, 500)).toBeUndefined()
  started.server.stdin.end()
  await started.server.exited
  expect(existsSync(started.socketPath)).toBe(false)
}, 20_000)

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
  expect(await exitWithin(started.server, 500)).toBeUndefined()
  started.server.kill("SIGTERM")
  const code = await started.server.exited
  expect(code, started.stderr()).toBe(0)
  expect(started.stderr()).not.toContain("channel server failed")
  expect(existsSync(started.socketPath)).toBe(false)
}, 30_000)

test("a stop asked for while the dial waits on a mute hub exits 0, its socket removed", async () => {
  const hub = muteHub()
  const started = await startServer([hub.url], { recordDialTimer: true })
  await startedTo(
    started,
    () => hub.connections() >= 1 && existsSync(started.socketPath),
    "the hub's connection and its mod socket",
  )
  started.server.kill("SIGTERM")
  await started.server.exited
  // What decides the row is how the server ended, not when. A correct stop ends the start's wait
  // on the dial, and the process exits 0 through its own SIGTERM handler while nats.js's dial
  // timer is still armed. A server with no handler is killed by the signal. A stop that leaves the
  // dial's wait alone also exits 0, but only after that timer fired. A server started without the
  // preload records no timer at all. The row's own timeout is its only bound, against a process
  // that never ends.
  expect(exitEvent(started), started.stderr()).toBe("exit 0")
  const timer =
    started.dialTimerRecord === undefined
      ? "none armed"
      : (await readFile(started.dialTimerRecord, "utf8").catch(() => "none armed")).trim()
  expect(timer, started.stderr()).toBe("armed and not fired")
  expect(existsSync(started.socketPath)).toBe(false)
}, 30_000)
