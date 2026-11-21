// The startup flush outlasts a redial: nats.js rejects every pending flush with DISCONNECT each
// time the connection redials, and a channel server whose hub was restarting as it started must
// come up once the connection is back, not end the Claude Code session's channel for good.
import { expect, test } from "bun:test"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { settled, until } from "@legion/test-fixtures/wait"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

test("a startup flush the redial rejects is asked again, and the session registers", async () => {
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.failFlushes = 2
  const identity = new SessionIdentity("ses_redial", "/tmp")
  const session = await startChannelSession(
    sessionOptions(identity, scratchState(), {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 60_000,
    }),
  )
  try {
    await waitFor(
      async () => calls.some((call) => call.startsWith("subscribe ses_redial")),
      "the first registration",
    )
    // Two flushes the redial rejected, then the one that reached the server.
    expect(nats.flushes).toBe(3)
  } finally {
    await session.shutdown()
  }
})

// A session given no way to replace its connection (`ChannelSessionOptions.reopen`) cannot start
// without it; the channel servers give theirs one, and the rows after these show what it does.
test("a startup flush on a connection closed for good still fails the start", async () => {
  const nats = new FakeNats([])
  nats.failFlushes = 1
  void nats.closeForGood()
  const identity = new SessionIdentity("ses_closed", "/tmp")
  const started = startChannelSession(
    sessionOptions(identity, scratchState(), {
      connection: nats,
      client: recordingClient([]),
      heartbeatMs: 60_000,
    }),
  )
  // Closed before the first attempt: the start says so, and registers nothing.
  await expect(started).rejects.toThrow(
    "the NATS connection closed before its subscriptions reached the server",
  )
})

/** Starts a session on `nats` with `signal` as its start signal, runs `meanwhile` once the start
 *  is under way, and answers what the start came to; a start that never ends fails at the row's
 *  deadline, naming it. */
async function startOutcome(
  nats: FakeNats,
  signal: AbortSignal,
  calls: string[],
  meanwhile: () => Promise<void> = async () => {},
) {
  const identity = new SessionIdentity(`ses_stop_${crypto.randomUUID().slice(0, 8)}`, "/tmp")
  const started = startChannelSession(
    sessionOptions(identity, scratchState(), {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 60_000,
      startSignal: signal,
    }),
  )
  const ended = started.then(
    async (session) => {
      await session.shutdown()
      return "started" as const
    },
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  await meanwhile()
  const outcome = await settled("the start's outcome", ended)
  return { outcome, registered: calls.some((call) => call.startsWith("subscribe ses_stop_")) }
}

const STOPPING = "the channel server stopped before its NATS subscriptions reached the server"

test("a start whose process is already stopping registers nothing", async () => {
  const stopping = new AbortController()
  stopping.abort()
  const calls: string[] = []
  expect(await startOutcome(new FakeNats(calls), stopping.signal, calls)).toEqual({
    outcome: STOPPING,
    registered: false,
  })
})

test("a stop while the startup flush is pending ends the start", async () => {
  const stopping = new AbortController()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.stallFlush = true
  expect(
    await startOutcome(nats, stopping.signal, calls, async () => {
      await until("the startup flush asked for", () => nats.flushes > 0)
      stopping.abort()
    }),
  ).toEqual({
    outcome: STOPPING,
    registered: false,
  })
})

test("a stop while the startup flush waits to ask again ends the start", async () => {
  const stopping = new AbortController()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.failFlushes = 1
  // The first flush is rejected at once; the stop lands once it was, during the wait before the
  // second.
  expect(
    await startOutcome(nats, stopping.signal, calls, async () => {
      await until("the first startup flush rejected", () => nats.flushes > 0)
      stopping.abort()
    }),
  ).toEqual({
    outcome: STOPPING,
    registered: false,
  })
})

test("a connection closed for good while the startup flush is pending ends the start", async () => {
  // nats.js's close does not reject a pending flush, so the start watches the connection itself.
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.stallFlush = true
  expect(
    await startOutcome(nats, new AbortController().signal, calls, async () => {
      await until("the startup flush asked for", () => nats.flushes > 0)
      void nats.closeForGood()
    }),
  ).toEqual({
    outcome: "the NATS connection closed before its subscriptions reached the server",
    registered: false,
  })
})

// The channel servers give every session a way to replace its connection, since neither Claude
// Code nor Codex starts a channel server again: a start whose connection closed for good before
// its flush goes on, and the session registers with its relay once the new connection is there.
test("with a way to replace it, a connection closed for good while the startup flush is pending is replaced, and the session registers with its relay on the new one", async () => {
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.stallFlush = true
  const replacement = new FakeNats(calls)
  const reopened = Promise.withResolvers<void>()
  const identity = new SessionIdentity("ses_start_replaced", "/tmp")
  const starting = startChannelSession(
    sessionOptions(identity, scratchState(), {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 60_000,
      reopen: async () => {
        await reopened.promise
        return { connection: replacement }
      },
    }),
  )
  // nats.js's close leaves a pending flush unsettled: the connection closes for good once the
  // startup flush is asked for, and the start goes on to replace it.
  await until("the startup flush asked for", () => nats.flushes > 0)
  void nats.closeForGood(
    Object.assign(new Error("'Authorization Violation'"), {
      name: "NatsError",
      code: "AUTHORIZATION_VIOLATION",
    }),
  )
  const session = await settled("the start on a connection closed for good", starting)
  const registered = (): boolean => calls.some((call) => call.startsWith("subscribe ses_start_"))
  try {
    // Started with no live connection: nothing registered with a relay it does not hold.
    expect(registered()).toBe(false)
    reopened.resolve()
    await waitFor(async () => registered(), "the registration on the new connection")
    expect(replacement.subscriptions.has(deliverSubject("ses_start_replaced"))).toBe(true)
  } finally {
    await session.shutdown()
  }
})

// A close the server makes itself is no loss to replace: a stop aborts the start signal and then
// closes the connection, and the start ends as a stop, dialing nothing.
test("with a way to replace it, a stop that closes the connection while the startup flush is pending ends the start and dials nothing", async () => {
  const stopping = new AbortController()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.stallFlush = true
  let dials = 0
  const started = startChannelSession(
    sessionOptions(new SessionIdentity("ses_start_stopped", "/tmp"), scratchState(), {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 60_000,
      startSignal: stopping.signal,
      reopen: async () => {
        dials += 1
        return { connection: new FakeNats() }
      },
    }),
  )
  await until("the startup flush asked for", () => nats.flushes > 0)
  stopping.abort()
  void nats.closeForGood()
  await expect(started).rejects.toThrow(STOPPING)
  expect(dials).toBe(0)
  expect(calls.some((call) => call.startsWith("subscribe ses_start_"))).toBe(false)
})
