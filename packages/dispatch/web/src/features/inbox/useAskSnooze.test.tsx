/**
 * The snooze writes (`useAskSnoozeWrites`, `useAskSnoozeMany`) against the Inbox's read of the
 * open asks they move the rows in: a read asked while a snooze is out waits for Dispatch's answer.
 */

import { expect, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { jump, onFakeClock, settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { answerAskSnoozes } from "../../__tests__/send-answers";
import { ApiError, NetworkError } from "../../api/client";
import { openAsksQuery, whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { AskSnooze, DispatchEvent, OpenAskRow } from "../../api/types";
import { FIRST_BACKOFF_MS, SEND_DEADLINE_MS } from "../writes/keyed-write";
import { writesOf } from "../writes/write-store";
import { SnoozeWrites } from "./ask-snooze-fixture";
import { answerOpenAsks, issueAsk } from "./inbox-fixture";
import { type AskSnoozeFailure, useAskSnooze, useAskSnoozeMany } from "./useAskSnooze";

const UNTIL = "2026-11-20T09:00:00Z";

/**
 * `use` beside the Inbox's read of the open asks and under the Inbox's snooze writes, under
 * Dispatch's own client signed in as alice, with `GET /api/v1/asks/open` answering what
 * `served.rows` holds when it is asked: what Dispatch holds, which the test moves as Dispatch
 * takes each write.
 */
function renderBesideInbox<T>(served: { rows: OpenAskRow[] }, use: () => T) {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const listOpenAsks = answerOpenAsks(() => served.rows);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <SnoozeWrites>{children}</SnoozeWrites>
    </QueryClientProvider>
  );
  const hook = renderHook(() => ({ inbox: useQuery(openAsksQuery()).data, write: use() }), {
    wrapper,
  });
  return { client, listOpenAsks, hook };
}

/** When each row of a read of the open asks is snoozed until. */
function snoozedUntil(rows: readonly OpenAskRow[] | undefined): (string | null)[] | undefined {
  return rows?.map((row) => row.snoozed_until);
}

// A read of the open asks asked while a snooze is out - an event's refresh, another surface - can
// reach Dispatch before the snooze does, and its answer would bring the row back out of Later
// while the control still says it is saving.
test("a read of the open asks asked while a snooze is out waits for its answer, and never brings the row back out of Later", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" })] };
  const snooze = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = answerAskSnoozes().mockReturnValue(snooze.promise);
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => useAskSnooze("ask-1"));
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null]));
    act(() => hook.result.current.write.submit(UNTIL));
    await waitFor(() =>
      expect(snoozeAsk).toHaveBeenCalledWith("ask-1", { snoozed_until: UNTIL }, keyedAttempt)
    );
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch has not taken the snooze, so a read that reached it now would answer no moment.
    await act(async () => {
      void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch takes it, and somebody asks another question meanwhile: the read goes now, and
    // draws both.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      snooze.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});

// A bulk snooze moves every marked row into Later at once and is out until the last of its asks
// is decided: a read answered before then would bring back out of Later the rows Dispatch has not
// taken yet.
test("a read of the open asks asked while a bulk snooze is out waits until the last of its asks is decided", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = new Map<string, ReturnType<typeof Promise.withResolvers<AskSnooze>>>();
  answerAskSnoozes().mockImplementation((id) => {
    const answer = Promise.withResolvers<AskSnooze>();
    answers.set(id, answer);
    return answer.promise;
  });
  const { client, listOpenAsks, hook } = renderBesideInbox(served, useAskSnoozeMany);
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
      void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // It takes the first; the second is still out, so the read still waits.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      answers.get("ask-1")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
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
      failed: [{ askId: "ask-2", reason: "snoozed_until is too far", stopped: false }],
    });
    await waitFor(() => expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
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
test("a read of the open asks held behind a snooze goes on the snooze's own event, and shows a question asked meanwhile before its answer", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" })] };
  const snooze = Promise.withResolvers<AskSnooze>();
  const snoozeAsk = answerAskSnoozes().mockReturnValue(snooze.promise);
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => useAskSnooze("ask-1"));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
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
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // Neither bob's snooze nor a change of alice's state that moves no snooze is this snooze.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("bob", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("alice", ["pinned"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL]);

    // Dispatch takes the snooze, and somebody asks another question meanwhile: the snooze's event
    // comes, and the read it held goes, with the refresh the event asks for behind it.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
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
    listOpenAsks.mockRestore();
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
  const { client, listOpenAsks, hook } = renderBesideInbox(served, useAskSnoozeMany);
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
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
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // It takes the first, whose event comes; bob's snooze and a change of alice's state that
    // moves no snooze come too. The second may not be in yet, so the read still waits.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("bob", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("alice", ["pinned"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
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
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
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
    listOpenAsks.mockRestore();
  }
});

/** A snooze route that answers each ask's snooze when the row says: each answer, by ask id. */
function heldSnoozeAnswers(): Map<string, (answer: AskSnooze) => void> {
  const answers = new Map<string, (answer: AskSnooze) => void>();
  answerAskSnoozes().mockImplementation((id) => {
    const answer = Promise.withResolvers<AskSnooze>();
    answers.set(id, answer.resolve);
    return answer.promise;
  });
  return answers;
}

// A snooze's event names no ask: Dispatch replays it to every signed-in feed. So with two snoozes
// out, one such event is one of the two, and says nothing of the other, which Dispatch may not
// have taken yet: it ends one hold, and a read of the Inbox asked then still waits, since its
// answer could bring the other row back out of Later. The second event ends the other hold.
test("one snooze event ends the hold of one of two snoozes out, and a read of the Inbox waits for the other's", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = heldSnoozeAnswers();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    first: useAskSnooze("ask-1"),
    second: useAskSnooze("ask-2"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    act(() => hook.result.current.write.first.submit(UNTIL));
    act(() => hook.result.current.write.second.submit(UNTIL));
    await waitFor(() => expect([...answers.keys()].sort()).toEqual(["ask-1", "ask-2"]));
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // Dispatch takes one of the two, whose event comes. The other may not be in yet, so a read
    // asked now waits.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // It takes the other, and somebody asks another question meanwhile: the second event comes,
    // and the read goes.
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
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      answers.get("ask-1")?.({ snoozed_until: UNTIL });
      answers.get("ask-2")?.({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.first.pending).toBe(false));
    await waitFor(() => expect(hook.result.current.write.second.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});

// Dispatch can take a later snooze first and answer it while an older one is still out. That
// answer says the later snooze is in, so its event is due, where the older one may not be in yet:
// the next snooze event goes to the snooze whose answer is in, and a read of the Inbox asked then
// still waits for the older snooze's event.
test("a snooze answered before the one pressed ahead of it takes the next snooze event, and a read of the Inbox waits for the older one's", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = heldSnoozeAnswers();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    first: useAskSnooze("ask-1"),
    second: useAskSnooze("ask-2"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    act(() => hook.result.current.write.first.submit(UNTIL));
    act(() => hook.result.current.write.second.submit(UNTIL));
    await waitFor(() => expect([...answers.keys()].sort()).toEqual(["ask-1", "ask-2"]));

    // Dispatch takes the second and answers it. The first is still out, so the read the second
    // asks as it settles waits.
    served.rows = [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2", snoozed_until: UNTIL })];
    await act(async () => {
      answers.get("ask-2")?.({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.second.pending).toBe(false));
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // The second's event comes after its answer. The first is still out, and the read waits.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // Dispatch takes the first, whose event comes, and somebody asks another question meanwhile:
    // the read goes.
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
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      answers.get("ask-1")?.({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.first.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});

// A bulk snooze of two asks waits for two snooze events, and a snooze pressed after it for one.
// Each event is one write's, so it counts toward one hold alone, the oldest still waiting for
// one: the first two end the bulk snooze's hold, and a read of the Inbox asked then still waits
// for the third, which ends the later snooze's.
test("a bulk snooze of two asks and a snooze pressed after it hold the Inbox until three snooze events have come", async () => {
  const served = {
    rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" }), issueAsk({ id: "ask-3" })],
  };
  const answers = heldSnoozeAnswers();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    many: useAskSnoozeMany(),
    one: useAskSnooze("ask-3"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() =>
      expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null, null])
    );
    let submitted: Promise<{ failed: readonly AskSnoozeFailure[] }> | undefined;
    act(() => {
      submitted = hook.result.current.write.many.submit(["ask-1", "ask-2"], UNTIL);
    });
    await waitFor(() => expect([...answers.keys()].sort()).toEqual(["ask-1", "ask-2"]));
    act(() => hook.result.current.write.one.submit(UNTIL));
    await waitFor(() => expect([...answers.keys()].sort()).toEqual(["ask-1", "ask-2", "ask-3"]));
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL, UNTIL]);

    // Two snooze events come: as many as the bulk snooze has asks, but one of them may be the
    // later snooze's, and a read asked now waits.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL, UNTIL]);

    // The third comes, and the read goes.
    served.rows = [
      issueAsk({ id: "ask-1", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-2", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-3", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-4" }),
    ];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      for (const answer of answers.values()) answer({ snoozed_until: UNTIL });
      await settle();
    });
    expect(await submitted).toEqual({ failed: [] });
    await waitFor(() => expect(hook.result.current.write.one.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});

/**
 * Has Dispatch take a snooze later than its deadline and then a snooze pressed after it: the first
 * snooze's hold lets the Inbox's reads go at its deadline, with the snooze still out, and the
 * reader then snoozes the second ask. Dispatch answers the first, and its event comes
 * `eventAfterAnswer` ms after that answer. A read of the Inbox asked then waits through the first
 * snooze's event for the second's, and then shows what Dispatch holds.
 */
async function snoozePastDeadlineThenAnother(eventAfterAnswer: number): Promise<void> {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const answers = heldSnoozeAnswers();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    first: useAskSnooze("ask-1"),
    second: useAskSnooze("ask-2"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    act(() => hook.result.current.write.first.submit(UNTIL));
    await waitFor(() => expect([...answers.keys()]).toEqual(["ask-1"]));

    // Dispatch has not taken the first snooze by its deadline, and the reader snoozes the second
    // ask.
    await jump(SEND_DEADLINE_MS);
    act(() => hook.result.current.write.second.submit(UNTIL));
    await waitFor(() => expect([...answers.keys()]).toEqual(["ask-1", "ask-2"]));
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // Dispatch takes the first and answers it. The second is still out, so the read the first
    // asks as it settles waits.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      answers.get("ask-1")?.({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.first.pending).toBe(false));
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // The first's event comes after its answer. It is the first's, and the read waits still.
    if (eventAfterAnswer > 0) await jump(eventAfterAnswer);
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([UNTIL, UNTIL]);

    // Dispatch takes the second, whose event comes, and somebody asks another question meanwhile:
    // the read goes.
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
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      answers.get("ask-2")?.({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.second.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
}

// Dispatch can take a snooze later than its deadline: its hold lets the Inbox's reads go then,
// and the snooze is still out. Its event still comes once Dispatch takes it, here after its
// answer, and is still that snooze's. A snooze pressed after the deadline holds the Inbox until
// its own event: the late snooze's event, though it names no ask, ends no hold of the later one.
test("a snooze past its deadline takes its own event after its answer, and a read of the Inbox waits for the event of the snooze pressed after it", async () => {
  await onFakeClock(() => snoozePastDeadlineThenAnother(0));
});

// The page's live feed carries an event only once every write transaction open on Dispatch's
// Postgres instance before it has ended (the safe horizon of the event stream relay), so with the
// socket up all along a snooze's event can come seconds after its answer. It is still that
// snooze's, and ends no hold of the snooze pressed after it.
test("a snooze past its deadline takes its own event 5 s after its answer, and a read of the Inbox waits for the event of the snooze pressed after it", async () => {
  await onFakeClock(() => snoozePastDeadlineThenAnother(5_000));
});

/** How the row has Dispatch decide one snooze: answer it, or refuse it. */
interface SnoozeVerdict {
  readonly resolve: (answer: AskSnooze) => void;
  readonly reject: (refusal: unknown) => void;
}

/** A snooze route that answers or refuses each ask's snooze when the row says: each snooze's
 *  verdict, by ask id. */
function heldSnoozeVerdicts(): Map<string, SnoozeVerdict> {
  const verdicts = new Map<string, SnoozeVerdict>();
  answerAskSnoozes().mockImplementation((id) => {
    const verdict = Promise.withResolvers<AskSnooze>();
    verdicts.set(id, verdict);
    return verdict.promise;
  });
  return verdicts;
}

/** What Dispatch answers a snooze it refuses. */
const refusal = () =>
  new ApiError(400, { code: "SNOOZE_INPUT", error: "snoozed_until is too far" });

// Dispatch commits nothing of a snooze it refuses, so it publishes no event for it. The refused
// snooze's hold waits for none: the next snooze event is the one pressed after it, and ends that
// one's hold, so a read of the Inbox asked then goes.
test("a snooze Dispatch refuses waits for no event, and the next snooze event ends the hold of the snooze pressed after it", async () => {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const verdicts = heldSnoozeVerdicts();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    first: useAskSnooze("ask-1"),
    second: useAskSnooze("ask-2"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    act(() => hook.result.current.write.first.submit(UNTIL));
    act(() => hook.result.current.write.second.submit(UNTIL));
    await waitFor(() => expect([...verdicts.keys()].sort()).toEqual(["ask-1", "ask-2"]));

    // Dispatch refuses the first, and the read it asks as it settles waits for the second.
    await act(async () => {
      verdicts.get("ask-1")?.reject(refusal());
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.first.failed).toBe(true));
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // Dispatch takes the second, whose event comes, and somebody asks another question meanwhile:
    // the read goes.
    served.rows = [
      issueAsk({ id: "ask-1" }),
      issueAsk({ id: "ask-2", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-3" }),
    ];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      verdicts.get("ask-2")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.second.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});

/**
 * Has Dispatch take a first snooze whose answer is lost on the way back, and refuse the page's
 * retry of it, the snooze's moment having passed: a snooze is a `PUT`, which sends no key, so the
 * retry runs Dispatch's handler again. `retry` is how the page comes to try it again. A second
 * snooze is pressed meanwhile and stays out. That refusal says nothing of the attempt the page had
 * no answer to, so the first snooze's hold still waits for its event, which comes and ends no
 * hold of the second: a read of the Inbox waits for the second's own event.
 */
async function snoozeRefusedAfterLostAnswer(
  retry: (client: DispatchQueryClient) => Promise<void>
): Promise<void> {
  const served = { rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" })] };
  const verdicts = heldSnoozeVerdicts();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    first: useAskSnooze("ask-1"),
    second: useAskSnooze("ask-2"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    // Signed in as the app's shell signs in on each load of the page.
    act(() => writesOf(client).signedIn("alice"));
    await waitFor(() => expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null]));
    act(() => hook.result.current.write.first.submit(UNTIL));
    await waitFor(() => expect([...verdicts.keys()]).toEqual(["ask-1"]));
    const firstAttempt = verdicts.get("ask-1");

    // Dispatch takes the first snooze, and its answer is lost on the way back.
    served.rows = [issueAsk({ id: "ask-1", snoozed_until: UNTIL }), issueAsk({ id: "ask-2" })];
    await act(async () => {
      firstAttempt?.reject(
        new NetworkError("/api/v1/asks/ask-1/snooze", new TypeError("Failed to fetch"))
      );
      await settle();
    });
    // The reader snoozes the second ask, which stays out.
    act(() => hook.result.current.write.second.submit(UNTIL));
    await waitFor(() => expect(verdicts.has("ask-2")).toBe(true));

    // The page tries the first again, and Dispatch refuses the retry. The read the first asks as
    // it settles waits for the second.
    await retry(client);
    await waitFor(() => expect(verdicts.get("ask-1")).not.toBe(firstAttempt));
    await act(async () => {
      verdicts.get("ask-1")?.reject(refusal());
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.first.failed).toBe(true));
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // The first snooze's event comes. It is the first's, and the read waits still.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // Dispatch takes the second, whose event comes, and somebody asks another question
    // meanwhile: the read goes.
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
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      verdicts.get("ask-2")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.second.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
}

// The page's own backoff tries the snooze again, in the same try.
test("a snooze refused after an attempt it had no answer to takes its own event, and a read of the Inbox waits for the event of the snooze pressed after it", async () => {
  await onFakeClock(() => snoozeRefusedAfterLostAnswer(() => jump(FIRST_BACKOFF_MS)));
});

// The session lapses while the page waits to try the snooze again, which stops that try, and the
// same person signs in again: the page takes the snooze up in a try of its own, whose first attempt
// is the one Dispatch refuses. The attempt it had no answer to was the earlier try's, and the
// refusal still says nothing of it.
test("a snooze refused after a lapse and a sign-in, its first attempt unanswered before the lapse, takes its own event, and a read of the Inbox waits for the event of the snooze pressed after it", async () => {
  await onFakeClock(() =>
    snoozeRefusedAfterLostAnswer(async (client) => {
      act(() => writesOf(client).signedIn(undefined));
      await settle();
      act(() => writesOf(client).signedIn("alice"));
      await settle();
    })
  );
});

// A bulk snooze waits for one snooze event for each ask, and Dispatch publishes none for an ask it
// refuses. So a bulk snooze of two asks, one refused, waits for one event: the snooze event after
// it is the one of a snooze pressed after the bulk snooze, and ends that one's hold.
test("a bulk snooze waits for no event of an ask Dispatch refused, and the next snooze event ends the hold of a snooze pressed after it", async () => {
  const served = {
    rows: [issueAsk({ id: "ask-1" }), issueAsk({ id: "ask-2" }), issueAsk({ id: "ask-3" })],
  };
  const verdicts = heldSnoozeVerdicts();
  const { client, listOpenAsks, hook } = renderBesideInbox(served, () => ({
    many: useAskSnoozeMany(),
    one: useAskSnooze("ask-3"),
  }));
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() =>
      expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, null, null])
    );
    let submitted: Promise<{ failed: readonly AskSnoozeFailure[] }> | undefined;
    act(() => {
      submitted = hook.result.current.write.many.submit(["ask-1", "ask-2"], UNTIL);
    });
    await waitFor(() => expect([...verdicts.keys()].sort()).toEqual(["ask-1", "ask-2"]));
    act(() => hook.result.current.write.one.submit(UNTIL));
    await waitFor(() => expect([...verdicts.keys()].sort()).toEqual(["ask-1", "ask-2", "ask-3"]));

    // Dispatch refuses the first ask and takes the second. The read the bulk snooze asks once it
    // is decided waits for the later snooze.
    await act(async () => {
      verdicts.get("ask-1")?.reject(refusal());
      verdicts.get("ask-2")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    expect(await submitted).toEqual({
      failed: [{ askId: "ask-1", reason: "snoozed_until is too far", stopped: false }],
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(snoozedUntil(hook.result.current.inbox)).toEqual([null, UNTIL, UNTIL]);

    // The second ask's event comes. The later snooze is still out, and the read waits.
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // Dispatch takes the later snooze, whose event comes, and somebody asks another question
    // meanwhile: the read goes.
    served.rows = [
      issueAsk({ id: "ask-1" }),
      issueAsk({ id: "ask-2", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-3", snoozed_until: UNTIL }),
      issueAsk({ id: "ask-4" }),
    ];
    await act(async () => {
      hearLiveEvent(client, userStateEvent("alice", ["snoozed"]), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(hook.result.current.inbox).toEqual(served.rows));

    await act(async () => {
      verdicts.get("ask-3")?.resolve({ snoozed_until: UNTIL });
      await settle();
    });
    await waitFor(() => expect(hook.result.current.write.one.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(hook.result.current.inbox).toEqual(served.rows);
  } finally {
    hook.unmount();
    listOpenAsks.mockRestore();
  }
});
