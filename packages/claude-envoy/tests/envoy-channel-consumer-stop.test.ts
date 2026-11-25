import { expect, test } from "bun:test"
import { EnvoyApiError, type SubscribeInput } from "@legion/envoy-client/transport"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  heldNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

// A session whose shutdown has begun is told nothing about its inbound (R24 of
// dispatch://INFRA-3589). These rows drive a registered session through two paths that run on
// after its shutdown began: a heartbeat that runs after the shutdown closed the relay, and a
// registration on the wire when the shutdown began that the listener refuses during it. Each of
// these absences has a control with no shutdown begun, in which the session is told. The
// heartbeat's control is the row after it here. The refusal's control is the 409 `not_requested`
// row of `envoy-channel-consumer.test.ts`, the file of the session's other consumer rows.

isolatePaneEnvironment()

const scratchState = useScratchState()

const SESSION = "ses_claude"

// R24 (INFRA-3589): a session is told it receives nothing only when that is so. A session that
// closes its own relay on purpose - its shutdown closes the relay before the registration chain
// is drained - has lost nothing, and a heartbeat that was waiting on that chain and runs after
// the close is no outage. This test holds that race open on purpose. Its control is the row
// after it.
test("a heartbeat that runs after the session's shutdown closed its relay tells the session nothing", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let hold: PromiseWithResolvers<void> | undefined
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      const gate = hold
      if (gate !== undefined) await gate.promise
      return noInterest(input.sessionID)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
      heartbeatMs: 10,
    }),
  )
  // A notice this server writes for itself is the one write that names no Envoy event.
  const notices = () => notifier.written.filter((written) => written.eventId === undefined)
  try {
    // The session is registered and receiving: a later relay-less registration is no start race.
    await waitFor(async () => registrations.length >= 2, "two accepted registrations")
    expect(notices()).toEqual([])

    // A follow's registration is held on the wire, so the next heartbeat waits behind it on
    // the registration chain.
    hold = Promise.withResolvers<void>()
    const asked = registrations.length
    const following = session.follow(["notifications.team.r24-shutdown"])
    await waitFor(async () => registrations.length > asked, "the held registration")
    // The heartbeat's own interval timer is what puts its write on the chain, and nothing
    // marks a write queued there, so the test lets several intervals pass.
    await Bun.sleep(60)

    // The session shuts down: it closes its relay, then waits out the chain, and the held
    // registration is answered meanwhile.
    const stopping = session.shutdown()
    hold.resolve()
    await following.catch(() => [])
    await stopping
    for (const write of notifier.pending) write.resolve()

    expect(notices().map((written) => written.content)).toEqual([])
  } finally {
    hold?.resolve()
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})

// The control of the row above: the same registered session, whose relay the connection closes
// for good with no shutdown begun while a follow's registration is held on the wire. A heartbeat
// after that finds no relay subject, and a registration was accepted already, so that is no start
// race: the session is told at once.
test("control: a heartbeat that runs after the connection closed the relay with no shutdown begun tells the session its inbound is unavailable", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let hold: PromiseWithResolvers<void> | undefined
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      const gate = hold
      if (gate !== undefined) await gate.promise
      return noInterest(input.sessionID)
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
      heartbeatMs: 10,
    }),
  )
  // A notice this server writes for itself is the one write that names no Envoy event.
  const notices = () => notifier.written.filter((written) => written.eventId === undefined)
  try {
    await waitFor(async () => registrations.length >= 2, "two accepted registrations")
    expect(notices()).toEqual([])

    // A follow's registration is held on the wire, and heartbeats queue behind it, as in the row
    // above: nothing marks a write queued on the chain, so the row lets several intervals pass.
    hold = Promise.withResolvers<void>()
    const asked = registrations.length
    const following = session.follow(["notifications.team.r24-no-shutdown"])
    await waitFor(async () => registrations.length > asked, "the held registration")
    await Bun.sleep(60)

    // The connection closes for good, which closes the relay, and the held registration is
    // answered meanwhile. No shutdown begins.
    await nats.closeForGood()
    hold.resolve()
    await following.catch(() => [])
    await waitFor(async () => {
      for (const write of notifier.pending) write.resolve()
      return notices().length > 0
    }, "the outage notice")

    expect(notices().map((written) => written.content)).toEqual([
      expect.stringContaining("Envoy inbound unavailable"),
    ])
  } finally {
    hold?.resolve()
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})

// The outage report's own rule says that a stopping session is told nothing
// (`inboundUnavailable`'s `stopping`). `register` sends nothing once the stop began, but a
// registration already on the wire when it began is answered during it, and its refusal says
// nothing about what a session whose record is deleted next receives.
test("a registration on the wire when the session's shutdown began and refused during it tells the session nothing", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let hold: PromiseWithResolvers<void> | undefined
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      const gate = hold
      if (gate === undefined) return noInterest(input.sessionID)
      await gate.promise
      throw new EnvoyApiError({
        method: "POST",
        url: "http://127.0.0.1:4000/v1/interests/subscribe",
        status: 409,
        responseBody: JSON.stringify({
          error: "the listener could not give this session a consumer",
          reason: "not_requested",
        }),
      })
    },
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
    }),
  )
  try {
    // A follow's registration is held on the wire.
    hold = Promise.withResolvers<void>()
    const asked = registrations.length
    const following = session.follow(["notifications.team.r24-refused"])
    await waitFor(async () => registrations.length > asked, "the held registration")

    // The session shuts down, and the listener refuses the held registration meanwhile.
    const stopping = session.shutdown()
    hold.resolve()
    await following.catch(() => [])
    await stopping
    for (const write of notifier.pending) write.resolve()

    // A notice this server writes for itself is the one write that names no Envoy event.
    const notices = notifier.written.filter((written) => written.eventId === undefined)
    expect(notices.map((written) => written.content)).toEqual([])
  } finally {
    hold?.resolve()
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})
