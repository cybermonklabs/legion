import { describe, expect, test } from "bun:test";
import {
  createDeliveryDedupe,
  type DedupeIdentity,
  type DeliveryClaim,
  type DeliveryDedupe,
} from "@legion/envoy-client/delivery";
import {
  type HeldStoredCard,
  heldStoredCards,
  REPLAY_DELIVERED_ENTRY,
  replaySnapshot,
  STORED_CARD_ENTRY,
  storedCardRecord,
} from "./missed-messages";
import { type CardDelivery, UnwrittenFrames, unwrittenCardKeys } from "./unwritten-frames";

/** A Dispatch frame: its dedupe key names its event, so the ledger knows it by both. */
const dispatchFrame = (n: number): DedupeIdentity => ({
  event_id: `event-${n}`,
  dedupe_key: `dispatch-${n}`,
  source: "dispatch",
});

/** The message the host writes for a card that names `details`. */
const writtenCard = (details: Record<string, string>) => ({
  customType: "envoy-message",
  content: "card",
  details,
});

/** The entry the host writes for a card that names `details`, as the branch holds it. */
const storedEntry = (details: Record<string, string>) => ({
  type: "custom_message",
  ...writtenCard(details),
});

interface Handed {
  readonly content: unknown;
  readonly deliverAs: CardDelivery;
  readonly identity: DedupeIdentity | undefined;
  readonly dispatchMessageId?: string;
}

/**
 * A claim that notes what became of it: a frame the dedupe keeps nothing of (no event id, a key
 * that does not name its event) has no record a real claim's state could be read from.
 */
function notingClaim() {
  const became: ("settled" | "released")[] = [];
  const claim: DeliveryClaim = {
    settle: () => {
      became.push("settled");
    },
    release: () => {
      became.push("released");
    },
  };
  return { claim, became };
}

/** The module beside a fake host, with real claims from a delivery dedupe. */
function harness() {
  const dedupe: DeliveryDedupe = createDeliveryDedupe();
  const state = {
    session: "s1",
    // The host's live id while the host has moved and no session event has told the instance of
    // it: `session`, the id the instance serves, lags behind until that event. Undefined while
    // the two are the same.
    hostSession: undefined as string | undefined,
    settled: true,
    refuseHand: false,
    refuseRecord: false,
    matched: undefined as string | undefined,
    // The entries of the session's branch once the session's store holds it; undefined before
    // that, as before the session's first assistant message.
    stored: undefined as unknown[] | undefined,
  };
  // Each frame the host was asked to record, with the session whose ledger it names. The real
  // host binds the write to that session (`DeliveryLedger.recordDeliveredFor`) and records nothing
  // once it is not the one this process writes its transcript to.
  const recorded: { session: string; identity: DedupeIdentity }[] = [];
  // The cards handed to nobody, each with the session it was for.
  const notShown: { session: string; identity: DedupeIdentity | undefined }[] = [];
  const handed: Handed[] = [];
  const withdrawn: [string, string][] = [];
  const handBackFailures: string[] = [];
  const notices: string[] = [];
  const noticeFailures: string[] = [];
  // The notice records the transcript of the session they were made on got, in order.
  const noticeRecords: { session: string; id: string; content?: string; handed?: true }[] = [];
  const recordFailures: string[] = [];
  // The stored cards recorded in the transcript of the session each names, in order.
  const storedRecords: { session: string; card: HeldStoredCard }[] = [];
  const storedRecordFailures: string[] = [];
  const frames = new UnwrittenFrames({
    sessionID: () => state.session,
    hostIsOn: (id) => id === (state.hostSession ?? state.session),
    settled: () => state.settled,
    storedEntries: () => state.stored,
    recordDelivered: async (session, identity) => {
      recorded.push({ session, identity });
    },
    handCard: (content, deliverAs, identity, dispatchMessageId) => {
      if (state.refuseHand) throw new Error("the host refused the card");
      handed.push({
        content,
        deliverAs,
        identity,
        ...(dispatchMessageId === undefined ? {} : { dispatchMessageId }),
      });
    },
    cardNotShown: (session, identity) => notShown.push({ session, identity }),
    handNotice: (content) => {
      if (state.refuseHand) throw new Error("the host refused the notice");
      notices.push(content);
    },
    matchTurn: () => state.matched,
    turnWithdrawn: (sessionID, messageID) => withdrawn.push([sessionID, messageID]),
    handBackFailed: (identity, error) =>
      handBackFailures.push(`${identity.event_id}: ${(error as Error).message}`),
    handNoticeFailed: (error) => noticeFailures.push((error as Error).message),
    recordNotice: (id, content) => {
      if (state.refuseRecord) throw new Error("the session file refused the record");
      noticeRecords.push({ session: state.session, id, content });
    },
    noticeHanded: (id) => {
      if (state.refuseRecord) throw new Error("the session file refused the record");
      noticeRecords.push({ session: state.session, id, handed: true });
    },
    recordNoticeFailed: (error) => recordFailures.push((error as Error).message),
    recordStored: (session, card) => {
      if (state.refuseRecord) throw new Error("the session file refused the record");
      storedRecords.push({ session, card });
    },
    recordStoredFailed: (identity, error) =>
      storedRecordFailures.push(`${identity.event_id}: ${(error as Error).message}`),
  });
  /** A claim of `identity`, as the live path takes one before it hands a frame on. */
  const claim = (identity: DedupeIdentity) => {
    const taken = dedupe.claim(identity);
    if (taken === undefined) throw new Error("the frame was claimed already");
    return taken;
  };
  /** `settled`: done with, so a copy is a repeat. `released`: claimable again. */
  const claimState = (identity: DedupeIdentity): "pending" | "settled" | "released" => {
    if (dedupe.pending(identity)) return "pending";
    const again = dedupe.claim(identity);
    if (again === undefined) return "settled";
    again.release();
    return "released";
  };
  return {
    frames,
    state,
    recorded,
    notShown,
    handed,
    notices,
    withdrawn,
    handBackFailures,
    noticeFailures,
    noticeRecords,
    recordFailures,
    storedRecords,
    storedRecordFailures,
    claim,
    claimState,
  };
}

describe("a card handed to the host", () => {
  test("waits under its event id and its dedupe key, and is recorded once written under either", async () => {
    const { frames, recorded, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("s1", "card", "aside", frame, claim(frame))).toBe("queued");
    expect(handed).toEqual([{ content: "card", deliverAs: "aside", identity: frame }]);
    expect(frames.waitsOnCard({ dedupe_key: "dispatch-1", source: "dispatch" })).toBe(true);
    expect(claimState(frame)).toBe("pending");

    await frames.written(writtenCard({ dedupe_key: "dispatch-1" }));

    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
    expect(frames.waitsOnCard(frame)).toBe(false);
    // A second write of the same card finds nothing waiting.
    await frames.written(writtenCard({ event_id: "event-1" }));
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
  });

  test("does not wait when the ledger keeps nothing of its frame, and settles its claim at once", () => {
    const { frames, recorded, handed } = harness();
    // No event id, and a key that does not name its event.
    const frame: DedupeIdentity = { dedupe_key: "content-hash", source: "mcp" };
    const { claim, became } = notingClaim();

    expect(unwrittenCardKeys({})).toEqual([]);
    expect(frames.sendCard("s1", "card", "card", frame, claim)).toBe("shown");
    expect(frames.sendCard("s1", "notice", "card", undefined, undefined)).toBe("shown");
    expect(handed).toHaveLength(2);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(became).toEqual(["settled"]);
    expect(recorded).toEqual([]);
  });

  test("leaves nothing waiting and releases its claim when the host refuses it", () => {
    const { frames, state, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    const unkept = notingClaim();
    state.refuseHand = true;

    expect(() => frames.sendCard("s1", "card", "card", frame, claim(frame))).toThrow(
      "the host refused the card"
    );
    expect(() =>
      frames.sendCard("s1", "card", "card", { dedupe_key: "content-hash" }, unkept.claim)
    ).toThrow("the host refused the card");
    expect(frames.waitsOnCard(frame)).toBe(false);
    // Claimable again: a re-send, or a sweep, shows the frame.
    expect(claimState(frame)).toBe("released");
    expect(unkept.became).toEqual(["released"]);
  });

  test("written once the host is on another session is recorded for the session it was shown in", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("s1", "card", "card", frame, claim(frame));

    state.session = "s2";
    await frames.written(writtenCard({ event_id: "event-1" }));

    // The card was shown in s1, so s1 is the session its record names. The write binds to that
    // session (`DeliveryLedger.recordDeliveredFor`) and records nothing once the process writes
    // another session's transcript, which is not read here.
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });

  test("for a session that is not the one open here is handed to nobody, its claim released", () => {
    const { frames, state, handed, notShown, claim, claimState } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("s2", "card", "card", frame, claim(frame))).toBe("closed");

    // The host writes what it is handed in the conversation it has open, so nothing is handed
    // and nothing waits: the claim goes back, and a sweep of s2 shows the frame there.
    expect(handed).toEqual([]);
    expect(notShown).toEqual([{ session: "s2", identity: frame }]);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(claimState(frame)).toBe("released");

    // Nothing is held for s2 either: the settled moment after the host moves hands nothing.
    state.session = "s2";
    frames.handBackWithdrawn();
    expect(handed).toEqual([]);
  });

  // The window `hostIsOn` admits a card in: the host moved to s2 and the switch event has not
  // reached this instance, whose own id still reads s1. A frame that is no Dispatch delivery
  // names no session, so its card goes to the conversation the host has (`cardSession` in
  // `../extensions/envoy.ts`) - s2, the session it was shown in. What this module decides is the
  // session it asks the record under, which is that one. The engine refuses that record until
  // the switch event: `recordDeliveredFor` binds to `transcriptSessionID`, which answers
  // undefined while the host and this instance disagree, and the card the host wrote is what the
  // moved-to conversation's restore reads.
  test("carded into the session the host moved to asks for its record under that session", async () => {
    const { frames, state, recorded, handed, claim, claimState } = harness();
    const frame: DedupeIdentity = { event_id: "event-1", dedupe_key: "relay-1", source: "github" };
    state.hostSession = "s2";

    expect(frames.sendCard("s2", "card", "card", frame, claim(frame))).toBe("queued");
    expect(handed).toEqual([{ content: "card", deliverAs: "card", identity: frame }]);

    await frames.written(writtenCard({ event_id: "event-1" }));

    expect(recorded).toEqual([{ session: "s2", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });
});

describe("a card the host withdrew unwritten", () => {
  test("is handed again, as it was and under its claim, only once a host moment passed", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("s1", "card", "aside", frame, claim(frame));

    // Handed since the last moment: it may still be on its way into the host's queue.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
    // The host is busy, or on another session: nothing is handed back, but the moment counts.
    state.settled = false;
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
    state.settled = true;
    frames.handBackWithdrawn();

    expect(handed).toEqual([
      { content: "card", deliverAs: "aside", identity: frame },
      { content: "card", deliverAs: "aside", identity: frame },
    ]);
    expect(claimState(frame)).toBe("pending");
    expect(frames.waitsOnCard(frame)).toBe(true);
    // Just handed again, so the next settled moment leaves it alone.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(2);
  });

  test("is handed again after a run that ended aborted", () => {
    const { frames, handed, claim } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("s1", "card", "card", frame, claim(frame));

    frames.runAborted();
    frames.handBackWithdrawn();

    expect(handed).toHaveLength(2);
  });

  test("of another session is not handed back", () => {
    const { frames, state, handed, claim } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("s1", "card", "card", frame, claim(frame));

    state.session = "s2";
    frames.runAborted();
    frames.handBackWithdrawn();

    expect(handed).toHaveLength(1);
  });

  test("that cannot be handed again is reported and still waits, to be handed at the next moment", () => {
    const { frames, state, handed, handBackFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("s1", "card", "card", frame, claim(frame));
    frames.runAborted();

    state.refuseHand = true;
    frames.handBackWithdrawn();
    expect(handBackFailures).toEqual(["event-1: the host refused the card"]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(claimState(frame)).toBe("pending");

    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(2);
  });
});

// An inert card goes as a steer that starts no turn, so a busy host would steer it into the turn
// it runs: it is handed only to a settled host. While it is held, its record in the transcript
// holds it across a restart once the session's store holds the record, so its claim settles once
// that record is written, as a held stored card's does.
describe("an inert card", () => {
  test("sent while the host is not settled is held and recorded, its claim settles, and it is handed once at the first settled moment", async () => {
    const { frames, state, handed, recorded, storedRecords, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;

    expect(frames.sendCard("s1", "card", "inert", frame, claim(frame))).toBe("queued");
    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(storedRecords).toEqual([
      {
        session: "s1",
        card: {
          identity: frame,
          content: "card",
          dispatchMessageId: undefined,
          deliverAs: "inert",
        },
      },
    ]);
    expect(claimState(frame)).toBe("settled");
    // Recorded, not shown: the ledger records the frame once the host writes the card.
    expect(recorded).toEqual([]);
    // An inert card waits for no store: no stored check runs for it.
    expect(frames.waitsOnStored()).toBe(false);

    frames.handBackWithdrawn();
    expect(handed).toEqual([]);

    state.settled = true;
    frames.handBackWithdrawn();
    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);

    // Just handed, so the next settled moment leaves it alone.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);

    await frames.written(writtenCard({ event_id: "event-1" }));
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(storedRecords).toHaveLength(1);
  });

  test("sent while the host is settled is handed at once, and not recorded", () => {
    const { frames, handed, storedRecords, claim, claimState } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("s1", "card", "inert", frame, claim(frame))).toBe("queued");

    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
    // An idle host writes it at once, and its claim waits for that write.
    expect(storedRecords).toEqual([]);
    expect(claimState(frame)).toBe("pending");
  });

  test("held, that cannot be handed at a settled moment, is reported and handed at the next", () => {
    const { frames, state, handed, handBackFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;
    frames.sendCard("s1", "card", "inert", frame, claim(frame));

    state.settled = true;
    state.refuseHand = true;
    frames.handBackWithdrawn();
    expect(handBackFailures).toEqual(["event-1: the host refused the card"]);
    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(claimState(frame)).toBe("settled");

    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
  });

  // A record that cannot be written holds nothing across a restart, so the relay's copy is all
  // that does: the claim waits for the host's write, as an inert card's did before it was recorded.
  test("held, whose record cannot be written, keeps its claim until the host writes it", async () => {
    const { frames, state, handed, recorded, storedRecordFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;
    state.refuseRecord = true;

    expect(frames.sendCard("s1", "card", "inert", frame, claim(frame))).toBe("queued");
    expect(storedRecordFailures).toEqual(["event-1: the session file refused the record"]);
    expect(claimState(frame)).toBe("pending");

    state.settled = true;
    frames.handBackWithdrawn();
    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
    expect(claimState(frame)).toBe("pending");

    await frames.written(writtenCard({ event_id: "event-1" }));
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });

  test("held when the host moves to another session is dropped: a recorded one keeps its settled claim, an unrecorded one's claim is released", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const [kept, unkept] = [dispatchFrame(1), dispatchFrame(2)];
    state.settled = false;
    frames.sendCard("s1", "kept", "inert", kept, claim(kept));
    state.refuseRecord = true;
    frames.sendCard("s1", "unkept", "inert", unkept, claim(unkept));

    state.session = "s2";
    frames.releaseOtherSessions("s2");
    expect(frames.waitsOnCard(kept)).toBe(false);
    expect(frames.waitsOnCard(unkept)).toBe(false);
    // The record of the first holds it for the restore once the host is back on its session.
    expect(claimState(kept)).toBe("settled");
    expect(claimState(unkept)).toBe("released");

    state.settled = true;
    frames.handBackWithdrawn();
    state.session = "s1";
    frames.handBackWithdrawn();
    expect(handed).toEqual([]);
  });

  // The frame of a recorded card is answered `done`, so the stream keeps no copy of it: a restart
  // holds the card again from its record, and hands it as the record says, a steer that starts no
  // turn, never as a stored card.
  test("recorded, is held again by a restore and handed inert at the first settled moment", async () => {
    const first = harness();
    const [late, written] = [dispatchFrame(1), dispatchFrame(2)];
    first.state.settled = false;
    first.frames.sendCard("s1", "late BTW", "inert", late, first.claim(late), "message-1");
    first.frames.sendCard("s1", "written", "inert", written, first.claim(written));
    const transcript = [
      ...first.storedRecords.map(({ card }) => ({
        type: "custom",
        customType: STORED_CARD_ENTRY,
        data: storedCardRecord(card),
      })),
      // The host wrote the second card before the stop.
      storedEntry({ event_id: "event-2" }),
    ];
    const held = heldStoredCards(transcript, replaySnapshot(transcript).ledger);
    expect(held).toEqual([
      { identity: late, content: "late BTW", dispatchMessageId: "message-1", deliverAs: "inert" },
    ]);

    const second = harness();
    second.state.settled = false;
    // A restore that reads the same transcript twice holds the card once.
    second.frames.requeueStored(held);
    second.frames.requeueStored(held);
    expect(second.frames.waitsOnCard(late)).toBe(true);
    // No stored check hands it: it is an inert card.
    expect(second.frames.waitsOnStored()).toBe(false);
    second.state.settled = true;
    expect(await second.frames.storedCheck()).toBe(false);
    expect(second.handed).toEqual([]);

    second.frames.handBackWithdrawn();
    expect(second.handed).toEqual([
      { content: "late BTW", deliverAs: "inert", identity: late, dispatchMessageId: "message-1" },
    ]);
    // The relay sends the frame again: the card handed is that frame's, so nothing more is
    // handed or recorded, and the copy settles.
    expect(second.frames.sendCard("s1", "late BTW", "inert", late, second.claim(late))).toBe(
      "queued"
    );
    expect(second.handed).toHaveLength(1);
    expect(second.storedRecords).toEqual([]);
    expect(second.claimState(late)).toBe("settled");

    await second.frames.written(writtenCard({ event_id: "event-1" }));
    expect(second.recorded).toEqual([{ session: "s1", identity: late }]);
    expect(second.frames.waitsOnCard(late)).toBe(false);
  });

  test("whose frame the ledger keeps nothing of, sent while the host is busy, is not handed and its claim is released", () => {
    const { frames, state, handed } = harness();
    // No event id, and a key that does not name its event.
    const frame: DedupeIdentity = { dedupe_key: "content-hash", source: "mcp" };
    const { claim, became } = notingClaim();
    state.settled = false;

    frames.sendCard("s1", "card", "inert", frame, claim);

    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(became).toEqual(["released"]);
  });

  // The live view draws a Dispatch message from Dispatch's store, so the agent stream leaves out
  // the card that delivers it, which its `details` name (dispatch://INFRA-5271): a card held for a
  // settled host still names it when it is handed.
  test("held, is handed naming the Dispatch message it delivers, as one handed at once is", () => {
    const { frames, state, handed, claim } = harness();
    const held = dispatchFrame(1);
    const atOnce = dispatchFrame(2);
    state.settled = false;
    frames.sendCard("s1", "late BTW", "inert", held, claim(held), "message-1");
    expect(handed).toEqual([]);

    state.settled = true;
    frames.handBackWithdrawn();
    frames.sendCard("s1", "card", "card", atOnce, claim(atOnce), "message-2");

    expect(handed).toEqual([
      { content: "late BTW", deliverAs: "inert", identity: held, dispatchMessageId: "message-1" },
      { content: "card", deliverAs: "card", identity: atOnce, dispatchMessageId: "message-2" },
    ]);
  });
});

// A `low` card goes as `nextTurn` with no turn: an idle host stores it in the context and the
// session file at once, with no `message_end`, and a busy host keeps it in memory only. So it is
// handed only to a settled host, and the session file is the witness that it was written. While it
// waits, held or handed, its record in the transcript holds it across a restart once the session's
// store holds the record, so its claim settles once that record is written.
describe("a stored card", () => {
  test("sent while the host is not settled is held and recorded, its claim settles, and the first settled check hands it", async () => {
    const { frames, state, handed, recorded, storedRecords, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;

    expect(frames.sendCard("s1", "card", "stored", frame, claim(frame), "message-1")).toBe(
      "queued"
    );
    expect(handed).toEqual([]);
    expect(storedRecords).toEqual([
      {
        session: "s1",
        card: {
          identity: frame,
          content: "card",
          dispatchMessageId: "message-1",
          deliverAs: "stored",
        },
      },
    ]);
    expect(claimState(frame)).toBe("settled");
    // Recorded, not shown: the ledger records the frame once the session's store holds the card.
    expect(recorded).toEqual([]);
    expect(await frames.storedCheck()).toBe(true);
    expect(handed).toEqual([]);

    state.settled = true;
    expect(await frames.storedCheck()).toBe(true);
    expect(handed).toEqual([
      { content: "card", deliverAs: "stored", identity: frame, dispatchMessageId: "message-1" },
    ]);
    expect(storedRecords).toHaveLength(1);
  });

  test("is written once the session's store holds its entry, and never handed again before that", async () => {
    const { frames, state, handed, recorded, storedRecords, claim, claimState } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("s1", "card", "stored", frame, claim(frame))).toBe("queued");
    expect(handed).toHaveLength(1);
    // Handed and recorded: the record holds the card from here on, so its claim settles.
    expect(storedRecords).toHaveLength(1);
    expect(claimState(frame)).toBe("settled");
    // Not stored yet: the host stores no session before its first assistant message.
    expect(await frames.storedCheck()).toBe(true);
    state.stored = [storedEntry({ event_id: "event-2" })];
    expect(await frames.storedCheck()).toBe(true);
    for (const _moment of [1, 2, 3]) frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
    expect(recorded).toEqual([]);

    state.stored = [
      storedEntry({ event_id: "event-2" }),
      storedEntry({ dedupe_key: "dispatch-1" }),
    ];
    expect(await frames.storedCheck()).toBe(false);
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(frames.waitsOnStored()).toBe(false);
  });

  test("held is handed before a card that starts a turn, in the order it came", () => {
    const { frames, state, handed, claim } = harness();
    const [first, second, card] = [dispatchFrame(1), dispatchFrame(2), dispatchFrame(3)];
    state.settled = false;
    frames.sendCard("s1", "first", "stored", first, claim(first));
    frames.sendCard("s1", "second", "stored", second, claim(second));

    state.settled = true;
    frames.sendCard("s1", "card", "card", card, claim(card));

    expect(handed.map(({ content, deliverAs }) => [content, deliverAs])).toEqual([
      ["first", "stored"],
      ["second", "stored"],
      ["card", "card"],
    ]);
  });

  test("held holds back no later card", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const [low, card] = [dispatchFrame(1), dispatchFrame(2)];
    state.settled = false;
    frames.sendCard("s1", "low", "stored", low, claim(low));
    frames.sendCard("s1", "card", "card", card, claim(card));

    expect(handed.map(({ content }) => content)).toEqual(["card"]);
    expect(claimState(low)).toBe("settled");
  });

  // An edge listener's relay holds at most 16 frames of a session in flight (`relayEdgeInFlight`
  // in packages/envoy/cmd/listener/session_relay.go): a held card whose claim waited for the
  // session's store would take one of those places until the turn ends.
  test("held, more than an edge listener's relay holds in flight, leaves no claim open", () => {
    const { frames, state, handed, storedRecords, claim, claimState } = harness();
    const lows = Array.from({ length: 17 }, (_, n) => dispatchFrame(n + 1));
    const card = dispatchFrame(18);
    state.settled = false;
    for (const low of lows) frames.sendCard("s1", `low ${low.event_id}`, "stored", low, claim(low));

    expect(storedRecords).toHaveLength(17);
    expect(lows.map(claimState)).toEqual(lows.map(() => "settled"));
    expect(frames.sendCard("s1", "card", "card", card, claim(card))).toBe("queued");
    expect(handed.map(({ content }) => content)).toEqual(["card"]);
    expect(frames.waitsOnStored()).toBe(true);
  });

  // The architect's ruling on test handoff v20's class search (the class of
  // PRRT_kwDORFy7ds67LAM9): a late BTW goes to the host as an inert card with its relay claim
  // (extensions/envoy.ts, `relayed.late`), and an inert card sent while the host is busy waits for
  // the first settled heartbeat. Its claim must not hold one of the edge listener's 16 in-flight
  // places of the session while it waits, or 16 late BTWs held through one long turn stop every
  // later card (the spec's Design: a held card keeps no later card from its reader).
  test("an inert card held while the host is busy, more than an edge listener's relay holds in flight, leaves no claim open", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const lates = Array.from({ length: 17 }, (_, n) => dispatchFrame(n + 1));
    const card = dispatchFrame(18);
    state.settled = false;
    for (const late of lates)
      frames.sendCard("s1", `late ${late.event_id}`, "inert", late, claim(late));

    expect(lates.map(claimState)).toEqual(lates.map(() => "settled"));
    expect(frames.sendCard("s1", "card", "card", card, claim(card))).toBe("queued");
    expect(handed.map(({ content }) => content)).toEqual(["card"]);
  });

  test("whose record cannot be written keeps its claim until the session's store holds it", async () => {
    const { frames, state, recorded, storedRecordFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.refuseRecord = true;

    frames.sendCard("s1", "card", "stored", frame, claim(frame));
    expect(storedRecordFailures).toEqual(["event-1: the session file refused the record"]);
    expect(claimState(frame)).toBe("pending");

    state.stored = [storedEntry({ event_id: "event-1" })];
    expect(await frames.storedCheck()).toBe(false);
    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });

  // The frame of a recorded card is answered `done`, so the stream keeps no copy of it: a restart
  // holds the card again from its record, until the transcript shows the card written.
  test("recorded, is held again by a restore until the transcript shows it written", async () => {
    const first = harness();
    const [low, written, shown] = [dispatchFrame(1), dispatchFrame(2), dispatchFrame(3)];
    first.state.settled = false;
    first.frames.sendCard("s1", "low", "stored", low, first.claim(low), "message-1");
    first.frames.sendCard(
      "s1",
      [{ type: "text", text: "written" }],
      "stored",
      written,
      first.claim(written)
    );
    first.frames.sendCard("s1", "shown", "stored", shown, first.claim(shown));
    const transcript = [
      ...first.storedRecords.map(({ card }) => ({
        type: "custom",
        customType: STORED_CARD_ENTRY,
        data: storedCardRecord(card),
      })),
      // The host stored the second card, and the ledger recorded the third, before the stop.
      storedEntry({ event_id: "event-2" }),
      { type: "custom", customType: REPLAY_DELIVERED_ENTRY, data: { event_id: "event-3", at: 1 } },
    ];
    const held = heldStoredCards(transcript, replaySnapshot(transcript).ledger);
    expect(held).toEqual([
      { identity: low, content: "low", dispatchMessageId: "message-1", deliverAs: "stored" },
    ]);

    const second = harness();
    // A restore that reads the same transcript twice holds the card once.
    second.frames.requeueStored(held);
    second.frames.requeueStored(held);
    expect(await second.frames.storedCheck()).toBe(true);
    expect(second.handed).toEqual([
      { content: "low", deliverAs: "stored", identity: low, dispatchMessageId: "message-1" },
    ]);
    // The relay sends the frame again, a frame before it holding the resume point below it: the
    // card handed is that frame's, so nothing more is handed or recorded, and the copy settles.
    expect(second.frames.sendCard("s1", "low", "stored", low, second.claim(low))).toBe("queued");
    expect(second.handed).toHaveLength(1);
    expect(second.storedRecords).toEqual([]);
    expect(second.claimState(low)).toBe("settled");

    second.state.stored = [storedEntry({ dedupe_key: "dispatch-1" })];
    expect(await second.frames.storedCheck()).toBe(false);
    expect(second.recorded).toEqual([{ session: "s1", identity: low }]);
  });

  // The host reads idle until it starts the turn a card asked for, after its own awaits; a
  // stored card handed in between would find it streaming and stay in its memory.
  test("is held while a card handed to start a turn has not started it", async () => {
    const { frames, handed, claim } = harness();
    const card = dispatchFrame(1);
    const low = dispatchFrame(2);
    const later = dispatchFrame(3);
    const last = dispatchFrame(4);
    frames.sendCard("s1", "card", "card", card, claim(card));
    frames.sendCard("s1", "low", "stored", low, claim(low));
    await frames.storedCheck();
    frames.handBackWithdrawn();
    expect(handed.map(({ content }) => content)).toEqual(["card"]);

    // Once that turn has started, the stored card goes at the first settled check.
    frames.runStarted();
    await frames.storedCheck();
    expect(handed.map(({ content }) => content)).toEqual(["card", "low"]);

    // A card the host folds with no turn, which it writes with no run, ends the wait as well.
    frames.sendCard("s1", "card", "card", later, claim(later));
    frames.sendCard("s1", "later-low", "stored", last, claim(last));
    await frames.written(writtenCard({ event_id: "event-3" }));
    await frames.storedCheck();
    expect(handed.map(({ content }) => content)).toEqual(["card", "low", "card", "later-low"]);
  });

  test("whose frame the ledger keeps nothing of, sent while the host is busy, is not handed and its claim is released", () => {
    const { frames, state, handed } = harness();
    const frame: DedupeIdentity = { dedupe_key: "content-hash", source: "mcp" };
    const { claim, became } = notingClaim();
    state.settled = false;

    frames.sendCard("s1", "card", "stored", frame, claim);

    expect(handed).toEqual([]);
    expect(frames.waitsOnStored()).toBe(false);
    expect(became).toEqual(["released"]);
  });
});

describe("a replay notice", () => {
  test("sent while the host is not settled is held, and handed once at the first settled moment", () => {
    const { frames, state, notices, handed } = harness();
    state.settled = false;

    frames.sendNotice("stream recreated");
    frames.sendNotice("retention gap");
    expect(notices).toEqual([]);
    frames.handBackWithdrawn();
    expect(notices).toEqual([]);

    state.settled = true;
    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);

    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);
    // A notice is no card: nothing of it is handed as one, or waited on.
    expect(handed).toEqual([]);
  });

  test("sent while the host is settled is handed at once", () => {
    const { frames, notices } = harness();

    frames.sendNotice("retention gap");

    expect(notices).toEqual(["retention gap"]);
    frames.handBackWithdrawn();
    expect(notices).toEqual(["retention gap"]);
  });

  test("that cannot be handed is reported and held, in order, for the next settled moment", () => {
    const { frames, state, notices, noticeFailures } = harness();
    state.refuseHand = true;

    frames.sendNotice("stream recreated");
    expect(noticeFailures).toEqual(["the host refused the notice"]);
    state.settled = false;
    frames.sendNotice("retention gap");
    expect(notices).toEqual([]);

    state.settled = true;
    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);
    expect(noticeFailures).toHaveLength(1);
  });

  test("held when the host moves to another session is dropped", () => {
    const { frames, state, notices } = harness();
    state.settled = false;
    frames.sendNotice("retention gap");

    state.session = "s2";
    frames.releaseOtherSessions("s2");
    state.settled = true;
    frames.handBackWithdrawn();
    state.session = "s1";
    frames.handBackWithdrawn();

    expect(notices).toEqual([]);
  });

  test("held while the host is not settled is recorded in the transcript of its session", () => {
    const { frames, state, notices, noticeRecords } = harness();
    state.settled = false;

    frames.sendNotice("stream recreated");
    frames.sendNotice("retention gap");

    expect(notices).toEqual([]);
    expect(noticeRecords).toEqual([
      { session: "s1", id: expect.any(String), content: "stream recreated" },
      { session: "s1", id: expect.any(String), content: "retention gap" },
    ]);
    expect(noticeRecords[0]?.id).not.toBe(noticeRecords[1]?.id);
  });

  test("held, then handed at a settled moment, is recorded handed under the id it was held by", () => {
    const { frames, state, notices, noticeRecords } = harness();
    state.settled = false;
    frames.sendNotice("retention gap");
    const id = noticeRecords[0]?.id;

    state.settled = true;
    frames.handBackWithdrawn();
    frames.handBackWithdrawn();

    expect(notices).toEqual(["retention gap"]);
    expect(noticeRecords).toEqual([
      { session: "s1", id: expect.any(String), content: "retention gap" },
      { session: "s1", id, handed: true },
    ]);
  });

  test("handed at once records nothing", () => {
    const { frames, notices, noticeRecords } = harness();

    frames.sendNotice("retention gap");
    frames.handBackWithdrawn();

    expect(notices).toEqual(["retention gap"]);
    expect(noticeRecords).toEqual([]);
  });

  test("the host refused at once is held and recorded", () => {
    const { frames, state, notices, noticeRecords } = harness();
    state.refuseHand = true;
    frames.sendNotice("retention gap");
    expect(noticeRecords).toEqual([
      { session: "s1", id: expect.any(String), content: "retention gap" },
    ]);

    state.refuseHand = false;
    frames.handBackWithdrawn();

    expect(notices).toEqual(["retention gap"]);
    expect(noticeRecords[1]).toEqual({ session: "s1", id: noticeRecords[0]?.id, handed: true });
  });

  test("whose record the session file refuses is reported and still held in this process", () => {
    const { frames, state, notices, noticeRecords, recordFailures } = harness();
    state.settled = false;
    state.refuseRecord = true;
    frames.sendNotice("retention gap");
    expect(recordFailures).toEqual(["the session file refused the record"]);

    state.settled = true;
    frames.handBackWithdrawn();

    expect(notices).toEqual(["retention gap"]);
    // Recorded nowhere as held, so nothing is marked handed.
    expect(noticeRecords).toEqual([]);
    expect(recordFailures).toHaveLength(1);
  });

  test("requeued from the transcript by its id is handed once, however often it is requeued", () => {
    const { frames, state, notices, noticeRecords } = harness();
    state.settled = false;
    frames.sendNotice("stream recreated");
    const recordedHeld = noticeRecords.length;
    const held = [{ id: "notice-1", content: "retention gap" }];

    frames.requeueNotices(held);
    frames.requeueNotices(held);
    // Recorded held already: requeuing records nothing.
    expect(noticeRecords).toHaveLength(recordedHeld);
    state.settled = true;
    frames.handBackWithdrawn();
    // A restore that reads the transcript again, its handed mark unread, holds nothing again.
    frames.requeueNotices(held);
    frames.handBackWithdrawn();

    expect(notices).toEqual(["stream recreated", "retention gap"]);
    expect(noticeRecords.slice(recordedHeld)).toEqual([
      { session: "s1", id: noticeRecords[0]?.id, handed: true },
      { session: "s1", id: "notice-1", handed: true },
    ]);
  });

  test("held for a session, then requeued from a fork's copy of its entry, is handed once in the fork", () => {
    const { frames, state, notices, noticeRecords } = harness();
    state.settled = false;
    frames.sendNotice("retention gap");
    const id = noticeRecords[0]?.id ?? "";
    // The fork's transcript copies the entry, id included.
    const held = [{ id, content: "retention gap" }];

    state.session = "s2";
    frames.requeueNotices(held);
    frames.requeueNotices(held);
    frames.releaseOtherSessions("s2");
    state.settled = true;
    frames.handBackWithdrawn();
    expect(notices).toEqual(["retention gap"]);
    // A restore that reads the fork's transcript again, its handed mark unread, holds nothing.
    frames.requeueNotices(held);
    frames.handBackWithdrawn();

    expect(notices).toEqual(["retention gap"]);
    expect(noticeRecords).toEqual([
      { session: "s1", id, content: "retention gap" },
      { session: "s2", id, handed: true },
    ]);
  });
});

describe("a person's turn handed to the host", () => {
  test("is recorded once the host writes the message it is found as", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: frame,
      claim: claim(frame),
      via: "typed",
    });

    await frames.written({ role: "user", content: "other" });
    expect(recorded).toEqual([]);

    state.matched = "m-1";
    await frames.written({ role: "user", content: "hello" });

    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });

  test("answered as submitting no message is done with at once", async () => {
    const { frames, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: frame,
      claim: claim(frame),
      via: "typed",
    });

    await frames.turnHandled("m-1");
    await frames.turnHandled("m-unknown");

    expect(recorded).toEqual([{ session: "s1", identity: frame }]);
    expect(claimState(frame)).toBe("settled");
  });

  test("sent as text is done with at the run's end, found or not; a typed one waits on", async () => {
    const { frames, recorded, claim, claimState } = harness();
    const text = dispatchFrame(1);
    const typed = dispatchFrame(2);
    const elsewhere = dispatchFrame(3);
    frames.awaitTurn("m-1", { sessionID: "s1", identity: text, claim: claim(text), via: "text" });
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: typed,
      claim: claim(typed),
      via: "typed",
    });
    frames.awaitTurn("m-3", {
      sessionID: "s0",
      identity: elsewhere,
      claim: claim(elsewhere),
      via: "text",
    });

    await frames.textTurnsEnded();

    expect(recorded).toEqual([{ session: "s1", identity: text }]);
    expect(claimState(text)).toBe("settled");
    expect(claimState(typed)).toBe("pending");
    expect(claimState(elsewhere)).toBe("pending");
  });

  // In order as production runs it: the aborted run's `agent_end` calls `runAborted`, then
  // `textTurnsEnded` (extensions/envoy.ts), before any heartbeat's `handBackWithdrawn`. So only a
  // typed input is released; a `pi.sendUserMessage` turn the interrupt withdrew is recorded as
  // shown at the run's end, as the host keeps no mark that tells it from one it wrote.
  test("an interrupt withdrew is released, not handed again, once it was in the host's queue, unless sent as text", async () => {
    const { frames, recorded, withdrawn, handed, claim, claimState } = harness();
    const typed = dispatchFrame(1);
    const text = dispatchFrame(2);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: typed,
      claim: claim(typed),
      via: "typed",
    });
    frames.awaitTurn("m-2", { sessionID: "s1", identity: text, claim: claim(text), via: "text" });
    frames.runAborted();
    await frames.textTurnsEnded();
    expect(recorded).toEqual([{ session: "s1", identity: text }]);
    expect(claimState(text)).toBe("settled");

    // The typed input is not in the host's queue until the host answers that it submitted one.
    frames.handBackWithdrawn();
    expect(withdrawn).toEqual([]);
    expect(claimState(typed)).toBe("pending");

    frames.turnSubmitted("m-1");
    frames.handBackWithdrawn();
    expect(withdrawn).toEqual([]);
    frames.handBackWithdrawn();

    expect(withdrawn).toEqual([["s1", "m-1"]]);
    expect(claimState(typed)).toBe("released");
    expect(recorded).toEqual([{ session: "s1", identity: text }]);
    expect(handed).toEqual([]);
  });

  test("of a frame with no identity settles its claim at once and waits on nothing", async () => {
    const { frames, state, recorded, withdrawn } = harness();
    const text = notingClaim();
    const typed = notingClaim();

    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: undefined,
      claim: text.claim,
      via: "text",
    });
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: undefined,
      claim: typed.claim,
      via: "typed",
    });
    expect(text.became).toEqual(["settled"]);
    expect(typed.became).toEqual(["settled"]);

    // Nothing waits: no write, run end, interrupt or session change finds either turn.
    state.matched = "m-1";
    await frames.written({ role: "user", content: "hello" });
    await frames.textTurnsEnded();
    frames.turnSubmitted("m-2");
    frames.runAborted();
    frames.handBackWithdrawn();
    frames.handBackWithdrawn();
    frames.releaseOtherSessions("s2");
    expect(recorded).toEqual([]);
    expect(withdrawn).toEqual([]);
    expect(text.became).toEqual(["settled"]);
    expect(typed.became).toEqual(["settled"]);
  });
});

describe("a session change", () => {
  test("releases what the session left was handed and has not written, and keeps the rest", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const leftCard = dispatchFrame(1);
    const leftTurn = dispatchFrame(2);
    const keptCard = dispatchFrame(3);
    frames.sendCard("s1", "card", "card", leftCard, claim(leftCard));
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: leftTurn,
      claim: claim(leftTurn),
      via: "typed",
    });
    state.session = "s2";
    frames.sendCard("s2", "card", "card", keptCard, claim(keptCard));

    frames.releaseOtherSessions("s2");

    expect(claimState(leftCard)).toBe("released");
    expect(claimState(leftTurn)).toBe("released");
    expect(claimState(keptCard)).toBe("pending");
    expect(frames.waitsOnCard(leftCard)).toBe(false);
    // The released card's write, should the host still make it, records nothing.
    await frames.written(writtenCard({ event_id: "event-1" }));
    await frames.written(writtenCard({ event_id: "event-3" }));
    expect(recorded).toEqual([{ session: "s2", identity: keptCard }]);
  });
});
