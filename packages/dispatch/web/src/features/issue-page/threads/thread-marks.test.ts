import { describe, expect, test } from "bun:test";
import type { AskThreadPage, CommentThreadPage, MessageThreadPage } from "@legion/contracts";

import type { EventPages } from "../../../api/event-feed";
import type {
  CommentWithTrees,
  DispatchEvent,
  MessageWithTrees,
  MyRepliesResponse,
  UserState,
} from "../../../api/types";
import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import eventsBody from "./fixtures/events.json";
import eventsPast200Body from "./fixtures/events-past-200.json";
import meStateBody from "./fixtures/me-state.json";
import meStateFreshBody from "./fixtures/me-state-fresh.json";
import messagesInBody from "./fixtures/messages-in.json";
import repliesUnreadBody from "./fixtures/replies-unread.json";
import { type MarkedRecord, type MarkReads, recordMark } from "./thread-marks";
import { type ThreadRecord, threadRecords } from "./thread-records";

// The answers the server gave the reader, alice, at the stack base (fixtures/README.md).
const KEY = "PDFD720CB-1";
const SECOND_KEY = "PDFD720CB-2";
const EVENTS = eventsBody as unknown as DispatchEvent[];
const EVENTS_PAST_200 = eventsPast200Body as unknown as DispatchEvent[];
const REPLIES = repliesUnreadBody as unknown as MyRepliesResponse;
const STATE = meStateBody as unknown as UserState;
const FRESH_STATE = meStateFreshBody as unknown as UserState;

const RECORDS: readonly ThreadRecord[] = [
  ...((commentsInBody as unknown as CommentThreadPage).threads ?? []),
  ...((commentsInBeforeBody as unknown as CommentThreadPage).threads ?? []),
  ...((messagesInBody as unknown as MessageThreadPage).threads ?? []),
  ...((asksThreadBody as unknown as AskThreadPage).threads ?? []),
].flatMap(threadRecords);

/** The record whose id begins with `prefix`, as the fixtures' README names them. */
function record(prefix: string): ThreadRecord {
  const found = RECORDS.find((one) => one.id.startsWith(prefix));
  if (found === undefined) throw new Error(`No page of the fixtures holds the record ${prefix}.`);
  return found;
}

function page(events: readonly DispatchEvent[]): EventPages {
  return { pageParams: [null], pages: [[...events]] };
}

/** What Threads reads for the reader on the seeded issue, with `change` over it. */
function reads(change: Partial<MarkReads> = {}): MarkReads {
  return {
    events: page(EVENTS),
    issueKey: KEY,
    issueOpen: true,
    reader: "alice",
    replies: REPLIES,
    state: STATE,
    ...change,
  };
}

/** A copy of the reader's state with the seeded issue's last read moved to `seq`. */
function lastReadAt(seq: number): UserState {
  const issue = STATE[KEY];
  if (issue === undefined) throw new Error("The state read holds no state for the seeded issue.");
  return { ...STATE, [KEY]: { ...issue, last_read_seq: seq } };
}

/** A copy of the reader's unread threads with the `read_through` of `thread`'s row moved to
 *  `at`. */
function readThrough(thread: string, at: string): MyRepliesResponse {
  return {
    ...REPLIES,
    rows: (REPLIES.rows ?? []).map((row) =>
      row.thread_id === thread ? { ...row, read_through: at } : row
    ),
  };
}

test("a record the reader wrote in counts the writes of others past its thread's read_through", () => {
  expect(recordMark(record("73914aae"), reads())).toBe("2");
  expect(recordMark(record("2dc5b959"), reads())).toBe("2");
  expect(recordMark(record("c1d04870"), reads())).toBe("2");
});

test("a record the reader wrote in draws no mark where me/replies lists no row for its thread, whatever the events say", () => {
  // The planner asked at seq 110, past the reader's last read at 81, and the reader answered.
  expect(recordMark(record("267776fd"), reads())).toBeUndefined();
  expect(recordMark(record("1d7c4317"), reads())).toBeUndefined();
});

test("the reader's own writes past the read_through are never counted", () => {
  // The row's read_through is moved back to the root's time, before the planner's first reply,
  // so the reader's own reply after it and the planner's three replies are past it.
  const root = record("2dc5b959");
  const replies = readThrough(root.thread, root.root.created_at);
  expect(recordMark(root, reads({ replies }))).toBe("3");
});

test("the reader's own write events past the last read are never counted", () => {
  // The last read is moved to 0: the open ask's events are the planner's question, the reader's
  // reply and the planner's reply.
  expect(recordMark(record("4b7efdcf"), reads({ state: lastReadAt(0) }))).toBe("2");
});

test("a record that a reply with a quote begins counts only its own writes, by its post's read_through", () => {
  // The post's row counts 3 writes past its read_through: the reader's colleague's reply is the
  // post's, and the planner's quoting reply and the reviewer's answer to it are the other
  // record's.
  expect(recordMark(record("18ddeb81"), reads())).toBe("1");
  expect(recordMark(record("b6099daa"), reads())).toBe("2");
  expect(recordMark(record("0d7bfef6"), reads())).toBeUndefined();
  expect(recordMark(record("b788d337"), reads())).toBe("1");
});

test("the open ask that waits on the reader counts by the reader's last read of the issue", () => {
  // The reader replied in it, but me/replies leaves it out. The planner's reply at seq 83 is past
  // the last read at 81.
  expect(recordMark(record("4b7efdcf"), reads())).toBe("1");
});

test("on a closed issue the open ask is a thread of me/replies again", () => {
  expect(recordMark(record("4b7efdcf"), reads({ issueOpen: false }))).toBeUndefined();
});

test("a record the reader did not write in counts its write events past the reader's last read", () => {
  expect(recordMark(record("b3ab4dfe"), reads())).toBe("1");
  // The colleague's message is at seq 66 and the reviewer's reply at seq 82.
  expect(recordMark(record("882e100d"), reads())).toBe("1");
  // The planner's note is at seq 39.
  expect(recordMark(record("99945b76"), reads())).toBeUndefined();
});

test("with no state for the issue, every write event of a record the reader did not write in counts", () => {
  // The fresh person's state read, which holds no issue, stands for the reader's here.
  expect(recordMark(record("882e100d"), reads({ state: FRESH_STATE }))).toBe("2");
  expect(recordMark(record("b3ab4dfe"), reads({ state: FRESH_STATE }))).toBe("1");
});

describe("200 events past the last read", () => {
  const second = { events: page(EVENTS_PAST_200), issueKey: SECOND_KEY };
  const answered = EVENTS_PAST_200.find((event) => event.type === "message.answered");
  const comments = EVENTS_PAST_200.filter((event) => event.type === "comment.created");
  if (answered === undefined) throw new Error("The second issue's events hold no reply.");
  const reply = answered.payload as MessageWithTrees;
  const posts = comments.map((event) => event.payload as CommentWithTrees);
  // The records of the second issue's two threads are read from the events' own payloads: no
  // page of the fixtures holds them.
  const note: MarkedRecord = {
    thread: reply.in_reply_to ?? reply.id,
    threadWriters: [reply.author],
    waitsOn: undefined,
    writes: [{ at: reply.created_at, author: reply.author, id: reply.id }],
  };
  const thread: MarkedRecord = {
    thread: posts[0]?.reply_to ?? "",
    threadWriters: posts.map((post) => post.author),
    waitsOn: undefined,
    writes: posts.map((post) => ({ at: post.created_at, author: post.author, id: post.id })),
  };

  test("the count of each record carries + after it", () => {
    expect(recordMark(note, reads(second))).toBe("1+");
    expect(recordMark(thread, reads(second))).toBe("199+");
  });

  test("a page the feed's prepends grew keeps its +", () => {
    // A copy of the newest comment event, one seq on, stands for an event the feed prepended.
    const newest = comments[0];
    if (newest === undefined) throw new Error("The second issue's events hold no comment.");
    const grown = page([
      { ...newest, seq: (EVENTS_PAST_200[0]?.seq ?? 0) + 1 },
      ...EVENTS_PAST_200,
    ]);
    expect(recordMark(note, reads({ ...second, events: grown }))).toBe("1+");
  });
});

test("a page that holds the issue's first event draws no +, however many events the feed's prepends put above it", () => {
  // 88 copies of the reader's newest comment event, each one seq on, grow the seeded issue's 112
  // events to 200. With no state for the issue every one of them is past the last read.
  const newest = EVENTS[0];
  if (newest === undefined) throw new Error("The seeded issue's events are empty.");
  const prepended = Array.from({ length: 88 }, (_, at) => ({ ...newest, seq: 200 - at }));
  const grown = page([...prepended, ...EVENTS]);
  expect(grown.pages[0]).toHaveLength(200);
  expect(recordMark(record("b3ab4dfe"), reads({ events: grown, state: FRESH_STATE }))).toBe("1");
});

test("an edit after the read_through is no write", () => {
  // A copy of the reader's remark whose two replies were edited after the row's read_through,
  // which is moved past the replies' own times.
  const row = (commentsInBody as unknown as CommentThreadPage).threads?.find((one) =>
    one.comment.id.startsWith("73914aae")
  );
  if (row === undefined) throw new Error("The comments' first page holds no remark.");
  const edited = {
    ...row,
    replies: (row.replies ?? []).map((reply) => ({
      ...reply,
      edited_at: "2026-11-26T11:40:00.000000Z",
    })),
  };
  const [remark] = threadRecords(edited);
  if (remark === undefined) throw new Error("The remark gives no record.");
  const replies = readThrough(remark.thread, "2026-11-26T11:39:00.000000Z");
  expect(recordMark(remark, reads({ replies }))).toBeUndefined();
});

describe("each event type the events answers hold, and those the seed writes none of", () => {
  // The fresh person, who wrote in no thread and holds no state for the issue, reads each event
  // alone, so each event that is a write of an entry of the record counts once.
  const fresh = {
    reader: "carol",
    replies: { ...REPLIES, rows: [], total: 0 },
    state: FRESH_STATE,
  };
  const of = (type: DispatchEvent["type"], seq: number): DispatchEvent => {
    const event = EVENTS.find((one) => one.type === type && one.seq === seq);
    if (event === undefined) throw new Error(`The events hold no ${type} at seq ${seq}.`);
    return event;
  };
  /** A copy of `event` under `type`, for a type the seed writes none of: the README's table says
   *  each names the same entry as the type it is copied from. */
  const as = (event: DispatchEvent, type: string) => ({ ...event, type }) as DispatchEvent;
  const cases: readonly [string, DispatchEvent, string, string | undefined][] = [
    ["issue.created", of("issue.created", 1), "b5979723", undefined],
    ["comment.created", of("comment.created", 112), "b5979723", "1"],
    ["comment.deleted", of("comment.deleted", 8), "c36ac28f", undefined],
    ["message.created", of("message.created", 107), "1d7c4317", "1"],
    ["message.answered", of("message.answered", 99), "b788d337", "1"],
    ["message.delivery", of("message.delivery", 108), "1d7c4317", undefined],
    ["ask.opened", of("ask.opened", 110), "267776fd", "1"],
    ["ask.answered", of("ask.answered", 111), "267776fd", "1"],
    ["comment.answered", as(of("comment.created", 112), "comment.answered"), "b5979723", "1"],
    ["comment.edited", as(of("comment.created", 112), "comment.edited"), "b5979723", undefined],
    ["message.edited", as(of("message.created", 107), "message.edited"), "1d7c4317", undefined],
    [
      "message.accepted",
      as(of("message.delivery", 108), "message.accepted"),
      "1d7c4317",
      undefined,
    ],
    ["ask.resolved", as(of("ask.answered", 111), "ask.resolved"), "267776fd", "1"],
  ];
  const titled = cases.map(
    ([type, event, id, mark]): readonly [string, DispatchEvent, string, string | undefined] => [
      mark === undefined
        ? `${type} counts no write`
        : `${type} counts one write of the entry it names`,
      event,
      id,
      mark,
    ]
  );
  test.each(titled)("%s", (_, event, id, mark) => {
    expect(recordMark(record(id), reads({ ...fresh, events: page([event]) }))).toBe(mark);
  });
});
