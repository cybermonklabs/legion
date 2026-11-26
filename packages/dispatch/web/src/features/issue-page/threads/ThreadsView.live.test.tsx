import { beforeAll, describe, expect, spyOn, test } from "bun:test";
import type {
  CommentThreadPage,
  CommentThreadRow,
  MessageThreadPage,
  MessageThreadRow,
} from "@legion/contracts";
import { QueryClient } from "@tanstack/react-query";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { http } from "msw";

import { signedInAsReader } from "../../../__tests__/draft-matrix-fixture";
import { answerRequests } from "../../../__tests__/network";
import { api } from "../../../api/client";
import { prependEventToLog } from "../../../api/event-feed";
import { eventRefreshTargets } from "../../../api/event-refresh";
import { refreshQueries } from "../../../api/query-refresh";
import type { Actor, DispatchEvent, MyRepliesResponse, MyReplyRow } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DASHBOARD_WORDS,
  EMPTY_STATE_LINES,
  THREADS_WORDS,
} from "../page-words";
import commentReplyToBody from "./fixtures/comment-reply-to.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import eventsBody from "./fixtures/events.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneBody from "./fixtures/messages-none.json";
import messagesNoneAuthorBody from "./fixtures/messages-none-author.json";
import repliesUnreadBody from "./fixtures/replies-unread.json";
import { answerThreads, drawThreads, stubWindow, threadsAt } from "./threads-view-fixture";

/**
 * Live Threads (dispatch://INFRA-5704/spec#b-r-live, R14) over the server's answers captured at
 * the stack base (`fixtures/README.md`), with a query client, as plan list 3 table D gives its
 * states: a write that lands while Threads is open is drawn with no reload, in the list and in the
 * open record, and the rows the reader paged to stay drawn.
 *
 * Each write is the server's captured answer moved as the write moves it on the server. Each
 * event reaches the page as the event feed hands it over (`useEventFeed`): prepended to the
 * issue's log, then its refresh targets read again through the one scheduler
 * (`eventRefreshTargets`, `refreshQueries`), so the reads it reaches are the very queries Threads
 * renders, under whatever keys they stand.
 */

/** The reader the fixtures were read for. */
const READER = "alice";

const COMMENTS = commentsInBody as unknown as CommentThreadPage;
const COMMENTS_NEXT = commentsInBeforeBody as unknown as CommentThreadPage;
const MESSAGES = messagesInBody as unknown as MessageThreadPage;
const AGENTS_PAGE = messagesNoneBody as unknown as MessageThreadPage;
/** The server's answer for a route that holds no thread. */
const NO_THREAD = messagesNoneAuthorBody as unknown as MessageThreadPage;
const EVENTS = eventsBody as unknown as DispatchEvent[];
const REPLIES = repliesUnreadBody as unknown as MyRepliesResponse;

/** The comment thread of All's first comment page whose root's id begins with `prefix`, as the
 *  fixtures' README and `thread-merge.test.ts` name them. */
function commentThread(prefix: string): CommentThreadRow {
  const found = (COMMENTS.threads ?? []).find((row) => row.comment.id.startsWith(prefix));
  if (found === undefined) throw new Error(`No comment page of the fixtures holds ${prefix}.`);
  return found;
}

/** The reader's comment on the spec's text, with the planner's and the reviewer's replies: a row
 *  of All's second page, and a thread of the reader's unread set, whose row counts 2. */
const TEXT = commentThread("c1d04870");
/** The reader's filler comment 25, the first row of All's second page. */
const FILLER = commentThread("678608f6");
/** A comment of agents alone on the spec's words, which the reader did not write in, on All's
 *  second page with no mark. */
const QUIET = commentThread("cc70d974");
/** The comments' second page, which no page of All holds until the reader pages to it: the
 *  reader's filler comment 1, with the reader's deleted reply, and the reader's post with the
 *  planner's reply that quotes it, a thread of the reader's unread set. */
const [OLDEST, POST] = (COMMENTS_NEXT.threads ?? []) as [CommentThreadRow, CommentThreadRow];
/** A person's message to the reviewer's role, in whose shape a new thread is written. */
const ASKED = (MESSAGES.threads ?? []).find((row) => row.message.id.startsWith("882e100d"));
/** The thread between agents, Agents' one row, which no page of All holds. */
const AGENTS_ROW = AGENTS_PAGE.threads?.[0];
const AGENTS = AGENTS_ROW?.message.id ?? "";

/** The writers of the writes the cases land: the reader, the planner's session and a person. */
const READER_ACTOR = TEXT.comment.author;
const PLANNER = TEXT.replies?.[0]?.author as Actor;
const BOB = ASKED?.message.author as Actor;

/** The time each case's write lands, past every time the fixtures hold. */
const LATER = "2026-11-26T12:00:00.000000Z";
/** The ids the server gives the writes the cases land. */
const OWN_REPLY = "5e1f0000-0000-4000-8000-000000000001";
const AGENT_REPLY = "5e1f0000-0000-4000-8000-000000000002";
const NEW_THREAD = "5e1f0000-0000-4000-8000-000000000003";
const PERSON_REPLY = "5e1f0000-0000-4000-8000-000000000004";

/** The event the server appends for a case's write: the issue's next in its log. */
const NEXT_ID = Math.max(...EVENTS.map((event) => event.id)) + 1;
const NEXT_SEQ = Math.max(...EVENTS.map((event) => event.seq)) + 1;

/** The first event of the log of `type`, whose shape a case's event takes, a reply's where
 *  `reply` holds. */
function eventShape(type: DispatchEvent["type"], reply = false): DispatchEvent {
  const found = EVENTS.find((event) => {
    if (event.type !== type) return false;
    const payload: unknown = event.payload;
    return (
      !reply ||
      (typeof payload === "object" &&
        payload !== null &&
        "reply_to" in payload &&
        payload.reply_to != null)
    );
  });
  if (found === undefined) throw new Error(`The events of the fixtures hold no ${type}.`);
  return found;
}

/** The event the server appends for a write of `author`, in the shape of its own event of that
 *  type, with `payload` the write's own fields. */
function nextEvent(
  shape: DispatchEvent,
  author: Actor,
  payload: Readonly<Record<string, unknown>>
): DispatchEvent {
  return {
    ...shape,
    actor: author,
    created_at: LATER,
    id: NEXT_ID,
    payload: { ...shape.payload, author, created_at: LATER, ...payload },
    seq: NEXT_SEQ,
  } as DispatchEvent;
}

type Comment = CommentThreadRow["comment"];

/** A reply of `author` to the head of `thread`, as the server stores it. */
function commentReply(thread: CommentThreadRow, author: Actor, id: string, body: string): Comment {
  return {
    ...thread.comment,
    anchor: null,
    answers: thread.comment.id,
    author,
    body,
    created_at: LATER,
    deliveries: [],
    id,
    mentions: [],
    reply_block: null,
    reply_to: thread.comment.id,
  };
}

/** The event of `reply`, with `logins` the people of its thread, as the server names them. */
function commentEvent(reply: Comment, logins: readonly string[]): DispatchEvent {
  return nextEvent(eventShape("comment.created", true), reply.author, {
    answers: reply.answers,
    body: reply.body,
    id: reply.id,
    reply_to: reply.reply_to,
    thread_logins: logins,
    thread_root_id: reply.reply_to,
  });
}

/** All's first comment page once `reply` landed in `thread`: that thread first, with the reply
 *  and its new last activity. Each case's thread stands on this page before the write and after
 *  it, so the page's `before` is the same. */
function commentsWith(thread: CommentThreadRow, reply: Comment): CommentThreadPage {
  const others = (COMMENTS.threads ?? []).filter((row) => row.comment.id !== thread.comment.id);
  return {
    ...COMMENTS,
    threads: [
      { ...thread, last_activity_at: LATER, replies: [...(thread.replies ?? []), reply] },
      ...others,
    ],
  };
}

/** All's message page with `thread` first, as the server answers it once `thread` is the
 *  newest. */
function messagesWith(thread: MessageThreadRow): MessageThreadPage {
  const others = (MESSAGES.threads ?? []).filter((row) => row.message.id !== thread.message.id);
  return { ...MESSAGES, threads: [thread, ...others] };
}

/** All's first comment page once `thread`, which stood on the second page, is the newest: that
 *  thread first, then the first page's threads but its last, which moves to the second page. Its
 *  `before` is the last thread's last activity and id, as the server writes a page's cursor
 *  (`comments-in.json`). */
function commentsFirst(thread: CommentThreadRow): CommentThreadPage {
  const kept = (COMMENTS.threads ?? []).slice(0, -1);
  const last = kept.at(-1);
  return {
    ...COMMENTS,
    before: last === undefined ? null : `${last.last_activity_at},${last.comment.id}`,
    threads: [thread, ...kept],
  };
}

/** The reader's unread row of `thread` once `reply` landed in it, read through the reader's own
 *  last write there, in the shape of the server's row of the reader's post
 *  (`replies-unread.json`). */
function unreadRow(thread: CommentThreadRow, reply: Comment): MyReplyRow {
  const shape = (REPLIES.rows ?? []).find((row) => row.thread_id === POST.comment.id);
  if (shape === undefined) throw new Error("The reader's unread threads hold no row of the post.");
  return {
    ...shape,
    read_through: thread.last_activity_at,
    ref: shape.ref.replace(POST.comment.id, thread.comment.id),
    reply: {
      ...shape.reply,
      author: reply.author,
      created_at: reply.created_at,
      first_line: reply.body,
      id: reply.id,
    },
    subject: thread.comment.body,
    thread_id: thread.comment.id,
    unread: true,
    unread_count: 1,
  };
}

/** A comment thread's own read (`GET /api/v1/comments/{id}`), as the server answers it, and what
 *  its answer waits for, where a case holds it. */
interface ThreadRead {
  readonly body: unknown;
  readonly held?: Promise<void>;
}

/** What the server answers Threads' reads with, which a case moves as a write lands. */
interface Server {
  comments: CommentThreadPage;
  /** What the answer of All's first comment page waits for, where a case holds it. */
  commentsHeld: Promise<void> | undefined;
  /** The comments' second page, or a lost connection where it is null. */
  commentsNext: CommentThreadPage | null;
  messages: MessageThreadPage;
  /** What the answer of All's message page waits for, where a case holds it. */
  messagesHeld: Promise<void> | undefined;
  agents: MessageThreadPage;
  events: readonly DispatchEvent[];
  replies: MyRepliesResponse;
  /** Each comment thread's own read by its head's id, which Waiting on me sends for a row whose
   *  thread no page holds. */
  threads: Map<string, ThreadRead>;
  /** How many times each of those reads was sent, by the head's id. */
  threadReads: Map<string, number>;
}

/** Answers every read Threads sends from `Server`, which starts as the captured answers, and
 *  every other read as Threads' own (`answerThreads`). */
function answerServer(): Server {
  const server: Server = {
    agents: AGENTS_PAGE,
    comments: COMMENTS,
    commentsHeld: undefined,
    commentsNext: COMMENTS_NEXT,
    events: EVENTS,
    messages: MESSAGES,
    messagesHeld: undefined,
    replies: REPLIES,
    threads: new Map([[POST.comment.id, { body: commentReplyToBody }]]),
    threadReads: new Map(),
  };
  answerThreads();
  // Answered last, so each runs before Threads' own.
  answerRequests(
    http.get("/api/v1/issues/:key/comments", async ({ request }) => {
      if (new URL(request.url).searchParams.has("before")) {
        return server.commentsNext === null ? Response.error() : Response.json(server.commentsNext);
      }
      await server.commentsHeld;
      return Response.json(server.comments);
    }),
    http.get("/api/v1/issues/:key/messages", async ({ request }) => {
      if (new URL(request.url).searchParams.get("person") === "none") {
        return Response.json(server.agents);
      }
      await server.messagesHeld;
      return Response.json(server.messages);
    }),
    http.get("/api/v1/issues/:key/events", () => Response.json(server.events)),
    // The read mark's read, with `limit`, stays Threads' own.
    http.get("/api/v1/me/replies", ({ request }) =>
      new URL(request.url).searchParams.has("limit") ? undefined : Response.json(server.replies)
    ),
    http.get("/api/v1/comments/:id", async ({ params }) => {
      const id = String(params.id);
      const answer = server.threads.get(id);
      if (answer === undefined) return undefined;
      server.threadReads.set(id, (server.threadReads.get(id) ?? 0) + 1);
      await answer.held;
      return Response.json(answer.body);
    })
  );
  return server;
}

/** Threads at 1440 px at `path`, every read answered from the server's state. */
function drawAt(path: string, client?: QueryClient) {
  const window = stubWindow(1440);
  const server = answerServer();
  const drawn = drawThreads(path, client === undefined ? {} : { client });
  return {
    ...drawn,
    server,
    done(): void {
      drawn.done();
      window.restore();
    },
  };
}

/** Hands `event` to the page as the event feed does (`useEventFeed`). */
function deliver(client: QueryClient, event: DispatchEvent): void {
  act(() => {
    prependEventToLog(client, event);
    refreshQueries(client, eventRefreshTargets(event, READER));
  });
}

const rows = () => [...document.querySelectorAll<HTMLElement>("[data-thread-row]")];
const rowIds = () => rows().map((row) => row.getAttribute("data-thread-row") ?? "");
const rowOf = (id: string) => rows().find((row) => row.getAttribute("data-thread-row") === id);
const markOf = (id: string) =>
  rowOf(id)?.querySelector("[data-thread-mark]")?.textContent ?? undefined;
const record = () => document.querySelector("[data-thread-record]");
const entry = (id: string) => document.querySelector(`[data-thread-entry="${id}"]`) ?? undefined;
const choice = (name: string) => screen.getByRole("button", { name });
const showNext = () =>
  screen.getByRole("button", { name: new RegExp(`^${DASHBOARD_WORDS.next_twenty}`) });
/** The number the control under the rows says is left. */
const left = () => Number(/· (\d+) /.exec(showNext().textContent ?? "")?.[1]);
const foot = () => document.querySelector<HTMLElement>('[data-thread-box="foot"]') ?? document.body;

/** Draws All's first 40 rows, its second page shown with the control under the rows, and answers
 *  them. */
async function pageAll(user: UserEvent): Promise<string[]> {
  await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
  await user.click(showNext());
  await waitFor(() => expect(rows().length, "the control drew no next rows").toBe(40));
  return rowIds();
}

/** Presses the link of the row of `id`, and answers the record it opens. */
async function pressRow(user: UserEvent, id: string): Promise<Element | null> {
  const row = rowOf(id);
  if (row === undefined) throw new Error(`The list draws no row of ${id}.`);
  await user.click(within(row).getByRole("link"));
  await waitFor(() => expect(record()).toHaveAttribute("data-thread-record", id));
  return record();
}

/** Each list of rows the list draws from now on, read at each change of the page, so a case
 *  reads what the reader saw at every moment and not only where the page came to rest. */
function watchRows() {
  const seen: string[][] = [];
  const observer = new MutationObserver(() => seen.push(rowIds()));
  observer.observe(document.body, { characterData: true, childList: true, subtree: true });
  return {
    seen,
    stop: () => observer.disconnect(),
  };
}

/** That at every moment `seen` holds, every row of `paged` was drawn. */
function expectEachStayed(seen: readonly (readonly string[])[], paged: readonly string[]): void {
  expect(seen.length, "the page drew nothing as the write landed").toBeGreaterThan(0);
  for (const drawn of seen) {
    expect(
      paged.filter((id) => !drawn.includes(id)),
      "a row the reader paged to left the list while the pages were read again"
    ).toEqual([]);
  }
}

beforeAll(async () => {
  await warmMarkdownRenderer();
});

describe("table D: live (R14)", () => {
  test("the reader's own reply from a record of All's second page moves its row first with no mark, keeps the second page's rows, and is drawn in the open record", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    const own = commentReply(TEXT, READER_ACTOR, OWN_REPLY, "My own reply.");
    const create = spyOn(api, "createComment").mockImplementation(async () => {
      // The server stores the reply. Its thread is the newest now, and the reader's own write
      // moves the reader's read mark of it past the reply, so it leaves the reader's unread
      // threads (migration `0095_user_threads`).
      drawn.server.comments = commentsWith(TEXT, own);
      drawn.server.replies = {
        ...REPLIES,
        rows: (REPLIES.rows ?? []).filter((row) => row.thread_id !== TEXT.comment.id),
      };
      return own as never;
    });
    try {
      const paged = await pageAll(user);
      expect(paged.indexOf(TEXT.comment.id), "the record is no row of All's second page").toBe(25);
      expect(markOf(TEXT.comment.id), "the record's row draws no mark before the write").toBe("2");
      const opened = await pressRow(user, TEXT.comment.id);
      const watch = watchRows();
      await user.click(within(foot()).getByRole("textbox"));
      await user.paste("My own reply.");
      await user.click(within(foot()).getByRole("button", { name: CONTROL_LABELS.send }));
      await waitFor(() =>
        expect(rowIds()[0], "the reader's reply did not move its row first").toBe(TEXT.comment.id)
      );
      await waitFor(() => expect(entry(OWN_REPLY), "the open record draws no reply").toBeDefined());
      await waitFor(() =>
        expect(markOf(TEXT.comment.id), "the reader's own write is counted").toBeUndefined()
      );
      watch.stop();
      expect(create).toHaveBeenCalledTimes(1);
      expect(record(), "the record was drawn anew").toBe(opened);
      expect(drawn.address()).toBe(threadsAt(TEXT.comment.id));
      expect(new Set(rowIds()), "the list draws other rows than it was paged to").toEqual(
        new Set(paged)
      );
      expectEachStayed(watch.seen, paged);
    } finally {
      create.mockRestore();
      drawn.done();
    }
  });

  test("an agent's reply in a thread of All's second page moves its row first with its new mark, and the second page's rows stay drawn", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    const reply = commentReply(QUIET, PLANNER, AGENT_REPLY, "A reply while the list is open.");
    const event = commentEvent(reply, []);
    try {
      const paged = await pageAll(user);
      expect(paged.indexOf(QUIET.comment.id), "the thread is no row of All's second page").toBe(28);
      expect(markOf(QUIET.comment.id), "the thread's row draws a mark before the write").toBe(
        undefined
      );
      const watch = watchRows();
      drawn.server.comments = commentsWith(QUIET, reply);
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await waitFor(() =>
        expect(rowIds()[0], "the thread of the new reply did not move first").toBe(QUIET.comment.id)
      );
      await waitFor(() =>
        expect(markOf(QUIET.comment.id), "the moved row draws no mark of its new write").toBe("1")
      );
      watch.stop();
      expect(new Set(rowIds()), "the list draws other rows than it was paged to").toEqual(
        new Set(paged)
      );
      expectEachStayed(watch.seen, paged);
    } finally {
      drawn.done();
    }
  });

  test("a new thread that the reader has not read comes first with its mark, and the last row drawn moves into the number left", async () => {
    const drawn = drawAt(threadsAt());
    const root = {
      ...(ASKED?.message as MessageThreadRow["message"]),
      body: "A new thread from a person.",
      created_at: LATER,
      deliveries: [],
      id: NEW_THREAD,
    };
    const event = nextEvent(eventShape("message.created"), BOB, {
      body: root.body,
      id: root.id,
      target: root.target,
      thread_logins: [BOB.id],
    });
    try {
      await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
      const before = rowIds();
      const leftBefore = left();
      drawn.server.messages = messagesWith({ last_activity_at: LATER, message: root, replies: [] });
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await waitFor(() =>
        expect(rowIds()[0], "the new thread did not come first").toBe(NEW_THREAD)
      );
      await waitFor(() => expect(markOf(NEW_THREAD), "the new thread draws no mark").toBe("1"));
      expect(rowIds(), "the rows below the new thread moved").toEqual([
        NEW_THREAD,
        ...before.slice(0, 19),
      ]);
      expect(left(), "the last row drawn is not in the number left").toBe(leftBefore + 1);
    } finally {
      drawn.done();
    }
  });

  test("a person's reply in a thread between agents takes it out of Agents and into All, and its open record stays open through the move", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    const lookups = [
      spyOn(api, "getAsk"),
      spyOn(api, "getComment"),
      spyOn(api, "getMessageThread"),
    ];
    const joined = Promise.withResolvers<void>();
    const reply: MessageThreadRow["message"] = {
      ...(AGENTS_ROW?.replies?.[0] as MessageThreadRow["message"]),
      author: BOB,
      body: "A person's word in the agents' thread.",
      created_at: LATER,
      id: PERSON_REPLY,
      in_reply_to: AGENTS,
    };
    const event = nextEvent(eventShape("message.answered"), BOB, {
      body: reply.body,
      id: reply.id,
      in_reply_to: AGENTS,
      reply_block: null,
      thread_logins: [BOB.id],
      thread_target: AGENTS_ROW?.message.target,
    });
    try {
      await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
      await user.click(choice(THREADS_WORDS.choices.agents));
      await waitFor(() => expect(rowIds(), "Agents draws no row of its thread").toEqual([AGENTS]));
      const opened = await pressRow(user, AGENTS);
      // The route the thread leaves answers first, and the one it joins is held.
      drawn.server.messagesHeld = joined.promise;
      drawn.server.agents = NO_THREAD;
      drawn.server.messages = messagesWith({
        ...(AGENTS_ROW as MessageThreadRow),
        last_activity_at: LATER,
        replies: [...(AGENTS_ROW?.replies ?? []), reply],
      });
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await screen.findByText(EMPTY_STATE_LINES.threads_agents);
      expect(record(), "the record went while the route it joins was read").toBe(opened);
      await act(async () => joined.resolve());
      await waitFor(() => expect(entry(PERSON_REPLY), "the record draws no reply").toBeDefined());
      expect(record(), "the record was drawn anew once All held it").toBe(opened);
      await user.click(choice(THREADS_WORDS.choices.all));
      await waitFor(() =>
        expect(rowIds()[0], "All draws the thread elsewhere than first").toBe(AGENTS)
      );
      expect(record(), "the record went as All was turned on").toBe(opened);
      expect(drawn.address()).toBe(threadsAt(AGENTS));
      for (const lookup of lookups) {
        expect(
          lookup,
          "the record was looked up while a page that holds it was out"
        ).not.toHaveBeenCalled();
      }
    } finally {
      joined.resolve();
      for (const lookup of lookups) lookup.mockRestore();
      drawn.done();
    }
  });

  test("an agent's reply in the open record is drawn in it, and the record stays open while its row moves first", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    const reply = commentReply(FILLER, PLANNER, AGENT_REPLY, "A reply in the open record.");
    const event = commentEvent(reply, [READER]);
    try {
      const paged = await pageAll(user);
      expect(paged.indexOf(FILLER.comment.id), "the record is no row of All's second page").toBe(
        20
      );
      const opened = await pressRow(user, FILLER.comment.id);
      const watch = watchRows();
      drawn.server.comments = commentsWith(FILLER, reply);
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await waitFor(() => expect(entry(AGENT_REPLY), "the record draws no reply").toBeDefined());
      await waitFor(() =>
        expect(rowIds()[0], "the record's row did not move first").toBe(FILLER.comment.id)
      );
      watch.stop();
      expect(record(), "the record was drawn anew").toBe(opened);
      expect(drawn.address()).toBe(threadsAt(FILLER.comment.id));
      expectEachStayed(watch.seen, paged);
    } finally {
      drawn.done();
    }
  });

  test("a write in a thread no page holds keeps Waiting on me's rows drawn until that thread's own read answers, then draws the page whole with it", async () => {
    const drawn = drawAt(threadsAt());
    const user = userEvent.setup();
    const reply = commentReply(OLDEST, PLANNER, AGENT_REPLY, "A reply past All's first pages.");
    const event = commentEvent(reply, [READER]);
    const joined = {
      ...OLDEST,
      last_activity_at: LATER,
      replies: [...(OLDEST.replies ?? []), reply],
    };
    const threadRead = Promise.withResolvers<void>();
    const commentsRead = Promise.withResolvers<void>();
    try {
      await waitFor(() => expect(rows().length, "All drew no row").toBe(20));
      await user.click(choice(THREADS_WORDS.choices.waiting));
      await waitFor(() =>
        expect(rowIds(), "Waiting on me draws no row of the post no page holds").toContain(
          POST.comment.id
        )
      );
      const before = rowIds();
      expect(before, "Waiting on me draws the thread before its write").not.toContain(
        OLDEST.comment.id
      );
      const watch = watchRows();
      // The server stores the reply: the thread is the newest of the reader's unread threads and
      // of All's first comment page. That page's answer and the thread's own read are held, so
      // no page holds the thread while its read is out.
      drawn.server.commentsHeld = commentsRead.promise;
      drawn.server.comments = commentsFirst(joined);
      drawn.server.threads.set(OLDEST.comment.id, {
        body: { comment: joined.comment, replies: joined.replies },
        held: threadRead.promise,
      });
      drawn.server.replies = {
        ...REPLIES,
        rows: [unreadRow(OLDEST, reply), ...(REPLIES.rows ?? [])],
        total: REPLIES.total + 1,
      };
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await waitFor(() =>
        expect(
          drawn.server.threadReads.get(OLDEST.comment.id),
          "Waiting on me sent no read of the thread its new row names"
        ).toBe(1)
      );
      expect(rowIds(), "Waiting on me drew other rows while the thread's read was out").toEqual(
        before
      );
      await act(async () => threadRead.resolve());
      await waitFor(() =>
        expect(rowIds()[0], "the thread's row is not first once its read answered").toBe(
          OLDEST.comment.id
        )
      );
      expect(rowIds(), "the page was not drawn whole with the new row").toEqual([
        OLDEST.comment.id,
        ...before,
      ]);
      watch.stop();
      expectEachStayed(watch.seen, before);
      await act(async () => commentsRead.resolve());
      await waitFor(() => expect(drawn.client.isFetching(), "a read is still out").toBe(0));
      expect(rowIds(), "the rows moved once All's page held the thread").toEqual([
        OLDEST.comment.id,
        ...before,
      ]);
    } finally {
      threadRead.resolve();
      commentsRead.resolve();
      drawn.done();
    }
  });

  test("a refresh that fails on its second page keeps every row drawn and draws the failed-read line, whose Retry reads the pages again", async () => {
    // A failed read is asked again at once, so the line is drawn with no wait.
    const client = signedInAsReader(
      new QueryClient({
        defaultOptions: {
          queries: { retry: false, retryDelay: 0, staleTime: Number.POSITIVE_INFINITY },
        },
      })
    );
    const drawn = drawAt(threadsAt(), client);
    const user = userEvent.setup();
    const reply = commentReply(QUIET, PLANNER, AGENT_REPLY, "A reply while the list is open.");
    const event = commentEvent(reply, []);
    try {
      const paged = await pageAll(user);
      const watch = watchRows();
      drawn.server.comments = commentsWith(QUIET, reply);
      drawn.server.commentsNext = null;
      drawn.server.events = [event, ...EVENTS];
      deliver(drawn.client, event);
      await screen.findByText(ANSWER_LINES.threads_unreadable);
      watch.stop();
      expect(rowIds(), "the rows drawn moved with the pages' read failed").toEqual(paged);
      expectEachStayed(watch.seen, paged);
      drawn.server.commentsNext = COMMENTS_NEXT;
      await user.click(screen.getByRole("button", { name: CONTROL_LABELS.retry }));
      await waitFor(() =>
        expect(rowIds()[0], "Retry did not read the pages again").toBe(QUIET.comment.id)
      );
      expect(screen.queryByText(ANSWER_LINES.threads_unreadable)).toBeNull();
      expect(new Set(rowIds())).toEqual(new Set(paged));
    } finally {
      drawn.done();
    }
  });
});
