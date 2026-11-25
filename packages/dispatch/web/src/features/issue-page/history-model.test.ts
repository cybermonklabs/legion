import { expect, test } from "bun:test";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import type { Actor, Artifact, Comment, DispatchEvent, MessageRead } from "../../api/types";
import { dayLabel } from "../activity/activity-model";
import { buildIssuePath, buildReferencePath, parseNodeReference } from "../refs/routes";
import {
  buildHistory,
  HISTORY_PAGE,
  type HistoryBlock,
  type HistoryDay,
  type HistoryEntry,
  type HistoryInput,
  historyBlocks,
  historyDownTo,
  placeUnder,
  turnPlace,
} from "./history-model";
import { CONTROL_LABELS, DECLARED_WORDS, wordsIn } from "./page-words";

/**
 * The issue's history as the panel draws it: every event that is no row of the outline, newest
 * first, by day, with a run of one author's uploads - the newest and those made at most a minute
 * before it - as one entry and the threads the history holds drawn whole.
 */

const KEY = "CORE-1";
const PERSON: Actor = { id: "alice", kind: "user" };
const AGENT: Actor = { id: "planner", kind: "session" };
/** The Legion daemon of the issue's project, as it writes. */
const DAEMON: Actor = { id: "legion-daemon:CORE", kind: "session" };
const REVIEWER: Actor = { id: "reviewer", kind: "session" };

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
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author,
    body: `Comment ${id}.`,
    created_at: at,
    edited_at: null,
    id,
    issue_key: KEY,
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

/** A message of the issue by `author`, at `at`. */
function message(
  id: string,
  at: string,
  author: Actor,
  over: Partial<MessageRead["message"]> = {}
): MessageRead["message"] {
  return {
    author,
    body: `Message ${id}.`,
    broadcast_id: null,
    created_at: at,
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: KEY,
    reply_block: null,
    target: null,
    ...over,
  };
}

/** Where a comment's head is tied to the words of a document. */
const TIED = {
  artifact_id: "spec",
  block_id: null,
  mark_id: "mark-1",
  orphaned: false,
  quote: "The words it is tied to.",
  version: 1,
};

/** The thread entries of `entries`, newest first: each one's root turn and its replies. */
function threadsIn(entries: readonly HistoryEntry[]): { replies: string[]; root: string }[] {
  return entries.flatMap((entry) =>
    entry.kind === "thread"
      ? [{ replies: entry.replies.map((one) => one.id), root: entry.root.id }]
      : []
  );
}

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
      asks: [],
      blocks: new Map(),
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
      asks: [],
      blocks: new Map(),
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.seq] : []));

  expect(kept).toEqual([4, 3]);
});

// A follower's event names its ask and not the ask's kind, so the history reads that kind from
// the issue's asks: an approval request's followers added and removed are its lines, as its other
// events are, and those of a question, or of an ask the issue's asks do not hold, are not.
test("the history keeps an approval request's followers added and removed", () => {
  seq = 0;
  const follower = (type: string, at: string, askId: string) =>
    event(type, at, { ask_id: askId, by: AGENT, session_id: "reviewer" });
  const events = [
    event("ask.opened", "2026-11-15T09:00:00Z", { id: "a1", kind: "approval" }),
    follower("ask.follower_added", "2026-11-15T09:01:00Z", "a1"),
    follower("ask.follower_added", "2026-11-15T09:02:00Z", "q1"),
    follower("ask.follower_added", "2026-11-15T09:03:00Z", "unheld"),
    follower("ask.follower_removed", "2026-11-15T09:04:00Z", "a1"),
  ];
  const kept = flat(
    buildHistory({
      artifacts: [],
      asks: [
        { id: "a1", kind: "approval" },
        { id: "q1", kind: "question" },
      ],
      blocks: new Map(),
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [[entry.event.seq, entry.event.type]] : []));

  expect(kept, "the approval request's followers are not lines of the history").toEqual([
    [5, "ask.follower_removed"],
    [2, "ask.follower_added"],
    [1, "ask.opened"],
  ]);
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
  const history = (events: typeof uploads) =>
    flat(
      buildHistory({
        artifacts: shots,
        asks: [],
        blocks: new Map(),
        comments: [],
        events,
        lastReadSeq: 0,
        messages: [],
        today: "2026-11-15",
      })
    );
  const entries = history([...uploads, later]);

  // Newest first, as the history lists every entry.
  const runs = entries.flatMap((entry) =>
    entry.kind === "uploads" ? [entry.files.map((one) => one.name)] : []
  );
  expect(runs).toEqual([["late.txt"], ["shot-3.png", "shot-2.png", "shot-1.png"]]);
  // A run named by its newest upload keeps its name when an older page adds its oldest one.
  const runOf = (drawn: typeof entries) =>
    drawn.find((entry) => entry.kind === "uploads" && entry.files.length > 1)?.id;
  expect(
    runOf(history([...uploads.slice(1), later])),
    "an older page that extends a run of uploads renamed the run"
  ).toBe(runOf(entries));
});

/** An upload of the image `name` by `actor` at 2026-11-15 `time` UTC. */
function shot(time: string, name: string, actor: Actor = PERSON): DispatchEvent {
  return event(
    "artifact.created",
    `2026-11-15T${time}Z`,
    { artifact: file(name, "image", `${name}.png`) },
    actor
  );
}

/** The runs of uploads the history of `events` draws, newest first: each run's id and its
 *  files' uploads, newest first. */
function uploadRuns(events: readonly DispatchEvent[]): { files: number[]; id: string }[] {
  return flat(
    buildHistory({
      artifacts: [],
      asks: [],
      blocks: new Map(),
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) =>
    entry.kind === "uploads" ? [{ files: entry.files.map((one) => one.id), id: entry.id }] : []
  );
}

// A run's minute is counted back from its newest upload, the uploads walked newest first, so an
// older page only lengthens the oldest run drawn or adds runs under it. Counted from a run's
// first upload, it split runs: of four uploads at :00, :50, 1:20 and 2:00, a first page holding
// the last three drew a run of :50 and 1:20 under one of 2:00, and the older page moved 1:20 up
// into the run of 2:00. Counted from the upload before each, it drew all four as one entry two
// minutes long.
test("an older page that extends the oldest run of uploads moves no file of a run drawn", () => {
  seq = 999;
  const uploads = [
    shot("09:00:00", "first"),
    shot("09:00:50", "second"),
    shot("09:01:20", "third"),
    shot("09:02:00", "fourth"),
  ];
  const firstPage = uploadRuns(uploads.slice(1));
  const whole = uploadRuns(uploads);

  expect(firstPage, "the first page's runs are not counted back from each run's newest").toEqual([
    { files: [1003, 1002], id: "uploads:1003" },
    { files: [1001], id: "uploads:1001" },
  ]);
  for (const run of firstPage) {
    const holders = new Set(run.files.map((id) => whole.find((one) => one.files.includes(id))?.id));
    expect([...holders], `the older page moved a file of ${run.id} to another run`).toEqual([
      run.id,
    ]);
  }
  expect(whole, "the older page did more than extend the oldest run").toEqual([
    { files: [1003, 1002], id: "uploads:1003" },
    { files: [1001, 1000], id: "uploads:1001" },
  ]);
});

// The live feed adds a newer upload above every entry. Within a minute of the newest run's newest
// upload it joins that run, and the run's uploads more than a minute older than it are split off
// into a run of their own, as a run's minute is counted back from its newest upload; no file of
// any run under them moves. Counted from the upload before each, a newer upload lengthened the
// run however long before the run began.
test("a newer upload within a minute of the newest run's newest upload joins it and splits off the run's uploads more than a minute older than it", () => {
  seq = 0;
  const drawn = [
    shot("09:00:00", "early"),
    shot("09:00:20", "agents", AGENT),
    shot("09:00:40", "first"),
    shot("09:01:20", "second"),
  ];
  const before = uploadRuns(drawn);
  expect(before.map((run) => run.files)).toEqual([[4, 3], [2], [1]]);

  const after = uploadRuns([...drawn, shot("09:02:00", "third")]);

  expect(after[0]?.files, "the newer upload did not join the newest run's newest upload").toEqual([
    5, 4,
  ]);
  expect(
    after[1]?.files,
    "the newest run's upload 80 s older than the newer one was not split off into a run of its own"
  ).toEqual([3]);
  expect(after.slice(2), "a newer upload moved a file of a run under the newest").toEqual(
    before.slice(1)
  );
});

// No run spans more than a minute: of three uploads 40 s apart, the oldest is 80 s older than
// the newest, so it begins a run of its own under the newest two. Counted from the upload before
// each, the three were one entry, and one entry could span any length of time.
test("three uploads 40 s apart are two entries, the newest two together", () => {
  seq = 0;
  expect(
    uploadRuns([shot("09:00:00", "first"), shot("09:00:40", "second"), shot("09:01:20", "third")]),
    "the upload 80 s older than the newest is in the newest run"
  ).toEqual([
    { files: [3, 2], id: "uploads:3" },
    { files: [1], id: "uploads:1" },
  ]);
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
      author: DAEMON,
      body: "The daemon moved the issue.",
      broadcast_id: null,
      created_at: "2026-11-15T09:02:00Z",
      deliveries: [],
      ...messageCorrectionFields(),
      id: "daemon",
      in_reply_to: null,
      issue_key: KEY,
      reply_block: null,
      target: null,
    },
    replies: [],
  };
  const history = (comments: Comment[], asks: HistoryInput["asks"] = []) =>
    flat(
      buildHistory({
        artifacts: [],
        asks,
        blocks: new Map(),
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
  // A comment under an approval request names the ask, where the server's climb to a thread's
  // head stops, so every comment naming it is one thread, at its earliest, whatever `reply_to`
  // says (`agents-only.ts`): agents' turns there are one entry, not one entry each.
  const ask = { id: "approval-1", kind: "approval" as const };
  const later = comment("approve-later", "2026-11-15T08:30:00Z", AGENT, { ask_id: ask.id });
  const earlier = comment("approve-first", "2026-11-15T08:00:00Z", AGENT, { ask_id: ask.id });
  expect(history([later, earlier], [ask])).toEqual([
    { replies: [], root: "daemon" },
    { replies: ["approve-later"], root: "approve-first" },
  ]);
});

// A thread the Legion daemon began is the history's whole, whoever writes in it: a person's reply
// leaves it one entry, with that reply among its turns, and the outline draws none of it.
test("a thread the Legion daemon began is one entry, and stays one when a person replies", () => {
  seq = 0;
  const root = message("daemon", "2026-11-15T09:00:00Z", DAEMON);
  const agent = message("planner-reply", "2026-11-15T09:01:00Z", AGENT, { in_reply_to: "daemon" });
  const person = message("person-reply", "2026-11-15T09:02:00Z", PERSON, {
    in_reply_to: "daemon",
  });

  expect(threadsIn(flat(daysOf([], 0, [], [{ message: root, replies: [agent] }])))).toEqual([
    { replies: ["planner-reply"], root: "daemon" },
  ]);
  expect(threadsIn(flat(daysOf([], 0, [], [{ message: root, replies: [agent, person] }])))).toEqual(
    [{ replies: ["planner-reply", "person-reply"], root: "daemon" }]
  );
});

// The history holds no message thread but the daemon's: an agent's note to the issue's people and
// a thread between agents are Threads', and the outline draws them until Threads does. Nor does it
// hold a comment thread whose head is tied to the words of a document, which the margin draws,
// though agents alone wrote in it. An agents' comment thread tied to no text is the one entry.
test("an agent's note, a thread between agents and an agents' comment thread tied to text are no entry", () => {
  seq = 0;
  const note = message("note", "2026-11-15T09:00:00Z", AGENT);
  const between = message("asked", "2026-11-15T09:01:00Z", AGENT, {
    target: "role:legion-p-core-1-reviewer",
  });
  const answer = message("answered", "2026-11-15T09:02:00Z", REVIEWER, { in_reply_to: "asked" });
  const tied = comment("tied", "2026-11-15T09:03:00Z", AGENT, { anchor: TIED });
  const tiedReply = comment("tied-reply", "2026-11-15T09:04:00Z", REVIEWER, {
    answers: "tied",
    reply_to: "tied",
  });
  const post = comment("post", "2026-11-15T09:05:00Z", AGENT);

  expect(
    threadsIn(
      flat(
        daysOf(
          [],
          0,
          [tied, tiedReply, post],
          [
            { message: note, replies: [] },
            { message: between, replies: [answer] },
          ]
        )
      )
    )
  ).toEqual([{ replies: [], root: "post" }]);
});

// A thread the history holds is drawn from its comments and messages, each turn whole with its
// marks, and the outline draws every other thread from the same reads: so no creation, edit,
// delete, answer or acceptance of a post is a line, whichever thread the post is of.
test("no event that creates, edits, deletes or answers a post is a line, whichever thread it is of", () => {
  seq = 0;
  const daemon = message("daemon", "2026-11-15T09:00:00Z", DAEMON);
  const daemonReply = message("daemon-reply", "2026-11-15T09:01:00Z", PERSON, {
    in_reply_to: "daemon",
  });
  const note = message("note", "2026-11-15T09:02:00Z", AGENT);
  const post = comment("post", "2026-11-15T09:03:00Z", AGENT);
  const mine = comment("mine", "2026-11-15T09:04:00Z", PERSON);
  const at = "2026-11-15T09:10:00Z";
  const events = [
    ...["post", "mine"].flatMap((id) =>
      ["comment.created", "comment.edited", "comment.deleted", "comment.answered"].map((type) =>
        event(type, at, { id })
      )
    ),
    ...["daemon", "daemon-reply", "note"].flatMap((id) => [
      ...["message.created", "message.edited", "message.deleted", "message.answered"].map((type) =>
        event(type, at, { id })
      ),
      event("message.accepted", at, { message_id: id }),
    ]),
    event("handoff.recorded", at, {}),
  ];

  const lines = flat(
    daysOf(
      events,
      0,
      [post, mine],
      [
        { message: daemon, replies: [daemonReply] },
        { message: note, replies: [] },
      ]
    )
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.type] : []));
  expect(lines).toEqual(["handoff.recorded"]);
});

// What happened to a post after it was written is a line where the history holds its thread, and
// the outline's where it draws the thread: a delivery of the daemon's message is a line, and one
// of an agent's note, which the outline draws, is not.
test("a delivery of a message is a line in a thread the daemon began, and none of an agent's note", () => {
  seq = 0;
  const daemon = message("daemon", "2026-11-15T09:00:00Z", DAEMON);
  const note = message("note", "2026-11-15T09:01:00Z", AGENT);
  const delivered = (id: string) =>
    event("message.delivery", "2026-11-15T09:02:00Z", {
      message_id: id,
      session_id: "tester-7",
      state: "sent",
      target: "session:tester-7",
    });

  const lines = flat(
    daysOf(
      [delivered("daemon"), delivered("note")],
      0,
      [],
      [
        { message: daemon, replies: [] },
        { message: note, replies: [] },
      ]
    )
  ).flatMap((entry) =>
    entry.kind === "event" && entry.event.type === "message.delivery"
      ? [entry.event.payload.message_id]
      : []
  );
  expect(lines).toEqual(["daemon"]);
});

/** `hour`:`minute` of the reader's own `day` of November 2026, as the log writes a moment. The
 *  history heads the reader's local days, so a case that names the day an event falls on builds
 *  the event here: a UTC instant falls on another local day in a zone far from UTC. */
function november(day: number, hour: number, minute = 0): string {
  return new Date(2026, 10, day, hour, minute).toISOString();
}

test("each day is headed, newest first, and the read mark falls between read and unread", () => {
  seq = 0;
  const events = [
    event("handoff.recorded", november(13, 9), {}),
    event("handoff.recorded", november(14, 9), {}),
    event("handoff.recorded", november(15, 9), {}),
  ];
  const days = buildHistory({
    artifacts: [],
    asks: [],
    blocks: new Map(),
    comments: [],
    events,
    lastReadSeq: 2,
    messages: [],
    today: "2026-11-15",
  });

  expect(days.map((day) => day.date)).toEqual(["2026-11-15", "2026-11-14", "2026-11-13"]);
  // Today and Yesterday are the page's own words, each a word it declares; an
  // older day is its date, written out in the reader's locale.
  expect(
    wordsIn(`${days[0]?.label} ${days[1]?.label}`).filter((word) => !DECLARED_WORDS.includes(word)),
    "a day's heading draws a word the page does not declare"
  ).toEqual([]);
  expect(days.map((day) => day.label)).toEqual([
    CONTROL_LABELS.history_today,
    CONTROL_LABELS.history_yesterday,
    dayLabel("2026-11-13", "2026-11-15"),
  ]);
  // The line that says what is new closes the day of the oldest unread entry, and never heads
  // the next day under that day's own line.
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
    asks: [],
    blocks: new Map(),
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
// among entries the log has not read.
test("a thread that began before the oldest event read so far waits for its page", () => {
  seq = 0;
  const old = comment("old", "2026-11-10T09:00:00Z", AGENT);
  const recent = comment("recent", "2026-11-15T09:00:00Z", AGENT);
  const events = [event("handoff.recorded", "2026-11-14T09:00:00Z", {})];
  const roots = (loadedFrom: string | undefined) =>
    flat(
      buildHistory({
        artifacts: [],
        asks: [],
        blocks: new Map(),
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

/** `count` claims of the issue, oldest first, one a minute up to 2026-11-15 11:59 UTC, each
 *  later in the log than the one before. */
function claims(count: number): DispatchEvent[] {
  return Array.from({ length: count }, (_, index) =>
    event("issue.claimed", new Date(Date.UTC(2026, 10, 15, 12, index - count)).toISOString(), {})
  );
}

/** The history of `events`, `comments` and `messages`, the reader's mark at `lastReadSeq`. */
function daysOf(
  events: readonly DispatchEvent[],
  lastReadSeq = 0,
  comments: readonly Comment[] = [],
  messages: readonly MessageRead[] = []
): HistoryDay[] {
  return buildHistory({
    artifacts: [],
    asks: [],
    blocks: new Map(),
    comments,
    events,
    lastReadSeq,
    messages,
    today: "2026-11-15",
  });
}

/** How many entries `days` draws, the line that says what is new not counted. */
function entriesIn(days: readonly HistoryDay[]): number {
  return flat(days).filter((entry) => entry.kind !== "unread").length;
}

// The history draws its first 20 entries, and each press of its control the next 20, where a
// thread with every turn of it is one entry, a run of uploads one entry, and the line that says
// what is new none. The control says how many entries are left to draw.
test("the history is drawn 20 entries at a time, a thread and a run of uploads one entry each", () => {
  seq = 0;
  const thread = comment("post", "2026-11-15T12:00:30Z", AGENT);
  const replies = ["first", "second"].map((id, index) =>
    comment(id, `2026-11-15T12:00:4${index}Z`, REVIEWER, { answers: "post", reply_to: "post" })
  );
  const events = [
    ...claims(41),
    ...["09:00:10", "09:00:20", "09:00:30"].map((time, index) => shot(time, `run-${index}`)),
  ];
  // Read through the claim of 11:50, so the line that says what is new stands among the first 20.
  const days = daysOf(events, 32, [thread, ...replies]);
  expect(entriesIn(days), "the history holds 41 claims, a thread and a run").toBe(43);

  const first = historyDownTo(days, placeUnder(days, undefined, HISTORY_PAGE));
  expect(entriesIn(first.days)).toBe(HISTORY_PAGE);
  expect(
    flat(first.days).some((entry) => entry.kind === "unread"),
    "the line that says what is new is not drawn among the first entries"
  ).toBe(true);
  expect(flat(first.days)[0]?.kind, "the thread is not the newest entry").toBe("thread");
  expect(first.left).toBe(23);

  const second = historyDownTo(
    days,
    placeUnder(days, placeUnder(days, undefined, HISTORY_PAGE), HISTORY_PAGE)
  );
  expect(entriesIn(second.days)).toBe(2 * HISTORY_PAGE);
  expect(second.left).toBe(3);

  const end = placeUnder(days, placeUnder(days, undefined, 2 * HISTORY_PAGE), HISTORY_PAGE);
  const whole = historyDownTo(days, end);
  expect(entriesIn(whole.days)).toBe(43);
  expect(whole.left).toBe(0);
  expect(flat(whole.days).at(-1)?.kind, "the run of three uploads is not the last entry").toBe(
    "uploads"
  );
  expect(placeUnder(days, end, HISTORY_PAGE), "no entry is left under the last one").toEqual(end);
});

// The history is drawn down to the place of the last entry drawn: an entry that arrives above
// it is drawn too, and takes none of the entries drawn out from under the reader.
test("an entry that arrives above the entries drawn is drawn, and every entry drawn stays", () => {
  seq = 0;
  const events = claims(30);
  const end = placeUnder(daysOf(events), undefined, HISTORY_PAGE);
  const arrived = event("issue.claimed", "2026-11-15T12:00:00Z", {});

  const drawn = historyDownTo(daysOf([...events, arrived]), end);
  expect(entriesIn(drawn.days)).toBe(HISTORY_PAGE + 1);
  expect(flat(drawn.days)[0]?.id).toBe(`event:${arrived.id}`);
  expect(drawn.left).toBe(10);
});

// The line that says what is new stands between two entries, so it is drawn only where the entry
// under it is drawn too: under the last entry drawn, it would say nothing of what follows.
test("the line that says what is new is drawn only with the entry under it", () => {
  seq = 0;
  const events = claims(25);
  // Read through the fifth claim: the line stands under the twentieth entry.
  const days = daysOf(events, 5);
  const first = historyDownTo(days, placeUnder(days, undefined, HISTORY_PAGE));
  expect(flat(first.days).map((entry) => entry.kind)).not.toContain("unread");
  const next = historyDownTo(days, placeUnder(days, undefined, HISTORY_PAGE + 1));
  expect(flat(next.days).at(-2)?.kind).toBe("unread");
});

// The address of a turn draws the history down to that turn's thread, which can stand under the
// entries drawn: a reply's place is its thread's.
test("a turn's place is its thread's, and the history drawn down to it draws its thread", () => {
  seq = 0;
  const root = message("daemon", "2026-11-15T09:00:00Z", DAEMON);
  const reply = message("reply", "2026-11-15T12:00:00Z", PERSON, { in_reply_to: "daemon" });
  const days = daysOf(claims(30), 0, [], [{ message: root, replies: [reply] }]);
  const first = historyDownTo(days, placeUnder(days, undefined, HISTORY_PAGE));
  expect(threadsIn(flat(first.days)), "the thread is among the first entries").toEqual([]);

  const toTurn = historyDownTo(days, turnPlace(days, "reply"));
  expect(threadsIn(flat(toTurn.days))).toEqual([{ replies: ["reply"], root: "daemon" }]);
  expect(toTurn.left).toBe(0);
  expect(turnPlace(days, "no-such-turn")).toBeUndefined();
});

// A turn carries the marks of a post its author corrected, as the outline's rows do: when its
// author edited it, and its deletion, by whom and when, which stands in place of its words.
test("a turn carries when its author edited it, and its deletion", () => {
  seq = 0;
  const root = message("daemon", "2026-11-15T09:00:00Z", DAEMON, {
    edited_at: "2026-11-15T09:30:00Z",
  });
  const reply = message("reply", "2026-11-15T09:10:00Z", PERSON, {
    body: "",
    deleted_at: "2026-11-15T09:20:00Z",
    deleted_by: PERSON,
    in_reply_to: "daemon",
  });
  const [thread] = flat(daysOf([], 0, [], [{ message: root, replies: [reply] }]));
  if (thread?.kind !== "thread") throw new Error("the daemon's thread is no entry");
  expect(thread.root).toMatchObject({ deleted: null, editedAt: "2026-11-15T09:30:00Z" });
  expect(thread.replies[0]).toMatchObject({
    deleted: { at: "2026-11-15T09:20:00Z", by: PERSON },
    editedAt: null,
  });
});

// What happened to a thread agents wrote among themselves is no row's of the outline, so it is
// a line of the history; the same events of a comment a person wrote are the outline's.
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
      asks: [],
      blocks: new Map(),
      comments: [among, mine],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.event.type] : []));
  expect(lines).toEqual(["comment.resolved", "comment.delivery"]);
});

// A delivery's line names where it went by the attempt's target, an address - `session:<id>`, or
// `role:<token>` for a role a session held - which no reader names an agent by: the history
// draws the agent it went to as the page names an agent (`AgentName`), from the session the
// attempt reached, else the one its target names.
test("a delivery of an agents' thread names its agent, never its address", () => {
  seq = 0;
  const among = comment("among", "2026-11-15T09:00:00Z", AGENT);
  const delivery = (at: string, target: string, sessionId: string | null) =>
    event("comment.delivery", at, {
      attempt: 1,
      comment_id: "among",
      delivery: "btw",
      duplicate: false,
      reply_id: null,
      session_id: sessionId,
      state: "sent",
      target,
    });
  const events = [
    event("comment.created", "2026-11-15T09:00:00Z", { id: "among" }),
    delivery("2026-11-15T09:01:00Z", "session:reviewer", null),
    delivery("2026-11-15T09:02:00Z", "role:legion-p-core-1-tester", "tester-7"),
  ];
  const lines = flat(
    buildHistory({
      artifacts: [],
      asks: [],
      blocks: new Map(),
      comments: [among],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => (entry.kind === "event" ? [entry.description] : []));
  expect(lines).toEqual([
    ["delivered a comment mention to ", { sessionId: "tester-7", storedTitle: undefined }],
    ["delivered a comment mention to ", { sessionId: "reviewer", storedTitle: undefined }],
  ]);
});

// The page draws a stored text by its words, never the markup its author typed or an address.
// An approval request is a line of the history (the outline draws no row for it), and its line
// quotes the request's question. Found by the tester on a running build of b0082656 with a
// copy of a large real issue: the lines of the spec's approval request read “… The new page
// reaches `main` in parts …”, the backticks drawn as typed.
test("an approval request's line draws its question's words, never its markup or an address", () => {
  seq = 0;
  const question =
    "Approve **spec.md** (version 10)? It reaches `main` in parts, as dispatch://CORE-2/spec#b-r1 asks.";
  const events = [
    event("ask.opened", "2026-11-15T09:00:00Z", { id: "a1", kind: "approval", question }),
    event("ask.answered", "2026-11-15T09:01:00Z", { id: "a1", kind: "approval", question }, PERSON),
  ];
  const lines = flat(
    buildHistory({
      artifacts: [],
      asks: [{ id: "a1", kind: "approval" }],
      blocks: new Map(),
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) =>
    entry.kind === "event"
      ? [entry.description.filter((part): part is string => typeof part === "string").join("")]
      : []
  );
  expect(lines, "the request's two lines are not drawn").toHaveLength(2);
  for (const line of lines) {
    for (const typed of ["`", "**", "dispatch://"]) {
      expect(line, `the approval request's line draws "${typed}" as typed`).not.toContain(typed);
    }
  }
});

// A block's line names the block by its id, and a UUID is no name a reader reads: the history
// names such a block by the first line of the words of the node that carries it, as the outline
// names a question, and the panel links those words to that node. An id its author chose, a slug
// such as `c-units`, reads as words and stays as `main` writes it.

const QUESTION_BLOCK = "01f349b4-2a84-4ebb-81be-91fa2f4e2300";
const CHECK_BLOCK = "6c0e1a52-93d4-4b7f-a0c8-2f5d8e9b1c47";
const GONE_BLOCK = "9a7b3c2d-1e4f-4a5b-8c6d-7e8f9a0b1c2d";

/** The blocks of the issue's question, asked from the typed block `QUESTION_BLOCK`, and of its
 *  two checks, the blocks `CHECK_BLOCK` and `c-units`, as the asks read and the results read
 *  carry them. */
const BLOCKS = historyBlocks(
  KEY,
  [
    {
      block_id: QUESTION_BLOCK,
      id: "q1",
      question: "Which **layout** reads best on a phone? The rail or the drawer.",
    },
  ],
  [
    {
      block_id: CHECK_BLOCK,
      ref: `dispatch://${KEY}/artifact/verification-md#b-${CHECK_BLOCK}`,
      text: "The units pass on a clean checkout.",
    },
    {
      block_id: "c-units",
      ref: `dispatch://${KEY}/artifact/verification-md#b-c-units`,
      text: "The units pass.",
    },
  ]
);

/** The lines the history draws of `events` with the blocks `blocks`, newest first: each line's
 *  text, and the block it names by its node's words, whose part of the line it checks holds
 *  those words. */
function blockLines(
  blocks: HistoryInput["blocks"],
  events: readonly DispatchEvent[]
): { block: HistoryBlock | undefined; text: string }[] {
  return flat(
    buildHistory({
      artifacts: [],
      asks: [{ id: "q1", kind: "question" }],
      blocks,
      comments: [],
      events,
      lastReadSeq: 0,
      messages: [],
      today: "2026-11-15",
    })
  ).flatMap((entry) => {
    if (entry.kind !== "event") return [];
    if (entry.block !== undefined) {
      expect(
        entry.description[entry.block.part],
        "the block's part of the line is not its words"
      ).toBe(entry.block.block.words);
    }
    return [
      {
        block: entry.block?.block,
        text: entry.description.filter((part): part is string => typeof part === "string").join(""),
      },
    ];
  });
}

test("a repaired block is named by its question's first words, never by its id", () => {
  seq = 0;
  const [line, ...rest] = blockLines(BLOCKS, [
    event("block.repaired", "2026-11-15T09:00:00Z", {
      block_id: QUESTION_BLOCK,
      disturbed_by: PERSON,
      version: 3,
    }),
  ]);
  expect(rest).toEqual([]);
  expect(line?.text, "the repaired block's line draws its id").not.toContain(QUESTION_BLOCK);
  expect(line?.text).toBe(
    "repaired server-owned state on block “Which layout reads best on a phone?”"
  );
  expect(line?.block).toEqual({
    path: buildIssuePath({ id: "q1", key: KEY, kind: "ask" }),
    words: "Which layout reads best on a phone?",
  });
});

test("a question's block marked malformed is named by the question's first words, never by its id", () => {
  seq = 0;
  const [line, ...rest] = blockLines(BLOCKS, [
    event("block.invalid", "2026-11-15T09:00:00Z", {
      block_id: QUESTION_BLOCK,
      disturbed_by: PERSON,
      reason: "it names no options",
      version: 3,
    }),
  ]);
  expect(rest).toEqual([]);
  expect(line?.text, "the malformed block's line draws its id").not.toContain(QUESTION_BLOCK);
  expect(line?.text).toBe(
    "marked decision “Which layout reads best on a phone?” malformed: it names no options"
  );
  expect(line?.block?.path).toBe(buildIssuePath({ id: "q1", key: KEY, kind: "ask" }));
});

test("a refused check is named by its words where its id is a UUID, and by its id where that is a slug", () => {
  seq = 0;
  const refused = (at: string, blockId: string) =>
    event("block.invalid", at, {
      block_id: blockId,
      block_type: "check",
      disturbed_by: PERSON,
      reason: "its ci_check names no job",
      version: 2,
    });
  const lines = blockLines(BLOCKS, [
    refused("2026-11-15T09:00:00Z", CHECK_BLOCK),
    refused("2026-11-15T09:01:00Z", "c-units"),
  ]);
  expect(lines[1]?.text, "the refused check's line draws its id").not.toContain(CHECK_BLOCK);
  expect(lines.map((line) => line.text)).toEqual([
    "refused check c-units: its ci_check names no job",
    "refused check “The units pass on a clean checkout.”: its ci_check names no job",
  ]);
  expect(lines.map((line) => line.block)).toEqual([
    undefined,
    {
      path: checkPath(),
      words: "The units pass on a clean checkout.",
    },
  ]);
});

/** The page address of the check `BLOCKS` names by its words: its `ref`, read as a route. */
function checkPath(): string {
  const route = parseNodeReference(`dispatch://${KEY}/artifact/verification-md#b-${CHECK_BLOCK}`);
  if (route === undefined) throw new Error("the check's ref reads as no route");
  return buildReferencePath(route);
}

test("a block whose id is a UUID that no node carries reads as a block that is gone", () => {
  seq = 0;
  const lines = blockLines(BLOCKS, [
    event("block.repaired", "2026-11-15T09:00:00Z", {
      block_id: GONE_BLOCK,
      disturbed_by: PERSON,
      version: 3,
    }),
    event("block.invalid", "2026-11-15T09:01:00Z", {
      block_id: GONE_BLOCK,
      disturbed_by: PERSON,
      reason: "it names no options",
      version: 3,
    }),
  ]);
  expect(lines).toEqual([
    {
      block: undefined,
      text: `marked ${CONTROL_LABELS.history_gone_block} malformed: it names no options`,
    },
    {
      block: undefined,
      text: `repaired server-owned state on ${CONTROL_LABELS.history_gone_block}`,
    },
  ]);
  expect(CONTROL_LABELS.history_gone_block).toBe("a block that is gone");
});

// Until every read the blocks come from has answered, whether a block is gone is not known, so
// a line names the block by `main`'s word for it alone: never by its id, and never as gone.
test("while the blocks are not known, a repaired block's line names a block, never its id or one that is gone", () => {
  seq = 0;
  const lines = blockLines(undefined, [
    event("block.repaired", "2026-11-15T09:00:00Z", {
      block_id: QUESTION_BLOCK,
      disturbed_by: PERSON,
      version: 3,
    }),
  ]);
  expect(lines).toEqual([{ block: undefined, text: "repaired server-owned state on a block" }]);
  expect(lines[0]?.text, "the repaired block's line draws its id").not.toContain(QUESTION_BLOCK);
  expect(lines[0]?.text, "the repaired block's line says it is gone").not.toContain(
    CONTROL_LABELS.history_gone_block
  );
});
