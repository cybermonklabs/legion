import { expect, test } from "bun:test";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Actor, Artifact, Comment, DispatchEvent, MessageRead } from "../../api/types";
import { dayLabel } from "../activity/activity-model";
import {
  buildHistory,
  type HistoryDay,
  type HistoryEntry,
  type HistoryInput,
} from "./history-model";
import { CONTROL_LABELS, DECLARED_WORDS, wordsIn } from "./page-words";

/**
 * The issue's history as the panel draws it: every event that is no row of the outline, newest
 * first, by day, with a run of one author's uploads - the newest and those made at most a minute
 * before it - as one entry and the threads agents wrote among themselves drawn whole.
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
  const history = (comments: Comment[], asks: HistoryInput["asks"] = []) =>
    flat(
      buildHistory({
        artifacts: [],
        asks,
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
    asks: [],
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
        asks: [],
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

// Only a thread waits for the page that reaches back to it, so the day the pages read so far end
// in holds as many of its entries as those pages do: a day of five claims, two of them on the
// first page, was counted two until Load older counted it five. That day is partial while older
// pages remain, and a day after it is whole.
test("the day the pages of the log read so far end in is partial until the log is in", () => {
  seq = 0;
  const older = [1, 2, 3].map((minute) => event("issue.claimed", november(13, 12, minute), {}));
  const read = [
    event("issue.claimed", november(13, 12, 4), {}),
    event("issue.claimed", november(13, 12, 5), {}),
    event("issue.claimed", november(14, 12), {}),
  ];
  const days = (events: readonly DispatchEvent[], loadedFrom: string | undefined) =>
    buildHistory({
      artifacts: [],
      asks: [],
      comments: [],
      events,
      lastReadSeq: 0,
      ...(loadedFrom === undefined ? {} : { loadedFrom }),
      messages: [],
      today: "2026-11-15",
    }).map((day) => [day.date, day.partial, day.entries.length]);

  expect(
    days(read, november(13, 12, 4)),
    "the day the first page ends in is drawn as whole"
  ).toEqual([
    ["2026-11-14", false, 1],
    ["2026-11-13", true, 2],
  ]);
  // The whole log is in: no day is partial.
  expect(days([...older, ...read], undefined)).toEqual([
    ["2026-11-14", false, 1],
    ["2026-11-13", false, 5],
  ]);
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
