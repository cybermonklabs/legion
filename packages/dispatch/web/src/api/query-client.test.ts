import { expect, jest, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { QueryClient, QueryObserver, skipToken } from "@tanstack/react-query";

import { SEND_DEADLINE_MS } from "../features/writes/keyed-write";
import { ApiError, createApiClient, isRetryableQueryError } from "./client";
import {
  DispatchQueryClient,
  hearLiveEvent,
  holdBackReads,
  LATE_EVENT_GRACE_MS,
  QUERY_RETRY_DELAY_MS,
  queryRetry,
  queryRetryDelay,
  replaceQueryData,
} from "./query-client";
import type { DispatchEvent } from "./types";
import { snoozeWritten } from "./write-events";

/**
 * A server for one read whose every request is answered by the test: `answer()` lands the oldest
 * request still out with the value the server held when that request reached it.
 */
function heldServer(initial: string) {
  let value = initial;
  let inFlight = 0;
  let mostInFlight = 0;
  let requests = 0;
  const out: (() => void)[] = [];
  return {
    answer: () => out.shift()?.(),
    mostInFlight: () => mostInFlight,
    queryFn: () => {
      requests += 1;
      inFlight += 1;
      mostInFlight = Math.max(mostInFlight, inFlight);
      const read = value;
      const { promise, resolve } = Promise.withResolvers<string>();
      out.push(() => {
        inFlight -= 1;
        resolve(read);
      });
      return promise;
    },
    requests: () => requests,
    write: (next: string) => {
      value = next;
    },
  };
}

async function settle(): Promise<void> {
  for (let index = 0; index < 20; index += 1) {
    await Promise.resolve();
  }
}

test("a mutation's invalidation during a fetch ends on the answer after the write, with never two requests in flight", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["issues", "pinned"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    expect(server.requests()).toBe(1);

    // The write lands while the read is out, and the mutation invalidates the read - twice, as
    // two mutations settling together would.
    server.write("after");
    let invalidated = false;
    void queryClient.invalidateQueries({ queryKey: ["issues"] }).then(() => {
      invalidated = true;
    });
    void queryClient.invalidateQueries({ queryKey: ["issues", "pinned"] });
    await settle();
    // Nothing cancelled the read already out, and nothing went out beside it.
    expect(server.requests()).toBe(1);

    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("before");
    // One more request, sent once the first settled, which reads the write.
    expect(server.requests()).toBe(2);
    expect(invalidated).toBe(false);

    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("after");
    // The mutation's own wait ends on the answer composed after its write.
    expect(invalidated).toBe(true);
    expect(server.requests()).toBe(2);
    expect(server.mostInFlight()).toBe(1);
  } finally {
    unsubscribe();
  }
});

// A write's answer put into a query over its reads: the read already out was asked before the
// write, and its answer must not put back what the write replaced, nor may the refetch an
// invalidation left waiting behind it. The write is there in the step it was made, for a reader
// of that same step.
test("a value put over a query's reads is there at once and stays over a read asked before it", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["user-agent-state"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    expect(server.requests()).toBe(1);
    // An invalidation while that read is out leaves a refetch waiting behind it.
    void queryClient.invalidateQueries({ queryKey: ["user-agent-state"] });
    await settle();
    expect(server.requests()).toBe(1);

    server.write("after");
    replaceQueryData<string>(queryClient, ["user-agent-state"], "after");
    expect(queryClient.getQueryData<string>(["user-agent-state"])).toBe("after");

    // The read asked before the write answers what the server held then.
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-agent-state"])).toBe("after");
    // The waiting refetch went out once that read had settled, after the write.
    expect(server.requests()).toBe(2);
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-agent-state"])).toBe("after");
  } finally {
    unsubscribe();
  }
});

// A read the write cancels was asked for a reason - here another issue's change, which the
// server holds by the time of the write. Its answer would have brought that news beside the old
// value of the written entry; once the write is in, the query is read again, and the read asked
// then answers both.
test("a read a value is put over is asked again once the value is in, and brings what it was asked for", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["user-state"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    expect(server.requests()).toBe(1);

    server.write("the write and the other issue's news");
    replaceQueryData<string>(queryClient, ["user-state"], "the write");
    expect(queryClient.getQueryData<string>(["user-state"])).toBe("the write");
    await settle();
    expect(server.requests()).toBe(2);

    // The cancelled read lands with what the server held before the write, and is dropped.
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-state"])).toBe("the write");
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-state"])).toBe(
      "the write and the other issue's news"
    );
  } finally {
    unsubscribe();
  }
});

// An optimistic write stands in its query until its answer is in. A read asked after the write
// but answered before the server took it would put back the state the write replaced, so the read
// waits: it asks once the hold ends, and reads what the write left.
test("a read asked while a write holds its query asks only once the hold ends", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["issues", "pinned"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("before");

    // The hold names the query by its exact key.
    const release = holdBackReads(queryClient, { keys: [["issues", "pinned"]] });
    queryClient.setQueryData(["issues", "pinned"], "optimistic");
    void queryClient.invalidateQueries({ queryKey: ["issues", "pinned"] });
    await settle();
    expect(server.requests()).toBe(1);
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("optimistic");

    server.write("after");
    release();
    // Ended once; a second call ends nothing more.
    release();
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["issues", "pinned"])).toBe("after");
  } finally {
    unsubscribe();
  }
});

// A hold is on the queries it names alone, and on each until its own end: a read of another query
// goes at once, and one of a query two writes hold asks once both have ended.
test("a hold keeps back only the reads it names, and each until the last hold on it ends", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const inbox = heldServer("rows");
  const state = heldServer("pins");
  const unsubscribe = [
    new QueryObserver(queryClient, { queryFn: inbox.queryFn, queryKey: ["inbox"] }).subscribe(
      () => {}
    ),
    new QueryObserver(queryClient, { queryFn: state.queryFn, queryKey: ["user-state"] }).subscribe(
      () => {}
    ),
  ];
  try {
    await settle();
    inbox.answer();
    state.answer();
    await settle();

    const first = holdBackReads(queryClient, { keys: [["user-state"]] });
    const second = holdBackReads(queryClient, { keys: [["user-state"], ["inbox", "elsewhere"]] });
    void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    await settle();
    expect(inbox.requests()).toBe(2);
    expect(state.requests()).toBe(1);

    first();
    await settle();
    expect(state.requests()).toBe(1);
    second();
    await settle();
    expect(state.requests()).toBe(2);
  } finally {
    for (const stop of unsubscribe) stop();
  }
});

// A hold names its queries by their exact keys: a read under a held key - an issue's coverage
// under the issue a components save writes - is none of the write's, and goes at once.
test("a hold keeps back the queries it names exactly, and no query under their keys", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const issue = heldServer("CORE-1");
  const coverage = heldServer("coverage");
  const unsubscribe = [
    new QueryObserver(queryClient, { queryFn: issue.queryFn, queryKey: ["issue", "CORE-1"] }),
    new QueryObserver(queryClient, {
      queryFn: coverage.queryFn,
      queryKey: ["issue", "CORE-1", "coverage"],
    }),
  ].map((observer) => observer.subscribe(() => {}));
  try {
    await settle();
    issue.answer();
    coverage.answer();
    await settle();

    const release = holdBackReads(queryClient, { keys: [["issue", "CORE-1"]] });
    void queryClient.invalidateQueries({ queryKey: ["issue", "CORE-1"] });
    await settle();
    expect(issue.requests()).toBe(1);
    expect(coverage.requests()).toBe(2);
    release();
    await settle();
    expect(issue.requests()).toBe(2);
  } finally {
    for (const stop of unsubscribe) stop();
  }
});

// A write that goes late may not reach Dispatch for a long while, and its hold would keep back,
// for as long, every change anybody else makes to its queries. So no hold outlasts the time after
// which its write is late: the read it held then asks.
test("a hold ends once its write is late, and the read it held asks", async () => {
  jest.useFakeTimers();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["inbox"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    const release = holdBackReads(queryClient, { keys: [["inbox"]] });
    void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    await settle();
    jest.advanceTimersByTime(SEND_DEADLINE_MS - 1);
    await settle();
    expect(server.requests()).toBe(1);
    server.write("somebody's new ask");
    jest.advanceTimersByTime(1);
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["inbox"])).toBe("somebody's new ask");
    // The write's own end, after, ends nothing more.
    release();
    await settle();
    expect(server.requests()).toBe(2);
  } finally {
    unsubscribe();
    jest.useRealTimers();
  }
});

// Dispatch publishes a write's event once the write has committed, so a read asked after it
// answers the write: the hold ends on its write's own event, and the read it held asks then, with
// whatever anybody else changed meanwhile. Another event on the same query leaves the hold.
test("a hold ends on its write's own live event, and on no other", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["user-agent-state"],
  });
  const unsubscribe = observer.subscribe(() => {});
  const event = (sessionId: string) =>
    ({
      actor: { id: "alice", kind: "user" },
      created_at: "2026-11-19T00:00:00Z",
      id: 1,
      issue_key: null,
      notify: false,
      payload: { login: "alice", session_id: sessionId },
      seq: 1,
      type: "user_agent_state.updated",
    }) as DispatchEvent;
  try {
    await settle();
    server.answer();
    await settle();

    const release = holdBackReads(queryClient, {
      endsOn: (heard, viewer) =>
        heard.type === "user_agent_state.updated" &&
        heard.payload.session_id === "planner" &&
        heard.payload.login === viewer,
      keys: [["user-agent-state"]],
    });
    void queryClient.invalidateQueries({ queryKey: ["user-agent-state"] });
    await settle();
    hearLiveEvent(queryClient, event("reviewer"), "alice");
    hearLiveEvent(queryClient, event("planner"), "bob");
    await settle();
    expect(server.requests()).toBe(1);

    server.write("the pin and another session's reply");
    hearLiveEvent(queryClient, event("planner"), "alice");
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-agent-state"])).toBe(
      "the pin and another session's reply"
    );
    release();
  } finally {
    unsubscribe();
  }
});

/** The `user_state.updated` naming `snoozed` that a snooze of alice's publishes (`snoozeWritten`):
 *  it names no ask, so it matches every snooze of hers still out. */
function snoozeEvent(): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-11-20T00:00:00Z",
    id: 1,
    issue_key: null,
    notify: false,
    payload: { changed: ["snoozed"], login: "alice" },
    seq: 1,
    type: "user_state.updated",
  } as DispatchEvent;
}

// A bulk snooze of two asks goes past its deadline still out, and a snooze is pressed after it.
// Dispatch then answers the bulk snooze, one of whose asks moved nothing, so one of its two events
// never comes. It takes the snooze events that come within `LATE_EVENT_GRACE_MS` of its answer,
// and then waits for none: the event after that ends the later snooze's hold.
test("a hold past its deadline takes its write's events until LATE_EVENT_GRACE_MS after its answer, and none after", async () => {
  jest.useFakeTimers();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("rows");
  const observer = new QueryObserver(queryClient, { queryFn: server.queryFn, queryKey: ["inbox"] });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    const bulk = holdBackReads(queryClient, {
      endsOn: snoozeWritten,
      events: 2,
      keys: [["inbox"]],
    });
    jest.advanceTimersByTime(SEND_DEADLINE_MS);
    const later = holdBackReads(queryClient, { endsOn: snoozeWritten, keys: [["inbox"]] });
    bulk();
    void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    await settle();
    expect(server.requests()).toBe(1);

    // An event just within the grace is the bulk snooze's, and the read waits for the later's.
    jest.advanceTimersByTime(LATE_EVENT_GRACE_MS - 1);
    hearLiveEvent(queryClient, snoozeEvent(), "alice");
    await settle();
    expect(server.requests()).toBe(1);

    // Past it, the bulk snooze waits for no event: the next is the later snooze's, and the read
    // asks.
    jest.advanceTimersByTime(1);
    hearLiveEvent(queryClient, snoozeEvent(), "alice");
    await settle();
    expect(server.requests()).toBe(2);
    later();
  } finally {
    unsubscribe();
    jest.useRealTimers();
  }
});

// A snooze the page tries for as long as the reader leaves it may never be answered. Past its
// deadline its hold holds no read and still waits for its write's events, for no longer than
// `IDEMPOTENCY_KEY_TTL_MS` after it began: an event just within that is its own, and the one
// after ends the hold of a snooze pressed after it.
test("a hold whose write is never answered takes its write's events until IDEMPOTENCY_KEY_TTL_MS after it began, and none after", async () => {
  jest.useFakeTimers();
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("rows");
  const observer = new QueryObserver(queryClient, { queryFn: server.queryFn, queryKey: ["inbox"] });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    const unanswered = holdBackReads(queryClient, {
      endsOn: snoozeWritten,
      events: 2,
      keys: [["inbox"]],
    });
    jest.advanceTimersByTime(IDEMPOTENCY_KEY_TTL_MS - SEND_DEADLINE_MS / 2);
    const later = holdBackReads(queryClient, { endsOn: snoozeWritten, keys: [["inbox"]] });
    void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    await settle();
    expect(server.requests()).toBe(1);

    // An event just within the key's lifetime is the unanswered snooze's, and the read waits.
    jest.advanceTimersByTime(SEND_DEADLINE_MS / 2 - 1);
    hearLiveEvent(queryClient, snoozeEvent(), "alice");
    await settle();
    expect(server.requests()).toBe(1);

    // Past it, the unanswered snooze waits for no event: the next is the later snooze's, and the
    // read asks.
    jest.advanceTimersByTime(1);
    hearLiveEvent(queryClient, snoozeEvent(), "alice");
    await settle();
    expect(server.requests()).toBe(2);
    later();
    unanswered();
  } finally {
    unsubscribe();
    jest.useRealTimers();
  }
});

// The write's own answer lands while the read it holds is still waiting: `replaceQueryData`
// cancels that read, which has asked nothing, and the answer stays. What the read was asked for -
// news of another change, here the other issue's - is not lost: the query is read again once the
// hold ends, after the answer, and the read asked then has both.
test("a held read the write's answer gives up is asked again once the hold ends", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["user-state"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    const release = holdBackReads(queryClient, { keys: [["user-state"]] });
    void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    await settle();
    server.write("answer and the other issue's news");
    replaceQueryData<string>(queryClient, ["user-state"], "answer");
    await settle();
    expect(server.requests()).toBe(1);
    expect(queryClient.getQueryData<string>(["user-state"])).toBe("answer");

    release();
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await settle();
    expect(queryClient.getQueryData<string>(["user-state"])).toBe(
      "answer and the other issue's news"
    );
  } finally {
    unsubscribe();
  }
});

// Holding reads back is the Dispatch client's: a plain TanStack client, as most unit tests build,
// lets a held read go.
test("a plain query client lets a held read ask at once", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["user-state"],
  });
  const release = holdBackReads(queryClient, { keys: [["user-state"]] });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    expect(server.requests()).toBe(1);
  } finally {
    unsubscribe();
    release();
  }
});

test("an invalidation of an idle read fetches it at once", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const server = heldServer("before");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["projects"],
  });
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    server.answer();
    await settle();

    server.write("after");
    const invalidation = queryClient.invalidateQueries({ queryKey: ["projects"] });
    await settle();
    expect(server.requests()).toBe(2);
    server.answer();
    await invalidation;
    expect(queryClient.getQueryData<string>(["projects"])).toBe("after");
  } finally {
    unsubscribe();
  }
});

test("each attempt of a read tells the attempt listeners its query's hash once, and the read answers what its function returned", async () => {
  const queryClient = new DispatchQueryClient({
    defaultOptions: { queries: { retry: 1, retryDelay: QUERY_RETRY_DELAY_MS } },
  });
  const told: string[] = [];
  const stop = queryClient.onAttempt((queryHash) => told.push(queryHash));
  const answer = { projects: ["CORE"] };
  const asked: unknown[] = [];
  // The first attempt is refused, and the retry answers.
  const observer = new QueryObserver(queryClient, {
    queryFn: async ({ queryKey }) => {
      asked.push(queryKey);
      if (asked.length === 1) throw new ApiError(503, { error: "service unavailable" });
      return answer;
    },
    queryKey: ["projects"],
  });
  jest.useFakeTimers();
  const unsubscribe = observer.subscribe(() => {});
  try {
    await settle();
    const hash = '["projects"]';
    expect(queryClient.getQueryCache().find({ queryKey: ["projects"] })?.queryHash).toBe(hash);
    expect(told).toEqual([hash]);
    expect(observer.getCurrentResult().failureCount).toBe(1);

    jest.advanceTimersByTime(QUERY_RETRY_DELAY_MS);
    await settle();
    expect(asked).toEqual([["projects"], ["projects"]]);
    expect(told).toEqual([hash, hash]);
    expect(observer.getCurrentResult().data).toBe(answer);

    // A query that does not run, and one with no function of its own, are left as they are.
    const skipped = queryClient.defaultQueryOptions({ queryFn: skipToken, queryKey: ["skipped"] });
    expect(skipped.queryFn).toBe(skipToken);
    expect(skipped.enabled).toBe(false);
    expect(queryClient.defaultQueryOptions({ queryKey: ["bare"] }).queryFn).toBeUndefined();
  } finally {
    jest.useRealTimers();
    unsubscribe();
    stop();
    queryClient.clear();
  }
});

test("an attempt listener that throws fails no read: the read answers, and the other listeners are told", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  const told: string[] = [];
  const thrown: unknown[] = [];
  const failure = new Error("listener");
  const stopFirst = queryClient.onAttempt(() => {
    throw failure;
  });
  const stopSecond = queryClient.onAttempt((queryHash) => told.push(queryHash));
  const microtask = spyOn(globalThis, "queueMicrotask").mockImplementation((callback) => {
    try {
      callback();
    } catch (error) {
      thrown.push(error);
    }
  });
  try {
    const answer = await queryClient.fetchQuery({
      queryFn: async () => "answered",
      queryKey: ["projects"],
    });
    expect(answer).toBe("answered");
    expect(told).toEqual(['["projects"]']);
    expect(thrown).toEqual([failure]);
  } finally {
    microtask.mockRestore();
    stopFirst();
    stopSecond();
    queryClient.clear();
  }
});

/**
 * When each request of one read went out, in ms after the first, through main.tsx's retry options
 * and the real API client, with every response a 503 refusal carrying `headers`. The clock is
 * fake and moves in 10 ms steps until `ms` have passed.
 */
async function refusedReadStarts(headers: Record<string, string>, ms: number): Promise<number[]> {
  const starts: number[] = [];
  const client = createApiClient(async () => {
    starts.push(Date.now());
    return Response.json({ code: "OVERLOADED", error: "busy" }, { headers, status: 503 });
  });
  const queryClient = new DispatchQueryClient({
    defaultOptions: {
      queries: {
        retry: (failureCount, error) => failureCount < 2 && isRetryableQueryError(error),
        retryDelay: queryRetryDelay,
      },
    },
  });
  jest.useFakeTimers();
  try {
    void queryClient
      .fetchQuery({ queryFn: () => client.whoAmI(), queryKey: ["whoami"] })
      .catch(() => undefined);
    for (let elapsed = 0; elapsed < ms; elapsed += 10) {
      await settle();
      jest.advanceTimersByTime(10);
    }
    await settle();
  } finally {
    jest.useRealTimers();
    queryClient.clear();
  }
  return starts.map((start) => start - (starts[0] ?? start));
}

test("a read refused with Retry-After: 2 is retried no sooner than 2 s later and before 4 s", async () => {
  for (const draw of [0, 0.9999]) {
    const random = spyOn(Math, "random").mockReturnValue(draw);
    try {
      const starts = await refusedReadStarts({ "Retry-After": "2" }, 4_100);
      expect(starts[1]).toBeGreaterThanOrEqual(2_000);
      expect(starts[1]).toBeLessThan(4_000);
    } finally {
      random.mockRestore();
    }
  }
});

test("a read refused without Retry-After is retried at 500 ms", async () => {
  const starts = await refusedReadStarts({}, 700);
  expect(starts[1]).toBeGreaterThanOrEqual(500);
  expect(starts[1]).toBeLessThan(510);
});

test("a read refused with a Retry-After longer than a minute still retries within a minute", () => {
  // A proxy in front of Dispatch may answer a day, or more than setTimeout can hold, with a 503 that
  // carries no Dispatch code; a page read that waits that long shows its spinner with no error and
  // no Retry for days.
  for (const seconds of [86_400, 4_294_968]) {
    const refusal = new ApiError(503, { error: "service unavailable" }, seconds * 1_000);
    for (const draw of [0, 0.9999]) {
      const wait = queryRetryDelay(0, refusal, () => draw);
      expect(wait).toBeGreaterThan(0);
      expect(wait).toBeLessThanOrEqual(60_000);
    }
  }
});

test("the sign-in check's EVENT_HEAD_UNAVAILABLE is asked again without limit, every other 503 twice", () => {
  // EVENT_HEAD_UNAVAILABLE ends on its own once the event write holding it does; a failed read of
  // Dispatch's database (EVENT_HEAD_READ_FAILED) may not, so it ends at the page's error and Retry.
  const unproven = new ApiError(503, { code: "EVENT_HEAD_UNAVAILABLE", error: "ask again" });
  const readFailed = new ApiError(503, { code: "EVENT_HEAD_READ_FAILED", error: "unreadable" });
  for (const failures of [0, 2, 3, 50]) {
    expect(queryRetry(failures, unproven)).toBe(true);
  }
  for (const refusal of [readFailed, new ApiError(503, { error: "service unavailable" })]) {
    expect(queryRetry(0, refusal)).toBe(true);
    expect(queryRetry(1, refusal)).toBe(true);
    expect(queryRetry(2, refusal)).toBe(false);
  }
  expect(queryRetryDelay(0, unproven)).toBe(500);
  expect(queryRetryDelay(4, unproven)).toBe(5_000);
  expect(queryRetryDelay(4, readFailed)).toBe(500);
});
