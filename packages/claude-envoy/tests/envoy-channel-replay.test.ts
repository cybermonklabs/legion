/**
 * What a channel session keeps of its deliveries, end to end over its relay: the record in the
 * session id's state file (`src/replay-state.ts`), the topics it follows again at start, the
 * registry entry a stored removal closes, and the notice naming a frame it never took. The
 * ledger, the notice's wording and the relay's own rules are
 * `@legion/envoy-client/delivery-ledger` and `/consumer-delivery`, tested there; here they are
 * wired to this server.
 */
import { expect, mock, spyOn, test } from "bun:test"
import { mkdir, open, readdir, readFile, stat } from "node:fs/promises"
import { agentSubject } from "@legion/contracts"
import type {
  DeliveryLedgerHost,
  DeliveryLedgerOptions,
  FailureNoticeOutcome,
} from "@legion/envoy-client/delivery-ledger"
import type { Interest, SubscribeResult } from "@legion/envoy-client/transport"
import { type ChannelNotification, startChannelSession } from "../src/envoy-channel-server"
import { type ReplayStateFile, replayStateFile, writeReplayState } from "../src/replay-state"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
  type FakeClient,
  FakeNats,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

// The delivery ledger, which calls the host with the session the frame a failure notice names
// was relayed to (`DeliveryLedgerHost.showFailureNotice`). Everything passes straight through;
// `beforeFailureNotice` is the one seam: the ledger binds the give-up to that session before it
// calls, so the session the notice names and the id this process serves can part company only
// inside the call, and the row that drives that changes the id from this hook. The module is
// read here, and not imported at the top, because the mock replaces it: this is the real one it
// passes through to.
const realDeliveryLedger = await import("@legion/envoy-client/delivery-ledger")
// The function itself, taken now: the namespace above is live, so once the mock below replaces
// the module, `realDeliveryLedger.createDeliveryLedger` names the mock, and a mock that called
// through it would call itself without end (measured on pi-envoy's copy of this seam: one row of
// that file grew past 2.5 GB in 9 s, against 422 MB without the seam).
const realCreateDeliveryLedger = realDeliveryLedger.createDeliveryLedger
const ledgerSeam: {
  beforeFailureNotice?: ((addressed: string) => Promise<void>) | undefined
  /** Every call of the host's own show, settled once that show has: a row reads what the show
   *  did only after it is over, since the card it writes is queued behind the channel's writes. */
  readonly shows: Promise<unknown>[]
} = { shows: [] }
mock.module("@legion/envoy-client/delivery-ledger", () => ({
  ...realDeliveryLedger,
  createDeliveryLedger: (options: DeliveryLedgerOptions) => {
    const host: DeliveryLedgerHost = {
      ...options.host,
      showFailureNotice: async (addressed, notice) => {
        await ledgerSeam.beforeFailureNotice?.(addressed)
        const shown: Promise<FailureNoticeOutcome | undefined> = (async () =>
          (await options.host.showFailureNotice(addressed, notice)) ?? undefined)()
        ledgerSeam.shows.push(shown.catch(() => undefined))
        const outcome = await shown
        return outcome === "queued" ? "queued" : "shown"
      },
    }
    return realCreateDeliveryLedger({ ...options, host })
  },
}))

const scratchState = useScratchState()

const SESSION = "ses_replay"
const DIRECT = agentSubject(SESSION)
const ISSUE = "DSP-7"
const ISSUE_TOPIC = `notifications.dispatch.issue.${ISSUE}.>`
const ISSUE_BASE = `notifications.dispatch.issue.${ISSUE}`

/** What the session was shown: the event a card names, and its text. */
interface Shown {
  readonly eventId: string | undefined
  readonly content: string
}

/** The session's conversation, as the notifier writes a card into it; `refuse` makes a write
 *  fail, as a session that cannot take the card does. */
function channelSink(refuse: (shown: Shown) => boolean = () => false) {
  const shown: Shown[] = []
  return {
    shown,
    notifier: {
      notification: async (notification: ChannelNotification) => {
        const entry = { eventId: notification.eventId, content: notification.content }
        if (refuse(entry)) throw new Error("the session refused the write")
        shown.push(entry)
      },
    },
  }
}

const NOTICE = "message not shown"
const cards = (shown: readonly Shown[], eventId: string) =>
  shown.filter((entry) => entry.eventId === eventId && !entry.content.includes(NOTICE))
const notices = (shown: readonly Shown[], eventId: string) =>
  shown.filter((entry) => entry.content.includes(NOTICE) && entry.content.includes(eventId))

/** A peer session's message to `sessionID`. */
function peerMessage(sessionID: string, label: string): { raw: string; eventId: string } {
  const eventId = `evt-${label}`
  return {
    eventId,
    raw: JSON.stringify({
      event_id: eventId,
      source: "agent",
      source_session: "ses_peer",
      topic: agentSubject(sessionID),
      dedupe_key: `agent.ses_peer.${label}`,
      issued_at: 1,
      payload_summary: `peer message ${label}`,
    }),
  }
}

/** A person's removal of `sessionID`'s subscription to `topic`, as Dispatch publishes it. */
function removalFrame(sessionID: string, topic: string): string {
  return JSON.stringify({
    event_id: "dispatch-remove-1",
    source: "dispatch",
    source_event_id: "remove-1",
    topic: agentSubject(sessionID),
    dedupe_key: "dispatch-remove-1",
    issued_at: 1,
    payload_summary: `${ISSUE} subscription removed`,
    payload: JSON.stringify({
      issue_key: ISSUE,
      type: "subscription.removed",
      actor: { kind: "user", id: "alice" },
      payload: { session_id: sessionID, by: { kind: "user", id: "alice" }, topics: [topic] },
    }),
  })
}

/**
 * The listener's interest registry, as a session's registrations reach it: a registration merges
 * its topics into the entry and never removes one, and every one it accepts is answered with the
 * session's own consumer.
 */
function registryClient(): {
  readonly client: FakeClient
  topicsOf(sessionID: string): string[]
} {
  const entries = new Map<string, Set<string>>()
  const entry = (sessionID: string): Set<string> => {
    const held = entries.get(sessionID) ?? new Set<string>()
    entries.set(sessionID, held)
    return held
  }
  const client = recordingClient([], {
    subscribe: async (input): Promise<SubscribeResult> => {
      for (const topic of input.topics) entry(input.sessionID).add(topic)
      return { ...noInterest(input.sessionID), topics: [...entry(input.sessionID)] }
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) entry(input.sessionID).delete(topic)
    },
    getInterest: async (sessionID): Promise<Interest> => ({
      session_id: sessionID,
      machine_id: "devbox",
      dir: "/tmp",
      topics: [...entry(sessionID)],
    }),
  })
  return { client, topicsOf: (sessionID) => [...entry(sessionID)] }
}

async function stateOf(directory: string, sessionID: string): Promise<ReplayStateFile> {
  return JSON.parse(
    await readFile(replayStateFile(directory, sessionID), "utf8"),
  ) as ReplayStateFile
}

test("a frame the channel never takes is named in one notice on its third delivery, and never shown after a restart", async () => {
  const directory = scratchState()
  const frame = peerMessage(SESSION, "refused")
  let attempts = 0
  // Every card naming the frame fails; the notice naming it carries no event id of its own.
  const sink = channelSink((shown) => {
    if (shown.eventId !== frame.eventId) return false
    attempts += 1
    return true
  })
  const nats = new FakeNats()
  const first = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registryClient().client,
    }),
  )
  try {
    // The listener delivers the frame again after each `nak`, counting its deliveries.
    for (const delivered of [1, 2]) {
      const reply = `_INBOX.try-${delivered}`
      nats.emitRelay(SESSION, { subject: DIRECT, seq: 9, raw: frame.raw, reply, delivered })
      await waitFor(async () => nats.repliesOn(reply).includes("nak"), `the ${delivered}. nak`)
      expect(notices(sink.shown, frame.eventId)).toHaveLength(0)
    }
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 9,
      raw: frame.raw,
      reply: "_INBOX.try-3",
      delivered: 3,
    })
    await waitFor(
      async () => notices(sink.shown, frame.eventId).length === 1,
      "the failure notice",
      () => `attempts ${attempts}`,
    )
    // Given up on: the listener is told so, rather than sending it forever. The answer follows
    // the notice rather than racing it: the frame is settled on whether the notice reached
    // anybody (`failed`, packages/envoy-client/src/consumer-delivery.ts), so `done` is sent in
    // the turn after the notice was shown, and the wait above ends on the notice itself.
    await waitFor(
      async () => nats.repliesOn("_INBOX.try-3").includes("done"),
      "the answer the notice settles",
    )
    expect(nats.repliesOn("_INBOX.try-3")).toEqual(["received", "done"])
    expect(attempts).toBe(3)
    expect(notices(sink.shown, frame.eventId)[0]?.content).toContain("envoy_inbox")
    expect(cards(sink.shown, frame.eventId)).toHaveLength(0)
  } finally {
    await first.shutdown()
  }

  const laterNats = new FakeNats()
  const second = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: laterNats,
      notifier: sink.notifier,
      client: registryClient().client,
    }),
  )
  try {
    // The notice recorded the frame, so the relay's next copy of it is passed over in silence.
    laterNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 9,
      raw: frame.raw,
      reply: "_INBOX.after",
      delivered: 4,
    })
    await waitFor(async () => laterNats.repliesOn("_INBOX.after").includes("done"), "done")
    expect(notices(sink.shown, frame.eventId)).toHaveLength(1)
    expect(cards(sink.shown, frame.eventId)).toHaveLength(0)
  } finally {
    await second.shutdown()
  }
})

test("a session the listener reaped follows its remembered topics again and registers them", async () => {
  const directory = scratchState()
  // The state a stopped session left: it followed the issue topic, and the listener has since
  // reaped its interests, so the registry holds nothing for it.
  await writeReplayState(replayStateFile(directory, SESSION), {
    ledger: [],
    state: { topics: [DIRECT, ISSUE_BASE, ISSUE_TOPIC] },
  })
  const registry = registryClient()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registry.client,
    }),
  )
  try {
    expect(session.topics()).toEqual(expect.arrayContaining([DIRECT, ISSUE_BASE, ISSUE_TOPIC]))
    expect(registry.topicsOf(SESSION)).toEqual(
      expect.arrayContaining([DIRECT, ISSUE_BASE, ISSUE_TOPIC]),
    )
  } finally {
    await session.shutdown()
  }
})

test("a removal the relay delivers after a restart re-registered the topic takes it out of the registry, so a later restart does not follow it", async () => {
  const directory = scratchState()
  const registry = registryClient()
  const start = async (connection: FakeNats) =>
    startChannelSession(
      sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
        connection,
        client: registry.client,
      }),
    )

  const firstNats = new FakeNats()
  const first = await start(firstNats)
  await first.follow([ISSUE_TOPIC])
  await first.shutdown()
  expect(registry.topicsOf(SESSION)).toContain(ISSUE_TOPIC)

  // The restart follows the topic again from its state and registers it; the removal a person
  // made while the session was stopped is the first frame its consumer relays.
  const secondNats = new FakeNats()
  const second = await start(secondNats)
  try {
    expect(second.topics()).toContain(ISSUE_TOPIC)
    secondNats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 11,
      raw: removalFrame(SESSION, ISSUE_TOPIC),
      reply: "_INBOX.removal",
    })
    await waitFor(
      async () =>
        !second.topics().includes(ISSUE_TOPIC) && !registry.topicsOf(SESSION).includes(ISSUE_TOPIC),
      "the relayed removal",
    )
  } finally {
    await second.shutdown()
  }

  const third = await start(new FakeNats())
  try {
    expect(third.topics()).not.toContain(ISSUE_TOPIC)
    expect(registry.topicsOf(SESSION)).not.toContain(ISSUE_TOPIC)
  } finally {
    await third.shutdown()
  }
})

test("a followed topic reaching a private stream subject is followed and never registered", async () => {
  const directory = scratchState()
  const registry = registryClient()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registry.client,
    }),
  )
  const refused = [
    "notifications.*.x",
    "notifications.agent.*",
    agentSubject("ses_other"),
    `notifications.envoy.exceptions.${DIRECT}`,
    "notifications.legion.ACME.controller",
  ]
  try {
    await session.follow([...refused, ISSUE_TOPIC])

    expect(session.topics()).toEqual(expect.arrayContaining([...refused, ISSUE_TOPIC]))
    const registered = registry.topicsOf(SESSION)
    expect(registered).toEqual(expect.arrayContaining([DIRECT, ISSUE_TOPIC]))
    for (const topic of refused) expect(registered).not.toContain(topic)
  } finally {
    await session.shutdown()
  }
})

test("the delivery state file is private to its user and replaced whole by each write", async () => {
  const directory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: new FakeNats(),
      client: registryClient().client,
    }),
  )
  const file = replayStateFile(directory, SESSION)
  try {
    // Nothing is written until the session has something to record: the first follow stamps it.
    await session.follow([ISSUE_TOPIC])
    await waitFor(async () => (await stat(file).catch(() => undefined)) !== undefined, "the file")
    expect((await stat(file)).mode & 0o777).toBe(0o600)
    const firstText = await readFile(file, "utf8")
    // The file as the first write left it, held open across the next write.
    const held = await open(file, "r")
    try {
      await session.follow(["notifications.team.deploy"])
      await waitFor(async () => (await readFile(file, "utf8")) !== firstText, "the next write")
      expect((await stat(file)).mode & 0o777).toBe(0o600)
      // A rename put a new file in its place; nothing was written into the old one.
      expect(await held.readFile("utf8")).toBe(firstText)
    } finally {
      await held.close()
    }
    expect((await readdir(directory)).filter((name) => name.endsWith(".tmp"))).toEqual([])
    const state = await stateOf(directory, SESSION)
    expect(state.state?.topics).toEqual([
      DIRECT,
      ISSUE_BASE,
      ISSUE_TOPIC,
      "notifications.team.deploy",
    ])
    expect(Object.keys(state.consumer?.follows ?? {})).toEqual([
      ISSUE_BASE,
      ISSUE_TOPIC,
      "notifications.team.deploy",
    ])
  } finally {
    await session.shutdown()
  }
})

test("a state file that cannot be written is logged, and what the session was shown stays shown", async () => {
  const directory = scratchState()
  // A directory where the file goes: every rename onto it fails.
  await mkdir(replayStateFile(directory, SESSION))
  const sink = channelSink()
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  const nats = new FakeNats()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registryClient().client,
    }),
  )
  try {
    const frame = peerMessage(SESSION, "unpersisted")
    nats.emitRelay(SESSION, { subject: DIRECT, seq: 4, raw: frame.raw, reply: "_INBOX.first" })
    await waitFor(async () => cards(sink.shown, frame.eventId).length === 1, "the frame")
    await waitFor(
      async () => written.some((line) => line.includes("could not persist")),
      "the failed write",
    )
    // The in-memory ledger still holds it, so the relay's next copy is not shown again.
    nats.emitRelay(SESSION, {
      subject: DIRECT,
      seq: 4,
      raw: frame.raw,
      reply: "_INBOX.again",
      delivered: 2,
    })
    await waitFor(async () => nats.repliesOn("_INBOX.again").includes("done"), "done")
    expect(cards(sink.shown, frame.eventId)).toHaveLength(1)
  } finally {
    await session.shutdown()
    stderr.mockRestore()
  }
})

test("a /clear handoff to a new session id starts a fresh delivery state in a file of its own", async () => {
  const directory = scratchState()
  const registry = registryClient()
  const handoff = sessionHandoffFile(directory, 931)
  await writeSessionHandoff(handoff, "ses_old")
  const sink = channelSink()
  const nats = new FakeNats()
  const identity = new SessionIdentity("ses_old", "/tmp")
  const session = await startChannelSession(
    sessionOptions(identity, directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registry.client,
      heartbeatMs: 20,
      handoffPid: 931,
    }),
  )
  try {
    await session.follow([ISSUE_TOPIC])
    const before = peerMessage("ses_old", "before")
    nats.emitRelay("ses_old", {
      subject: agentSubject("ses_old"),
      seq: 1,
      raw: before.raw,
      reply: "_INBOX.old",
    })
    await waitFor(async () => cards(sink.shown, before.eventId).length === 1, "the old id's frame")
    await waitFor(
      async () => (await stateOf(directory, "ses_old")).ledger.length === 1,
      "the old id's record",
    )

    await writeSessionHandoff(handoff, "ses_new")
    await waitFor(
      async () => nats.subscriptions.has("notifications.envoy.deliver.ses_new"),
      "the new id's relay subscription",
    )

    // The new id's relay subject is the only one this process holds; the old id's is gone.
    expect([...nats.subscriptions.keys()]).toEqual(["notifications.envoy.deliver.ses_new"])
    await waitFor(
      async () =>
        (await stat(replayStateFile(directory, "ses_new")).catch(() => undefined)) !== undefined,
      "the new id's state file",
    )
    // A new id starts afresh, in a file of its own: nothing the id before it was shown comes
    // back, and the id before it keeps its own record whole.
    expect((await stateOf(directory, "ses_new")).ledger).toEqual([])
    const old = await stateOf(directory, "ses_old")
    expect(old.ledger.map((entry) => entry.event_id)).toEqual([before.eventId])
    expect(old.state?.topics).toEqual(
      expect.arrayContaining([agentSubject("ses_old"), ISSUE_TOPIC]),
    )

    const taken = peerMessage("ses_new", "taken")
    nats.emitRelay("ses_new", {
      subject: agentSubject("ses_new"),
      seq: 2,
      raw: taken.raw,
      reply: "_INBOX.new",
    })
    await waitFor(async () => cards(sink.shown, taken.eventId).length === 1, "the new id's frame")
  } finally {
    await session.shutdown()
  }
})

// The notice naming a frame the session was never shown is that session's: the ledger hands the
// host the session the frame was relayed to (`DeliveryLedgerHost.showFailureNotice`), and this
// process cards it only while that is the id it serves. A `/clear` moves the id while the notice
// is in flight - the one window where the session the notice names and the conversation open
// here part company, the ledger having bound the give-up to that session before it called - and
// the new conversation is shown nothing of the old id's frame; the frame goes back to the stream
// for whoever takes ses_old next (dispatch://INFRA-771).
test("shows the id a /clear moved to nothing of a frame whose notice was in flight", async () => {
  const directory = scratchState()
  const handoff = sessionHandoffFile(directory, 932)
  await writeSessionHandoff(handoff, "ses_old")
  const registry = registryClient()
  // Every card of a frame fails, so each of its deliveries fails; the notice naming a frame is
  // no card of it, and is taken.
  const sink = channelSink((shown) => !shown.content.includes(NOTICE))
  const nats = new FakeNats()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_old", "/tmp"), directory, {
      connection: nats,
      notifier: sink.notifier,
      client: registry.client,
      heartbeatMs: 20,
      handoffPid: 932,
    }),
  )
  try {
    // One frame's three deliveries: the third is given up on, and what comes back is what the
    // session answered the listener on that delivery's inbox.
    const givenUp = async (sessionID: string, label: string, seq: number): Promise<string[]> => {
      const frame = peerMessage(sessionID, label)
      let reply = ""
      for (const delivered of [1, 2, 3]) {
        reply = `_INBOX.${label}-${delivered}`
        nats.emitRelay(sessionID, {
          subject: agentSubject(sessionID),
          seq,
          raw: frame.raw,
          reply,
          delivered,
        })
        await waitFor(
          async () => nats.repliesOn(reply).length > 1,
          `the answer to delivery ${delivered} of ${label}`,
        )
      }
      return nats.repliesOn(reply)
    }

    // The `/clear` lands while the old id's frame has been given up on and its notice is in
    // flight: the handoff file names the new id, and the heartbeat adopts it.
    ledgerSeam.beforeFailureNotice = async () => {
      ledgerSeam.beforeFailureNotice = undefined
      await writeSessionHandoff(handoff, "ses_new")
      await waitFor(
        async () => nats.subscriptions.has("notifications.envoy.deliver.ses_new"),
        "the new id's relay subscription",
      )
    }
    // The frame's own answer decides nothing here, and the row asserts none: the /clear closes
    // the old id's delivery, which answers `nak` for every frame it still holds whatever the
    // notice does (`close`, packages/envoy-client/src/consumer-delivery.ts). An earlier copy of
    // this row required that `nak` and then read the notices at once, so it passed with the
    // addressing refusal disabled too: the frame was naked by the close, and the notice, queued
    // behind the channel's writes, had usually not landed when the row looked.
    const shows = ledgerSeam.shows.length
    await givenUp("ses_old", "moved-notice", 11)
    // So the row waits for the host's show itself to be over - written, or refused - before it
    // reads what the conversation this process serves now was shown. What decides that, and the
    // only thing that does, is whether the host refuses a notice naming an id it no longer serves.
    await waitFor(async () => ledgerSeam.shows.length > shows, "the host's show of the notice")
    await Promise.all(ledgerSeam.shows)

    // The conversation this process serves now was shown nothing of the old id's frame: no card
    // of it, and no notice naming it.
    const old = peerMessage("ses_old", "moved-notice")
    expect(notices(sink.shown, old.eventId)).toHaveLength(0)
    expect(cards(sink.shown, old.eventId)).toHaveLength(0)

    // The new id's own give-up is carded there, so the rows above read a conversation that is
    // shown notices, not one that is shown nothing at all.
    expect(await givenUp("ses_new", "after-move", 12)).toEqual(["received", "done"])
    const after = peerMessage("ses_new", "after-move")
    expect(notices(sink.shown, after.eventId)).toHaveLength(1)
  } finally {
    ledgerSeam.beforeFailureNotice = undefined
    ledgerSeam.shows.length = 0
    await session.shutdown()
  }
})
