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
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { AskSnooze, DispatchEvent, InboxRow } from "../../api/types";
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

/** The `user_state.updated` event alice's feed carries once Dispatch has moved `changed` of
 *  `login`'s state: a snooze names `snoozed` and no ask. */
function userStateEvent(login: string, changed: readonly string[]): DispatchEvent {
  return {
    actor: { id: login, kind: "user" },
    created_at: "2026-11-19T09:00:00Z",
    id: 1,
    issue_key: null,
    notify: false,
    payload: { changed, login },
    seq: 1,
    type: "user_state.updated",
  } as DispatchEvent;
}

// The Inbox lists the questions other people ask, so a read held until the snooze's answer,
// however late, would keep back a question asked meanwhile that long. The snooze's own event -
// alice's state, its snoozes moved - says Dispatch has taken it: the held read goes then, and draws
// the row in Later beside the new question while the snooze's answer is still out. Bob's snooze,
// or a change of alice's state that moves no snooze, leaves the read held.
test("a read of the Inbox held behind a snooze goes on the snooze's own event, and shows a question asked meanwhile before its answer", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" })] };
  const snooze = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = answerAskSnoozes().mockReturnValue(snooze.promise);
  const { client, getInbox, hook } = renderBesideInbox(served, () => useAskSnooze("ask-1"));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
  };
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null]));
    act(() => hook.result.current.write.submit(UNTIL));
    await waitFor(() =>
      expect(snoozeAsk).toHaveBeenCalledWith("ask-1", { snoozed_until: UNTIL }, keyedAttempt)
    );
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch has not taken the snooze, so a read that reached it now would answer no moment.
    await act(async () => {
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);

    // Neither bob's snooze nor a change of alice's state that moves no snooze is this snooze.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("bob", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("alice", ["pinned"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch takes the snooze, and somebody asks another question meanwhile: the snooze's event
    // comes, and the read it held goes, with the refresh the event asks for behind it.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
    expect(hook.result.current.write.pending).toBe(true);

    await act(async () => {
      snooze.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    getInbox.mockRestore();
  }
});

// A bulk snooze's asks each publish a snooze event that names no ask, so its hold ends once as
// many of them have come as it has asks: one alone may be the first ask's, with the second not
// yet taken. The held read goes then, and draws both rows in Later beside the question asked
// meanwhile while the asks' answers are still out. A change that moves no snooze counts none.
test("a read of the Inbox held behind a bulk snooze goes once as many snooze events as asks have come, and shows a question asked meanwhile before their answers", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = new Map<string, ReturnType<typeof Promise.withResolvers<AskSnooze>>>();
  answerAskSnoozes().mockImplementation((id) => {
    const answer = Promise.withResolvers<AskSnooze>();
    answers.set(id, answer);
    return answer.promise;
  });
  const { client, getInbox, hook } = renderBesideInbox(served, useAskSnoozeMany);
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
  };
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
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);

    // It takes the first, whose event comes; bob's snooze and a change of alice's state that
    // moves no snooze come too. The second may not be in yet, so the read still waits.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("bob", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("alice", ["pinned"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // It takes the second, and somebody asks another question meanwhile: the second event comes,
    // and the read the set held goes, with the refresh the event asks for behind it.
    served.rows = [
      issueAsk({ id: "ask-1", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-2", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-3" }),
    ];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(getInbox.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
    expect(hook.result.current.write.pending).toBe(true);

    await act(async () => {
      answers.get("ask-1")?.resolve({ snoozed_until: UNTIL });
      answers.get("ask-2")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    expect(await submitted).toEqual({ failed: [] });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    getInbox.mockRestore();
  }
});
