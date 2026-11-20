// dialChannelNats: the channel server's NATS connection, opened with the session's credential and,
// where that is the broker token, kept renewed for the connection's life and stopped on request.
import { afterEach, expect, jest, spyOn, test } from "bun:test"
import { writeFile } from "node:fs/promises"
import { join } from "node:path"
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential"
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
import { dialChannelNats } from "../src/envoy-channel-server"

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

test("a channel server's start waits through a broker that refuses connections, and comes up when it answers", async () => {
  // The channel server's start is the one caller that waits for ready (`ready(life)`): Claude
  // Code does not restart a channel server that ended, so a start that failed fast on a broker
  // that is down would leave the session with no delivery for its life. Every other caller fails
  // fast. The broker's port is closed at the first trade, and a broker listens there once the
  // retry loop has failed once: once the token client says so, at the wait after that trade.
  const SESSION = "ses_channel_broker_down"
  const probe = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response() })
  const port = probe.port as number
  probe.stop(true)
  const minted: string[] = []
  const home = makeDirectory()
  const tokenFile = join(home, "token")
  await writeFile(tokenFile, `${fixtureSecret("machine")}\n`)
  const credential = brokerNatsCredential(
    {
      HOME: home,
      AGENT_SECRETS_URL: `http://127.0.0.1:${port}`,
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
    if (refused > 1) return
    servers.push(
      Bun.serve({
        port,
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
      }),
    )
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
