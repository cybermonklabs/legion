import { describe, expect, test } from "bun:test";
import {
  type ConsumerDeliveryOptions,
  type ConsumerDeliverySnapshot,
  type ConsumerFrame,
  type ConsumerGapNotice,
  createConsumerDelivery,
  DELIVER_SUBJECT_PREFIX,
  decodeRelayFrame,
  deliverSubject,
  FOLLOW_CLOCK_ALLOWANCE_MS,
  inboundUnavailable,
  RELAY_PROGRESS_INTERVAL_MS,
  type RelayMessage,
  type RelayReply,
  registerWithRelay,
} from "../consumer-delivery";
import { createDeliveryLedger, type DeliveryLedger, type LedgerSnapshot } from "../delivery-ledger";
import type { DeliveryAnswer } from "../transport";

const SESSION = "ses_a";
const AGENT = "notifications.agent.ses_a";
const ISSUE_TOPIC = "notifications.dispatch.issue.INFRA-1.>";
const ISSUE = "notifications.dispatch.issue.INFRA-1.comment";
const STREAM = "stream-1";
const T0 = Date.parse("2026-10-27T05:00:00Z");
const MINUTE = 60_000;

interface RelayInput {
  readonly seq: number;
  readonly subject?: string;
  readonly delivered?: number;
  readonly storedAt?: number;
  readonly gap?: boolean;
  readonly streamID?: string;
  readonly eventID?: string;
  readonly reply?: string;
  /** The frame's bytes, for a row driving what the client does with a frame it cannot read. */
  readonly payload?: string;
}

function relay(input: RelayInput): RelayMessage {
  const headers: Record<string, string> = {
    "Envoy-Subject": input.subject ?? AGENT,
    "Envoy-Stream-Seq": String(input.seq),
    "Envoy-Stream-Id": input.streamID ?? STREAM,
    "Envoy-Delivered": String(input.delivered ?? 1),
    "Envoy-Stored-At": new Date(input.storedAt ?? T0 + MINUTE).toISOString(),
    ...(input.gap === true ? { "Envoy-Gap": "true" } : {}),
  };
  return {
    data: new TextEncoder().encode(
      input.payload ??
        JSON.stringify({ event_id: input.eventID ?? `evt-${input.seq}`, source: "agent" })
    ),
    headers: { get: (name) => headers[name] ?? "" },
    reply: input.reply ?? `_INBOX.${input.seq}.${input.delivered ?? 1}`,
  };
}

/** Intervals that fire as the fake clock moves. */
function fakeTimers(clock: { now: number }) {
  const running = new Set<{ readonly ms: number; next: number; readonly run: () => void }>();
  return {
    every(ms: number, run: () => void) {
      const timer = { ms, next: clock.now + ms, run };
      running.add(timer);
      return () => running.delete(timer);
    },
    advance(ms: number) {
      const until = clock.now + ms;
      for (;;) {
        const due = [...running]
          .filter((timer) => timer.next <= until)
          .sort((a, b) => a.next - b.next)[0];
        if (due === undefined) break;
        clock.now = due.next;
        due.next += due.ms;
        due.run();
      }
      clock.now = until;
    },
  };
}

function deliveryLedger(clock: { now: number }, written: { snapshot?: LedgerSnapshot } = {}) {
  return createDeliveryLedger({
    host: {
      sessionID: () => SESSION,
      followedSubjects: () => [AGENT],
      showFailureNotice: async () => undefined,
      failureNoticeFailed: () => undefined,
      log: () => undefined,
      write(change) {
        written.snapshot = change.snapshot();
      },
    },
    now: () => clock.now,
  });
}

function setup(
  options: {
    readonly clock?: { now: number };
    readonly ledger?: Pick<DeliveryLedger, "delivered">;
    /** The give-up's own render, for the row driving a frame whose bytes cannot be read. */
    readonly render?: ConsumerDeliveryOptions["render"];
  } = {}
) {
  const clock = options.clock ?? { now: T0 };
  const timers = fakeTimers(clock);
  const replies: Array<readonly [string, RelayReply]> = [];
  const handed: ConsumerFrame[] = [];
  const running: Promise<void>[] = [];
  const gaps: ConsumerGapNotice[] = [];
  // The failure notices shown through the ledger (`liveFailed`), each with the session it names:
  // the notice belongs to the session the frame was addressed to, not to whatever the host has
  // open when the frame gives up.
  const failed: Array<{ readonly session: string; readonly eventID: string | undefined }> = [];
  /** Every notice the give-up path started, which `settled` waits on. */
  const notices: Promise<unknown>[] = [];
  // What the ledger answers a notice with: whether anybody was told. A session the host has left
  // is told nothing, and the relay must then leave the frame on the stream.
  const notice = { told: true, rejectWith: undefined as Error | undefined };
  const logs: string[] = [];
  // The snapshots the host was asked to write.
  const persisted: ConsumerDeliverySnapshot[] = [];
  const state = {
    deliver: (_frame: ConsumerFrame): void | Promise<void> => undefined,
  };
  const config: ConsumerDeliveryOptions = {
    sessionID: SESSION,
    host: {
      deliver(frame) {
        handed.push(frame);
        const result = state.deliver(frame);
        if (result instanceof Promise) running.push(result);
        return result;
      },
      gap: (notice) => gaps.push(notice),
      log: (message) => logs.push(message),
    },
    publish: (reply, body) => replies.push([reply, body]),
    ledger: {
      delivered: (identity) => options.ledger?.delivered(identity) ?? false,
      liveFailed: (session, envelope) => {
        failed.push({ session, eventID: envelope.event_id });
        const answer = (async () => {
          // A ledger of this repository never rejects; a host may pass one of its own, and the
          // relay must keep the frame for it rather than ack a give-up it knows nothing about.
          if (notice.rejectWith !== undefined) throw notice.rejectWith;
          return notice.told;
        })();
        notices.push(answer.catch(() => undefined));
        return answer;
      },
    },
    persist: (snapshot) => {
      persisted.push(snapshot);
      return true;
    },
    now: () => clock.now,
    every: timers.every,
    ...(options.render === undefined ? {} : { render: options.render }),
  };
  const delivery = createConsumerDelivery(config);
  /** Waits until every hand-off the host started has finished, those they let go included, and
   *  then waits on the notices the give-up path started: the relay answers a frame it gave up on
   *  only once the ledger has said whether anybody was told (`failed`), so a row reading that
   *  answer waits on the notice itself rather than on a count of microtasks. */
  const settled = async () => {
    let seen: number;
    do {
      seen = running.length + notices.length;
      await Promise.allSettled([...running, ...notices]);
      // The settle runs in the notice's own `then`, one microtask after it resolves.
      await Promise.resolve();
    } while (running.length + notices.length !== seen);
  };
  return {
    clock,
    timers,
    replies,
    handed,
    gaps,
    failed,
    notice,
    logs,
    state,
    delivery,
    settled,
    persisted,
  };
}

const seqs = (frames: readonly ConsumerFrame[]) => frames.map((frame) => frame.streamSeq);
const repliesTo = (replies: ReadonlyArray<readonly [string, RelayReply]>, reply: string) =>
  replies.filter(([to]) => to === reply).map(([, body]) => body);

describe("relay frames", () => {
  test("the relay subject is the session's deliver subject", () => {
    expect(deliverSubject(SESSION)).toBe("notifications.envoy.deliver.ses_a");
    expect(DELIVER_SUBJECT_PREFIX).toBe("notifications.envoy.deliver.");
    expect(setup().delivery.subject).toBe("notifications.envoy.deliver.ses_a");
  });

  test("decodes every header, a nanosecond stored time included", () => {
    const headers: Record<string, string> = {
      "Envoy-Subject": ISSUE,
      "Envoy-Stream-Seq": "42",
      "Envoy-Stream-Id": STREAM,
      "Envoy-Delivered": "3",
      "Envoy-Stored-At": "2026-10-27T05:00:01.123456789Z",
      "Envoy-Gap": "true",
    };
    const decoded = decodeRelayFrame({
      data: "{}",
      headers: { get: (name) => headers[name] ?? "" },
      reply: "_INBOX.x",
    });

    expect(decoded).toEqual({
      frame: {
        subject: ISSUE,
        streamSeq: 42,
        streamID: STREAM,
        delivered: 3,
        storedAt: "2026-10-27T05:00:01.123456789Z",
        storedAtMs: Date.parse("2026-10-27T05:00:01.123Z"),
        gap: true,
        data: "{}",
        reply: "_INBOX.x",
      },
    });
  });

  test("a frame that cannot decode is answered done and never handed over", () => {
    const { delivery, replies, handed, logs } = setup();
    delivery.receive({ data: "{}", headers: { get: () => "" }, reply: "_INBOX.bad" });

    expect(replies).toEqual([["_INBOX.bad", "done"]]);
    expect(handed).toEqual([]);
    expect(logs.some((line) => line.includes("undecodable"))).toBe(true);
  });
});

describe("replies", () => {
  test("received goes out before the host works on the frame", () => {
    const { delivery, replies, state } = setup();
    const seen: Array<ReadonlyArray<readonly [string, RelayReply]>> = [];
    state.deliver = () => {
      seen.push([...replies]);
    };

    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    expect(seen).toEqual([[["_INBOX.1", "received"]]]);
  });

  test("progress every 20 s while held, done after the write, and nothing after", () => {
    const { delivery, replies, handed, timers } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    timers.advance(RELAY_PROGRESS_INTERVAL_MS - 1);
    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received"]);
    timers.advance(1);
    timers.advance(RELAY_PROGRESS_INTERVAL_MS);
    handed[0]?.done();
    handed[0]?.done();
    handed[0]?.nak();
    timers.advance(5 * RELAY_PROGRESS_INTERVAL_MS);

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "progress", "progress", "done"]);
  });

  test("nak once, and no reply after it", () => {
    const { delivery, replies, handed, timers } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    handed[0]?.nak();
    handed[0]?.done();
    timers.advance(3 * RELAY_PROGRESS_INTERVAL_MS);

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "nak"]);
  });

  // The attempt limit is of this host's own failures of one frame, counted across the frame's
  // deliveries, so three failures here are three throws and not three arrivals.
  test("a frame the host throws on comes again, until its third failure gives up", async () => {
    const { delivery, logs, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));

    delivery.receive(relay({ seq: 1, reply: "_INBOX.a" }));
    await settled();
    delivery.receive(relay({ seq: 1, delivered: 2, reply: "_INBOX.b" }));
    await settled();
    delivery.receive(relay({ seq: 1, delivered: 3, reply: "_INBOX.c" }));
    await settled();

    expect(repliesTo(replies, "_INBOX.a")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.b")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.c")).toEqual(["received", "done"]);
    expect(failed).toEqual([{ session: SESSION, eventID: "evt-1" }]);
    // Each failure is logged: the two that come again, and the one that gives up on it.
    expect(logs).toEqual([
      "consumer delivery: frame 1 failed; it comes again",
      "consumer delivery: frame 1 failed; it comes again",
      "consumer delivery: frame 1 failed and does not come again; its session gets the notice",
    ]);
  });

  // JetStream's `delivered` counts every delivery of the frame, and most of them are no failure
  // of this host at all: a claim the session released, a frame that arrived while the host was
  // switching, every restart of the relay at the ack floor, and each nak a give-up itself
  // answers. A session that comes back to a frame delivered many times before must still get its
  // three attempts, or its first stumble is its last.
  test("counts this host's own failures, not the deliveries the server has made", async () => {
    const { delivery, logs, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));

    delivery.receive(relay({ seq: 1, delivered: 7, reply: "_INBOX.back-1" }));
    await settled();
    delivery.receive(relay({ seq: 1, delivered: 8, reply: "_INBOX.back-2" }));
    await settled();

    // Two failures of this host, so no notice yet whatever the server's count says.
    expect(failed).toEqual([]);
    expect(repliesTo(replies, "_INBOX.back-1")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.back-2")).toEqual(["received", "nak"]);
    expect(logs).toEqual([
      "consumer delivery: frame 1 failed; it comes again",
      "consumer delivery: frame 1 failed; it comes again",
    ]);

    delivery.receive(relay({ seq: 1, delivered: 9, reply: "_INBOX.back-3" }));
    await settled();
    expect(failed).toEqual([{ session: SESSION, eventID: "evt-1" }]);
  });

  // A frame the host took, having failed it before, leaves nothing behind it: its next trouble
  // starts at the first attempt again, so a session that stumbles once an hour never gives up.
  test("forgets a frame's failures once the host takes it", async () => {
    const { delivery, handed, logs, replies, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));

    delivery.receive(relay({ seq: 1, reply: "_INBOX.try-1" }));
    await settled();
    delivery.receive(relay({ seq: 1, delivered: 2, reply: "_INBOX.try-2" }));
    await settled();

    state.deliver = () => undefined;
    delivery.receive(relay({ seq: 1, delivered: 3, reply: "_INBOX.taken" }));
    await settled();
    expect(repliesTo(replies, "_INBOX.taken")).toEqual(["received"]);
    // The host answers the frame it took, which settles it and forgets its trouble.
    handed.at(-1)?.done();
    await settled();
    expect(repliesTo(replies, "_INBOX.taken")).toEqual(["received", "done"]);

    state.deliver = () => Promise.reject(new Error("render failed"));
    delivery.receive(relay({ seq: 1, delivered: 4, reply: "_INBOX.again" }));
    await settled();

    expect(repliesTo(replies, "_INBOX.again")).toEqual(["received", "nak"]);
    expect(logs.at(-1)).toBe("consumer delivery: frame 1 failed; it comes again");
  });

  // The silent loss this change exists to prevent: `done` takes the frame off the consumer, so a
  // give-up nobody was told of would leave the message nowhere - not in the transcript, not in the
  // ledger, and, with the replay sweep gone, not on any later read of the stream. The relay holds
  // it instead, and the stream keeps it for whoever takes this session next.
  test("a give-up the ledger reported to nobody goes back to the stream rather than being acked", async () => {
    const { delivery, logs, notice, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));
    // The host has moved to another session, so the ledger shows the notice to nobody.
    notice.told = false;

    for (const [delivered, reply] of [
      [1, "_INBOX.gone-1"],
      [2, "_INBOX.gone-2"],
      [3, "_INBOX.gone"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply }));
      await settled();
    }

    expect(failed).toEqual([{ session: SESSION, eventID: "evt-1" }]);
    expect(repliesTo(replies, "_INBOX.gone")).toEqual(["received", "nak"]);
    expect(logs.slice(-2)).toEqual([
      "consumer delivery: frame 1 failed and does not come again; its session gets the notice",
      "consumer delivery: frame 1 was reported to nobody; it goes back to the stream",
    ]);

    // The frame is not written either: nothing below it is settled, so the resume point stays
    // under it and the session that takes it next is shown it.
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 0 });
  });

  // A frame whose bytes name no envelope can never be reported, and a frame sent again would fail
  // the same way, so it is the one give-up that is still acked.
  test("a give-up whose frame names no envelope is dropped rather than sent again for ever", async () => {
    const { delivery, logs, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));

    for (const [delivered, reply] of [
      [1, "_INBOX.bare-1"],
      [2, "_INBOX.bare-2"],
      [3, "_INBOX.bare"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply, payload: "not an envelope" }));
      await settled();
    }

    expect(failed).toEqual([]);
    expect(repliesTo(replies, "_INBOX.bare")).toEqual(["received", "done"]);
    expect(logs.at(-1)).toBe(
      "consumer delivery: frame 1 carries no envelope to report; it is dropped"
    );
  });

  // Round 40's loss by its most likely door: the host fails a frame by rendering it, so a frame
  // whose bytes its render throws on throws in the give-up too. Read as a deliberate drop, that
  // frame would be acked away under a line saying it names no envelope, and the message would be
  // nowhere. It goes back to the stream instead, under its own line.
  test("a give-up whose render throws goes back to the stream, named as what it is", async () => {
    const { delivery, logs, replies, failed, state, settled } = setup({
      render: () => {
        throw new Error("the frame's bytes could not be read");
      },
    });
    state.deliver = () => Promise.reject(new Error("render failed"));

    for (const [delivered, reply] of [
      [1, "_INBOX.unread-1"],
      [2, "_INBOX.unread-2"],
      [3, "_INBOX.unread"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply }));
      await settled();
    }

    expect(failed).toEqual([]);
    expect(repliesTo(replies, "_INBOX.unread")).toEqual(["received", "nak"]);
    expect(logs.at(-1)).toBe(
      "consumer delivery: frame 1 could not be read to report it; it goes back to the stream"
    );
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 0 });
  });

  // A ledger of this repository never rejects, and a host may pass one of its own: a notice this
  // client cannot get an answer out of is no proof that anybody was told, so the frame stays.
  test("a give-up whose ledger rejects goes back to the stream", async () => {
    const { delivery, logs, notice, replies, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));
    notice.rejectWith = new Error("this host's own ledger threw");

    for (const [delivered, reply] of [
      [1, "_INBOX.reject-1"],
      [2, "_INBOX.reject-2"],
      [3, "_INBOX.reject"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply }));
      await settled();
    }

    expect(repliesTo(replies, "_INBOX.reject")).toEqual(["received", "nak"]);
    expect(logs.at(-1)).toBe(
      "consumer delivery: frame 1 could not be reported; it goes back to the stream"
    );
  });

  // The bound's client half: a frame whose give-up reached nobody comes back, and the stream may
  // hand it over hundreds of times before its consumer's own cap ends it. Rendering it again,
  // attempting a second notice and raising a second warning to the person on each lap tells
  // nobody anything the first lap did not, so the episode is worked once and the laps after it
  // go back in silence.
  test("a frame already given up on goes back in silence, not once per delivery", async () => {
    const { delivery, logs, notice, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));
    notice.told = false;

    for (const [delivered, reply] of [
      [1, "_INBOX.lap-1"],
      [2, "_INBOX.lap-2"],
      [3, "_INBOX.lap-3"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply }));
      await settled();
    }
    const afterTheEpisode = logs.length;

    for (const [delivered, reply] of [
      [4, "_INBOX.lap-4"],
      [5, "_INBOX.lap-5"],
    ] as const) {
      delivery.receive(relay({ seq: 1, delivered, reply }));
      await settled();
      expect(repliesTo(replies, reply)).toEqual(["received", "nak"]);
    }

    // One notice for the episode, and not one per lap, and no line for the laps after it.
    expect(failed).toEqual([{ session: SESSION, eventID: "evt-1" }]);
    expect(logs).toHaveLength(afterTheEpisode);
  });

  test("closing the session answers nak for every unsettled frame and any later one", () => {
    const { delivery, replies } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));

    delivery.close();
    delivery.receive(relay({ seq: 3, reply: "_INBOX.3" }));

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.3")).toEqual(["nak"]);
  });
});

describe("hand-off order", () => {
  test("frames go to the host in stream order, one at a time", async () => {
    const { delivery, handed, replies, state, settled } = setup();
    const first = Promise.withResolvers<void>();
    state.deliver = (frame) => (frame.streamSeq === 1 ? first.promise : undefined);

    delivery.receive(relay({ seq: 1 }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));
    delivery.receive(relay({ seq: 3 }));

    expect(seqs(handed)).toEqual([1]);
    // The later frames' receipts did not wait for the first.
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received"]);

    first.resolve();
    await settled();

    expect(seqs(handed)).toEqual([1, 2, 3]);
  });

  test("a released side question does not hold the next frame", async () => {
    const { delivery, handed, replies, state, settled } = setup();
    const modelCall = Promise.withResolvers<void>();
    state.deliver = async (frame) => {
      if (frame.streamSeq !== 1) return;
      frame.release();
      await modelCall.promise;
      frame.done();
    };

    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));

    expect(seqs(handed)).toEqual([1, 2]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received"]);
    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received"]);

    modelCall.resolve();
    await settled();

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
  });

  test("a redelivery of a held frame joins it: handed once, answered on both inboxes", () => {
    const { delivery, handed, replies } = setup();
    delivery.receive(relay({ seq: 4, reply: "_INBOX.first" }));

    delivery.receive(relay({ seq: 4, delivered: 2, reply: "_INBOX.again" }));
    handed[0]?.done();

    expect(seqs(handed)).toEqual([4]);
    expect(repliesTo(replies, "_INBOX.first")).toEqual(["received", "done"]);
    expect(repliesTo(replies, "_INBOX.again")).toEqual(["received", "done"]);
  });
});

describe("frames already written", () => {
  test("a frame the restored ledger holds is answered done and not handed over", async () => {
    const clock = { now: T0 };
    const persisted: { snapshot?: LedgerSnapshot } = {};
    const before = deliveryLedger(clock, persisted);
    const first = setup({ clock, ledger: before });
    first.state.deliver = async (frame) => {
      await before.recordDelivered({ event_id: `evt-${frame.streamSeq}` });
      frame.done();
    };
    first.delivery.receive(relay({ seq: 7, reply: "_INBOX.7" }));
    await first.settled();
    expect(repliesTo(first.replies, "_INBOX.7")).toEqual(["received", "done"]);

    // The process restarts; the relay restarts at the ack floor and sends the frame again.
    const after = deliveryLedger(clock);
    after.restore(persisted.snapshot ?? { ledger: [] });
    const second = setup({ clock, ledger: after });
    second.delivery.receive(relay({ seq: 7, delivered: 2, reply: "_INBOX.7b" }));
    second.delivery.receive(relay({ seq: 8, reply: "_INBOX.8" }));

    expect(repliesTo(second.replies, "_INBOX.7b")).toEqual(["received", "done"]);
    expect(seqs(second.handed)).toEqual([8]);
  });

  test("a copy written while it waited in the queue is answered done when its turn comes", async () => {
    const written = new Set<string>();
    const { delivery, handed, replies, state, settled } = setup({
      ledger: { delivered: (frame) => frame.event_id !== undefined && written.has(frame.event_id) },
    });
    const first = Promise.withResolvers<void>();
    state.deliver = (frame) => (frame.streamSeq === 1 ? first.promise : undefined);
    delivery.receive(relay({ seq: 1, eventID: "evt-same" }));
    delivery.receive(relay({ seq: 2, subject: AGENT, eventID: "evt-same", reply: "_INBOX.2" }));

    written.add("evt-same");
    handed[0]?.done();
    first.resolve();
    await settled();

    expect(seqs(handed)).toEqual([1]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received", "done"]);
  });
});

describe("follow filter", () => {
  test("a frame on a subject the session does not follow is answered done and dropped", () => {
    const { delivery, handed, replies } = setup();
    delivery.receive(relay({ seq: 1, subject: ISSUE, reply: "_INBOX.1" }));

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
    expect(handed).toEqual([]);
  });

  test("a frame stored before its topic was followed is dropped; one stored after is shown", () => {
    const { delivery, handed, replies, clock } = setup();
    clock.now = T0 + 10 * MINUTE;
    delivery.setFollowed([ISSUE_TOPIC]);

    delivery.receive(relay({ seq: 1, subject: ISSUE, storedAt: T0, reply: "_INBOX.1" }));
    delivery.receive(
      relay({ seq: 2, subject: ISSUE, storedAt: T0 + 10 * MINUTE - FOLLOW_CLOCK_ALLOWANCE_MS })
    );
    delivery.receive(relay({ seq: 3, subject: ISSUE, storedAt: T0 + 11 * MINUTE }));
    // The base subject of a `.>` follow is followed too.
    delivery.receive(
      relay({ seq: 4, subject: "notifications.dispatch.issue.INFRA-1", storedAt: T0 + 11 * MINUTE })
    );

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
    expect(seqs(handed)).toEqual([2, 3, 4]);
  });

  test("the agent subject is always taken, whenever it was stored", () => {
    const { delivery, handed } = setup();
    delivery.receive(relay({ seq: 1, subject: AGENT, storedAt: T0 - 60 * MINUTE }));

    expect(seqs(handed)).toEqual([1]);
  });

  test("a topic's follow time survives a restart, and an unfollow forgets it", () => {
    const { delivery, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    const snapshot = delivery.snapshot();

    clock.now = T0 + 30 * MINUTE;
    const restarted = setup({ clock });
    restarted.delivery.restore(snapshot);
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.receive(relay({ seq: 5, subject: ISSUE, storedAt: T0 + 5 * MINUTE }));
    expect(seqs(restarted.handed)).toEqual([5]);

    restarted.delivery.setFollowed([]);
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.receive(relay({ seq: 6, subject: ISSUE, storedAt: T0 + 6 * MINUTE }));
    expect(seqs(restarted.handed)).toEqual([5]);
  });
});

describe("late frames", () => {
  test("a redelivered frame, or one stored before the epoch began, is late", () => {
    const { delivery, handed, clock } = setup();
    delivery.receive(relay({ seq: 1, storedAt: T0 + MINUTE }));
    delivery.receive(relay({ seq: 2, storedAt: T0 + MINUTE, delivered: 2 }));
    delivery.receive(relay({ seq: 3, storedAt: T0 - MINUTE }));
    clock.now = T0 + 5 * MINUTE;
    delivery.beginEpoch();
    delivery.receive(relay({ seq: 4, storedAt: T0 + 2 * MINUTE }));
    delivery.receive(relay({ seq: 5, storedAt: T0 + 6 * MINUTE }));

    expect(handed.map((frame) => [frame.streamSeq, frame.late])).toEqual([
      [1, false],
      [2, true],
      [3, true],
      [4, true],
      [5, false],
    ]);
  });
});

describe("resume point", () => {
  test("below the lowest held frame, else the highest written; none before any", () => {
    const { delivery, handed } = setup();
    expect(delivery.resumePoint()).toBeUndefined();

    delivery.receive(relay({ seq: 5 }));
    delivery.receive(relay({ seq: 6 }));
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 4 });

    handed[1]?.done();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 4 });
    handed[0]?.done();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 6 });

    // A nak leaves the frame unwritten: the point stays below it.
    delivery.receive(relay({ seq: 9 }));
    handed[2]?.nak();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 8 });
  });

  test("a snapshot restores the point, and the registration carries it with the epoch", () => {
    const { delivery, handed, clock } = setup();
    delivery.receive(relay({ seq: 11 }));
    handed[0]?.done();
    const restarted = setup({ clock });

    restarted.delivery.restore(delivery.snapshot());

    expect(restarted.delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 11 });
    expect(restarted.delivery.registration()).toEqual({
      consumer: true,
      epoch: restarted.delivery.epoch().id,
      resume: { stream_id: STREAM, seq: 11 },
    });
    expect(setup().delivery.registration()).not.toHaveProperty("resume");
  });

  // The floor a registration answer leaves: the start the listener created the session's
  // consumer at, less one. A consumer delivers nothing below its own start, so a session that
  // has received nothing still holds a point, and comes back where that consumer began rather
  // than at the stream's end.
  const answerStarting = (consumerStart?: number): DeliveryAnswer => ({
    mode: "consumer",
    subject: deliverSubject(SESSION),
    stream_id: STREAM,
    gap: false,
    ...(consumerStart === undefined ? {} : { consumer_start: consumerStart }),
  });

  test("the consumer's start the answer carries is the floor the next registration resumes at", () => {
    const { delivery, persisted } = setup();

    delivery.answered(answerStarting(42));

    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 41 });
    expect(delivery.registration()).toEqual({
      consumer: true,
      epoch: delivery.epoch().id,
      resume: { stream_id: STREAM, seq: 41 },
    });
    // `answered` persists, so the floor is in the transcript at once.
    expect(persisted.at(-1)?.resume).toEqual({ stream_id: STREAM, seq: 41 });
  });

  test("a consumer start below the point the session already holds does not lower it", () => {
    const { delivery, handed } = setup();
    delivery.receive(relay({ seq: 11 }));
    handed[0]?.done();

    delivery.answered(answerStarting(5));

    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 11 });
  });

  test("an answer with no consumer start, from an earlier listener, changes nothing", () => {
    const { delivery } = setup();
    delivery.restore({ resume: { stream_id: STREAM, seq: 11 }, follows: {} });
    const fresh = setup();

    delivery.answered(answerStarting());
    fresh.delivery.answered(answerStarting());

    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 11 });
    expect(fresh.delivery.resumePoint()).toBeUndefined();
    expect(fresh.delivery.registration()).not.toHaveProperty("resume");
  });

  test("each epoch is a new random string", () => {
    const { delivery } = setup();
    const before = delivery.epoch().id;

    expect(delivery.beginEpoch().id).not.toBe(before);
    expect(delivery.registration().epoch).toBe(delivery.epoch().id);
  });
});

describe("gaps", () => {
  test("a gap the answer reports is told once, not again by the frame after it", () => {
    const { delivery, gaps } = setup();
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: STREAM,
      gap: true,
    });
    delivery.receive(relay({ seq: 1, gap: true }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["retention_gap"]);
  });

  test("a frame's gap is told when no answer reported it", () => {
    const { delivery, gaps } = setup();
    delivery.receive(relay({ seq: 1 }));
    delivery.receive(relay({ seq: 9, gap: true }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["retention_gap"]);
  });

  test("an answer naming another stream than the resume point's is told once", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 90 }, follows: {} });

    const consumer = {
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: STREAM,
      gap: false,
    } as const;
    delivery.answered(consumer);
    delivery.answered(consumer);

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
    expect(delivery.resumePoint()).toBeUndefined();
  });

  // The floor a registration answer leaves is the whole point of a session that has received
  // nothing (`answered`), and `written` holds it like a sequence the session wrote: a frame of
  // another stream says the stream that floor stands on was replaced, so what the session asked
  // to resume from is gone and the notice is true of it.
  test("a frame of another stream than the answer's floor is told once", () => {
    const { delivery, gaps } = setup();
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: STREAM,
      gap: false,
      consumer_start: 42,
    });
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 41 });

    delivery.receive(relay({ seq: 1, streamID: "stream-new" }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
  });

  // The listener starts the relay before it writes the registration's answer, and the relay's
  // frames come over NATS apart from that HTTP answer, so the first frame of the stream that
  // replaced the one the session resumed on can arrive before the answer naming it. The
  // replacement is still told, once.
  test("a frame from another stream than the resume point's, before the answer, is told once", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 9 }, follows: {} });

    delivery.registration();
    delivery.receive(relay({ seq: 1, streamID: "stream-new" }));
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: "stream-new",
      gap: false,
    });
    delivery.receive(relay({ seq: 2, streamID: "stream-new" }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
  });

  test("a frame from the resume point's own stream, before the answer, tells nothing", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 9 }, follows: {} });

    delivery.registration();
    delivery.receive(relay({ seq: 10, streamID: "stream-old" }));
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: "stream-old",
      gap: false,
    });

    expect(gaps).toEqual([]);
  });
});

describe("what the registration resumes at", () => {
  test("nothing before the session has taken a frame: the registration carries no resume point", () => {
    const { delivery } = setup();

    expect(delivery.registration()).not.toHaveProperty("resume");
  });

  test("the session's own resume point once it has taken a frame", () => {
    const { delivery, handed } = setup();
    delivery.receive(relay({ seq: 20 }));
    handed[0]?.done();

    expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 20 });
  });

  // A removal the stream kept while the session was away must not close a topic the session
  // followed again since (`closeReplayedRemoval`).
  test("a topic counts as followed before a frame only when the follow predates it", () => {
    const { delivery, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    clock.now = T0 + MINUTE;

    expect(delivery.followedBefore(ISSUE_TOPIC, T0 + MINUTE)).toBe(true);
    expect(delivery.followedBefore(ISSUE_TOPIC, T0 - FOLLOW_CLOCK_ALLOWANCE_MS)).toBe(true);
    expect(delivery.followedBefore(ISSUE_TOPIC, T0 - FOLLOW_CLOCK_ALLOWANCE_MS - 1)).toBe(false);
    expect(delivery.followedBefore("notifications.team.alpha", T0 + MINUTE)).toBe(false);
  });
});

describe("persisting the snapshot", () => {
  test("a change is written once; what a restore brought back is not written again", () => {
    const { delivery, handed, persisted, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    delivery.setFollowed([ISSUE_TOPIC]);
    delivery.receive(relay({ seq: 4 }));
    handed[0]?.done();
    delivery.persist();
    delivery.persist();

    expect(persisted).toEqual([
      { follows: { [ISSUE_TOPIC]: T0 } },
      { resume: { stream_id: STREAM, seq: 4 }, follows: { [ISSUE_TOPIC]: T0 } },
    ]);

    const restarted = setup({ clock });
    restarted.delivery.restore(delivery.snapshot());
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.persist();

    expect(restarted.persisted).toEqual([]);
  });

  test("the session's own agent subject is not one of the follow times", () => {
    const { delivery } = setup();
    delivery.setFollowed([AGENT, ISSUE_TOPIC]);

    expect(delivery.snapshot().follows).toEqual({ [ISSUE_TOPIC]: T0 });
  });
});

describe("one registration", () => {
  const UNCAPTURED = "notifications.team";
  const TEAM = "notifications.team.deploys";

  /** A relay that records what a registration stamped on it. */
  function fakeRelay() {
    const followed: string[][] = [];
    return {
      followed,
      setFollowed: (topics: Iterable<string>) => followed.push([...topics]),
      registration: () => ({ consumer: true, epoch: "epoch-1" }) as const,
    };
  }

  test("carries the topics the listener will store, and takes the answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];
    const sent: { topics: readonly string[]; epoch: string }[] = [];

    const result = await registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      // The bare base a stored row or a transcript brings back: the listener stores it for no
      // session and warns about it, so it never goes out (INFRA-771 review).
      followed: [TEAM, UNCAPTURED, "notifications.agent.ses_b"],
      outage: inboundUnavailable((why) => told.push(why)),
      send: (topics, delivery) => {
        sent.push({ topics, epoch: delivery.epoch });
        return Promise.resolve("registered");
      },
      answered: (answer) => answers.push(answer),
    });

    expect(sent).toEqual([{ topics: [AGENT, TEAM], epoch: "epoch-1" }]);
    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([result, answers, told]).toEqual(["registered", ["registered"], []]);
  });

  test("a session holding no relay subject registers nothing and is told once", async () => {
    const told: string[] = [];
    const outage = inboundUnavailable((why) => told.push(why));
    const sent: unknown[] = [];
    const register = () =>
      registerWithRelay({
        relay: undefined,
        ownAgentSubject: AGENT,
        followed: [TEAM],
        outage,
        send: (topics) => {
          sent.push(topics);
          return Promise.resolve("registered");
        },
        answered: () => undefined,
      });

    await expect(register()).rejects.toThrow("holds no relay subject");
    // The second outage tick says nothing more: the session was told once.
    await expect(register()).rejects.toThrow("holds no relay subject");
    expect(sent).toEqual([]);
    expect(told).toEqual([
      "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing",
    ]);
  });

  test("a refused registration tells the session once and takes no answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];

    await expect(
      registerWithRelay({
        relay,
        ownAgentSubject: AGENT,
        followed: [TEAM],
        outage: inboundUnavailable((why) => told.push(why)),
        send: () => Promise.reject(new Error("no_grant")),
        answered: (answer: string) => answers.push(answer),
      })
    ).rejects.toThrow("no_grant");

    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([answers, told]).toEqual([[], ["no_grant"]]);
  });
});
