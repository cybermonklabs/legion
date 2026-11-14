import { expect, spyOn, test } from "bun:test";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { answerAgentStates } from "../../__tests__/send-answers";
import {
  answeredByDispatch,
  cacheOf,
  QueryClient,
  QueryClientProvider,
  QueryObserver,
} from "../../api/cache/test-client";
import { storeAgentState } from "../../api/cache/viewer-state";
import { ApiError, api, NetworkError } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type { UserAgentState, UserAgentStates } from "../../api/types";
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
  const states = () => cacheOf(queryClient).shown<UserAgentStates>(userAgentStateQuery().queryKey);
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
    cacheOf(queryClient).writes.clear();
    getMyAgentState.mockRestore();
  }
});

// R10 of dispatch://INFRA-299. A pin's press undoes nothing itself: a refused pin stops being
// composed, so the page shows the agent state as Dispatch last gave it, before the state is read
// again. Where that read fails - offline, a 5xx - no session is left pinned on the page.
test("a refused pin leaves the agent state Dispatch last gave when the read again fails", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const getMyAgentState = spyOn(api, "getMyAgentState")
    .mockResolvedValueOnce({ "planner-session": { unread_replies: 0 } })
    .mockRejectedValue(new NetworkError("/api/v1/me/agents/state", new TypeError("offline")));
  answerAgentStates().mockRejectedValue(
    new ApiError(422, { code: "INVALID_STATE", error: "not a state" })
  );
  // A surface drawing the viewer's agent state, so the read again goes out.
  const unsubscribe = new QueryObserver(queryClient, userAgentStateQuery()).subscribe(() => {});
  const hook = renderHook(() => useAgentPinWrite(), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  try {
    await queryClient.fetchQuery(userAgentStateQuery());
    let made: boolean | undefined;
    await act(async () => {
      made = await hook.result.current("planner-session", true);
    });
    expect(made).toBe(false);
    await waitFor(() => expect(getMyAgentState).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(queryClient.getQueryState(userAgentStateQuery().queryKey)?.status).toBe("error")
    );
    expect(
      pinnedAgentSessions(
        cacheOf(queryClient).shown<UserAgentStates>(userAgentStateQuery().queryKey)
      )
    ).toEqual([]);
  } finally {
    hook.unmount();
    unsubscribe();
    cacheOf(queryClient).writes.clear();
    getMyAgentState.mockRestore();
  }
});

// R7 and R10 of dispatch://INFRA-299. Another session's state a PUT answered with - a read mark,
// a Clear - is merged while a pin is out (`storeAgentState`), into the agent state the cache
// holds, Dispatch's alone, the pin being composed over it as it is read. So once the pin is
// refused and the read again fails, the page shows that answer and no pin Dispatch never took.
test("an agent state answer merged while a pin is out stays once the pin is refused and the read fails", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const getMyAgentState = spyOn(api, "getMyAgentState")
    .mockResolvedValueOnce({
      "planner-session": { unread_replies: 0 },
      "tester-session": { unread_replies: 3 },
    })
    .mockRejectedValue(new NetworkError("/api/v1/me/agents/state", new TypeError("offline")));
  const refusal = Promise.withResolvers<UserAgentState>();
  const put = answerAgentStates().mockImplementation(() => refusal.promise);
  // A surface drawing the viewer's agent state, so the read again goes out.
  const unsubscribe = new QueryObserver(queryClient, userAgentStateQuery()).subscribe(() => {});
  const hook = renderHook(() => useAgentPinWrite(), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  const states = () => cacheOf(queryClient).shown<UserAgentStates>(userAgentStateQuery().queryKey);
  try {
    await queryClient.fetchQuery(userAgentStateQuery());
    let made: Promise<boolean> | undefined;
    await act(async () => {
      made = hook.result.current("planner-session", true);
    });
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1));
    expect(pinnedAgentSessions(states())).toEqual(["planner-session"]);

    // The tester's conversation is read on another surface while the pin is out.
    act(() => {
      storeAgentState(
        cacheOf(queryClient),
        "tester-session",
        answeredByDispatch({ unread_replies: 0 })
      );
    });
    expect(pinnedAgentSessions(states())).toEqual(["planner-session"]);

    await act(async () => {
      refusal.reject(new ApiError(422, { code: "INVALID_STATE", error: "not a state" }));
      expect(await made).toBe(false);
    });
    await waitFor(() =>
      expect(queryClient.getQueryState(userAgentStateQuery().queryKey)?.status).toBe("error")
    );
    expect(pinnedAgentSessions(states())).toEqual([]);
    expect(states()?.["tester-session"]).toEqual({ unread_replies: 0 });
  } finally {
    hook.unmount();
    unsubscribe();
    cacheOf(queryClient).writes.clear();
    getMyAgentState.mockRestore();
  }
});
