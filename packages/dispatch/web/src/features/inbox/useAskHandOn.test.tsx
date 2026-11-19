/**
 * The hand-on (`useAskHandOn`) against the Inbox read it moves the ask in: a read asked while the
 * hand-on is out waits for Dispatch's answer.
 */

import { expect, spyOn, test } from "bun:test";
import { QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { api } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { InboxRow } from "../../api/types";
import { issueAsk } from "./inbox-fixture";
import { useAskHandOn } from "./useAskHandOn";

/** Whom each ask of an Inbox read waits on. */
function waitingOn(rows: readonly InboxRow[] | undefined): (string | undefined)[] | undefined {
  return rows?.map((row) => row.waiting_on_person);
}

// A read of the Inbox asked while a hand-on is out - an event's refresh, another surface - can
// reach Dispatch before the hand-on does, and its answer would put the ask back on the person the
// reader just handed it away from while the card still says it is handing on.
test("a read of the Inbox asked while a hand-on is out waits for its answer, and never puts back the person from before the press", async () => {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const ask = issueAsk({ id: "ask-1" });
  // What Dispatch holds: the ask waits on alice, who holds its issue, until it takes the hand-on.
  let served: InboxRow[] = [ask];
  const getInbox = spyOn(api, "getInbox").mockImplementation(async () => served);
  const handOn = Promise.withResolvers<InboxRow>();
  const handAskTo = spyOn(api, "handAskTo").mockReturnValue(handOn.promise);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const { result, unmount } = renderHook(
    () => ({ handOn: useAskHandOn(ask), inbox: useQuery(inboxQuery()).data }),
    { wrapper }
  );
  try {
    await waitFor(() => expect(waitingOn(result.current.inbox)).toEqual(["alice"]));
    act(() => result.current.handOn.submit("bob"));
    await waitFor(() => expect(handAskTo).toHaveBeenCalledWith("ask-1", "bob", keyedAttempt));
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch has not taken the hand-on, so a read that reached it now would answer alice.
    await act(async () => {
      void client.invalidateQueries({ queryKey: inboxQuery().queryKey });
      await settle();
    });
    expect(getInbox).toHaveBeenCalledTimes(1);
    expect(waitingOn(result.current.inbox)).toEqual(["bob"]);

    // Dispatch takes it, stamped with its own moment: the read goes now, and draws what it holds.
    const handed: InboxRow = {
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
    await waitFor(() => expect(getInbox.mock.calls.length).toBeGreaterThan(1));
    await waitFor(() => expect(result.current.inbox).toEqual([handed]));
  } finally {
    unmount();
    getInbox.mockRestore();
    handAskTo.mockRestore();
  }
});
