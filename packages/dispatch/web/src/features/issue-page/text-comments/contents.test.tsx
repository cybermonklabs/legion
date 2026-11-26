import { afterEach, expect, test } from "bun:test";
import { type RenderResult, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { Actor, Comment } from "../../../api/types";
import { producingMessages, withThreadEntries } from "../contents-entries";
import { KEY, node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { type ContentsEntry, contentsOf } from "../IssueContents";
import type { TreeNode } from "../issue-tree";
import { readComment } from "../outline-fixtures";
import {
  answerSeededReads,
  columnOf,
  drawPage,
  newClient,
  ON_PLAN_TASK,
  ON_STATEMENT,
  REPLY_ON_STATEMENT,
  STATEMENT,
  type TextThreadSeed,
  threadOn,
} from "./margin-fixtures";
import type { NodeThreads, TextThread } from "./text-threads";

/**
 * R8 (root R14): the Contents list holds an entry for each comment on text that the reader wrote,
 * since it begins a thread, and none for a reply in its thread; the entry stands in the tree's
 * order at the node the comment is on, under the nearest entry above that node, and a press on it
 * opens its thread as an address of R8 does. The page is drawn by its own root (`drawPage`), whose
 * Contents pane stands beside the stage from 1024 px.
 */

const COLUMN = "Comments on text";

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The ids of the Contents list's entries, in the order the list draws them. */
function entriesOf(view: RenderResult): string[] {
  return [...view.container.querySelectorAll<HTMLElement>("[data-contents-entry]")].map(
    (entry) => entry.dataset.contentsEntry ?? ""
  );
}

/** A comment of another reader, bob, on the words of the statement. */
const BOBS: TextThreadSeed = (() => {
  const seed = threadOn("on-bob", SPEC_DOCUMENT, STATEMENT, "plan", 8);
  const bob: Actor = { id: "bob", kind: "user" };
  return { ...seed, comment: { ...seed.comment, author: bob } };
})();

// R8: the reader's own comment on text, a thread's root, is an entry; a reply in a thread is none,
// nor is another reader's thread (the rule of `isContentsEntry`). The comment on a plan task,
// which Conversation draws no row of, stands under the node its thread hangs from.
test("the Contents list holds an entry for each comment on text the reader wrote, and none for a reply", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_PLAN_TASK, BOBS] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    expect(await columnOf(view, COLUMN, 8000), "no column is drawn").not.toBeNull();
    await waitFor(() =>
      expect(entriesOf(view), "the list's entries").toEqual([
        ON_STATEMENT.comment.id,
        ON_PLAN_TASK.comment.id,
      ])
    );
    expect(entriesOf(view), "a reply in a thread is an entry").not.toContain(REPLY_ON_STATEMENT.id);
    expect(entriesOf(view), "another reader's comment is an entry").not.toContain(BOBS.comment.id);
  } finally {
    view.finish();
    client.clear();
  }
});

// R8: a press on the entry opens the comment's thread, as the address `/comments/<id>` does: the
// thread stands at its words and holds the focus.
test("a press on that entry opens its thread", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const entry = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>(
        `[data-contents-entry="${ON_STATEMENT.comment.id}"]`
      );
      if (found === null) throw new Error("the list holds no entry for the comment");
      return found;
    });
    await userEvent.setup().click(entry);
    await waitFor(() =>
      expect(view.held.location?.pathname, "the press did not go to the comment's address").toBe(
        `/issues/${KEY}/comments/${ON_STATEMENT.comment.id}`
      )
    );
    await waitFor(() =>
      expect(
        document.activeElement?.closest(`[data-margin-item="${ON_STATEMENT.comment.id}"]`),
        "the thread does not hold the focus"
      ).not.toBeNull()
    );
  } finally {
    view.finish();
    client.clear();
  }
});

/** One thread as the column draws it, rooted at `comment`: only the fields the entries read. */
function threadOf(comment: Comment): TextThread {
  return {
    firstLine: false,
    kind: "comment",
    lenses: new Map(),
    occurrence: 0,
    orphaned: false,
    quote: "words",
    replyRoot: false,
    thread: {
      anchor: null,
      key: comment.id,
      lastReplyAt: undefined,
      replies: [],
      resolved: false,
      root: { comment, kind: "comment" },
    },
  };
}

/** The threads of each node, by the node's id. */
function threadsOn(
  ...nodes: readonly (readonly [string, string, readonly Comment[]])[]
): ReadonlyMap<string, NodeThreads> {
  return new Map(
    nodes.map(([id, parent, comments]): [string, NodeThreads] => [
      id,
      { parent, threads: comments.map(threadOf) },
    ])
  );
}

/** Each entry as its id, with the entries under it after a colon. */
function shape(entries: readonly ContentsEntry[]): string[] {
  return entries.map((entry) =>
    entry.children.length === 0
      ? entry.node.id
      : `${entry.node.id}: ${shape(entry.children).join(", ")}`
  );
}

const alice: Actor = { id: "alice", kind: "user" };
const mine = (id: string): Comment => readComment(id, 1, null, alice);

function tree(...children: TreeNode[]): TreeNode {
  return node({ children, id: KEY, type: "issue" });
}

// R8: the entry stands in the tree's order at the node its comment is on, under the nearest entry
// above that node: a question's statement's comment is under the question; a node that Conversation
// draws no row of takes its comment under the node it hangs from; and a comment on a comment,
// which the rule of an entry refuses as a reply, stands after it, under the entry above it.
test("an entry stands under the nearest entry above the node its comment is on", () => {
  const post = node({ author: alice, id: "post", type: "comment" });
  const root = tree(
    node({
      children: [
        node({
          children: [node({ id: "task", type: "plan_task" })],
          id: "statement",
          type: "requirement",
        }),
      ],
      id: "ask",
      type: "ask",
    }),
    post,
    node({ id: "later", type: "requirement" })
  );
  const { kinds, root: held } = withThreadEntries(
    root,
    threadsOn(
      ["statement", "ask", [mine("on-statement")]],
      // The plan task is no node of this tree: its thread hangs from the statement.
      ["gone-task", "statement", [mine("on-task")]],
      ["post", KEY, [mine("on-post")]],
      [
        "later",
        KEY,
        [mine("on-later"), { ...mine("on-bobs"), author: { id: "bob", kind: "user" } }],
      ]
    ),
    "alice",
    KEY
  );
  expect(shape(contentsOf(held, "alice", producingMessages(held)))).toEqual([
    "ask: on-statement, on-task",
    "post",
    "on-post",
    "on-later",
  ]);
  expect([...kinds.keys()].sort(), "what each entry opens").toEqual([
    "on-later",
    "on-post",
    "on-statement",
    "on-task",
  ]);
  expect(withThreadEntries(root, new Map(), "alice", KEY).root, "no thread, no new tree").toBe(
    root
  );
});
