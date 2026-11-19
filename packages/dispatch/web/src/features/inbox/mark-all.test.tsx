import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { answerInboxReads } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import {
  inboxThreadsQuery,
  myRepliesCountQuery,
  myRepliesKey,
  whoAmIQuery,
} from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { InboxBar, type InboxFilters } from "./InboxBar";
import type { InboxListRow } from "./list-row";
import { MARK_ALL_REFUSED_TEXT, markAllReadBody, marksThread, useMarkAllRead } from "./mark-all";
import { useUnreadThreadCount } from "./useInboxCount";

afterEach(cleanup);

const AS_OF = "2026-10-12T02:00:00Z";

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** What `GET /api/v1/me/replies` answers while Dispatch holds `rows`: the Inbox's list, or for
 *  the one-row page the shared count reads (`limit: 1`, `myRepliesCountQuery`) the number of
 *  them still unread. */
function repliesAnswer(
  page: { readonly limit?: number },
  rows: readonly MyReplyRow[]
): MyRepliesResponse {
  if (page.limit !== 1) return threadsRead(rows);
  return { ...threadsRead([]), limit: 1, total: rows.filter((row) => row.unread).length };
}

function noop(): void {}

/**
 * The bar over one list, with the one number the sidebar's Inbox count and the compact header's
 * Inbox link both read beside it (`useUnreadThreadCount` over `myRepliesCountQuery`, which is
 * what `Sidebar.tsx` and `app.tsx` each add their Needs-you count to). The bar is the surface
 * the button and its refusal live on; the press itself is the Inbox's own hook.
 */
function Bar({ filters = {} }: { filters?: InboxFilters }): ReactNode {
  const read = useQuery(inboxThreadsQuery());
  const rows: readonly InboxListRow[] = (read.data?.rows ?? []).map((thread) => ({
    kind: "thread",
    thread,
  }));
  const markAll = useMarkAllRead({ asOf: AS_OF, filters, rows });
  const count = useUnreadThreadCount();
  return (
    <>
      <output data-testid="inbox-count">{count}</output>
      <output data-testid="listed">{rows.length}</output>
      <output data-testid="unread">{(read.data?.rows ?? []).filter((t) => t.unread).length}</output>
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

function renderBar(
  rows: readonly MyReplyRow[],
  count: number,
  filters?: InboxFilters,
  client: QueryClient = new QueryClient(CLIENT_OPTIONS)
) {
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(rows));
  client.setQueryData(myRepliesCountQuery().queryKey, count);
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Bar filters={filters} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return client;
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

// A read of the list asked before the press - an event's refresh, say - answers the rows as they
// stood, unread. Landing after the press, it would draw both rows unread again under an Inbox
// count of zero while the press is still going through.
test("a read of the list asked before Mark all read does not put back the rows the press drew read", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  const after = [replyRow("c1", { unread: false }), replyRow("c2", { unread: false })];
  const asked = Promise.withResolvers<MyRepliesResponse>();
  const listMyReplies = spyOn(api, "listMyReplies")
    .mockImplementationOnce(() => asked.promise)
    .mockImplementation(async (page) => repliesAnswer(page, after));
  try {
    const client = renderBar(before, 2);
    let read = Promise.resolve();
    await act(async () => {
      read = client.refetchQueries({ queryKey: inboxThreadsQuery().queryKey });
    });
    expect(listMyReplies).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(unread()).toBe("0"));
    // The read answers as Dispatch held the rows when it was asked: both unread.
    await act(async () => {
      asked.resolve(threadsRead(before));
      await read;
    });
    expect(unread()).toBe("0");
    expect(listed()).toBe("2");

    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    // The press's own refresh reads the list and the count again, after the mark.
    await waitFor(() => expect(listMyReplies).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(unread()).toBe("0");
    expect(inboxCount()).toBe("0");
  } finally {
    listMyReplies.mockRestore();
  }
});

// A read of the shared count asked before the press answers the count as it stood. Landing after
// the press, it would put the sidebar's Inbox count and the compact header's Inbox link back at
// two over rows the press drew read.
test("a read of the unread count asked before Mark all read does not put back the count the press zeroed", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  const before = [replyRow("c1"), replyRow("c2")];
  const after = [replyRow("c1", { unread: false }), replyRow("c2", { unread: false })];
  const asked = Promise.withResolvers<MyRepliesResponse>();
  const listMyReplies = spyOn(api, "listMyReplies")
    .mockImplementationOnce(() => asked.promise)
    .mockImplementation(async (page) => repliesAnswer(page, after));
  try {
    const client = renderBar(before, 2);
    let read = Promise.resolve();
    await act(async () => {
      read = client.refetchQueries({ queryKey: myRepliesCountQuery().queryKey });
    });
    expect(listMyReplies).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(inboxCount()).toBe("0"));
    // The read answers as Dispatch held the count when it was asked: two unread.
    await act(async () => {
      asked.resolve(repliesAnswer({ limit: 1 }, before));
      await read;
    });
    expect(inboxCount()).toBe("0");

    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    await waitFor(() => expect(listMyReplies).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(inboxCount()).toBe("0");
    expect(unread()).toBe("0");
  } finally {
    listMyReplies.mockRestore();
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
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async (page) =>
    repliesAnswer(page, served)
  );
  try {
    const client = renderBar(before, 2, undefined, new DispatchQueryClient(CLIENT_OPTIONS));
    await user.click(screen.getByRole("button", { name: "Mark all read" }));
    await waitFor(() => expect(unread()).toBe("0"));
    expect(inboxCount()).toBe("0");

    await act(async () => {
      void client.invalidateQueries({ queryKey: myRepliesKey });
    });
    // Neither read has asked Dispatch anything, though both have started.
    expect(listMyReplies).not.toHaveBeenCalled();
    expect(client.getQueryState(inboxThreadsQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(client.getQueryState(myRepliesCountQuery().queryKey)?.fetchStatus).toBe("fetching");
    expect(unread()).toBe("0");
    expect(inboxCount()).toBe("0");

    served = after;
    await act(async () => {
      settle.resolve({ marked: 2 });
    });
    await waitFor(() => expect(listed()).toBe("3"));
    expect(listMyReplies).toHaveBeenCalled();
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(unread()).toBe("1");
    expect(inboxCount()).toBe("1");
  } finally {
    listMyReplies.mockRestore();
  }
});
