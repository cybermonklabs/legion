// The channel record's `registered` line names the epoch the answered registration's request
// carried, which is the epoch the listener restarts the relay on. A reconnect begins a new epoch
// at once (`renewRelayEpoch`), outside the queue registrations go through, so the relay's epoch
// when an answer lands can be a later one than the answered request named.
import { expect, test } from "bun:test"
import { existsSync, readFileSync } from "node:fs"
import { join } from "node:path"
import { legacyAgentSubject } from "@legion/contracts"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import type { SubscribeInput, SubscribeResult } from "@legion/envoy-client/transport"
import { CHANNEL_RECORD_FILE, type ChannelRecordLine } from "../src/channel-record"
import { startChannelSession } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  noInterest,
  recordingClient,
  sessionOptions,
  settled,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const scratchState = useScratchState()

const SESSION = "ses_claude"

/** The epochs the record says were registered, in the order it wrote them. */
function recordedEpochs(file: string): string[] {
  if (!existsSync(file)) return []
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as ChannelRecordLine)
    .flatMap((line) => ("registered" in line ? [line.registered] : []))
}

test("an answer to a registration sent before a reconnect records the epoch that registration named", async () => {
  const directory = scratchState()
  const recordFile = join(directory, "channel-record.jsonl")
  const previous = process.env[CHANNEL_RECORD_FILE]
  process.env[CHANNEL_RECORD_FILE] = recordFile
  const nats = new FakeNats()
  const registrations: SubscribeInput[] = []
  const answer = (): SubscribeResult => ({
    ...noInterest(),
    topics: [legacyAgentSubject(SESSION)],
    delivery: consumerDelivery(SESSION, { streamID: "stream-1" }),
  })
  /** The registration the listener holds until the test answers it, once `holdNext` is set. */
  let holdNext = false
  const held = Promise.withResolvers<SubscribeResult>()
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      if (!holdNext) return answer()
      holdNext = false
      return held.promise
    },
  })
  try {
    const session = await startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, { connection: nats, client }),
    )
    try {
      expect(registrations).toHaveLength(1)
      // A reconnect begins a new epoch and registers it; the listener holds that registration.
      holdNext = true
      nats.emitStatus("reconnect")
      await waitFor(async () => registrations.length === 2, "the registration after a reconnect")
      // A second reconnect begins a third epoch while that registration is on the wire; its own
      // registration waits behind the one held.
      nats.emitStatus("reconnect")
      await settled()
      held.resolve(answer())
      await waitFor(async () => registrations.length === 3, "the registration after the second")
      await waitFor(
        async () => recordedEpochs(recordFile).length === 3,
        "the record of three answered registrations",
      )
      const sent = registrations.map((registration) => registration.delivery?.epoch)
      expect(new Set(sent).size).toBe(3)
      expect(recordedEpochs(recordFile)).toEqual(sent as string[])
    } finally {
      await session.shutdown()
    }
  } finally {
    if (previous === undefined) delete process.env[CHANNEL_RECORD_FILE]
    else process.env[CHANNEL_RECORD_FILE] = previous
  }
})
