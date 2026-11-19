// A whole channel-server process for the rows whose NATS connection closes for good while the
// session runs (`envoy-channel-reopen.test.ts`, the Claude Code host; `codex-reopen.test.ts`, the
// Codex host): its NATS servers are a gate in front of a fake hub, and its listener a stand-in that
// gives every registration the session's consumer. Real nats.js and real time: the server's own
// dial waits are what those rows watch.
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import type { DeliveryRequest } from "@legion/envoy-client/transport"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { SERVER_START_MS, waitFor } from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"
import { type NatsGate, natsGate } from "./nats-gate"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

/** Each wait's deadline: every step here is a whole server process, which a loaded machine
 *  starts and runs slowly. */
export const PROCESS_WAIT_MS = 15_000
/** Each row's: above its chain of waits, so a stuck wait names its step. */
export const ROW_TIMEOUT_MS = 180_000

/** How many times `text` holds `part`. */
export function count(text: string, part: string): number {
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

/** A secrets broker stand-in for a session that presents its broker token (`sessionBroker`). */
export interface SessionBroker {
  /** Every token the broker minted, in order. */
  readonly minted: readonly string[]
  /** The variables that make a server present the session's broker token, traded here. */
  readonly env: Readonly<Record<string, string>>
  stop(): Promise<void>
}

/**
 * A secrets broker whose token exchange mints a token that lives fifteen minutes, each kept in
 * `minted`: `channel-nats.test.ts`'s stand-in, with a life no keeper renews inside a row, and the
 * machine token file a pod's server trades with.
 */
export async function sessionBroker(): Promise<SessionBroker> {
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
  const machine = await mkdtemp("/tmp/ce-reopen-machine-")
  const tokenFile = join(machine, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  return {
    minted,
    env: {
      NATS_AUTH: "broker",
      AGENT_SECRETS_URL: `http://127.0.0.1:${server.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
    async stop() {
      server.stop(true)
      await rm(machine, { recursive: true, force: true })
    },
  }
}

export interface ReopenServerOptions {
  /** The session the server registers, whose relay subject the rows watch. */
  readonly sessionId: string
  /** The server's arguments: none for Claude Code's, `--host codex` for Codex's. */
  readonly args?: readonly string[]
  /** The hub's nonce, which a seed or a broker token signs. */
  readonly nonce?: string
  /** The host's variables and the credential's, given the server's state directory. */
  readonly env: (stateDirectory: string) => Readonly<Record<string, string>>
  /** Writes what the server reads before it registers into its state directory (a Codex
   *  session's record), before it starts. */
  readonly prepare?: (stateDirectory: string) => Promise<void>
  /** Told each listener call as it arrives, with the hub; the call is answered once what it
   *  returns settles, so a row can hold one. */
  readonly onRequest?: (
    method: string,
    path: string,
    broker: FakeNatsServer,
  ) => void | Promise<void>
  /** Whether the start resolves only once the session has registered and subscribed its relay
   *  (the default), or once the server has written its first stderr line: a row that stops the
   *  server while it starts never sees the registration. */
  readonly awaitRegistration?: boolean
  /** Whether the gate in front of the hub starts open (the default), or shut: the server's first
   *  dial is then refused, and dialed again, until the row opens it. */
  readonly gateOpen?: boolean
}

export interface ReopenServer {
  readonly broker: FakeNatsServer
  readonly gate: NatsGate
  readonly server: Bun.Subprocess<"pipe", "ignore", "pipe">
  readonly stateDirectory: string
  /** Every listener call, as `METHOD path`. */
  readonly requests: readonly string[]
  /** Each registration's session id and delivery request, in arrival order. */
  readonly sessions: readonly string[]
  readonly deliveries: readonly DeliveryRequest[]
  stderr(): string
  /** Settles once the server's stderr has ended: what a row reads after the process exits. */
  readonly stderrEnded: Promise<void>
  /** How many times the session's relay subject has been subscribed, over every connection. */
  relaySubscriptions(): number
  /** What a wait that timed out reports: the process, its relay, its connections, its stderr. */
  state(): string
  /** Each CONNECT the hub saw: its connection name, whether it presented a token, the port its
   *  client dialed the gate from, and the process that owns that port. */
  describeConnects(): Promise<string>
  /** Fails naming every CONNECT the hub saw and the process each came from, unless `held`: a
   *  CONNECT check that fails says whose CONNECT it read. */
  checkConnects(held: boolean, what: string): Promise<void>
  /** Closed for good: the hub refuses the session twice and nats.js gives up, and every redial is
   *  refused until the gate opens again. */
  closeForGood(): Promise<void>
  stop(): Promise<void>
}

/** Starts the server, and resolves once its session has registered and subscribed its relay, or
 *  once it has written its first stderr line (`awaitRegistration`). */
export async function startReopenServer(options: ReopenServerOptions): Promise<ReopenServer> {
  const relay = deliverSubject(options.sessionId)
  const broker = new FakeNatsServer(options.nonce === undefined ? {} : { nonce: options.nonce })
  // Each client's owner is looked up as the gate accepts it, while its socket is still up.
  const owners = new Map<number, Promise<string>>()
  const gate = natsGate(Number(new URL(broker.url).port), (leg) => {
    owners.set(leg.clientPort, socketOwner(leg.clientPort))
  })
  if (options.gateOpen !== false) gate.open()
  const requests: string[] = []
  const sessions: string[] = []
  const deliveries: DeliveryRequest[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      requests.push(`${request.method} ${url.pathname}`)
      await options.onRequest?.(request.method, url.pathname, broker)
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
  const cleanUp = async (): Promise<void> => {
    listener.stop(true)
    gate.stop()
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
  try {
    await options.prepare?.(stateDirectory)
  } catch (error) {
    await cleanUp()
    throw error
  }
  const server = Bun.spawn(["bun", entry, ...(options.args ?? [])], {
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: gate.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      // Several heartbeats register while the connection is down, so "told once" is told once
      // over several refusals.
      ENVOY_HEARTBEAT_MS: "200",
      XDG_RUNTIME_DIR: stateDirectory,
      ...options.env(stateDirectory),
    },
    stdin: "pipe",
    stdout: "ignore",
    stderr: "pipe",
  })
  let stderr = ""
  const decoder = new TextDecoder()
  const stderrEnded = (async () => {
    for await (const chunk of server.stderr) stderr += decoder.decode(chunk, { stream: true })
  })()
  const relaySubscriptions = (): number =>
    broker.subscribed.filter((subject) => subject === relay).length
  const state = (): string =>
    `exit code ${server.exitCode}; relay subscriptions ${relaySubscriptions()}; live connections ${broker.liveConnections}; stderr:\n${stderr}`
  const stop = async (): Promise<void> => {
    server.kill("SIGKILL")
    await server.exited
    await cleanUp()
  }
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
    if (options.awaitRegistration !== false) {
      await waitFor(
        async () => deliveries.length > 0 && relaySubscriptions() === 1,
        "the session's first registration and its relay subscription",
        state,
        PROCESS_WAIT_MS,
      )
    }
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
    async checkConnects(held, what) {
      if (!held) throw new Error(`${what}: ${await describeConnects()}`)
    },
    async closeForGood() {
      gate.shut()
      await broker.closeClients()
    },
    stop,
  }
}
