import { afterEach, beforeAll, expect, jest, test } from "bun:test";
import { meInboxThreads } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle } from "../../__tests__/fake-clock";
import { answerInboxReads } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, NetworkError } from "../../api/client";
import {
  meInboxCountQuery,
  meInboxDismissedQuery,
  meInboxKey,
  meInboxOwnerQuery,
  meInboxQuery,
  whoAmIQuery,
} from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { DispatchEvent, MeInboxResponse, MyReplyRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { stoppedWriteNote } from "../writes/keyed-write";
import { InboxBar, type InboxFilters } from "./InboxBar";
import {
  answerOneRead,
  ONE_READ_AS_OF,
  oneReadAnswer,
  oneReadDismissedPages,
  oneReadReply,
} from "./inbox-fixture";
import type { InboxListRow } from "./list-row";
import { MARK_ALL_REFUSED_TEXT, markAllReadBody, marksThread, useMarkAllRead } from "./mark-all";
import { useUnreadThreadCount } from "./useInboxCount";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

const AS_OF = ONE_READ_AS_OF;

function noop(): void {}

/**
 * The bar over one list, with the one number the sidebar's Inbox count and the compact header's
 * Inbox link both read beside it (`useUnreadThreadCount` over the one read's `meInboxCountQuery`,
 * which is what `Sidebar.tsx` and `app.tsx` each add their Needs-you count to). The list is the
 * reader's Reply and Agent rows of the one read, which the Inbox draws them from. The bar is the
 * surface the button and its refusal live on; the press itself is the Inbox's own hook.
 */
function Bar({ filters = {} }: { filters?: InboxFilters }): ReactNode {
  const threads = meInboxThreads(useQuery(meInboxQuery()).data);
  const rows: readonly InboxListRow[] = threads.map((thread) => ({ kind: "thread", thread }));
  const markAll = useMarkAllRead({ asOf: AS_OF, filters, rows });
  const count = useUnreadThreadCount();
  return (
    <>
      <output data-testid="inbox-count">{count}</output>
      <output data-testid="listed">{rows.length}</output>
      <output data-testid="unread">{threads.filter((t) => t.unread).length}</output>
      <InboxBar
        agentToken={undefined}
        assignees={undefined}
        filters={filters}
        markAll={markAll}
        onAssigneesChange={noop}
        onClearAgent={noop}
        onFiltersChange={noop}
        onPopoverOpenChange={noop}
        rows={rows}
        selection={["alice"]}
        view={{
          group: "priority",
          onGroupChange: noop,
          onShowChange: noop,
          onSortChange: noop,
          onThreadOrderChange: noop,
          show: "inbox",
          sort: "newest",
          threadOrder: "oldest",
        }}
        viewer="alice"
      />
    </>
  );
}

/** What every client of this file is made with: a read the row seeds is never stale, so nothing
 *  reads the server but what the row asks for. */
const CLIENT_OPTIONS = {
  defaultOptions: {
    mutations: { retry: false },
    queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
  },
};

/** The bar over `rows`, the shared count at `count`, under `filters`; `narrow` draws it again
 *  under other filters, over the same client, as the reader's Filter does. */
function renderBar(
  rows: readonly MyReplyRow[],
  count: number,
  filters?: InboxFilters,
  client: QueryClient = new QueryClient(CLIENT_OPTIONS)
) {
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer(rows));
  client.setQueryData(meInboxCountQuery().queryKey, count);
  const tree = (narrowed: InboxFilters | undefined): ReactNode => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Bar filters={narrowed} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(filters));
  return { client, narrow: (narrowed: InboxFilters) => view.rerender(tree(narrowed)) };
}

function inboxCount(): string {
  return screen.getByTestId("inbox-count").textContent ?? "";
}

function listed(): string {
  return screen.getByTestId("listed").textContent ?? "";
}

function unread(): string {
  return screen.getByTestId("unread").textContent ?? "";
}

test("the body carries the list's read mark and the filters the list is under", () => {
  // Nothing narrowed: both kinds, and no list for the three fields, which the route reads as
  // every value.
  expect(markAllReadBody({}, AS_OF)).toEqual({ kind: ["reply", "agent"], read_through: AS_OF });
  expect(
    markAllReadBody(
      { priority: ["P0", "none"], project: ["CORE"], type: ["reply"], urgency: ["none"] },
      AS_OF
    )
  ).toEqual({
    kind: ["reply"],
    priority: ["P0", "none"],
    project: ["CORE"],
    read_through: AS_OF,
    urgency: ["none"],
  });
});

test("a thread written in since the read mark, or already read, is not one this press marks", () => {
  const body = markAllReadBody({}, AS_OF);
  const row = replyRow("c1");
  expect(marksThread(row, body)).toBe(true);
  expect(
    marksThread({ ...row, reply: { ...row.reply, created_at: "2026-10-12T03:00:00Z" } }, body)
  ).toBe(false);
  expect(marksThread({ ...row, unread: false }, body)).toBe(false);
  // An Agent row carries no project, no priority and no urgency, so a filter naming a value for
  // one of them leaves it out and a filter naming `none` keeps it.
  expect(marksThread(plannerAgentRow(), markAllReadBody({ project: ["CORE"] }, AS_OF))).toBe(false);
  expect(marksThread(plannerAgentRow(), markAllReadBody({ project: ["none"] }, AS_OF))).toBe(true);
});

test("the button shows only while the list holds an unread Reply or Agent row", () => {
  renderBar([replyRow("c1", { unread: false })], 0);
  expect(screen.queryByRole("button", { name: "Mark all read" })).not.toBeInTheDocument();
  cleanup();
  renderBar([replyRow("c1")], 1);
  expect(screen.getByRole("button", { name: "Mark all read" })).toBeTruthy();
});

test("one press zeroes the count and draws the rows read, without taking one out of the list", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  renderBar([replyRow("c1", { priority: 1 }), replyRow("c2", { priority: 1 })], 2, {
    priority: ["P1"],
  });

  expect(inboxCount()).toBe("2");
  await user.click(screen.getByRole("button", { name: "Mark all read" }));

  // At once, with the request still out: every badge reading the shared count is at zero and no
  // row is unread any more - and both rows are still in the list, where a thread stays until it
  // is dismissed.
  await waitFor(() => expect(unread()).toBe("0"));
  expect(listed()).toBe("2");
  expect(inboxCount()).toBe("0");
  expect(screen.queryByRole("button", { name: "Mark all read" })).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();

  expect(marked.mock.calls[0]?.[0]).toEqual({
    kind: ["reply", "agent"],
    priority: ["P1"],
    read_through: AS_OF,
  });
  settle.resolve({ marked: 2 });
  await waitFor(() => expect(marked).toHaveBeenCalledTimes(1));
});

test("a refused press brings the rows and the counts back and says nothing was marked", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  // An old server mid-deploy has no such route, which is a failed request like any other.
  marked.mockRejectedValue(new ApiError(404, { error: "not found" }));
  renderBar([replyRow("c1"), replyRow("c2")], 2);

  await user.click(screen.getByRole("button", { name: "Mark all read" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe(MARK_ALL_REFUSED_TEXT);
  expect(MARK_ALL_REFUSED_TEXT).toBe("Nothing was marked read.");
  // The sidebar's Inbox count and the compact header's Inbox link hold their pre-click number
  // again, and the rows are back in the list with the button over them.
  expect(inboxCount()).toBe("2");
  expect(unread()).toBe("2");
  expect(listed()).toBe("2");
});

// Two presses out at once over two narrowings of the list - its Reply rows, then its Agent rows -
// each refused in turn. A refusal draws unread again the rows its own press drew read, and raises
// the count by them, and nothing else: had it put every read back as its press found it, the first
// refusal would draw the second press's row unread while that press is still out, and the second
// would draw the first press's row read again, though Dispatch marked neither, with the count one
// too low.
test("two presses refused in a row each draw their own rows unread again, and the count as it was", async () => {
  const reply = replyRow("c1");
  const agent = plannerAgentRow();
  const client = new QueryClient(CLIENT_OPTIONS);
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([reply, agent]));
  client.setQueryData(meInboxCountQuery().queryKey, 2);
  const first = Promise.withResolvers<never>();
  const second = Promise.withResolvers<never>();
  const marked = answerInboxReads();
  marked.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  const rows: readonly InboxListRow[] = [
    { kind: "thread", thread: reply },
    { kind: "thread", thread: agent },
  ];
  const { result } = renderHook(
    () => ({
      agents: useMarkAllRead({ asOf: AS_OF, filters: { type: ["agent"] }, rows }),
      count: useQuery(meInboxCountQuery()).data,
      replies: useMarkAllRead({ asOf: AS_OF, filters: { type: ["reply"] }, rows }),
      unread: meInboxThreads(useQuery(meInboxQuery()).data)
        .filter((thread) => thread.unread)
        .map((thread) => thread.thread_id),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );

  act(() => result.current.replies.markAll());
  await waitFor(() => expect(result.current).toMatchObject({ count: 1, unread: ["message-1"] }));
  act(() => result.current.agents.markAll());
  await waitFor(() => expect(result.current).toMatchObject({ count: 0, unread: [] }));

  // The first press is refused while the second is still out: its own row is unread again, and
  // the second's is still drawn read.
  await act(async () => first.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() => expect(marked).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(result.current).toMatchObject({ count: 1, unread: ["c1"] }));

  await act(async () => second.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() =>
    expect(result.current).toMatchObject({ count: 2, unread: ["c1", "message-1"] })
  );
});

// A refused press takes back only what it wrote, read by read. It drew read the rows of every
// cached read of the one read - the Inbox answer and each owner's unread set - so each row goes
// back as its own read held it, not as the Inbox answer held it. A read that landed since the
// press holds what Dispatch holds, and stays as it landed: the issue's unread set, after the
// reader read c1 in another tab, and the Dismissed view, after they dismissed it there.
test("a refused press puts back each read's own rows, and leaves a read that landed since as it landed", async () => {
  const c1 = replyRow("c1");
  const c2 = replyRow("c2");
  const onDocument = replyRow("c2", { subject: "As the document's unread set drew it" });
  const issueSet = meInboxOwnerQuery({ issue: "CORE-1" }).queryKey;
  const documentSet = meInboxOwnerQuery({ artifact: "artifact-1" }).queryKey;
  const client = new QueryClient(CLIENT_OPTIONS);
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([c1, c2]));
  client.setQueryData(meInboxCountQuery().queryKey, 2);
  client.setQueryData(issueSet, oneReadAnswer([c1]));
  client.setQueryData(documentSet, oneReadAnswer([onDocument]));
  const refused = Promise.withResolvers<never>();
  answerInboxReads().mockReturnValue(refused.promise);
  const rows: readonly InboxListRow[] = [
    { kind: "thread", thread: c1 },
    { kind: "thread", thread: c2 },
  ];
  const { result } = renderHook(() => useMarkAllRead({ asOf: AS_OF, filters: {}, rows }), {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    ),
  });

  act(() => result.current.markAll());
  await waitFor(() => expect(client.getQueryData<number>(meInboxCountQuery().queryKey)).toBe(0));
  expect(meInboxThreads(client.getQueryData(documentSet))[0]?.unread).toBe(false);

  const readSince = replyRow("c1", {
    read_through: "2026-10-12T01:30:00Z",
    unread: false,
    unread_count: 0,
  });
  const dismissedSince = replyRow("c1", {
    dismissed_at: "2026-10-12T01:40:00Z",
    unread: false,
    unread_count: 0,
  });
  act(() => {
    client.setQueryData(issueSet, oneReadAnswer([readSince]));
    client.setQueryData(meInboxDismissedQuery().queryKey, oneReadDismissedPages([dismissedSince]));
  });

  await act(async () => refused.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() => expect(client.getQueryData<number>(meInboxCountQuery().queryKey)).toBe(2));
  expect(meInboxThreads(client.getQueryData(meInboxQuery().queryKey))).toEqual([c1, c2]);
  expect(meInboxThreads(client.getQueryData(documentSet))).toEqual([onDocument]);
  expect(meInboxThreads(client.getQueryData(issueSet))).toEqual([readSince]);
  const dismissed = client.getQueryData(meInboxDismissedQuery().queryKey);
  expect(dismissed?.pages.map((page) => meInboxThreads(page))).toEqual([[dismissedSince]]);
});

// The count can be older than the rows: read before c2 reached the Inbox answer, it says 1 over
// two unread rows. The press lowers it to 0 and no further, so a refusal gives back the 1 it took,
// not one for each row it draws unread again.
test("a refused press over a count below its unread rows gives back only what it took off the count", async () => {
  const client = new QueryClient(CLIENT_OPTIONS);
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([replyRow("c1"), replyRow("c2")]));
  client.setQueryData(meInboxCountQuery().queryKey, 1);
  const refused = Promise.withResolvers<never>();
  answerInboxReads().mockReturnValue(refused.promise);
  const { result } = renderHook(
    () => ({
      count: useQuery(meInboxCountQuery()).data,
      markAll: useMarkAllRead({ asOf: AS_OF, filters: {}, rows: [] }),
      unread: meInboxThreads(useQuery(meInboxQuery()).data)
        .filter((thread) => thread.unread)
        .map((thread) => thread.thread_id),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );

  act(() => result.current.markAll.markAll());
  await waitFor(() => expect(result.current).toMatchObject({ count: 0, unread: [] }));

  await act(async () => refused.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() => expect(result.current).toMatchObject({ count: 1, unread: ["c1", "c2"] }));
});

// A read of the list asked before the press - an event's refresh, say - answers the rows as they
// stood, unread. Landing after the press, it would draw both rows unread again under an Inbox
// count of zero while the press is still going through. The press gives it up and asks for the
// list again, and on Dispatch's own client (`main.tsx`) that read waits for the press's answer.
test("a read of the list asked before Mark all read does not put back the rows the press drew read", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  const after = [replyRow("c1", { unread: false }), replyRow("c2", { unread: false })];
  const asked = Promise.withResolvers<MeInboxResponse>();
  const getMeInbox = answerOneRead({ threads: after }).mockImplementationOnce(() => asked.promise);
  try {
    const { client } = renderBar(before, 2, undefined, new DispatchQueryClient(CLIENT_OPTIONS));
    let read = Promise.resolve();
    await act(async () => {
      read = client.refetchQueries({ queryKey: meInboxQuery().queryKey });
    });
    expect(getMeInbox).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(unread()).toBe("0"));
    // The read answers as Dispatch held the rows when it was asked: both unread.
    await act(async () => {
      asked.resolve(oneReadAnswer(before));
      await read;
    });
    expect(unread()).toBe("0");
    expect(listed()).toBe("2");
    // The list the press asked for again waits for its answer, and has asked Dispatch nothing.
    expect(getMeInbox).toHaveBeenCalledTimes(1);

    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    // That read and the press's own refresh of the list and the count each ask after the mark.
    await waitFor(() => expect(getMeInbox).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(unread()).toBe("0");
    expect(inboxCount()).toBe("0");
  } finally {
    getMeInbox.mockRestore();
  }
});

// A read of the shared count asked before the press answers the count as it stood. Landing after
// the press, it would put the sidebar's Inbox count and the compact header's Inbox link back at
// two over rows the press drew read. The press gives it up and asks for the count again, and on
// Dispatch's own client (`main.tsx`) that read waits for the press's answer.
test("a read of the unread count asked before Mark all read does not put back the count the press zeroed", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  const after = [replyRow("c1", { unread: false }), replyRow("c2", { unread: false })];
  const asked = Promise.withResolvers<MeInboxResponse>();
  const getMeInbox = answerOneRead({ threads: after }).mockImplementationOnce(() => asked.promise);
  try {
    const { client } = renderBar(before, 2, undefined, new DispatchQueryClient(CLIENT_OPTIONS));
    let read = Promise.resolve();
    await act(async () => {
      read = client.refetchQueries({ queryKey: meInboxCountQuery().queryKey });
    });
    expect(getMeInbox).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(inboxCount()).toBe("0"));
    // The read answers as Dispatch held the count when it was asked: two unread.
    await act(async () => {
      asked.resolve(oneReadReply({ state: "unread" }, { threads: before }));
      await read;
    });
    expect(inboxCount()).toBe("0");
    // The count the press asked for again waits for its answer, and has asked Dispatch nothing.
    expect(getMeInbox).toHaveBeenCalledTimes(1);

    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    // That read and the press's own refresh of the list and the count each ask after the mark.
    await waitFor(() => expect(getMeInbox).toHaveBeenCalledTimes(4));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(inboxCount()).toBe("0");
    expect(unread()).toBe("0");
  } finally {
    getMeInbox.mockRestore();
  }
});

// A read asked while the press is out - an event's refresh of the Inbox's threads, say - would
// reach Dispatch before the press marks anything, and draw the rows unread and the count back at
// two while the press is still going through. It waits for the press's answer, and then reads
// what Dispatch holds after it.
test("a read of the list or the count asked while Mark all read is out waits for its answer", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  // What Dispatch holds once the press is in: both rows read, and a reply written since.
  const after = [
    replyRow("c1", { unread: false }),
    replyRow("c2", { unread: false }),
    replyRow("c3"),
  ];
  let served = before;
  const getMeInbox = answerOneRead(() => ({ threads: served }));
  try {
    const { client } = renderBar(before, 2, undefined, new DispatchQueryClient(CLIENT_OPTIONS));
    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(unread()).toBe("0"));
    expect(inboxCount()).toBe("0");

    await act(async () => {
      void client.invalidateQueries({ queryKey: meInboxKey });
    });
    // Neither read has asked Dispatch anything, though both have started.
    expect(getMeInbox).not.toHaveBeenCalled();
    expect(client.getQueryState(meInboxQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(client.getQueryState(meInboxCountQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(unread()).toBe("0");
    expect(inboxCount()).toBe("0");

    served = after;
    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    await waitFor(() => expect(listed()).toBe("3"));
    expect(getMeInbox).toHaveBeenCalled();
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(unread()).toBe("1");
    expect(inboxCount()).toBe("1");
  } finally {
    getMeInbox.mockRestore();
  }
});

/** A `user_thread_state.updated` the live feed delivers: `payload.login`'s read of the Inbox's
 *  threads moved, of `payload.thread_id` alone or, with none, of every thread a Mark all read
 *  marked. */
function threadStateEvent(payload: { login: string; thread_id?: string }): DispatchEvent {
  return {
    actor: { id: payload.login, kind: "user" },
    created_at: "2026-10-12T02:01:00Z",
    id: 1,
    issue_key: null,
    notify: false,
    payload,
    seq: 1,
    type: "user_thread_state.updated",
  } as DispatchEvent;
}

/** A `user_agent_state.updated` the live feed delivers: `payload.login`'s Clear, read mark or pin
 *  of session `payload.session_id` moved. */
function agentStateEvent(payload: { login: string; session_id: string }): DispatchEvent {
  return {
    actor: { id: payload.login, kind: "user" },
    created_at: "2026-10-12T02:01:00Z",
    id: 1,
    issue_key: null,
    notify: false,
    payload,
    seq: 1,
    type: "user_agent_state.updated",
  } as DispatchEvent;
}

// Dispatch publishes a Mark all read's thread-state event, which names no thread, once it has
// marked the rows, so a read asked after it answers the press. The hold ends on that event though
// the press's answer is still out, and a reply thread somebody else started meanwhile shows then
// rather than wait for an answer that may be late. The viewer's read of one thread in another tab,
// another login's Mark all read, and the viewer's pin of an agent (an agent-state event, which a
// press publishes too, but so does that pin) are not this press's and leave the hold.
test("a reply thread somebody else starts while Mark all read is out shows once the press's thread-state event has come", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settled = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settled.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  let served = before;
  const getMeInbox = answerOneRead(() => ({ threads: served }));
  try {
    const { client } = renderBar(before, 2, undefined, new DispatchQueryClient(CLIENT_OPTIONS));
    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(unread()).toBe("0"));
    expect(inboxCount()).toBe("0");

    // Dispatch has marked both rows, and bob has started a reply thread to alice since.
    const started = replyRow("c3");
    served = [
      replyRow("c1", { unread: false }),
      replyRow("c2", { unread: false }),
      { ...started, reply: { ...started.reply, author: { id: "bob", kind: "user" } } },
    ];
    await act(async () => {
      hearLiveEvent(client, threadStateEvent({ login: "alice", thread_id: "c1" }), "alice");
      hearLiveEvent(client, threadStateEvent({ login: "bob" }), "alice");
      hearLiveEvent(client, agentStateEvent({ login: "alice", session_id: "planner" }), "alice");
      void client.invalidateQueries({ queryKey: meInboxKey });
      await settle();
    });
    // None of those events is the press's: both reads have started, and neither has asked
    // Dispatch.
    expect(getMeInbox).not.toHaveBeenCalled();
    expect(client.getQueryState(meInboxQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(client.getQueryState(meInboxCountQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(listed()).toBe("2");

    await act(async () => {
      hearLiveEvent(client, threadStateEvent({ login: "alice" }), "alice");
      void client.invalidateQueries({ queryKey: meInboxKey });
    });
    // The press's answer is still out, and the list and the count are what Dispatch holds: the
    // press's two rows read, and bob's thread unread.
    await waitFor(() => expect(listed()).toBe("3"));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(getMeInbox).toHaveBeenCalled();
    expect(client.isMutating()).toBe(1);
    expect(unread()).toBe("1");
    expect(inboxCount()).toBe("1");

    await act(async () => {
      settled.resolve({ marked: 2 });
    });
    await waitFor(() => expect(client.isMutating()).toBe(0));
    await waitFor(() => expect(client.isFetching()).toBe(0));
  } finally {
    getMeInbox.mockRestore();
  }
});

// A press is the list's under the narrowing it was made over. Its refusal is said beside that
// list alone: once the reader narrows the list another way, the bar says nothing of a press they
// did not make there, and the refusal does not come back when they return to the first list.
test("a refused press's word goes once the reader narrows the list another way", async () => {
  const user = userEvent.setup();
  answerInboxReads().mockRejectedValue(new ApiError(404, { error: "not found" }));
  const bar = renderBar([replyRow("c1", { priority: 1 }), replyRow("c2")], 2);

  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  expect((await screen.findByRole("alert")).textContent).toBe(MARK_ALL_REFUSED_TEXT);

  bar.narrow({ priority: ["P1"] });
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Mark all read" })).toBeTruthy();
  bar.narrow({});
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

// A press the reader stopped is no refusal: it may have marked the rows, and here it had. So the
// bar does not say nothing was marked, and the rows are not drawn unread again: the list and its
// count are read again from Dispatch, which holds them read.
test("a stopped press reads the list again rather than saying nothing was marked", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  answerInboxReads().mockRejectedValue(
    new NetworkError("/api/v1/me/inbox/read", new TypeError("Failed to fetch"))
  );
  const reread = answerOneRead({
    threads: [replyRow("c1", { unread: false }), replyRow("c2", { unread: false })],
  });
  const { client } = renderBar([replyRow("c1"), replyRow("c2")], 2);
  try {
    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await stopTheLateWrite(client);

    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the read mark", "saved", {
        anew: "The Inbox is read again from Dispatch, and Mark all read sends a new mark for any thread still unread.",
      })
    );
    expect(reread).toHaveBeenCalled();
    expect(unread()).toBe("0");
    expect(inboxCount()).toBe("0");
    expect(listed()).toBe("2");
  } finally {
    reread.mockRestore();
    realClock();
  }
});
