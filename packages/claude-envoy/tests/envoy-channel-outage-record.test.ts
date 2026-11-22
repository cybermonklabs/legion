// What a session was told of its inbound is the session id's own, across a `/clear` as across a
// restart: the outage report reads the record of the id the server binds, a notice is addressed to
// the id current as it is said, and it is recorded in that id's state file only. Each row drives
// `startChannelSession` on `FakeNats`; the listener refuses a registration by its session id.
import { expect, spyOn, test } from "bun:test"
import type { ChannelNotification } from "../src/envoy-channel-server"
import { startChannelSession } from "../src/envoy-channel-server"
import { readReplayState, replayStateFile } from "../src/replay-state"
import { SessionIdentity, sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import {
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

const SESSION = "ses_outage_record"
const NEXT = "ses_outage_record_next"

/** A notifier whose writes of a notice under `failing`'s heading are refused while it is set. */
class RefusingNotifier extends FakeNotifier {
  failing: string | undefined
  /** How many writes it refused. */
  refused = 0

  override notification(notification: ChannelNotification): Promise<void> {
    if (this.failing !== undefined && notification.content.includes(this.failing)) {
      this.refused += 1
      return Promise.reject(new Error("the mod is not linked"))
    }
    return super.notification(notification)
  }
}

/** A notifier that holds each write of a notice under `holding`'s heading until `release()`, and
 *  so every write queued behind it. */
class HoldingNotifier extends FakeNotifier {
  holding: string | undefined
  held = 0
  private readonly gate = Promise.withResolvers<void>()

  override async notification(notification: ChannelNotification): Promise<void> {
    if (this.holding !== undefined && notification.content.includes(this.holding)) {
      this.held += 1
      await this.gate.promise
    }
    return super.notification(notification)
  }

  release(): void {
    this.holding = undefined
    this.gate.resolve()
  }
}

function notices(notifier: FakeNotifier, heading: string): number {
  return notifier.notifications.filter(
    (written) => written.eventId === undefined && written.content.includes(heading),
  ).length
}

/** What `id`'s state file says it was last told of its inbound; undefined for no file. */
async function toldIn(directory: string, id: string): Promise<boolean | undefined> {
  const file = await readReplayState(replayStateFile(directory, id)).catch(() => undefined)
  return file?.inboundOutageTold
}

/** A session on `SESSION` whose registrations the listener refuses while `refusing` names the
 *  registering id, which a `/clear` to `NEXT` moves once `clear()` runs. */
async function startRefusable(notifier: FakeNotifier, refusing: Set<string>) {
  const directory = scratchState()
  const handoff = sessionHandoffFile(directory, 781)
  await writeSessionHandoff(handoff, SESSION)
  const registered: string[] = []
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), directory, {
      notifier,
      heartbeatMs: 25,
      handoffPid: 781,
      client: recordingClient([], {
        subscribe: async (input) => {
          registered.push(input.sessionID)
          if (refusing.has(input.sessionID)) throw new Error("no_grant")
          return noInterest(input.sessionID)
        },
      }),
    }),
  )
  const state = () => JSON.stringify({ registered, notices: notifier.notifications.length })
  const clear = () => writeSessionHandoff(handoff, NEXT)
  /** Waits for `id` to have registered `times` more times from now. */
  const registrations = async (id: string, times: number) => {
    const from = registered.filter((each) => each === id).length
    await waitFor(
      async () => registered.filter((each) => each === id).length >= from + times,
      `${times} more registrations of ${id}`,
      state,
    )
  }
  return { directory, session, state, clear, registrations }
}

test("an outage told under one id is that id's: the id a /clear moves to is told nothing of it, and only the first id's file records it", async () => {
  const notifier = new FakeNotifier()
  const run = await startRefusable(notifier, new Set([SESSION]))
  try {
    await waitFor(
      async () =>
        notices(notifier, "Envoy inbound unavailable") === 1 &&
        (await toldIn(run.directory, SESSION)) === true,
      "the outage told under the first id, and its file recording it",
      run.state,
    )
    await run.clear()
    // NEXT's registrations are accepted. Two heartbeats after its first, a restored notice its
    // first answer owed has been written.
    await run.registrations(NEXT, 3)
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    expect(await toldIn(run.directory, NEXT)).not.toBe(true)
    // The first id's record still says it was told, so a server that resumes it tells its end.
    expect(await toldIn(run.directory, SESSION)).toBe(true)
  } finally {
    await run.session.shutdown()
  }
}, 30_000)

test("the id a /clear moves to during an outage is told its own outage and its end, each in its own file", async () => {
  const notifier = new FakeNotifier()
  const refusing = new Set([SESSION, NEXT])
  const run = await startRefusable(notifier, refusing)
  try {
    await waitFor(
      async () => notices(notifier, "Envoy inbound unavailable") === 1,
      "the outage told under the first id",
      run.state,
    )
    await run.clear()
    await waitFor(
      async () =>
        notices(notifier, "Envoy inbound unavailable") === 2 &&
        (await toldIn(run.directory, NEXT)) === true,
      "the new id's own outage notice, and its file recording it",
      run.state,
    )
    refusing.delete(NEXT)
    await waitFor(
      async () =>
        notices(notifier, "Envoy inbound restored") === 1 &&
        (await toldIn(run.directory, NEXT)) === false,
      "the new id's restored notice, and its file recording it",
      run.state,
    )
    await run.registrations(NEXT, 2)
    expect(notices(notifier, "Envoy inbound unavailable")).toBe(2)
    expect(notices(notifier, "Envoy inbound restored")).toBe(1)
    expect(await toldIn(run.directory, SESSION)).toBe(true)
  } finally {
    await run.session.shutdown()
  }
}, 30_000)

test("an end that could not be shown under one id leaves that id's record told, and nothing of it in the id a /clear moves to", async () => {
  const notifier = new RefusingNotifier()
  const refusing = new Set([SESSION])
  const run = await startRefusable(notifier, refusing)
  try {
    await waitFor(
      async () => (await toldIn(run.directory, SESSION)) === true,
      "the outage told under the first id, recorded in its file",
      run.state,
    )
    notifier.failing = "Envoy inbound restored"
    refusing.delete(SESSION)
    await run.registrations(SESSION, 2)
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    notifier.failing = undefined
    await run.clear()
    await run.registrations(NEXT, 3)
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    expect(await toldIn(run.directory, NEXT)).not.toBe(true)
    expect(await toldIn(run.directory, SESSION)).toBe(true)
  } finally {
    await run.session.shutdown()
  }
}, 30_000)

// The outage notice is refused by the notifier (the mod is not linked) at the first refusal and at
// the next, which tells it again; the listener then accepts a registration, and only after that can
// notices be written again. The session never saw the outage, so it is told no end of it, and its
// file records nothing told; the next outage is told. The log says the outage began and ended once
// each, however many times the session was told it.
test("a session whose outage notice could not be shown is told it again at the next refusal and no end of that outage, the log says its start and end once, and its next outage is told", async () => {
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  const logged = (line: string) => written.filter((each) => each.includes(line)).length
  const notifier = new RefusingNotifier()
  notifier.failing = "Envoy inbound unavailable"
  const refusing = new Set([SESSION])
  const run = await startRefusable(notifier, refusing)
  try {
    await waitFor(
      async () => notifier.refused >= 2,
      "the outage notice refused, and refused again at a later refusal",
      run.state,
    )
    refusing.delete(SESSION)
    // Accepted while notices still fail, so no outage notice queued before can be shown after it.
    await run.registrations(SESSION, 2)
    notifier.failing = undefined
    await run.registrations(SESSION, 2)
    expect(notices(notifier, "Envoy inbound unavailable")).toBe(0)
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    expect(await toldIn(run.directory, SESSION)).not.toBe(true)
    expect(logged("envoy-channel: this session receives nothing (")).toBe(1)
    expect(logged("envoy-channel: this session receives again")).toBe(1)
    refusing.add(SESSION)
    await waitFor(
      async () =>
        notices(notifier, "Envoy inbound unavailable") === 1 &&
        (await toldIn(run.directory, SESSION)) === true,
      "the next outage told, and its file recording it",
      run.state,
    )
    expect(logged("envoy-channel: this session receives nothing (")).toBe(2)
  } finally {
    stderr.mockRestore()
    await run.session.shutdown()
  }
}, 30_000)

test("an end told under one id that is still queued when a /clear lands is not shown to the id it moved to", async () => {
  const notifier = new HoldingNotifier()
  notifier.holding = "Envoy inbound unavailable"
  const refusing = new Set([SESSION])
  const run = await startRefusable(notifier, refusing)
  try {
    // The outage notice is held mid-write when the outage ends: the report waits for that notice
    // before it tells the end, and the `/clear`'s bind (`recordRead`) drops the wait, so the end
    // is never told, to either id.
    await waitFor(async () => notifier.held === 1, "the outage notice held", run.state)
    refusing.delete(SESSION)
    await run.registrations(SESSION, 1)
    await run.clear()
    await run.registrations(NEXT, 1)
    notifier.release()
    await waitFor(
      async () => notices(notifier, "Envoy inbound unavailable") === 1,
      "the held outage notice written",
      run.state,
    )
    await run.registrations(NEXT, 2)
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    expect(await toldIn(run.directory, NEXT)).not.toBe(true)
  } finally {
    await run.session.shutdown()
  }
}, 30_000)

// The outage notice was shown, and the outage ends while a frame's write holds the channel's
// queue: the end's notice is queued behind that write, addressed to the id it was told under. The
// `/clear` is taken up before its turn comes, so the id the server serves is the new one at the
// notice's turn, which refuses it: the new conversation never heard of the outage.
test("an end queued behind a held write when a /clear is taken up is not shown to the id it moved to, and the first id's record keeps the outage told", async () => {
  const HELD = "a peer message held mid-write"
  const notifier = new HoldingNotifier()
  const refusing = new Set([SESSION])
  const run = await startRefusable(notifier, refusing)
  /** A peer's message to `sessionID`, as the relay hands it to the channel's write queue. */
  const frame = (sessionID: string, eventId: string, summary: string) => {
    const subject = `notifications.agent.${sessionID}`
    return {
      subject,
      raw: JSON.stringify({
        event_id: eventId,
        source: "agent",
        source_session: "ses_peer",
        topic: subject,
        dedupe_key: `delivery-${eventId}`,
        issued_at: 1_760_000_000_000,
        payload_summary: summary,
      }),
    }
  }
  try {
    await waitFor(
      async () =>
        notices(notifier, "Envoy inbound unavailable") === 1 &&
        (await toldIn(run.directory, SESSION)) === true,
      "the outage shown under the first id, and its file recording it",
      run.state,
    )
    notifier.holding = HELD
    const writing = run.session.delivery.enqueue(frame(SESSION, "evt-held", HELD))
    await waitFor(async () => notifier.held === 1, "the frame's write held", run.state)
    // The end is told at the accepted registration, and its notice waits behind the held write.
    refusing.delete(SESSION)
    await run.registrations(SESSION, 1)
    await run.clear()
    await run.registrations(NEXT, 1)
    notifier.release()
    await writing
    // A write queued after the end's notice has its turn after it.
    await run.session.delivery.enqueue(frame(NEXT, "evt-after", "a peer message after the clear"))
    expect(notices(notifier, "Envoy inbound restored")).toBe(0)
    expect(await toldIn(run.directory, NEXT)).not.toBe(true)
    expect(await toldIn(run.directory, SESSION)).toBe(true)
  } finally {
    notifier.release()
    await run.session.shutdown()
  }
}, 30_000)
