// Ways a running session loses its relay subject without its connection being replaced at once,
// each driven on `FakeNats`, which refuses a subscribe, a publish and a flush on a connection
// closed for good and ends a refused subscription with its error, as nats.js does:
// - the relay subscription ends with an error of its own while the connection stays up (a
//   permissions refusal): nothing took it again, and every registration asked the listener to
//   relay to a subscription that no longer existed, with no outage told;
// - the subscription taken again in its place is not yet the server's when the flush behind it
//   is lost to a redial or times out, and the server can still refuse it after the reconnect;
// - the connection closes for good between a `/clear`'s deregistration of the old id and its
//   registration of the new one: the credential move must wait for the connection that replaces
//   it, and the outage is told, naming the close.
import { expect, test } from "bun:test"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import type { SubscribeInput } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  connectionClosedError,
  FakeNats,
  FakeNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

/** nats.js's error for a server's refusal, as it reaches a subscription or the closed connection. */
function serverRefusal(code: string, text: string): Error {
  return Object.assign(new Error(text), { name: "NatsError", code })
}

/** The notices the server wrote for itself under `heading`: the one write that names no event. */
function notices(notifier: FakeNotifier, heading: string): string[] {
  return notifier.notifications
    .filter((written) => written.eventId === undefined && written.content.includes(heading))
    .map((written) => written.content)
}

test("a relay subscription the server ends with an error is told as an outage naming why, and the next heartbeat takes it again at a new epoch", async () => {
  const SESSION = "ses_relay_lost"
  const RELAY = deliverSubject(SESSION)
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const relaySubscribes = (): number =>
    nats.calls.filter((call) => call === `nats.subscribe ${RELAY}`).length
  /** Each registration the listener saw, with how many times the relay had been subscribed. */
  const registrations: { readonly input: SubscribeInput; readonly relaySubscribes: number }[] = []
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
      connection: nats,
      notifier,
      heartbeatMs: 25,
      client: recordingClient([], {
        subscribe: async (input) => {
          registrations.push({ input, relaySubscribes: relaySubscribes() })
          return noInterest(input.sessionID)
        },
      }),
    }),
  )
  try {
    const firstEpoch = registrations.at(-1)?.input.delivery?.epoch
    const failedAt = registrations.length
    nats.failSubscription(
      RELAY,
      serverRefusal(
        "PERMISSIONS_VIOLATION",
        `Permissions Violation for Subscription to "${RELAY}"`,
      ),
    )
    await waitFor(
      async () => notices(notifier, "Envoy inbound restored").length === 1,
      "the outage told and then its end",
      () => JSON.stringify(notifier.notifications),
    )
    const told = notices(notifier, "Envoy inbound unavailable")
    expect(told).toHaveLength(1)
    expect(told[0]).toContain(
      "because its NATS connection failed: the server refused a permission (its text is withheld)",
    )
    // Taken again, once, on the same connection, and registered at a new epoch.
    expect(relaySubscribes()).toBe(2)
    expect(nats.liveSubscriptions(RELAY)).toBe(1)
    const after = registrations.slice(failedAt)
    expect(after.length).toBeGreaterThan(0)
    // No registration asked the listener to relay to the subscription that had ended.
    for (const registration of after) expect(registration.relaySubscribes).toBe(2)
    expect(after[0]?.input.delivery?.epoch).not.toBe(firstEpoch)
    // A frame on the subject taken again reaches the session.
    nats.emitRelay(SESSION, {
      subject: `notifications.agent.${SESSION}`,
      seq: 4,
      raw: JSON.stringify({
        event_id: "evt-after-relay-loss",
        source: "agent",
        source_session: "ses_peer",
        topic: `notifications.agent.${SESSION}`,
        dedupe_key: "delivery-after-relay-loss",
        issued_at: 1_760_000_000_000,
        payload_summary: "peer message after the relay came back",
      }),
      reply: "_INBOX.after-relay-loss",
    })
    await waitFor(
      async () => nats.repliesOn("_INBOX.after-relay-loss").includes("done"),
      "the frame on the relay taken again",
    )
    expect(
      notifier.notifications.filter((written) => written.eventId === "evt-after-relay-loss"),
    ).toHaveLength(1)
  } finally {
    await session.shutdown()
  }
}, 30_000)

// The relay subject is taken again only once the server has answered a flush behind the
// subscribe: a flush lost to a redial (rejected) or one that never comes back (timed out) says
// nothing of whether the server took the subscription, and a refusal the server sends after the
// reconnect would come after the registration had asked the listener to relay to it and told the
// session it receives again. While the server refuses the subject, no registration asks for it,
// the outage is told once, and nothing tells the session it receives again.
for (const lost of [
  { name: "rejected by a redial", flushTimeoutMs: undefined },
  { name: "timed out", flushTimeoutMs: 50 },
] as const) {
  test(`a relay subscription taken again behind a flush ${lost.name} is not confirmed: nothing asks for it until a flush confirms it`, async () => {
    const SESSION = "ses_relay_unconfirmed"
    const RELAY = deliverSubject(SESSION)
    const nats = new FakeNats()
    const notifier = new FakeNotifier()
    const relaySubscribes = (): number =>
      nats.calls.filter((call) => call === `nats.subscribe ${RELAY}`).length
    const registrations: SubscribeInput[] = []
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
        connection: nats,
        notifier,
        heartbeatMs: 25,
        ...(lost.flushTimeoutMs === undefined
          ? {}
          : { handoffFlushTimeoutMs: lost.flushTimeoutMs }),
        client: recordingClient([], {
          subscribe: async (input) => {
            registrations.push(input)
            return noInterest(input.sessionID)
          },
        }),
      }),
    )
    const refusal = serverRefusal(
      "PERMISSIONS_VIOLATION",
      `Permissions Violation for Subscription to "${RELAY}"`,
    )
    /** Registrations before the outage was told: every one after it asked for the relay. */
    let sentAt = registrations.length
    const counts = () => ({
      restored: notices(notifier, "Envoy inbound restored").length,
      sentWhileRefused: registrations.length - sentAt,
      unavailable: notices(notifier, "Envoy inbound unavailable").length,
    })
    try {
      const firstEpoch = registrations.at(-1)?.delivery?.epoch
      // The server refuses the subject from now on, and the connection redials: no flush comes
      // back until the test says so.
      nats.refuse(RELAY, refusal)
      if (lost.flushTimeoutMs === undefined) nats.failFlushes = Number.POSITIVE_INFINITY
      else nats.stallFlush = true
      nats.failSubscription(RELAY, refusal)
      await waitFor(
        async () => notices(notifier, "Envoy inbound unavailable").length === 1,
        "the outage told",
      )
      sentAt = registrations.length
      // Heartbeats take the subject again behind flushes that never come back.
      const lostAt = relaySubscribes()
      await waitFor(
        async () => relaySubscribes() >= lostAt + 3 || counts().sentWhileRefused > 0,
        "three attempts behind flushes that never came back",
        () => JSON.stringify(counts()),
      )
      // The connection is back: its flushes are answered, and the server's refusal of the
      // subscription it held reaches it; heartbeats take the subject again, and are refused.
      nats.failFlushes = 0
      nats.stallFlush = false
      nats.deliverRefusals()
      const backAt = relaySubscribes()
      await waitFor(
        async () => relaySubscribes() >= backAt + 3,
        "three attempts the server refused",
        () => JSON.stringify(counts()),
      )
      expect(counts()).toEqual({ restored: 0, sentWhileRefused: 0, unavailable: 1 })
      // Every subscription an unconfirmed flush left was given up: none is live while the server
      // refuses the subject. nats.js keeps a subscription the session does not unsubscribe, and
      // the relay's next one would be one more beside it.
      expect(nats.liveSubscriptions(RELAY)).toBe(0)

      // The server takes the subject again: the next heartbeat's flush confirms it, a new epoch
      // is registered, and the end of the outage is told once.
      nats.refuse(RELAY, undefined)
      await waitFor(
        async () => notices(notifier, "Envoy inbound restored").length === 1,
        "the end of the outage told",
        () => JSON.stringify(counts()),
      )
      expect(nats.liveSubscriptions(RELAY)).toBe(1)
      expect(registrations.at(-1)?.delivery?.epoch).not.toBe(firstEpoch)
      expect(notices(notifier, "Envoy inbound unavailable")).toHaveLength(1)
    } finally {
      await session.shutdown()
    }
  }, 30_000)
}

test("a connection closed for good between a /clear's deregistration and its registration tells the outage once, and the /clear completes on the new connection", async () => {
  const SESSION = "ses_clear_closed"
  const NEXT = "ses_clear_closed_next"
  const directory = scratchState()
  const handoff = sessionHandoffFile(directory, 779)
  await writeSessionHandoff(handoff, SESSION)
  const first = new FakeNats()
  const second = new FakeNats()
  const notifier = new FakeNotifier()
  /** Which connection each credential move ran on. */
  const moves: string[] = []
  const follow = (connection: FakeNats, name: string) => async (): Promise<void> => {
    moves.push(name)
    // `followNatsCredential` on a connection closed for good fails, as it waits for a reconnect
    // that never comes.
    if (connection.isClosed()) throw connectionClosedError()
  }
  const reopened = Promise.withResolvers<void>()
  const registered: string[] = []
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: first,
      followSession: follow(first, "first"),
      notifier,
      heartbeatMs: 25,
      handoffPid: 779,
      reopen: async () => {
        await reopened.promise
        return { connection: second, followSession: follow(second, "second") }
      },
      client: recordingClient([], {
        subscribe: async (input) => {
          registered.push(input.sessionID)
          return noInterest(input.sessionID)
        },
        unregisterSession: async ({ sessionID }) => {
          // The hub closes the connection as the old id leaves, before the new id registers.
          if (sessionID === SESSION) {
            await first.closeForGood(
              serverRefusal("AUTHORIZATION_VIOLATION", "'Authorization Violation'"),
            )
          }
        },
      }),
    }),
  )
  try {
    await writeSessionHandoff(handoff, NEXT)
    await waitFor(
      async () => notices(notifier, "Envoy inbound unavailable").length === 1,
      "the outage told",
      () => JSON.stringify({ moves, registered }),
    )
    expect(notices(notifier, "Envoy inbound unavailable")[0]).toContain(
      "because its NATS connection failed: Authorization Violation",
    )
    // Several heartbeats pass with no connection, and none moves the credential on the closed one
    // or flushes or subscribes on it. An absence needs the time to pass: eight 25 ms heartbeats.
    const callsBefore = first.flushes + first.subscribes
    await Bun.sleep(200)
    expect(moves).toEqual([])
    expect(first.flushes + first.subscribes).toBe(callsBefore)
    expect(registered).not.toContain(NEXT)

    reopened.resolve()
    await waitFor(
      async () =>
        registered.includes(NEXT) &&
        second.subscriptions.has(deliverSubject(NEXT)) &&
        notices(notifier, "Envoy inbound restored").length === 1,
      "the /clear completed on the new connection, and the end told",
      () => JSON.stringify({ moves, registered }),
    )
    expect(moves).toEqual(["second"])
    expect(notices(notifier, "Envoy inbound unavailable")).toHaveLength(1)
    expect(notices(notifier, "Envoy inbound restored")).toHaveLength(1)
  } finally {
    await session.shutdown()
  }
}, 30_000)
