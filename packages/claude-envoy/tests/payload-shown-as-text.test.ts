import { expect, spyOn, test } from "bun:test"
import { createChannelDelivery } from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import { FakeNotifier, useScratchState } from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

// A peer's message whose JSON payload is nested past the depth the card's TOON encoder reaches
// made the shared renderer throw, so every copy of the frame failed and went back to the stream.
// The card shows such a payload as the text its sender sent, and the server says so once on
// stderr, naming the message.

isolatePaneEnvironment()
useScratchState()

const SESSION = "ses_claude"
const AGENT = `notifications.agent.${SESSION}`

test("createChannelDelivery (envoy-channel-server.ts enqueue): a message whose payload is nested 9,999 deep is written as its text, and logged once with its id", async () => {
  const nested = `${'{"a":'.repeat(9_999)}1${"}".repeat(9_999)}`
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  try {
    const notifier = new FakeNotifier()
    const delivery = createChannelDelivery({
      identity: new SessionIdentity(SESSION, "/tmp"),
      notifier,
      // The session's answer to a Dispatch delivery: a peer's message is answered by nobody.
      reply: async () => {
        throw new Error("a peer's message takes no Dispatch reply")
      },
      stderr: (line) => void process.stderr.write(line),
    })
    const raw = JSON.stringify({
      event_id: "evt-deep",
      source: "agent",
      topic: AGENT,
      payload: nested,
    })

    expect(await delivery.enqueue({ subject: AGENT, raw })).toBe(true)
    expect(notifier.notifications.map(({ eventId }) => eventId)).toEqual(["evt-deep"])
    // TOON quotes the text as JSON quotes a string, so the card holds the sender's text whole.
    expect(notifier.notifications[0]?.content).toContain(`\n  message: ${JSON.stringify(nested)}`)
    expect(written.filter((line) => line.includes("shown as its text"))).toEqual([
      "envoy-channel: message evt-deep's payload is shown as its text; the card could not lay it out (its layout is larger than the 1048576 bytes a card holds)\n",
    ])
  } finally {
    stderr.mockRestore()
  }
})

// Envoy sends a frame again when its answer did not arrive, and the mod writes one event once
// (`../hooks/mod-writer.ts`): the copy says nothing the first did not, so it logs nothing either.
test("createChannelDelivery (envoy-channel-server.ts enqueue, list): a copy of a message shown as its text is not logged again", async () => {
  const nested = `${'{"a":'.repeat(9_999)}1${"}".repeat(9_999)}`
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  try {
    const delivery = createChannelDelivery({
      identity: new SessionIdentity(SESSION, "/tmp"),
      notifier: new FakeNotifier(),
      // The session's answer to a Dispatch delivery: a peer's message is answered by nobody.
      reply: async () => {
        throw new Error("a peer's message takes no Dispatch reply")
      },
      stderr: (line) => void process.stderr.write(line),
    })
    const raw = JSON.stringify({
      event_id: "evt-again",
      source: "agent",
      topic: AGENT,
      payload: nested,
    })

    expect(await delivery.enqueue({ subject: AGENT, raw })).toBe(true)
    expect(await delivery.enqueue({ subject: AGENT, raw })).toBe(true)
    expect(written.filter((line) => line.includes("shown as its text"))).toEqual([
      "envoy-channel: message evt-again's payload is shown as its text; the card could not lay it out (its layout is larger than the 1048576 bytes a card holds)\n",
    ])
  } finally {
    stderr.mockRestore()
  }
})
