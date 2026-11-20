// The Codex plugin's server runs the same session code as the Claude Code one
// (`startChannelSession`), so a Codex session whose NATS connection closes for good is given a new
// one too: Codex starts no plugin server again for a thread whose server lost its connection. The
// server is a whole process, started with `--host codex` as Codex starts it, beneath this test's
// process, which stands in for the `codex` process: the session record names this process and
// a stand-in `codex` binary that writes each `codex queue` it is asked for to a file, which is how
// every line reaches an idle Codex session (`../src/codex/codex-delivery`).
import { expect, test } from "bun:test"
import { readdir, readFile } from "node:fs/promises"
import { join } from "node:path"
import { agentStreamControlSubject, legacyAgentSubject } from "@legion/contracts"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { waitFor } from "./channel-session-harness"
import {
  CODEX_QUEUED,
  codexSession as codexSessionFor,
  count,
  PROCESS_WAIT_MS,
  ROW_TIMEOUT_MS,
  startReopenServer,
} from "./reopen-server"

/** A Codex thread id, in the shape Codex gives one. */
const SESSION = "019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5b"
const CONTROL = agentStreamControlSubject(SESSION)
const DIRECT = legacyAgentSubject(SESSION)
/** What every `codex queue` call wrote, one call after another. */
const QUEUED = CODEX_QUEUED
const codexSession = codexSessionFor(SESSION)

test(
  "a Codex session's NATS connection closed for good is dialed again: the relay reopens on a new connection at a new epoch, and the end of the outage is told once",
  async () => {
    const run = await startReopenServer({
      sessionId: SESSION,
      args: ["--host", "codex"],
      env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
      prepare: codexSession,
    })
    const { broker, gate, stderr, state } = run
    const queued = (): Promise<string> =>
      readFile(join(run.stateDirectory, QUEUED), "utf8").catch(() => "")
    const controlSubscriptions = (): number =>
      broker.subscribed.filter((subject) => subject === CONTROL).length
    try {
      // The live view takes its control subject as the session starts.
      await waitFor(
        async () => controlSubscriptions() === 1,
        "the live view's control subject",
        state,
        PROCESS_WAIT_MS,
      )
      const firstEpoch = run.deliveries.at(-1)?.epoch
      await run.closeForGood()

      // The session is told once that it receives nothing, naming why, as it is told everything:
      // through `codex queue`.
      await waitFor(
        async () =>
          count(await queued(), "Envoy inbound unavailable") === 1 &&
          stderr().includes("envoy-channel: this session receives nothing ("),
        "the outage told in the session and on stderr",
        state,
        PROCESS_WAIT_MS,
      )
      expect(await queued()).toContain(
        "because its relay subject could not be opened: Authorization Violation",
      )

      // The hub admits again: the relay subject is taken on a new connection and a new epoch is
      // registered, so the listener restarts the relay at its ack floor.
      const refusedUntil = run.deliveries.length
      gate.open()
      await waitFor(
        async () =>
          run.relaySubscriptions() === 2 &&
          broker.liveConnections === 1 &&
          run.deliveries.slice(refusedUntil).some((asked) => asked.epoch !== firstEpoch),
        "the relay subscription on a new connection and a registration of a new epoch",
        state,
        PROCESS_WAIT_MS,
      )
      await waitFor(
        async () => controlSubscriptions() === 2,
        "the live view's control subject on the new connection",
        state,
        PROCESS_WAIT_MS,
      )

      broker.deliverRelay(SESSION, {
        subject: DIRECT,
        seq: 3,
        payload: JSON.stringify({
          event_id: "evt-codex-after-reopen",
          source: "agent",
          source_session: "ses_peer",
          topic: DIRECT,
          dedupe_key: "delivery-codex-after-reopen",
          issued_at: 1_760_000_000_000,
          payload_summary: "codex peer message after the reopen",
        }),
        reply: "_INBOX.codex-after-reopen",
      })
      const answers = (): string[] =>
        broker.published
          .filter((frame) => frame.subject === "_INBOX.codex-after-reopen")
          .map((frame) => frame.payload)
      await waitFor(
        async () => answers().includes("done"),
        "the frame relayed on the new connection to be queued and answered done",
        state,
        PROCESS_WAIT_MS,
      )
      expect(answers()).toEqual(["received", "done"])
      expect(count(await queued(), "codex peer message after the reopen")).toBe(1)

      // The accepted registration ends the outage, told once in the session and on stderr.
      await waitFor(
        async () =>
          count(await queued(), "Envoy inbound restored") === 1 &&
          stderr().includes("envoy-channel: this session receives again"),
        "the end of the outage told in the session and on stderr",
        state,
        PROCESS_WAIT_MS,
      )
      expect(count(stderr(), "envoy-channel: this session receives again")).toBe(1)
      expect(count(await queued(), "Envoy inbound unavailable")).toBe(1)
      expect(stderr()).toContain(
        "envoy-channel: the NATS connection closed for good (Authorization Violation); dialing a new one",
      )
    } finally {
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)

// A Codex server's start can wait on its connection: the startup flush is asked again through every
// redial (`flushThroughRedials`). A stop meanwhile ends that wait at once, as it does the Claude
// Code server's, and the process exits as an asked-for stop does. Here the hub stops answering
// PINGs just as the start reads the listener's interests, the step before the flush, so the flush
// never comes back.
test(
  "a stop while a Codex session's start waits on its flush ends the start at once, and the server exits leaving no connection open",
  async () => {
    const interests = `/v1/interests/${SESSION}`
    const run = await startReopenServer({
      sessionId: SESSION,
      args: ["--host", "codex"],
      env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
      prepare: codexSession,
      awaitRegistration: false,
      onRequest: (method, path, broker) => {
        if (method === "GET" && path === interests) broker.answerPings = false
      },
    })
    const { broker, server, stderr, state } = run
    try {
      await waitFor(
        async () => run.requests.includes(`GET ${interests}`),
        "the start to read the listener's interests, the step before its flush",
        state,
        PROCESS_WAIT_MS,
      )
      expect(run.sessions).toEqual([])
      server.kill("SIGTERM")
      // Bounded, so a server the stop does not end fails here by name rather than at the row's
      // timeout.
      const exited = await Promise.race([
        server.exited,
        Bun.sleep(PROCESS_WAIT_MS).then(() => "still running" as const),
      ])
      expect({ exited, stderr: stderr() }).toMatchObject({ exited: 0 })
      await run.stderrEnded
      expect(stderr()).not.toContain("Codex Envoy channel server failed")
      // Nothing registered: the start ended before its first registration.
      expect(run.sessions).toEqual([])
      await waitFor(
        async () => broker.liveConnections === 0,
        "no connection left open",
        state,
        PROCESS_WAIT_MS,
      )
    } finally {
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)

// The dial before it waits too: a hub the server cannot reach is dialed again for the server's
// life. A stop meanwhile ends the dial and the process exits as an asked-for stop does, not as a
// failed start.
test(
  "a stop while a Codex session's first dial is dialed again ends the dial, and the server exits as an asked-for stop does",
  async () => {
    const run = await startReopenServer({
      sessionId: SESSION,
      args: ["--host", "codex"],
      env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
      prepare: codexSession,
      awaitRegistration: false,
      gateOpen: false,
    })
    const { broker, server, stderr, state } = run
    try {
      await waitFor(
        async () => stderr().includes("; dialing again in"),
        "a refused dial dialed again",
        state,
        PROCESS_WAIT_MS,
      )
      server.kill("SIGTERM")
      const exited = await Promise.race([
        server.exited,
        Bun.sleep(PROCESS_WAIT_MS).then(() => "still running" as const),
      ])
      // Its stderr to its end once it exited; a process still running is named as it stands.
      if (exited !== "still running") await run.stderrEnded
      expect({ exited, stderr: stderr() }).toMatchObject({ exited: 0 })
      expect(stderr()).not.toContain("Codex Envoy channel server failed")
      expect(run.sessions).toEqual([])
      expect(broker.connections).toBe(0)
    } finally {
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)

// A stop that lands while the start's first registration is out lets the start finish, and the
// live view it then starts is stopped at once, before the session's shutdown closes the relay:
// its control subject goes first. Left to the shutdown, it would go only at the drain, after the
// relay.
test(
  "a stop while a Codex session's start registers stops its live view before the session shuts down",
  async () => {
    const held = Promise.withResolvers<void>()
    let asked = false
    const run = await startReopenServer({
      sessionId: SESSION,
      args: ["--host", "codex"],
      env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
      prepare: codexSession,
      awaitRegistration: false,
      onRequest: (method, path) => {
        if (method !== "POST" || path !== "/v1/interests/subscribe") return undefined
        asked = true
        return held.promise
      },
    })
    const { broker, server, stderr, state } = run
    // The server's entry among this `codex` process's servers (the test process is the session's
    // `codex`, `codexSession`): its stop removes it, so an empty directory is the stop having run.
    const servers = join(run.stateDirectory, "codex-servers", String(process.pid))
    const entries = async () =>
      (await readdir(servers).catch(() => [] as string[])).filter((name) => name.endsWith(".json"))
    try {
      await waitFor(async () => asked, "the first registration, held", state, PROCESS_WAIT_MS)
      await waitFor(
        async () => (await entries()).length === 1,
        "the server's entry",
        state,
        PROCESS_WAIT_MS,
      )
      server.kill("SIGTERM")
      // The stop runs before the held registration is answered: it has removed the entry.
      await waitFor(
        async () => (await entries()).length === 0,
        "the stop to have run",
        state,
        PROCESS_WAIT_MS,
      )
      held.resolve()
      const exited = await Promise.race([
        server.exited,
        Bun.sleep(PROCESS_WAIT_MS).then(() => "still running" as const),
      ])
      expect({ exited, stderr: stderr() }).toMatchObject({ exited: 0 })
      const relay = deliverSubject(SESSION)
      expect(
        broker.unsubscribed.filter((subject) => subject === CONTROL || subject === relay)[0],
      ).toBe(CONTROL)
    } finally {
      held.resolve()
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)
