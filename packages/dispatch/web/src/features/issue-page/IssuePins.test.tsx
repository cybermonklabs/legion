import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import { issueAskThreadsQuery } from "../../api/queries";
import type { CommentWithTrees } from "../../api/types";
import { conversationItemId, pinnedItemMarker } from "../issue/pins";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { IssuePins } from "./IssuePins";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { CONTROL_LABELS } from "./page-words";
import {
  commentNode,
  markedCommentNode,
  skipWithoutTreeNodeMarks,
  treeAnswer,
  treeNode,
} from "./tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswerNode } from "./tree-read/tree-read";
import { issueMessagesQuery } from "./useIssueTree";

/** The reader's Pinned list (`IssuePins`), drawn over the reads it shares with the outline: its
 *  comments are the outline's tree read's, and a pinned comment that read does not hold is read
 *  on its own, only while Details is open. */

const KEY = "CORE-1";

/** Skips the cases that need the tree read to answer a post's marks on its node. */
const withoutTreeNodeMarks = skipWithoutTreeNodeMarks();

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let restore = () => {};

afterEach(() => {
  restore();
  restore = () => {};
});

/** The Details drawer, open or shut. */
const drawer = (open: boolean): MarginDrawer => ({
  open,
  setOpen: () => {},
  setPage: () => {},
});

/** A comment of the issue, by `id`, with `body` and the server's reading of it. */
function commentOf(id: string, body: string): CommentWithTrees {
  return {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    body_tree: displayTree(body),
    created_at: "2026-11-05T01:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
}

/** `comment`'s node in the tree read's answer, with the server's reading of its words. */
function nodeOf(comment: CommentWithTrees): TreeAnswerNode {
  return { ...commentNode(comment), text_tree: comment.body_tree };
}

/** A client whose reads Pinned shares with the outline hold their answers: the asks, the
 *  messages and Conversation's tree read, answered with `nodes` under the issue. */
function clientWith(nodes: readonly TreeAnswerNode[]): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(issueMessagesQuery(KEY).queryKey, []);
  client.setQueryData(
    outlineQuery(KEY, "conversation").queryKey,
    treeAnswer([treeNode(KEY, "issue", null), ...nodes])
  );
  return client;
}

/** Pinned of the comments `ids`, in the Details drawer, `open` or shut. */
function pinsOf(client: QueryClient, ids: readonly string[], open: boolean) {
  const tree = (isOpen: boolean) => (
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <MarginSlotProvider drawer={drawer(isOpen)}>
          <IssuePins
            dismissed={ids.map((id) => pinnedItemMarker(conversationItemId("comment", id)))}
            issueKey={KEY}
          />
        </MarginSlotProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(tree(open));
  return { ...view, redraw: (isOpen: boolean) => view.rerender(tree(isOpen)) };
}

/** Spies on the issue's comments list and on a comment's own read, which answers `comments` by
 *  id. */
function spyCommentReads(comments: readonly CommentWithTrees[]) {
  const reads = {
    getComment: spyOn(api, "getComment").mockImplementation(async (id: string) => {
      const comment = comments.find((one) => one.id === id);
      if (comment === undefined) throw new Error(`no comment ${id}`);
      return { comment: { ...comment, anchor_node: null }, replies: [] } as never;
    }),
    listComments: spyOn(api, "listComments").mockResolvedValue([]),
  };
  restore = () => {
    reads.getComment.mockRestore();
    reads.listComments.mockRestore();
  };
  return reads;
}

// A pin's line is read as its row in the outline reads it: from the first block of the server's
// reading of its record, so a pinned block quote of two paragraphs is drawn by the first. The
// comment is the tree read's own, which the list draws it from.
test("a pinned comment that quotes two paragraphs is drawn by the first paragraph's words", () => {
  const comment = commentOf(
    "4a7c2e19-8b3d-4f60-a5e1-6c9d0b2f3e47",
    "> Yes\n>\n> Keep the old page."
  );
  const view = pinsOf(clientWith([nodeOf(comment)]), [comment.id], false);
  try {
    const pins = view.getByRole("region", { name: CONTROL_LABELS.pinned });
    expect(
      within(pins).getByRole("link").textContent,
      "the pin is not drawn by its quote's first paragraph"
    ).toBe("Yes");
  } finally {
    view.unmount();
  }
});

// The page reads no issue's whole comments list at load: a pinned comment the outline's tree
// read holds is drawn from that read, and one it does not hold - of a thread only agents wrote
// in, or older than the read's first comment page - is read on its own (`GET
// /api/v1/comments/{id}`), only while Details is open, and only that one.
test("Pinned draws a held comment from the tree read and reads an unheld one only while Details is open", async () => {
  const held = commentOf("1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e", "Held by the tree. Then more.");
  const unheld = commentOf("2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f", "Read on its own. Then more.");
  const reads = spyCommentReads([held, unheld]);
  const client = clientWith([nodeOf(held)]);
  const view = pinsOf(client, [held.id, unheld.id], false);
  try {
    const link = (id: string) =>
      within(view.getByRole("region", { name: CONTROL_LABELS.pinned }))
        .getAllByRole("link")
        .find((one) => one.closest(`[data-pin="${conversationItemId("comment", id)}"]`) !== null);
    await waitFor(() => expect(link(held.id)?.textContent).toBe("Held by the tree."));
    expect(reads.listComments, "Pinned read the issue's comments").not.toHaveBeenCalled();
    expect(
      client.getQueryCache().find({ exact: true, queryKey: ["comments", KEY] }),
      "Pinned observes the issue's comments list"
    ).toBeUndefined();
    expect(reads.getComment, "Pinned read a comment with Details shut").not.toHaveBeenCalled();

    view.redraw(true);
    await waitFor(() => expect(link(unheld.id)?.textContent).toBe("Read on its own."));
    expect(reads.getComment.mock.calls.map(([id]) => id)).toEqual([unheld.id]);
    expect(reads.listComments, "Pinned read the issue's comments").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    client.clear();
  }
});

// Owed on the tree read answering a post's marks on its node (`edited_at`, `deleted_at`,
// `deleted_by`): a pinned comment its author deleted, which the read answers with no text and
// the delete's marks, is drawn as the deleted line from the read alone, with no read of its own.
test.skipIf(withoutTreeNodeMarks)(
  "a pinned comment the tree read answers deleted is drawn as the deleted line from that read",
  async () => {
    const comment = {
      ...commentOf("3d4e5f6a-7b8c-4d9e-af0a-2b3c4d5e6f7a", "Ship the fix tonight."),
      deleted_at: "2026-11-05T02:00:00Z",
      deleted_by: { id: "alice", kind: "user" },
    };
    const reads = spyCommentReads([comment]);
    const client = clientWith([markedCommentNode(comment)]);
    const view = pinsOf(client, [comment.id], true);
    try {
      const pins = view.getByRole("region", { name: CONTROL_LABELS.pinned });
      await waitFor(() =>
        expect(
          within(pins).getByRole("link").querySelector("[data-post-deleted]"),
          "the pin is not drawn as the deleted line"
        ).not.toBeNull()
      );
      expect(within(pins).getByRole("link").textContent).not.toContain("Ship the fix tonight.");
      expect(reads.getComment).not.toHaveBeenCalled();
      expect(reads.listComments).not.toHaveBeenCalled();
    } finally {
      view.unmount();
      client.clear();
    }
  }
);
