import { beforeAll, expect, setSystemTime, spyOn, test } from "bun:test";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { CommentWithTrees } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { buildIssueTree } from "./issue-tree";
import {
  type OutlineWriter,
  OutlineWriterProvider,
  replyItemOf,
  useOutlineWriter,
} from "./outline-writer";
import { nodesInOrder } from "./tree-keys";
import {
  commentNode,
  markedCommentNode,
  skipWithoutTreeNodeMarks,
  treeAnswer,
  treeNode,
} from "./tree-read/tree-fixtures";

/** Skips the cases that need the tree read to answer a post's marks on its node. */
const withoutTreeNodeMarks = skipWithoutTreeNodeMarks();

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

// R7: the writer adds no request. It reads the issue's asks and messages under the keys the tree
// reads them by (`useIssueTree.ts`), and keeps them fresh as the tree does, by events and never
// by their age. Staleness is per observer, so under the app's 30 s `staleTime` and its
// window-focus refetch (`main.tsx`) a reader who comes back to the window more than 30 s after
// the load would read both again for the writer alone. It reads no comments list at all: a
// reply's mentions are read from the replied comment's own read, as the reader presses Reply.
test("a window focus more than 30 s after the load reads the writer's asks, comments and messages no more", async () => {
  const reads = [
    spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  const comments = spyOn(api, "listComments").mockResolvedValue([]);
  // The app's client's defaults (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  try {
    const view = render(
      <QueryClientProvider client={client}>
        <OutlineWriterProvider issueKey="CORE-1">{null}</OutlineWriterProvider>
      </QueryClientProvider>
    );
    try {
      await waitFor(() => expect(reads.map((read) => read.mock.calls.length)).toEqual([1, 1]));
      await waitFor(() => expect(client.isFetching()).toBe(0));
      expect(comments, "the writer read the issue's comments at mount").not.toHaveBeenCalled();
      expect(
        client.getQueryCache().find({ exact: true, queryKey: ["comments", "CORE-1"] }),
        "the writer observes the issue's comments list"
      ).toBeUndefined();
      // The reader leaves the window and comes back to it more than 30 s after the load.
      setSystemTime(Date.now() + 31_000);
      act(() => {
        focusManager.setFocused(false);
        focusManager.setFocused(true);
      });
      // Long enough for a read the focus started to have been sent.
      await act(async () => {
        const waited = Promise.withResolvers<void>();
        setTimeout(waited.resolve, 50);
        await waited.promise;
      });
      expect(
        reads.map((read) => read.mock.calls.length),
        "the window's focus read the asks or the messages again"
      ).toEqual([1, 1]);
      expect(comments, "the window's focus read the issue's comments").not.toHaveBeenCalled();
    } finally {
      view.unmount();
    }
  } finally {
    focusManager.setFocused(undefined);
    setSystemTime();
    for (const read of reads) read.mockRestore();
    comments.mockRestore();
    client.clear();
  }
});

/** A person's comment of CORE-1 that mentioned `mentions`, as its own read answers it. */
function personsComment(id: string, mentions: readonly string[]): CommentWithTrees {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "Ship it.",
    created_at: "2026-11-05T01:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: mentions.map((target) => ({ target }) as never),
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** The writer of CORE-1, held as each render hands it over, with its reads answered empty. */
function renderWriter() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["asks", "CORE-1", "thread"], []);
  client.setQueryData(["issue-messages", "CORE-1"], []);
  const held: { writer?: OutlineWriter } = {};
  const Hold = () => {
    held.writer = useOutlineWriter();
    return null;
  };
  const view = render(
    <QueryClientProvider client={client}>
      <OutlineWriterProvider issueKey="CORE-1">
        <Hold />
      </OutlineWriterProvider>
    </QueryClientProvider>
  );
  return { client, held, view };
}

// A reply to a person's comment starts with the mentions that comment made, which the tree read
// does not answer: the comment's own read does (`GET /api/v1/comments/{id}`), read as the reader
// presses Reply, once, and the line is set to reply once it answers. A reply to a session's
// comment mentions that session and reads nothing. Neither reads the issue's comments list.
test("Reply to a person's comment reads that comment for its mentions; Reply to a session's reads nothing", async () => {
  const person = personsComment("c-person", ["session:s1", "role:architect"]);
  const getComment = spyOn(api, "getComment").mockResolvedValue({
    comment: person,
    replies: [],
  } as never);
  const comments = spyOn(api, "listComments").mockResolvedValue([]);
  const { client, held, view } = renderWriter();
  try {
    act(() =>
      held.writer?.replyTo({
        author: person.author,
        id: person.id,
        kind: "comment",
        text: person.body,
      })
    );
    await waitFor(() => expect(held.writer?.reply?.id).toBe(person.id));
    expect(getComment.mock.calls.map(([id]) => id)).toEqual([person.id]);
    expect(held.writer?.reply?.mentions?.map((mention) => mention.target)).toEqual([
      "session:s1",
      "role:architect",
    ]);

    getComment.mockClear();
    act(() =>
      held.writer?.replyTo({
        author: { id: "s2", kind: "session" },
        id: "c-session",
        kind: "comment",
        text: "Done.",
      })
    );
    await waitFor(() => expect(held.writer?.reply?.id).toBe("c-session"));
    expect(held.writer?.reply?.mentions?.map((mention) => mention.target)).toEqual(["session:s2"]);
    expect(getComment, "a reply to a session's comment read the comment").not.toHaveBeenCalled();
    expect(comments, "a Reply read the issue's comments").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getComment.mockRestore();
    comments.mockRestore();
    client.clear();
  }
});

// The line is set once the comment's read answers, and only where the reader has pressed nothing
// else that sets the line meanwhile: a Reply to a second comment, pressed while the first one's
// read is out, is the one the line replies to.
test("a Reply whose comment's read answers after a later press sets nothing", async () => {
  const first = personsComment("c-first", ["session:s1"]);
  const second = personsComment("c-second", ["session:s3"]);
  const out = Promise.withResolvers<never>();
  const getComment = spyOn(api, "getComment").mockImplementation(async (id: string) =>
    id === first.id ? out.promise : ({ comment: second, replies: [] } as never)
  );
  const { client, held, view } = renderWriter();
  try {
    act(() =>
      held.writer?.replyTo({ author: first.author, id: first.id, kind: "comment", text: "a" })
    );
    act(() =>
      held.writer?.replyTo({ author: second.author, id: second.id, kind: "comment", text: "b" })
    );
    await waitFor(() => expect(held.writer?.reply?.id).toBe(second.id));
    await act(async () => out.resolve({ comment: first, replies: [] } as never));
    expect(held.writer?.reply?.id, "the earlier Reply's read set the line").toBe(second.id);
  } finally {
    view.unmount();
    getComment.mockRestore();
    client.clear();
  }
});

// Owed on the tree read answering a post's marks on its node (`edited_at`, `deleted_at`,
// `deleted_by`): a comment the read answers deleted takes no Reply, since Dispatch refuses a reply
// to it (409 `POST_DELETED`); the same comment the read answers with no delete takes one.
test.skipIf(withoutTreeNodeMarks)("a comment the tree read answers deleted takes no Reply", () => {
  const comment = personsComment("c-deleted", []);
  const deleted = {
    ...comment,
    deleted_at: "2026-11-05T02:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const issue = issueDetails();
  const rowOf = (node: ReturnType<typeof commentNode>) =>
    nodesInOrder(
      buildIssueTree({
        issue,
        tree: treeAnswer([treeNode(issue.key, "issue", null), node]),
      })
    ).find((one) => one.id === comment.id);
  const drawn = rowOf(markedCommentNode(deleted, issue.key));
  expect(drawn?.deletedAt, "the comment's row carries no delete").toBe(deleted.deleted_at);
  expect(drawn === undefined ? "no row" : replyItemOf(drawn)).toBeUndefined();
  const standing = rowOf(commentNode(comment, issue.key));
  expect(standing === undefined ? undefined : replyItemOf(standing)?.id).toBe(comment.id);
});
