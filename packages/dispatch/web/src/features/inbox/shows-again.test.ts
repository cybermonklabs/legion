import { expect, spyOn, test } from "bun:test";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { settle } from "../../__tests__/fake-clock";
import { answerAskAnswers, answerAskHandOns, answerAskSnoozes } from "../../__tests__/send-answers";
import { useShownQuery } from "../../api/cache/shown-query";
import { cacheOf, QueryClient, QueryClientProvider } from "../../api/cache/test-client";
import { ApiError, api, askAnswerRequest } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { AnswerAskInput, Ask, InboxRow } from "../../api/types";
import { issueAsk } from "./inbox-fixture";
import { useOutstandingAnswerWrite } from "./outstanding-answers";
import { useAskHandOn } from "./useAskHandOn";
import { useAskSnooze, useAskSnoozeMany } from "./useAskSnooze";

/**
 * The rows here measure R7 of dispatch://INFRA-299 for the four writes the Inbox makes on its own
 * read: the hand-on (`useAskHandOn`), the snooze and the bulk snooze (`useAskSnooze`), and the
 * answer that takes a row out (`outstanding-answers.ts`). Each shows its change through the store
 * (`WriteSpec.shows`), which shows it again over each answer that lands on a cache the write
 * wrote, until the write settles. So each row lets an Inbox answer land while the write is still
 * out - the server's rows as they were, and a change of another row the same answer carries - and
 * reads that the person's change is still there and that the other row's change arrived.
 */

/** The moment the snooze rows defer a row to: a fixed instant, so a change shown again is the
 *  same change. */
const until = "2026-12-01T00:00:00Z";

/** The ask each row writes. */
const askA = issueAsk({ id: "ask-a" });

/** An ask on another issue, which no row here writes: the row an answer landing under an unsaved
 *  change must still bring through. */
const askB = issueAsk({
  id: "ask-b",
  issue: { assignee: "bob", key: "CORE-2", title: "Another issue" },
  issue_key: "CORE-2",
  question: "Which format?",
});

/** A second ask of the reader's, which the bulk snooze defers with `ask-a`. */
const askC = issueAsk({ id: "ask-c", question: "Which name?" });

/** The Inbox as the server has it. */
const inboxRows: InboxRow[] = [askA, askB, askC];

/** The Inbox as another browser has left it while this one holds an unsaved change: every ask as
 *  the server still has it, and `ask-b` moved to P3 by somebody else. */
const inboxSinceOtherBrowser: InboxRow[] = inboxRows.map((row) =>
  row.id === "ask-b" ? { ...row, priority: 3 } : row
);

/** The Inbox's own read and one write of it, over one query client - so one write store
 *  (`Cache.writes`) and one copy of the cache - with the signed-in person every write is stamped
 *  with. */
function renderInboxWrite<T>(use: () => T) {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([...inboxRows]);
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(() => ({ inbox: useShownQuery(inboxQuery()), write: use() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children),
  });
  const rows = (): readonly InboxRow[] => hook.result.current.inbox.data ?? [];
  return {
    client,
    getInbox,
    hook,
    rows,
    row: (id: string): InboxRow | undefined => rows().find((each) => each.id === id),
    write: (): T => hook.result.current.write,
    /** The page as the reader finds it: the Inbox read answered. */
    async ready(): Promise<void> {
      await waitFor(() => expect(rows()).toHaveLength(inboxRows.length));
    },
    /** An event refresh asks the Inbox again, and everything its answer could do runs. */
    async answerLands(next: readonly InboxRow[]): Promise<void> {
      getInbox.mockResolvedValue([...next]);
      await act(async () => {
        void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
        await settle();
      });
      await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(1));
    },
    cleanup(): void {
      hook.unmount();
      getInbox.mockRestore();
    },
  };
}

/** Lets a held route answer, and everything that answer sets off run. */
async function land<T>(held: { resolve: (value: T) => void }, answer: T): Promise<void> {
  await act(async () => {
    held.resolve(answer);
    await settle();
  });
}

test("an inbox answer landing while a hand-on is out keeps the person and brings every other row", async () => {
  const held = Promise.withResolvers<Ask>();
  const handOn = answerAskHandOns().mockImplementation(() => held.promise);
  const view = renderInboxWrite(() => useAskHandOn(askA));
  try {
    await view.ready();
    await act(async () => {
      view.write().submit("bob");
      await settle();
    });
    await waitFor(() => expect(handOn).toHaveBeenCalledTimes(1));

    // The answer lands while the PUT is out. The server still has the ask waiting on alice, and
    // another browser has moved ask-b to P3 since this page read the list.
    await view.answerLands(inboxSinceOtherBrowser);

    expect(view.row("ask-a")?.waiting_on_person).toBe("bob");
    expect(view.row("ask-a")?.asked_of).toBe("bob");
    expect(view.row("ask-b")?.priority).toBe(3);
  } finally {
    await land(held, askA as Ask);
    view.cleanup();
    handOn.mockRestore();
  }
});

test("an inbox answer landing while a snooze is out keeps the moment and brings every other row", async () => {
  const held = Promise.withResolvers<{ snoozed_until: string }>();
  const snooze = answerAskSnoozes().mockImplementation(() => held.promise);
  const view = renderInboxWrite(() => useAskSnooze("ask-a"));
  try {
    await view.ready();
    await act(async () => {
      view.write().submit(until);
      await settle();
    });
    await waitFor(() => expect(snooze).toHaveBeenCalledTimes(1));

    // The server still has ask-a open now; its answer alone would pull the row back out of Later.
    await view.answerLands(inboxSinceOtherBrowser);

    expect(view.row("ask-a")?.snoozed_until).toBe(until);
    expect(view.row("ask-b")?.priority).toBe(3);
  } finally {
    await land(held, { snoozed_until: until });
    view.cleanup();
    snooze.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. Two snoozes of one ask, both live: the first goes out, and the
// second, pressed while the first's moment is on screen, waits behind it in the ask's line.
// Dispatch refuses the first, then the second, and the Inbox cannot be read again - offline, a
// 5xx. The row is left as Dispatch last gave it: never on the first's refused moment, which a
// snapshot taken at the second's press holds, and never on the second's.
test("two snoozes of one ask, both refused while the Inbox cannot be read, leave the row as Dispatch last gave it", async () => {
  const first = Promise.withResolvers<{ snoozed_until: string }>();
  const refusal = new ApiError(422, { code: "INVALID_SNOOZE", error: "not a moment" });
  const snooze = answerAskSnoozes()
    .mockImplementationOnce(() => first.promise)
    .mockImplementation(() => Promise.reject(refusal));
  const later = "2026-12-08T00:00:00Z";
  const view = renderInboxWrite(() => useAskSnooze("ask-a"));
  try {
    await view.ready();
    const confirmed = view.row("ask-a")?.snoozed_until;
    await act(async () => {
      view.write().submit(until);
      await settle();
    });
    await waitFor(() => expect(snooze).toHaveBeenCalledTimes(1));
    await act(async () => {
      view.write().submit(later);
      await settle();
    });
    expect(view.row("ask-a")?.snoozed_until).toBe(later);
    // From here on every read of the Inbox fails.
    view.getInbox.mockRejectedValue(new Error("Failed to fetch"));

    await act(async () => {
      first.reject(refusal);
      await first.promise.catch(() => {});
      await settle();
    });
    await waitFor(() => expect(snooze).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(cacheOf(view.client).writes.rows()).toEqual([]));
    await waitFor(() =>
      expect(view.client.getQueryState(inboxQuery().queryKey)?.status).toBe("error")
    );
    expect(view.row("ask-a")?.snoozed_until).toBe(confirmed);
  } finally {
    view.cleanup();
    snooze.mockRestore();
  }
});

test("an inbox answer landing while a bulk snooze is out keeps every marked row and brings the rest", async () => {
  const held = Promise.withResolvers<{ snoozed_until: string }>();
  const snooze = answerAskSnoozes().mockImplementation(() => held.promise);
  const view = renderInboxWrite(() => useAskSnoozeMany());
  try {
    await view.ready();
    await act(async () => {
      void view.write().submit(["ask-a", "ask-c"], until);
      await settle();
    });
    await waitFor(() => expect(snooze).toHaveBeenCalledTimes(2));

    await view.answerLands(inboxSinceOtherBrowser);

    expect(view.row("ask-a")?.snoozed_until).toBe(until);
    expect(view.row("ask-c")?.snoozed_until).toBe(until);
    expect(view.row("ask-b")?.priority).toBe(3);
  } finally {
    await land(held, { snoozed_until: until });
    view.cleanup();
    snooze.mockRestore();
  }
});

test("an inbox answer landing while an answer is out keeps the row out and brings every other row", async () => {
  const held = Promise.withResolvers<Ask>();
  const answer = answerAskAnswers().mockImplementation(() => held.promise);
  const view = renderInboxWrite(() =>
    useOutstandingAnswerWrite<unknown, AnswerAskInput>({
      askId: "ask-a",
      label: () => "Answer to “Which approach?”",
      request: (input) => askAnswerRequest("ask-a", input),
    })
  );
  try {
    await view.ready();
    await act(async () => {
      view.write().mutate({ expected_edited_at: null, text: "The first one." });
      await settle();
    });
    await waitFor(() => expect(answer).toHaveBeenCalledTimes(1));
    expect(view.row("ask-a")).toBeUndefined();

    // The server has not recorded the answer yet, so its own rows still carry ask-a.
    await view.answerLands(inboxSinceOtherBrowser);

    expect(view.row("ask-a")).toBeUndefined();
    expect(view.row("ask-b")?.priority).toBe(3);
  } finally {
    await land(held, askA as Ask);
    view.cleanup();
    answer.mockRestore();
  }
});
