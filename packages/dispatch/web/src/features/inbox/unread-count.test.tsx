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
  return { dismissed, listMyReplies, read, readCount };
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

// A press made while an earlier one is out has left by the time the earlier one is refused, so
// the count it goes back to still holds the made press's rows: the refusal reads the count again,
// and Dispatch's answer, which counts the made press, is the count.
test("a Dismiss refused after a later Mark all read was made reads the count again", async () => {
  const user = userEvent.setup();
  const { dismissed, listMyReplies, read } = renderPresses();

  await user.click(screen.getByRole("button", { name: "Dismiss" }));
  await waitFor(() => expect(inboxCount()).toBe("4"));
  await user.click(screen.getByRole("button", { name: "Mark all read" }));
  await waitFor(() => expect(inboxCount()).toBe("2"));

  // From here Dispatch counts the two rows Mark all read marks and not the Dismiss's row: three.
  listMyReplies.mockImplementation(async (page) =>
    page.limit === 1 ? { ...threadsRead([]), total: COUNT - 2 } : threadsRead(ROWS)
  );
  read.resolve({ marked: 2 });
  await waitFor(() => expect(inboxCount()).toBe("3"));

  const reads = listMyReplies.mock.calls.length;
  dismissed.reject(refusal());
  await screen.findByText("Could not dismiss 1 thread: not found.");
  await waitFor(() => expect(inboxCount()).toBe("3"));
  expect(listMyReplies.mock.calls.length).toBeGreaterThan(reads);
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
