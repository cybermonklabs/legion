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

import { correction, correctionFeed, DELETION, EDITED_AT } from "../../__tests__/correction-feed";
import { displayTree } from "../../__tests__/display-trees";
import { graphNode, graphNodeFields } from "../../__tests__/graph-fixture";
import { messageCorrectionFields } from "../../__tests__/message-fixture";
import { ApiError, api } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import { whoAmIQuery } from "../../api/queries";
import type { GraphReferences, IssueReferenceSummary } from "../../api/types";
import { comment as commentFixture } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "./markdown-engine";
import {
  blockQuery,
  documentSummaryQuery,
  issueSummaryQuery,
  messageQuery,
  prefetchReference,
  useReferenceLabel,
  useReferenceTarget,
  watchPicturePath,
} from "./reference-target";
import type { DispatchReferenceRoute } from "./routes";

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
// short form). A gone block of a comment or message has the link read the post, whose mark says
// whether its author deleted it; these posts stand. A pinned reference to the removed block
// answers 200 with the pin's `removed` state and keeps the pinned text; any other failure keeps
// the title it last read.
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
  const standing = { body: "Beta as written.", deleted_at: null, deleted_by: null };
  const getComment = spyOn(api, "getComment").mockResolvedValue({ comment: standing } as never);
  const getMessage = spyOn(api, "getMessage").mockResolvedValue({ message: standing } as never);
  restore = () => {
    getReferences.mockRestore();
    getComment.mockRestore();
    getMessage.mockRestore();
  };
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

// A reference to a post, or to a block of one, answers the post's marks beside its text: the
// time its author edited it, and its deletion. A block of a post that stands reads its node
// alone and carries the marks the node carries. A block of a deleted post is gone with the
// post's text, and its read answers 404 `NOT_FOUND` as any gone block's does, in words nothing
// reads: the post's own read then decides by its mark, so a gone block of a deleted message
// carries the deletion and one of a message that stands carries none.
test("a reference answers the edit and the deletion of the post it names, or whose block it names", async () => {
  const reads = spyReads();
  const deletion = {
    deleted_at: "2026-09-09T03:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  reads.getMessage.mockImplementation((_key, id) =>
    Promise.resolve(
      (id === "m-1"
        ? { message: { body: "", edited_at: "2026-09-09T02:30:00Z", ...deletion } }
        : { message: { body: "Still here.", deleted_at: null, deleted_by: null } }) as never
    )
  );
  // A read of any comment answers a deleted one, so a hook that read the comment behind a block
  // whose read answers would carry its deletion.
  reads.getComment.mockResolvedValue({ comment: { body: "", ...deletion }, replies: [] } as never);
  const standing = blockRead("Beta as written.");
  // The edited message `m-1` stands when its block is first read, and the block's node carries
  // the edit. The delete then takes the block, and the next read of it answers 404. TanStack
  // keeps the data of the last read that answered through that failure, so only the hook's rule
  // that a gone block reads as no block keeps that edit off the deleted message's block.
  let m1Deleted = false;
  // The words of the refusal decide nothing, either way: the block of the deleted post `m-1` is
  // refused in words that say nothing of a delete, and the block of the standing post `m-2` is
  // refused in the very words the server writes for a deleted post. The post's own mark is
  // what each reading follows.
  reads.getReferences.mockImplementation((reference) =>
    reference.includes("/message/m-2#")
      ? Promise.reject(
          new ApiError(404, {
            code: "NOT_FOUND",
            error: 'no block "b1" in message m-2: this message is deleted',
          })
        )
      : reference.includes("/message/m-1#")
        ? m1Deleted
          ? Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: "block gone" }))
          : Promise.resolve({
              ...standing,
              node: { ...standing.node, edited_at: "2026-09-09T02:30:00Z" },
            })
        : Promise.resolve({
            ...standing,
            node: { ...standing.node, edited_at: "2026-09-09T02:00:00Z" },
          })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(QueryClientProvider, { client: queryClient }, children);
  const edited = renderHook(
    () =>
      useReferenceTarget(
        { block: { id: "b2" }, id: "c-1", key: "CORE-1", kind: "comment" },
        { text: false }
      ),
    { wrapper }
  );
  const deleted = renderHook(
    () =>
      useReferenceTarget(
        { block: { id: "b1" }, id: "m-1", key: "CORE-1", kind: "message" },
        { text: false }
      ),
    { wrapper }
  );
  const gone = renderHook(
    () =>
      useReferenceTarget(
        { block: { id: "b1" }, id: "m-2", key: "CORE-1", kind: "message" },
        { text: false }
      ),
    { wrapper }
  );
  try {
    await waitFor(() => expect(edited.result.current.title).toBe("Beta as written."));
    expect(edited.result.current.edited).toBe("2026-09-09T02:00:00Z");
    expect(edited.result.current.deleted).toBeNull();

    await waitFor(() => expect(deleted.result.current.edited).toBe("2026-09-09T02:30:00Z"));
    m1Deleted = true;
    await act(() =>
      queryClient.refetchQueries({
        queryKey: blockQuery("dispatch://CORE-1/message/m-1#b-b1").queryKey,
      })
    );
    await waitFor(() =>
      expect(deleted.result.current.deleted).toEqual({
        at: "2026-09-09T03:00:00Z",
        by: { id: "alice", kind: "user" },
      })
    );
    expect(deleted.result.current.edited).toBeNull();
    expect(reads.getMessage).toHaveBeenCalledWith("CORE-1", "m-1");
    expect(reads.getComment).not.toHaveBeenCalled();

    await waitFor(() =>
      expect(queryClient.getQueryState(messageQuery("CORE-1", "m-2").queryKey)?.status).toBe(
        "success"
      )
    );
    expect(gone.result.current.deleted).toBeNull();
    expect(reads.getMessage).toHaveBeenCalledTimes(2);
  } finally {
    gone.unmount();
    deleted.unmount();
    edited.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 00:23Z. A hover
// card, a link's label and an unfurl card read a post's own read, or a block's graph read, and
// keep what it answered through any failure, so a post's correction writes its own facts into
// each read the cache holds (`markCorrectedPost`). After refetches that all fail, a comment, a
// message and a block of the comment read as edited, then as deleted, and none gives a word of
// what it said.
const CITED_COMMENT = "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d";
const CITED_MESSAGE = "1b2c3d4e-5f6a-4b7c-9d8e-9f0a1b2c3d4e";
const CITATIONS = {
  block: { block: { id: "b2" }, id: CITED_COMMENT, key: "CORE-1", kind: "comment" },
  comment: { id: CITED_COMMENT, key: "CORE-1", kind: "comment" },
  message: { id: CITED_MESSAGE, key: "CORE-1", kind: "message" },
} satisfies Record<string, DispatchReferenceRoute>;

/** What a link to `route` and its cards read: the target's marks, the words the link writes and
 *  the title it is drawn with. */
function useCitation(route: DispatchReferenceRoute) {
  const target = useReferenceTarget(route, { text: false });
  const { deleted, edited, title } = target;
  return { deleted, edited, label: useReferenceLabel(route, target).text, title };
}

test("a post's reference whose refetch fails after its edit and delete reads as edited, then deleted", async () => {
  let failing = false;
  const refuse = () => Promise.reject(new Error("the read failed"));
  const getComment = spyOn(api, "getComment").mockImplementation(() =>
    failing
      ? refuse()
      : Promise.resolve({
          comment: { ...commentFixture, body: "Hold the deploy.", id: CITED_COMMENT },
          replies: [],
        })
  );
  const getMessage = spyOn(api, "getMessage").mockImplementation(() =>
    failing
      ? refuse()
      : Promise.resolve({
          message: {
            author: { id: "alice", kind: "user" },
            body: "Ship on Friday.",
            broadcast_id: null,
            created_at: "2026-11-19T00:00:00Z",
            deliveries: [],
            id: CITED_MESSAGE,
            in_reply_to: null,
            issue_key: "CORE-1",
            reply_block: null,
            target: null,
            ...messageCorrectionFields(),
          },
          replies: [],
        })
  );
  const getReferences = spyOn(api, "getReferences").mockImplementation(() =>
    failing
      ? refuse()
      : Promise.resolve({
          edges: [],
          node: graphNode({
            block_id: "b2",
            id: `comment/${CITED_COMMENT}#b2`,
            kind: "block",
            parent: { id: CITED_COMMENT, kind: "comment" },
            text: "Beta as written.",
            text_tree: displayTree("Beta as written."),
          }),
        })
  );
  restore = () => {
    getComment.mockRestore();
    getMessage.mockRestore();
    getReferences.mockRestore();
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const feed = correctionFeed(queryClient);
  const hook = renderHook(
    () => {
      useEventFeed();
      return {
        block: useCitation(CITATIONS.block),
        comment: useCitation(CITATIONS.comment),
        message: useCitation(CITATIONS.message),
      };
    },
    {
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  const commentReads = [["comment"], ["references", "to"]];
  const edited = (words: string) => ({
    deleted: null,
    edited: EDITED_AT,
    label: `${words} · edited`,
    title: words,
  });
  const deleted = (label: string | undefined) => ({
    deleted: { at: DELETION.deleted_at, by: DELETION.deleted_by },
    edited: EDITED_AT,
    label,
    title: "",
  });

  try {
    await waitFor(() =>
      expect([
        hook.result.current.comment.label,
        hook.result.current.message.label,
        hook.result.current.block.label,
      ]).toEqual(["Hold the deploy.", "Ship on Friday.", "Beta as written."])
    );
    feed.start();
    failing = true;

    await feed.deliver(correction("comment", CITED_COMMENT, true), commentReads);
    await feed.deliver(correction("message", CITED_MESSAGE, true), [["issue-message"]]);
    expect(hook.result.current).toEqual({
      block: edited("Beta as written."),
      comment: edited("Hold the deploy."),
      message: edited("Ship on Friday."),
    });

    await feed.deliver(correction("comment", CITED_COMMENT, false), commentReads);
    await feed.deliver(correction("message", CITED_MESSAGE, false), [["issue-message"]]);
    // A link to a block of a deleted post keeps the words its author wrote (`useReferenceLabel`).
    await waitFor(() =>
      expect(hook.result.current).toEqual({
        block: deleted(undefined),
        comment: deleted("Deleted by alice"),
        message: deleted("Deleted by alice"),
      })
    );
  } finally {
    hook.unmount();
    feed.restore();
    queryClient.clear();
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
