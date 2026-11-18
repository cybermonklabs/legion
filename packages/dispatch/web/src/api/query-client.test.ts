import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryObserver, skipToken } from "@tanstack/react-query";

import { ApiError, createApiClient, isRetryableQueryError } from "./client";
import {
  askSignInAgain,
  DispatchQueryClient,
  QUERY_RETRY_DELAY_MS,
  queryRetry,
  queryRetryDelay,
} from "./query-client";

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

// Two asks of the sign-in check while the browser is offline, as two writes answered 401 at once
// make them: the second cancels the check the first started, which ends the first's invalidation
// with no answer, and starts its own, which the network holds paused. A paused check has not
// answered, so neither ask is over until the network returns and that check answers.
test("an ask of the sign-in check waits through a check paused offline for the newest answer", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  // Mounted, as the page's provider mounts it, so its queries hear the network come back.
  queryClient.mount();
  const server = heldServer("alice");
  const observer = new QueryObserver(queryClient, {
    queryFn: server.queryFn,
    queryKey: ["whoami"],
  });
  const unsubscribe = observer.subscribe(() => {});
  const answered: string[] = [];
  try {
    await settle();
    server.answer();
    await settle();

    onlineManager.setOnline(false);
    const first = askSignInAgain(queryClient).then(() => answered.push("first"));
    await settle();
    const second = askSignInAgain(queryClient).then(() => answered.push("second"));
    await settle();
    expect({
      answered,
      check: queryClient.getQueryState(["whoami"])?.fetchStatus,
      requests: server.requests(),
    }).toEqual({ answered: [], check: "paused", requests: 1 });

    server.write("bob");
    onlineManager.setOnline(true);
    await settle();
    expect({ answered, requests: server.requests() }).toEqual({ answered: [], requests: 2 });
    server.answer();
    await Promise.all([first, second]);
    expect({
      answered: answered.length,
      login: queryClient.getQueryData<string>(["whoami"]),
    }).toEqual({ answered: 2, login: "bob" });
  } finally {
    onlineManager.setOnline(true);
    unsubscribe();
    queryClient.unmount();
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
