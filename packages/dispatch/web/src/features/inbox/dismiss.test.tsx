/**
 * The Inbox's **Dismiss**: what one press sends, what it takes off the page before the server
 * answers, what it puts back when the server refuses, and the `e` and the bulk bar that press it
 * (INFRA-450).
 */

import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { meInboxThreads } from "@legion/contracts";
import {
  QueryClient,
  QueryClientProvider,
  useInfiniteQuery,
  useQuery,
} from "@tanstack/react-query";
import { act, cleanup, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { realClock, settle } from "../../__tests__/fake-clock";
import { answerAskSnoozes, answerInboxDismisses } from "../../__tests__/send-answers";
import { stopTheLateWrite } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import {
  meInboxCountQuery,
  meInboxDismissedQuery,
  meInboxKey,
  meInboxQuery,
  openAsksQuery,
  whoAmIQuery,
} from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { DispatchEvent, IssueDetails, MyReplyRow, OpenAskRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { stoppedWriteNote } from "../writes/keyed-write";
import { Inbox } from "./Inbox";
import {
  answerOneRead,
  issueAsk,
  ONE_READ_AS_OF,
  oneReadAnswer,
  oneReadDismissedPages,
} from "./inbox-fixture";
import { askWaitingOnPerson } from "./list-row";
import {
  dismissesThread,
  dismissRefusalText,
  dismissThreadRow,
  inboxDismissBody,
  useInboxDismiss,
} from "./useInboxDismiss";

// A thread row's checkbox is named after its subject's plain-text reading (`PlainText`), which
// reads nothing until the lazy Markdown engine has loaded, and the rows find each checkbox by that
// name. So the file loads the engine before its first row, in whatever order the rows run.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** What a press carries for one of this file's Reply rows: the version the fixture draws them
 *  at, which every row of it shares. */
const SEEN = { version: replyRow("c1").version };

const AS_OF = ONE_READ_AS_OF;

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

/** A client holding everything the Inbox reads, with `asks` open and `rows` the reader's Reply
 *  and Agent rows, each drawn from the one read, as the reader's own asks among `asks` are. The
 *  refetch a landed press triggers is answered from the same rows. */
function seeded(rows: readonly MyReplyRow[], asks: readonly OpenAskRow[] = []): QueryClient {
  const mine = asks.filter((ask) => askWaitingOnPerson(ask) === "alice");
  restores.push(spyOn(api, "getCredentialPending").mockResolvedValue({ pending: [] }));
  restores.push(answerOneRead({ asks: mine, threads: rows }));
  // The unread set of the issue a thread row is on, which the reading column reads, is the one
  // read's `unread` state narrowed to that issue, which `answerOneRead` answers from `rows`.
  restores.push(spyOn(api, "getMyAgentState").mockResolvedValue({}));
  // The reading column reads the issue behind the thread row it is on, with that issue's
  // comments and subscribers: answered here, since no test of this file asserts them.
  restores.push(
    spyOn(api, "getIssue").mockResolvedValue({ closed_at: null } as unknown as IssueDetails)
  );
  restores.push(spyOn(api, "listComments").mockResolvedValue([]));
  restores.push(spyOn(api, "getIssueSubscribers").mockResolvedValue([]));
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(openAsksQuery().queryKey, [...asks]);
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([...mine, ...rows]));
  client.setQueryData(meInboxCountQuery().queryKey, rows.length);
  return client;
}

function renderInbox(client: QueryClient, address = "/"): void {
  render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Inbox />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

test("the body names either the rows with what the page drew in each or the list's narrowing with its read mark", () => {
  // A dismissal names every row it changes with what the page drew in it, per kind: a Reply
  // row's own version, and an Agent row's newest session reply with the ids it drew as unread,
  // which is what keeps the press from burying a write the page never drew.
  const reply = replyRow("c1");
  const agent = plannerAgentRow();
  expect(inboxDismissBody({ rows: [dismissThreadRow(reply), dismissThreadRow(agent)] })).toEqual({
    seen_replies: {
      "agent:message-1": { newest_reply: "reply-1", unread_replies: ["reply-1"] },
      "reply:c1": { version: reply.version },
    },
    threads: ["reply:c1", "agent:message-1"],
  });
  // A restore draws the rows back rather than burying anything, so it carries nothing: the
  // route refuses a restore that names any.
  expect(inboxDismissBody({ restore: true, rows: [dismissThreadRow(reply)] })).toEqual({
    restore: true,
    threads: ["reply:c1"],
  });
  // **Select all** of the paged view: the ids are not all loaded, so the narrowing and the list's
  // own `as_of` go instead - never both, which the route refuses - and it carries nothing drawn
  // either, the server drawing the rows itself there.
  expect(inboxDismissBody({ filters: { type: ["reply"] }, through: AS_OF })).toEqual({
    dismiss_through: AS_OF,
    kind: ["reply"],
  });
});

test("a press under the narrowing changes the threads the list answers at or before its mark", () => {
  const body = inboxDismissBody({ filters: {}, through: AS_OF });
  const row = replyRow("c1");
  expect(dismissesThread(row, body)).toBe(true);
  // Written in since the mark: the reader has not seen that write, so the press leaves it.
  expect(
    dismissesThread({ ...row, reply: { ...row.reply, created_at: "2026-10-12T03:00:00Z" } }, body)
  ).toBe(false);
  // By id, the narrowing says nothing: the ids alone decide.
  expect(dismissesThread(row, inboxDismissBody({ rows: [dismissThreadRow(row)] }))).toBe(true);
  expect(dismissesThread(row, inboxDismissBody({ rows: [dismissThreadRow(replyRow("c9"))] }))).toBe(
    false
  );
});

test("the row's Dismiss sends the row's own id and drawn version and takes it out of the list at once", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  const settle = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(settle.promise);
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  const control = document.querySelector<HTMLElement>('[data-inbox-dismiss="reply:c1"]');
  await user.click(control as HTMLElement);

  // At once, with the request still out: the row is gone and the shared unread count has dropped
  // by the one unread row it took.
  await waitFor(() => expect(rowIds()).toEqual(["reply:c9"]));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
  settle.resolve({ dismissed: 1 });
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
});

test("a refused dismiss puts the row back where it stood and says so", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(new ApiError(404, { error: "not found" }));
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  await user.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe(dismissRefusalText(1, false, "not found"));
  expect(dismissRefusalText(1, false, "not found")).toBe("Could not dismiss 1 thread: not found.");
  // Back in the list, in the place it held.
  expect(rowIds()).toEqual(["reply:c1", "reply:c9"]);
});

test("a row a refused dismiss put back is the reader's hand's again, held under the next read", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(new ApiError(404, { error: "not found" }));
  const client = seeded([replyRow("c1"), replyRow("c9")]);
  renderInbox(client);

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  await user.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);
  await screen.findByRole("alert");

  // The reader's hand is on the row the refusal put back, and the next read of the list no longer
  // holds it - somebody else dealt with the thread. It stays where they can see it, as any row
  // under their hand does: the press that took it out of the hold has settled.
  const row = document.querySelector<HTMLElement>('[data-inbox-row="reply:c1"]');
  act(() => row?.focus());
  act(() => {
    client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([replyRow("c9")]));
  });
  expect(rowIds()).toEqual(["reply:c1", "reply:c9"]);
});

// Two presses out at once, each refused in turn. A refusal puts back the rows its own press took
// out and nothing else: had it put the view back as its press found it, the first refusal would
// draw the second press's row back while that press is still out, and the second would take the
// first press's row out again, though Dispatch dismissed neither, and the unread count would end
// one too low.
test("two dismissals refused in a row put both rows back where they stood, and the unread count as it was", async () => {
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(meInboxQuery().queryKey, oneReadAnswer([replyRow("c1"), replyRow("c9")]));
  client.setQueryData(meInboxCountQuery().queryKey, 2);
  const first = Promise.withResolvers<never>();
  const second = Promise.withResolvers<never>();
  const dismissed = answerInboxDismisses();
  dismissed.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);
  const { result } = renderHook(
    () => ({
      count: useQuery(meInboxCountQuery()).data,
      dismiss: useInboxDismiss(),
      inboxThreads: meInboxThreads(useQuery(meInboxQuery()).data).map((row) => row.thread_id),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );

  act(() => result.current.dismiss.submit({ rows: [dismissThreadRow(replyRow("c1"))] }));
  await waitFor(() => expect(result.current).toMatchObject({ count: 1, inboxThreads: ["c9"] }));
  act(() => result.current.dismiss.submit({ rows: [dismissThreadRow(replyRow("c9"))] }));
  await waitFor(() => expect(result.current).toMatchObject({ count: 0, inboxThreads: [] }));

  // The first press is refused while the second is still out: its own row is back, and the
  // second's is still out of the list.
  await act(async () => first.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(result.current).toMatchObject({ count: 1, inboxThreads: ["c1"] }));

  await act(async () => second.reject(new ApiError(409, { error: "refused" })));
  await waitFor(() =>
    expect(result.current).toMatchObject({ count: 2, inboxThreads: ["c1", "c9"] })
  );
  expect(result.current.dismiss.refusal).toBe(dismissRefusalText(1, false, "refused"));
});

// A read of the Inbox's threads, of the Dismissed view or of the count asked while a dismiss is
// out - an event's refresh, another surface - can reach Dispatch before the dismiss does, and its
// answer would put the dismissed thread back in the list and in the unread count while the press
// is still out.
test("a read of either view or the count asked while a dismiss is out waits for its answer, and never puts the thread back", async () => {
  // What Dispatch holds, which each read answers with as it is when it is asked.
  const server = { dismissed: [] as MyReplyRow[], inbox: [replyRow("c1"), replyRow("c9")] };
  const getMeInbox = answerOneRead(() => ({ dismissed: server.dismissed, threads: server.inbox }));
  restores.push(getMeInbox);
  const dismissed = answerInboxDismisses();
  const answer = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(answer.promise);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const { result } = renderHook(
    () => ({
      count: useQuery(meInboxCountQuery()).data,
      dismiss: useInboxDismiss(),
      dismissedThreads: useInfiniteQuery(meInboxDismissedQuery()).data?.pages.flatMap((page) =>
        meInboxThreads(page).map((row) => row.thread_id)
      ),
      inboxThreads: meInboxThreads(useQuery(meInboxQuery()).data).map((row) => row.thread_id),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );

  await waitFor(() =>
    expect(result.current).toMatchObject({
      count: 2,
      dismissedThreads: [],
      inboxThreads: ["c1", "c9"],
    })
  );
  act(() => result.current.dismiss.submit({ rows: [dismissThreadRow(replyRow("c1"))] }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(result.current).toMatchObject({ count: 1, inboxThreads: ["c9"] });

  // Dispatch has not taken the dismiss, so a read that reached it now would answer c1 still in
  // the Inbox, and counted.
  const reads = getMeInbox.mock.calls.length;
  await act(async () => {
    void client.invalidateQueries({ queryKey: meInboxKey });
    await settle();
  });
  expect(getMeInbox).toHaveBeenCalledTimes(reads);
  expect(result.current).toMatchObject({ count: 1, dismissedThreads: [], inboxThreads: ["c9"] });

  // Dispatch takes it, and a reply lands on c5 meanwhile: the reads go now, and draw what it
  // holds.
  server.inbox = [replyRow("c9"), replyRow("c5")];
  server.dismissed = [replyRow("c1")];
  await act(async () => {
    answer.resolve({ dismissed: 1 });
    await settle();
  });
  await waitFor(() => expect(getMeInbox.mock.calls.length).toBeGreaterThan(reads));
  await waitFor(() =>
    expect(result.current).toMatchObject({
      count: 2,
      dismissedThreads: ["c1"],
      inboxThreads: ["c9", "c5"],
    })
  );
});

/** A `user_thread_state.updated` the live feed delivers: `payload.login`'s read of the Inbox's
 *  threads moved, of `payload.thread_id` alone or, with none, of every thread a dismiss or a
 *  Mark all read changed. */
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

// Dispatch publishes a dismiss's thread-state event, which names no thread, once it has taken the
// threads out, so a read asked after it answers the dismiss. The hold ends on that event though
// the dismiss's answer is still out, and a thread somebody else started meanwhile shows then
// rather than wait for an answer that may be late. The viewer's read of one thread in another tab,
// and another login's dismiss, are not this press's and leave the hold.
test("a thread somebody else starts while a dismiss is out shows once the dismiss's thread-state event has come", async () => {
  // What Dispatch holds, which each read answers with as it is when it is asked.
  const server = { dismissed: [] as MyReplyRow[], inbox: [replyRow("c1"), replyRow("c9")] };
  const getMeInbox = answerOneRead(() => ({ dismissed: server.dismissed, threads: server.inbox }));
  restores.push(getMeInbox);
  const dismissed = answerInboxDismisses();
  const answer = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(answer.promise);
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const { result } = renderHook(
    () => ({
      count: useQuery(meInboxCountQuery()).data,
      dismiss: useInboxDismiss(),
      dismissedThreads: useInfiniteQuery(meInboxDismissedQuery()).data?.pages.flatMap((page) =>
        meInboxThreads(page).map((row) => row.thread_id)
      ),
      inboxThreads: meInboxThreads(useQuery(meInboxQuery()).data).map((row) => row.thread_id),
    }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      ),
    }
  );

  await waitFor(() =>
    expect(result.current).toMatchObject({
      count: 2,
      dismissedThreads: [],
      inboxThreads: ["c1", "c9"],
    })
  );
  act(() => result.current.dismiss.submit({ rows: [dismissThreadRow(replyRow("c1"))] }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(result.current).toMatchObject({ count: 1, inboxThreads: ["c9"] });

  // Dispatch has taken the dismiss, and bob has started a thread with alice since (c5).
  const started = replyRow("c5");
  server.inbox = [
    replyRow("c9"),
    { ...started, reply: { ...started.reply, author: { id: "bob", kind: "user" } } },
  ];
  server.dismissed = [replyRow("c1")];
  const reads = getMeInbox.mock.calls.length;
  await act(async () => {
    hearLiveEvent(client, threadStateEvent({ login: "alice", thread_id: "c9" }), "alice");
    hearLiveEvent(client, threadStateEvent({ login: "bob" }), "alice");
    void client.invalidateQueries({ queryKey: meInboxKey });
    await settle();
  });
  // Neither event is the dismiss's: no read has asked Dispatch.
  expect(getMeInbox).toHaveBeenCalledTimes(reads);
  expect(result.current).toMatchObject({ count: 1, dismissedThreads: [], inboxThreads: ["c9"] });

  await act(async () => {
    hearLiveEvent(client, threadStateEvent({ login: "alice" }), "alice");
    void client.invalidateQueries({ queryKey: meInboxKey });
  });
  // The dismiss's answer is still out, and both views and the count are what Dispatch holds: c1
  // dismissed, and bob's thread in the Inbox and counted.
  await waitFor(() =>
    expect(result.current).toMatchObject({
      count: 2,
      dismissedThreads: ["c1"],
      inboxThreads: ["c9", "c5"],
    })
  );
  expect(getMeInbox.mock.calls.length).toBeGreaterThan(reads);
  expect(client.isMutating()).toBe(1);

  await act(async () => {
    answer.resolve({ dismissed: 1 });
    await settle();
  });
  await waitFor(() => expect(client.isMutating()).toBe(0));
  await waitFor(() => expect(client.isFetching()).toBe(0));
});

test("`e` dismisses the thread row in hand and does nothing on an ask", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  // Held open, so the list under test is the one the key leaves behind rather than the read the
  // landed press asks for.
  const settle = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(settle.promise);
  const ask = issueAsk({ id: "ask-1", question: "Which approach?" });
  renderInbox(seeded([replyRow("c1")], [ask]));

  await waitFor(() => expect(rowIds()).toContain("reply:c1"));
  // The focused row is the key's hand below the reading column's width.
  const askRow = document.querySelector<HTMLElement>('[data-inbox-row="ask-1"]');
  act(() => askRow?.focus());
  await user.keyboard("e");
  expect(dismissed).not.toHaveBeenCalled();

  const threadRow = document.querySelector<HTMLElement>('[data-inbox-row="reply:c1"]');
  act(() => threadRow?.focus());
  await user.keyboard("e");
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
  expect(rowIds()).not.toContain("reply:c1");
  await act(async () => {
    settle.resolve({ dismissed: 1 });
  });
});

test("the bulk bar offers Snooze for the asks among the selection and Dismiss for the threads", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  // Held open, as the row's own press above is: this file's read answers the same rows, so a
  // landed press would draw the dismissed row again before the press is read here.
  const settle = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(settle.promise);
  const ask = issueAsk({ id: "ask-1", question: "Which approach?" });
  renderInbox(seeded([replyRow("c1")], [ask]));

  await waitFor(() => expect(rowIds()).toContain("reply:c1"));
  // A thread row takes the mark `x` makes, as an ask does.
  await user.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  expect(screen.queryByLabelText("Snooze selected asks")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Dismiss selected threads" })).toBeTruthy();

  const asksMark = [...document.querySelectorAll<HTMLElement>('input[type="checkbox"]')].find(
    (box) => box.getAttribute("aria-label")?.startsWith("Select Root") !== true
  );
  await user.click(asksMark as HTMLElement);
  await screen.findByText("2 selected");
  expect(screen.getByLabelText("Snooze selected asks")).toBeTruthy();

  await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
  await waitFor(() => expect(rowIds()).not.toContain("reply:c1"));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
  await act(async () => {
    settle.resolve({ dismissed: 1 });
  });
});

test("Select all over the Inbox's own read names every row of the group by id and drawn version", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  dismissed.mockResolvedValue({ dismissed: 2 });
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  await user.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  await user.click(screen.getByRole("button", { name: "Select all" }));
  await screen.findByText("Every row selected");

  // The Inbox's thread read is unpaged, so every id of the group is in hand - the rows behind
  // **Show more** and inside a folded band with them - and one request names them all, each
  // under the version its own row is showing.
  await user.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN, "reply:c9": SEEN },
    threads: ["reply:c1", "reply:c9"],
  });
});

test("Select all in the paged Dismissed view sends the narrowing with that list's read mark", async () => {
  const user = userEvent.setup();
  const dismissed = answerInboxDismisses();
  dismissed.mockResolvedValue({ dismissed: 2 });
  const client = seeded([]);
  client.setQueryData(
    meInboxDismissedQuery().queryKey,
    oneReadDismissedPages([replyRow("c1"), replyRow("c9")])
  );
  renderInbox(client, "/?show=dismissed");

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  await user.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  await user.click(screen.getByRole("button", { name: "Select all" }));
  await screen.findByText("Every row selected");

  // This list is the one the server pages, so the ids behind **Show more** are not loaded: the
  // press names the narrowing and the mark the list was read at instead, never both.
  await user.click(screen.getByRole("button", { name: "Move to Inbox selected threads" }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    dismiss_through: AS_OF,
    kind: ["reply", "agent"],
    restore: true,
  });
});

/** The test DOM's window, whose viewport `matchMedia` answers from: from 1280 px the Inbox draws
 *  short rows beside a reading column, below it cards. */
const testWindow = window as unknown as {
  happyDOM: { setViewport(size: { width: number }): void };
  innerWidth: number;
};

// A thread row prints its root in one line, so a root its author deleted - whose subject the
// delete cleared - reads as the deleted line in the title and in every name the row's controls
// carry, rather than as an empty line; an edited root carries ` · edited` after its title; and
// a root that stands unedited carries neither. Both layouts draw the same.
test.each([
  ["a card, below the reading column's width", undefined],
  ["a short row, beside the reading column", 1440],
] as const)("a thread row drawn as %s reads a deleted root as deleted and marks an edited one", async (_layout, width) => {
  const was = testWindow.innerWidth;
  if (width !== undefined) testWindow.happyDOM.setViewport({ width });
  try {
    renderInbox(
      seeded([
        // The deleted root was edited first, so ` · edited` can be drawn, and only the row's own
        // reading of the delete keeps it off.
        replyRow("c1", {
          deleted_at: "2026-10-12T00:40:00Z",
          deleted_by: { id: "alice", kind: "user" },
          edited_at: "2026-10-12T00:10:00Z",
          subject: "",
        }),
        replyRow("c2", { edited_at: "2026-10-12T00:20:00Z", subject: "Edited root" }),
        replyRow("c9", { subject: "Standing root" }),
      ])
    );
    await waitFor(() => expect(rowIds().sort()).toEqual(["reply:c1", "reply:c2", "reply:c9"]));
    const row = (id: string) =>
      document.querySelector<HTMLElement>(`[data-inbox-row="${id}"]`) as HTMLElement;

    const deleted = row("reply:c1");
    await waitFor(() =>
      expect(deleted.querySelector("[data-post-deleted]")?.textContent).toContain(
        "Deleted by alice"
      )
    );
    expect(deleted.querySelector('[title="Deleted by alice"]')).not.toBeNull();
    expect(deleted.querySelector('[aria-label="Select Deleted by alice"]')).not.toBeNull();
    expect(deleted.querySelector('[aria-label="Dismiss: Deleted by alice"]')).not.toBeNull();
    expect(deleted.textContent).not.toContain("edited");

    const edited = row("reply:c2");
    expect(edited.textContent).toContain("Edited root · edited");
    expect(edited.querySelector('[aria-label="Select Edited root"]')).not.toBeNull();

    const standing = row("reply:c9");
    expect(standing.textContent).toContain("Standing root");
    expect(standing.textContent).not.toContain("edited");
    expect(standing.querySelector("[data-post-deleted]")).toBeNull();
  } finally {
    testWindow.happyDOM.setViewport({ width: was });
  }
});

// A dismissal the reader stopped may have been made: the rows come back as after a refusal, but
// the page says Dispatch may have made it and that the next press is one of its own, rather
// than a refusal in the stopped write's own words, which are a composer's.
test("a dismissal the reader stopped says it may have been made, not that it was refused", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(
    new NetworkError("/api/v1/me/inbox/dismiss", new TypeError("Failed to fetch"))
  );
  const client = seeded([replyRow("c1"), replyRow("c9")]);
  renderInbox(client);
  try {
    await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
    await user.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);
    await stopTheLateWrite(client);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the dismissal of 1 thread", "made", {
        anew: "Dismissing it again sends a new write.",
      })
    );
  } finally {
    realClock();
    client.clear();
  }
});

// A move back to the Inbox the reader stopped may have been made as well: the rows come back to
// the Dismissed view, and the page says so in the words of the move, not of a dismissal.
test("a move back to the Inbox the reader stopped says it may have been made", async () => {
  jest.useFakeTimers();
  const user = userOnFakeClock();
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(
    new NetworkError("/api/v1/me/inbox/dismiss", new TypeError("Failed to fetch"))
  );
  const client = seeded([]);
  client.setQueryData(
    meInboxDismissedQuery().queryKey,
    oneReadDismissedPages([replyRow("c1"), replyRow("c9")])
  );
  renderInbox(client, "/?show=dismissed");
  try {
    await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
    await user.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);
    await stopTheLateWrite(client);
    expect(screen.getByRole("alert").textContent).toBe(
      stoppedWriteNote("the move of 1 thread back to the Inbox", "made", {
        anew: "Moving it back again sends a new write.",
      })
    );
  } finally {
    realClock();
    client.clear();
  }
});

// A bulk snooze the reader stopped may have been saved: the bar says so apart from any refusal,
// and that the next pick is a press of its own, rather than "Could not snooze" in the stopped
// write's own words, which are a composer's.
test("a bulk snooze the reader stopped says it may have been saved, not that it was refused", async () => {
  const snoozes = answerAskSnoozes().mockRejectedValue(
    new NetworkError("/api/v1/me/asks/ask-1/snooze", new TypeError("Failed to fetch"))
  );
  restores.push(snoozes);
  const ask = issueAsk({ id: "ask-1", question: "Which approach?" });
  const client = seeded([], [ask]);
  renderInbox(client);
  try {
    await screen.findByText("Which approach?");
    const user = userEvent.setup();
    const asksMark = [...document.querySelectorAll<HTMLElement>('input[type="checkbox"]')][0];
    await user.click(asksMark as HTMLElement);
    await screen.findByText("1 selected");
    jest.useFakeTimers();
    await userOnFakeClock().selectOptions(
      screen.getByLabelText("Snooze selected asks"),
      "tomorrow"
    );
    await stopTheLateWrite(client);
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe(
        stoppedWriteNote("the snooze of 1 of 1", "saved", {
          anew: "It stays marked, and the next pick snoozes it as a new write.",
        })
      )
    );
  } finally {
    realClock();
    client.clear();
  }
});
