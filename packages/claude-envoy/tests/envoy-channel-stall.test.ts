import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import {
  type EnvoyStack,
  envoyStackEnabled,
  readInRow,
  releaseAll,
  STOP_GRACE_MS,
  startStoppableEnvoyStack,
  useEnvoyStackDirectory,
} from "@legion/envoy-client/test/envoy-stack"
import { type StallProxy, startStallProxy } from "@legion/envoy-client/test/stall-proxy"
import { FREEZE_HEARTBEAT_MS, runFreezeScenario } from "@legion/envoy-client/test/stall-scenario"
import type { Subprocess } from "bun"
import { type ModCallArguments, modSocketPath } from "../hooks/mod-protocol"
import { startFakeMod } from "./fake-mod"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

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
  /** Aborted once `stop` has run: by the row's own `finally`, or for a row its limit abandoned,
   *  by the describe's `afterEach`. Its reason says the row ended. */
  readonly ended: AbortSignal
  /** Resolves once `condition` holds; throws when the server or a process of the stack has
   *  exited, by an exit code or a signal, when the row has ended (`ended`), or when `deadline`,
   *  where one is given, passes first. */
  waitFor(
    condition: () => boolean | Promise<boolean>,
    deadline: number | undefined,
    what: string,
  ): Promise<void>
  /** Ends the host's row (`ended`) and releases what the host holds, the last taken first: the
   *  fake mod; the server (SIGTERM, then SIGKILL after 10 s), whose release ends once it has
   *  exited or been sent that SIGKILL, whichever comes first; the home; the proxy. Every release
   *  runs even when one fails, and the call then throws that failure, or an `AggregateError` of
   *  them all where several failed. A call made while another runs waits for it to settle, then
   *  releases whatever was taken since. The host leaves `openHosts` once its releases have run. */
  stop(): Promise<void>
}

/** The stop of each host started whose releases have not all run. The describe's `afterEach`
 *  runs each one: a row its limit abandoned never reaches its own `finally`, and its host must not
 *  outlive it. A host whose stop is still running stays here until that stop has released all,
 *  or until the `afterEach` takes it out to wait on that stop. */
const openHosts = new Set<() => Promise<void>>()

/**
 * Starts the channel server from this checkout as Claude Code would, for a new session whose
 * Envoy is `stack`'s listener and whose NATS goes through a stall proxy to `stack`'s natsd, and
 * sends it the MCP initialize handshake.
 */
async function startChannelServer(
  stack: EnvoyStack,
  sessionPrefix: string,
): Promise<ChannelServer> {
  // What the host holds, each with its release from the moment it exists. `stop` is in
  // `openHosts` before the first of them, so a start that throws, or that its row's limit
  // abandoned, leaves nothing behind once its row has ended.
  const ended = new AbortController()
  const releases: (() => Promise<void>)[] = []
  let stopping = Promise.resolve()
  const stop = (): Promise<void> => {
    ended.abort(new Error("the row ended"))
    stopping = stopping
      .catch(() => undefined)
      .then(async () => {
        try {
          await releaseAll(releases, "the host's releases")
        } finally {
          // Only now: a row whose limit fired while its own `finally` ran this stop leaves the
          // host here, and the `afterEach`'s stop of it waits for these releases to settle.
          openHosts.delete(stop)
        }
      })
    return stopping
  }
  openHosts.add(stop)
  try {
    const proxy = await startStallProxy(stack.natsHostPort)
    releases.push(() => proxy.close())
    const home = await mkdtemp(join(SOCKET_ROOT, "claude-envoy-stall-"))
    releases.push(() => rm(home, { recursive: true, force: true }))
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    await writeFile(
      join(configDirectory, "envoy.json"),
      JSON.stringify({ envoy: { enabled: true, url: stack.envoyUrl, natsUrls: [proxy.url] } }),
    )
    const sessionID = `${sessionPrefix}_${crypto.randomUUID().replaceAll("-", "")}`
    // A start its row's limit abandoned spawns no server into the row after it.
    ended.signal.throwIfAborted()
    const server = Bun.spawn(["bun", channelEntry], {
      cwd: home,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: sessionID,
        CLAUDE_PLUGIN_DATA: home,
        // The base the server and the mod below both resolve the link's socket from.
        XDG_RUNTIME_DIR: home,
        ENVOY_HEARTBEAT_MS: FREEZE_HEARTBEAT_MS,
        ...NO_TRANSPILER_CACHE,
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    })
    releases.push(async () => {
      server.kill("SIGTERM")
      const killed = Promise.withResolvers<void>()
      const killer = setTimeout(() => {
        server.kill("SIGKILL")
        killed.resolve()
      }, STOP_GRACE_MS)
      // A process sent SIGKILL runs no more of its own code, so from then on nothing it does
      // needs the proxy or writes into the home: their releases wait on no exit that may not come.
      await Promise.race([server.exited, killed.promise])
      clearTimeout(killer)
    })
    const socketPath = modSocketPath(sessionID, { XDG_RUNTIME_DIR: home })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    // Read before the probe below, so a server that writes while it starts never blocks on a
    // full pipe.
    let stderr = ""
    void (async () => {
      for await (const chunk of server.stderr.pipeThrough(new TextDecoderStream())) {
        stderr += chunk
      }
    })()
    // Every delivery reaches the model as the mod's own write - the row of a turn running, or
    // the prompt of a new turn while the session is idle, which this one is throughout (`show`
    // in `../src/mod-session`) - so the mod is this test's view of what the session was shown.
    const channel: string[] = []
    const mod = await startFakeMod(socketPath, {
      sessionId: sessionID,
      exited: server.exited,
      signal: ended.signal,
      answers: {
        "session.message": (args) => {
          channel.push(shownText(args))
          // The prompt an idle session took, which is what answers the frame: a prompt carries
          // no row of its own (`../hooks/mod-protocol.ts`, `ModCallResults["session.message"]`).
          return { as: "prompt" }
        },
      },
    })
    releases.push(() => mod.stop())
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
    server.stdin.write(
      `${JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" })}\n`,
    )
    // What has ended of the server, natsd and the listener, with its exit code or signal, or
    // undefined while all three run.
    const exit = (): string | undefined =>
      server.exitCode !== null || server.signalCode !== null
        ? `the channel server exited (${server.exitCode ?? server.signalCode}): ${stderr}`
        : stack.exited()
    return {
      server,
      proxy,
      sessionID,
      home,
      channel,
      ended: ended.signal,
      // The server runs on nats.js's real ping and dial timers and a real listener, so each wait
      // polls the real clock until its condition holds. Only a wait whose time the row asserts
      // has a deadline: the freeze scenario's recovery, within `RECOVERY_BOUND_MS` of the freeze
      // (`runFreezeScenario`). A wait given none ends at the row's limit, which names the row.
      // Every wait throws on the poll after the server, natsd or the listener exits, naming that
      // exit, and names it in place of the error a read meets once one has. Once its row has
      // ended it throws before each poll and in place of its answer, so a wait of a row its limit
      // abandoned does not poll on into the row after it.
      async waitFor(condition, deadline, what) {
        const doing = `waiting for ${what}`
        for (;;) {
          const met = await readInRow(ended.signal, doing, exit, condition)
          if (met) return
          const exited = exit()
          if (exited !== undefined) throw new Error(`${doing}: ${exited}`)
          if (deadline !== undefined && Date.now() > deadline) {
            throw new Error(`timed out waiting for ${what}: ${stderr}`)
          }
          await Bun.sleep(100)
        }
      },
      stop,
    }
  } catch (error) {
    await stop()
    throw error
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
    // The room one hang has in this file's step of the `envoy-session-stall` job
    // (`.github/workflows/ci-job-envoy-session-stall.yaml`) is 847 s: the job's
    // `timeout-minutes: 20`, less the 351 s from the job's start to this step's in the slowest of
    // its ten runs on this branch from d7aa4f9b to 919ee888 (run 45070043835, at a61f3fc7), and
    // less the 2 s at most that the steps run after a failed step took in those runs (the ci-ok
    // report, which runs `if: always()`, and the post steps). The steps after this one are not
    // counted: a step that fails skips them. Each limit below is sized for one hang, that hook
    // or row at its limit and every other at its worst, and keeps 30 s for the `afterAll`'s stop
    // (worst 4.7 s) and the teardown (a host's stop: up to 10 s before its server's SIGKILL, then
    // the home's removal and the proxy's close).
    let stack: EnvoyStack | undefined
    let stopStack: (() => Promise<void>) | undefined
    beforeAll(async () => {
      const start = startStoppableEnvoyStack({ work: stackDirectory })
      stopStack = start.stop
      stack = await start.started
      // A hang guard: what can hang is the stack's start, which builds natsd and the listener with
      // `go build` and waits for each to serve. It is the start pi-envoy's live describe makes
      // (`startStoppableEnvoyStack`). Worst measured cost of that start: 249 197.32 ms over 20 runs
      // at the 96 rung in the Job infra-1580-krows-kfh-tgvmp of the cluster's `builds` namespace,
      // two CPUs and 8 GiB (218 busy loops, at which row A's runs took 33.5 s against the
      // calibration's 31.2 s), 2026-11-22T08:32Z to 09:41Z. Six times the worst is 1 495 s, past
      // the room above, so the room decides: 847 s less the 30 s kept is 817 s, and 810_000 is
      // 3.3 times the worst.
    }, 810_000)
    afterAll(async () => {
      await stopStack?.()
      // A hang guard: what can hang is the stop of natsd or the listener, which a start cut off
      // makes too. Worst measured cost of the stop: 4 703.96 ms over the same 20 runs, so 60_000
      // is 12.8 times the worst. A stop that hangs to this limit fits the room above: 847 s less
      // the stack's slowest start (249 s, the `beforeAll` above) and the two rows' worst (189.7 s
      // each) leaves 218.6 s, and 60 s is inside it. This limit takes the place of the 30 s kept.
    }, 60_000)
    afterEach(async () => {
      // A row its limit abandoned never reaches its own `finally`: its host is stopped here, so
      // its waits throw and its server, home and proxy go with the row. A row whose limit fired
      // while its `finally` stopped its host left that host here, and its stop below waits for
      // those releases to settle. A row whose `finally` finished its stop left no host open.
      // Each host leaves the set before its stop is waited on, so a stop that hangs here is
      // waited on by this row's hook alone and not again by every row after it.
      for (const stop of [...openHosts]) {
        openHosts.delete(stop)
        await stop()
      }
      // A hang guard: what can hang is a host's stop. Its server's release ends 10 s after the
      // SIGTERM at the latest (`STOP_GRACE_MS`), when the server is sent SIGKILL. The home's
      // removal and the proxy's close come after that release and have no bound of their own.
      // Worst measured cost of a host's stop with its server running: 57.36 ms over 3 runs of
      // this file, idle on the issue's pod (two CPUs, 8 GiB), timed around the first row's
      // `stop`, 2026-11-25T04:47Z to 04:52Z, at 353017d0 with these edits. The server exited on
      // its SIGTERM each time, so no run reached the SIGKILL. No reading under load exists.
      // 30_000 is three times `STOP_GRACE_MS`. A server that ignores its SIGTERM holds the stop
      // 10 s of it, and the 20 s left for the rest are 349 times the whole stop's idle worst. A
      // stop that hangs to this limit fits the room above: 847 s less the stack's slowest start
      // (249 s, the `beforeAll` above), the two rows' worst (189.7 s each) and the 30 s kept
      // leaves 188.6 s, and 30 s is inside it.
    }, 30_000)

    test("a session whose NATS path freezes subscribes its relay subject again on the new connection within 60 s, registers a new epoch, and shows each message once", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      const { proxy, sessionID, channel, waitFor, stop } = await startChannelServer(
        stack,
        "ses_stall",
      )
      try {
        await waitFor(
          async () => (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok,
          undefined,
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
          stack,
          sessionID,
          proxy,
          shown: (marker) => channel.filter((content) => content.includes(marker)),
          waitFor,
        })
      } finally {
        await stop()
      }
      // A hang guard, not a bound on work: what can hang is a wait for the registration or a
      // message that never comes.
      // Worst measured cost: 120 895.29 ms over 20 runs at the 96 rung in the Job
      // infra-1580-krows-kc-4bm4r of the cluster's `builds` namespace, two CPUs and 8 GiB (199
      // busy loops, at which row A's runs took 39.1 s against the calibration's 31.2 s),
      // 2026-11-22T18:21Z to 20:50Z; 116 398.78 ms in the Job infra-1580-krows-kc-g4zvc (120
      // busy loops, row A at 34.2 s), 2026-11-22T15:21Z to 17:53Z; 90 893.78 ms at 2712deb2 in
      // the Job infra-1580-krows-kc-pw59w (145 busy loops, row A at 33.4 s), 2026-11-23T06:44Z
      // to 08:35Z; at b5a8b94f, in the Job infra-1580-t25-ce1-ww769 (189 busy loops, row A at
      // 36.8 s), 2026-11-23T17:50Z to 20:58Z, a run whose slower row took 189 698 ms and its
      // other 182 403 ms, so each row's worst is taken as 189 698 ms. Six times that is 1 138 s,
      // past the room above, so the room decides: 847 s less the stack's slowest start (249 s,
      // the `beforeAll` above), the other row's worst (189.7 s) and the 30 s kept is 378.3 s, and
      // 375_000 is 2.0 times the worst.
    }, 375_000)

    test("a session whose redial once hung still exits on SIGTERM", async () => {
      if (stack === undefined) throw new Error("the Envoy stack did not start")
      const { envoyUrl } = stack
      const { server, proxy, sessionID, ended, waitFor, stop } = await startChannelServer(
        stack,
        "ses_stall_exit",
      )
      try {
        await waitFor(
          async () =>
            (await fetch(`${envoyUrl}/v1/registry/${sessionID}`)).ok &&
            proxy.connections[0]?.subscribedAt !== undefined,
          undefined,
          "the session's registration and first subscription",
        )
        // The first redial after the freeze hangs (accepted, never answered); the next forwards.
        // The row asserts no time for this recovery, only the exit on SIGTERM below, so the wait
        // has no deadline: a recovery that never comes ends at the row's limit.
        proxy.stallNext(1)
        const frozenAt = proxy.freeze()
        await waitFor(
          () =>
            proxy.connections.some(
              (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined,
            ),
          undefined,
          "the session to subscribe again after the hung redial",
        )
        const signalledAt = Date.now()
        server.kill("SIGTERM")
        // No timer bounds the exit. A server that never exits holds the row to its limit, which
        // fails the row by name. Bun's runner then sends SIGTERM to every process the file's rows
        // started that still runs. This server handles SIGTERM once (`process.once` in
        // `../src/envoy-channel-server.ts`), so that second signal ends it, and the describe's
        // `afterEach` stops the host whatever the server does.
        await server.exited
        // Once the row has ended the body stops here, rather than assert on an exit it did not
        // cause.
        ended.throwIfAborted()
        const exitSeconds = ((Date.now() - signalledAt) / 1000).toFixed(1)
        console.log(`claude-envoy stall exit: SIGTERM -> exited after ${exitSeconds} s`)
        // It exited of itself, through its SIGTERM handler. A process a signal ended has that
        // signal here, and no exit code.
        expect(server.signalCode).toBeNull()
      } finally {
        await stop()
      }
      // A hang guard, not a bound on work: what can hang is a wait for the registration, for the
      // session to subscribe again, or an exit that never comes.
      // Worst measured cost: 177 296.68 ms over 20 runs at the 96 rung in the Job
      // infra-1580-krows-kc-4bm4r of the cluster's `builds` namespace, two CPUs and 8 GiB (199
      // busy loops, at which row A's runs took 39.1 s against the calibration's 31.2 s),
      // 2026-11-22T18:21Z to 20:50Z; 118 302.75 ms in the Job infra-1580-krows-kc-g4zvc (120
      // busy loops, row A at 34.2 s), 2026-11-22T15:21Z to 17:53Z; 111 289.38 ms at 2712deb2 in
      // the Job infra-1580-krows-kc-pw59w (145 busy loops, row A at 33.4 s), 2026-11-23T06:44Z
      // to 08:35Z; at b5a8b94f, in the Job infra-1580-t25-ce1-ww769 (189 busy loops, row A at
      // 36.8 s), 2026-11-23T17:50Z to 20:58Z, a run whose slower row took 189 698 ms and its
      // other 182 403 ms, so each row's worst is taken as 189 698 ms. Six times that is 1 138 s,
      // past the room above, so the room decides: 847 s less the stack's slowest start (249 s,
      // the `beforeAll` above), the other row's worst (189.7 s) and the 30 s kept is 378.3 s, and
      // 375_000 is 2.0 times the worst.
    }, 375_000)
  },
)
