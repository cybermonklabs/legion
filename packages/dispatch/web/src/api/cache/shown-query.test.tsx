import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { api } from "../client";
import { issueQuery } from "../queries";
import type { IssueDetails } from "../types";
import { cacheOf } from "./cache";
import { useShownQuery, watchShownQuery } from "./shown-query";

/**
 * The rows here read through the wrapper over a real `QueryClient`, as the page does. A query's
 * own rule for its answers (`issueQuery`'s `structuralSharing`, which keeps the held issue over
 * an answer with an older `updated_at`) is a rule about Dispatch's answers: it must judge only
 * what enters the cache, never what a reader shows - and never a value of the query a reader
 * read before its key changed.
 */

/** The issue the reader is on first: changed most recently. */
const newer = issue("CORE-1", {
  closed_at: null,
  title: "Open and busy",
  updated_at: "2026-11-10T00:00:00Z",
});

/** The issue the reader moves to: changed long before the first one, and closed. */
const older = issue("CORE-2", {
  closed_at: "2026-01-01T00:00:00Z",
  title: "Closed and quiet",
  updated_at: "2026-01-01T00:00:00Z",
});

function issue(key: string, fields: Partial<IssueDetails>): IssueDetails {
  return { key, priority: 2, ...fields } as unknown as IssueDetails;
}

let getIssue: Mock<typeof api.getIssue>;

beforeEach(() => {
  getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === newer.key ? newer : older
  );
});

afterEach(() => {
  getIssue.mockRestore();
});

/** A client whose cache already holds both issues, as Dispatch answered them. */
function clientWithBoth(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(issueQuery(newer.key).queryKey, newer);
  client.setQueryData(issueQuery(older.key).queryKey, older);
  return client;
}

/** One mounted reader of `use`, first on `newer`'s key; `rerender` moves it to another key. */
function renderReader<T>(client: QueryClient, use: (key: string) => T) {
  return renderHook(({ issueKey }: { issueKey: string }) => use(issueKey), {
    initialProps: { issueKey: newer.key },
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children),
  });
}

test("a reader moved between two issues shows the issue it is on: the issue read, with no live write", () => {
  const client = clientWithBoth();
  const reader = renderReader(client, (key) => useShownQuery(issueQuery(key)).data);
  try {
    expect(reader.result.current).toEqual(newer);
    reader.rerender({ issueKey: older.key });
    expect(reader.result.current).toEqual(older);
    reader.rerender({ issueKey: newer.key });
    expect(reader.result.current).toEqual(newer);
  } finally {
    reader.unmount();
  }
});

test("a reader moved between two issues shows the issue it is on: the issue read, with a live write on the issue it left", () => {
  const client = clientWithBoth();
  const pressed = cacheOf(client).writes.fromPress((shown) =>
    shown.on<IssueDetails>(issueQuery(newer.key).queryKey, (data) => ({ ...data, priority: 0 }))
  );
  const reader = renderReader(client, (key) => useShownQuery(issueQuery(key)).data);
  try {
    expect(reader.result.current).toEqual({ ...newer, priority: 0 });
    reader.rerender({ issueKey: older.key });
    expect(reader.result.current).toEqual(older);
    // The cache still holds Dispatch's answers alone.
    expect<unknown>(cacheOf(client).dispatched(issueQuery(newer.key).queryKey)).toBe(newer);
  } finally {
    reader.unmount();
    pressed.release();
  }
});

test("a reader moved between two issues shows the issue it is on: a caller's select over the issue read", () => {
  const client = clientWithBoth();
  const reader = renderReader(
    client,
    (key) =>
      useShownQuery({
        ...issueQuery(key),
        select: ({ closed_at, key: shownKey, updated_at }) => ({
          closed: closed_at !== null,
          key: shownKey,
          updated_at,
        }),
      }).data
  );
  try {
    expect(reader.result.current).toEqual({
      closed: false,
      key: newer.key,
      updated_at: newer.updated_at,
    });
    reader.rerender({ issueKey: older.key });
    expect(reader.result.current).toEqual({
      closed: true,
      key: older.key,
      updated_at: older.updated_at,
    });
  } finally {
    reader.unmount();
  }
});

test("a caller's select given to watchShownQuery runs over the issue as the person sees it", () => {
  const client = clientWithBoth();
  const seen: (string | undefined)[] = [];
  const stop = watchShownQuery(
    cacheOf(client),
    { ...issueQuery(newer.key), select: (data) => ({ ...data, title: data.title.toUpperCase() }) },
    (data) => seen.push(data?.title)
  );
  try {
    expect(seen).toEqual(["OPEN AND BUSY"]);
    const pressed = cacheOf(client).writes.fromPress((shown) =>
      shown.on<IssueDetails>(issueQuery(newer.key).queryKey, (data) => ({
        ...data,
        title: "Renamed",
      }))
    );
    expect(seen).toEqual(["OPEN AND BUSY", "RENAMED"]);
    pressed.release();
    expect(seen).toEqual(["OPEN AND BUSY", "RENAMED", "OPEN AND BUSY"]);
  } finally {
    stop();
  }
});
