/**
 * The issue's read mark at the end of the new page's outline: the end
 * three-quarters on screen for a second writes the issue's head as read, once, and a mark already
 * at the head is written again by nothing; nor is the end on screen before the outline holds the
 * reads it draws the conversation from.
 */

import { afterEach, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";

import { jump, realClock } from "../../__tests__/fake-clock";
import { dwellOn, installIntersectionObserver } from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerIssueStates } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { conversationTreeKey } from "../../api/queries";
import type { DispatchEvent, UserIssueState, UserState } from "../../api/types";
import { IssueReadMark } from "./IssueReadMark";
import { treeAnswer } from "./tree-read/tree-fixtures";

afterEach(() => {
  cleanup();
});

/** The issue's newest event, which is what a dwell on the end reads the issue through. */
function headEvent(seq: number): DispatchEvent {
  return {
    actor: { id: "bob", kind: "user" },
    created_at: "2026-11-16T00:00:00Z",
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {},
    seq,
    type: "issue.created",
  } as DispatchEvent;
}

function issueState(lastReadSeq: number): UserIssueState {
  return { dismissed: [], last_read_seq: lastReadSeq, pinned: false, seq: 0 };
}

/** The reads the outline draws the conversation from, under the keys it reads them by
 *  (`useIssueTree.ts`, `tree-read/tree-read.ts`), which the end waits on: the asks with their
 *  threads, Conversation's tree read, whose comment nodes are the outline's comments, and the
 *  messages. */
const OUTLINE_READS = {
  asks: ["asks", "CORE-1", "thread"],
  messages: ["issue-messages", "CORE-1"],
  tree: conversationTreeKey("CORE-1"),
} as const;

/** What each of those reads answers with nothing in it. */
const EMPTY_READS: Readonly<Record<keyof typeof OUTLINE_READS, unknown>> = {
  asks: [],
  messages: [],
  tree: treeAnswer([]),
};

/** Draws the mark for CORE-1 on a page of its own, signed in as alice, and hands back the mark's
 *  element; the reads it makes are the test's to answer. The outline's reads hold their answers
 *  before it mounts, as the outline drawn above it holds them, save those `unanswered` names:
 *  the test answers those. */
function drawMark(unanswered: readonly (keyof typeof OUTLINE_READS)[] = []) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The mark goes as the person reading.
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  for (const read of Object.keys(OUTLINE_READS) as (keyof typeof OUTLINE_READS)[]) {
    if (!unanswered.includes(read)) client.setQueryData(OUTLINE_READS[read], EMPTY_READS[read]);
  }
  const view = render(
    <QueryClientProvider client={client}>
      <IssueReadMark issueKey="CORE-1" />
    </QueryClientProvider>
  );
  const mark = view.container.querySelector("[data-outline-end]");
  if (mark === null) throw new Error("expected the end of the outline");
  return { client, mark, view };
}

test("the end on screen for a second writes the mark once, and nothing when it is there", async () => {
  const viewport = installIntersectionObserver();
  let stored = 1;
  const getMyState = spyOn(api, "getMyState").mockImplementation(
    async (): Promise<UserState> => ({ "CORE-1": issueState(stored) })
  );
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([headEvent(7)]);
  const putIssueState = answerIssueStates().mockImplementation(async (_key, input) =>
    issueState(input.last_read_seq ?? 0)
  );
  try {
    // Under the head: the end on screen for a second reads the issue through it, as the reader.
    const unread = drawMark();
    await waitFor(() =>
      expect(unread.client.getQueryData<UserState>(["user-state"])?.["CORE-1"]?.last_read_seq).toBe(
        1
      )
    );
    await waitFor(() => expect(viewport.observed()).toContain(unread.mark));
    expect(getIssueEvents).toHaveBeenCalledWith("CORE-1", { limit: 1, order: "desc" });
    jest.useFakeTimers();
    try {
      await dwellOn(viewport, unread.mark, { height: 1 });
      expect(putIssueState.mock.calls).toEqual([["CORE-1", { last_read_seq: 7 }, keyedAttempt]]);
      expect(putIssueState.mock.calls[0]?.[2]?.login).toBe("alice");
      expect(unread.client.getQueryData<UserState>(["user-state"])?.["CORE-1"]?.last_read_seq).toBe(
        7
      );
      // Once: the end on screen again, and for longer, writes nothing more.
      await dwellOn(viewport, unread.mark, { height: 1 });
      await jump(2_000);
      expect(putIssueState).toHaveBeenCalledTimes(1);
    } finally {
      realClock();
    }
    unread.view.unmount();

    // At the head already: the same dwell writes nothing.
    stored = 7;
    const read = drawMark();
    await waitFor(() =>
      expect(read.client.getQueryData<UserState>(["user-state"])?.["CORE-1"]?.last_read_seq).toBe(7)
    );
    await waitFor(() => expect(viewport.observed()).toContain(read.mark));
    jest.useFakeTimers();
    try {
      await dwellOn(viewport, read.mark, { height: 1 });
      await jump(2_000);
    } finally {
      realClock();
    }
    expect(putIssueState).toHaveBeenCalledTimes(1);
  } finally {
    getMyState.mockRestore();
    getIssueEvents.mockRestore();
    putIssueState.mockRestore();
    viewport.restore();
  }
});

// The reviewer's finding 4593511267: while the outline's tree read is out the outline draws none
// of the conversation, and the end under it on screen is no end the reader has read to, so it
// writes nothing however long it stays there; once the read answers, the same end on screen for a
// second writes the mark, once. The outline's comments are that read's own nodes, and the mark
// reads no comments list of its own.
test("the end on screen writes nothing until the outline holds its reads, and then once", async () => {
  const viewport = installIntersectionObserver();
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": issueState(1) });
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([headEvent(7)]);
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);
  const putIssueState = answerIssueStates().mockImplementation(async (_key, input) =>
    issueState(input.last_read_seq ?? 0)
  );
  try {
    const page = drawMark(["tree"]);
    await waitFor(() =>
      expect(page.client.getQueryData<UserState>(["user-state"])?.["CORE-1"]?.last_read_seq).toBe(1)
    );
    await waitFor(() => expect(getIssueEvents).toHaveBeenCalledTimes(1));
    jest.useFakeTimers();
    try {
      // The end on screen for two seconds while the tree read is out.
      await dwellOn(viewport, page.mark, { height: 1 });
      await jump(1_000);
    } finally {
      realClock();
    }
    expect(putIssueState).not.toHaveBeenCalled();

    // The outline's tree read answers.
    act(() => page.client.setQueryData(OUTLINE_READS.tree, EMPTY_READS.tree));
    await waitFor(() => expect(viewport.observed()).toContain(page.mark));
    jest.useFakeTimers();
    try {
      await dwellOn(viewport, page.mark, { height: 1 });
      expect(putIssueState.mock.calls).toEqual([["CORE-1", { last_read_seq: 7 }, keyedAttempt]]);
      await dwellOn(viewport, page.mark, { height: 1 });
      await jump(2_000);
    } finally {
      realClock();
    }
    expect(putIssueState).toHaveBeenCalledTimes(1);
    expect(
      listComments,
      "the read mark read the issue's whole comments list"
    ).not.toHaveBeenCalled();
  } finally {
    getMyState.mockRestore();
    getIssueEvents.mockRestore();
    listComments.mockRestore();
    putIssueState.mockRestore();
    viewport.restore();
  }
});
