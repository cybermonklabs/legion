// A connection that closes for good after the start's dial and before the start's flush reaches
// the server: neither Claude Code nor Codex starts a channel server again, so a start that failed
// here would leave the session without Envoy for its life. The connection is replaced instead, as
// one lost while the session runs is, and the session starts on the new one. nats.js closes a
// connection for good on a repeated authorization refusal under an nkey seed, which
// `FakeNatsServer.closeClients` sends; the hub stops answering PINGs as the start reads the
// listener's interests, the step before its flush, so the flush is waiting when the close lands,
// and answers them again once the close is taken up, so no later dial waits on a PONG.
import { afterEach, expect, test } from "bun:test"
import { nkeys } from "nats"
import { waitFor } from "./channel-session-harness"
import { firedAfter, lastDialAtExit, markDialTimers, readDialTimers } from "./dial-timer"
import {
  codexSession,
  count,
  type ReopenServer,
  type ReopenServerOptions,
  startReopenServer,
} from "./reopen-server"
import { runTeardown } from "./teardown"

/** Each row's server and its stand-ins, stopped after the row whichever way it ended. */
const stops: (() => Promise<void>)[] = []
afterEach(() => runTeardown(stops))

interface Host {
  readonly name: string
  readonly sessionId: string
  readonly args: readonly string[]
  readonly env: (stateDirectory: string) => Readonly<Record<string, string>>
  readonly prepare?: (stateDirectory: string) => Promise<void>
}

const HOSTS: readonly Host[] = [
  {
    name: "Claude Code",
    sessionId: "ses_start_reopen",
    args: [],
    env: (stateDirectory) => ({
      CLAUDE_CODE_SESSION_ID: "ses_start_reopen",
      CLAUDE_PLUGIN_DATA: stateDirectory,
    }),
  },
  {
    name: "Codex",
    // A Codex thread id, in the shape Codex gives one.
    sessionId: "019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5d",
    args: ["--host", "codex"],
    env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
    prepare: codexSession("019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5d"),
  },
]

const CLOSED = "envoy-channel: the NATS connection closed for good (Authorization Violation)"
const GAVE_UP = "envoy-channel: gave up opening a new NATS connection"

/**
 * `host`'s server signing in with an nkey seed, whose start's flush is left waiting: the hub
 * stops answering PINGs as the start reads the listener's interests, and resolves once the flush
 * is out. `recordDialTimer` runs the server with the dial-timer record (`./dial-timer`).
 */
async function startWithPendingFlush(host: Host, recordDialTimer = false): Promise<ReopenServer> {
  const seed = new TextDecoder().decode(nkeys.createUser().getSeed())
  const interests = `/v1/interests/${host.sessionId}`
  let pingsAtInterests: number | undefined
  const options: ReopenServerOptions = {
    sessionId: host.sessionId,
    args: host.args,
    nonce: "fake-nonce",
    env: (stateDirectory) => ({ ...host.env(stateDirectory), NATS_NKEY_SEED: seed }),
    ...(host.prepare === undefined ? {} : { prepare: host.prepare }),
    awaitRegistration: false,
    recordDialTimer,
    onRequest: (method, path, broker) => {
      if (method !== "GET" || path !== interests || pingsAtInterests !== undefined) return
      broker.answerPings = false
      pingsAtInterests = broker.pings
    },
  }
  const run = await startReopenServer(options)
  stops.push(run.stop)
  await whileRunning(
    run,
    "the start's flush to wait on the hub",
    async () => pingsAtInterests !== undefined && run.broker.pings > pingsAtInterests,
  )
  return run
}

/**
 * Closes the connection for good while the start's flush waits, with the hub refusing each dial
 * after it, and waits until the server has taken the close up and a replacement dial was refused.
 * The hub then answers PINGs again: the start's flush it held is gone with the connection, and a
 * dial the hub admits later must not wait on a PONG it never sends.
 */
async function closeWhileFlushWaits(run: ReopenServer): Promise<void> {
  const { broker, stderr } = run
  broker.refuseConnects = true
  await broker.closeClients()
  await whileRunning(
    run,
    "the close taken up and a replacement dial refused",
    async () => stderr().includes(`${CLOSED}; dialing a new one`) && stderr().includes(GAVE_UP),
  )
  broker.answerPings = true
}

/** Waits for `condition`, failing at once, naming the server's end, if the server exits first. */
function whileRunning(
  run: ReopenServer,
  what: string,
  condition: () => Promise<boolean>,
): Promise<void> {
  return waitFor(
    async () => {
      if (run.server.exitCode !== null) {
        throw new Error(`the server exited while waiting for ${what}: ${run.state()}`)
      }
      return condition()
    },
    what,
    run.state,
  )
}

for (const host of HOSTS) {
  test(`${host.name}: a connection closed for good while the start's flush waits is replaced, the session starts on the new one and registers with its relay, and the server keeps running`, async () => {
    const run = await startWithPendingFlush(host)
    const { broker, stderr } = run
    await closeWhileFlushWaits(run)
    // The session is told once that it receives nothing, naming why; nothing was registered.
    await whileRunning(run, "the outage told", async () =>
      stderr().includes("envoy-channel: this session receives nothing ("),
    )
    expect(count(stderr(), "this session receives nothing")).toBe(1)
    expect(stderr()).toContain(
      "because its relay subject could not be opened: Authorization Violation",
    )
    expect(run.sessions).toEqual([])

    // The hub admits again: the session registers with its relay on a new connection.
    broker.refuseConnects = false
    await whileRunning(
      run,
      "a registration with the relay on a new connection, and the end of the outage",
      async () =>
        run.deliveries.length > 0 &&
        run.relaySubscriptions() === 2 &&
        broker.liveConnections === 1 &&
        stderr().includes("envoy-channel: this session receives again"),
    )
    expect(run.sessions.every((session) => session === host.sessionId)).toBe(true)
    expect(run.deliveries.at(-1)).toMatchObject({ consumer: true })
    expect(count(stderr(), "envoy-channel: this session receives again")).toBe(1)
    expect(count(stderr(), "this session receives nothing")).toBe(1)
    // The process keeps running: a heartbeat later it has not exited, and failed nothing.
    const registered = run.deliveries.length
    await whileRunning(run, "a heartbeat after the registration", async () => {
      return run.deliveries.length > registered
    })
    expect(run.server.exitCode).toBeNull()
    expect(stderr()).not.toContain("channel server failed")
  })

  test(`${host.name}: a stop while that replacement is dialed ends the dial, and the server exits 0 leaving no connection open`, async () => {
    const run = await startWithPendingFlush(host, true)
    const { broker, server, stderr } = run
    const record = run.dialTimerRecord
    if (record === undefined) throw new Error("the server runs with no dial-timer record")
    await closeWhileFlushWaits(run)
    // The hub holds the next connection, its INFO withheld, and still refuses every CONNECT: a
    // dial that reached it before the hold is refused, as the ones before it were, and the next
    // heartbeat's dial is in flight when the stop lands.
    markDialTimers(record, "hold")
    const acceptedAt = broker.connections
    broker.holdNext()
    await whileRunning(run, "a replacement dial in flight", async () => {
      return broker.connections > acceptedAt
    })

    server.kill("SIGTERM")
    const exited = await run.exitCode("the server's exit after SIGTERM")
    await run.stderrEnded
    expect({ exited, stderr: stderr() }).toMatchObject({ exited: 0 })
    expect(stderr()).not.toContain("channel server failed")
    // How its dials ended, from the process's own record rather than a clock: no dial waited out
    // nats.js's dial timer from the hold on, and the held dial's was still armed as the process
    // exited. A stop that waits for the held dial exits 0 too, but only once that timer fired; a
    // dial the hub admitted and never answered fires its timer before the stop.
    const timers = await readDialTimers(record)
    expect(
      { fired: firedAfter(timers, "hold"), held: lastDialAtExit(timers) },
      `the dial timers from the hold to the exit: ${JSON.stringify(timers)}`,
    ).toEqual({ fired: [], held: "armed and not fired" })
    // Nothing was registered with a relay: the session never had a live connection.
    expect(run.sessions).toEqual([])
    // The dial in flight answers now, to nobody: the process that made it is gone.
    broker.release()
    await waitFor(async () => broker.liveConnections === 0, "no connection left open", run.state)
  })
}
