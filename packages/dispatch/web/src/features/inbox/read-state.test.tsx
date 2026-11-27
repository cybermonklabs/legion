/**
 * What reading a thread in the Inbox does: the mark it writes, and the row it leaves exactly where
 * it was, drawn as read (INFRA-450). Only a dismiss takes a row out of the list.
 */

import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { onFakeClock, realClock, tick } from "../../__tests__/fake-clock";
import { heldAnswers } from "../../__tests__/held-answers";
import { heldNotDisabled } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { dwellOn, installIntersectionObserver } from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { answerAgentStates, answerThreadReads } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { api, NetworkError, REQUEST_DEADLINE_MS } from "../../api/client";
import {
  inboxQuery,
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesOwnerQuery,
  userAgentStateQuery,
  whoAmIQuery,
} from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  Agent,
  Comment,
  IssueDetails,
  Message,
  MessageRead,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { Inbox } from "./Inbox";
import { widenToReadingPane } from "./inbox-fixture";
import { ListedCards } from "./thread-cards-fixture";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

const AS_OF = "2026-10-12T02:00:00Z";
const NEWEST = "2026-10-12T01:00:00Z";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
  window.localStorage.clear();
});

function spy<K extends keyof typeof api>(name: K, value: unknown): void {
  restores.push(spyOn(api, name).mockResolvedValue(value as never));
}

function commentRow(threadId: string, subject: string): MyReplyRow {
  return replyRow(threadId, {
    issue: { key: "CORE-1", title: "Fix the thing" },
    read_through: "2026-10-12T00:30:00Z",
    reply: {
      author: { id: "bob", kind: "user" },
      created_at: NEWEST,
      first_line: "Agreed.",
      id: `${threadId}-reply`,
      kind: "reply",
    },
    subject,
  });
}

function comment(id: string, body: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-10-12T00:00:00Z",
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...overrides,
  };
}

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** A client holding everything the Inbox reads but the thread's own comments: `client`, a plain
 *  one unless the row needs the app's own. */
function seeded(
  rows: readonly MyReplyRow[],
  client: QueryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  })
): QueryClient {
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxQuery().queryKey, []);
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(rows));
  client.setQueryData(myRepliesCountQuery().queryKey, rows.length);
  // The unread set `useMarkThreadRead` watches: without the thread in it nothing is marked.
  client.setQueryData(myRepliesOwnerQuery({ issue: "CORE-1" }).queryKey, threadsRead(rows));
  return client;
}

/** `pane: false` is the drawer's list of cards, where a thread is read in its own row: the page
 *  reads one row at a time at every width. */
function renderInbox(client: QueryClient, { pane = true }: { pane?: boolean } = {}): void {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Inbox pane={pane} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The element the read mark watches: the wrapper around the thread's newest message. */
function newestTurn(id: string): Element {
  const turn = document.querySelector(`[data-inbox-thread-turn="${id}"]`);
  const watched = turn?.parentElement;
  if (watched === null || watched === undefined) throw new Error(`${id} is not drawn`);
  return watched;
}

function mockCommentReads(): void {
  spy("getIssue", { closed_at: null } as unknown as IssueDetails);
  spy("getCredentialPending", { pending: [] });
  spy("listComments", [
    comment("comment-1", "The retry budget is too small."),
    comment("comment-2", "Agreed.", {
      author: { id: "bob", kind: "user" },
      created_at: NEWEST,
      reply_to: "comment-1",
    }),
    comment("comment-9", "And the timeout."),
  ]);
}

test("a card read by the dwell rule stays in the list, drawn as read", async () => {
  const viewport = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  mockCommentReads();
  const row = commentRow("comment-1", "The retry budget");
  const client = seeded([row]);
  renderInbox(client, { pane: false });
  try {
    await screen.findByTestId("inbox-thread-comment");
    await waitFor(() => expect(document.querySelector('[aria-label="Unread"]')).not.toBeNull());

    await onFakeClock(() => dwellOn(viewport, newestTurn("comment-2")));
    expect(mark.mock.calls).toEqual([["comment-1", { read_through: NEWEST }, keyedAttempt]]);

    // The next read of the Inbox still holds the row - a thread stays until it is dismissed - and
    // says it is read now.
    act(() => {
      client.setQueryData(
        inboxThreadsQuery().queryKey,
        threadsRead([{ ...row, unread: false, unread_count: 0 }])
      );
    });
    expect(document.querySelector("[data-inbox-thread-card]")).not.toBeNull();
    expect(document.querySelector('[aria-label="Unread"]')).toBeNull();
    expect(screen.queryByLabelText("Inbox empty state")).not.toBeInTheDocument();
  } finally {
    viewport.restore();
  }
});

test("a row read in the column stays in the list after the selection moves off it", async () => {
  const user = userEvent.setup();
  const narrow = widenToReadingPane();
  const viewport = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  mockCommentReads();
  const rows = [commentRow("comment-1", "The retry budget"), commentRow("comment-9", "Timeouts")];
  const client = seeded(rows);
  renderInbox(client);
  try {
    // The Inbox selects the first row for the reader, and the column draws its thread.
    await screen.findByTestId("inbox-pane");
    await waitFor(() =>
      expect(document.querySelector('[data-inbox-pane-body="issue"]')).not.toBeNull()
    );

    await onFakeClock(() => dwellOn(viewport, newestTurn("comment-2")));
    expect(mark.mock.calls).toEqual([["comment-1", { read_through: NEWEST }, keyedAttempt]]);

    act(() => {
      client.setQueryData(
        inboxThreadsQuery().queryKey,
        threadsRead([
          { ...(rows[0] as MyReplyRow), unread: false, unread_count: 0 },
          rows[1] as MyReplyRow,
        ])
      );
    });
    const read = document.querySelector('[data-inbox-row="reply:comment-1"]');
    expect(read).not.toBeNull();
    expect(read?.querySelector('[aria-label="Unread"]')).toBeNull();

    // The selection moves off it: the row the reader has read is still listed, drawn as read.
    const other = document.querySelector<HTMLElement>('[data-inbox-row="reply:comment-9"]');
    await user.click(other as HTMLElement);
    await waitFor(() =>
      expect(
        document.querySelector('[data-inbox-row="reply:comment-9"][data-inbox-selected]')
      ).not.toBeNull()
    );
    expect(document.querySelector('[data-inbox-row="reply:comment-1"]')).not.toBeNull();
  } finally {
    viewport.restore();
    narrow();
  }
});

// A card's Mark read is a `PUT` of the thread's mark. One the reader stopped may have been saved,
// so the card neither says it failed nor shows the stopped write's own words, which are a
// composer's and speak of a message: it says what its Retry does with the mark.
test("a Mark read the reader stopped says what Retry does, not a message's words", async () => {
  answerThreadReads().mockRejectedValue(
    new NetworkError("/api/v1/me/threads/comment-1/read", new TypeError("Failed to fetch"))
  );
  mockCommentReads();
  // No thread is unread to the card's watcher, so the press is the one write the card makes.
  const client = seeded([]);
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ListedCards threads={[commentRow("comment-1", "The retry budget")]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await screen.findByTestId("inbox-thread-comment");
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(screen.getByRole("button", { name: "Mark read" }));
    await stopTheLateWrite(client);

    // The alert's paragraph, without its Retry button's name.
    expect(screen.getByRole("alert").querySelector("p")?.textContent).toBe(
      stoppedWriteNote("this thread's read mark", "saved", {
        method: "PUT",
        stopped: new SendStoppedError(),
      })
    );
  } finally {
    realClock();
  }
});

test("Mark read releases its hold when its landed mark's re-read reaches the request deadline", async () => {
  const answer = Promise.withResolvers<void>();
  const mark = answerThreadReads().mockReturnValue(answer.promise);
  mockCommentReads();
  const client = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const key = inboxThreadsQuery().queryKey;
  client.setQueryData(key, threadsRead([]));
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const online = onlineManager.isOnline();
  function ReadThreads() {
    useQuery({ queryKey: key, queryFn: async () => threadsRead([]) });
    return null;
  }
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReadThreads />
        <ListedCards threads={[commentRow("comment-1", "The retry budget")]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByTestId("inbox-thread-comment");
    jest.useFakeTimers();
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Mark read" });
    await userOnFakeClock().click(button);
    await waitFor(() => expect(mark).toHaveBeenCalledTimes(1));
    act(() => onlineManager.setOnline(false));
    await act(async () => answer.resolve());
    await tick(0);
    expect(client.getQueryState(key)?.fetchStatus).toBe("paused");
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(button, "Mark read")).toBe(true);

    await tick(1_000);
    expect(client.getQueryState(key)?.fetchStatus).toBe("paused");
    expect(heldNotDisabled(button, "Mark read")).toBe(false);
    expect(button).toHaveFocus();
    expect(screen.getByRole("alert")).toHaveTextContent("Dispatch could not be reached.");
  } finally {
    view.unmount();
    client.clear();
    act(() => onlineManager.setOnline(online));
    realClock();
  }
});

// The line a Mark read's re-read the bound ended draws is that mark's own: the next Mark read
// takes it away as its mark lands, though the earlier re-read is still out, and that mark's
// re-read answering says nothing more. Dispatch holds the first mark's re-read until the row
// answers it, so the next mark's re-read, which waits for that one (`DispatchQueryClient`),
// answers after it.
test("the line a Mark read's re-read the bound ended draws goes as the next mark lands, whose re-read answers", async () => {
  // Each mark's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: marks, next } = heldAnswers<void>();
  const mark = answerThreadReads().mockImplementation(next);
  mockCommentReads();
  const client = new DispatchQueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const key = inboxThreadsQuery().queryKey;
  client.setQueryData(key, threadsRead([]));
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const reread = Promise.withResolvers<MyRepliesResponse>();
  let reads = 0;
  function ReadThreads() {
    useQuery({
      queryKey: key,
      queryFn: () => {
        reads += 1;
        return reads === 1 ? reread.promise : Promise.resolve(threadsRead([]));
      },
    });
    return null;
  }
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReadThreads />
        <ListedCards threads={[commentRow("comment-1", "The retry budget")]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const why = "Could not load your inbox. Dispatch did not answer within 30 seconds.";
  try {
    await screen.findByTestId("inbox-thread-comment");
    jest.useFakeTimers();
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Mark read" });
    const user = userOnFakeClock();
    await user.click(button);
    await tick(0);
    await act(async () => marks[0]?.());
    await tick(0);
    expect(reads).toBe(1);
    await tick(REQUEST_DEADLINE_MS);
    expect(heldNotDisabled(button, "Mark read")).toBe(false);
    expect(screen.getByRole("alert")).toHaveTextContent(why);

    await user.click(button);
    await tick(0);
    expect(mark).toHaveBeenCalledTimes(2);
    await act(async () => marks[1]?.());
    await tick(0);
    expect(heldNotDisabled(button, "Mark read")).toBe(true);
    expect(
      screen.queryByRole("alert"),
      "the earlier mark's re-read kept its line past the next mark"
    ).toBeNull();

    await act(async () => reread.resolve(threadsRead([])));
    await tick(0);
    expect(heldNotDisabled(button, "Mark read")).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  } finally {
    view.unmount();
    client.clear();
    realClock();
  }
});

// Every Mark read reads the Inbox's one list again, so the line the first card draws for a read
// the bound ended goes as a second card's mark lands, though that read is still out: the list's
// one hold forgets it then (`useListReadBound`), and the second card alone says why the read its
// own mark made has not settled, once that read's bound ends.
test("the line one card's Mark read leaves goes as another card's mark lands", async () => {
  // Each mark's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: marks, next } = heldAnswers<void>();
  const mark = answerThreadReads().mockImplementation(next);
  mockCommentReads();
  // Dispatch answers no read of the list while the row runs.
  restores.push(
    spyOn(api, "listMyReplies").mockReturnValue(Promise.withResolvers<MyRepliesResponse>().promise)
  );
  const read = (row: MyReplyRow): MyReplyRow => ({ ...row, unread: false, unread_count: 0 });
  const client = seeded(
    [read(commentRow("comment-1", "The retry budget")), read(commentRow("comment-9", "Timeouts"))],
    new DispatchQueryClient({
      defaultOptions: {
        mutations: { retry: false },
        queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
      },
    })
  );
  // Two cards' Mark read: the cards are the list's that selects nothing, the Peek drawer's.
  renderInbox(client, { pane: false });
  const why = "Could not load your inbox. Dispatch did not answer within 30 seconds.";
  try {
    await waitFor(() => expect(screen.getAllByTestId("inbox-thread-comment")).toHaveLength(2));
    const card = (id: string) =>
      document.querySelector<HTMLElement>(`[data-inbox-row="reply:${id}"]`) as HTMLElement;
    const firstCard = card("comment-1");
    const secondCard = card("comment-9");
    const first = within(firstCard).getByRole<HTMLButtonElement>("button", { name: "Mark read" });
    const second = within(secondCard).getByRole<HTMLButtonElement>("button", {
      name: "Mark read",
    });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(first);
    await tick(0);
    await act(async () => marks[0]?.());
    await tick(0);
    await tick(REQUEST_DEADLINE_MS);
    expect(within(firstCard).getByRole("alert")).toHaveTextContent(why);

    await user.click(second);
    await tick(0);
    expect(mark).toHaveBeenCalledTimes(2);
    await act(async () => marks[1]?.());
    await tick(0);
    expect(heldNotDisabled(second, "Mark read")).toBe(true);
    expect(
      within(firstCard).queryByRole("alert"),
      "the first mark's re-read kept its line on its card past the second mark"
    ).toBeNull();

    await tick(REQUEST_DEADLINE_MS);
    expect(within(secondCard).getByRole("alert")).toHaveTextContent(why);
    expect(within(firstCard).queryByRole("alert")).toBeNull();
  } finally {
    cleanup();
    client.clear();
    realClock();
  }
});

// The Peek drawer draws nothing while it is closed (`InboxDrawer`), so the drawer opened again
// draws a new list of cards while a Mark read's re-read is still out. That read has one hold for
// every list of it (`useListReadBound`), so the card drawn again holds its Mark read through the
// read as the card it was pressed on did, and says why at the bound and after, until the read
// answers (the tester's row of `inbox-read-state.e2e.ts`).
test("a card drawn again while its Mark read's re-read is out holds Mark read through it, and says why at the bound until it answers", async () => {
  // Each mark's answer, held until the row gives it, so what it sets off runs in `act`.
  const { answers: marks, next } = heldAnswers<void>();
  answerThreadReads().mockImplementation(next);
  mockCommentReads();
  // The mark's re-read of the list, held until the row answers it.
  const reread = Promise.withResolvers<MyRepliesResponse>();
  restores.push(spyOn(api, "listMyReplies").mockReturnValue(reread.promise));
  const thread = { ...commentRow("comment-1", "The retry budget"), unread: false, unread_count: 0 };
  const client = seeded(
    [thread],
    new DispatchQueryClient({
      defaultOptions: {
        mutations: { retry: false },
        queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
      },
    })
  );
  // The drawer's list of cards, closed and drawn again as the drawer is.
  const drawAgain = () => {
    cleanup();
    renderInbox(client, { pane: false });
  };
  renderInbox(client, { pane: false });
  const why = "Could not load your inbox. Dispatch did not answer within 30 seconds.";
  const card = () =>
    document.querySelector<HTMLElement>('[data-inbox-row="reply:comment-1"]') as HTMLElement;
  const markOf = () =>
    card().querySelector<HTMLButtonElement>('[data-inbox-action="mark-read"]') as HTMLButtonElement;
  try {
    await screen.findByTestId("inbox-thread-comment");
    jest.useFakeTimers();
    await userOnFakeClock().click(markOf());
    await tick(0);
    await act(async () => marks[0]?.());
    await tick(0);

    drawAgain();
    await tick(0);
    expect(
      heldNotDisabled(markOf(), "Mark read"),
      "the card drawn again let Mark read go while its re-read was out"
    ).toBe(true);
    expect(markOf()).toHaveTextContent("Marking read…");

    await tick(REQUEST_DEADLINE_MS);
    expect(heldNotDisabled(markOf(), "Mark read")).toBe(false);
    expect(markOf()).toHaveTextContent("Mark read");
    expect(within(card()).getByRole("alert")).toHaveTextContent(why);
    drawAgain();
    await tick(0);
    expect(
      within(card()).queryByRole("alert"),
      "the card drawn again after the bound said nothing of the re-read still out"
    ).toHaveTextContent(why);

    await act(async () => reread.resolve(threadsRead([thread])));
    await tick(0);
    expect(within(card()).queryByRole("alert")).toBeNull();
  } finally {
    cleanup();
    client.clear();
    realClock();
  }
});

// A mark that never answers - offline, each try failing at the network - holds Mark read until
// its deadline and no longer: past it the mark is still tried, and a press then marks again.
test("Mark read lets go at the mark's write deadline and marks again", async () => {
  const mark = answerThreadReads().mockRejectedValue(
    new NetworkError("/api/v1/me/threads/comment-1/read", new TypeError("Failed to fetch"))
  );
  mockCommentReads();
  const client = seeded([]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ListedCards threads={[commentRow("comment-1", "The retry budget")]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByTestId("inbox-thread-comment");
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Mark read" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([mark]);
    await heldUntilLate(user, button, {
      label: "Thread marked read",
      name: "Mark read",
      queryClient: client,
      sent,
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent(), "a press past the deadline marks again").toBe(2);
  } finally {
    realClock();
    view.unmount();
    client.clear();
  }
});

// The row moves on while a stopped mark's refusal is on screen: a newer reply arrives and the
// list read again names it. The card's Retry is still the stopped mark, so it marks the thread
// through the reply that mark was of, not through the newer one the card draws by then.
test("a Mark read's Retry marks what the stopped press marked, not the row's newer reply", async () => {
  const mark = answerThreadReads().mockRejectedValue(
    new NetworkError("/api/v1/me/threads/comment-1/read", new TypeError("Failed to fetch"))
  );
  mockCommentReads();
  const client = seeded([]);
  const row = commentRow("comment-1", "The retry budget");
  const card = (thread: MyReplyRow) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ListedCards threads={[thread]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(card(row));
  await screen.findByTestId("inbox-thread-comment");
  jest.useFakeTimers();
  try {
    const user = userOnFakeClock();
    await user.click(screen.getByRole("button", { name: "Mark read" }));
    await stopTheLateWrite(client);
    const stoppedKey = mark.mock.calls[0]?.[2]?.key;
    const stopped = mark.mock.calls.length;

    view.rerender(
      card({
        ...row,
        reply: { ...row.reply, created_at: "2026-10-12T01:30:00Z", id: "comment-1-later" },
      })
    );
    mark.mockResolvedValue(undefined);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await tick(1_000);

    expect(mark).toHaveBeenCalledTimes(stopped + 1);
    const again = mark.mock.calls.at(-1);
    expect(again?.[1]).toEqual({ read_through: NEWEST });
    expect(again?.[2]?.key).toBe(stoppedKey);
  } finally {
    realClock();
  }
});

const session: Agent = {
  capabilities: ["steer", "btw"],
  dir: "/work",
  host: "box",
  last_activity: "2026-10-12T00:00:00Z",
  last_seen: Date.now(),
  machine_id: "box",
  open_asks: 0,
  roles: [],
  session_id: "planner",
  state: "live",
  title: "Planner",
};

function message(id: string, body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    broadcast_id: null,
    created_at: "2026-10-12T00:00:00Z",
    deliveries: [],
    ...messageCorrectionFields(),
    id,
    in_reply_to: null,
    issue_key: null,
    reply_block: null,
    target: "session:planner",
    ...overrides,
  };
}

test("an Agent thread is read by reply id, and moves no session read mark", async () => {
  const viewport = installIntersectionObserver();
  const state = answerAgentStates().mockResolvedValue({ unread_replies: 0 });
  const threadMark = answerThreadReads().mockResolvedValue(undefined);
  spy("listAgents", [session]);
  spy("listAgentMessages", [
    {
      message: message("message-1", "Have a look at the build"),
      replies: [
        message("reply-1", "Looked at it.", {
          author: { id: "planner", kind: "session" },
          created_at: NEWEST,
          in_reply_to: "message-1",
        }),
      ],
    } satisfies MessageRead,
  ]);
  const client = seeded([]);
  // The server still counts a reply of this session's unread, so the mark is wanted.
  client.setQueryData(userAgentStateQuery().queryKey, {
    planner: { unread_replies: 1 },
  });
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ListedCards threads={[plannerAgentRow()]} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await screen.findByTestId("inbox-thread-agent");
    await waitFor(() =>
      expect(document.querySelector('[data-inbox-thread-turn="reply-1"]')).not.toBeNull()
    );

    await onFakeClock(() => dwellOn(viewport, newestTurn("reply-1")));

    // By id, and by id alone: no `read_through` for the session, and no thread mark either.
    expect(state.mock.calls).toEqual([["planner", { read_replies: ["reply-1"] }, keyedAttempt]]);
    expect(threadMark).not.toHaveBeenCalled();
  } finally {
    viewport.restore();
  }
});
