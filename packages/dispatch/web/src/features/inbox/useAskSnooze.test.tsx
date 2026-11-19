/**
 * The snooze writes (`useAskSnooze`, `useAskSnoozeMany`) against the Inbox read they move the
 * rows in: a read asked while a snooze is out waits for Dispatch's answer.
 */

import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerAskSnoozes } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { AskSnooze, InboxRow } from "../../api/types";
import { issueAsk } from "./inbox-fixture";
import { type AskSnoozeFailure, useAskSnooze, useAskSnoozeMany } from "./useAskSnooze";

const UNTIL = "2026-11-20T09:00:00Z";

/**
 * `use` beside the Inbox's read, under Dispatch's own client signed in as alice, with
 * `GET /api/v1/inbox` answering what `served.rows` holds when it is asked: what Dispatch holds,
 * which the test moves as Dispatch takes each write.
 */
function renderBesideInbox<T>(served: { rows: InboxRow[] }, use: () => T) {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served.rows);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(() => ({ inbox: useQuery(inboxQuery()).data, write: use() }), {
    wrapper,
  });
  return { client, getInbox, hook };
}

/** When each row of an Inbox read is snoozed until. */
function snoozedUntil(rows: readonly InboxRow[] | undefined): (string | null)[] | undefined {
  return rows?.map((row) => row.snoozed_until);
}

// A read of the Inbox asked while a snooze is out - an event's refresh, another surface - can
// reach Dispatch before the snooze does, and its answer would bring the row back out of Later
// while the control still says it is saving.
test("a read of the Inbox asked while a snooze is out waits for its answer, and never brings the row back out of Later", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" })] };
  const snooze = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = answerAskSnoozes().mockReturnValue(snooze.promise);
  const { client, getInbox, hook } = renderBesideInbox(served, () => useAskSnooze("ask-1"));
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null]));
    act(() => hook.result.current.write.submit(UNTIL));
    await waitFor(() =>
      expect(snoozeAsk).toHaveBeenCalledWith("ask-1", { snoozed_until: UNTIL }, keyedAttempt)
    );
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch has not taken the snooze, so a read that reached it now would answer no moment.
    await act(async () => {
      void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch takes it, and somebody asks another question meanwhile: the read goes now, and
    // draws both.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      snooze.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
  } finally {
    hook.unmount();
    getInbox.mockRestore();
  }
});

// A bulk snooze moves every marked row into Later at once and is out until the last of its asks
// is decided: a read answered before then would bring back out of Later the rows Dispatch has not
// taken yet.
test("a read of the Inbox asked while a bulk snooze is out waits until the last of its asks is decided", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = new Map<string, ReturnType<typeof Promise.withResolvers<AskSnooze>>>();
  answerAskSnoozes().mockImplementation((id) => {
    const answer = Promise.withResolvers<AskSnooze>();
    answers.set(id, answer);
    return answer.promise;
  });
  const { client, getInbox, hook } = renderBesideInbox(served, useAskSnoozeMany);
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    let submitted: Promise<{ failed: readonly AskSnoozeFailure[] }> | undefined;
    act(() => {
      submitted = hook.result.current.write.submit(["ask-1", "ask-2"], UNTIL);
    });
    await waitFor(() => expect([...answers.keys()]).toEqual(["ask-1", "ask-2"]));
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // Dispatch has taken neither, so a read that reached it now would answer no moment for both.
    await act(async () => {
      void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);

    // It takes the first; the second is still out, so the read still waits.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      answers.get("ask-1")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // It refuses the second, and somebody asks another question meanwhile: that row is rolled
    // back, and then the read goes and draws what Dispatch holds.
    served.rows = [...served.rows, issueAsk({ id: "ask-3" })];
    await act(async () => {
      answers
        .get("ask-2")
        ?.reject(new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until is too far" }));
      await settle();
    });
    expect(await submitted).toEqual({
      failed: [{ askId: "ask-2", reason: "snoozed_until is too far" }],
    });
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
  } finally {
    hook.unmount();
    getInbox.mockRestore();
  }
});
