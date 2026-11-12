import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { answerAgentStates } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type { UserAgentState, UserAgentStates } from "../../api/types";
import { writesOf } from "../writes/write-store";
import { legacyPinnedIds, pinnedAgentSessions, useAgentPinWrite } from "./pins";

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

// R7 of dispatch://INFRA-299: while a pin is live it shows its change again over each answer that
// lands on the viewer's agent state. The change is that one session's `pinned_at`, over the state
// as it stands, so a read composed before Dispatch took the pin leaves the pin on screen, and
// what that read says of every other session arrives whole.
test("a pin stays shown over an answer to the viewer's agent state that lands while it is out", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue({
    "planner-session": { unread_replies: 0 },
    "tester-session": { unread_replies: 0 },
  });
  // A pin Dispatch never answers, so it stays live for the read below.
  const put = answerAgentStates().mockImplementation(
    () => Promise.withResolvers<UserAgentState>().promise
  );
  const states = () => queryClient.getQueryData<UserAgentStates>(userAgentStateQuery().queryKey);
  const hook = renderHook(() => useAgentPinWrite(), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  try {
    await queryClient.fetchQuery(userAgentStateQuery());
    await act(async () => {
      void hook.result.current("planner-session", true);
    });
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(pinnedAgentSessions(states())).toEqual(["planner-session"]);

    // The page reads the agent state again while the pin is out: Dispatch has not taken it yet,
    // and a reply from the tester arrived with that answer.
    getMyAgentState.mockResolvedValue({
      "planner-session": { unread_replies: 0 },
      "tester-session": { unread_replies: 3 },
    });
    await act(() => queryClient.refetchQueries({ queryKey: userAgentStateQuery().queryKey }));

    expect(pinnedAgentSessions(states())).toEqual(["planner-session"]);
    expect(states()?.["tester-session"]?.unread_replies).toBe(3);
  } finally {
    hook.unmount();
    writesOf(queryClient).clear();
    getMyAgentState.mockRestore();
  }
});
