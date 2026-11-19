// The startup flush outlasts a redial: nats.js rejects every pending flush with DISCONNECT each
// time the connection redials, and a channel server whose hub was restarting as it started must
// come up once the connection is back, not end the Claude Code session's channel for good.
import { expect, test } from "bun:test"
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

test("a startup flush on a connection closed for good still fails the start", async () => {
  const nats = new FakeNats([])
  nats.failFlushes = 1
  nats.closed = true
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

/** Starts a session on `nats` with `signal` as its start signal; what the start came to. */
async function startOutcome(nats: FakeNats, signal: AbortSignal, calls: string[]) {
  const identity = new SessionIdentity(`ses_stop_${crypto.randomUUID().slice(0, 8)}`, "/tmp")
  const started = startChannelSession(
    sessionOptions(identity, scratchState(), {
      connection: nats,
      client: recordingClient(calls),
      heartbeatMs: 60_000,
      startSignal: signal,
    }),
  )
  const late = Bun.sleep(3_000).then(() => "still starting" as const)
  const outcome = await Promise.race([
    started.then(
      async (session) => {
        await session.shutdown()
        return "started" as const
      },
      (error: unknown) => (error instanceof Error ? error.message : String(error)),
    ),
    late,
  ])
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
  setTimeout(() => stopping.abort(), 100)
  expect(await startOutcome(nats, stopping.signal, calls)).toEqual({
    outcome: STOPPING,
    registered: false,
  })
})

test("a stop while the startup flush waits to ask again ends the start", async () => {
  const stopping = new AbortController()
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.failFlushes = 1
  // The first flush is rejected at once; the stop lands during the wait before the second.
  setTimeout(() => stopping.abort(), 50)
  expect(await startOutcome(nats, stopping.signal, calls)).toEqual({
    outcome: STOPPING,
    registered: false,
  })
})

test("a connection closed for good while the startup flush is pending ends the start", async () => {
  // nats.js's close does not reject a pending flush, so the start watches the connection itself.
  const calls: string[] = []
  const nats = new FakeNats(calls)
  nats.stallFlush = true
  setTimeout(() => {
    nats.closed = true
  }, 100)
  expect(await startOutcome(nats, new AbortController().signal, calls)).toEqual({
    outcome: "the NATS connection closed before its subscriptions reached the server",
    registered: false,
  })
})
