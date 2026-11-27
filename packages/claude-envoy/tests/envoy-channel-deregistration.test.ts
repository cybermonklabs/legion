// The shutdown's deregistration of a channel session: it waits for a registration the listener
// stalled on, then goes out anyway, and that wait is one bound over the whole shutdown path.
import { expect, test } from "bun:test"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

// The record outlives this process by the live window and the gone horizon after it, so the
// deregistration is what ends the session for its peers: a registry write this listener has
// stalled on must delay it, never swallow it (dispatch://INFRA-392).
test("the shutdown deregistration waits for a stalled registration, then goes out anyway", async () => {
  const stateDirectory = scratchState()
  const calls: string[] = []
  const wedged = Promise.withResolvers<void>()
  let registrations = 0
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: new FakeNats(),
      registrationSettleMs: 50,
      heartbeatMs: 25,
      client: recordingClient(calls, {
        subscribe: async (input) => {
          calls.push(`subscribe ${input.sessionID}`)
          registrations += 1
          // The first registration answers; every later one never does.
          if (registrations > 1) await wedged.promise
          return noInterest()
        },
      }),
    }),
  )

  try {
    await waitFor(async () => registrations > 1, "a registration the listener never answers")

    await session.shutdown()

    expect(calls).toContain("unregister ses_claude")
  } finally {
    wedged.resolve()
  }
})

// The wait above is the shutdown path's whole budget, not one each: the two five-second waits in
// a row left ten seconds before the DELETE was sent, and the process group is killed five
// seconds after SIGTERM, so the record survived for the gone horizon (dispatch://INFRA-392).
test("the shutdown deregistration spends a quarter second settling, not the whole of both waits", async () => {
  const stateDirectory = scratchState()
  const calls: string[] = []
  const wedged = Promise.withResolvers<void>()
  let registrations = 0
  const session = await startChannelSession(
    // No `registrationSettleMs`: this is the shipped bound, over both the write chain the wedged
    // registration holds and that registration itself.
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: new FakeNats(),
      heartbeatMs: 25,
      client: recordingClient(calls, {
        subscribe: async (input) => {
          calls.push(`subscribe ${input.sessionID}`)
          registrations += 1
          if (registrations > 1) await wedged.promise
          return noInterest()
        },
      }),
    }),
  )

  try {
    await waitFor(async () => registrations > 1, "a registration the listener never answers")

    // Real time: the bound is a real timer, and the failure is that the DELETE waits out two
    // five-second settles the process is never given.
    const startedAt = Date.now()
    await session.shutdown()
    const elapsed = Date.now() - startedAt

    expect(calls).toContain("unregister ses_claude")
    expect(elapsed).toBeLessThan(2_000)
  } finally {
    wedged.resolve()
  }
})
