// The NATS stall rows of claude-envoy, which run only with LEGION_E2E_ENVOY_STACK=1 (CI's
// envoy-session-stall job), in the package script's second command, whose --timeout bounds every
// wait. The real channel server registers against a listener built from packages/envoy, and the
// proxy freezes its NATS path; the freeze and what it checks are `runFreezeScenario`, which
// pi-envoy's stall row runs too, and the settings that keep that recovery inside the spec's minute
// are held by envoy-client's src/__tests__/nats-stall.test.ts. A session whose first redial hung
// still exits on SIGTERM through its own shutdown: status 0, and its registry entry gone.
import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { existsSync, readFileSync } from "node:fs"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import {
  type EnvoyStack,
  envoyStackEnabled,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "@legion/envoy-client/test/envoy-stack"
import { type StallProxy, startStallProxy } from "@legion/envoy-client/test/stall-proxy"
import {
  expectRecoveredByPingOut,
  type NatsDial,
  RECOVERY_BOUND_MS,
  recoveryBoundMs,
  runFreezeScenario,
  type SessionStep,
} from "@legion/envoy-client/test/stall-scenario"
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment"
import {
  TIMER_RECORD_FILE,
  TIMER_RECORD_ROOTS,
  type TimerStep,
  timersBetween,
} from "@legion/test-fixtures/timer-record"
import { completeLines } from "@legion/test-fixtures/transcript-lines"
import { settled, until } from "@legion/test-fixtures/wait"
import type { Subprocess } from "bun"
import { type ModCallArguments, modSocketPath } from "../hooks/mod-protocol"
import { CHANNEL_RECORD_FILE, type ChannelRecordLine } from "../src/channel-record"
import { SHUTDOWN_BOUNDS } from "../src/way-out"
import { startFakeMod } from "./fake-mod"

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")
/** The preload that records the timers the server's own code arms, into the server's record. */
const timerRecordPreload = Bun.resolveSync(
  "@legion/test-fixtures/timer-record-preload",
  import.meta.dir,
)
/** The server's own code: this package's and the envoy-client and pi-shared sources it runs. */
const serverRoots = [
  resolve(import.meta.dir, "..", "src"),
  resolve(import.meta.dir, "..", "bin"),
  resolve(import.meta.dir, "..", "..", "envoy-client", "src"),
  resolve(import.meta.dir, "..", "..", "pi-shared", "src"),
]

/** The exit this file held a server to on SIGTERM before its waits ended at the row's deadline:
 *  the bounded waits a shutdown arms on its way out must sum to less. */
const EXIT_BOUND_MS = 15_000

/** Where the socket homes are made. A mod socket is `<home>/claude-envoy/ce-<hash>.sock`, and that
 *  whole path has 100 bytes to fit in (`MOD_SOCKET_PATH_MAX_BYTES`): under `tmpdir()` a deep
 *  `TMPDIR` (a dev box's `/mnt/scratch/...`, a Legion pod's) leaves none, so the homes go under
 *  `/tmp`, whatever `TMPDIR` is, as channel-fatal-exit.test.ts's do. */
const SOCKET_ROOT = "/tmp"

/** What one `session.message` the server made carries: the message's own words, which the mod
 *  writes into the session as the prompt of a new turn or as a row of the turn running
 *  (INFRA-1026). */
function shownText(args: ModCallArguments["session.message"]): string {
  return args.message.text
}

/** A channel server process, initialized, whose only NATS path goes through `proxy`. */
interface ChannelServer {
  readonly server: Subprocess<"pipe", "pipe", "pipe">
  readonly proxy: StallProxy
  readonly sessionID: string
  readonly home: string
  /** The channel messages Claude Code would be shown, in the order the server sent them. */
  readonly channel: readonly string[]
  /** What the server wrote to its record so far, in its order (`../src/channel-record`), with
   *  the timers its own code armed and fired between (`@legion/test-fixtures/timer-record`). */
  record(): readonly (ChannelRecordLine | TimerStep)[]
  /** The server's own record of its way back from a freeze, in its order, each message it showed
   *  given by its text, which the record holds by its Envoy event only. */
  sessionRecord(): readonly SessionStep[]
  /** The server's NATS connection as its record gives it: the settings it dialled with and each
   *  status event since; undefined before it dialled. */
  natsDial(): NatsDial | undefined
  /** What the server wrote to stderr so far. */
  stderr(): string
  /** Resolves once `condition` holds; throws when the server exits first, or at the row's
   *  deadline, naming `what` and what the server wrote to stderr. */
  waitFor(condition: () => boolean | Promise<boolean>, what: string): Promise<void>
  /** Stops the server (SIGTERM, and SIGKILL when it has not exited by the row's deadline), closes
   *  the proxy, removes its home. */
  stop(): Promise<void>
}

/**
 * Runs `body` against a channel server started for it, then stops the server. A failure of the
 * body is the row's own, named by the wait or the assertion that failed: a body that failed at
 * the row's deadline leaves the stop's own wait no time, and that wait's give-up is reported only
 * when the body passed.
 */
async function withChannelServer(
  stack: EnvoyStack,
  sessionPrefix: string,
  body: (server: ChannelServer) => Promise<void>,
): Promise<void> {
  const server = await startChannelServer(stack, sessionPrefix)
  let failure: { readonly error: unknown } | undefined
  try {
    await body(server)
  } catch (error) {
    failure = { error }
  }
  try {
    await server.stop()
  } catch (error) {
    if (failure === undefined) throw error
  }
  if (failure !== undefined) throw failure.error
}

/**
 * Starts the channel server from this checkout as Claude Code would, for a new session whose
 * Envoy is `stack`'s listener and whose NATS goes through a stall proxy to `stack`'s natsd, and
 * sends it the MCP initialize handshake.
 */
async function startChannelServer(
  stack: EnvoyStack,
  sessionPrefix: string,
): Promise<ChannelServer> {
  const proxy = await startStallProxy(stack.natsHostPort)
  const home = await mkdtemp(join(SOCKET_ROOT, "claude-envoy-stall-"))
  const configDirectory = join(home, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({ envoy: { enabled: true, url: stack.envoyUrl, natsUrls: [proxy.url] } }),
  )
  const sessionID = `${sessionPrefix}_${crypto.randomUUID().replaceAll("-", "")}`
  const recordFile = join(home, "channel-record.jsonl")
  const server = Bun.spawn(["bun", "--preload", timerRecordPreload, channelEntry], {
    cwd: home,
    env: {
      ...bunCacheOff,
      PATH: process.env["PATH"] ?? "",
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: sessionID,
      CLAUDE_PLUGIN_DATA: home,
      // The base the server and the mod below both resolve the link's socket from.
      XDG_RUNTIME_DIR: home,
      // No heartbeat registration runs in the test's window: the reconnect's registration, on a
      // new epoch, is the only one that restarts the relay.
      ENVOY_HEARTBEAT_MS: "240000",
      // The server's own record of its dial and its steps
      // (`../src/channel-record`), and into the same file, so one order holds both, each timer
      // its own code arms and fires (`@legion/test-fixtures/timer-record`).
      [CHANNEL_RECORD_FILE]: recordFile,
      [TIMER_RECORD_FILE]: recordFile,
      [TIMER_RECORD_ROOTS]: serverRoots.join(":"),
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  const socketPath = modSocketPath(sessionID, { XDG_RUNTIME_DIR: home })
  if (socketPath === undefined) throw new Error("the session names no mod socket")
  // Every delivery reaches the model as the mod's own write - the row of a turn running, or the
  // prompt of a new turn while the session is idle, which this one is throughout (`show` in
  // `../src/mod-session`) - so the mod is this test's view of what the session was shown.
  const channel: string[] = []
  /** The Envoy event each shown message was handed to the mod under, beside its text. */
  const shownEvents: { readonly text: string; readonly eventId: string | undefined }[] = []
  const mod = await startFakeMod(socketPath, {
    sessionId: sessionID,
    answers: {
      "session.message": (args) => {
        channel.push(shownText(args))
        shownEvents.push({ text: shownText(args), eventId: args.eventId })
        // The prompt an idle session took, which is what answers the frame: a prompt carries
        // no row of its own (`../hooks/mod-protocol.ts`, `ModCallResults["session.message"]`).
        return { as: "prompt" }
      },
    },
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
  const record = (): (ChannelRecordLine | TimerStep)[] =>
    existsSync(recordFile)
      ? completeLines(readFileSync(recordFile, "utf8")).map(
          (line) => JSON.parse(line) as ChannelRecordLine | TimerStep,
        )
      : []
  return {
    server,
    proxy,
    sessionID,
    home,
    channel,
    record,
    sessionRecord() {
      return record().flatMap((line): SessionStep[] => {
        if ("status" in line) return [{ status: line.status }]
        if ("registered" in line) return [{ registered: line.registered }]
        if ("shown" in line) {
          const text = shownEvents.find((shown) => shown.eventId === line.shown)?.text
          return text === undefined ? [] : [{ shown: text }]
        }
        if ("timer" in line || "fired" in line) return [line]
        return []
      })
    },
    stderr: () => stderr,
    natsDial() {
      const lines = record()
      const dial = lines.find((line) => "dial" in line)
      if (dial === undefined || !("dial" in dial)) return undefined
      return {
        options: dial.dial,
        statuses: lines.flatMap((line) =>
          "status" in line ? [{ type: line.status, data: line.data }] : [],
        ),
      }
    },
    // The server runs on nats.js's real ping and dial timers and a real listener, so each wait
    // asks again through the shared wait until its condition holds; a server that exits first
    // ends the wait at once, and so does the row's deadline.
    async waitFor(condition, what) {
      try {
        await until(what, async () => {
          if (server.exitCode !== null) {
            throw new Error(`the channel server exited ${server.exitCode} waiting for ${what}`)
          }
          return await condition()
        })
      } catch (error) {
        throw new Error(
          `${error instanceof Error ? error.message : String(error)}; its stderr: ${stderr}`,
          { cause: error },
        )
      }
    },
    async stop() {
      await mod.stop()
      server.kill("SIGTERM")
      try {
        await settled("the channel server's exit on SIGTERM", server.exited)
      } finally {
        // A server still running when the row's deadline passed goes too, rather than outliving
        // the run (a no-op for one that exited).
        server.kill("SIGKILL")
        await server.exited
        await proxy.close()
        await rm(home, { recursive: true, force: true })
      }
    },
  }
}

// One rollout state is left: every listener gives the session a consumer and relays it. A
// listener that can give none refuses the registration, which `envoy-client`'s own stack tests
// cover; there is no core path to fall back to here.
// Reserved out here, at the file's module scope: the file's own `afterAll` removes it once the
// `describe` has stopped the stack writing into it.
const stackDirectory = useEnvoyStackDirectory()

describe.skipIf(!envoyStackEnabled)(
  "the channel server against a real natsd and Envoy listener",
  () => {
    let stack: EnvoyStack | undefined
    beforeAll(async () => {
      stack = await startEnvoyStack({ work: stackDirectory })
    })
    afterAll(async () => {
      await stack?.stop()
    })

    test("a session whose NATS path freezes subscribes its relay subject again on the new connection within 60 s, registers a new epoch, and shows each message once", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const running = stack
      const { envoyUrl } = running
      await withChannelServer(running, "ses_stall", async (channelServer) => {
        const { proxy, sessionID, channel, waitFor, natsDial, sessionRecord } = channelServer
        await waitFor(
          async () => (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok,
          "the session's registration",
        )
        // The bundle runs from this checkout, outside Claude Code's plugin cache.
        const registration = {
          session_id: sessionID,
          plugin: "@sjawhar/claude-envoy",
          plugin_version: "source",
        }
        expect(await (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).json()).toMatchObject(
          registration,
        )
        expect(await (await fetch(`${envoyUrl}/v1/sessions`)).json()).toContainEqual(
          expect.objectContaining(registration),
        )
        await runFreezeScenario({
          label: "claude-envoy stall",
          stack: running,
          sessionID,
          proxy,
          shown: (marker) => channel.filter((content) => content.includes(marker)),
          sessionRecord,
          natsDial,
          waitFor,
        })
      })
    })

    test("a session whose redial once hung still exits on SIGTERM", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      await withChannelServer(stack, "ses_stall_exit", async (channelServer) => {
        const { server, proxy, sessionID, waitFor, record, natsDial, stderr } = channelServer
        await waitFor(
          async () =>
            (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok &&
            proxy.connections[0]?.subscribedAt !== undefined,
          "the session's registration and first subscription",
        )
        // The first redial after the freeze hangs (accepted, never answered); the next forwards.
        proxy.stallNext(1)
        const frozenAt = proxy.freeze()
        await waitFor(
          () =>
            proxy.connections.some(
              (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined,
            ),
          "the session to subscribe again after the hung redial",
        )
        // Past the hung redial within the minute, by the server's own record of its dial.
        await waitFor(
          () => natsDial()?.statuses.some((status) => status.type === "reconnect") ?? false,
          "the server's NATS client to report its reconnect",
        )
        const dial = natsDial()
        if (dial === undefined) throw new Error("the server recorded no NATS dial")
        expect(recoveryBoundMs(dial.options, 1)).toBeLessThan(RECOVERY_BOUND_MS)
        expectRecoveredByPingOut(dial, 1)
        // The hung redial must not hold the process: it exits on SIGTERM, which the shared wait
        // reads through its exit, failing at the row's deadline while the server still runs. How
        // long the way out may take is what the server chose for it, read from its own record
        // (R4: the bounds it armed and the order it took its steps, not a clock): every step of
        // the way is recorded, in the order the shutdown takes them; only a step of the server's
        // own bounds (`SHUTDOWN_BOUNDS`) armed one, so each other step waited for its fact - a
        // last write still running, the DELETE, a close - however long it took; and those bounds,
        // with any other timer its own code ran on the way, stay under the exit bound it holds the
        // whole way to, which is no more than the 15 s this row holds its exit to. That the way
        // ended on its own, not on that bound, is the record's `completed` before the process's
        // exit; a step that never ends - a write, a sleep, a handle left open - ends it on the
        // exit bound instead, which exits 1 naming the steps it dropped. That it ran the shutdown
        // to its end is its exit, 0 and not the signal's, and the deregistration the shutdown
        // sends reaching the listener.
        const beforeStop = record().length
        // Read for `SHUTDOWN_EXIT_MS`'s sizing beside it, never asserted: the way out's time.
        const stoppedAt = Date.now()
        server.kill("SIGTERM")
        const exitCode = await settled("the channel server's exit on SIGTERM", server.exited)
        console.log(
          `claude-envoy stall exit: the way out took ${Date.now() - stoppedAt} ms from SIGTERM to the process's exit`,
        )
        expect(
          { exitCode, signal: server.signalCode },
          `the channel server's exit on SIGTERM; its stderr: ${stderr()}`,
        ).toEqual({ exitCode: 0, signal: null })
        expect((await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok).toBe(false)
        const steps = record()
        const wayOut = steps.slice(beforeStop)
        expect(wayOut.flatMap((line) => ("shutdown" in line ? [line.shutdown] : []))).toEqual([
          "began",
          "completed",
          "exited",
        ])
        const began = wayOut.find((line) => "shutdown" in line && line.shutdown === "began")
        const exitBoundMs = began !== undefined && "exitBoundMs" in began ? began.exitBoundMs : NaN
        expect(exitBoundMs).toBeLessThanOrEqual(EXIT_BOUND_MS)
        const waits = wayOut.flatMap((line) => ("wait" in line ? [line] : []))
        const names = waits.map((wait) => wait.wait)
        // The drained connection's close is waited for only where the drain left it open.
        expect(names.filter((name) => name !== "close")).toEqual([
          "drain",
          "settle",
          "deregister",
          "state",
          "turns",
          "bind",
          "mod",
          "server",
        ])
        expect([-1, 1]).toContain(names.indexOf("close"))
        // A bound is armed only by a step the server bounds itself; every other step ended on
        // what it waited for.
        const ownBounds: readonly string[] = Object.keys(SHUTDOWN_BOUNDS)
        expect(
          waits.filter((wait) => wait.armedMs !== undefined || wait.endedBy !== "fact"),
          `the steps that armed a bound or ended on one, of ${JSON.stringify(waits)}`,
        ).toEqual(waits.filter((wait) => ownBounds.includes(wait.wait)))
        const completed = steps.findIndex(
          (line) => "shutdown" in line && line.shutdown === "completed",
        )
        const otherTimers = timersBetween(steps, beforeStop - 1, completed).filter(
          (timer) => !timer.from.includes("/way-out.ts:"),
        )
        const armedMs =
          waits.reduce((armed, wait) => armed + (wait.armedMs ?? 0), 0) +
          otherTimers.reduce((armed, timer) => armed + timer.delayMs, 0)
        expect(
          armedMs,
          `the way out's bounds, ${JSON.stringify(waits)}, and its other timers, ${JSON.stringify(otherTimers)}`,
        ).toBeLessThan(exitBoundMs)
      })
    })
  },
)
