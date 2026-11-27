/**
 * The hand-on (`useAskHandOn`) against the Inbox's read of the open asks it moves the ask in: a
 * read asked while the hand-on is out waits for Dispatch's answer.
 */

import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import { openAsksQuery, whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient, hearLiveEvent } from "../../api/query-client";
import type { DispatchEvent, OpenAskRow } from "../../api/types";
import { answerOpenAsks, issueAsk } from "./inbox-fixture";
import { useAskHandOn } from "./useAskHandOn";

/** Whom each ask of a read of the open asks waits on. */
function waitingOn(rows: readonly OpenAskRow[] | undefined): (string | undefined)[] | undefined {
  return rows?.map((row) => row.waiting_on_person);
}

// A read of the open asks asked while a hand-on is out - an event's refresh, another surface - can
// reach Dispatch before the hand-on does, and its answer would put the ask back on the person the
// reader just handed it away from while the card still says it is handing on.
test("a read of the open asks asked while a hand-on is out waits for its answer, and never puts back the person from before the press", async () => {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const ask = issueAsk({ id: "ask-1" });
  // What Dispatch holds: the ask waits on alice, who holds its issue, until it takes the hand-on.
  let served: OpenAskRow[] = [ask];
  const listOpenAsks = answerOpenAsks(() => served);
  const handOn = Promise.withResolvers<OpenAskRow>();
  const handAskTo = spyOn(api, "handAskTo").mockReturnValue(handOn.promise);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const { result, unmount } = renderHook(
    () => ({ handOn: useAskHandOn(ask), inbox: useQuery(openAsksQuery()).data }),
    { wrapper }
  );
  try {
    await waitFor(() => expect(waitingOn(result.current.inbox)).toEqual(["alice"]));
    act(() => result.current.handOn.submit("bob"));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-1", "bob", keyedAttempt));
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch has not taken the hand-on, so a read that reached it now would answer alice.
    await act(async () => {
      void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch takes it, stamped with its own moment: the read goes now, and draws what it holds.
    const handed: OpenAskRow = {
      ...ask,
      asked_of: "bob",
      asked_of_at: "2026-11-19T09:00:00Z",
      asked_of_by: "alice",
      waiting_on_person: "bob",
    };
    served = [handed];
    await act(async () => {
      handOn.resolve(handed);
      await settle();
    });
    await waitFor(() => expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(result.current.inbox).toEqual([handed]));
  } finally {
    unmount();
    listOpenAsks.mockRestore();
    handAskTo.mockRestore();
  }
});

/** The event alice's feed carries once Dispatch has written ask `id`: `type`, by `actor`. */
function askEvent(
  type: "ask.answered" | "ask.asked_of",
  id: string,
  actor = "alice"
): DispatchEvent {
  return {
    actor: { id: actor, kind: "user" },
    created_at: "2026-11-19T09:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: { id },
    seq: 1,
    type,
  } as DispatchEvent;
}

// The Inbox lists the questions other people ask, so a read held until the hand-on's answer,
// however late, would keep back a question asked meanwhile that long. The hand-on's own event -
// `ask.asked_of` for its ask - says Dispatch has taken it: the held read goes then, and draws the
// ask on bob beside the new question while the hand-on's answer is still out. Another ask's
// hand-on, an answer of this ask, or carol's hand-on of this ask leaves the read held.
test("a read of the open asks held behind a hand-on goes on the hand-on's own event, and shows a question asked meanwhile before its answer", async () => {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const ask = issueAsk({ id: "ask-1" });
  let served: OpenAskRow[] = [ask];
  const listOpenAsks = answerOpenAsks(() => served);
  const handOn = Promise.withResolvers<OpenAskRow>();
  const handAskTo = spyOn(api, "handAskTo").mockReturnValue(handOn.promise);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const { result, unmount } = renderHook(
    () => ({ handOn: useAskHandOn(ask), inbox: useQuery(openAsksQuery()).data }),
    { wrapper }
  );
  const readInboxAgain = () => {
    void client.invalidateQueries({ queryKey: openAsksQuery().queryKey });
  };
  try {
    await waitFor(() => expect(waitingOn(result.current.inbox)).toEqual(["alice"]));
    act(() => result.current.handOn.submit("bob"));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-1", "bob", keyedAttempt));
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch has not taken the hand-on, so a read that reached it now would answer alice.
    await act(async () => {
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);

    // Neither another ask's hand-on, nor an answer of this one, nor carol's hand-on of this one
    // is alice's hand-on.
    await act(async () => {
      hearLiveEvent(client, askEvent("ask.asked_of", "ask-4"), "alice");
      hearLiveEvent(client, askEvent("ask.answered", ask.id), "alice");
      hearLiveEvent(client, askEvent("ask.asked_of", ask.id, "carol"), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks).toHaveBeenCalledTimes(1);
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch takes the hand-on, and somebody asks alice a new question meanwhile: the hand-on's
    // event comes, and the read it held goes, with the refresh the event asks for behind it.
    const handed: OpenAskRow = {
      ...ask,
      asked_of: "bob",
      asked_of_at: "2026-11-19T09:00:00Z",
      asked_of_by: "alice",
      waiting_on_person: "bob",
    };
    served = [handed, issueAsk({ id: "ask-2" })];
    await act(async () => {
      hearLiveEvent(client, askEvent("ask.asked_of", ask.id), "alice");
      readInboxAgain();
      await settle();
    });
    expect(listOpenAsks.mock.calls.length).toBeGreaterThan(1);
    await waitFor(() => expect(result.current.inbox).toEqual(served));
    expect(result.current.handOn.pending).toBe(true);

    await act(async () => {
      handOn.resolve(handed);
      await settle();
    });
    await waitFor(() => expect(result.current.handOn.pending).toBe(false));
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(result.current.inbox).toEqual(served);
  } finally {
    unmount();
    listOpenAsks.mockRestore();
    handAskTo.mockRestore();
  }
});
