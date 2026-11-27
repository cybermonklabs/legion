import { expect, test } from "bun:test";
import type { QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { type Lists, newQueryClient, stubLists, tab } from "../../__tests__/draft-matrix-fixture";
import { expectSameElements } from "../../__tests__/same-elements";
import type { Comment, DispatchEvent, UserState } from "../../api/types";

/**
 * The Conversation tab's `New` marks over one visit (`ConversationTab`'s `visitBoundary`). It is a
 * file of its own beside `ConversationTab.test.tsx`, over the same fixture
 * (`draft-matrix-fixture.tsx`).
 */

function comment(id: string, body: string, createdAt: string): Comment {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** One event of CORE-1's log, as the tab's one-row reads of the log answer it. */
function logEvent(seq: number, at: string): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: at,
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {},
    seq,
    type: "issue.created",
  } as DispatchEvent;
}

/** The reader's state of CORE-1, read through event `seq`. */
function readThrough(seq: number): UserState {
  return { "CORE-1": { dismissed: [], last_read_seq: seq, pinned: false, seq: 0 } };
}

/** The list's turns in page order. */
function turnElements(): HTMLElement[] {
  return [...document.querySelectorAll<HTMLElement>("[data-turn]")];
}

/** Each turn's `data-turn`, and whether it draws a `New` mark. */
function marks(): [string | null, boolean][] {
  return turnElements().map((turn) => [
    turn.getAttribute("data-turn"),
    within(turn).queryByText("New", { exact: true }) !== null,
  ]);
}

// dispatch://INFRA-2160, R1: the read mark, written a second after the newest card is on screen,
// moved `lastReadSeq`, which is in the unread boundary read's key; the read under the new key
// started empty, every `New` went with it, and each turn below one moved up by its height, under
// a press the reader was making on a link there.
test("the read mark moving lastReadSeq leaves every New drawn and moves no turn", async () => {
  const stub = stubLists({
    comments: [
      comment("comment-1", "An older comment", "2026-09-20T00:00:00Z"),
      comment("comment-2", "A newer comment", "2026-09-20T00:05:00Z"),
    ],
    head: [logEvent(9, "2026-09-20T00:05:00Z")],
    unread: [logEvent(4, "2026-09-20T00:00:00Z")],
  });
  const queryClient = newQueryClient();
  const view = render(tab(readThrough(3), true, queryClient));
  try {
    await screen.findByText("A newer comment");
    await waitFor(() => expect(marks().map(([, marked]) => marked)).toEqual([true, true]));
    const drawn = marks();
    const elements = turnElements();

    // The read mark through the head: nothing is unread under the new key.
    stub.lists.unread = [];
    view.rerender(tab(readThrough(9), true, queryClient));
    expect(marks()).toEqual(drawn);
    await waitFor(() =>
      expect(queryClient.getQueryState(["events", "CORE-1", "unread-boundary", 9])?.status).toBe(
        "success"
      )
    );
    expect(marks()).toEqual(drawn);
    expectSameElements(turnElements(), elements);
  } finally {
    view.unmount();
    stub.restore();
  }
});

/** CORE-1's two comments and its head, event 9, the second comment's. */
function twoComments(): Lists {
  return {
    comments: [
      comment("comment-1", "An older comment", "2026-09-20T00:00:00Z"),
      comment("comment-2", "A newer comment", "2026-09-20T00:05:00Z"),
    ],
    head: [logEvent(9, "2026-09-20T00:05:00Z")],
  };
}

/** A third comment arrives as event 10, the first the reader has not read: what the next read of
 *  each list answers. */
function arrive(lists: Lists): void {
  lists.comments = [
    ...(lists.comments ?? []),
    comment("comment-3", "The newest comment", "2026-09-20T00:10:00Z"),
  ];
  lists.head = [logEvent(10, "2026-09-20T00:10:00Z")];
  lists.unread = [logEvent(10, "2026-09-20T00:10:00Z")];
}

/** The live event of that comment, refreshed as the refresh scheduler does when the boundary read
 *  began less than a second before it (`MIN_REFETCH_INTERVAL_MS`, `api/query-refresh.ts`): the
 *  comments are read again at once, and the issue's event reads only after the list has drawn the
 *  new turn. */
async function deliverArrival(queryClient: QueryClient): Promise<void> {
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] });
  });
  await screen.findByText("The newest comment");
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: ["events", "CORE-1"] });
  });
}

function boundaryAnswered(queryClient: QueryClient, lastReadSeq: number): void {
  expect(
    queryClient.getQueryState(["events", "CORE-1", "unread-boundary", lastReadSeq])?.status
  ).toBe("success");
}

// A live event that adds a turn after the visit's first read gives that turn its `New`, though
// nothing was unread when the visit began, and the turns already drawn keep theirs and stay.
test("a turn arriving when nothing was unread gets its New once the boundary read answers it", async () => {
  const stub = stubLists({ ...twoComments(), unread: [] });
  const queryClient = newQueryClient();
  const view = render(tab(readThrough(9), true, queryClient));
  try {
    await screen.findByText("A newer comment");
    await waitFor(() => boundaryAnswered(queryClient, 9));
    expect(marks().map(([, marked]) => marked)).toEqual([false, false]);
    const elements = turnElements();

    arrive(stub.lists);
    await deliverArrival(queryClient);
    await waitFor(() => expect(marks().map(([, marked]) => marked)).toEqual([true, false, false]));
    expect(within(turnElements()[0] as HTMLElement).getByText("The newest comment")).toBeTruthy();
    expectSameElements(turnElements().slice(1), elements);
  } finally {
    view.unmount();
    stub.restore();
  }
});

// The read mark moved the boundary past the two marked turns before the third arrived; the read
// under that key answers the new turn's event, later than the boundary the marks were drawn from,
// and takes none of them away.
test("a turn arriving after the read mark moved gets its New, and every New drawn before stays", async () => {
  const stub = stubLists({ ...twoComments(), unread: [logEvent(4, "2026-09-20T00:00:00Z")] });
  const queryClient = newQueryClient();
  const view = render(tab(readThrough(3), true, queryClient));
  try {
    await screen.findByText("A newer comment");
    await waitFor(() => expect(marks().map(([, marked]) => marked)).toEqual([true, true]));
    const elements = turnElements();
    stub.lists.unread = [];
    view.rerender(tab(readThrough(9), true, queryClient));
    await waitFor(() => boundaryAnswered(queryClient, 9));

    arrive(stub.lists);
    await deliverArrival(queryClient);
    await waitFor(() => expect(marks().map(([, marked]) => marked)).toEqual([true, true, true]));
    expectSameElements(turnElements().slice(1), elements);
  } finally {
    view.unmount();
    stub.restore();
  }
});
