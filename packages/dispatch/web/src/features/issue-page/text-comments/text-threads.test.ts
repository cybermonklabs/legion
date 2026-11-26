import { expect, test } from "bun:test";

import type { Anchor, Comment } from "../../../api/types";
import { KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type IssueTreeReads, type TreeNode } from "../issue-tree";
import { readComment } from "../outline-fixtures";
import { inDocument, treeOf } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { commentNode, type TreeNodeExtra, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { GATES } from "./gates";
import {
  CHECK,
  DECISION,
  ON_CHECK,
  ON_PLAN_TASK,
  ON_STATEMENT,
  OTHER,
  PLAN_DOCUMENT,
  PLAN_TASK,
  REPLY_ON_STATEMENT,
  recordedIssue,
  recordedReads,
  SERVES,
  STATEMENT,
  seededIssue,
  seededNodes,
} from "./margin-fixtures";
import type { QuotedReplyBlock } from "./on-text";
import { type NodeThreads, textThreadsOf } from "./text-threads";

/**
 * Each drawn node's threads on text (`textThreadsOf`), over the issue the margin part's cases
 * share (`margin-fixtures.tsx`: a statement of the spec, the plan task and the check that serve
 * it, a decision and a block of the plan), its blocks as the tree read answers them, and over the
 * answers the recorder read from the real server at part W's head (`fixtures/`, gate B0). The
 * nodes a view draws are the rows `buildIssueTree` draws from the same reads, as Conversation
 * draws them, unless a case names them. A case owed on a gate is skipped on its `gates.ts` line.
 */

/** The recorder's answers (`recorded` in `margin-fixtures.tsx`): the reads of Conversation's
 *  tree, of the comments and of the messages. */
const { comments: COMMENTS, conversation: CONVERSATION, messages: MESSAGES } = recordedReads();

/** The anchor of a comment on the words `quote` of `block` in `document`. */
function anchorOn(
  document: { readonly id: string },
  block: string,
  quote: string,
  extra: Partial<Anchor> = {}
): Anchor {
  return {
    artifact_id: document.id,
    block_id: block,
    mark_id: "",
    orphaned: false,
    quote,
    version: 1,
    ...extra,
  };
}

/** Alice's comment `id`, the `at`th, on the words `quote` of the statement. */
function onStatement(id: string, at: number, quote: string, extra: Partial<Anchor> = {}): Comment {
  return { ...readComment(id, at), anchor: anchorOn(SPEC_DOCUMENT, STATEMENT, quote, extra) };
}

/** `comment` as the tree read answers it, under the node `parent` names, with the anchor the
 *  comments read gives it. */
function nodeOf(comment: Comment, parent: string, extra: TreeNodeExtra = {}): TreeAnswerNode {
  return treeNode(comment.id, "comment", parent, {
    ...(comment.anchor === null ? {} : { anchor: comment.anchor }),
    author: [comment.author],
    made_at: comment.created_at,
    text: comment.body,
    ...extra,
  });
}

/** The tree id of each row `root` draws. */
function rowIds(root: TreeNode): Set<string> {
  const ids = new Set<string>();
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      ids.add(child.id);
      walk(child);
    }
  };
  walk(root);
  return ids;
}

/** The threads of the seeded issue's answer `tree` with `comments` as its comments read, beside
 *  the rows it draws, or the nodes `drawn` names. */
function threadsOver(
  tree: TreeAnswer,
  comments: readonly Comment[],
  drawn?: ReadonlySet<string>
): ReadonlyMap<string, NodeThreads> {
  const reads: IssueTreeReads = { comments, issue: seededIssue(), tree };
  return textThreadsOf(tree, reads, drawn ?? rowIds(buildIssueTree(reads)));
}

/** The root of each thread `threads` holds for `node`, in their order. */
function keysAt(threads: ReadonlyMap<string, NodeThreads>, node: string): string[] | undefined {
  return threads.get(node)?.threads.map(({ thread }) => thread.key);
}

// R2: a thread stands by the node its anchor names, though the read hangs it under another node.
test("a thread stands by its anchor's node", () => {
  const tree = treeOf([...seededNodes([]), nodeOf(ON_STATEMENT.comment, DECISION)], SERVES);
  const threads = threadsOver(tree, [ON_STATEMENT.comment]);

  expect([...threads.keys()]).toEqual([ON_STATEMENT.node]);
  expect(keysAt(threads, ON_STATEMENT.node)).toEqual([ON_STATEMENT.comment.id]);
});

// R2 (G4): a thread rooted in a reply that quotes a block of a post stands by the post's row.
test.skipIf(!GATES.REPLY_QUOTE)("a reply thread stands by the item it quotes", () => {
  const post = readComment("post", 6);
  const block: QuotedReplyBlock = {
    block_id: "b1",
    node: "comment/post#b1",
    occurrence: 0,
    quote: "words",
    removed: false,
    text: post.body,
  };
  const quoted = { ...readComment("quoted", 7, "post"), reply_block: block };
  const tree = treeOf([...seededNodes([]), commentNode(post), commentNode(quoted)], SERVES);
  const threads = threadsOver(tree, [post, quoted]);

  expect(
    threads
      .get("post")
      ?.threads.map(({ quote, replyRoot, thread }) => [thread.key, replyRoot, quote])
  ).toEqual([["quoted", true, "words"]]);
});

// R2: an orphaned anchor that names a block a node stands for places its thread beside that
// node, though the read hangs the comment under the issue.
test("an orphaned thread stands by its anchor's block although the read hangs it under the issue", () => {
  const orphan = onStatement("orphan", 6, "draws", { orphaned: true });
  const tree = treeOf([...seededNodes([]), nodeOf(orphan, KEY)], SERVES);
  const threads = threadsOver(tree, [orphan]);

  expect(
    threads.get(ON_STATEMENT.node)?.threads.map(({ orphaned, thread }) => [thread.key, orphaned])
  ).toEqual([["orphan", true]]);
});

// R2: the orphaned thread the recorder seeded is anchored in a paragraph no node stands for and
// the read hangs it under the issue, so no node of the view holds its thread.
test("B0's recorded orphaned thread stands by no node", () => {
  const reads: IssueTreeReads = {
    comments: COMMENTS,
    issue: recordedIssue(),
    messages: MESSAGES,
    tree: CONVERSATION,
  };
  const threads = textThreadsOf(CONVERSATION, reads, rowIds(buildIssueTree(reads)));

  expect(
    [...threads].flatMap(([node, held]) =>
      held.threads.some(({ thread }) => thread.key === "orphanedThread") ? [node] : []
    )
  ).toEqual([]);
});

// R2: where the view does not draw the node the anchor names - Plan draws its plan tasks - the
// thread stands by the node the read hangs it under; where it draws both, by the anchor's.
test("a thread whose node the view does not draw stands by the read's parent", () => {
  const onOther = { ...readComment("on-o1", 6), anchor: anchorOn(PLAN_DOCUMENT, OTHER, "A note") };
  const tree = treeOf([...seededNodes([]), nodeOf(onOther, PLAN_TASK)], SERVES);
  const other = blockNodeId(PLAN_DOCUMENT.slug, OTHER);

  expect([...threadsOver(tree, [onOther], new Set([ON_PLAN_TASK.node])).keys()]).toEqual([
    ON_PLAN_TASK.node,
  ]);
  expect([...threadsOver(tree, [onOther], new Set([ON_PLAN_TASK.node, other])).keys()]).toEqual([
    other,
  ]);
});

// R2: a node's threads stand in the order of their words in its text; a thread whose words its
// node's text does not hold stands at the row's first line, first.
test("a row that does not draw the words places its thread at the row's first line", () => {
  const comments = [
    onStatement("on-plan", 1, "the plan"),
    onStatement("on-page", 2, "page"),
    onStatement("on-elsewhere", 3, "words no row draws"),
  ];
  const tree = treeOf(
    [...seededNodes([]), ...comments.map((comment) => nodeOf(comment, STATEMENT))],
    SERVES
  );

  expect(
    threadsOver(tree, comments)
      .get(ON_STATEMENT.node)
      ?.threads.map(({ firstLine, thread }) => [thread.key, firstLine])
  ).toEqual([
    ["on-elsewhere", true],
    ["on-page", false],
    ["on-plan", false],
  ]);
});

// R4: a thread whose words left its node's text stands at the node's first line, with the card's
// orphan notice, though the words it quoted stand in the text again.
test("an orphaned thread stands at its node's first line", () => {
  const comments = [
    onStatement("live", 1, "draws"),
    onStatement("orphaned", 2, "page", { orphaned: true }),
  ];
  const tree = treeOf(
    [...seededNodes([]), ...comments.map((comment) => nodeOf(comment, STATEMENT))],
    SERVES
  );

  expect(
    threadsOver(tree, comments)
      .get(ON_STATEMENT.node)
      ?.threads.map(({ firstLine, orphaned, thread }) => [
        thread.key,
        orphaned,
        firstLine,
        thread.anchor?.orphaned,
      ])
  ).toEqual([
    ["orphaned", true, true, true],
    ["live", false, false, false],
  ]);
});

// R2: a check the outline draws under its own statement and again under another it serves has
// its threads beside its row under its stored parent alone.
test("a check under two requirements has its threads beside its stored parent's row", () => {
  const second = treeNode("r2", "requirement", KEY, {
    ...inDocument(SPEC_DOCUMENT, 2),
    text: "The page draws the check.",
  });
  const tree = treeOf(
    [...seededNodes([]), second, nodeOf(ON_CHECK.comment, CHECK)],
    [...SERVES, treeTuple("serves", CHECK, "r2")]
  );
  const reads: IssueTreeReads = { comments: [ON_CHECK.comment], issue: seededIssue(), tree };
  const root = buildIssueTree(reads);
  const underParents: string[] = [];
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (child.id === ON_CHECK.node) underParents.push(node.id);
      walk(child);
    }
  };
  walk(root);

  expect(underParents, "the check is drawn under each statement").toEqual([
    ON_STATEMENT.node,
    blockNodeId(SPEC_DOCUMENT.slug, "r2"),
  ]);
  expect(
    [...textThreadsOf(tree, reads, rowIds(root))].map(([node, { parent, threads }]) => [
      node,
      parent,
      threads.map(({ thread }) => thread.key),
    ])
  ).toEqual([[ON_CHECK.node, ON_STATEMENT.node, [ON_CHECK.comment.id]]]);
});

// R4: a thread is one record: a reply written elsewhere is in the margin's thread once the
// comments read is read again, with no read of the margin's own.
test("a reply written elsewhere stands in the margin thread after the re-read", () => {
  const tree = treeOf([...seededNodes([]), nodeOf(ON_STATEMENT.comment, STATEMENT)], SERVES);
  const repliesOf = (comments: readonly Comment[]) =>
    threadsOver(tree, comments)
      .get(ON_STATEMENT.node)
      ?.threads.map(({ thread }) => thread.replies.map(({ id }) => id));

  expect(repliesOf([ON_STATEMENT.comment])).toEqual([[]]);
  expect(repliesOf([ON_STATEMENT.comment, REPLY_ON_STATEMENT])).toEqual([[REPLY_ON_STATEMENT.id]]);
});

// R7: each lens the read hangs under a comment of a thread stands under that comment in the
// thread, in the read's order, drawn as W drafts a lens row, with its finding and its risk under
// it; none of them is a row of the outline.
test("a lens on a thread comment stands under it in the read's order", () => {
  const reads: IssueTreeReads = {
    comments: COMMENTS,
    issue: recordedIssue(),
    messages: MESSAGES,
    tree: CONVERSATION,
  };
  const rows = rowIds(buildIssueTree(reads));
  const thread = textThreadsOf(CONVERSATION, reads, rows)
    .get(blockNodeId("spec", "r-s1"))
    ?.threads.find(({ thread: { key } }) => key === "threadsS1[0]");
  const lenses = thread?.lenses.get("threadsS1[0]") ?? [];

  expect(lenses.map(({ id, type }) => [id, type])).toEqual([
    ["triggerLens", "lens"],
    ["lens", "lens"],
  ]);
  expect(lenses[1]?.children.map(({ type }) => type)).toEqual(["finding", "risk"]);
  expect(["triggerLens", "lens"].filter((id) => rows.has(id))).toEqual([]);
});

// R7: a lens the read hangs under a block of a thread comment stands under that comment.
test("a lens on a block of a thread comment stands under that comment", () => {
  const block = treeNode("on-r1#b1", "comment_block", ON_STATEMENT.comment.id, {
    text: ON_STATEMENT.comment.body,
  });
  const lens = treeNode("lens-1", "lens", "on-r1#b1", {
    attributes: { verdict: "It holds." },
    author: [{ id: "alice", kind: "user" }],
  });
  const finding = treeNode("finding-1", "block", "lens-1", { text: "A finding." });
  const tree = treeOf(
    [...seededNodes([]), nodeOf(ON_STATEMENT.comment, STATEMENT), block, lens, finding],
    SERVES
  );
  const thread = threadsOver(tree, [ON_STATEMENT.comment]).get(ON_STATEMENT.node)?.threads[0];
  const lenses = thread?.lenses.get(ON_STATEMENT.comment.id) ?? [];

  expect(lenses.map(({ id, text, type }) => [id, text, type])).toEqual([
    ["lens-1", "It holds.", "lens"],
  ]);
  expect(lenses[0]?.children.map(({ type }) => type)).toEqual(["finding"]);
});

// R9: the answer the view holds decides the threads: while the comments page that holds a root
// is not loaded, its thread and its reply are drawn nowhere; once it loads, both are.
test("a thread is drawn once the comments page holding its root loads", () => {
  const comments = [ON_STATEMENT.comment, REPLY_ON_STATEMENT];
  const before = treeOf(seededNodes([]), SERVES);
  const after = treeOf([...seededNodes([]), nodeOf(ON_STATEMENT.comment, STATEMENT)], SERVES);

  expect(threadsOver(before, comments).size).toBe(0);
  expect(
    threadsOver(after, comments)
      .get(ON_STATEMENT.node)
      ?.threads.map(({ thread }) => [thread.key, thread.replies.map(({ id }) => id)])
  ).toEqual([[ON_STATEMENT.comment.id, [REPLY_ON_STATEMENT.id]]]);
});

// R9: a thread's node, anchor and words are the tree read's, and its body and resolution the
// comments read's, though the two disagree.
test("a thread takes its body from the comments read and its place from the tree read", () => {
  const inTree = nodeOf(ON_STATEMENT.comment, STATEMENT, { text: "The tree read's words." });
  const inRead: Comment = {
    ...ON_STATEMENT.comment,
    anchor: anchorOn(SPEC_DOCUMENT, DECISION, "decision d1"),
    body: "The comments read's words.",
    resolved: true,
  };
  const tree = treeOf([...seededNodes([]), inTree], SERVES);
  const threads = threadsOver(tree, [inRead]);

  expect([...threads.keys()]).toEqual([ON_STATEMENT.node]);
  expect(
    threads
      .get(ON_STATEMENT.node)
      ?.threads.map(({ quote, thread }) => [quote, thread.root.comment.body, thread.resolved])
  ).toEqual([[ON_STATEMENT.quote, "The comments read's words.", true]]);
});

// R9 (G3c): a thread on the text of another issue's node is drawn from the tree read's answer
// alone, with no read of that issue's comments.
test.skipIf(!GATES.OTHER_ISSUE_COMMENTS)(
  "a thread on another issue's node needs no read of that issue",
  () => {
    const document = { id: "artifact-other", slug: "spec" };
    const theirs = treeNode("x1", "requirement", KEY, {
      ...inDocument(document),
      issue: "CORE-2",
      text: "The other issue's words.",
    });
    const remark = treeNode("c-x", "comment", "x1", {
      anchor: anchorOn(document, "x1", "other issue"),
      author: [{ id: "alice", kind: "user" }],
      issue: "CORE-2",
      made_at: "2026-11-05T03:00:00Z",
      text: "A remark on the other issue's words.",
    });
    const tree = treeOf([...seededNodes([]), theirs, remark], SERVES);
    const threads = threadsOver(tree, [], new Set(["x1"]));

    expect(
      threads
        .get("x1")
        ?.threads.map(({ quote, thread }) => [thread.key, quote, thread.root.comment.body])
    ).toEqual([["c-x", "other issue", "A remark on the other issue's words."]]);
  }
);
