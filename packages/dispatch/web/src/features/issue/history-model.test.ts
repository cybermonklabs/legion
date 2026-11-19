import { expect, test } from "bun:test";

import type { Actor, Artifact, Comment, DispatchEvent, MessageRead } from "../../api/types";
import { buildHistory, type HistoryDay, type HistoryEntry } from "./history-model";

/**
 * The issue's history as the panel draws it (dispatch://INFRA-1196 R15): every event that is no
 * row of the outline, newest first, by day, with one author's uploads within a minute as one
 * entry and the threads agents wrote among themselves drawn whole.
 */

const KEY = "CORE-1";
const PERSON: Actor = { id: "alice", kind: "user" };
const AGENT: Actor = { id: "planner", kind: "session" };

let seq = 0;
/** One event of the issue's log, its sequence the next, at `at`. */
function event(type: string, at: string, payload: unknown, actor: Actor = AGENT): DispatchEvent {
  seq += 1;
  return {
    actor,
    created_at: at,
    id: seq,
    issue_key: KEY,
    notify: false,
    payload,
    seq,
    type,
  } as DispatchEvent;
}

function file(id: string, kind: Artifact["kind"], name: string): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: PERSON,
    id,
    issue_key: KEY,
    kind,
    name,
    primary: false,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug: name.replace(".", "-"),
    versions: [],
  };
}

function comment(id: string, at: string, author: Actor, over: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author,
    body: `Comment ${id}.`,
    created_at: at,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: KEY,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...over,
  };
}

const flat = (days: readonly HistoryDay[]): HistoryEntry[] => days.flatMap((day) => day.entries);

test("the history leaves out every event of a row the outline draws, and a document's versions", () => {
  seq = 0;
  const spec = file("spec", "doc", "spec.md");
  const events = [
    event("issue.updated", "2026-11-15T09:00:00Z", { status: "in_progress" }),
    event("issue.claimed", "2026-11-15T09:01:00Z", {}),
    event("ask.opened", "2026-11-15T09:02:00Z", {}),
    event("ask.answered", "2026-11-15T09:03:00Z", {}),
    event("ask.resolved", "2026-11-15T09:04:00Z", {}),
    event("comment.created", "2026-11-15T09:05:00Z", { id: "c1" }, PERSON),
    event("comment.resolved", "2026-11-15T09:06:00Z", { id: "c1" }, PERSON),
    event("result.recorded", "2026-11-15T09:07:00Z", {}),
    event("child.status", "2026-11-15T09:08:00Z", {}),
    event("artifact.version", "2026-11-15T09:09:00Z", {
      artifact_id: spec.id,
      name: spec.name,
      version: { number: 2 },
    }),
    event("handoff.recorded", "2026-11-15T09:10:00Z", {}),
  ];
  const kept = flat(
    buildHistory({
      artifacts: [spec],
      comments: [comment("c1", "2026-11-15T09:05:00Z", PERSON)],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.type] : []));

  expect(kept).toEqual(["handoff.recorded", "issue.claimed", "issue.updated"]);
});

// An approval request is no row of the outline: the identity block draws its step. So where the
// history leaves a question's asking, answer and hand-back to the outline, it keeps a request's.
test("the history keeps the events of an approval request, which the outline draws no row for", () => {
  seq = 0;
  const events = [
    event("ask.opened", "2026-11-15T09:00:00Z", { id: "q1", kind: "question" }),
    event("ask.handed_back", "2026-11-15T09:01:00Z", { id: "q1", kind: "question" }),
    event("ask.opened", "2026-11-15T09:02:00Z", { id: "a1", kind: "approval" }),
    event("ask.handed_back", "2026-11-15T09:03:00Z", { id: "a1", kind: "approval" }),
  ];
  const kept = flat(
    buildHistory({
      artifacts: [],
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.seq] : []));

  expect(kept).toEqual([4, 3]);
});

test("one author's uploads within a minute are one entry that opens each file", () => {
  seq = 0;
  const shots = [1, 2, 3].map((at) => file(`shot-${at}`, "image", `shot-${at}.png`));
  const uploads = shots.map((shot, index) =>
    event("artifact.created", `2026-11-15T09:00:${10 + index * 20}Z`, { artifact: shot }, PERSON)
  );
  const later = event(
    "artifact.created",
    "2026-11-15T09:05:00Z",
    { artifact: file("late", "file", "late.txt") },
    PERSON
  );
  const entries = flat(
    buildHistory({
      artifacts: shots,
      comments: [],
      events: [...uploads, later],
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  );

  const runs = entries.flatMap((entry) =>
    entry.kind === "uploads" ? [entry.files.map((one) => one.name)] : []
  );
  expect(runs).toEqual([["late.txt"], ["shot-1.png", "shot-2.png", "shot-3.png"]]);
});

test("a thread agents wrote among themselves is one entry with its replies, until a person writes", () => {
  seq = 0;
  const root = comment("among", "2026-11-15T09:00:00Z", AGENT);
  const reply = comment(
    "answer",
    "2026-11-15T09:01:00Z",
    { id: "reviewer", kind: "session" },
    {
      answers: "among",
      reply_to: "among",
    }
  );
  const daemon: MessageRead = {
    message: {
      author: AGENT,
      body: "The daemon moved the issue.",
      broadcast_id: null,
      created_at: "2026-11-15T09:02:00Z",
      deliveries: [],
      id: "daemon",
      in_reply_to: null,
      issue_key: KEY,
      reply_block: null,
      target: null,
    },
    replies: [],
  };
  const history = (comments: Comment[]) =>
    flat(
      buildHistory({
        artifacts: [],
        comments,
        events: [],
        lastReadSeq: 0,
        messages: [daemon],
        today: "2026-11-15",
      })
    ).flatMap((entry) =>
      entry.kind === "thread"
        ? [{ replies: entry.replies.map((one) => one.id), root: entry.root.id }]
        : []
    );

  expect(history([root, reply])).toEqual([
    { replies: [], root: "daemon" },
    { replies: ["answer"], root: "among" },
  ]);
  // A person's reply makes the thread the conversation's: the outline draws it, and this does not.
  const person = comment("person", "2026-11-15T09:03:00Z", PERSON, {
    answers: "answer",
    reply_to: "among",
  });
  expect(history([root, reply, person])).toEqual([{ replies: [], root: "daemon" }]);
});

test("each day is headed, newest first, and the read mark falls between read and unread", () => {
  seq = 0;
  const events = [
    event("handoff.recorded", "2026-11-13T09:00:00Z", {}),
    event("handoff.recorded", "2026-11-14T09:00:00Z", {}),
    event("handoff.recorded", "2026-11-15T09:00:00Z", {}),
  ];
  const days = buildHistory({
    artifacts: [],
    comments: [],
    events,
    lastReadSeq: 2,
    messages: [],
    today: "2026-11-15",
  });

  expect(days.map((day) => [day.date, day.today])).toEqual([
    ["2026-11-15", true],
    ["2026-11-14", false],
    ["2026-11-13", false],
  ]);
  expect(days[1]?.label).toBe("Yesterday");
  // The line that says what is new closes the day of the oldest unread entry - today's list,
  // which is open - and never heads an earlier day, which the list folds.
  expect(days[0]?.entries.map((entry) => entry.kind)).toEqual(["event", "unread"]);
  expect(days[1]?.entries.map((entry) => entry.kind)).toEqual(["event"]);
});

test("the read mark falls inside a day where that day holds read and unread entries", () => {
  seq = 0;
  const events = [
    event("handoff.recorded", "2026-11-15T08:00:00Z", {}),
    event("handoff.recorded", "2026-11-15T09:00:00Z", {}),
  ];
  const days = buildHistory({
    artifacts: [],
    comments: [],
    events,
    lastReadSeq: 1,
    messages: [],
    today: "2026-11-15",
  });
  expect(days[0]?.entries.map((entry) => entry.kind)).toEqual(["event", "unread", "event"]);
});

// Threads come from the whole comment list and events from the pages of the log read so far, so
// a thread older than the oldest event read waits for its page: drawn before it, it would stand
// in a day of its own that counts that thread alone.
test("a thread that began before the oldest event read so far waits for its page", () => {
  seq = 0;
  const old = comment("old", "2026-11-10T09:00:00Z", AGENT);
  const recent = comment("recent", "2026-11-15T09:00:00Z", AGENT);
  const events = [event("handoff.recorded", "2026-11-14T09:00:00Z", {})];
  const roots = (loadedFrom: string | undefined) =>
    flat(
      buildHistory({
        artifacts: [],
        comments: [old, recent],
        events,
        lastReadSeq: 0,
        ...(loadedFrom === undefined ? {} : { loadedFrom }),
        messages: [],
        today: "2026-11-15",
      })
    ).flatMap((entry) => (entry.kind === "thread" ? [entry.root.id] : []));
  expect(roots("2026-11-14T09:00:00Z")).toEqual(["recent"]);
  // The whole log is in: nothing waits.
  expect(roots(undefined)).toEqual(["recent", "old"]);
});

// What happened to a thread agents wrote among themselves is no row's of the outline, so it is
// a line of the history (R15); the same events of a comment a person wrote are the outline's.
test("a delivery and a resolution of an agents' thread are lines of the history", () => {
  seq = 0;
  const among = comment("among", "2026-11-15T09:00:00Z", AGENT);
  const mine = comment("mine", "2026-11-15T09:00:00Z", PERSON);
  const events = [
    event("comment.created", "2026-11-15T09:00:00Z", { id: "among" }),
    event("comment.delivery", "2026-11-15T09:01:00Z", { comment_id: "among" }),
    event("comment.resolved", "2026-11-15T09:02:00Z", { id: "among" }),
    event("comment.delivery", "2026-11-15T09:03:00Z", { comment_id: "mine" }),
    event("comment.resolved", "2026-11-15T09:04:00Z", { id: "mine" }),
  ];
  const lines = flat(
    buildHistory({
      artifacts: [],
      comments: [among, mine],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.type] : []));
  expect(lines).toEqual(["comment.resolved", "comment.delivery"]);
});
