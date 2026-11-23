// A connection that closes for good after the start's dial and before the start's flush reaches
// the server: neither Claude Code nor Codex starts a channel server again, so a start that failed
// here would leave the session without Envoy for its life. The connection is replaced instead, as
// one lost while the session runs is, and the session starts on the new one. nats.js closes a
// connection for good on a repeated authorization refusal under an nkey seed, which
// `FakeNatsServer.closeClients` sends; the hub stops answering PINGs as the start reads the
// listener's interests, the step before its flush, so the flush is waiting when the close lands.
import { expect, test } from "bun:test"
import { nkeys } from "nats"
import { waitFor } from "./channel-session-harness"
import {
  codexSession,
  count,
  PROCESS_WAIT_MS,
  type ReopenServer,
  type ReopenServerOptions,
  ROW_TIMEOUT_MS,
  startReopenServer,
} from "./reopen-server"

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
 * is out.
 */
async function startWithPendingFlush(host: Host): Promise<ReopenServer> {
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
    onRequest: (method, path, broker) => {
      if (method !== "GET" || path !== interests || pingsAtInterests !== undefined) return
      broker.answerPings = false
      pingsAtInterests = broker.pings
    },
  }
  const run = await startReopenServer(options)
  try {
    await whileRunning(
      run,
      "the start's flush to wait on the hub",
      async () => pingsAtInterests !== undefined && run.broker.pings > pingsAtInterests,
    )
    return run
  } catch (error) {
    await run.stop()
    throw error
  }
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
    PROCESS_WAIT_MS,
  )
}

for (const host of HOSTS) {
  test(
    `${host.name}: a connection closed for good while the start's flush waits is replaced, the session starts on the new one and registers with its relay, and the server keeps running`,
    async () => {
      const run = await startWithPendingFlush(host)
      const { broker, stderr } = run
      try {
        // Closed for good, and the hub refuses each dial after it for a while.
        broker.refuseConnects = true
        await broker.closeClients()
        await whileRunning(
          run,
          "the close taken up and a replacement dial refused",
          async () =>
            stderr().includes(`${CLOSED}; dialing a new one`) && stderr().includes(GAVE_UP),
        )
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
        broker.answerPings = true
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
      } finally {
        await run.stop()
      }
    },
    ROW_TIMEOUT_MS,
  )

  test(
    `${host.name}: a stop while that replacement is dialed ends the dial, and the server exits 0 leaving no connection open`,
    async () => {
      const run = await startWithPendingFlush(host)
      const { broker, server, stderr } = run
      try {
        broker.refuseConnects = true
        await broker.closeClients()
        await whileRunning(
          run,
          "the close taken up and a replacement dial refused",
          async () =>
            stderr().includes(`${CLOSED}; dialing a new one`) && stderr().includes(GAVE_UP),
        )
        // The next heartbeat's dial reaches the hub, which withholds its INFO: the dial is in
        // flight when the stop lands.
        const acceptedAt = broker.connections
        broker.refuseConnects = false
        broker.holdNext()
        await whileRunning(run, "a replacement dial in flight", async () => {
          return broker.connections > acceptedAt
        })

        server.kill("SIGTERM")
        // Bounded, so a server the stop does not end fails here by name rather than at the row's
        // timeout.
        const exited = await Promise.race([
          server.exited,
          Bun.sleep(PROCESS_WAIT_MS).then(() => "still running" as const),
        ])
        if (exited !== "still running") await run.stderrEnded
        expect({ exited, stderr: stderr() }).toMatchObject({ exited: 0 })
        expect(stderr()).not.toContain("channel server failed")
        // Nothing was registered with a relay: the session never had a live connection.
        expect(run.sessions).toEqual([])
        // The dial in flight answers now, to nobody: the process that made it is gone.
        broker.release()
        await waitFor(
          async () => broker.liveConnections === 0,
          "no connection left open",
          run.state,
          PROCESS_WAIT_MS,
        )
      } finally {
        await run.stop()
      }
    },
    ROW_TIMEOUT_MS,
  )
}
