import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { settle } from "../../__tests__/fake-clock";
import { answerAgentStates } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type { UserAgentState, UserAgentStates } from "../../api/types";
import { legacyPinnedIds, pinnedAgentSessions, useAgentPinWrite, usePinnedAgents } from "./pins";

// The rail lists the person's pins oldest first (INFRA-812, R1), so the agent they pinned first
// keeps the top row however their agent state is keyed, and a session they have only read or
// cleared is not in the rail at all.
test("the pins are the sessions with a pinned_at, oldest pin first", () => {
  const states: UserAgentStates = {
    "tester-session": { pinned_at: "2026-09-14T00:00:03Z", unread_replies: 0 },
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z", unread_replies: 2 },
    "reviewer-session": { pinned_at: "2026-09-14T00:00:02Z", unread_replies: 0 },
    "quiet-session": { read_through: "2026-09-14T00:00:00Z", unread_replies: 1 },
  };

  expect(pinnedAgentSessions(states)).toEqual([
    "planner-session",
    "reviewer-session",
    "tester-session",
  ]);
});

// The server writes RFC 3339 with its trailing fractional zeros dropped, which does not sort as
// text (`lib/timestamps.ts`): a pin made at `…00.12Z` is older than one at `…00.123456Z`.
test("two pins in the same second are ordered by their fractions, not as text", () => {
  const states: UserAgentStates = {
    "second-session": { pinned_at: "2026-09-14T00:00:00.123456Z", unread_replies: 0 },
    "first-session": { pinned_at: "2026-09-14T00:00:00.12Z", unread_replies: 0 },
  };

  expect(pinnedAgentSessions(states)).toEqual(["first-session", "second-session"]);
});

test("nothing is pinned when the viewer has no agent state yet", () => {
  expect(pinnedAgentSessions(undefined)).toEqual([]);
  expect(pinnedAgentSessions({})).toEqual([]);
});

// A `pinned_at` this page cannot read orders nothing, and throwing on it would take the whole
// rail - every other pin with it - off the page.
test("a pin whose time cannot be read is listed last, not thrown on", () => {
  const states: UserAgentStates = {
    "broken-session": { pinned_at: "yesterday", unread_replies: 0 },
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z", unread_replies: 0 },
  };

  expect(pinnedAgentSessions(states)).toEqual(["planner-session", "broken-session"]);
});

// What the shell migrates once: the ids this browser kept under the old preference key.
test("the old key's session ids are read in the order it kept them", () => {
  expect(legacyPinnedIds(JSON.stringify(["planner-session", "reviewer-session"]))).toEqual([
    "planner-session",
    "reviewer-session",
  ]);
});

test("an old key holding no list of ids carries no pins", () => {
  expect(legacyPinnedIds(null)).toEqual([]);
  expect(legacyPinnedIds("")).toEqual([]);
  expect(legacyPinnedIds("not json")).toEqual([]);
  expect(legacyPinnedIds(JSON.stringify({ "planner-session": true }))).toEqual([]);
  expect(legacyPinnedIds(JSON.stringify(["planner-session", 7, null]))).toEqual([
    "planner-session",
  ]);
});

/** Alice's agent states as Dispatch holds them before she pins the planner: read, not pinned. */
const unpinned: UserAgentStates = { "planner-session": { unread_replies: 0 } };

/** Dispatch's answer to her pin of the planner. */
const pinAnswer: UserAgentState = { pinned_at: "2026-09-14T00:00:01Z", unread_replies: 0 };

/** Dispatch's answers to the page's reads of the viewer's agent states, each held until the test
 *  gives it: `asked[n]` is the read the page made n-th. */
function heldAgentStateReads() {
  const asked: ReturnType<typeof Promise.withResolvers<UserAgentStates>>[] = [];
  const getMyAgentState = spyOn(api, "getMyAgentState").mockImplementation(() => {
    const read = Promise.withResolvers<UserAgentStates>();
    asked.push(read);
    return read.promise;
  });
  return { asked, getMyAgentState };
}

/** A pin control and the rail's pins, as a page of alice's mounts them over `client`. */
function renderPins(client: QueryClient) {
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  return renderHook(() => ({ pin: useAgentPinWrite(), pinned: usePinnedAgents() }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children),
  });
}

/** Asks the page's agent states again, as an event's refresh or another surface does. */
function readAgentStatesAgain(client: QueryClient): void {
  void client.invalidateQueries({ queryKey: userAgentStateQuery().queryKey });
}

// A read of the agent states that was out when the reader pressed Pin was answered from the
// states before the press. When it lands after the press, the session the reader just pinned
// must not drop out of the rail again while the pin is still out.
test("a read asked before a pin press does not take the session out of the rail when it answers", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const reads = heldAgentStateReads();
  const write = Promise.withResolvers<UserAgentState>();
  const putAgentState = answerAgentStates().mockImplementation(() => write.promise);
  const hook = renderPins(client);
  try {
    await waitFor(() => expect(reads.asked).toHaveLength(1));
    await act(async () => reads.asked[0]?.resolve(unpinned));
    expect(hook.result.current.pinned).toEqual([]);

    act(() => readAgentStatesAgain(client));
    await waitFor(() => expect(reads.asked).toHaveLength(2));
    let pinned: Promise<boolean> | undefined;
    act(() => {
      pinned = hook.result.current.pin("planner-session", true);
    });
    await waitFor(() => expect(hook.result.current.pinned).toEqual(["planner-session"]));

    // The read asked before the press answers the states from before it.
    await act(async () => {
      reads.asked[1]?.resolve(unpinned);
      await settle();
    });
    expect(hook.result.current.pinned).toEqual(["planner-session"]);

    await act(async () => {
      write.resolve(pinAnswer);
      expect(await pinned).toBe(true);
    });
    expect(hook.result.current.pinned).toEqual(["planner-session"]);
  } finally {
    hook.unmount();
    putAgentState.mockRestore();
    reads.getMyAgentState.mockRestore();
  }
});

// A read of the agent states asked while a pin is out could reach Dispatch before the pin does,
// and its answer, the states from before the press, would take the session out of the rail
// while the pin is still out. The read waits until the pin's answer is in (`holds`), and then
// reads what Dispatch holds after the pin.
test("a read asked while a pin is out waits for the pin's answer", async () => {
  const client = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const reads = heldAgentStateReads();
  const write = Promise.withResolvers<UserAgentState>();
  const putAgentState = answerAgentStates().mockImplementation(() => write.promise);
  const hook = renderPins(client);
  try {
    await waitFor(() => expect(reads.asked).toHaveLength(1));
    await act(async () => reads.asked[0]?.resolve(unpinned));

    let pinned: Promise<boolean> | undefined;
    act(() => {
      pinned = hook.result.current.pin("planner-session", true);
    });
    await waitFor(() => expect(putAgentState).toHaveBeenCalledTimes(1));
    expect(hook.result.current.pinned).toEqual(["planner-session"]);

    await act(async () => {
      readAgentStatesAgain(client);
      await settle();
    });
    expect(reads.getMyAgentState).toHaveBeenCalledTimes(1);
    expect(hook.result.current.pinned).toEqual(["planner-session"]);

    await act(async () => {
      write.resolve(pinAnswer);
      expect(await pinned).toBe(true);
    });
    expect(hook.result.current.pinned).toEqual(["planner-session"]);

    // The read goes out once the answer is in, and what Dispatch holds now - the reviewer pinned
    // in another browser as well - is what the rail shows.
    await waitFor(() => expect(reads.getMyAgentState).toHaveBeenCalledTimes(2));
    await act(async () => {
      reads.asked[1]?.resolve({
        "planner-session": pinAnswer,
        "reviewer-session": { pinned_at: "2026-09-14T00:00:02Z", unread_replies: 0 },
      });
      await settle();
    });
    expect(hook.result.current.pinned).toEqual(["planner-session", "reviewer-session"]);
  } finally {
    hook.unmount();
    putAgentState.mockRestore();
    reads.getMyAgentState.mockRestore();
  }
});
