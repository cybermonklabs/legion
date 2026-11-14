import { expect, jest, spyOn, test } from "bun:test";
import { type InfiniteData, InfiniteQueryObserver, QueryObserver } from "@tanstack/react-query";

import { ApiError, createApiClient, isRetryableQueryError } from "../client";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./query-client";

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

/** A page of the Dismissed list as this row reads it: what the server held when the page was
 *  read, and the cursor of the page after it. */
interface HeldPage {
  readonly next: string | undefined;
  readonly value: string;
}

// Round eight of dispatch://INFRA-299's pull request: an invalidation made while a refetch of an
// infinite read is out waits for that refetch; a Show more then cancels it (`cancelRefetch`) and
// reads one page after the pages the cache holds. That page read is no whole read, so it does
// not answer the invalidation: once it settles the client reads the whole query, and the first
// page shows what the server held after the write.
test("an invalidation during a refetch that a Show more cancels still ends on a whole read", async () => {
  const queryClient = new DispatchQueryClient({ defaultOptions: { queries: { retry: false } } });
  let value = "before";
  const asked: { readonly page: string; readonly answer: () => void }[] = [];
  const observer = new InfiniteQueryObserver(queryClient, {
    getNextPageParam: (last: HeldPage) => last.next,
    initialPageParam: "first",
    queryFn: ({ pageParam }: { pageParam: string }) => {
      const read = value;
      const { promise, resolve } = Promise.withResolvers<HeldPage>();
      asked.push({
        answer: () => resolve({ next: pageParam === "first" ? "second" : undefined, value: read }),
        page: pageParam,
      });
      return promise;
    },
    queryKey: ["me", "replies", "inbox", "dismissed"],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const unsubscribe = observer.subscribe(() => {});
  const firstPage = () =>
    queryClient.getQueryData<InfiniteData<HeldPage>>(["me", "replies", "inbox", "dismissed"])
      ?.pages[0]?.value;
  try {
    await settle();
    asked[0]?.answer();
    await settle();
    expect(firstPage()).toBe("before");

    // A refetch is out; the write lands and the mutation invalidates the read.
    void observer.refetch();
    await settle();
    value = "after";
    const invalidated = queryClient.invalidateQueries({ queryKey: ["me", "replies"] });
    await settle();
    expect(asked.map(({ page }) => page)).toEqual(["first", "first"]);

    // Show more cancels the refetch and reads the page after the pages held.
    const more = observer.fetchNextPage();
    await settle();
    expect(asked.map(({ page }) => page)).toEqual(["first", "first", "second"]);
    asked[2]?.answer();
    await more;
    await settle();
    expect(firstPage()).toBe("before");

    // The whole read the invalidation asked for follows, and reads the write.
    expect(asked.map(({ page }) => page)).toEqual(["first", "first", "second", "first"]);
    asked[3]?.answer();
    await settle();
    asked[4]?.answer();
    await invalidated;
    expect(firstPage()).toBe("after");
  } finally {
    unsubscribe();
  }
});

/**
 * When each request of one read went out, in ms after the first, through api/cache/provider.tsx's retry options
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
