import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, QueryObserver } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { answerAgentStates } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import { DispatchQueryClient } from "../../api/query-client";
import type {
  Message,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
} from "../../api/types";
import { writesOf } from "../writes/write-store";
import { storeAgentState, totalUnreadReplies, useMarkShownRepliesRead } from "./unread";

// An API older than the unread count answers a session's state with its Clear alone. The badge
// then counts that session as nothing unread rather than reading "New replies NaN".
test("the unread total counts a state with no unread count as none", () => {
  // The route table declares `unread_replies` on every state, because the server this build
  // talks to sends one; a server older than the count answers a session's Clear alone, which a
  // browser of this build still meets while two builds are up, so the key is dropped from the
  // value rather than set to anything.
  const old: UserAgentState = { cleared_before: "2026-09-27T20:00:00Z", unread_replies: 0 };
  Reflect.deleteProperty(old, "unread_replies");
  const states: UserAgentStates = {
    old,
    s1: { unread_replies: 2 },
  };
  expect(totalUnreadReplies(states)).toBe(2);
});

// A read of the viewer's agent state that was asked before a mark answered counts the replies as
// they stood before the mark. When that read answers after the mark, its answer must not put back
// the count the mark cleared: the header's and the navigation's badge would read "New reply 1"
// again, after the reader opened the conversation, until some later read answered.
test("a read asked before a mark answered does not put back the unread count the mark cleared", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { promise: older, resolve: answerOlder } = Promise.withResolvers<UserAgentStates>();
  const read = queryClient
    .fetchQuery({ queryFn: () => older, queryKey: userAgentStateQuery().queryKey })
    .catch(() => undefined);

  storeAgentState(queryClient, "planner-session", {
    read_through: "2026-09-14T01:05:00Z",
    unread_replies: 0,
  });
  answerOlder({ "planner-session": { unread_replies: 1 } });
  await read;

  expect(
    queryClient.getQueryData<UserAgentStates>(userAgentStateQuery().queryKey)?.["planner-session"]
      ?.unread_replies
  ).toBe(0);
});

/** Each session's unread count as a test writes it: `{ A: 1 }` is session A with one reply unread. */
type Counts = Readonly<Record<string, number>>;

function statesOf(counts: Counts): UserAgentStates {
  return Object.fromEntries(
    Object.entries(counts).map(([session, unread]) => [session, { unread_replies: unread }])
  );
}

/**
 * The agent-state read under Dispatch's own client, with something on the page showing it: every
 * request is answered by the test, with what the server held when the request reached it.
 * `answerAll` answers each request out, and each one the answers set going, until none is out.
 */
function agentStatesServed(initial: Counts) {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  let held = initial;
  let requests = 0;
  const out: (() => void)[] = [];
  const observer = new QueryObserver(queryClient, {
    queryFn: () => {
      requests += 1;
      const answer = statesOf(held);
      const { promise, resolve } = Promise.withResolvers<UserAgentStates>();
      out.push(() => resolve(answer));
      return promise;
    },
    queryKey: userAgentStateQuery().queryKey,
  });
  const unsubscribe = observer.subscribe(() => {});
  /** One turn of the real loop: the late-reads lane holds each answer one such turn. */
  const turn = () => new Promise<void>((resolve) => setImmediate(resolve));
  return {
    answerAll: async () => {
      for (let step = 0; step < 50; step += 1) {
        await turn();
        const query = queryClient
          .getQueryCache()
          .find({ queryKey: userAgentStateQuery().queryKey });
        if (out.length === 0 && query?.state.fetchStatus === "idle") return;
        out.shift()?.();
      }
    },
    counts: (): Counts =>
      Object.fromEntries(
        Object.entries(
          queryClient.getQueryData<UserAgentStates>(userAgentStateQuery().queryKey) ?? {}
        ).map(([session, state]) => [session, state.unread_replies])
      ),
    queryClient,
    requests: () => requests,
    serverHolds: (counts: Counts) => {
      held = counts;
    },
    stop: unsubscribe,
  };
}

// The read a mark's answer cancels was asked for news of its own: here a second session's new
// reply, whose event asked the query again. Once the answer is in the query is asked again, and
// that read brings B's reply beside the mark, as the cancelled read would have. Left out, B's
// count stays 0 on every badge until the next event aimed at the query.
test("a mark's answer that cancels a read asks again, so another session's new reply is counted", async () => {
  const served = agentStatesServed({ A: 1, B: 0 });
  try {
    await served.answerAll();
    expect(served.counts()).toEqual({ A: 1, B: 0 });

    // B replies: the server counts it, and the reply's event asks the query again.
    served.serverHolds({ A: 1, B: 1 });
    void served.queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey });
    expect(served.requests()).toBe(2);
    // The reader opens A's conversation while that read is out: the mark reaches the server, and
    // its answer is put into the query.
    served.serverHolds({ A: 0, B: 1 });
    storeAgentState(served.queryClient, "A", { unread_replies: 0 });
    expect(served.counts()).toEqual({ A: 0, B: 0 });

    await served.answerAll();
    expect(served.counts()).toEqual({ A: 0, B: 1 });
    expect(served.requests()).toBe(3);
  } finally {
    served.stop();
  }
});

// A mark answered while the page's first read is still out: the answer names one session, and the
// first read, which the mark cancels, was the only read asked for every other one. Asked again
// once the answer is in, the query brings them all.
test("a mark's answer that lands during the first load leaves every other session's count to arrive", async () => {
  const served = agentStatesServed({ A: 1, B: 1, C: 2 });
  try {
    expect(served.requests()).toBe(1);
    served.serverHolds({ A: 0, B: 1, C: 2 });
    storeAgentState(served.queryClient, "A", { unread_replies: 0 });
    expect(served.counts()).toEqual({ A: 0 });

    await served.answerAll();
    expect(served.counts()).toEqual({ A: 0, B: 1, C: 2 });
    expect(served.requests()).toBe(2);
  } finally {
    served.stop();
  }
});

const SESSION = "planner-session";

function reply(id: string): Message {
  return {
    author: { id: SESSION, kind: "session" },
    body: "Done.",
    broadcast_id: null,
    created_at: "2026-10-03T12:00:00Z",
    deliveries: [],
    id,
    in_reply_to: "message-1",
    issue_key: null,
    reply_block: null,
    target: null,
  };
}

/** Mounts the broadcast page's read mark for `SESSION`, showing reply `a1`, against a server that
 *  counts 2 of the session's replies unread and answers every PUT with `put`, and runs `row` on
 *  it. Every PUT body is recorded in `sent`, in the order it was sent. */
async function withShownReplies(
  put: (body: UserAgentStateInput) => Promise<UserAgentState>,
  row: (page: ShownReplies) => Promise<void>
): Promise<void> {
  let states: UserAgentStates = { [SESSION]: { unread_replies: 2 } };
  const sent: UserAgentStateInput[] = [];
  const getMyAgentState = spyOn(api, "getMyAgentState").mockImplementation(async () => states);
  const putAgentState = answerAgentStates().mockImplementation(async (session, body, bounds) => {
    // As `api.putAgentState` does, the request is told to `sent` before anything goes out: a
    // mark waiting its turn is planned there, and nothing past it runs (`Writes`).
    bounds.sent?.({ body, method: "PUT", path: `/api/v1/me/agents/${session}/state` });
    sent.push(body);
    return put(body);
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(
    ({ replies }: { replies: readonly Message[] }) => useMarkShownRepliesRead(SESSION, replies),
    {
      initialProps: { replies: [reply("a1")] },
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  /** Waits until the store holds no mark still out. */
  const settled = () => waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
  let failure: { error: unknown } | undefined;
  try {
    await row({
      hook,
      queryClient,
      sent,
      settled,
      async countBecomes(unread: number) {
        states = { [SESSION]: { unread_replies: unread } };
        await act(() =>
          queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey })
        );
        await settled();
      },
    });
  } catch (error) {
    failure = { error };
  }
  // No mark may still be out once the spies are gone, or its PUT would reach the real API. A row
  // that failed may have spent its deadline, and then this wait gives up at once: the row's own
  // failure is the one it reports, not this wait's.
  try {
    await settled();
  } catch (error) {
    if (failure === undefined) throw error;
  } finally {
    hook.unmount();
    putAgentState.mockRestore();
    getMyAgentState.mockRestore();
  }
  if (failure !== undefined) throw failure.error;
}

interface ShownReplies {
  readonly hook: { rerender(props: { replies: Message[] }): void };
  readonly queryClient: QueryClient;
  readonly sent: UserAgentStateInput[];
  /** Waits until the store holds no mark still out. */
  settled(): Promise<void>;
  /** The session's unread count moves, as the refetch a `user_agent_state.updated` event causes
   *  brings it in, and every PUT the new count calls for is answered. */
  countBecomes(unread: number): Promise<void>;
}

const refusal = new ApiError(400, { code: "STATE_INPUT", error: "refused" });

// A reply that arrives while the page's mark is still out changes what the page shows, so the page
// makes a new mark that covers both; the marks of one session go out in order, so the new one
// waits behind the old. When the old one is then refused, the new mark still stands: forgetting it
// would send it once more at the next change in the count, a PUT the server answers by changing
// nothing.
test("a superseded mark that is refused sends nothing more", async () => {
  const held = Promise.withResolvers<void>();
  await withShownReplies(
    async (body) => {
      if (body.read_replies?.length === 1) {
        await held.promise;
        throw refusal;
      }
      // The session has another unread reply this page does not show, so the count stays up.
      return { unread_replies: 1 };
    },
    async (page) => {
      try {
        await waitFor(() => expect(page.sent).toEqual([{ read_replies: ["a1"] }]));
        page.hook.rerender({ replies: [reply("a1"), reply("a2")] });
        await waitFor(() => expect(writesOf(page.queryClient).rows()).toHaveLength(2));
        expect(page.sent).toHaveLength(1);
        held.resolve();
        await page.settled();
        expect(page.sent).toEqual([{ read_replies: ["a1"] }, { read_replies: ["a1", "a2"] }]);
        await page.countBecomes(3);
        expect(page.sent).toHaveLength(2);
      } finally {
        // A held PUT is let go on a failure too, so the page's marks can settle before it goes.
        held.resolve();
      }
    }
  );
});

// The mark the page still stands by is another matter: when it is refused its key is forgotten, so
// the next change in the count sends it again rather than leaving the badge up until the session
// replies once more.
test("the current mark that is refused is sent again when the count changes", async () => {
  let refusals = 1;
  await withShownReplies(
    async () => {
      refusals -= 1;
      if (refusals >= 0) throw refusal;
      return { unread_replies: 0 };
    },
    async (page) => {
      await waitFor(() => expect(page.sent).toHaveLength(1));
      await page.settled();
      await page.countBecomes(3);
      expect(page.sent).toEqual(Array(2).fill({ read_replies: ["a1"] }));
    }
  );
});
