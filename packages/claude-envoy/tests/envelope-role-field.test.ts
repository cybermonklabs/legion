import { expect, test } from "bun:test"
import type { Envelope } from "@legion/contracts"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import { createEnvoyClient } from "@legion/envoy-client/transport"
import envelopeWithoutRole from "../../contracts/fixtures/envelope-without-role.json"
import { deliveryIdentity } from "../src/channel-topics"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  CLAUDE_ENVOY_USER_AGENT,
  createChannelDelivery,
  executeEnvoyTool,
  startChannelSession,
} from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNats,
  FakeNotifier,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

// Every reader of an envelope a listener delivers in this package, given one that carries `role`:
// the field the listener stores on a message Dispatch's API sent to a role's holder for a route to
// that role, and echoes in the answer to that send. Each row reads one envelope with the field and
// without it, and checks that the reader reads every other field as before. No reader here names
// the field: each one parses with the shared renderer, the shared identity schema or the shared
// transport (`@legion/envoy-client`), whose own rows are in its `transport.test.ts`.

isolatePaneEnvironment()

const scratchState = useScratchState()
const SESSION = "ses_claude"
const AGENT = `notifications.agent.${SESSION}`
const ROLE = "legion-legion-legion-1-architect"
const ROLE_TOPIC = `notifications.role.${ROLE}`

/** Every field `EnvelopeSchema` names except `role`, each one set, on `SESSION`'s own subject. */
const withoutRole = {
  ...(envelopeWithoutRole as Omit<Envelope, "role">),
  topic: AGENT,
  dedupe_key: `agent.${SESSION}.0123456789abcdef0123456789abcdef`,
} satisfies Omit<Envelope, "role">
const withRole = { ...withoutRole, role: ROLE }

test("createChannelDelivery (envoy-channel-server.ts enqueue: renderInbound, the inbox entry, the notification): a frame with the role field is written and listed as without it", async () => {
  const written = async (envelope: object) => {
    const notifier = new FakeNotifier()
    const delivery = createChannelDelivery({
      identity: new SessionIdentity(SESSION, "/tmp"),
      notifier,
      configFile: sessionConfigFile("claude", process.env),
      userAgent: CLAUDE_ENVOY_USER_AGENT,
    })
    expect(await delivery.enqueue({ subject: AGENT, raw: JSON.stringify(envelope) })).toBe(true)
    return { notifications: notifier.notifications, inbox: delivery.inbox() }
  }
  const before = await written(withoutRole)

  expect(await written(withRole)).toEqual(before)
  expect(before.notifications).toHaveLength(1)
  expect(before.notifications[0]?.eventId).toBe(withoutRole.event_id)
  expect(before.notifications[0]?.content).toContain(`to: you (${SESSION})`)
  expect(before.notifications[0]?.content).toContain("message: Review the plan before noon.")
  expect(before.inbox).toEqual([
    {
      event_id: withoutRole.event_id,
      at: "2026-11-18T09:00:00Z",
      from: "ses_sender",
      summary: "Review the plan",
    },
  ])
})

test("deliveryIdentity (channel-topics.ts, DedupeIdentitySchema extended): a frame with the role field has the identity it has without it", () => {
  const before = deliveryIdentity(JSON.stringify(withoutRole))

  expect(deliveryIdentity(JSON.stringify(withRole))).toStrictEqual(before)
  expect(before).toStrictEqual({
    event_id: withoutRole.event_id,
    dedupe_key: withoutRole.dedupe_key,
    source: "agent",
    source_event_id: withoutRole.source_event_id,
    topic: AGENT,
  })
})

test("the channel session's relay (channel-topics.ts deliveryIdentity, consumer-delivery.ts parseIdentity): a copy of a written frame that carries the role field is the same message, written once", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier(nats.order)
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
      connection: nats,
      notifier,
    }),
  )

  try {
    nats.emitRelay(SESSION, {
      subject: AGENT,
      seq: 1,
      raw: JSON.stringify(withoutRole),
      reply: "_INBOX.1",
    })
    nats.emitRelay(SESSION, {
      subject: AGENT,
      seq: 2,
      raw: JSON.stringify(withRole),
      reply: "_INBOX.2",
    })
    await waitFor(
      async () =>
        nats.repliesOn("_INBOX.1").includes("done") && nats.repliesOn("_INBOX.2").includes("done"),
      "both frames answered",
    )

    expect(notifier.notifications).toHaveLength(1)
    expect(notifier.notifications[0]?.eventId).toBe(withoutRole.event_id)
    expect(nats.repliesOn("_INBOX.1")).toEqual(["received", "done"])
    expect(nats.repliesOn("_INBOX.2")).toEqual(["received", "done"])
  } finally {
    await session.shutdown()
  }
})

/** The session the tools act for, following nothing but its own subject. */
const toolSession: ChannelSession = {
  delivery: { enqueue: async () => true, inbox: () => [] },
  topics: () => [AGENT],
  follow: async () => [],
  unfollow: async () => [],
  rememberRole: async () => undefined,
  onRoleRegained: () => undefined,
  capabilitiesChanged: () => undefined,
  observe: () => undefined,
  messageOf: () => undefined,
  shutdown: async () => undefined,
}

/** A runtime whose listener answers every send and publish with `answer` and `status`. */
function runtimeAnswering(answer: object, status = 200): ChannelToolRuntime {
  return {
    identity: new SessionIdentity(SESSION, process.cwd()),
    client: createEnvoyClient({
      baseUrl: "http://envoy.test",
      fetch: async () => Response.json(answer, { status }),
    }),
    session: toolSession,
    clientInstanceID: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
  }
}

test("envoy_send (envoy-channel-server.ts executeEnvoyTool: the transport's send answer, sendConfirmationText): an answer with the role field gives the tool result it gives without it", async () => {
  const answer = { recipient: "ses_receiver", recipient_state: "live", state: "connected" }
  const sent = (envelope: object) =>
    executeEnvoyTool(runtimeAnswering({ ...envelope, ...answer }), "envoy_send", {
      session_id: "ses_receiver",
      message: "Review the plan",
    })
  const before = await sent(withoutRole)

  expect(await sent(withRole)).toStrictEqual(before)
  expect(before).toMatchObject({
    message: `sent ${withoutRole.event_id} to ses_receiver (live)`,
    event_id: withoutRole.event_id,
    recipient: "ses_receiver",
    recipient_state: "live",
    confirmed: true,
    state: "connected",
  })
})

for (const [label, status, answer] of [
  ["stored for the role's live holder", 200, { holder: "ses_holder", holder_state: "live" }],
  [
    "kept for the role's next holder",
    202,
    {
      holder: "ses_holder",
      holder_state: "unresponsive",
      last_seen: Date.parse("2026-11-18T08:50:00Z"),
      kept: true,
      kept_reason: "holder_lapsed",
      kept_until: Date.parse("2026-11-18T10:00:00Z"),
    },
  ],
] as const) {
  test(`envoy_publish (envoy-channel-server.ts executeEnvoyTool: the transport's publish answer, publishConfirmationText, publishResultDetails): an answer ${label} with the role field gives the tool result it gives without it`, async () => {
    const published = (envelope: object) =>
      executeEnvoyTool(
        runtimeAnswering({ ...envelope, topic: ROLE_TOPIC, ...answer }, status),
        "envoy_publish",
        { topic: ROLE_TOPIC, message: "Review the plan" },
      )
    const before = await published(withoutRole)

    expect(await published(withRole)).toStrictEqual(before)
    expect(before).toMatchObject({
      event_id: withoutRole.event_id,
      topic: ROLE_TOPIC,
      holder: "ses_holder",
      holder_state: answer.holder_state,
    })
  })
}
