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
//   it, and the outage is told, naming the close;
// - a `/clear` written while the connection is closed for good is read by a heartbeat that the
//   registry chain held as the replacement connected: the heartbeat reaches the new connection
//   before that connection's own set-up, and the `/clear` must wait for the set-up, then complete
//   and be reported, rather than move the identity and end with the new id's registration refused;
// - the relay subscription ends as the session shuts down, with a registration still queued for
//   it: the leaving session is told no outage, and nothing registers behind the DELETE.
import { expect, spyOn, test } from "bun:test"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import type { SubscribeInput } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import * as sessionIdentity from "../src/session-identity"
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
      "because its relay subject could not be opened: the server refused a permission (its text is withheld)",
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
      "because its relay subject could not be opened: Authorization Violation",
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

// The window a loaded machine opens (dispatch://INFRA-3904, the reopen row "a /clear while the NATS
// connection is closed for good"): a heartbeat queued in the registry chain while the connection
// is closed for good runs after the replacement has connected and before the replacement's own step
// in the chain, which clears the loss. Before the fix that heartbeat adopted the `/clear` on the new
// connection, moved the identity and deregistered the old id, had the new id's registration refused
// for want of a relay, and the `/clear` was never reported. Each row below opens that window by its
// own facts, never by time passing, counts the handoff reads made in it (a row whose window never
// opened fails by name), and checks the one report against what the listener and the replacement
// connection hold when it is written: the new id registered and its relay subscribed there.
const WINDOW_ROW_MS = 30_000

/** The report's state as each write of it happens, and the waits of a window row, each to 5 s
 *  before the row's own timeout counted from its start (not the harness's 5 s): a slow machine
 *  slows the row and does not decide it, and a fact that never comes fails by name. */
function windowRow(session: string, next: string, second: FakeNats, registered: readonly string[]) {
  const rowEnds = performance.now() + WINDOW_ROW_MS - 5_000
  const report = `envoy: session id changed ${session} -> ${next}; re-registered`
  const written: string[] = []
  /** At each write of the report: was the new id registered, and its relay subscribed on the
   *  replacement connection? */
  const atReport: { readonly registered: boolean; readonly relay: boolean }[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    const text = String(chunk)
    written.push(text)
    if (text.includes(report)) {
      atReport.push({
        registered: registered.includes(next),
        relay: second.subscriptions.has(deliverSubject(next)),
      })
    }
    return true
  })
  return {
    atReport,
    stderr,
    untilRowEnds: (): number => Math.max(0, rowEnds - performance.now()),
    /** Waits until the `/clear` has completed: the new id registered, its relay subscribed on the
     *  replacement connection, and a report written. */
    completed: async (untilRowEnds: () => number): Promise<void> => {
      await waitFor(
        async () =>
          registered.includes(next) &&
          second.subscriptions.has(deliverSubject(next)) &&
          atReport.length > 0,
        "the /clear's report, with the new id registered and its relay subscribed",
        () => JSON.stringify({ registered, atReport, stderr: written.join("") }),
        untilRowEnds(),
      )
    },
  }
}

test(
  "a /clear read by a heartbeat held behind a registry write as the new connection opens waits for that connection's set-up, then completes and is reported once",
  async () => {
    const SESSION = "ses_clear_window"
    const NEXT = "ses_clear_window_next"
    const TOPIC = "notifications.dispatch.issue.DSP-1"
    const directory = scratchState()
    const handoff = sessionHandoffFile(directory, 781)
    await writeSessionHandoff(handoff, SESSION)
    const first = new FakeNats()
    const second = new FakeNats()
    const reopened = Promise.withResolvers<void>()
    const reopenAsked = Promise.withResolvers<void>()
    let held: PromiseWithResolvers<void> | undefined
    const heldAsked = Promise.withResolvers<void>()
    /** Each extra heartbeat a failed heartbeat armed, for the row to run when it chooses. */
    const retries: (() => void)[] = []
    const registered: string[] = []
    /** How many registrations the listener had when the replacement was let connect. */
    let registeredAtReopen: number | undefined
    /** Handoff reads that found the `/clear` after the replacement had connected and before its
     *  set-up registered anything: reads made in the window. */
    let windowReads = 0
    const readHandoff = sessionIdentity.readSessionHandoff
    const reads = spyOn(sessionIdentity, "readSessionHandoff").mockImplementation(async (file) => {
      const read = await readHandoff(file)
      if (
        read === NEXT &&
        second.subscriptions.has(deliverSubject(SESSION)) &&
        registered.length === registeredAtReopen
      ) {
        windowReads += 1
      }
      return read
    })
    const row = windowRow(SESSION, NEXT, second, registered)
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
        connection: first,
        notifier: new FakeNotifier(),
        heartbeatMs: 25,
        handoffPid: 781,
        scheduleRetry: (run) => {
          retries.push(run)
          return () => undefined
        },
        reopen: async () => {
          reopenAsked.resolve()
          await reopened.promise
          return { connection: second }
        },
        client: recordingClient([], {
          subscribe: async (input) => {
            registered.push(input.sessionID)
            return noInterest(input.sessionID)
          },
          unsubscribe: async () => {
            if (held === undefined) return
            heldAsked.resolve()
            await held.promise
          },
        }),
      }),
    )
    try {
      expect(await session.follow([TOPIC])).toEqual([TOPIC])
      await first.closeForGood(
        serverRefusal("AUTHORIZATION_VIOLATION", "'Authorization Violation'"),
      )
      await reopenAsked.promise
      await writeSessionHandoff(handoff, NEXT)
      // A heartbeat in the outage fails, as the registration has no relay, and arms its one retry.
      await waitFor(
        async () => retries.length > 0,
        "a retry armed by a failed heartbeat",
        () => JSON.stringify({ registered }),
        row.untilRowEnds(),
      )
      // A held registry write keeps the chain busy. The retry's heartbeat enters the chain behind
      // it at once, as the call returns; a heartbeat already in flight now can only be waiting
      // behind that write too.
      held = Promise.withResolvers<void>()
      const unfollowed = session.unfollow([TOPIC])
      await heldAsked.promise
      retries.at(-1)?.()
      registeredAtReopen = registered.length
      reopened.resolve()
      // The replacement has connected once it takes the relay subject on it, which `reopenFor`
      // does as it takes the connection, before it queues its own set-up behind that heartbeat.
      await waitFor(
        async () => second.subscriptions.has(deliverSubject(SESSION)),
        "the replacement connection taken",
        () => JSON.stringify({ registered }),
        row.untilRowEnds(),
      )
      expect(registered).not.toContain(NEXT)
      held.resolve()
      expect(await unfollowed).toEqual([TOPIC])
      await row.completed(row.untilRowEnds)
      if (windowReads === 0) {
        throw new Error("no heartbeat read the /clear in the window: the row opened no window")
      }
      expect(row.atReport).toEqual([{ registered: true, relay: true }])
    } finally {
      reads.mockRestore()
      row.stderr.mockRestore()
      await session.shutdown()
    }
  },
  WINDOW_ROW_MS,
)

// The same window opened during the handoff read itself: the connection is live as a heartbeat
// starts to read the handoff file, and closes for good, and its replacement connects, while that
// read is awaited. A check of the loss made before the read passes and lets the `/clear` adopt on
// the replacement before its set-up; only a check after the read sees the loss.
test(
  "a /clear whose handoff read spans a close for good and the replacement's connect waits for that connection's set-up, then completes and is reported once",
  async () => {
    const SESSION = "ses_clear_read"
    const NEXT = "ses_clear_read_next"
    const directory = scratchState()
    const handoff = sessionHandoffFile(directory, 782)
    await writeSessionHandoff(handoff, SESSION)
    const first = new FakeNats()
    const second = new FakeNats()
    const reopened = Promise.withResolvers<void>()
    const reopenAsked = Promise.withResolvers<void>()
    const registered: string[] = []
    const row = windowRow(SESSION, NEXT, second, registered)
    let armed = false
    /** Handoff reads that the close for good and the replacement's connect spanned. */
    let spanned = 0
    const readHandoff = sessionIdentity.readSessionHandoff
    const reads = spyOn(sessionIdentity, "readSessionHandoff").mockImplementation(async (file) => {
      const read = await readHandoff(file)
      if (armed && read === NEXT) {
        armed = false
        await first.closeForGood(
          serverRefusal("AUTHORIZATION_VIOLATION", "'Authorization Violation'"),
        )
        await reopenAsked.promise
        reopened.resolve()
        await waitFor(
          async () => second.subscriptions.has(deliverSubject(SESSION)),
          "the replacement connection taken during the handoff read",
          () => JSON.stringify({ registered }),
          row.untilRowEnds(),
        )
        spanned += 1
      }
      return read
    })
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
        connection: first,
        notifier: new FakeNotifier(),
        heartbeatMs: 25,
        handoffPid: 782,
        reopen: async () => {
          reopenAsked.resolve()
          await reopened.promise
          return { connection: second }
        },
        client: recordingClient([], {
          subscribe: async (input) => {
            registered.push(input.sessionID)
            return noInterest(input.sessionID)
          },
        }),
      }),
    )
    try {
      armed = true
      await writeSessionHandoff(handoff, NEXT)
      await row.completed(row.untilRowEnds)
      if (spanned !== 1) {
        throw new Error(
          `the close and the replacement's connect spanned ${spanned} handoff reads, not 1`,
        )
      }
      expect(row.atReport).toEqual([{ registered: true, relay: true }])
    } finally {
      reads.mockRestore()
      row.stderr.mockRestore()
      await session.shutdown()
    }
  },
  WINDOW_ROW_MS,
)

// A relay subscription that ends while a heartbeat's registration is still on the wire queues a
// registration behind it, which names the outage. Shutdown then closes the relay and sends the
// DELETE once its bounded wait for the chain gives up. The queued registration runs after that:
// it must tell the leaving session nothing (`inboundOutage` says nothing once shutdown began),
// which the shutdown guards alone ensure. That it registers nothing after the DELETE is kept by
// more than `register`'s own guard: the stopped relay sets `relayMissing`, so `registerWithRelay`
// sends nothing, and `closeRelay()` removes the relay; this row cannot tell those apart.
test("a registration queued behind a held listener call as the session shuts down tells no outage and registers nothing after the DELETE", async () => {
  const SESSION = "ses_relay_shutdown"
  const RELAY = deliverSubject(SESSION)
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const calls: string[] = []
  // The start's registration is answered at once; the first heartbeat's is held until the test
  // answers it, after the DELETE.
  const held = Promise.withResolvers<void>()
  const heldAsked = Promise.withResolvers<void>()
  let registrations = 0
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
      connection: nats,
      notifier,
      heartbeatMs: 25,
      // The shutdown's wait for the chain gives up quickly, so the DELETE goes out while the
      // heartbeat's registration still holds the chain.
      registrationSettleMs: 100,
      client: recordingClient(calls, {
        subscribe: async (input) => {
          calls.push(`subscribe ${input.sessionID}`)
          registrations += 1
          if (registrations > 1) {
            heldAsked.resolve()
            await held.promise
          }
          return noInterest(input.sessionID)
        },
      }),
    }),
  )
  try {
    await heldAsked.promise
    // The relay subscription ends with the server's refusal: a registration is queued behind the
    // held one, and would refuse itself and name the outage.
    nats.failSubscription(
      RELAY,
      serverRefusal(
        "PERMISSIONS_VIOLATION",
        `Permissions Violation for Subscription to "${RELAY}"`,
      ),
    )
    await waitFor(async () => nats.liveSubscriptions(RELAY) === 0, "the relay subscription ended")

    await session.shutdown()
    const deregisteredAt = calls.indexOf(`unregister ${SESSION}`)
    expect(deregisteredAt).toBeGreaterThan(-1)

    // The held registration is answered now, after the DELETE, and the queued one runs behind it.
    // Its absence needs the time to pass: a few turns are ample for a chain with no wait left.
    held.resolve()
    await Bun.sleep(200)
    expect(notices(notifier, "Envoy inbound unavailable")).toEqual([])
    expect(notices(notifier, "Envoy inbound restored")).toEqual([])
    expect(calls.slice(deregisteredAt + 1).filter((call) => call.startsWith("subscribe "))).toEqual(
      [],
    )
  } finally {
    held.resolve()
    await session.shutdown()
  }
}, 30_000)
