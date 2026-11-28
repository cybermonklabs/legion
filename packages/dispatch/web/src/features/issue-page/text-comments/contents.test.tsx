import { afterEach, expect, spyOn, test } from "bun:test";
import type { CommentNodeAnchor } from "@legion/contracts";
import { type RenderResult, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { api } from "../../../api/client";
import type { Actor, Comment } from "../../../api/types";
import { producingMessages } from "../contents-entries";
import { KEY, node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { type ContentsEntry, contentsOf } from "../IssueContents";
import { buildIssueTree, type IssueTreeReads, type TreeNode } from "../issue-tree";
import { readComment } from "../outline-fixtures";
import { CONTROL_LABELS } from "../page-words";
import { presetView } from "../presets";
import { inDocument, treeOf } from "../slots/stage-fixtures";
import { nodesInOrder } from "../tree-keys";
import { commentNode, treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { withThreadEntries } from "./contents-threads";
import {
  answerSeededReads,
  CHECK,
  columnOf,
  drawPage,
  newClient,
  ON_CHECK,
  ON_PLAN_TASK,
  ON_STATEMENT,
  PLAN_TASK,
  REPLY_ON_STATEMENT,
  RESULT,
  rowOf,
  SERVES,
  STATEMENT,
  seededIssue,
  seededNodes,
  type TextThreadSeed,
  threadOn,
} from "./margin-fixtures";
import { THREAD_NODE_ATTRIBUTE } from "./TextMargin";
import type { ConversationKind, NodeThreads, TextThread } from "./text-threads";
import { viewThreadsOf } from "./useTextThreads";

/**
 * R8 (root R14): the Contents list holds an entry for each comment on text that the reader wrote,
 * since it begins a thread, and none for a reply in its thread; the entry stands in the tree's
 * order at the node the comment is on, under the nearest entry above that node, and a press on it
 * opens its thread as an address of R8 does. A comment on a node the list's tree does not hold
 * has its entry under the node that node hangs from, where Conversation's read returns nodes of
 * its kind, a finding and a prose block among them; a comment on a node of a kind that read does
 * not return, a result or a plan task, has no entry beside any view, Graph included.
 * The page is drawn by its own root (`drawPage`), whose Contents pane stands beside the stage
 * from 1024 px.
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

// R8: the reader's own comments on text, each a thread's root - on the statement and on the check
// under it - are entries; a reply in a thread is none, nor is another reader's thread (the rule of
// `isContentsEntry`).
test("the Contents list holds an entry for each comment on text the reader wrote, and none for a reply", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT, ON_CHECK, BOBS] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    expect(await columnOf(view, COLUMN, 8000), "no column is drawn").not.toBeNull();
    await waitFor(() =>
      expect(entriesOf(view), "the list's entries").toEqual([
        ON_STATEMENT.comment.id,
        ON_CHECK.comment.id,
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
    lenses: new Map(),
    orphaned: false,
    thread: {
      anchor: null,
      key: comment.id,
      lastReplyAt: undefined,
      replies: [],
      resolved: false,
      root: { comment, kind: "comment" },
    },
    words: { block: null, occurrence: 0, quote: "words" },
  };
}

/** One node's threads as a case names them: the node's id, its kind as Conversation's read names
 *  it (`NodeThreads.conversationKind`), the node it hangs from, and the comments that root them. */
type NodeSeed = readonly [string, ConversationKind | undefined, string, readonly Comment[]];

/** The threads of each node, by the node's id, with the node's kind as Conversation's read names
 *  it and the node it hangs from. */
function threadsOn(...nodes: readonly NodeSeed[]): ReadonlyMap<string, NodeThreads> {
  return new Map(
    nodes.map(([id, conversationKind, parent, comments]): [string, NodeThreads] => [
      id,
      { conversationKind, parent, threads: comments.map(threadOf) },
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

/** Alice's comment `id`, written `at` minutes past the hour, on the words `quote` of the node of
 *  the store kind `kind` whose read id is `nodeId`: the comments read's comment, and its node as
 *  the tree read answers a comment anchored on a node, under that node with its node anchor. */
function onNode(
  id: string,
  at: number,
  nodeId: string,
  kind: string,
  quote: string
): { readonly comment: Comment; readonly node: TreeAnswerNode } {
  const comment = readComment(id, at);
  return {
    comment,
    node: {
      ...commentNode(comment, nodeId),
      anchor: {
        node: { id: nodeId, kind, state: "live" },
        occurrence: 0,
        orphaned: false,
        quote,
      },
    },
  };
}

/**
 * The tree the Contents list is given and the threads its entries are built from, each through
 * the page's own step, for the answer `tree` with `comments` as the comments read: Conversation's
 * view under the words filter `query` (`presetView`, as `slots/contents.tsx` draws the list) of
 * the tree `buildIssueTree` draws, and the threads of that answer (`viewThreadsOf`, as
 * `useTextThreads` builds them), each by its node's kind as `textThreadsOf` reads it.
 */
function listAndThreads(
  tree: TreeAnswer,
  comments: readonly Comment[],
  query = ""
): { readonly list: TreeNode; readonly threads: ReadonlyMap<string, NodeThreads> } {
  const reads: IssueTreeReads = { comments, issue: seededIssue(), tree };
  return {
    list: presetView(buildIssueTree(reads), "conversation", query).root,
    threads: viewThreadsOf(tree, reads).threads,
  };
}

/** The id of each node `root` holds, itself included, at any depth. */
function idsIn(root: TreeNode): string[] {
  return nodesInOrder(root).map(({ id }) => id);
}

/** The ids of the children of the node `id` names in `root`, in their order; undefined where
 *  `root` holds no such node. */
function childrenOf(root: TreeNode, id: string): string[] | undefined {
  const held = nodesInOrder(root).find((one) => one.id === id);
  return held?.children.map((child) => child.id);
}

// R8: the entry stands in the tree's order at the node its comment is on, under the nearest entry
// above that node: a question's statement's comment is under the question; and a comment on a
// comment, which the rule of an entry refuses as a reply, stands after it, under the entry above
// it.
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
      ["statement", "requirement", "ask", [mine("on-statement")]],
      ["post", "comment", KEY, [mine("on-post")]],
      [
        "later",
        "requirement",
        KEY,
        [mine("on-later"), { ...mine("on-bobs"), author: { id: "bob", kind: "user" } }],
      ]
    ),
    "alice",
    KEY
  );
  expect(shape(contentsOf(held, "alice", producingMessages(held)))).toEqual([
    "ask: on-statement",
    "post",
    "on-post",
    "on-later",
  ]);
  expect([...kinds.keys()].sort(), "what each entry opens").toEqual([
    "on-later",
    "on-post",
    "on-statement",
  ]);
  expect(withThreadEntries(root, new Map(), "alice", KEY).root, "no thread, no new tree").toBe(
    root
  );
});

// R8: the list holds the same entries beside every view, Graph included. Graph's read holds a
// result and a plan task, which Conversation's read returns no node of (`CONVERSATION_KINDS`),
// with the reader's comment on the words of each - the result's anchored on the result itself,
// as the tree read and the comments read answer a node anchor - and Graph's column draws each
// thread beside its node. The list, which Graph draws from Graph's read, draws no row of either
// node, so each thread hangs from a node the list holds (the check, the statement); it still has
// no entry, as beside Conversation, and Threads lists it (root R37). The reader's comment on the
// statement has its entry. A list that gave an entry to a thread whatever its node's kind fails.
test("beside Graph, the reader's comment on a result or a plan task has no entry", async () => {
  const onResult: Comment & { readonly node_anchor: CommentNodeAnchor } = {
    ...readComment("on-res1", 7),
    node_anchor: {
      node: { id: RESULT, kind: "result", state: "live" },
      occurrence: 0,
      orphaned: false,
      quote: "result words",
    },
  };
  const threads = [ON_STATEMENT, ON_PLAN_TASK];
  stubs = answerSeededReads({ posts: [onResult], threads });
  const read = spyOn(api, "getIssueTree");
  const seeded = read.getMockImplementation();
  if (seeded === undefined) throw new Error("the seeded reads answer no tree read");
  stubs.push(read);
  // The tree read with no view: the seeded nodes and the comments on the statement's and the plan
  // task's words, and the comment on the result's words under the result, with its node anchor.
  read.mockImplementation(async (key, query = {}, init) =>
    query.view === undefined
      ? treeOf(
          [
            ...seededNodes(threads),
            {
              ...commentNode(onResult, RESULT),
              anchor: {
                node: { id: RESULT, kind: "result", state: "live" },
                occurrence: 0,
                orphaned: false,
                quote: "result words",
              },
            },
          ],
          SERVES
        )
      : seeded(key, query, init)
  );
  const client = newClient();
  const view = drawPage(client, "graph");
  try {
    const column = await columnOf(view, COLUMN, 8000);
    if (column === null) throw new Error("Graph draws no column");
    const nodeOf = (id: string) =>
      column.querySelector(`[data-text-thread="${id}"]`)?.getAttribute(THREAD_NODE_ATTRIBUTE);
    await waitFor(
      () =>
        expect(
          [ON_STATEMENT.comment.id, onResult.id, ON_PLAN_TASK.comment.id].map(nodeOf),
          "Graph's column does not draw each thread beside its node"
        ).toEqual([ON_STATEMENT.node, RESULT, ON_PLAN_TASK.node]),
      { timeout: 8000 }
    );
    await waitFor(() =>
      expect(
        entriesOf(view),
        "the list holds no entry for the reader's comment on the statement beside Graph"
      ).toContain(ON_STATEMENT.comment.id)
    );
    expect(
      entriesOf(view),
      "the list holds an entry for the reader's comment on a result beside Graph"
    ).not.toContain(onResult.id);
    expect(
      entriesOf(view),
      "the list holds an entry for the reader's comment on a plan task beside Graph"
    ).not.toContain(ON_PLAN_TASK.comment.id);
    expect(entriesOf(view), "the list's entries").toEqual([ON_STATEMENT.comment.id]);
  } finally {
    view.finish();
    client.clear();
  }
});

// R8: a thread on a node the list's tree does not hold has its entry under the node that node
// hangs from, where Conversation's read returns nodes of its kind. A finding is one, though the
// tree read answers it as a block under its lens - of the supertag `block` (by an id that names
// its lens) or `lens_block` - and never by the `kind` filter's name, `finding`. The words filter
// "verdict" keeps the lens, whose verdict holds the word, and leaves out both its findings, as
// the list is drawn (`presetView`), so the reader's comment on the words of each finding has its
// entry under the lens. A list that read a node's supertag as the filter's kind gives neither
// comment an entry.
test("the reader's comment on a finding the list's tree does not hold has its entry under the finding's lens", () => {
  const lens = treeNode("lens-1", "lens", KEY, {
    attributes: { verdict: "The verdict keeps its source." },
    text: "The verdict keeps its source.",
  });
  const block = {
    ...treeNode("f1", "block", "lens-1", { text: "A finding explains where it came from." }),
    id: "lens/lens-1#f1",
  };
  const lensBlock = treeNode("f2", "lens_block", "lens-1", { text: "A finding says when." });
  const onBlock = onNode("on-f1", 7, block.id, "block", "explains");
  const onLensBlock = onNode("on-f2", 8, lensBlock.id, "block", "says");
  const tree = treeOf(
    [...seededNodes([]), lens, block, lensBlock, onBlock.node, onLensBlock.node],
    SERVES
  );
  const comments = [onBlock.comment, onLensBlock.comment];
  const { list, threads } = listAndThreads(tree, comments, "verdict");
  const entries = [onBlock.comment.id, onLensBlock.comment.id];

  expect(
    ["f1", "f2"].map((id) => threads.get(id)?.conversationKind),
    "a finding's kind is not the read's kind of a finding"
  ).toEqual(["finding", "finding"]);
  expect(
    ["f1", "f2"].map((id) => threads.get(id)?.parent),
    "a finding's thread does not hang from its lens"
  ).toEqual(["lens-1", "lens-1"]);
  const ids = idsIn(list);
  expect(ids, "the words filter leaves out the lens").toContain("lens-1");
  expect(ids, "the list's tree holds the finding read as `block`").not.toContain("f1");
  expect(ids, "the list's tree holds the finding read as `lens_block`").not.toContain("f2");
  const { kinds, root } = withThreadEntries(list, threads, "alice", KEY);
  expect(childrenOf(root, "lens-1"), "the entries under the lens").toEqual(entries);
  const listed = shape(contentsOf(root, "alice", producingMessages(root)));
  expect(listed, "the list's entries").toEqual(entries);
  expect([...kinds.keys()], "what each entry opens").toEqual(entries);
});

// R8: a block of a converted document is a node Conversation's read returns, as prose, though
// the tree read answers it as a plain block (`block`) and Conversation, with no filter on, draws
// no row of it (root R1: the conversation's own blocks are the ones written in it). The reader's
// comment on the words of such a block under the statement has its entry under the statement,
// the node the block hangs from, first among its children. A list that read a node's supertag
// as the filter's kind gives the comment no entry.
test("the reader's comment on a prose block the list's tree does not hold has its entry under the node the block hangs from", () => {
  const prose = treeNode("p1", "block", STATEMENT, {
    ...inDocument(SPEC_DOCUMENT, 2),
    text: "A converted paragraph under the statement.",
  });
  const onProse = threadOn("on-p1", SPEC_DOCUMENT, "p1", "converted paragraph", 9);
  const tree = treeOf([...seededNodes([onProse]), prose], SERVES);
  const { list, threads } = listAndThreads(tree, [onProse.comment]);
  const held = threads.get(onProse.node);

  expect(held?.conversationKind, "a plain block's kind is not the read's prose").toBe("prose");
  expect(held?.parent, "the block's thread hangs from another node").toBe(ON_STATEMENT.node);
  const ids = idsIn(list);
  expect(ids, "Conversation draws no row of the statement").toContain(ON_STATEMENT.node);
  expect(ids, "Conversation draws a row of the converted block").not.toContain(onProse.node);
  const { kinds, root } = withThreadEntries(list, threads, "alice", KEY);
  const under = childrenOf(root, ON_STATEMENT.node);
  expect(under?.[0], "the first node under the statement").toBe(onProse.comment.id);
  const listed = shape(contentsOf(root, "alice", producingMessages(root)));
  expect(listed, "the list's entries").toEqual([onProse.comment.id]);
  expect([...kinds.keys()], "what each entry opens").toEqual([onProse.comment.id]);
});

// R8: a thread on a node of a kind that Conversation's read does not return has no entry where
// the list's tree does not hold its node, though that tree holds the node it hangs from. The
// reader's comment on the words of a result, anchored on the result itself, stands by the
// result, which hangs from the check; Conversation draws the check and no row of the result. The
// list gives the comment no entry, as beside Graph (above). A list that gave every thread on a
// node it does not hold an entry under the node that node hangs from fails.
test("the reader's comment on a result the list's tree does not hold has no entry, though that tree holds the check the result hangs from", () => {
  const onResult = onNode("on-res1", 7, RESULT, "result", "result words");
  const tree = treeOf([...seededNodes([]), onResult.node], SERVES);
  const { list, threads } = listAndThreads(tree, [onResult.comment]);
  const held = threads.get(RESULT);

  const keys = held?.threads.map(({ thread }) => thread.key);
  expect(keys, "the threads by the result").toEqual([onResult.comment.id]);
  expect(held?.parent, "the result's thread does not hang from the check").toBe(ON_CHECK.node);
  expect(held?.conversationKind, "Conversation's read returns a result").toBeUndefined();
  const ids = idsIn(list);
  expect(ids, "Conversation draws no row of the check").toContain(ON_CHECK.node);
  expect(ids, "Conversation draws a row of the result").not.toContain(RESULT);
  const { kinds, root } = withThreadEntries(list, threads, "alice", KEY);
  expect([...kinds.keys()], "what each entry opens").toEqual([]);
  expect(root, "the list's tree holds an entry for the comment on the result").toBe(list);
});

// R8 (root R14, R15), through the page: the reader's comment on a statement that the first page of
// Conversation's read does not hold. The server answers that page with every node of the issue but
// the statement, what hangs under it and the comment on it, and names the page after it as it
// does (`cursor`, the walk's next node, with `remaining`); that next page holds the statement and
// the comment, a node under the statement that carries its anchor, author, words and time, as the
// server answers a comment on text in the tree read. The comments read answers no comment, so the
// entry comes from the loaded pages of Conversation's read alone. Until the reader presses the
// control after the outline's last row the list holds no entry for the comment; once that page is
// read it does, under the nearest entry above the statement - none, since the statement stands
// under the issue, which is no entry - and a press on it opens its thread. A Contents slot that
// kept the entries of the first page alone fails on the entry the next page brings.
test("the reader's comment on a statement the first page of Conversation's read does not hold has its entry once the page that holds the statement is read, and a press on it opens its thread", async () => {
  stubs = answerSeededReads({ threads: [ON_STATEMENT] });
  // No comments list: nothing of the entry is built from the comments read.
  stubs.push(spyOn(api, "listComments").mockResolvedValue([]));
  const read = spyOn(api, "getIssueTree");
  const seeded = read.getMockImplementation();
  if (seeded === undefined) throw new Error("the seeded reads answer no tree read");
  stubs.push(read);
  const statement = seededNodes([]).find((one) => one.block_id === STATEMENT);
  if (statement === undefined) throw new Error("the seeded nodes hold no statement");
  // The cursor as the server writes it: the walk's next node, `<kind>:<id>` in URL base64 with no
  // padding (`api/issue_tree_read.go`'s `treeCursor`).
  const cursor = btoa(`${statement.kind}:${statement.id}`)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/u, "");
  // The first page: every seeded node but the statement, what hangs under it, and the comment.
  const firstPage = {
    ...treeOf(seededNodes([], { without: [STATEMENT, PLAN_TASK, CHECK, RESULT] })),
    cursor,
    remaining: 2,
  };
  // The next page, the read's last: the statement and the reader's comment on its words.
  const comment = ON_STATEMENT.comment;
  const nextPage = treeAnswer([
    statement,
    treeNode(comment.id, "comment", STATEMENT, {
      anchor: {
        artifact_id: SPEC_DOCUMENT.id,
        block_id: STATEMENT,
        mark_id: "",
        orphaned: false,
        quote: ON_STATEMENT.quote,
        version: 1,
      },
      author: [comment.author],
      made_at: comment.created_at,
      text: comment.body,
    }),
  ]);
  // The cursor of each page of Conversation's read the page asked for, undefined for its first.
  const pages: (string | undefined)[] = [];
  read.mockImplementation(async (key, query = {}, init) => {
    if (query.kind === undefined) return seeded(key, query, init);
    pages.push(query.cursor);
    if (query.cursor === undefined) return firstPage;
    if (query.cursor === cursor) return nextPage;
    throw new Error(`Conversation's read has no page at ${query.cursor}`);
  });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const more = await waitFor(
      () => {
        const found = view.container.querySelector<HTMLButtonElement>("[data-tree-more] button");
        if (found === null) {
          throw new Error("the outline draws no control for the read's next page");
        }
        return found;
      },
      { timeout: 8000 }
    );
    expect(more.textContent, "the control does not say the first page's remaining count").toBe(
      `2 ${CONTROL_LABELS.tree_page_rows}`
    );
    expect(rowOf(view, ON_STATEMENT.node), "the first page holds the statement").toBeNull();
    expect(pages, "the page read the next page before the press").not.toContain(cursor);
    expect(
      entriesOf(view),
      "the list holds an entry for a comment no loaded page of Conversation's read holds"
    ).not.toContain(comment.id);
    await userEvent.setup().click(more);
    const entry = await waitFor(
      () => {
        const found = view.container.querySelector<HTMLElement>(
          `[data-contents-entry="${comment.id}"]`
        );
        if (found === null) {
          throw new Error(
            "the list holds no entry for the reader's comment once the page that holds its statement is read"
          );
        }
        return found;
      },
      { timeout: 8000 }
    );
    expect(pages, "the press did not read the page the first page named").toContain(cursor);
    expect(
      rowOf(view, ON_STATEMENT.node),
      "the next page drew no row of the statement"
    ).not.toBeNull();
    expect(
      entry.getAttribute("aria-level"),
      "the entry stands under an entry, though its statement stands under the issue"
    ).toBe("1");
    await userEvent.setup().click(entry);
    await waitFor(() =>
      expect(view.held.location?.pathname, "the press did not go to the comment's address").toBe(
        `/issues/${KEY}/comments/${comment.id}`
      )
    );
    await waitFor(
      () =>
        expect(
          document.activeElement?.closest(`[data-margin-item="${comment.id}"]`),
          "the thread does not hold the focus"
        ).not.toBeNull(),
      { timeout: 8000 }
    );
  } finally {
    view.finish();
    client.clear();
  }
});
