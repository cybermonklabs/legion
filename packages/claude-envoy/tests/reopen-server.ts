// A whole channel-server process for the rows whose NATS connection closes for good, while the
// session runs or as it starts (`envoy-channel-reopen.test.ts`, `codex-reopen.test.ts`,
// `envoy-channel-start-reopen.test.ts`): its NATS servers are a gate in front of a fake hub, and
// its listener a stand-in that gives every registration the session's consumer. Real nats.js and
// real time: the server's own dial waits are what those rows watch.
import { chmod, mkdtemp, rm, writeFile } from "node:fs/promises"
import { join, resolve } from "node:path"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import type { DeliveryRequest } from "@legion/envoy-client/transport"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { modSocketPath } from "../hooks/mod-protocol"
import {
  codexSessionRecordFile,
  processStarted,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { waitFor } from "./channel-session-harness"
import { type FakeMod, startFakeMod } from "./fake-mod"
import { FakeNatsServer } from "./fake-nats-server"
import { type NatsGate, natsGate } from "./nats-gate"
import { collected } from "./row-deadline"
import { socketOwner } from "./socket-owner"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

/** The last-resort bound for a legacy `waitFor` on a whole server process. Every such wait ends
 *  the moment its fact holds; the bound only names the step of a row that is stuck. A row that took
 *  25.8 s alone took longer than an earlier 15 s bound beside other suites at a load of thousands. */
export const PROCESS_WAIT_MS = 120_000
/** Each row's: above its chain of waits, so a stuck wait names its step before the runner does. */
export const ROW_TIMEOUT_MS = 600_000

/** The fake mod of `run`'s session `sessionId`, once the server's mod socket answers its hello.
 *  The probe waits until the row's deadline (`@legion/test-fixtures/wait`), and ends at once,
 *  naming the exit and the server's stderr, once the server has exited. */
export async function startSessionMod(
  run: Pick<ReopenServer, "stateDirectory" | "server" | "stderr">,
  sessionId: string,
): Promise<FakeMod> {
  const socketPath = modSocketPath(sessionId, { XDG_RUNTIME_DIR: run.stateDirectory })
  if (socketPath === undefined) throw new Error("the session names no mod socket")
  return await startFakeMod(socketPath, {
    sessionId,
    exited: run.server.exited,
    wrote: run.stderr,
  })
}

/** How many times `text` holds `part`. */
export function count(text: string, part: string): number {
  return text.split(part).length - 1
}

/** The file in a Codex server's state directory that every `codex queue` call wrote to. */
export const CODEX_QUEUED = "queued.log"

/**
 * A `prepare` for a Codex server (`--host codex`) of thread `sessionId`: the record the plugin's
 * `SessionStart` hook writes for this test's process, which stands in for the `codex` process, and
 * the stand-in `codex` binary it names, which appends each message `codex queue` is handed to
 * `CODEX_QUEUED`: how every line reaches an idle Codex session (`../src/codex/codex-delivery`).
 */
export function codexSession(sessionId: string): (stateDirectory: string) => Promise<void> {
  return async (stateDirectory) => {
    const started = processStarted(process.pid)
    if (started === undefined) throw new Error("this process's start cannot be read")
    const codex = join(stateDirectory, "codex")
    await writeFile(
      codex,
      `#!/bin/sh\nfor arg in "$@"; do printf '%s\\n' "$arg"; done >> '${join(stateDirectory, CODEX_QUEUED)}'\n`,
    )
    await chmod(codex, 0o755)
    await writeCodexSessionRecord(codexSessionRecordFile(stateDirectory, process.pid), {
      codex_executable: codex,
      codex_home: join(stateDirectory, "codex-home"),
      codex_started: started,
      cwd: stateDirectory,
      session_id: sessionId,
      source: "startup",
      transcript_path: null,
    })
  }
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
  /** The loopback address the hub and its gate listen on (fake-nats-server.ts's fakeHost), else
   *  one of the hub's own that the fake picks. */
  readonly brokerHost?: string
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
  /** The shared `until` for a row on the command's guard. Older callers retain their per-step
   *  `waitFor` bounds. */
  readonly waitForStart?: (what: string, probe: () => Promise<boolean>) => Promise<unknown>
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
   *  client dialed the gate from, and the process that owns that connection to the gate. */
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
  const broker = new FakeNatsServer({
    ...(options.nonce === undefined ? {} : { nonce: options.nonce }),
    ...(options.brokerHost === undefined ? {} : { host: options.brokerHost }),
  })
  // Each carried client's owner: the owner of the connection from its port to the gate's address
  // and port, not of any socket on its port, since Linux gives one ephemeral port to connects to
  // other destinations too. The gate listens on the hub's own loopback address (fakeHost), not
  // 127.0.0.1, so the lookup is given that address. The lookup starts as the gate carries the
  // connection, which can close before a scan of every process reaches its holder: so it reads the
  // server's fds first, in milliseconds, and scans only for another holder.
  const owners = new Map<number, Promise<string>>()
  let serverPid: number | undefined
  const gate = natsGate(broker.url, (leg) => {
    const expected = serverPid === undefined ? {} : { expected: serverPid }
    owners.set(leg.clientPort, socketOwner(leg.clientPort, gateEnd, expected))
  })
  const gateUrl = new URL(gate.url)
  const gateEnd = { host: gateUrl.hostname, port: Number(gateUrl.port) }
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
      ...NO_TRANSPILER_CACHE,
      ...options.env(stateDirectory),
    },
    stdin: "pipe",
    stdout: "ignore",
    stderr: "pipe",
  })
  serverPid = server.pid
  const stderr = collected(server.stderr)
  const stderrEnded = stderr.ended.then(() => undefined)
  const relaySubscriptions = (): number =>
    broker.subscribed.filter((subject) => subject === relay).length
  const state = (): string =>
    `exit code ${server.exitCode}; signal ${server.signalCode}; relay subscriptions ${relaySubscriptions()}; live connections ${broker.liveConnections}; stderr:\n${stderr.text()}`
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
  const waitForStart =
    options.waitForStart ?? ((what, probe) => waitFor(probe, what, state, PROCESS_WAIT_MS))
  try {
    // A server that ends first is named with its exit and stderr. A server writing Bun's
    // transpiler cache into its empty HOME (`./transpiler-cache.ts`) had written nothing in 30 s
    // at a load near 1,000.
    await waitForStart("the server's first stderr line", async () => {
      if (stderr.text().length > 0 || deliveries.length > 0) return true
      if (server.exitCode === null && server.signalCode === null) return false
      await stderrEnded
      if (stderr.text().length > 0) return true
      throw new Error(`the server ended before its first stderr line: ${state()}`)
    })
    if (options.awaitRegistration !== false) {
      // A server that printed its first line and then ended is named with its end at once too.
      await waitForStart(
        "the session's first registration and its relay subscription",
        async () => {
          if (deliveries.length > 0 && relaySubscriptions() === 1) return true
          if (server.exitCode === null && server.signalCode === null) return false
          await stderrEnded
          throw new Error(`the server ended before its first registration: ${state()}`)
        },
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
    stderr: stderr.text,
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
