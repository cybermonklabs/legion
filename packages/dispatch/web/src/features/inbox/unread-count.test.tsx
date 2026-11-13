/**
 * The unread count every badge shares under two presses out at once - **Mark all read** and
 * **Dismiss** - each of which drops it (`unread-count.ts`): both live drop it by both, and a
 * refusal of either gives back its own rows alone, whichever was pressed first.
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { answerInboxDismisses, answerInboxReads } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { inboxThreadsQuery, myRepliesCountQuery, whoAmIQuery } from "../../api/queries";
import type {
  InboxDismissResponse,
  InboxReadResponse,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { replyRow } from "../threads/replies-fixture";
import { MARK_ALL_REFUSED_TEXT, useMarkAllRead } from "./mark-all";
import { useUnreadThreadCount } from "./useInboxCount";
import { dismissThreadRow, useInboxDismiss } from "./useInboxDismiss";

const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

const AS_OF = "2026-10-12T02:00:00Z";

/** Five unread threads, three of them in the list: `c1`, written in since the list's read mark,
 *  which **Mark all read** leaves and **Dismiss** takes (one row), and `c2` and `c3`, which
 *  **Mark all read** marks (two rows). */
const COUNT = 5;
const ROWS: readonly MyReplyRow[] = [
  replyRow("c1", {
    reply: { ...replyRow("c1").reply, created_at: "2026-10-12T03:00:00Z" },
  }),
  replyRow("c2"),
  replyRow("c3"),
];

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** The Inbox's two presses over one list, and the one number every badge reads beside them. */
function Presses(): ReactNode {
  const read = useQuery(inboxThreadsQuery());
  const threads = read.data?.rows ?? [];
  const markAll = useMarkAllRead({
    asOf: AS_OF,
    filters: {},
    rows: threads.map((thread) => ({ kind: "thread", thread })),
  });
  const dismiss = useInboxDismiss();
  const count = useUnreadThreadCount();
  const first = threads.find((thread) => thread.thread_id === "c1");
  return (
    <>
      <output data-testid="inbox-count">{count}</output>
      <output>{markAll.refused ? MARK_ALL_REFUSED_TEXT : dismiss.refusal}</output>
      <button onClick={markAll.markAll} type="button">
        Mark all read
      </button>
      <button
        onClick={() => {
          if (first !== undefined) dismiss.submit({ rows: [dismissThreadRow(first)] });
        }}
        type="button"
      >
        Dismiss
      </button>
    </>
  );
}

/** The presses over `ROWS`, each held open until the test answers it, and the count's reads
 *  answered with `COUNT` unless the test says otherwise. */
function renderPresses() {
  const read = Promise.withResolvers<InboxReadResponse>();
  const dismissed = Promise.withResolvers<InboxDismissResponse>();
  answerInboxReads().mockReturnValue(read.promise);
  answerInboxDismisses().mockReturnValue(dismissed.promise);
  const listMyReplies = spyOn(api, "listMyReplies").mockResolvedValue({
    ...threadsRead(ROWS),
    total: COUNT,
  });
  restores.push(listMyReplies);
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(ROWS));
  client.setQueryData(myRepliesCountQuery().queryKey, COUNT);
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Presses />
      </QueryClientProvider>
    </MemoryRouter>
  );
  /** A read of the count landing with `total`, while the presses are out. */
  const readCount = (total: number) => {
    listMyReplies.mockResolvedValueOnce({ ...threadsRead([]), total });
    return act(() =>
      client.refetchQueries({ exact: true, queryKey: myRepliesCountQuery().queryKey })
    );
  };
  return { client, dismissed, listMyReplies, read, readCount };
}

function inboxCount(): string {
  return screen.getByTestId("inbox-count").textContent ?? "";
}

const refusal = () => new ApiError(404, { error: "not found" });

test("a Dismiss refused while Mark all read is out gives back its own row alone", async () => {
  const user = userEvent.setup();
  const { dismissed, listMyReplies, read } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  // Dispatch took no row from the Dismiss: the count is five less the two Mark all read marks.
  dismissed.reject(refusal());
  await screen.findByText("Could not dismiss 1 thread: not found.");
  expect(inboxCount()).toBe("3");

  read.resolve({ marked: 2 });
  await waitFor(() => expect(listMyReplies).toHaveBeenCalled());
});

test("a Mark all read refused while a Dismiss is out gives back its own rows alone", async () => {
  const user = userEvent.setup();
  const { dismissed, listMyReplies, read } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("3"));
  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  // Dispatch marked nothing: the count is five less the one row the Dismiss takes.
  read.reject(refusal());
  await screen.findByText("Nothing was marked read.");
  expect(inboxCount()).toBe("4");

  dismissed.resolve({ dismissed: 1 });
  await waitFor(() => expect(listMyReplies).toHaveBeenCalled());
});

/** Dismiss, then Mark all read, which Dispatch makes; then the Dismiss is refused. From Mark all
 *  read's landing on Dispatch counts the two rows it marked and not the Dismiss's row - three -
 *  and every read of the count after the refusal is answered with `countRead`. Resolves once
 *  that read has gone out. */
async function dismissRefusedAfterMarkAllMade(countRead: () => Promise<MyRepliesResponse>) {
  const user = userEvent.setup();
  const { client, dismissed, listMyReplies, read } = renderPresses();
  const countReadSince = (calls: number) =>
    listMyReplies.mock.calls.slice(calls).some(([page]) => page?.limit === 1);

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  listMyReplies.mockImplementation(async (page) =>
    page?.limit === 1 ? { ...threadsRead([]), total: COUNT - 2 } : threadsRead(ROWS)
  );
  const marked = listMyReplies.mock.calls.length;
  read.resolve({ marked: 2 });
  // Dispatch's three lands under the Dismiss still out, which shows its row taken: two.
  await waitFor(() => {
    expect(countReadSince(marked)).toBe(true);
    expect(client.isFetching()).toBe(0);
  });
  expect(inboxCount()).toBe("2");

  listMyReplies.mockImplementation((page) =>
    page?.limit === 1 ? countRead() : Promise.resolve(threadsRead(ROWS))
  );
  const refused = listMyReplies.mock.calls.length;
  dismissed.reject(refusal());
  await screen.findByText("Could not dismiss 1 thread: not found.");
  await waitFor(() => expect(countReadSince(refused)).toBe(true));
  return client;
}

// A press made while an earlier one is out takes its rows out of the count before the earlier
// one: once the earlier one is refused and its drop is drawn no more, the count reads five less
// the two rows Dispatch marked - the count Dispatch last confirmed - while it reads the count
// again, never the five no answer gives.
test("a Dismiss refused after a later Mark all read was made reads three while the count is read again", async () => {
  const counted = Promise.withResolvers<MyRepliesResponse>();
  const client = await dismissRefusedAfterMarkAllMade(() => counted.promise);
  expect(client.getQueryState(myRepliesCountQuery().queryKey)?.fetchStatus).toBe("fetching");
  expect(inboxCount()).toBe("3");

  await act(async () => counted.resolve({ ...threadsRead([]), total: COUNT - 2 }));
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(inboxCount()).toBe("3");
});

test("a Dismiss refused after a later Mark all read was made reads three when the count read fails", async () => {
  const client = await dismissRefusedAfterMarkAllMade(() => Promise.reject(new Error("offline")));
  await waitFor(() =>
    expect(client.getQueryState(myRepliesCountQuery().queryKey)?.status).toBe("error")
  );
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(inboxCount()).toBe("3");
});

// R7 under two presses: a read of the count that lands while both are out may count neither,
// one, or both, and each is held at five less the three rows the two take - lowered once, never
// twice. One that lands between the presses and already counts the first is not lowered twice by
// the second either.
test("two presses out at once drop the count by both, whatever a count answer already counts", async () => {
  const user = userEvent.setup();
  const { dismissed, listMyReplies, read, readCount } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  // Counts the Dismiss already: four, which is not lowered again.
  await readCount(4);
  expect(inboxCount()).toBe("4");
  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  // Counts neither press.
  await readCount(5);
  expect(inboxCount()).toBe("2");
  // Counts the Dismiss alone.
  await readCount(4);
  expect(inboxCount()).toBe("2");
  // Counts both.
  await readCount(2);
  expect(inboxCount()).toBe("2");

  const reads = listMyReplies.mock.calls.length;
  read.resolve({ marked: 2 });
  dismissed.resolve({ dismissed: 1 });
  await waitFor(() => expect(listMyReplies.mock.calls.length).toBeGreaterThan(reads));
});

// R10. A count answer that lands while a Dismiss is out is Dispatch's count, held under the
// press's bound while it is out. Once the press is refused and the count cannot be read again,
// the badge shows that answer - six, a thread arrived - not the refused press's five less one,
// nor the five that stood before the press.
test("a count answer that landed while a Dismiss was out stays once it is refused and the count read fails", async () => {
  const user = userEvent.setup();
  const { client, dismissed, listMyReplies, readCount } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  // A thread arrived: six unread, held under the press's bound.
  await readCount(COUNT + 1);
  expect(inboxCount()).toBe("4");

  listMyReplies.mockImplementation((page) =>
    page?.limit === 1 ? Promise.reject(new Error("offline")) : Promise.resolve(threadsRead(ROWS))
  );
  dismissed.reject(refusal());
  await screen.findByText("Could not dismiss 1 thread: not found.");
  await waitFor(() =>
    expect(client.getQueryState(myRepliesCountQuery().queryKey)?.status).toBe("error")
  );
  expect(inboxCount()).toBe(String(COUNT + 1));
});

// R10. A press Dispatch made writes its own drop alone into the count Dispatch last gave: five,
// less the two rows Mark all read marked, never less the Dismiss still out beside it. So when the
// count cannot be read after that landing, and the Dismiss is then refused while it still cannot,
// the badge reads three - the two rows Dispatch took gone, the Dismiss's row back.
test("a Dismiss refused after a later Mark all read was made reads three when every count read since fails", async () => {
  const user = userEvent.setup();
  const { client, dismissed, listMyReplies, read } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  // From here on the count cannot be read.
  listMyReplies.mockImplementation((page) =>
    page?.limit === 1 ? Promise.reject(new Error("offline")) : Promise.resolve(threadsRead(ROWS))
  );
  read.resolve({ marked: 2 });
  await waitFor(() =>
    expect(client.getQueryState(myRepliesCountQuery().queryKey)?.status).toBe("error")
  );
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(inboxCount()).toBe("2");

  dismissed.reject(refusal());
  await screen.findByText("Could not dismiss 1 thread: not found.");
  await waitFor(() => expect(client.isFetching()).toBe(0));
  expect(inboxCount()).toBe("3");
});
