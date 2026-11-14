import { expect, jest, test } from "bun:test";

import { onFakeClock, settle } from "../../__tests__/fake-clock";
import {
  type InfiniteData,
  InfiniteQueryObserver,
  QueryClient,
  QueryObserver,
} from "./test-client";

/**
 * The behaviour of TanStack Query the cache layer is built on, each row one behaviour, asserted
 * against a real `QueryClient` and its observers, never a mock. A release of
 * `@tanstack/query-core` that moves a behaviour fails the row whose title names it.
 *
 * Above each row is the store row in this directory that fails when the layer's handling of that
 * behaviour is replaced by the plain library call: the row here says what TanStack does, the
 * store row that the layer does not leave it at that.
 */

/** A client as these rows read one: nothing retried, and nothing stale by time alone. */
function clientOf(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

/** A read whose every fetch waits for the row to answer it: `fetches[n]` is the n-th fetch
 *  begun, with the signal TanStack handed it. */
function heldRead<T>() {
  const fetches: { readonly resolve: (value: T) => void; readonly signal: AbortSignal }[] = [];
  return {
    fetches,
    queryFn: ({ signal }: { signal: AbortSignal }) => {
      const { promise, resolve } = Promise.withResolvers<T>();
      fetches.push({ resolve, signal });
      return promise;
    },
  };
}

/** The fetch starts and answers the query cache raises from now on, as the store's fetch
 *  counter reads them (`Writes.#countFetches`): each `updated` event whose action is `fetch`,
 *  and each whose action is `success`, marked when it is `manual`. */
function actionsOf(client: QueryClient): string[] {
  const seen: string[] = [];
  client.getQueryCache().subscribe((event) => {
    if (event.type !== "updated") return;
    const { action } = event;
    if (action.type === "fetch") seen.push("fetch");
    else if (action.type === "success") seen.push(action.manual === true ? "manual" : "success");
  });
  return seen;
}

interface Issue {
  readonly key: string;
  readonly updated_at: string;
}

// Store rows: shown-query.test.tsx, "a reader moved between two issues shows the issue it is
// on: ..." (the issue read with a live write on the issue it left, and a caller's select). With
// the layer's composition handed to TanStack as the query's `select` again, as at 0a5d74e1,
// `issueQuery`'s rule is handed the issue the reader left and keeps it, and those rows fail.
test("a select with a function structuralSharing: after a key change TanStack hands the rule the observer's result for the key it left", () => {
  const client = clientOf();
  const newer: Issue = { key: "CORE-1", updated_at: "2026-11-10T00:00:00Z" };
  const older: Issue = { key: "CORE-2", updated_at: "2026-01-01T00:00:00Z" };
  client.setQueryData(["issue", newer.key], newer);
  client.setQueryData(["issue", older.key], older);
  const handed: unknown[] = [];
  const select = (issue: Issue) => issue;
  // `issueQuery`'s rule, in short: an answer older than the issue held keeps the one held.
  const keepNewer = (held: unknown, next: unknown) => {
    handed.push(held);
    return held !== undefined && (held as Issue).updated_at > (next as Issue).updated_at
      ? held
      : next;
  };
  const options = (key: string) => ({
    queryKey: ["issue", key],
    select,
    structuralSharing: keepNewer,
  });
  const observer = new QueryObserver<Issue>(client, options(newer.key));
  const stop = observer.subscribe(() => {});
  try {
    expect(observer.getCurrentResult().data).toBe(newer);
    handed.length = 0;
    observer.setOptions(options(older.key));
    expect(handed).toEqual([newer]);
    // The rule kept CORE-1 as the answer of CORE-2's read.
    expect(observer.getCurrentResult().data).toBe(newer);
  } finally {
    stop();
  }
});

/** A page of an offset-paged read, as the Answers page pages its rows. */
interface OffsetPage {
  readonly offset: number;
  readonly rows: readonly string[];
  readonly total: number;
}

// Store row: write-store.test.ts, "a live change taking a row out of an offset-paged list leaves
// Show more reading the next page". With the store writing its composition into the cache by
// `setQueryData`, the next page's offset is read off a page one row short, and that row fails.
test("fetchNextPage reads the next page's param off the pages the cache holds", async () => {
  const client = clientOf();
  const all = ["a", "b", "c"];
  const asked: number[] = [];
  const read = {
    getNextPageParam: (last: OffsetPage) =>
      last.offset + last.rows.length < last.total ? last.offset + last.rows.length : undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }: { pageParam: number }) => {
      asked.push(pageParam);
      return Promise.resolve<OffsetPage>({
        offset: pageParam,
        rows: all.slice(pageParam, pageParam + 2),
        total: all.length,
      });
    },
    queryKey: ["me", "answers"],
  };
  await client.prefetchInfiniteQuery(read);
  // The first page as a write into the cache leaves it: one row short of what Dispatch sent.
  client.setQueryData<InfiniteData<OffsetPage>>(
    read.queryKey,
    (pages) =>
      pages && {
        ...pages,
        pages: pages.pages.map((page) => ({ ...page, rows: page.rows.filter((id) => id !== "a") })),
      }
  );
  const observer = new InfiniteQueryObserver(client, read);
  const stop = observer.subscribe(() => {});
  try {
    expect(observer.getCurrentResult().hasNextPage).toBe(true);
    await observer.fetchNextPage();
    // Offset 1, the cached page's offset and row count, where Dispatch's page ends at 2.
    expect(asked).toEqual([0, 1]);
  } finally {
    stop();
  }
});

// Store row: write-store.test.ts, "a read sent before the save answers after it lands: the pick
// stays over it until a read begun after the landing answers". With the store letting a landed
// change go at its landing, the late answer shows the row as it stood, and that row fails.
test("a fetch begun before a setQueryData answers after it and replaces the data it set", async () => {
  const client = clientOf();
  const key = ["project-issues", "CORE"];
  const read = heldRead<string>();
  const fetched = client.fetchQuery({ queryFn: read.queryFn, queryKey: key });
  await settle();
  expect(read.fetches).toHaveLength(1);
  client.setQueryData(key, "as the write left it");
  read.fetches[0]?.resolve("as it stood");
  await fetched;
  expect(client.getQueryData<string>(key)).toBe("as it stood");
});

// Store row: query-client.test.ts, "a mutation's invalidation during a fetch ends on the answer
// after the write, with never two requests in flight". `DispatchQueryClient.refetchQueries`
// fetches with `cancelRefetch: false`, which joins the fetch out and begins none (this row), and
// fetches once more when that fetch settles. With TanStack's own `refetchQueries` in its place, an
// invalidation mid-fetch joins the fetch out of a read that holds no data yet, as that row's does,
// or cancels the fetch of one that does (the row below), and the row fails: no fetch begun after
// the write answers. The store's fetch counter (`Writes.#countFetches`) counts the follow-up, a
// fetch begun after the call, by its `fetch` action.
test("invalidateQueries while a fetch is out, with cancelRefetch false: no new fetch begins and no fetch action is raised", async () => {
  const client = clientOf();
  const key = ["project-issues", "CORE"];
  const read = heldRead<string>();
  const actions = actionsOf(client);
  const observer = new QueryObserver(client, { queryFn: read.queryFn, queryKey: key });
  const stop = observer.subscribe(() => {});
  try {
    await settle();
    expect(read.fetches).toHaveLength(1);
    const invalidated = client.invalidateQueries({ queryKey: key }, { cancelRefetch: false });
    await settle();
    expect(read.fetches).toHaveLength(1);
    expect(read.fetches[0]?.signal.aborted).toBe(false);
    read.fetches[0]?.resolve("answer");
    await invalidated;
    expect(actions).toEqual(["fetch", "success"]);
    expect(observer.getCurrentResult().data).toBe("answer");
  } finally {
    stop();
  }
});

// Store row: as the row above.
test("invalidateQueries while a fetch is out, by default: that fetch is cancelled and another begins, raising a second fetch action", async () => {
  const client = clientOf();
  const key = ["project-issues", "CORE"];
  const read = heldRead<string>();
  client.setQueryData(key, "first answer");
  const actions = actionsOf(client);
  const observer = new QueryObserver(client, { queryFn: read.queryFn, queryKey: key });
  const stop = observer.subscribe(() => {});
  try {
    const refetched = observer.refetch();
    await settle();
    expect(read.fetches).toHaveLength(1);
    const invalidated = client.invalidateQueries({ queryKey: key });
    await settle();
    expect(read.fetches).toHaveLength(2);
    expect(read.fetches[0]?.signal.aborted).toBe(true);
    read.fetches[0]?.resolve("the cancelled fetch's answer");
    read.fetches[1]?.resolve("the restarted fetch's answer");
    await Promise.all([refetched, invalidated]);
    expect(actions).toEqual(["fetch", "fetch", "success"]);
    expect(observer.getCurrentResult().data).toBe("the restarted fetch's answer");
  } finally {
    stop();
  }
});

// Store row: write-store.test.ts, "a read sent before the save answers after it lands: the pick
// stays over it until its list is removed". With the store's fetch counter ignoring `removed`, the
// pick stays composed on a key no cache holds, and that row fails.
test("removeQueries removes a query an observer still reads, and the query cache raises removed for it", () => {
  const client = clientOf();
  const key = ["project-issues", "CORE"];
  client.setQueryData(key, "answer");
  const hash = client.getQueryCache().find({ queryKey: key })?.queryHash;
  const removed: string[] = [];
  client.getQueryCache().subscribe((event) => {
    if (event.type === "removed") removed.push(event.query.queryHash);
  });
  const observer = new QueryObserver(client, { queryKey: key });
  const stop = observer.subscribe(() => {});
  try {
    client.removeQueries({ queryKey: key });
    expect(removed).toEqual([hash as string]);
    expect(client.getQueryCache().find({ queryKey: key })).toBeUndefined();
  } finally {
    stop();
  }
});

// Store row: as the row above; garbage collection leaves the store as `removeQueries` does.
test("garbage collection removes a query no observer reads once its gcTime has passed, and the query cache raises removed for it", async () => {
  await onFakeClock(async () => {
    const client = new QueryClient({
      defaultOptions: {
        queries: { gcTime: 10, retry: false, staleTime: Number.POSITIVE_INFINITY },
      },
    });
    const key = ["project-issues", "CORE"];
    const removed: string[] = [];
    client.getQueryCache().subscribe((event) => {
      if (event.type === "removed") removed.push(event.query.queryHash);
    });
    client.setQueryData(key, "answer");
    const hash = client.getQueryCache().find({ queryKey: key })?.queryHash;
    const stop = new QueryObserver(client, { queryKey: key }).subscribe(() => {});
    jest.advanceTimersByTime(50);
    expect(removed).toEqual([]);
    stop();
    jest.advanceTimersByTime(9);
    expect(removed).toEqual([]);
    jest.advanceTimersByTime(1);
    expect(removed).toEqual([hash as string]);
  });
});

// Store rows: write-store.test.ts, "a read sent before the save answers after it lands: the pick
// stays over it until a read begun after the landing answers", which fails with the store
// ordering fetches by `dataUpdatedAt` rather than counting their `fetch` actions; and "... a
// hand write while the read begun after the landing is out leaves the pick over it", which fails
// with the store letting a change go on a `manual` success.
test("the query cache raises updated with action fetch once at each fetch's start, success at its answer, and success with manual true for setQueryData", async () => {
  const client = clientOf();
  const key = ["project-issues", "CORE"];
  const actions = actionsOf(client);
  await client.prefetchQuery({ queryFn: () => Promise.resolve("first"), queryKey: key });
  client.setQueryData(key, "by hand");
  await client.refetchQueries({ queryKey: key });
  expect(actions).toEqual(["fetch", "success", "manual", "fetch", "success"]);
});
