import { expect, setSystemTime, spyOn, test } from "bun:test"
import { createServer, type Socket } from "node:net"
import { join } from "node:path"
import { BUS_UNANSWERED_PREFIX, legacyAgentSubject } from "@legion/contracts"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { INBOUND_FIRST_OPEN_BOUND_MS } from "@legion/envoy-client/inbound-outage"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import {
  type DeliveryRequest,
  EnvoyApiError,
  type SubscribeInput,
  type SubscribeResult,
} from "@legion/envoy-client/transport"
import { socketPathRefusal, socketRoot } from "@legion/test-fixtures/socket-root"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { startChannelSession } from "../src/envoy-channel-server"
import { createInboxNotifier, INBOX_CAPABILITIES } from "../src/legion-inbox"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  FakeNats,
  FakeNotifier,
  heldNotifier,
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
// The inbox's unix socket stands in for `legion claude-bridge`'s, which Go binds
// (packages/daemon/internal/claudebridge/inbox.go) and so holds to the 107 bytes a socket path holds
// on Linux (`socketPathRefusal`); Bun's own server binds past that. So it goes in a directory of its
// own, made under TMPDIR when its path fits there and under /tmp otherwise, which says so
// (`socketRoot`). The maker names a directory <prefix>-XXXXXX.
const inboxPrefix = "claude-envoy-inbox"
const inboxDirectory = temporaryDirectories(inboxPrefix, {
  root: socketRoot("envoy-channel-consumer.test.ts's inbox", (root) =>
    socketPathRefusal(join(root, `${inboxPrefix}-XXXXXX`, "inbox.sock")),
  ),
})

const SESSION = "ses_claude"
const DIRECT = legacyAgentSubject(SESSION)
const STREAM = "stream-1"

const CONSUMER = consumerDelivery(SESSION, { streamID: STREAM })

/** What a restored notice says of a session that received nothing before it. Row (e) below finds
 *  it in no notice after a bus-outage 503. The first relay open row after it finds it in the
 *  notice after an outage that left the session nothing: that absence's control. */
const RECEIVED_NOTHING = /receive[sd]? nothing/

/** A peer session's message to this session, `label` naming its event. */
function peerMessage(
  label: string,
  topic: string = DIRECT,
): { readonly raw: string; readonly id: string } {
  const id = `evt-${label}`
  return {
    id,
    raw: JSON.stringify({
      event_id: id,
      source: "agent",
      source_session: "ses_peer",
      topic,
      dedupe_key: `delivery-${label}`,
      issued_at: 1_760_000_000_000,
      payload_summary: `peer message ${label}`,
    }),
  }
}

/**
 * The listener, as the session's registrations reach it: `answer` decides what each one is
 * answered, or throws the refusal it is refused with.
 */
function listener(answer: (input: SubscribeInput) => SubscribeResult) {
  const registrations: SubscribeInput[] = []
  const client = recordingClient([], {
    subscribe: async (input) => {
      registrations.push(input)
      return answer(input)
    },
  })
  return { client, registrations }
}

const delivery = (input: SubscribeInput | undefined): DeliveryRequest | undefined => input?.delivery

test("a terminal session answers a relay frame received at once and done only once the write into the conversation resolves", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const held = heldNotifier()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: held.notifier,
      client,
    }),
  )
  try {
    expect(delivery(registrations[0])).toMatchObject({ consumer: true })
    const message = peerMessage("one")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 7, raw: message.raw, reply: "_INBOX.one" })
    await waitFor(async () => held.written.length === 1, "the notification write")
    // `received` went out before the write began, and nothing more while it is unresolved.
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received"])
    expect(held.written[0]?.eventId).toBe(message.id)
    await settled()
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received"])

    held.pending[0]?.resolve()
    await waitFor(async () => nats.repliesOn("_INBOX.one").length === 2, "done")
    expect(nats.repliesOn("_INBOX.one")).toEqual(["received", "done"])
  } finally {
    for (const write of held.pending) write.resolve()
    await session.shutdown()
  }
})

/** `legion claude-bridge`'s inbox, which answers each line once the test lets it. */
async function heldInbox() {
  const lines: { readonly id: string; answer(ok: boolean): void }[] = []
  const sockets = new Set<Socket>()
  const path = join(inboxDirectory(), "inbox.sock")
  const server = createServer((socket: Socket) => {
    sockets.add(socket)
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as { id: string }
        buffered = buffered.slice(end + 1)
        lines.push({
          id: line.id,
          answer: (ok) =>
            socket.write(
              `${JSON.stringify(ok ? { id: line.id, ok: true } : { id: line.id, error: "Claude Code has exited" })}\n`,
            ),
        })
      }
    })
  })
  await new Promise<void>((listening) => server.listen(path, listening))
  return {
    lines,
    notifier: createInboxNotifier(path),
    close() {
      for (const socket of sockets) socket.destroy()
      server.close()
    },
  }
}

test("bridge mode answers done once the inbox says Claude Code took the message, and nak on an inbox error", async () => {
  const directory = scratchState()
  const inbox = await heldInbox()
  const nats = new FakeNats()
  const { client } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: inbox.notifier,
      capabilities: () => INBOX_CAPABILITIES,
      client,
    }),
  )
  try {
    const taken = peerMessage("taken")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 1, raw: taken.raw, reply: "_INBOX.taken" })
    await waitFor(async () => inbox.lines.length === 1, "the inbox line")
    await settled()
    expect(nats.repliesOn("_INBOX.taken")).toEqual(["received"])
    inbox.lines[0]?.answer(true)
    await waitFor(async () => nats.repliesOn("_INBOX.taken").length === 2, "done")
    expect(nats.repliesOn("_INBOX.taken")).toEqual(["received", "done"])

    const refused = peerMessage("refused")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 2, raw: refused.raw, reply: "_INBOX.refused" })
    await waitFor(async () => inbox.lines.length === 2, "the second inbox line")
    inbox.lines[1]?.answer(false)
    await waitFor(async () => nats.repliesOn("_INBOX.refused").length === 2, "nak")
    expect(nats.repliesOn("_INBOX.refused")).toEqual(["received", "nak"])

    // The listener sends it again, and this time the bridge takes it: it is shown, not dropped.
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 2,
      raw: refused.raw,
      reply: "_INBOX.again",
      delivered: 2,
    })
    await waitFor(async () => inbox.lines.length === 3, "the redelivered inbox line")
    expect(inbox.lines[2]?.id).toBe(refused.id)
    inbox.lines[2]?.answer(true)
    await waitFor(async () => nats.repliesOn("_INBOX.again").length === 2, "done again")
    expect(nats.repliesOn("_INBOX.again")).toEqual(["received", "done"])
  } finally {
    await session.shutdown()
    inbox.close()
  }
})

test("a frame written before a restart is answered done unshown after it, and the resume point is registered", async () => {
  const directory = scratchState()
  const message = peerMessage("once")
  const first = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const firstNats = new FakeNats()
  const firstNotifier = heldNotifier()
  const before = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: firstNats,
      notifier: firstNotifier.notifier,
      client: first.client,
    }),
  )
  try {
    firstNats.emitRelay(SESSION, { subject: DIRECT, seq: 5, raw: message.raw, reply: "_INBOX.a" })
    await waitFor(async () => firstNotifier.written.length === 1, "the first write")
    firstNotifier.pending[0]?.resolve()
    await waitFor(async () => firstNats.repliesOn("_INBOX.a").includes("done"), "done")
  } finally {
    await before.shutdown()
  }

  const second = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const secondNats = new FakeNats()
  const secondNotifier = heldNotifier()
  const after = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: secondNats,
      notifier: secondNotifier.notifier,
      client: second.client,
    }),
  )
  try {
    // The resume point persisted with the state file seeds the listener's consumer.
    expect(delivery(second.registrations[0])?.resume).toEqual({ stream_id: STREAM, seq: 5 })
    // The listener lost the `done` and sends the frame again: the ledger knows it was shown.
    secondNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 5,
      raw: message.raw,
      reply: "_INBOX.b",
      delivered: 2,
    })
    await waitFor(async () => secondNats.repliesOn("_INBOX.b").includes("done"), "done")
    expect(secondNats.repliesOn("_INBOX.b")).toEqual(["received", "done"])
    expect(secondNotifier.written).toEqual([])
  } finally {
    for (const write of secondNotifier.pending) write.resolve()
    await after.shutdown()
  }
})

// A refusal is the whole of this session's inbound: nothing else reaches it, so it must say so
// and keep asking rather than run on quietly deaf. The listener refuses by name, and
// `parseSubscribeAnswer` refuses an answer naming no consumer the same way, so there is nothing
// left to fall back to.
for (const refusal of [
  { status: 503, reason: "no_grant" },
  { status: 409, reason: "not_requested" },
  // A consumer filter past the server's max payload, which the listener used to answer 503
  // consumer_error and now answers 413: the session is told the same way.
  { status: 413, reason: "too_large" },
] as const) {
  test(`a registration refused ${refusal.status} ${refusal.reason} leaves the session running with one notice naming it, the next heartbeat asks again, and the accepted registration is told once`, async () => {
    const directory = scratchState()
    const nats = new FakeNats()
    const notifier = heldNotifier()
    let refusing = true
    const { client, registrations } = listener(() => {
      if (!refusing) return { ...noInterest(), delivery: CONSUMER }
      throw new EnvoyApiError({
        method: "POST",
        url: "http://127.0.0.1:4000/v1/interests/subscribe",
        status: refusal.status,
        responseBody: JSON.stringify({
          error: "the listener could not give this session a consumer",
          reason: refusal.reason,
        }),
      })
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
      // The session runs on, and its relay subject stays open, so the consumer a later
      // registration is given has somewhere to land.
      expect([...nats.subscriptions.keys()]).toEqual([CONSUMER.subject])
      await waitFor(async () => notices().length === 1, "the notice")
      expect(notices()[0]?.content).toContain(refusal.reason)
      expect(notices()[0]?.content).toContain("Envoy inbound unavailable")

      // Every heartbeat asks again, and the session is told once, not once per tick.
      const asked = registrations.length
      await waitFor(async () => registrations.length >= asked + 2, "two more registrations")
      expect(notices()).toHaveLength(1)

      // Accepted at last: the session is told once that it receives again, and what the stream
      // kept for the session arrives on its relay.
      refusing = false
      const accepted = registrations.length
      await waitFor(async () => registrations.length > accepted, "the accepted registration")
      // Every write is let through as it comes: the notice, then the one behind it.
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return notices().length === 2
      }, "the notice that the session receives again")
      expect(notices()[1]?.content).toContain("Envoy inbound restored")
      const message = peerMessage("after-refusal")
      nats.emitRelay(SESSION, {
        subject: DIRECT,
        seq: 12,
        raw: message.raw,
        reply: "_INBOX.after-refusal",
      })
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return notifier.written.some((written) => written.eventId === message.id)
      }, "the frame after the refusal")
      // The heartbeats after the accepted one tell nothing more.
      const after = registrations.length
      await waitFor(async () => registrations.length >= after + 2, "two more registrations")
      expect(notices()).toHaveLength(2)

      // The accepted registration ends the outage, and the session is told so once.
      const restored = () =>
        notices().filter((written) => written.content.includes("Envoy inbound restored"))
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return restored().length === 1
      }, "the restored notice")
      const told = () =>
        notices().filter((written) => written.content.includes("Envoy inbound unavailable"))
      expect(told()).toHaveLength(1)

      // A second outage is told again, and its end too.
      refusing = true
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return told().length === 2
      }, "the second outage's notice")
      refusing = false
      await waitFor(async () => {
        for (const write of notifier.pending) write.resolve()
        return restored().length === 2
      }, "the second restored notice")
    } finally {
      for (const write of notifier.pending) write.resolve()
      await session.shutdown()
    }
  })
}

// Row (e) of check l23 (R24 of dispatch://INFRA-3589), for this server: the listener answers its
// bus-outage 503 to a registration it may not have recorded, and the session's consumer may still
// deliver. Such refusals are held for the bound. Past it, stderr says that the registration is
// not recorded for certain, and the accepted registration after it says that the listener
// recorded one. No line says that the session receives nothing or receives again. The first
// relay open row below shows both words where the outage left the session nothing.
test("(e) past the bound a bus-outage 503 is written to stderr as a registration not recorded for certain, and the accepted registration after it as one the listener recorded", async () => {
  const unrecorded = `${BUS_UNANSWERED_PREFIX}: write the known-session record of ${SESSION}: context deadline exceeded`
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let refusing = true
  const { client, registrations } = listener(() => {
    if (!refusing) return { ...noInterest(), delivery: CONSUMER }
    throw new EnvoyApiError({
      method: "POST",
      url: "http://127.0.0.1:4000/v1/interests/subscribe",
      status: 503,
      responseBody: JSON.stringify({ error: unrecorded }),
    })
  })
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
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
  const notices = () => notifier.written.filter((write) => write.eventId === undefined)
  try {
    // Inside the bound every refusal is held: nothing is said.
    const asked = registrations.length
    await waitFor(async () => registrations.length >= asked + 2, "two more registrations")
    expect(notices()).toEqual([])

    setSystemTime(new Date(Date.now() + INBOUND_FIRST_OPEN_BOUND_MS + 1_000))
    await waitFor(async () => notices().length === 1, "the notice")
    expect(notices()[0]?.content).toMatch(/not record/i)
    expect(written).toContain(
      `envoy-channel: this session's registration is not recorded for certain (${unrecorded}), and the session registers again at every heartbeat\n`,
    )

    refusing = false
    await waitFor(async () => {
      for (const write of notifier.pending) write.resolve()
      return notices().length === 2
    }, "the notice that the listener recorded a registration")
    expect(notices()[1]?.content).toContain("Envoy inbound restored")
    expect(notices()[1]?.content).toContain("The listener recorded a registration of this session")
    expect(notices()[1]?.content).not.toMatch(RECEIVED_NOTHING)
    expect(written).toContain(
      "envoy-channel: the listener recorded a registration of this session\n",
    )
    expect(written.filter((line) => /receives (nothing|again)/.test(line))).toEqual([])
  } finally {
    setSystemTime()
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
    stderr.mockRestore()
  }
})

// R24 of dispatch://INFRA-3589, after main's #4810, for this server: an edge listener that could
// not read the repositories of the session's projects refuses the registration 503
// `repositories_unknown`, and it withholds the GitHub messages. Where it already held the
// session's registration, it kept that registration and the consumer; where it held none, the
// session receives nothing. The client cannot tell which, so the notice and stderr say both, at
// once, and no line says outright that the session receives nothing. The accepted registration
// after it says that the listener follows the GitHub topics again, and no line says that the
// session receives again.
test("a 503 repositories_unknown is told and written to stderr as GitHub topics withheld, each with what the session receives where the listener held its registration and where it held none, and the accepted registration after it as GitHub topics followed again", async () => {
  const error =
    "the repositories of PRIV's issues could not be read, so no GitHub topic is followed now; register again shortly"
  const why = `${error} (reason: repositories_unknown)`
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let refusing = true
  const { client } = listener(() => {
    if (!refusing) return { ...noInterest(), delivery: CONSUMER }
    throw new EnvoyApiError({
      method: "POST",
      url: "http://127.0.0.1:4000/v1/interests/subscribe",
      status: 503,
      responseBody: JSON.stringify({ error, reason: "repositories_unknown" }),
    })
  })
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
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
  const notices = () => notifier.written.filter((write) => write.eventId === undefined)
  try {
    await waitFor(async () => notices().length === 1, "the notice")
    expect(notices()[0]?.content).toContain("Envoy GitHub topics withheld")
    expect(notices()[0]?.content).toContain(why)
    expect(notices()[0]?.content).toContain(
      "If the listener already held a registration of this session, it kept that registration and the session's consumer, so the session receives the messages of every other topic the listener follows for it",
    )
    expect(notices()[0]?.content).toContain(
      "If the listener held no registration of this session, the session receives nothing until a registration is accepted.",
    )
    expect(notices()[0]?.content).not.toContain("Envoy inbound unavailable")
    expect(written).toContain(
      `envoy-channel: the listener follows none of this session's GitHub topics now (${why}); the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none; registering again every heartbeat\n`,
    )

    refusing = false
    await waitFor(async () => {
      for (const write of notifier.pending) write.resolve()
      return notices().length === 2
    }, "the notice that the listener follows the GitHub topics again")
    expect(notices()[1]?.content).toContain("Envoy GitHub topics followed again")
    expect(notices()[1]?.content).not.toContain("Envoy inbound restored")
    expect(written).toContain(
      "envoy-channel: the listener follows this session's GitHub topics again: a registration was accepted\n",
    )
    expect(written.filter((line) => /receives (nothing|again)/.test(line))).toEqual([])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
    stderr.mockRestore()
  }
})

// R24 of dispatch://INFRA-3589, for this server: a session whose first relay open fails at its
// start holds no relay subject, and it receives once a later try opens the relay. That start race
// is held for the bound (`INBOUND_FIRST_OPEN_BOUND_MS`): inside it the session is told nothing.
// Past the bound it is told once that it receives nothing, naming why, and the accepted
// registration on the connection that opens the relay tells it once that it receives again.
test("a first relay open that fails at the start is told only past the bound, and the registration that opens the relay tells the session it receives again", async () => {
  const directory = scratchState()
  const first = new FakeNats()
  const second = new FakeNats()
  const notifier = new FakeNotifier()
  // The hub closes the start's connection for good before the start's flush reaches it, so the
  // relay subject cannot be opened until a new connection is dialed.
  await first.closeForGood(
    Object.assign(new Error("'Authorization Violation'"), {
      name: "NatsError",
      code: "AUTHORIZATION_VIOLATION",
    }),
  )
  const reopened = Promise.withResolvers<void>()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  // Each registration reads the title first, so this counts the registrations the session tried,
  // those refused before they reached the listener included.
  let tried = 0
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: first,
      notifier,
      client,
      heartbeatMs: 10,
      title: () => {
        tried += 1
        return ""
      },
      reopen: async () => {
        await reopened.promise
        return { connection: second, followSession: async () => undefined }
      },
    }),
  )
  /** The notices this server wrote for itself under `heading`: the one write that names no event. */
  const notices = (heading: string) =>
    notifier.notifications
      .filter((written) => written.eventId === undefined && written.content.includes(heading))
      .map((written) => written.content)
  try {
    // Inside the bound every registration is refused before it reaches the listener, for want of
    // a relay subject, and nothing is said.
    await waitFor(async () => tried >= 5, "five registrations with no relay subject")
    expect(written).toContainEqual(
      expect.stringContaining(
        "envoy-channel: registry heartbeat failed (this session holds no relay subject",
      ),
    )
    expect(registrations).toEqual([])
    expect(notifier.notifications.filter((written) => written.eventId === undefined)).toEqual([])

    setSystemTime(new Date(Date.now() + INBOUND_FIRST_OPEN_BOUND_MS + 1_000))
    await waitFor(
      async () => notices("Envoy inbound unavailable").length === 1,
      "the outage notice past the bound",
    )
    expect(notices("Envoy inbound unavailable")[0]).toContain(
      "because its relay subject could not be opened: Authorization Violation",
    )
    expect(
      written.filter((line) => line.startsWith("envoy-channel: this session receives nothing (")),
    ).toHaveLength(1)

    // A new connection opens the relay, and its registration is accepted: the session is told
    // once that it receives again.
    reopened.resolve()
    await waitFor(
      async () => notices("Envoy inbound restored").length === 1,
      "the notice that the session receives again",
    )
    expect(notices("Envoy inbound restored")[0]).toContain(
      "so the session receives Envoy messages again",
    )
    // The control of row (e)'s absence: this outage left the session nothing, so its restored
    // notice says that the session received nothing.
    expect(notices("Envoy inbound restored")[0]).toMatch(RECEIVED_NOTHING)
    expect(second.liveSubscriptions(deliverSubject(SESSION))).toBe(1)
    expect(delivery(registrations.at(-1))).toMatchObject({ consumer: true })
    expect(written).toContain(
      "envoy-channel: this session receives again: a registration was accepted\n",
    )
    expect(notices("Envoy inbound unavailable")).toHaveLength(1)
  } finally {
    setSystemTime()
    reopened.resolve()
    await session.shutdown()
    stderr.mockRestore()
  }
})

/** Dispatch's BTW to this session: a side question, which this session never advertises. */
function btwFrame(label: string): { readonly raw: string; readonly id: string } {
  const id = `dispatch-btw-${label}`
  return {
    id,
    raw: JSON.stringify({
      event_id: id,
      dedupe_key: id,
      source: "dispatch",
      source_event_id: "1",
      topic: DIRECT,
      issued_at: 1,
      payload_summary: `BTW ${label}`,
      payload: JSON.stringify({
        event: {
          actor: { id: "alice", kind: "user" },
          issue_key: "CORE-1",
          payload: {
            author: { id: "alice", kind: "user" },
            body: `BTW ${label}`,
            created_at: "2026-09-12T00:00:00Z",
            deliveries: [],
            id: "11111111-1111-4111-8111-111111111111",
            in_reply_to: null,
            issue_key: "CORE-1",
            target: `session:${SESSION}`,
          },
          type: "message.created",
        },
        delivery: { attempt: 1, mode: "btw" },
      }),
    }),
  }
}

test("a late BTW is passed over and answered done, and the frame after it is shown", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  const { client } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
    }),
  )
  try {
    const btw = btwFrame("late")
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 3,
      raw: btw.raw,
      reply: "_INBOX.btw",
      delivered: 2,
    })
    const after = peerMessage("after-btw")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 4, raw: after.raw, reply: "_INBOX.after" })
    await waitFor(async () => notifier.written.length === 1, "the frame after the BTW")
    expect(notifier.written[0]?.eventId).toBe(after.id)
    expect(nats.repliesOn("_INBOX.btw")).toEqual(["received", "done"])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})

// The acceptance line of dispatch://INFRA-771: no Envoy session receives from a plain NATS
// subscription. The whole subscription set is asserted, never the presence of the relay subject,
// so a core subscription added back on any followed subject fails here. Oh My Pi's plugin holds
// one more, its agent-stream control subject, and so does this server once a mod links to it:
// the stream it starts then subscribes to `agentstream.<id>.control` (`startAgentStream`,
// `../src/agent-stream-runner`). This harness starts the session alone, so the relay subject is
// the whole set.
test("holds exactly its relay subject after registration, and no other subscription", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
      heartbeatMs: 10,
    }),
  )
  try {
    await session.follow(["notifications.team.deploy", "notifications.dispatch.issue.DSP-7.>"])
    // Heartbeats ran meanwhile, each registering what the session follows.
    const ticks = registrations.length
    await waitFor(async () => registrations.length >= ticks + 2, "two more heartbeats")
    await settled()

    expect(registrations.at(-1)?.topics).toEqual(
      expect.arrayContaining(["notifications.team.deploy", "notifications.dispatch.issue.DSP-7.>"]),
    )
    expect([...nats.subscriptions.keys()]).toEqual([CONSUMER.subject])
    expect(nats.unsubscribed).toEqual([])
    // Nothing was published either: a session answers no receipt, and the only bodies on the bus
    // are a relay frame's own replies, of which there have been none.
    expect(nats.published).toEqual([])
    expect(notifier.written).toEqual([])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})

// The listener's connection check (INFRA-626) is a core request on the session's own relay
// subject that carries `Envoy-Probe: 1` and nothing else: an answer, or a second of silence,
// reads connected, and the no-responders a server gives a subject nothing subscribes reads
// stopped. The probe is no frame, so it reaches no conversation and moves no resume point, and
// the session holds no subscription for it beyond the relay subject the set above pins.
test("a connection probe on the relay subject is answered, follows a session-id change, and meets no subscriber once the session closes its connection", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  const identity = new SessionIdentity(SESSION, "/tmp")
  const next = "ses_claude_next"
  const handoff = sessionHandoffFile(directory, 778)
  await writeSessionHandoff(handoff, SESSION)
  const { client, registrations } = listener((input) => noInterest(input.sessionID))
  const session = await startChannelSession(
    sessionOptions(identity, directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
      heartbeatMs: 25,
      handoffPid: 778,
    }),
  )
  try {
    expect(nats.emitProbe(SESSION, "_INBOX.probe.1")).toBe(true)
    await waitFor(async () => nats.repliesOn("_INBOX.probe.1").length > 0, "the probe's answer")
    expect(nats.repliesOn("_INBOX.probe.1")).toEqual(["done"])
    // A registration after the probe still resumes from nothing: the probe moved no point.
    const before = registrations.length
    await waitFor(async () => registrations.length > before, "a heartbeat after the probe")
    expect(delivery(registrations.at(-1))?.resume).toBeUndefined()

    // A `/clear` hands the session a new id, and the relay subject moves to it: a probe of the
    // id left behind meets no subscriber, and the new id is answered.
    await writeSessionHandoff(handoff, next)
    await waitFor(
      async () => identity.id === next && registrations.some((input) => input.sessionID === next),
      "the registration under the new id",
    )
    expect(nats.emitProbe(SESSION, "_INBOX.probe.2")).toBe(false)
    expect(nats.emitProbe(next, "_INBOX.probe.3")).toBe(true)
    await waitFor(async () => nats.repliesOn("_INBOX.probe.3").length > 0, "the new id's answer")
    expect(nats.repliesOn("_INBOX.probe.3")).toEqual(["done"])

    // The session shuts down: its relay subject closes before its connection does.
    await session.shutdown()
    expect(nats.emitProbe(next, "_INBOX.probe.4")).toBe(false)
    expect(notifier.written).toEqual([])
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})

test("a reconnect begins a new epoch and registers it at once", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const { client, registrations } = listener(() => ({ ...noInterest(), delivery: CONSUMER }))
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      client,
    }),
  )
  try {
    const firstEpoch = delivery(registrations[0])?.epoch
    expect(firstEpoch).toBeString()
    const before = registrations.length
    nats.emitStatus("reconnect")
    await waitFor(async () => registrations.length > before, "a registration after the reconnect")
    const renewed = delivery(registrations.at(-1))?.epoch
    expect(renewed).toBeString()
    expect(renewed).not.toBe(firstEpoch)
  } finally {
    await session.shutdown()
  }
})

test("a gap the registration reports is told once, through the notice path", async () => {
  const directory = scratchState()
  const nats = new FakeNats()
  const notifier = heldNotifier()
  let gap = true
  const { client } = listener(() => {
    const answer = { ...noInterest(), delivery: { ...CONSUMER, gap } }
    gap = false
    return answer
  })
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: notifier.notifier,
      client,
    }),
  )
  try {
    // The first frame after the restart carries the same gap.
    const message = peerMessage("after-gap")
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 40,
      raw: message.raw,
      reply: "_INBOX.gap",
      gap: true,
    })
    // Every write is let through as it comes: the notice, then the frame behind it.
    await waitFor(async () => {
      for (const write of notifier.pending) write.resolve()
      return nats.repliesOn("_INBOX.gap").includes("done")
    }, "done")
    expect(notifier.written.map((written) => written.eventId)).toEqual([undefined, message.id])
    // A notice of Envoy's own names no envelope; every other line written is a frame's.
    const notices = notifier.written.filter((written) => written.eventId === undefined)
    expect(notices).toHaveLength(1)
    expect(notices[0]?.content).toContain("72 hours")
  } finally {
    for (const write of notifier.pending) write.resolve()
    await session.shutdown()
  }
})
