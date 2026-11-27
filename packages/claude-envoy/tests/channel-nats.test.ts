// dialChannelNats: the channel server's NATS connection, opened with the session's credential and,
// where that is the broker token, kept renewed for the connection's life and stopped on request.
import { afterEach, expect, jest, spyOn, test } from "bun:test"
import { mkdir, writeFile } from "node:fs/promises"
import { connect, createServer, type Socket } from "node:net"
import { join } from "node:path"
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential"
import { NATS_DIAL_TOKEN_DROPPED } from "@legion/envoy-client/nats-connection"
import {
  type BrokerNatsCredential,
  brokerNatsCredential,
} from "@legion/envoy-client/nats-credential"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import {
  type Authenticator,
  type ConnectionOptions,
  ErrorCode,
  type NatsConnection,
  NatsError,
  nkeys,
} from "nats"
import { dialChannelNats } from "../src/channel-connection"
import { channelConnectionName } from "../src/installed-build"

/** Each row's home, removed after it, failed or not. */
const makeDirectory = temporaryDirectories("channel-nats")

/** The resolver's error for `host`, as Bun throws it through nats.js: its text names the host. */
const unresolved = (host: string): Error =>
  Object.assign(new Error(`getaddrinfo EAI_AGAIN ${host}`), {
    name: "DNSException",
    code: "EAI_AGAIN",
    syscall: "getaddrinfo",
  })

const servers: { stop: (closeActive?: boolean) => void }[] = []
afterEach(() => {
  forgetBrokerClients()
  for (const server of servers.splice(0)) server.stop(true)
})

/** A broker whose token exchange mints a random token that lives two seconds, kept in `minted`. */
function shortTokenBroker() {
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
        expires_in: 2,
      })
    },
  })
  servers.push(server)
  return { url: `http://127.0.0.1:${server.port}`, minted }
}

/** One turn of the real event loop, which the fake clock does not hold: every promise the code
 *  has chained settles inside it. */
function turn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>()
  setImmediate(resolve)
  return promise
}

/** A connection as the keeper sees it: each connect and reconnect asks the authenticator for the
 * CONNECT's credential, as nats.js does, and records the token it presented; `reconnected`
 * settles at the first reconnect. Its status reports nothing until the keeper stops reading it. */
function fakeDial() {
  const presented: string[] = []
  let reconnects = 0
  const reconnected = Promise.withResolvers<void>()
  const ended = Promise.withResolvers<void>()
  const unread = Promise.withResolvers<void>()
  const statuses = {
    stop: () => unread.resolve(),
    [Symbol.asyncIterator]: () => ({
      next: async (): Promise<IteratorResult<never>> => {
        await unread.promise
        return { done: true, value: undefined }
      },
    }),
  }
  const dial = async (options: ConnectionOptions): Promise<NatsConnection> => {
    const present = () => {
      const authenticator = options.authenticator as Authenticator | undefined
      const credential = authenticator?.("nonce") as { auth_token?: string } | undefined
      if (credential?.auth_token !== undefined) presented.push(credential.auth_token)
    }
    present()
    return {
      reconnect: async () => {
        reconnects += 1
        present()
        reconnected.resolve()
      },
      isClosed: () => false,
      closed: () => ended.promise,
      status: () => statuses,
    } as unknown as NatsConnection
  }
  return { dial, presented, reconnects: () => reconnects, reconnected: reconnected.promise }
}

test("dialChannelNats keeps the broker token renewed until it is stopped", async () => {
  const broker = shortTokenBroker()
  const home = makeDirectory()
  const tokenFile = join(home, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const credential = brokerNatsCredential(
    {
      HOME: home,
      AGENT_SECRETS_URL: broker.url,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
    "ses_channel",
  )
  const fake = fakeDial()
  const warnings: string[] = []
  const opened = await dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "broker", broker: credential },
    },
    "claude-envoy-channel-ses_channel",
    (text) => warnings.push(text),
    fake.dial,
  )
  // The token is taken before the dial, so the first CONNECT presents the first one minted.
  const [first] = broker.minted
  if (first === undefined) throw new Error("the broker minted no token before the dial")
  expect(fake.presented).toEqual([first])
  expect(opened.followSession).toBeDefined()
  // The keeper renews 1 s before the 2 s token lapses and reconnects with the next one. It is
  // stopped the moment it does, so no renewal is in flight at the stop.
  await fake.reconnected
  opened.stop()
  expect(fake.reconnects()).toBe(1)
  expect(fake.presented.at(-1)).not.toBe(first)
  expect(warnings).toEqual([])
  const minted = broker.minted.length
  // A real 2.5 s, two renewals at this token's life: the row reads that nothing renews after the
  // stop, and a renewal is a trade over a real socket, which a fake clock moved past it would not
  // let land before the row looks.
  await Bun.sleep(2_500)
  expect(fake.reconnects()).toBe(1)
  expect(broker.minted).toHaveLength(minted)
}, 15_000)

test("a pod session registered past the trade deadline still comes up, admitted", async () => {
  // #4153's rule: a trade made before the pod's registration is retried for as long as the session
  // runs. Claude Code does not restart its channel server, so a start that failed at the trade's
  // deadline left the session with no delivery for its life, naming the wrong cause.
  const SESSION = "ses_channel_pending"
  // The broker refuses the first trade, and the registration lands with that refusal: the trade
  // it grants is the retry loop's next, at least 1 s on (half its 2 s first wait), well past the
  // 300 ms deadline.
  const started = Date.now()
  let refusals = 0
  let grantedAfter: number | undefined
  const minted: string[] = []
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      if (refusals === 0) {
        refusals += 1
        return Response.json(
          {
            error: "invalid_grant",
            error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${SESSION}'. A pod writes under a session name its deployment registered for it.`,
          },
          { status: 400 },
        )
      }
      grantedAfter ??= Date.now() - started
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
  servers.push(server)
  const home = makeDirectory()
  const tokenFile = join(home, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const credential = brokerNatsCredential(
    {
      HOME: home,
      AGENT_SECRETS_URL: `http://127.0.0.1:${server.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
    SESSION,
    "NATS_AUTH",
    300,
  )
  const fake = fakeDial()
  const opened = await dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "broker", broker: credential },
    },
    `claude-envoy-channel-${SESSION}`,
    () => {},
    fake.dial,
  ).then(
    (dialled) => dialled,
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  if (typeof opened === "string") throw new Error(`the channel server's start failed: ${opened}`)
  // Admitted with the token traded once the registration landed.
  const [first] = minted
  if (first === undefined) throw new Error("the broker minted no token before the dial")
  expect(fake.presented).toEqual([first])
  expect(refusals).toBe(1)
  expect(grantedAfter).toBeGreaterThan(300)
  opened.stop()
}, 90_000)

/** A broker's address the row holds from its start to its end: a listener on a port the kernel
 *  chose, which drops every connection at once (a reset, before any answer) until `answer` names
 *  the broker to pass connections to, and from then on hands each one to that broker. The broker
 *  that is down and the broker that answers are at one address without that port ever being let
 *  go: a freed port is any process's to take, and a broker started on it later could find it taken
 *  and fail to start, or the row's trade reach whatever took it. */
async function heldBrokerAddress(): Promise<{ url: string; answer(port: number): void }> {
  let broker: number | undefined
  const sockets = new Set<Socket>()
  const held = (socket: Socket): Socket => {
    sockets.add(socket)
    socket.on("close", () => sockets.delete(socket))
    return socket
  }
  const front = createServer((client) => {
    held(client).on("error", () => {})
    if (broker === undefined) {
      client.resetAndDestroy()
      return
    }
    const upstream = held(connect(broker, "127.0.0.1"))
    upstream.on("error", () => client.destroy())
    client.on("error", () => upstream.destroy())
    client.pipe(upstream)
    upstream.pipe(client)
  })
  const listening = Promise.withResolvers<void>()
  front.once("error", listening.reject)
  front.listen(0, "127.0.0.1", listening.resolve)
  await listening.promise
  servers.push({
    stop: () => {
      for (const socket of sockets) socket.destroy()
      front.close()
    },
  })
  const address = front.address()
  if (address === null || typeof address === "string") throw new Error("the front has no TCP port")
  return {
    url: `http://127.0.0.1:${address.port}`,
    answer: (port) => {
      broker = port
    },
  }
}

test("a channel server's start waits through a broker that drops every connection, and comes up when it answers", async () => {
  // The channel server's start is the one caller that waits for ready (`ready(life)`): Claude
  // Code does not restart a channel server that ended, so a start that failed fast on a broker
  // that is down would leave the session with no delivery for its life. Every other caller fails
  // fast. The broker's address drops every connection at the first trade, and passes connections
  // to a broker that answers once the retry loop has failed once: once the token client says so,
  // at the wait after that trade.
  const SESSION = "ses_channel_broker_down"
  const address = await heldBrokerAddress()
  const minted: string[] = []
  const broker = Bun.serve({
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
  servers.push(broker)
  const home = makeDirectory()
  const tokenFile = join(home, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const credential = brokerNatsCredential(
    {
      HOME: home,
      AGENT_SECRETS_URL: address.url,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
    SESSION,
    "NATS_AUTH",
    300,
  )
  let refused = 0
  const warn = spyOn(console, "warn").mockImplementation((line: unknown) => {
    if (!String(line).includes("trading again")) return
    refused += 1
    if (refused === 1) address.answer(broker.port as number)
  })
  try {
    const fake = fakeDial()
    const opened = await dialChannelNats(
      {
        natsUrls: ["nats://nats.test:4222"],
        natsServersSetting: "ENVOY_NATS_URL",
        natsCredential: { kind: "broker", broker: credential },
      },
      `claude-envoy-channel-${SESSION}`,
      () => {},
      fake.dial,
    ).then(
      (dialled) => dialled,
      (error: unknown) => (error instanceof Error ? error.message : String(error)),
    )
    if (typeof opened === "string") throw new Error(`the channel server's start failed: ${opened}`)
    const [first] = minted
    if (first === undefined) throw new Error("the broker minted no token before the dial")
    expect(fake.presented).toEqual([first])
    expect(refused).toBe(1)
    opened.stop()
  } finally {
    warn.mockRestore()
  }
}, 90_000)

test("a first dial whose broker token the hub refuses twice drops it, presents the next one traded, and says so", async () => {
  // The dials share the keeper's count of the token's refusals (`connectNats`' `hooks.credential`),
  // so the dial drops a token the hub refused twice, as the keeper does on an open connection. The
  // waits between the dials are recorded, not slept: each waits only for a live token.
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
  servers.push(server)
  const home = makeDirectory()
  const tokenFile = join(home, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const credential = brokerNatsCredential(
    {
      HOME: home,
      AGENT_SECRETS_URL: `http://127.0.0.1:${server.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    },
    "ses_channel_refused_twice",
  )
  const fake = fakeDial()
  let refusals = 0
  const dial = async (options: ConnectionOptions): Promise<NatsConnection> => {
    if (refusals < 2) {
      refusals += 1
      throw new NatsError("'Authorization Violation'", ErrorCode.AuthorizationViolation)
    }
    return fake.dial(options)
  }
  const warnings: string[] = []
  const opened = await dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "broker", broker: credential },
    },
    "claude-envoy-channel-ses_channel_refused_twice",
    (text) => warnings.push(text),
    dial,
    undefined,
    undefined,
    { delay: () => credential.ready(), random: () => 0 },
  )
  opened.stop()
  // The start's token, and the one traded at the drop, which the third dial presented.
  expect(minted).toHaveLength(2)
  expect(fake.presented).toEqual(minted.slice(1))
  expect(warnings.filter((text) => text.includes(NATS_DIAL_TOKEN_DROPPED))).toHaveLength(1)
}, 15_000)

test("dialChannelNats runs no keeper and no follow for a connection with no broker token", async () => {
  // The clock is the row's: moved an hour on, every timer the start left behind has fired.
  jest.useFakeTimers()
  try {
    const fake = fakeDial()
    const opened = await dialChannelNats(
      {
        natsUrls: ["nats://nats.test:4222"],
        natsServersSetting: "ENVOY_NATS_URL",
        natsCredential: { kind: "none" },
      },
      "claude-envoy-channel-ses_plain",
      () => {},
      fake.dial,
    )
    expect(opened.followSession).toBeUndefined()
    expect(fake.presented).toEqual([])
    jest.advanceTimersByTime(60 * 60_000)
    await turn()
    expect(fake.reconnects()).toBe(0)
    opened.stop()
  } finally {
    // A fake clock left behind stops the timers of every row after this one.
    jest.useRealTimers()
  }
})

test("a failed dial names its servers by their setting, never by any part of them", async () => {
  const refused = async (): Promise<NatsConnection> => {
    throw NatsError.errorForCode(ErrorCode.ConnectionRefused)
  }
  // Each canary URL is built here from its parts, so no source line reads as a credential-bearing
  // URL to the push leak check.
  const userinfo = (...parts: string[]) => `${parts.join(":")}@`
  const servers = [
    `nats://${userinfo("E2_SENTINEL_TOKEN")}127.0.0.1:4222`,
    `tls://${userinfo("operator", "E2_SENTINEL_PASSWORD")}nats.example:4443`,
    // nats.js dials a host and port with no scheme, so the message names it as written.
    "127.0.0.1:4223",
    `${userinfo("E2_SENTINEL_BARE")}localhost:4224`,
    "E2_SENTINEL_MALFORMED nats address",
  ]
  const failure = await dialChannelNats(
    { natsUrls: servers, natsServersSetting: "ENVOY_NATS_URL", natsCredential: { kind: "none" } },
    "claude-envoy-channel-ses_redact",
    () => {},
    refused,
    undefined,
    DIAL_ONCE,
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(failure).toBe(
    "could not connect to NATS at the 5 servers ENVOY_NATS_URL names presenting no credential: CONNECTION_REFUSED",
  )
  expect(failure).not.toContain("E2_SENTINEL")
})

test("a credential pasted as a bare server address is never named", async () => {
  // A throwaway user nkey seed, made here: it parses as a host name, as a pasted token's segments
  // do, so a value that does not read as a host name is not named.
  const seed = new TextDecoder().decode(nkeys.createUser().getSeed())
  const segments = Array.from({ length: 3 }, () =>
    crypto.randomUUID().replaceAll("-", "").repeat(2),
  )
  // The resolver's error names the host it could not resolve, lowercased, as getaddrinfo does.
  const refused = async (): Promise<NatsConnection> => {
    throw unresolved(seed.toLowerCase())
  }
  const failure = await dialChannelNats(
    {
      natsUrls: [seed, `nats://${seed}`, segments.join("."), "nats.example:4222"],
      natsServersSetting: "envoy.natsUrls in /home/u/.claude/envoy.json",
      natsCredential: { kind: "none" },
    },
    "claude-envoy-channel-ses_seed",
    () => {},
    refused,
    undefined,
    DIAL_ONCE,
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(failure.toLowerCase()).not.toContain(seed.toLowerCase())
  for (const segment of segments) expect(failure.toLowerCase()).not.toContain(segment.toLowerCase())
  expect(failure).toBe(
    "could not connect to NATS at the 4 servers envoy.natsUrls in /home/u/.claude/envoy.json names presenting no credential: getaddrinfo EAI_AGAIN",
  )
})

test("a token holding an unencoded / is named by none of its segments", async () => {
  // The URL's authority ends at the `/`, so its host is the token's first segment, which nats.js
  // resolves and names in its error.
  const userinfo = (...parts: string[]) => `${parts.join(":")}@`
  const refused = async (): Promise<NatsConnection> => {
    throw unresolved("e2sentineltok")
  }
  const failure = await dialChannelNats(
    {
      natsUrls: [`nats://${userinfo("e2sentineltok/def+gh==")}127.0.0.1:4222`],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "none" },
    },
    "claude-envoy-channel-ses_slash",
    () => {},
    refused,
    undefined,
    DIAL_ONCE,
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(failure).toBe(
    "could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential: getaddrinfo EAI_AGAIN",
  )
})

test("a stop ends the wait for the token and for the dial, and closes a connection made late", async () => {
  const STOPPING = "the channel server stopped before its NATS subscriptions reached the server"
  // The dial: pending until after the stop, then a connection the stop must close.
  const made = Promise.withResolvers<NatsConnection>()
  let closed = 0
  const late = {
    close: async () => {
      closed += 1
    },
  } as unknown as NatsConnection
  const stopping = new AbortController()
  const dialing = dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "none" },
    },
    "claude-envoy-channel-ses_stop",
    () => {},
    // The stop lands once the dial has begun.
    () => {
      queueMicrotask(() => stopping.abort())
      return made.promise
    },
    stopping.signal,
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(await dialing).toBe(STOPPING)
  made.resolve(late)
  await turn()
  expect(closed).toBe(1)

  // The token: a trade that never answers is waited for no longer than the stop.
  const waiting = new AbortController()
  // A broker credential whose trade never answers; nothing else of it is reached before the dial.
  // The stop lands once the wait for the token has begun.
  const broker = {
    ready: () => {
      queueMicrotask(() => waiting.abort())
      return new Promise<void>(() => undefined)
    },
  } as unknown as BrokerNatsCredential
  const trading = dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "broker", broker },
    },
    "claude-envoy-channel-ses_trade",
    () => {},
    async () => {
      throw new Error("dialled before the token")
    },
    waiting.signal,
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(await trading).toBe(STOPPING)
})

/** One dial, for a row that reads the line a dial that fails for good throws. */
const DIAL_ONCE = { firstDelayMs: 0, maxDelayMs: 0, budgetMs: 0 }
/** The channel server's own waits, scaled down, still for its life. */
const QUICK_FOR_LIFE = { firstDelayMs: 5, maxDelayMs: 5, budgetMs: Number.POSITIVE_INFINITY }

test("dialChannelNats dials again: a dial refused twice then answered comes up, naming each redial", async () => {
  const fake = fakeDial()
  const timeouts: (number | undefined)[] = []
  let dials = 0
  const dial = async (options: ConnectionOptions): Promise<NatsConnection> => {
    timeouts.push(options.timeout)
    dials += 1
    if (dials <= 2) throw new NatsError("'Authentication Timeout'", ErrorCode.AuthenticationTimeout)
    return fake.dial(options)
  }
  const warnings: string[] = []
  const opened = await dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "none" },
    },
    "claude-envoy-channel-ses_redial",
    (text) => warnings.push(text),
    dial,
    undefined,
    QUICK_FOR_LIFE,
  )
  opened.stop()
  expect(dials).toBe(3)
  expect(timeouts).toEqual([10_000, 10_000, 10_000])
  expect(warnings).toEqual([
    "could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential: Authentication Timeout; dialing again in 5 ms",
    "could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential: Authentication Timeout; dialing again in 5 ms",
  ])
})

test("dialChannelNats dials again until it stops: a stop during a wait ends the start, and no dial follows", async () => {
  const STOPPING = "the channel server stopped before its NATS subscriptions reached the server"
  const stopping = new AbortController()
  let dials = 0
  const dial = async (): Promise<NatsConnection> => {
    dials += 1
    throw NatsError.errorForCode(ErrorCode.ConnectionRefused)
  }
  const failure = dialChannelNats(
    {
      natsUrls: ["nats://nats.test:4222"],
      natsServersSetting: "ENVOY_NATS_URL",
      natsCredential: { kind: "none" },
    },
    "claude-envoy-channel-ses_stop",
    // The first redial's notice is the moment the dial is waiting: stop it there.
    () => stopping.abort(),
    dial,
    stopping.signal,
    { firstDelayMs: 200, maxDelayMs: 200, budgetMs: Number.POSITIVE_INFINITY },
  ).then(
    () => "dialled",
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  expect(await failure).toBe(STOPPING)
  // The wait the stop cut short has run out by now, and it started nothing.
  const after = Promise.withResolvers<void>()
  setTimeout(after.resolve, 400)
  await after.promise
  expect(dials).toBe(1)
})

// legacy-subjects-gate.sh reads each connection's build from the end of the name the hub's census of
// connections lists: the build-commit stamp the worker image writes beside the bundle, else the
// release Claude Code or Codex installed, else none.
test("the connection name the server sees ends with the channel's build", async () => {
  const commit = "0123456789abcdef0123456789abcdef01234567"
  const image = join(makeDirectory(), "dist")
  await mkdir(image, { recursive: true })
  await writeFile(join(image, "build-commit"), `${commit}\n`)
  const rows = [
    {
      name: channelConnectionName(
        "claude-envoy-channel-ses_build",
        join(image, "envoy-channel.js"),
        "@sjawhar/claude-envoy",
      ),
      want: `claude-envoy-channel-ses_build legion@${commit}`,
    },
    {
      name: channelConnectionName(
        "codex-envoy-channel-ses_build",
        "/home/me/.codex/plugins/cache/legion-plugins/codex-envoy/6.0.0/dist/envoy-channel.js",
        "@sjawhar/codex-envoy",
      ),
      want: "codex-envoy-channel-ses_build @sjawhar/codex-envoy@6.0.0",
    },
    {
      name: channelConnectionName(
        "claude-envoy-channel-ses_build",
        "/work/legion/packages/claude-envoy/dist/envoy-channel.js",
        "@sjawhar/claude-envoy",
      ),
      want: "claude-envoy-channel-ses_build",
    },
  ]
  for (const row of rows) {
    const seen: string[] = []
    await dialChannelNats(
      {
        natsUrls: ["nats://nats.test:4222"],
        natsServersSetting: "ENVOY_NATS_URL",
        natsCredential: { kind: "none" },
      },
      row.name,
      () => {},
      async (options: ConnectionOptions): Promise<NatsConnection> => {
        seen.push(options.name ?? "")
        throw NatsError.errorForCode(ErrorCode.ConnectionRefused)
      },
      undefined,
      DIAL_ONCE,
    ).catch(() => undefined)
    expect(seen).toEqual([row.want])
  }
})
