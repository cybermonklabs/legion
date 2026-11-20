import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import {
  focusManager,
  QueryClient,
  QueryClientProvider,
  type QueryKey,
  QueryObserver,
  type QueryObserverOptions,
} from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { graphNodeFields } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import { artifactAtAddress } from "../../api/issue-cache";
import { queryRetry } from "../../api/query-client";
import type { Agent, GraphReferences, IssueReferenceSummary } from "../../api/types";
import { agentsQuery } from "./agent-registry";
import { warmMarkdownRenderer } from "./markdown-engine";
import {
  blockQuery,
  documentSummaryQuery,
  issueSummaryQuery,
  messageQuery,
  prefetchReference,
  questionsQuery,
  useReferenceTarget,
  watchPicturePath,
} from "./reference-target";
import { useTextReferences } from "./useTextReferences";

// A block reference's title is the plain words of the tree its graph read carries, which the
// fixtures below build with the shared engine.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

let restore = () => {};

afterEach(() => {
  restore();
  focusManager.setFocused(undefined);
});

/** Every api read a reference can make, spied so a test sees which ran and none reaches the
 *  network. */
function spyReads() {
  const summary: IssueReferenceSummary = {
    documents: [
      {
        id: "doc-1",
        kind: "doc",
        latest_version: 2,
        name: "design.md",
        primary: true,
        slug: "design",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Design decision",
  };
  const spies = {
    getArtifactText: spyOn(api, "getArtifactText").mockResolvedValue({
      markdown: "Live",
      token: "sha256:live",
      version: null,
    }),
    getArtifactVersion: spyOn(api, "getArtifactVersion").mockResolvedValue({} as never),
    getAsk: spyOn(api, "getAsk").mockResolvedValue({} as never),
    getComment: spyOn(api, "getComment").mockResolvedValue({} as never),
    // Only what the hover warm-up reads: the primary spec among the issue's artifacts.
    getIssue: spyOn(api, "getIssue").mockResolvedValue({
      artifacts: [{ id: "spec-1", kind: "doc" }],
      key: "CORE-1",
      primary_artifact_id: "spec-1",
    } as never),
    getIssueSummary: spyOn(api, "getIssueSummary").mockResolvedValue(summary),
    getMessage: spyOn(api, "getMessage").mockResolvedValue({} as never),
    getProjectArtifact: spyOn(api, "getProjectArtifact").mockResolvedValue({} as never),
    getProjectDocumentSummary: spyOn(api, "getProjectDocumentSummary").mockResolvedValue({
      id: "doc-9",
      kind: "doc",
      latest_version: 4,
      name: "Runbook.md",
      project: "CORE",
      slug: "runbook-md",
    }),
    getReferences: spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: { id: "CORE-1/spec#intro", kind: "block" },
    } as never),
  };
  restore = () => {
    for (const spy of Object.values(spies)) {
      spy.mockRestore();
    }
  };
  return spies;
}

/** Waits until the client holds exactly `expected`, the keys of every prefetch started, loaded. */
async function expectKeys(queryClient: QueryClient, expected: unknown[][]): Promise<void> {
  await waitFor(() =>
    expect(
      queryClient
        .getQueryCache()
        .getAll()
        .filter((query) => query.state.status === "success")
        .map((query) => [...query.queryKey])
    ).toEqual(expected)
  );
}

test("summary reads are fetched again by events, never by age", () => {
  for (const options of [issueSummaryQuery("CORE-1"), documentSummaryQuery("CORE", "runbook")]) {
    expect(options.staleTime).toBe(Number.POSITIVE_INFINITY);
  }
  expect([...issueSummaryQuery("CORE-1").queryKey]).toEqual(["issue-summary", "CORE-1"]);
  expect([...documentSummaryQuery("CORE", "runbook").queryKey]).toEqual([
    "doc-summary",
    "CORE",
    "runbook",
  ]);
});

/**
 * The person comes back to the tab. Resolves once the client has handled the focus: its focus
 * listener awaits `resumePausedMutations` before it refetches, so awaiting the same call after it
 * finds any refetch the focus starts already at its read.
 */
async function refocus(queryClient: QueryClient): Promise<void> {
  focusManager.setFocused(false);
  focusManager.setFocused(true);
  await queryClient.resumePausedMutations();
}

/**
 * Observes `options` as a mounted reference does, with `read` failing its first call: the failed
 * read is fetched again on focus, a healthy one is not, and a refresh that fails over the data it
 * held (an event's, which fetches the query directly) is fetched again on the next focus.
 */
async function expectFocusRecoversFailedRead<TData, TKey extends QueryKey>(
  options: QueryObserverOptions<TData, Error, TData, TData, TKey>,
  read: { mock: { calls: unknown[] }; mockRejectedValueOnce: (error: Error) => unknown }
): Promise<void> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.mount();
  read.mockRejectedValueOnce(new Error("503 Service Unavailable"));
  const observer = new QueryObserver(queryClient, options);
  const unsubscribe = observer.subscribe(() => {});
  try {
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("error"));
    expect(read.mock.calls).toHaveLength(1);

    await refocus(queryClient);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("success"));
    expect(read.mock.calls).toHaveLength(2);

    await refocus(queryClient);
    expect(read.mock.calls).toHaveLength(2);

    read.mockRejectedValueOnce(new Error("503 Service Unavailable"));
    await queryClient
      .getQueryCache()
      .find({ exact: true, queryKey: options.queryKey })
      ?.fetch()
      .catch(() => undefined);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("error"));
    expect(read.mock.calls).toHaveLength(3);
    await refocus(queryClient);
    await waitFor(() => expect(observer.getCurrentResult().status).toBe("success"));
    expect(read.mock.calls).toHaveLength(4);
  } finally {
    unsubscribe();
    queryClient.unmount();
  }
}

test("a summary or block read that failed is fetched again when the window regains focus, and a healthy one is not", async () => {
  const reads = spyReads();
  await expectFocusRecoversFailedRead(issueSummaryQuery("CORE-1"), reads.getIssueSummary);
  await expectFocusRecoversFailedRead(
    documentSummaryQuery("CORE", "runbook-md"),
    reads.getProjectDocumentSummary
  );
  await expectFocusRecoversFailedRead(
    blockQuery("dispatch://CORE-1/spec#intro"),
    reads.getReferences
  );
});

// A target Dispatch answers does not exist is an answer, drawn as the words its writer wrote, so
// its 404 is asked once a page: the first reference that mounts asks it, with the tries the
// dashboard's retry rule allows, and a second and a third reference to it - another mention,
// the same body drawn again - read the answer that is there rather than ask again
// (`REFERENCE_READ`). An event that invalidates the read still asks once more.
test("a target Dispatch answers does not exist is asked once, however many references to it mount", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no issue CORE-404" })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: 0 } },
  });
  const route = { key: "CORE-404", kind: "issue" } as const;
  const { queryKey } = issueSummaryQuery("CORE-404");
  // The tries of one read: the request and the two retries `queryRetry` allows a 404.
  const oneRun = 3;
  const first = renderTarget(queryClient, route);
  const mounted = [first];
  try {
    await waitFor(() => expect(first.result.current.missing).toBe(true));
    expect(getIssueSummary).toHaveBeenCalledTimes(oneRun);

    // A mount that asks starts its read as it subscribes, inside the render's own `act`, so the
    // read stands idle after one that asks nothing.
    for (const _ of [2, 3]) {
      const later = renderTarget(queryClient, route);
      mounted.push(later);
      expect(queryClient.getQueryState(queryKey)?.fetchStatus).toBe("idle");
      expect(later.result.current.missing).toBe(true);
      expect(getIssueSummary).toHaveBeenCalledTimes(oneRun);
    }

    await act(() => queryClient.invalidateQueries({ queryKey }));
    expect(getIssueSummary).toHaveBeenCalledTimes(2 * oneRun);
  } finally {
    for (const hook of mounted) hook.unmount();
    getIssueSummary.mockRestore();
  }
});

// A target that was read and has since gone - an event invalidated its read while nothing drew
// it, and Dispatch then answered it does not exist - is held to the same answer: the failed
// fetch leaves the old data marked stale, which TanStack asks again on every mount
// (`refetchOnMount`), and `REFERENCE_READ` asks it once.
test("a target that has gone since it was read is asked once, however many references to it mount", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Fix the thing",
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: 0 } },
  });
  const route = { key: "CORE-1", kind: "issue" } as const;
  const { queryKey } = issueSummaryQuery("CORE-1");
  // The tries of one read that answers 404: the request and the two retries `queryRetry` allows.
  const oneRun = 3;
  const first = renderTarget(queryClient, route);
  const mounted = [first];
  try {
    await waitFor(() => expect(first.result.current.title).toBe("Fix the thing"));
    // Nothing draws the target when the event arrives.
    mounted.pop()?.unmount();
    await act(() => queryClient.invalidateQueries({ queryKey }));
    expect(getIssueSummary).toHaveBeenCalledTimes(1);

    getIssueSummary.mockRejectedValue(
      new ApiError(404, { code: "NOT_FOUND", error: "no issue CORE-1" })
    );
    const second = renderTarget(queryClient, route);
    mounted.push(second);
    await waitFor(() => expect(second.result.current.missing).toBe(true));
    expect(getIssueSummary).toHaveBeenCalledTimes(1 + oneRun);

    // A mount that asks starts its read as it subscribes, inside the render's own `act`, so the
    // read stands idle after one that asks nothing.
    const third = renderTarget(queryClient, route);
    mounted.push(third);
    expect(queryClient.getQueryState(queryKey)?.fetchStatus).toBe("idle");
    expect(third.result.current.missing).toBe(true);
    expect(getIssueSummary).toHaveBeenCalledTimes(1 + oneRun);
  } finally {
    for (const hook of mounted) hook.unmount();
    getIssueSummary.mockRestore();
  }
});

// A failure that can pass - a 503, a dropped connection - is no answer: the next reference to
// that target that mounts asks it again, within the retry rule, and names the target once
// Dispatch can answer.
test("a read that failed in a way that can pass is asked again when a reference to it next mounts", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "try again" })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: queryRetry, retryDelay: 0 } },
  });
  const route = { key: "CORE-1", kind: "issue" } as const;
  // The tries of one read: the request and the two retries `queryRetry` allows a 503.
  const oneRun = 3;
  const first = renderTarget(queryClient, route);
  const mounted = [first];
  try {
    await waitFor(() =>
      expect(queryClient.getQueryState(issueSummaryQuery("CORE-1").queryKey)?.status).toBe("error")
    );
    expect(getIssueSummary).toHaveBeenCalledTimes(oneRun);
    expect(first.result.current.missing).toBe(false);

    getIssueSummary.mockResolvedValue({
      documents: [],
      key: "CORE-1",
      project: "CORE",
      status: "in_progress",
      title: "Fix the thing",
    });
    const later = renderTarget(queryClient, route);
    mounted.push(later);
    await waitFor(() => expect(later.result.current.title).toBe("Fix the thing"));
    expect(getIssueSummary).toHaveBeenCalledTimes(oneRun + 1);
  } finally {
    for (const hook of mounted) hook.unmount();
    getIssueSummary.mockRestore();
  }
});

// A read a reference makes after it mounted - here the question map of a text once a `#id` is
// typed into it - is held to the same rule as one made at mount: over a read that failed, it
// asks nothing again (`useReferenceQuery`). TanStack's observer that is enabled late asks a failed
// read again whatever `retryOnMount` says.
test("a reference read made after mount, over a read that failed, is not asked again", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no document for comment c-1" })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const source = { id: "c-1", issue: "CORE-1", kind: "comment" } as const;
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client: queryClient }, children);
  const first = renderHook(() => useTextReferences("Decide #q2.", source), { wrapper });
  const later = renderHook(({ text }) => useTextReferences(text, source), {
    initialProps: { text: "Nothing to decide yet." },
    wrapper,
  });
  const { queryKey } = questionsQuery(source);
  try {
    await waitFor(() => expect(queryClient.getQueryState(queryKey)?.status).toBe("error"));
    expect(getReferenceQuestions).toHaveBeenCalledTimes(1);

    // A read that asks starts as its observer subscribes, inside the render's own `act`, so the
    // read stands idle after one that asks nothing.
    later.rerender({ text: "Decide #q2 now." });
    expect(queryClient.getQueryState(queryKey)?.fetchStatus).toBe("idle");
    expect(getReferenceQuestions).toHaveBeenCalledTimes(1);
  } finally {
    first.unmount();
    later.unmount();
    getReferenceQuestions.mockRestore();
  }
});

test("a message reference's read sits outside the issue details' key", () => {
  expect([...messageQuery("CORE-1", "m-1").queryKey]).toEqual(["issue-message", "CORE-1", "m-1"]);
});

test("hovering an issue warms the full issue and its spec text, which the hover card shows", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { key: "CORE-1", kind: "issue" });
  await expectKeys(queryClient, [
    ["issue", "CORE-1"],
    ["artifact", "spec-1", "text"],
  ]);
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
});

test("hovering an issue's document warms its issue summary and its text, not the full issue", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { key: "CORE-1", kind: "artifact", slug: "design" });
  await expectKeys(queryClient, [
    ["issue-summary", "CORE-1"],
    ["artifact", "doc-1", "text"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
});

test("hovering a project document warms its summary and the version it pins", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, {
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
    version: 3,
  });
  await expectKeys(queryClient, [
    ["doc-summary", "CORE", "runbook-md"],
    ["artifact", "doc-9", 3],
  ]);
  expect(reads.getProjectArtifact).not.toHaveBeenCalled();
});

test("hovering an ask, a comment or a message warms its own read and nothing of its issue or document", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, { id: "ask-1", key: "CORE-1", kind: "ask" });
  prefetchReference(queryClient, { id: "comment-1", key: "CORE-1", kind: "comment" });
  prefetchReference(queryClient, { id: "m-1", key: "CORE-1", kind: "message" });
  prefetchReference(queryClient, {
    item: { id: "ask-2", kind: "ask" },
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
  });
  await expectKeys(queryClient, [
    ["ask", "ask-1"],
    ["comment", "comment-1"],
    ["issue-message", "CORE-1", "m-1"],
    ["ask", "ask-2"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
  expect(reads.getProjectDocumentSummary).not.toHaveBeenCalled();
});

test("hovering a block of a comment or message warms its graph read alone, no summary of a document", async () => {
  const reads = spyReads();
  const queryClient = new QueryClient();
  prefetchReference(queryClient, {
    block: { id: "b2" },
    id: "c-1",
    key: "CORE-1",
    kind: "comment",
  });
  prefetchReference(queryClient, {
    block: { id: "b1" },
    id: "m-1",
    key: "CORE-1",
    kind: "message",
  });
  prefetchReference(queryClient, {
    block: { id: "b3" },
    item: { id: "c-2", kind: "comment" },
    kind: "document",
    project: "CORE",
    slug: "runbook-md",
  });
  await expectKeys(queryClient, [
    ["references", "to", "dispatch://CORE-1/comment/c-1#b-b2"],
    ["references", "to", "dispatch://CORE-1/message/m-1#b-b1"],
    ["references", "to", "dispatch://CORE/artifact/runbook-md/comment/c-2#b-b3"],
  ]);
  expect(reads.getIssue).not.toHaveBeenCalled();
  expect(reads.getIssueSummary).not.toHaveBeenCalled();
  expect(reads.getProjectDocumentSummary).not.toHaveBeenCalled();
  expect(reads.getComment).not.toHaveBeenCalled();
  expect(reads.getMessage).not.toHaveBeenCalled();
});

/** A block's graph read: its current text with the reading the server made of it, and the pin
 *  a pinned reference's read carries, with its own. A reference's title is the tree's plain
 *  words now, so a read with no tree has none. */
function blockRead(text: string | undefined, pin?: NonNullable<GraphReferences["node"]["pin"]>) {
  return {
    edges: [],
    node: {
      ...graphNodeFields(),
      block_id: "b2",
      id: "CORE-1/spec#b2",
      kind: "block",
      pin,
      text,
      text_tree: text === undefined ? undefined : displayTree(text),
    },
  } satisfies GraphReferences;
}

// A link to a block is titled by the block's text, from its graph read, which an event that can
// change the block fetches again. An edit that deletes the block makes the read answer 404: the
// link drops the title it showed and reads as a freshly loaded page's link (no title, so its
// short form). A pinned reference to the removed block answers 200 with the pin's `removed` state
// and keeps the pinned text; any other failure keeps the title it last read.
test.each([
  [
    "a comment block deleted by its author",
    { block: { id: "b2" }, id: "c-1", key: "CORE-1", kind: "comment" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a message block that is gone",
    { block: { id: "b2" }, id: "m-1", key: "CORE-1", kind: "message" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a spec block deleted by a document edit",
    { block: { id: "b2" }, key: "CORE-1", kind: "spec" } as const,
    new ApiError(404, { code: "NOT_FOUND", error: "no block" }),
    undefined,
  ],
  [
    "a spec block whose refresh failed with a 500",
    { block: { id: "b2" }, key: "CORE-1", kind: "spec" } as const,
    new ApiError(500, { code: "INTERNAL", error: "boom" }),
    "Beta as written.",
  ],
  [
    "a pinned comment block whose refresh answers it removed",
    { block: { id: "b2", pin: "1" }, id: "c-1", key: "CORE-1", kind: "comment" } as const,
    // Built when the row runs, not when this table is: the tree a read carries needs the
    // engine, which `beforeAll` warms.
    () =>
      blockRead(undefined, {
        ref: "dispatch://CORE-1/comment/c-1#b-b2@v1",
        removed_version: 2,
        state: "removed",
        text: "Beta as written.",
        text_tree: displayTree("Beta as written."),
        version: 1,
      }),
    "Beta as written.",
  ],
] as const)("the link to %s is titled as the refresh leaves it", async (_case, route, refresh, kept) => {
  const pin =
    "pin" in route.block
      ? {
          ref: "dispatch://CORE-1/comment/c-1#b-b2@v1",
          state: "unchanged" as const,
          text: "Beta as written.",
          text_tree: displayTree("Beta as written."),
          version: 1,
        }
      : undefined;
  const answer = typeof refresh === "function" ? refresh() : refresh;
  const getReferences = spyOn(api, "getReferences")
    .mockResolvedValueOnce(blockRead("Beta as written.", pin))
    .mockImplementationOnce(() =>
      answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer)
    );
  restore = () => getReferences.mockRestore();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const hook = renderHook(() => useReferenceTarget(route, { text: false }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
  try {
    await waitFor(() => expect(hook.result.current.title).toBe("Beta as written."));

    await act(() => queryClient.refetchQueries({ queryKey: ["references", "to"] }));
    expect(getReferences).toHaveBeenCalledTimes(2);
    await waitFor(() => expect(hook.result.current.title).toBe(kept));
  } finally {
    hook.unmount();
  }
});

test("watchPicturePath reports an image's latest version and follows its summary as it changes", async () => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const summary = (kind: "image" | "doc", latest: number): IssueReferenceSummary => ({
    documents: [
      {
        id: "shot",
        kind,
        latest_version: latest,
        name: "shot.png",
        primary: false,
        slug: "shot-png",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Core one",
  });
  queryClient.setQueryData(issueSummaryQuery("CORE-1").queryKey, summary("image", 2));
  const reported: (string | undefined)[] = [];
  const stop = watchPicturePath(
    queryClient,
    { key: "CORE-1", kind: "artifact", slug: "shot-png" },
    (path) => reported.push(path)
  );

  // A summary already read answers at once, so the picture draws with no link before it.
  expect(reported).toEqual(["/api/v1/issues/CORE-1/artifacts/shot-png/versions/2"]);
  // An event's refresh of the summary - a new version uploaded - reaches the watcher.
  queryClient.setQueryData(issueSummaryQuery("CORE-1").queryKey, summary("image", 3));
  await waitFor(() =>
    expect(reported.at(-1)).toBe("/api/v1/issues/CORE-1/artifacts/shot-png/versions/3")
  );
  // A document is no picture.
  queryClient.setQueryData(issueSummaryQuery("CORE-1").queryKey, summary("doc", 4));
  await waitFor(() => expect(reported.at(-1)).toBeUndefined());

  // Stopping unsubscribes: the summary query has no observer left to notify.
  const query = queryClient
    .getQueryCache()
    .find({ queryKey: issueSummaryQuery("CORE-1").queryKey });
  expect(query?.getObserversCount()).toBe(1);
  stop();
  expect(query?.getObserversCount()).toBe(0);
});

test("watchPicturePath answers undefined at once for a reference to no artifact", () => {
  const reported: (string | undefined)[] = [];
  watchPicturePath(new QueryClient(), { id: "ask-1", key: "CORE-1", kind: "ask" }, (path) =>
    reported.push(path)
  );
  expect(reported).toEqual([undefined]);
});

/** Renders `useReferenceTarget` for `route` over `queryClient`, which the test has seeded. */
function renderTarget(queryClient: QueryClient, route: Parameters<typeof useReferenceTarget>[0]) {
  return renderHook(() => useReferenceTarget(route, { text: false }), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client: queryClient }, children),
  });
}

// The read route finds an artifact by its slug, else a document by its name
// (`resolveDocumentReference`), and a document renamed to a slug-shaped name keeps its first
// slug: a reference written with either names the document, and a name finds no file or image.
test("an artifact reference finds its document by slug or by its current name, and a file by its slug alone", () => {
  spyReads();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(issueSummaryQuery("CORE-1").queryKey, {
    documents: [
      {
        id: "notes",
        kind: "doc",
        latest_version: 1,
        name: "launch-notes",
        primary: false,
        slug: "notes-md",
      },
      {
        id: "shot",
        kind: "image",
        latest_version: 1,
        name: "shot",
        primary: false,
        slug: "shot-png",
      },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Core one",
  } satisfies IssueReferenceSummary);
  const read = (slug: string) => {
    const hook = renderTarget(queryClient, { key: "CORE-1", kind: "artifact", slug });
    const { missing, title } = hook.result.current;
    hook.unmount();
    return { missing, title };
  };
  expect(read("notes-md")).toEqual({ missing: false, title: "launch-notes" });
  expect(read("launch-notes")).toEqual({ missing: false, title: "launch-notes" });
  expect(read("shot-png")).toEqual({ missing: false, title: "shot" });
  expect(read("shot")).toEqual({ missing: true, title: undefined });
});

// The one lookup the reference and the page it opens share, over any list of artifacts. A slug
// wins over another document whose name it is - a pair the server never lets a rename make
// (`renameArtifact`), and which its read route would answer the same way; a name finds a
// document alone; an address naming neither finds nothing.
test("artifactAtAddress finds an artifact by slug first, then a document by its name, and nothing else", () => {
  const artifacts = [
    { id: "notes", kind: "doc", name: "launch-notes", slug: "notes-md" },
    { id: "shot", kind: "image", name: "shot", slug: "shot-png" },
    { id: "plan", kind: "doc", name: "notes-md", slug: "plan" },
  ];
  expect(artifactAtAddress(artifacts, "notes-md")?.id).toBe("notes");
  expect(artifactAtAddress(artifacts, "launch-notes")?.id).toBe("notes");
  expect(artifactAtAddress(artifacts, "shot-png")?.id).toBe("shot");
  expect(artifactAtAddress(artifacts, "shot")).toBeUndefined();
  expect(artifactAtAddress(artifacts, "missing")).toBeUndefined();
  expect(artifactAtAddress(undefined, "notes-md")).toBeUndefined();
  expect(artifactAtAddress(null, "notes-md")).toBeUndefined();
});

// The registry is the shell's poller's to refresh: a session reference reads the answer the page
// holds, older than the registry's `staleTime` here, and asks for it on no mount of its own.
test("a session the registry lists reads as its title trimmed, one it lists untitled links in its words, and neither reads the registry", () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  restore = () => listAgents.mockRestore();
  const listed = (session_id: string, title: string): Agent => ({
    capabilities: ["aside"],
    dir: "/workspaces/legion",
    host: "build-host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id,
    state: "running",
    title,
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(
    agentsQuery().queryKey,
    [
      listed("11111111-1111-4111-8111-111111111111", "  Planner  "),
      listed("22222222-2222-4222-8222-222222222222", "   "),
    ],
    { updatedAt: Date.now() - 60_000 }
  );
  const read = (session: string) => {
    const hook = renderTarget(queryClient, { kind: "agent-session", session });
    const { known, missing, title } = hook.result.current;
    hook.unmount();
    return { known, missing, title };
  };
  expect(read("11111111-1111-4111-8111-111111111111")).toEqual({
    known: false,
    missing: false,
    title: "Planner",
  });
  expect(read("22222222-2222-4222-8222-222222222222")).toEqual({
    known: true,
    missing: false,
    title: undefined,
  });
  expect(listAgents).not.toHaveBeenCalled();
});
