/**
 * The live view's replay answer on a Claude Code session (`startAgentStream`,
 * `../src/agent-stream-runner`) through a real nats-server, on a connection authenticated as a
 * session user holding the grant the NATS auth service gives a session (`sessionGrantConfig`,
 * `natsgrant.Session` itself): publish its replies under `_INBOX.*.<session id>.>` alone. The
 * scenario is the one pi-envoy's row runs too (`runReplayGrantScenario`): a replay asked on a
 * reply subject naming the session is answered; one asked on nats.go's own request inbox, the
 * subject Dispatch used before, is refused by the server and reaches nobody; the session then
 * answers on its own prefix again. This row keeps the plugin's own boot, connection and shutdown.
 *
 * The mod is a silent stand-in: the replay answer is taken from the stream's own ring, which a
 * session no mod has spoken to holds empty, and the subject it goes to is what this row is about.
 *
 * Docker-backed: the claude-envoy job runs this suite with LEGION_E2E=1 after pulling the image.
 */
import { afterAll, beforeAll, describe, test } from "bun:test"
import { natsConnectionOptions } from "@legion/envoy-client/nats-connection"
import {
  type NatsServer,
  sessionGrantConfig,
  startNatsServerWithConfig,
  useNatsServerDirectory,
} from "@legion/envoy-client/test/nats-server"
import { runReplayGrantScenario } from "@legion/envoy-client/test/replay-grant-scenario"
import { connect, type NatsConnection, nkeys } from "nats"
import type { ModEvent } from "../hooks/mod-protocol"
import { startAgentStream } from "../src/agent-stream-runner"
import type { ModLink } from "../src/mod-link"

/** nats.js types its `nkeys` export `any`. */
const testKeys: { createUser(): { getSeed(): Uint8Array; getPublicKey(): string } } = nkeys

const serverDirectory = useNatsServerDirectory()

const SESSION = "0c3b6f6e-0000-4000-8000-0000000000aa"

// Destructured: this package's typecheck forbids `process.env.X`.
const { LEGION_E2E } = process.env

/** A mod link no mod ever speaks on: no event, no session id move, every call refused. */
function silentLink(): { readonly link: ModLink; readonly close: () => void } {
  const ending = Promise.withResolvers<void>()
  // No event ever comes: the iterator's one answer is its end, once the link closes.
  const events: AsyncIterableIterator<ModEvent> = {
    next: async () => {
      await ending.promise
      return { done: true, value: undefined }
    },
    [Symbol.asyncIterator]() {
      return this
    },
  }
  const link: ModLink = {
    close: async () => ending.resolve(),
    connected: () => false,
    events,
    onConnected: () => undefined,
    onSessionId: () => undefined,
    request: () => Promise.reject(new Error("no mod speaks on this link")),
    sessionId: () => SESSION,
    socketPath: "/scratch/claude-envoy-reply-grant.sock",
  }
  return { close: () => ending.resolve(), link }
}

describe.skipIf(LEGION_E2E !== "1")("the replay answer under a session's own NATS grant", () => {
  const sessionUser = testKeys.createUser()
  const requesterUser = testKeys.createUser()
  let server: NatsServer | undefined
  const open: NatsConnection[] = []

  beforeAll(async () => {
    server = await startNatsServerWithConfig(
      serverDirectory,
      sessionGrantConfig({
        requesterKey: requesterUser.getPublicKey(),
        sessionID: SESSION,
        sessionKey: sessionUser.getPublicKey(),
      }),
    )
  }, 120_000)
  afterAll(async () => {
    for (const connection of open) await connection.close()
    server?.stop()
  }, 60_000)

  test("answers a replay asked on its own reply prefix, and nothing on nats.go's request inbox", async () => {
    if (server === undefined) throw new Error("the NATS server did not start")
    const decoder = new TextDecoder()
    const session = await connect({
      ...natsConnectionOptions({
        name: "claude-envoy-reply-grant",
        credential: {
          kind: "seed",
          seed: { source: "the row's session user", value: decoder.decode(sessionUser.getSeed()) },
        },
        servers: [server.url],
      }),
      reconnect: false,
    })
    open.push(session)
    const requester = await connect({
      ...natsConnectionOptions({
        name: "claude-envoy-reply-grant-requester",
        credential: {
          kind: "seed",
          seed: {
            source: "the row's requester user",
            value: decoder.decode(requesterUser.getSeed()),
          },
        },
        servers: [server.url],
      }),
      reconnect: false,
    })
    open.push(requester)
    const mod = silentLink()
    const runner = startAgentStream({
      connection: session,
      followsSessionId: false,
      link: mod.link,
      sessionId: SESSION,
      warn: () => undefined,
    })
    try {
      await runReplayGrantScenario({ requester, server, sessionID: SESSION })
    } finally {
      runner.stop()
      mod.close()
    }
  }, 60_000)
})
