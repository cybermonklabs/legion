/**
 * The optimistic writes over the one read (`one-read-cache.ts`): each
 * write that moves one of the reader's own rows on the press moves it in the one read's cache
 * too, puts it back where the server refuses the write, and reads the one read again once the
 * server has decided. A hand-on and an assignee that make an ask wait on the reader add it to the
 * one read, which is where the Inbox draws the reader's own asks from, so the row stays in their
 * list while the write is out.
 */

import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import { type ReactNode, useRef } from "react";
import { MemoryRouter } from "react-router-dom";

import { settle } from "../../__tests__/fake-clock";
import {
  answerInboxDismisses,
  answerInboxReads,
  answerIssuePatches,
  answerRoute,
} from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import {
  meInboxCountQuery,
  meInboxKey,
  meInboxQuery,
  openAsksQuery,
  whoAmIQuery,
} from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { InboxReadResponse, MeInboxResponse, MyReplyRow, OpenAskRow } from "../../api/types";
import { issuePriority } from "../issue/issue-priority";
import { useIssueAssignee } from "../issue/useIssueAssignee";
import { useIssuePriority } from "../issue/useIssuePriority";
import { replyRow } from "../threads/replies-fixture";
import { SnoozeWrites } from "./ask-snooze-fixture";
import {
  answerOneRead,
  answerOpenAsks,
  issueAsk,
  ONE_READ_AS_OF,
  oneReadAnswer,
  oneReadReply,
} from "./inbox-fixture";
import { useMarkAllRead } from "./mark-all";
import { useInboxAsks } from "./one-read";
import { AskDeclineWritesContext, useOutstandingAnswerWrite } from "./outstanding-answers";
import { useAskHandOn } from "./useAskHandOn";
import { useAskSnooze, useAskSnoozeMany } from "./useAskSnooze";
import { useUnreadThreadCount } from "./useInboxCount";
import { dismissRefusalText, dismissThreadRow, useInboxDismiss } from "./useInboxDismiss";
import { useInboxThreadRows } from "./useInboxThreadRows";

const AS_OF = ONE_READ_AS_OF;

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

/** One answer of the one read, its entries carrying `asks` and `threads`. */
function oneRead(
  asks: readonly OpenAskRow[],
  threads: readonly MyReplyRow[] = []
): MeInboxResponse {
  return oneReadAnswer([...asks, ...threads]);
}

/** What every client of this file is made with: a read the row seeds is never stale, so nothing
 *  reads the server but what the row asks for. */
const CLIENT_OPTIONS = {
  defaultOptions: {
    mutations: { retry: false },
    queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
  },
};

/** `client`, signed in as alice and holding the read of the open asks `inbox`, and the one read's
 *  Inbox answer and unread count. */
function seeded(
  inbox: readonly OpenAskRow[],
  answer: MeInboxResponse,
  unread = 0,
  client: QueryClient = new QueryClient(CLIENT_OPTIONS)
): QueryClient {
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(openAsksQuery().queryKey, [...inbox]);
  client.setQueryData(meInboxQuery().queryKey, answer);
  client.setQueryData(meInboxCountQuery().queryKey, unread);
  return client;
}

function wrapper(client: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** `wrapper`, with the snooze writes the Inbox holds by ask around every Snooze it draws
 *  (`SnoozeWrites`): a row's snooze reads them, and no Snooze is drawn outside an Inbox. */
function snoozeWrapper(client: QueryClient) {
  const Around = wrapper(client);
  return ({ children }: { children: ReactNode }) => (
    <Around>
      <SnoozeWrites>{children}</SnoozeWrites>
    </Around>
  );
}

/** The one read's cached Inbox answer. */
function cached(client: QueryClient): MeInboxResponse | undefined {
  return client.getQueryData<MeInboxResponse>(meInboxQuery().queryKey);
}

function cachedAsk(client: QueryClient, id: string): OpenAskRow | undefined {
  return cached(client)?.entries?.find((entry) => entry.ask?.id === id)?.ask;
}

function cachedThread(client: QueryClient, id: string): MyReplyRow | undefined {
  return cached(client)?.entries?.find((entry) => entry.thread?.thread_id === id)?.thread;
}

/** A write's answer the test decides: `refuse` rejects it as the server refusing it. */
function deferred<T>() {
  const answer = Promise.withResolvers<T>();
  return {
    promise: answer.promise,
    refuse: () => answer.reject(new ApiError(409, { error: "refused" })),
    resolve: answer.resolve,
  };
}

/** Whether the one read's Inbox answer is marked to be read again. */
function invalidated(client: QueryClient): boolean {
  return client.getQueryState(meInboxQuery().queryKey)?.isInvalidated === true;
}

/** The one read's cached unread count. */
function unreadCount(client: QueryClient): number | undefined {
  return client.getQueryData<number>(meInboxCountQuery().queryKey);
}

/** The asks the Inbox lists with its filter cleared, beside a hand-on of `ask` (`useInboxAsks`):
 *  the reader's own from the one read, and everybody else's from the read of the open asks. */
function HandOnList({
  ask,
  onHandOn,
}: {
  ask: OpenAskRow;
  onHandOn: (submit: (to: string) => void) => void;
}): ReactNode {
  const listed = useInboxAsks([], "alice");
  const handOn = useAskHandOn(ask);
  onHandOn(handOn.submit);
  return (
    <ul>
      {(listed.asks ?? []).map((row) => (
        <li data-ask={row.id} data-waiting={row.waiting_on_person} key={row.id} />
      ))}
    </ul>
  );
}

test("a hand-on to the reader keeps the ask in their list, and a refusal puts the one read back", async () => {
  // carol's ask, which alice hands to herself: the one read lists none of it yet.
  const ask = issueAsk({
    id: "ask-b",
    issue: { assignee: "carol", key: "CORE-1", title: "Fix the thing" },
  });
  const client = seeded([ask], oneRead([]));
  // What Dispatch holds: no ask of the reader's, and of the open asks carol's, which each read
  // after the refusal answers. Both reads are held until the rollback has been checked, so what
  // the page draws then is what the rollback put back, not what a read brought.
  const reads = Promise.withResolvers<void>();
  restores.push(
    answerOneRead(async () => {
      await reads.promise;
      return {};
    })
  );
  restores.push(
    answerOpenAsks(async () => {
      await reads.promise;
      return [ask];
    })
  );
  const answer = deferred<unknown>();
  answerRoute("PUT", "/api/v1/asks/*/asked-of").mockReturnValue(answer.promise);
  let submit: ((to: string) => void) | undefined;
  render(<HandOnList ask={ask} onHandOn={(handOn) => (submit = handOn)} />, {
    wrapper: wrapper(client),
  });
  await waitFor(() => expect(document.querySelector('[data-ask="ask-b"]')).not.toBeNull());

  act(() => submit?.("alice"));
  await waitFor(() => expect(cachedAsk(client, "ask-b")?.waiting_on_person).toBe("alice"));
  expect(document.querySelector('[data-ask="ask-b"]')?.getAttribute("data-waiting")).toBe("alice");

  await act(async () => answer.refuse());
  await waitFor(() => expect(cached(client)?.entries).toEqual([]));
  expect(document.querySelector('[data-ask="ask-b"]')?.getAttribute("data-waiting")).toBe("carol");
  await waitFor(() => expect(api.getMeInboxPage).toHaveBeenCalled());
  await act(async () => reads.resolve());
  await waitFor(() => expect(client.isFetching()).toBe(0));
});

test("taking an unassigned ask adds it to the one read, and a refusal takes it out again", async () => {
  // Nobody holds CORE-1, so the ask waits on the project's default until alice takes the issue.
  const ask = issueAsk({
    id: "ask-c",
    issue: { assignee: null, key: "CORE-1", title: "Fix the thing" },
  });
  const client = seeded([ask], oneRead([]));
  const answer = deferred<never>();
  answerIssuePatches().mockReturnValue(answer.promise);
  const hook = renderHook(() => useIssueAssignee("CORE-1", ask.issue?.assignee ?? null), {
    wrapper: wrapper(client),
  });

  act(() => hook.result.current.submit("alice"));
  await waitFor(() => expect(cachedAsk(client, "ask-c")?.waiting_on_person).toBe("alice"));
  expect(cachedAsk(client, "ask-c")?.issue?.assignee).toBe("alice");

  await act(async () => answer.refuse());
  await waitFor(() => expect(cached(client)?.entries).toEqual([]));
  expect(invalidated(client)).toBe(true);
});

test("a priority lands on the one read's ask, and a refusal takes it back", async () => {
  const ask = issueAsk({ id: "ask-a" });
  const client = seeded([ask], oneRead([ask]));
  const answer = deferred<never>();
  answerIssuePatches().mockReturnValue(answer.promise);
  const hook = renderHook(() => useIssuePriority("CORE-1", issuePriority(ask.priority)), {
    wrapper: wrapper(client),
  });

  act(() => hook.result.current.submit(1));
  await waitFor(() => expect(cachedAsk(client, "ask-a")?.priority).toBe(1));

  await act(async () => answer.refuse());
  await waitFor(() => expect(cachedAsk(client, "ask-a")?.priority).toBeNull());
  expect(invalidated(client)).toBe(true);
});

test("a snooze lands on the one read's ask, and a refusal brings it back", async () => {
  const ask = issueAsk({ id: "ask-a" });
  const client = seeded([ask], oneRead([ask]));
  const answer = deferred<never>();
  answerRoute("PUT", "/api/v1/me/asks/*/snooze").mockReturnValue(answer.promise);
  const hook = renderHook(() => useAskSnooze("ask-a"), { wrapper: snoozeWrapper(client) });

  act(() => hook.result.current.submit("2026-10-20T09:00:00Z"));
  await waitFor(() =>
    expect(cachedAsk(client, "ask-a")?.snoozed_until).toBe("2026-10-20T09:00:00Z")
  );

  await act(async () => answer.refuse());
  await waitFor(() => expect(cachedAsk(client, "ask-a")?.snoozed_until).toBeNull());
  expect(invalidated(client)).toBe(true);
});

test("a bulk snooze lands on the one read's asks, and a refusal brings back only the refused ask", async () => {
  const taken = issueAsk({ id: "ask-a" });
  const refused = issueAsk({ id: "ask-b", snoozed_until: "2026-10-15T09:00:00Z" });
  const client = seeded([taken, refused], oneRead([taken, refused]));
  const refusal = deferred<never>();
  answerRoute<(id: string) => Promise<unknown>>(
    "PUT",
    "/api/v1/me/asks/*/snooze"
  ).mockImplementation((id) => (id === "ask-b" ? refusal.promise : Promise.resolve(undefined)));
  const hook = renderHook(() => useAskSnoozeMany(), { wrapper: wrapper(client) });

  let settled: Promise<unknown> | undefined;
  act(() => {
    settled = hook.result.current.submit(["ask-a", "ask-b"], "2026-10-20T09:00:00Z");
  });
  await waitFor(() =>
    expect(cachedAsk(client, "ask-b")?.snoozed_until).toBe("2026-10-20T09:00:00Z")
  );
  expect(cachedAsk(client, "ask-a")?.snoozed_until).toBe("2026-10-20T09:00:00Z");

  await act(async () => {
    refusal.refuse();
    await settled;
  });
  // The refused ask goes back to the moment it held before, not to no snooze at all.
  expect(cachedAsk(client, "ask-b")?.snoozed_until).toBe("2026-10-15T09:00:00Z");
  expect(cachedAsk(client, "ask-a")?.snoozed_until).toBe("2026-10-20T09:00:00Z");
  expect(invalidated(client)).toBe(true);
});

test("an answer or a decline takes the ask out of the one read, and a refusal puts it back", async () => {
  const ask = issueAsk({ id: "ask-a" });
  const client = seeded([ask], oneRead([ask]));
  const answer = deferred<unknown>();
  const hook = renderHook(
    () =>
      useOutstandingAnswerWrite(AskDeclineWritesContext, ask.id, {
        label: () => "Ask declined",
        method: "POST",
        write: () => answer.promise,
      }),
    { wrapper: wrapper(client) }
  );

  act(() => hook.result.current.mutate({ ask }));
  await waitFor(() => expect(cachedAsk(client, "ask-a")).toBeUndefined());
  expect(cached(client)?.total).toBe(0);

  await act(async () => answer.refuse());
  await waitFor(() => expect(cachedAsk(client, "ask-a")?.id).toBe("ask-a"));
  expect(invalidated(client)).toBe(true);
});

test("Mark all read draws the one read's rows read and its count at zero, and a refusal brings both back", async () => {
  const thread = replyRow("c1");
  const client = seeded([], oneRead([], [thread]), 1);
  const answer = deferred<never>();
  answerInboxReads().mockReturnValue(answer.promise);
  const hook = renderHook(
    () => useMarkAllRead({ asOf: AS_OF, filters: {}, rows: [{ kind: "thread", thread }] }),
    { wrapper: wrapper(client) }
  );

  act(() => hook.result.current.markAll());
  await waitFor(() => expect(cachedThread(client, "c1")?.unread).toBe(false));
  expect(unreadCount(client)).toBe(0);

  await act(async () => answer.refuse());
  await waitFor(() => expect(cachedThread(client, "c1")?.unread).toBe(true));
  expect(unreadCount(client)).toBe(1);
});

test("a dismissal takes the row out of the one read and lowers its count, and a refusal puts both back", async () => {
  const thread = replyRow("c1");
  const client = seeded([], oneRead([], [thread]), 1);
  const answer = deferred<never>();
  answerInboxDismisses().mockReturnValue(answer.promise);
  const settled: string[][] = [];
  const hook = renderHook(() => useInboxDismiss({ onSettled: (ids) => settled.push([...ids]) }), {
    wrapper: wrapper(client),
  });

  act(() => hook.result.current.submit({ rows: [dismissThreadRow(thread)] }));
  await waitFor(() => expect(cachedThread(client, "c1")).toBeUndefined());
  expect(unreadCount(client)).toBe(0);

  await act(async () => answer.refuse());
  await waitFor(() => expect(cachedThread(client, "c1")?.thread_id).toBe("c1"));
  expect(unreadCount(client)).toBe(1);
  expect(hook.result.current.refusal).toBe(dismissRefusalText(1, false, "refused"));
  expect(settled).toEqual([["reply:c1"]]);
});

/** The reader's Reply rows and the Inbox count, as the page draws them from the one read
 *  (`useInboxThreadRows`, `useUnreadThreadCount`), under their Mark all read. */
function OneReadThreads(): ReactNode {
  const listRef = useRef<HTMLUListElement>(null);
  const threads = useInboxThreadRows({ asksOnly: false, listRef, show: "inbox" });
  const count = useUnreadThreadCount();
  const markAll = useMarkAllRead({ asOf: AS_OF, filters: {}, rows: threads.rows });
  return (
    <>
      <output data-testid="count">{count}</output>
      <ul ref={listRef}>
        {threads.rows.map((row) =>
          row.kind === "thread" ? (
            <li
              data-thread={row.thread.thread_id}
              data-unread={String(row.thread.unread)}
              key={row.thread.thread_id}
            />
          ) : null
        )}
      </ul>
      <button onClick={markAll.markAll} type="button">
        Mark all read
      </button>
    </>
  );
}

/** Whether the row of thread `id` is drawn unread: "true", "false", or null where none is drawn. */
function drawnUnread(id: string): string | null {
  return document.querySelector(`[data-thread="${id}"]`)?.getAttribute("data-unread") ?? null;
}

function drawnCount(): string {
  return screen.getByTestId("count").textContent ?? "";
}

// The reader's Reply rows and the Inbox count are the one read's. A read of it asked while Mark
// all read is out - an event's refresh of the reader's threads, say - would reach Dispatch before
// the press marks anything, and draw the row unread and the count back at one while the press is
// still going through. It waits for the press's answer, and then reads what Dispatch holds after
// it.
test("a read of the one read asked while Mark all read is out waits for its answer, so the rows stay drawn read", async () => {
  const before = [replyRow("c1")];
  // What Dispatch holds once the press is in: the row read, and a reply written since.
  const after = [replyRow("c1", { unread: false }), replyRow("c2")];
  let served = before;
  const getMeInbox = answerOneRead(() => ({ threads: served }));
  restores.push(getMeInbox);
  const answer = deferred<InboxReadResponse>();
  answerInboxReads().mockReturnValue(answer.promise);
  const client = seeded([], oneRead([], before), 1, new DispatchQueryClient(CLIENT_OPTIONS));
  render(<OneReadThreads />, { wrapper: wrapper(client) });
  await waitFor(() => expect(drawnUnread("c1")).toBe("true"));
  expect(drawnCount()).toBe("1");

  act(() => screen.getByRole("button", { name: "Mark all read" }).click());
  await waitFor(() => expect(drawnUnread("c1")).toBe("false"));
  expect(drawnCount()).toBe("0");

  // Dispatch still holds the row unread, and a read that reached it now would answer so.
  await act(async () => {
    void client.invalidateQueries({ queryKey: meInboxKey });
    await settle();
  });
  // The page still draws what the press wrote: neither read has asked Dispatch anything, though
  // both have started.
  expect(drawnUnread("c1")).toBe("false");
  expect(drawnCount()).toBe("0");
  expect(getMeInbox).not.toHaveBeenCalled();
  expect(client.getQueryState(meInboxQuery().queryKey)?.fetchStatus).toBe("fetching");
  expect(client.getQueryState(meInboxCountQuery().queryKey)?.fetchStatus).toBe("fetching");

  served = after;
  await act(async () => answer.resolve({ marked: 1 }));
  await waitFor(() => expect(drawnUnread("c2")).toBe("true"));
  expect(getMeInbox).toHaveBeenCalled();
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(drawnUnread("c1")).toBe("false");
  expect(drawnCount()).toBe("1");
});

// A read of the one read asked before Mark all read answers the rows as they stood. Landing
// after the press, it would draw the row unread and the count back at one. The press gives it up
// and asks for the one read again, and on Dispatch's own client that read waits for the press's
// answer.
test("a read of the one read asked before Mark all read does not put back the rows the press drew read", async () => {
  const before = [replyRow("c1")];
  const after = [replyRow("c1", { unread: false })];
  const asked = Promise.withResolvers<MeInboxResponse>();
  const getMeInbox = answerOneRead({ threads: after }).mockImplementationOnce(() => asked.promise);
  restores.push(getMeInbox);
  const answer = deferred<InboxReadResponse>();
  answerInboxReads().mockReturnValue(answer.promise);
  const client = seeded([], oneRead([], before), 1, new DispatchQueryClient(CLIENT_OPTIONS));
  render(<OneReadThreads />, { wrapper: wrapper(client) });
  await waitFor(() => expect(drawnUnread("c1")).toBe("true"));
  let read = Promise.resolve();
  await act(async () => {
    read = client.refetchQueries({ queryKey: meInboxQuery().queryKey });
  });
  expect(getMeInbox).toHaveBeenCalledTimes(1);

  act(() => screen.getByRole("button", { name: "Mark all read" }).click());
  await waitFor(() => expect(drawnUnread("c1")).toBe("false"));
  // The read answers as Dispatch held the row when it was asked: unread.
  await act(async () => {
    asked.resolve(oneReadReply({}, { threads: before }));
    await read;
  });
  expect(drawnUnread("c1")).toBe("false");
  expect(drawnCount()).toBe("0");
  // The one read the press asked for again waits for its answer, and has asked Dispatch nothing.
  expect(getMeInbox).toHaveBeenCalledTimes(1);

  await act(async () => answer.resolve({ marked: 1 }));
  await waitFor(() => expect(getMeInbox.mock.calls.length).toBeGreaterThan(1));
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(drawnUnread("c1")).toBe("false");
  expect(drawnCount()).toBe("0");
});

/** The reader's own asks as the page lists them from the one read (`useInboxAsks`), beside a
 *  snooze of `ask`. */
function OneReadAsks({ ask, until }: { ask: OpenAskRow; until: string }): ReactNode {
  const listed = useInboxAsks(["alice"], "alice");
  const snooze = useAskSnooze(ask.id);
  return (
    <>
      <ul>
        {(listed.asks ?? []).map((row) => (
          <li data-ask={row.id} data-snoozed={row.snoozed_until ?? "none"} key={row.id} />
        ))}
      </ul>
      <button onClick={() => snooze.submit(until)} type="button">
        Snooze
      </button>
    </>
  );
}

function drawnSnooze(id: string): string | null {
  return document.querySelector(`[data-ask="${id}"]`)?.getAttribute("data-snoozed") ?? null;
}

// The reader's own asks are the one read's. A read of it asked while a snooze is out would reach
// Dispatch before the snooze, and draw the row back out of Later while the snooze is still going
// through. It waits for the snooze's answer, as the read of the open asks does.
test("a read of the one read asked while a snooze is out waits for its answer, so the row stays snoozed", async () => {
  const until = "2026-10-20T09:00:00Z";
  const ask = issueAsk({ id: "ask-a" });
  let served = [ask];
  const getMeInbox = answerOneRead(() => ({ asks: served }));
  restores.push(getMeInbox);
  const answer = deferred<unknown>();
  answerRoute("PUT", "/api/v1/me/asks/*/snooze").mockReturnValue(answer.promise);
  const client = seeded([ask], oneRead([ask]), 0, new DispatchQueryClient(CLIENT_OPTIONS));
  render(<OneReadAsks ask={ask} until={until} />, { wrapper: snoozeWrapper(client) });
  await waitFor(() => expect(drawnSnooze("ask-a")).toBe("none"));

  act(() => screen.getByRole("button", { name: "Snooze" }).click());
  await waitFor(() => expect(drawnSnooze("ask-a")).toBe(until));

  // Dispatch has not snoozed the ask yet, and a read that reached it now would answer so.
  await act(async () => {
    void client.invalidateQueries({ queryKey: meInboxKey });
    await settle();
  });
  // The page still draws the snooze: the read has started and asked Dispatch nothing.
  expect(drawnSnooze("ask-a")).toBe(until);
  expect(getMeInbox).not.toHaveBeenCalled();
  expect(client.getQueryState(meInboxQuery().queryKey)?.fetchStatus).toBe("fetching");

  served = [{ ...ask, snoozed_until: until }];
  await act(async () => answer.resolve(undefined));
  await waitFor(() => expect(getMeInbox).toHaveBeenCalled());
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(drawnSnooze("ask-a")).toBe(until);
});
