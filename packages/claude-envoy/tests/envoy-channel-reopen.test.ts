// A channel server whose NATS connection closes for good while its session runs dials a new one:
// Claude Code starts no channel server again, so without it the session would receive nothing for
// the rest of its life. nats.js closes a connection for good on a repeated authorization refusal
// under an nkey seed or no credential, which `FakeNatsServer.closeClients` sends, and on an
// authenticator that throws, which the broker token's does for a hub that sends no nonce; the gate
// in front of the broker refuses the redials until the test opens it. Whole processes, real
// nats.js and real time: the server's own dial waits are what these rows watch. A `/clear` is the
// handoff file the server polls, which names this test's process, the server's parent.
import { expect, test } from "bun:test"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import { agentStreamControlSubject, legacyAgentSubject } from "@legion/contracts"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import type { DeliveryRequest } from "@legion/envoy-client/transport"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { nkeys } from "nats"
import { modSocketPath } from "../hooks/mod-protocol"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import { waitFor } from "./channel-session-harness"
import { startFakeMod } from "./fake-mod"
import { FakeNatsServer } from "./fake-nats-server"
import { natsGate } from "./nats-gate"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")
const SESSION = "ses_reopen"
const RELAY = deliverSubject(SESSION)
const CONTROL = agentStreamControlSubject(SESSION)
const DIRECT = legacyAgentSubject(SESSION)
/** Each wait's deadline: every step here is a whole server process, which a loaded machine
 *  starts and runs slowly. */
const PROCESS_WAIT_MS = 15_000
/**
 * The wait for a spawned server's first stderr line (its start notice). Most of a start is Bun
 * loading and compiling this package, which a loaded machine slows most: a server here once wrote
 * nothing for 15 s at a load of 700 on 288 cores, and the package's other spawned-server rows give
 * a whole run 20 s, so a start alone gets half again that.
 */
const SERVER_START_MS = 30_000
/** Each row's: above its chain of waits, so a stuck wait names its step. */
const ROW_TIMEOUT_MS = 180_000

/** How many times `text` holds `part`. */
function count(text: string, part: string): number {
  return text.split(part).length - 1
}

/**
 * The process that owns the TCP socket bound to local `port`, as `ss` names it (`bun pid=123`):
 * what tells this row's server from any other process on the machine that dialed a port this row
 * now holds.
 */
async function socketOwner(port: number): Promise<string> {
  const ss = Bun.spawn(["ss", "-tnpH", `( sport = :${port} )`], {
    stdout: "pipe",
    stderr: "ignore",
  })
  const text = await new Response(ss.stdout).text()
  const owner = /users:\(\("([^"]+)",pid=(\d+)/.exec(text)
  return owner === null ? `unknown (${text.trim() || "no socket"})` : `${owner[1]} pid=${owner[2]}`
}

/**
 * A channel server whose NATS servers are a gate in front of `broker`, with the gate open, and a
 * listener that gives every registration the session's consumer, recording what each asked for.
 * `nonce` is the hub's (a seed or a broker token signs it), and `env` adds the credential's
 * variables.
 */
async function startServer(
  options: { readonly nonce?: string; readonly env?: Readonly<Record<string, string>> } = {},
) {
  const broker = new FakeNatsServer(options.nonce === undefined ? {} : { nonce: options.nonce })
  // Each client's owner is looked up as the gate accepts it, while its socket is still up.
  const owners = new Map<number, Promise<string>>()
  const gate = natsGate(Number(new URL(broker.url).port), (leg) => {
    owners.set(leg.clientPort, socketOwner(leg.clientPort))
  })
  gate.open()
  const requests: string[] = []
  /** Each registration's session id and delivery request, in arrival order. */
  const sessions: string[] = []
  const deliveries: DeliveryRequest[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      requests.push(`${request.method} ${url.pathname}`)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          delivery: DeliveryRequest
        }
        sessions.push(body.session_id)
        deliveries.push(body.delivery)
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  // A short root keeps the mod socket's path under its 100-byte limit whatever TMPDIR is.
  const stateDirectory = await mkdtemp("/tmp/ce-reopen-")
  const server = Bun.spawn(["bun", entry], {
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: gate.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      // Several heartbeats register while the connection is down, so "told once" is told once
      // over several refusals.
      ENVOY_HEARTBEAT_MS: "200",
      CLAUDE_CODE_SESSION_ID: SESSION,
      CLAUDE_PLUGIN_DATA: stateDirectory,
      XDG_RUNTIME_DIR: stateDirectory,
      ...options.env,
    },
    stdin: "pipe",
    stdout: "ignore",
    stderr: "pipe",
  })
  let stderr = ""
  const decoder = new TextDecoder()
  /** Settles once the server's stderr has ended: what a row reads after the process exits. */
  const stderrEnded = (async () => {
    for await (const chunk of server.stderr) stderr += decoder.decode(chunk, { stream: true })
  })()
  const relaySubscriptions = (): number =>
    broker.subscribed.filter((subject) => subject === RELAY).length
  const state = (): string =>
    `exit code ${server.exitCode}; relay subscriptions ${relaySubscriptions()}; live connections ${broker.liveConnections}; stderr:\n${stderr}`
  const stop = async (): Promise<void> => {
    server.kill("SIGKILL")
    await server.exited
    listener.stop(true)
    gate.stop()
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
  /**
   * Each CONNECT the hub saw: its connection name, whether it presented a token, the port its
   * client dialed the gate from, and the process that owns that port, beside this server's pid.
   */
  const describeConnects = async (): Promise<string> => {
    const seen = await Promise.all(
      broker.connects.map(async (connect, index) => {
        const leg = gate.legs.find((each) => each.upstreamPort === broker.connectPorts[index])
        const owner =
          leg === undefined
            ? "not through the gate"
            : await (owners.get(leg.clientPort) ?? "not looked up")
        return `#${index} name=${String(connect["name"])} token=${typeof connect["auth_token"] === "string"} client port=${leg?.clientPort ?? "?"} owner=${owner}`
      }),
    )
    return `this server is pid ${server.pid}; ${seen.join("; ")}`
  }
  try {
    await waitFor(
      async () => stderr.length > 0 || deliveries.length > 0,
      "the server's first stderr line",
      state,
      SERVER_START_MS,
    )
    await waitFor(
      async () => deliveries.length > 0 && relaySubscriptions() === 1,
      "the session's first registration and its relay subscription",
      state,
      PROCESS_WAIT_MS,
    )
  } catch (error) {
    await stop()
    throw error
  }
  return {
    broker,
    gate,
    server,
    stateDirectory,
    requests,
    sessions,
    deliveries,
    stderr: () => stderr,
    stderrEnded,
    relaySubscriptions,
    state,
    describeConnects,
    /** Fails naming every CONNECT the hub saw and the process each came from, unless `held`: a
     *  CONNECT check that fails says whose CONNECT it read. */
    async checkConnects(held: boolean, what: string): Promise<void> {
      if (!held) throw new Error(`${what}: ${await describeConnects()}`)
    },
    /** Closed for good: the hub refuses the session twice and nats.js gives up, and every
     *  redial is refused until the gate opens again. */
    async closeForGood(): Promise<void> {
      gate.shut()
      await broker.closeClients()
    },
    stop,
  }
}

/** The fake mod of `run`'s session, and the notices this server wrote into it under `heading`: a
 *  notice is the one card that names no Envoy event. */
async function sessionMod(run: { readonly stateDirectory: string }) {
  const socketPath = modSocketPath(SESSION, { XDG_RUNTIME_DIR: run.stateDirectory })
  if (socketPath === undefined) throw new Error("the session names no mod socket")
  const mod = await startFakeMod(socketPath, { sessionId: SESSION })
  const notices = (heading: string) =>
    mod
      .madeOf("session.message")
      .filter((args) => args.eventId === undefined && args.message.text.includes(heading))
  return { mod, notices }
}

test("a NATS connection closed for good while the session runs is dialed again: the outage is told once naming why, and the relay reopens on the new connection", async () => {
  const run = await startServer()
  const { broker, gate, stderr, state } = run
  const { mod, notices } = await sessionMod(run)
  const shown = (eventId: string) =>
    mod.madeOf("session.message").filter((args) => args.eventId === eventId)
  const controlSubscriptions = () =>
    broker.subscribed.filter((subject) => subject === CONTROL).length

  try {
    // The live view takes its control subject once the server binds the mod's socket.
    await waitFor(
      async () => controlSubscriptions() === 1,
      "the live view's control subject",
      state,
      PROCESS_WAIT_MS,
    )
    const firstEpoch = run.deliveries.at(-1)?.epoch
    await run.closeForGood()

    // Redials refused for a while, and heartbeats refused meanwhile: the session holds no relay
    // subject, so each registration is refused before it reaches the listener.
    await waitFor(
      async () =>
        count(stderr(), "; dialing again in") >= 2 &&
        stderr().includes("registry heartbeat failed (this session holds no relay subject") &&
        notices("Envoy inbound unavailable").length > 0,
      "refused redials, a refused heartbeat and the outage notice",
      state,
      PROCESS_WAIT_MS,
    )
    expect(stderr()).toContain(
      "envoy-channel: the NATS connection closed for good (Authorization Violation); dialing a new one",
    )
    // Each redial is warned of as the start's dial is: the servers by their setting, never by
    // any part of their address.
    expect(stderr()).toContain(
      "could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential:",
    )
    expect(stderr()).not.toContain(gate.url.slice("nats://".length))
    // The refused registration names the failure, and the outage is told once over every refusal.
    const why =
      "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing, because its NATS connection failed: Authorization Violation"
    expect(notices("Envoy inbound unavailable")).toHaveLength(1)
    expect(notices("Envoy inbound unavailable")[0]?.message.text).toContain(why)
    expect(count(stderr(), `envoy-channel: this session receives nothing (${why})`)).toBe(1)
    expect(count(stderr(), "this session receives nothing")).toBe(1)

    // The hub admits again: the relay subject is taken on a new connection, and a new epoch is
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
        event_id: "evt-after-reopen",
        source: "agent",
        source_session: "ses_peer",
        topic: DIRECT,
        dedupe_key: "delivery-after-reopen",
        issued_at: 1_760_000_000_000,
        payload_summary: "peer message after the reopen",
      }),
      reply: "_INBOX.after-reopen",
    })
    const answers = () =>
      broker.published
        .filter((frame) => frame.subject === "_INBOX.after-reopen")
        .map((frame) => frame.payload)
    await waitFor(
      async () => answers().includes("done"),
      "the frame relayed on the new connection to be written and answered done",
      state,
      PROCESS_WAIT_MS,
    )
    expect(shown("evt-after-reopen")).toHaveLength(1)
    expect(answers()).toEqual(["received", "done"])

    // The accepted registration ends the outage, told once on stderr and in the session.
    await waitFor(
      async () =>
        notices("Envoy inbound restored").length > 0 &&
        stderr().includes("envoy-channel: this session receives again"),
      "the restored notice, in the session and on stderr",
      state,
      PROCESS_WAIT_MS,
    )
    expect(notices("Envoy inbound restored")).toHaveLength(1)
    expect(
      count(stderr(), "envoy-channel: this session receives again: its registration was accepted"),
    ).toBe(1)
    expect(notices("Envoy inbound unavailable")).toHaveLength(1)
  } finally {
    await mod.stop()
    await run.stop()
  }
}, ROW_TIMEOUT_MS)

test("a stop while the server dials a new NATS connection ends the dials, and the server exits leaving no connection open", async () => {
  const run = await startServer()
  const { broker, gate, server, stderr, state } = run
  try {
    await run.closeForGood()
    await waitFor(
      async () => count(stderr(), "; dialing again in") >= 1,
      "a refused redial",
      state,
      PROCESS_WAIT_MS,
    )
    // The next dial reaches the hub, which withholds its INFO, so the dial is still in flight
    // when the stop lands.
    const accepted = broker.connections
    broker.holdNext()
    gate.open()
    await waitFor(
      async () => broker.connections > accepted,
      "a redial in flight",
      state,
      PROCESS_WAIT_MS,
    )
    const registered = run.deliveries.length

    server.kill("SIGTERM")
    // Bounded, so a server the stop does not end fails here by name rather than at the row's
    // timeout.
    const exited = await Promise.race([
      server.exited,
      Bun.sleep(PROCESS_WAIT_MS).then(() => "still running"),
    ])
    expect(exited).toBe(0)
    // Everything it wrote, before reading what it did not write.
    await run.stderrEnded

    // The dial in flight answers now, to nobody: the process that made it is gone.
    broker.release()
    await waitFor(
      async () => broker.liveConnections === 0,
      "no connection left open",
      state,
      PROCESS_WAIT_MS,
    )
    expect(run.requests).toContain(`DELETE /v1/sessions/${SESSION}`)
    // Nothing reopened the relay or registered after the stop.
    expect(run.relaySubscriptions()).toBe(1)
    expect(run.deliveries).toHaveLength(registered)
    expect(stderr()).not.toContain("Claude Envoy channel server failed")
    expect(stderr()).not.toContain("gave up opening a new NATS connection")
  } finally {
    await run.stop()
  }
}, ROW_TIMEOUT_MS)

test("a reopen refused for good under an nkey seed is dialed again at the next heartbeat, and the relay reopens once the hub admits", async () => {
  const user = nkeys.createUser()
  const run = await startServer({
    nonce: "fake-nonce",
    env: { NATS_NKEY_SEED: new TextDecoder().decode(user.getSeed()) },
  })
  const { broker, stderr, state } = run
  const { mod, notices } = await sessionMod(run)
  const gaveUp = "envoy-channel: gave up opening a new NATS connection"
  try {
    await run.checkConnects(
      broker.connects[0]?.["nkey"] === user.getPublicKey(),
      "the start's CONNECT does not sign with the seed",
    )
    const firstEpoch = run.deliveries.at(-1)?.epoch
    // Closed for good, and the hub refuses each dial after it: a seed refused is refused again, so
    // no redial inside a dial cures it, and only a later dial can.
    broker.refuseConnects = true
    await broker.closeClients()
    await waitFor(
      async () => count(stderr(), gaveUp) >= 2 && notices("Envoy inbound unavailable").length > 0,
      "a reopen refused for good, and another at a later heartbeat",
      state,
      PROCESS_WAIT_MS,
    )
    expect(stderr()).toContain(
      `${gaveUp} (could not connect to NATS at the server ENVOY_NATS_URL names presenting an nkey seed: Authorization Violation); dialing again at the next heartbeat`,
    )
    expect(count(stderr(), "; dialing again in")).toBe(0)
    // Told once over every refusal, naming the close.
    expect(notices("Envoy inbound unavailable")).toHaveLength(1)
    expect(notices("Envoy inbound unavailable")[0]?.message.text).toContain(
      "because its NATS connection failed: Authorization Violation",
    )
    expect(count(stderr(), "this session receives nothing")).toBe(1)

    // The hub admits again: a heartbeat's dial opens a connection, signed with the seed, and the
    // relay reopens on it at a new epoch.
    const refusedUntil = run.deliveries.length
    broker.refuseConnects = false
    await waitFor(
      async () =>
        run.relaySubscriptions() === 2 &&
        broker.liveConnections === 1 &&
        run.deliveries.slice(refusedUntil).some((asked) => asked.epoch !== firstEpoch) &&
        notices("Envoy inbound restored").length > 0 &&
        stderr().includes("envoy-channel: this session receives again"),
      "the relay on a heartbeat's new connection, and the restored notice, in the session and on stderr",
      state,
      PROCESS_WAIT_MS,
    )
    await run.checkConnects(
      broker.connects.at(-1)?.["nkey"] === user.getPublicKey(),
      "the new connection's CONNECT does not sign with the seed",
    )
    expect(notices("Envoy inbound restored")).toHaveLength(1)
    expect(
      count(stderr(), "envoy-channel: this session receives again: its registration was accepted"),
    ).toBe(1)
    expect(notices("Envoy inbound unavailable")).toHaveLength(1)
  } finally {
    await mod.stop()
    await run.stop()
  }
}, ROW_TIMEOUT_MS)

/** A secrets broker whose token exchange mints a token that lives fifteen minutes, each kept in
 *  `minted`: `channel-nats.test.ts`'s stand-in, with a life no keeper renews inside a row. */
function tokenBroker() {
  const minted: string[] = []
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      const token = fixtureSecret("channel")
      minted.push(token)
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 900,
      })
    },
  })
  return { url: `http://127.0.0.1:${server.port}`, minted, stop: () => server.stop(true) }
}

test("under the session's broker token a connection closed for good is reopened presenting a token, and a /clear after it moves the new connection to the new id", async () => {
  const tokens = tokenBroker()
  const machine = await mkdtemp("/tmp/ce-reopen-machine-")
  const tokenFile = join(machine, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const next = "ses_reopen_next"
  const cleanUp = async (): Promise<void> => {
    tokens.stop()
    await rm(machine, { recursive: true, force: true })
  }
  const started = await startServer({
    nonce: "fake-nonce",
    env: {
      NATS_AUTH: "broker",
      AGENT_SECRETS_URL: tokens.url,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
  }).catch(async (error: unknown) => {
    await cleanUp()
    throw error
  })
  const { broker, stderr, state } = started
  try {
    const tokenOf = (connect: Record<string, unknown> | undefined): string =>
      String(connect?.["auth_token"])
    await started.checkConnects(
      tokens.minted.includes(tokenOf(broker.connects[0])),
      "the start's CONNECT presents no token the broker minted",
    )
    const firstEpoch = started.deliveries.at(-1)?.epoch

    // Closed for good: the hub drops the connection and greets its reconnect with no nonce, which
    // the broker token's authenticator refuses to sign, and nats.js closes a connection whose
    // authenticator throws.
    const closedAt = broker.connects.length
    const acceptedAt = broker.connections
    broker.nonce = undefined
    await broker.dropClients()
    // nats.js's own reconnect reaches the hub, which greets it with no nonce as it opens.
    await waitFor(
      async () => broker.connections > acceptedAt,
      "the reconnect the hub greeted with no nonce",
      state,
      PROCESS_WAIT_MS,
    )
    broker.nonce = "fake-nonce"
    await waitFor(
      async () =>
        started.relaySubscriptions() === 2 &&
        broker.liveConnections === 1 &&
        started.deliveries.some((asked) => asked.epoch !== firstEpoch),
      "the relay on a new connection, registered at a new epoch",
      state,
      PROCESS_WAIT_MS,
    )
    expect(stderr()).toContain(
      "envoy-channel: the NATS connection closed for good (NATS_AUTH is broker, but the NATS server sent no nonce to sign",
    )
    await started.checkConnects(
      tokens.minted.includes(tokenOf(broker.connects.slice(closedAt).at(-1))),
      "the new connection's CONNECT presents no token the broker minted",
    )

    // A `/clear` after the reopen: the session's token moves to the new id on the live
    // connection, which reconnects presenting it, and only then is the new id's relay subject
    // opened. The closed connection could do neither.
    const clearedAt = broker.connects.length
    const tradedAt = tokens.minted.length
    await writeSessionHandoff(sessionHandoffFile(started.stateDirectory, process.pid), next)
    await waitFor(
      async () =>
        started.sessions.includes(next) &&
        broker.subscribed.includes(deliverSubject(next)) &&
        started.requests.includes(`DELETE /v1/sessions/${SESSION}`) &&
        stderr().includes(`envoy: session id changed ${SESSION} -> ${next}; re-registered`),
      "the new id's relay subject and registration",
      state,
      PROCESS_WAIT_MS,
    )
    const traded = tokens.minted.slice(tradedAt)
    expect(traded.length).toBeGreaterThan(0)
    await started.checkConnects(
      broker.connects.slice(clearedAt).some((connect) => traded.includes(tokenOf(connect))),
      "no CONNECT after the /clear presents the token traded for it",
    )
  } finally {
    await started.stop()
    await cleanUp()
  }
}, ROW_TIMEOUT_MS)

test("a /clear while the NATS connection is closed for good waits for the new connection and completes on it", async () => {
  const run = await startServer()
  const { broker, gate, stderr, state } = run
  const next = "ses_reopen_next"
  try {
    await run.closeForGood()
    await writeSessionHandoff(sessionHandoffFile(run.stateDirectory, process.pid), next)
    // The handoff must not complete while no connection is live, and an absence needs the time
    // to pass: this window holds about seven heartbeats (200 ms) and six polls of the handoff file
    // (250 ms), each of which finds the /clear, and no fake clock reaches another process.
    await Bun.sleep(1_500)
    expect(run.sessions).not.toContain(next)
    expect(run.requests).not.toContain(`DELETE /v1/sessions/${SESSION}`)

    gate.open()
    await waitFor(
      async () =>
        run.sessions.includes(next) &&
        broker.subscribed.includes(deliverSubject(next)) &&
        run.requests.includes(`DELETE /v1/sessions/${SESSION}`) &&
        stderr().includes(`envoy: session id changed ${SESSION} -> ${next}; re-registered`),
      "the /clear completed on the new connection",
      state,
      PROCESS_WAIT_MS,
    )
    expect(broker.liveConnections).toBe(1)
    expect(run.deliveries[run.sessions.lastIndexOf(next)]).toMatchObject({ consumer: true })
  } finally {
    await run.stop()
  }
}, ROW_TIMEOUT_MS)
