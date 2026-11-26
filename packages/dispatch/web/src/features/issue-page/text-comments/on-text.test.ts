import { expect, test } from "bun:test";

import type { Anchor, Comment, IssueDetails, MessageRead } from "../../../api/types";
import { withEntries } from "../../issue/decision-entries";
import {
  DECISIONS_DOCUMENT,
  decision,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
} from "../decisions-filter/decisions-fixtures";
import { buildIssueTree, type IssueTreeReads, type TreeNode } from "../issue-tree";
import { readComment } from "../outline-fixtures";
import { inDocument } from "../slots/stage-fixtures";
import { blockNodeId } from "../tree-drafts";
import { commentNode, treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import { treeIdOf } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { GATES } from "./gates";
import { recorded, recordedIssue, recordedReads } from "./margin-fixtures";
import {
  type OnText,
  onTextOf,
  onTextReads,
  type QuotedReplyBlock,
  type ReadMessage,
} from "./on-text";
import { textThreadsOf } from "./text-threads";

/**
 * R1's one rule of what is on text (`onTextOf`), over answers written as part T's cases write
 * them (`tree-read/tree-fixtures.ts`) and over the answers the recorder read from the real server
 * at part W's head (`fixtures/`, gate B0). Part T's `draftTurn` drafts no row for an item the rule
 * answers for, so a case reads the rows `buildIssueTree` draws from the same reads. A case owed on
 * a gate is skipped on its `gates.ts` line.
 */

/** The recorder's answers (`recorded`): the reads of Conversation's tree, of the comments and of
 *  the messages, and the read of Graph's tree. */
const { comments: COMMENTS, conversation: CONVERSATION, messages: MESSAGES } = recordedReads();
const GRAPH = recorded<{ read: TreeAnswer }>("tree-b0-graph.json").read;

/** The issue with its spec and its `decisions.md`. */
function issueWithDecisions(): IssueDetails {
  return { ...issueDetails(), artifacts: [SPEC_DOCUMENT, DECISIONS_DOCUMENT] };
}

/** The anchor of a comment on the words of `block` in `document`, as part T's cases write one. */
function anchorIn(
  document: { readonly id: string },
  block: string,
  extra: Partial<Anchor> = {}
): Anchor {
  return {
    artifact_id: document.id,
    block_id: block,
    mark_id: `mark-${block}`,
    orphaned: false,
    quote: `Words of ${block}.`,
    version: 1,
    ...extra,
  };
}

/** A statement of the spec, the block `block`, under the issue. */
function statement(block = "r1"): TreeAnswerNode {
  return treeNode(block, "requirement", KEY, {
    ...inDocument(SPEC_DOCUMENT),
    text: `Words of ${block}.`,
  });
}

/** The comment `id`, alice's, anchored at `anchor`. */
function anchoredComment(id: string, at: number, anchor: Anchor): Comment {
  return { ...readComment(id, at), anchor };
}

/** A block of a post that a reply answers, quoting `quote` where one is given (G4). */
function replyBlock(item: string, extra: Partial<QuotedReplyBlock> = {}): QuotedReplyBlock {
  return {
    block_id: "b1",
    node: `comment/${item}#b1`,
    removed: false,
    text: "Read the next line in order.",
    ...extra,
  };
}

/** A message of alice's, the `at`th, answering `inReplyTo`. */
function message(id: string, at: number, inReplyTo: string | null = null): ReadMessage {
  return {
    author: { id: "alice", kind: "user" },
    body: `The words of ${id}.`,
    broadcast_id: null,
    created_at: `2026-11-05T02:${String(at).padStart(2, "0")}:00Z`,
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id,
    in_reply_to: inReplyTo,
    issue_key: KEY,
    reply_block: null,
    target: null,
  };
}

/** What the rule answers for each item `ids` names, of the kind `supertag`, over the reads. */
function ruleOver(
  tree: TreeAnswer | undefined,
  reads: Pick<IssueTreeReads, "comments" | "messages">,
  ids: readonly string[],
  supertag = "comment"
): (OnText | null)[] {
  const held = onTextReads(tree, reads);
  return ids.map((id) => onTextOf({ id, supertag }, held));
}

/** The node each row of `root` stands under, by the row's id; the first placement of a node
 *  drawn twice. */
function rowsOf(root: TreeNode): Map<string, string> {
  const rows = new Map<string, string>();
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (!rows.has(child.id)) rows.set(child.id, node.id);
      walk(child);
    }
  };
  walk(root);
  return rows;
}

/** Every comment and message each thread of `answer` holds, by its id. */
function marginItems(answer: TreeAnswer, reads: IssueTreeReads, drawn: ReadonlySet<string>) {
  return new Set(
    [...textThreadsOf(answer, reads, drawn).values()].flatMap(({ threads }) =>
      threads.flatMap(({ thread }) => [thread.key, ...thread.replies.map(({ id }) => id)])
    )
  );
}

// R1: a comment whose anchor names a block of a document is the root of a thread on text,
// whether its words still stand in the block or not, and neither is a row: the orphaned one the
// read hangs under the issue, where the block it names is a node of the answer.
test("a comment anchored in a block is on text, orphaned or not", () => {
  const on = anchoredComment("on-r1", 1, anchorIn(SPEC_DOCUMENT, "r1"));
  const orphaned = anchoredComment(
    "orphaned-r1",
    2,
    anchorIn(SPEC_DOCUMENT, "r1", { orphaned: true })
  );
  const tree = treeAnswer([statement(), commentNode(on, "r1"), commentNode(orphaned, KEY)]);
  const comments = [on, orphaned];

  expect(ruleOver(tree, { comments }, ["on-r1", "orphaned-r1"])).toMatchObject([
    { kind: "root", place: { anchor: { block_id: "r1", orphaned: false }, kind: "block" } },
    { kind: "root", place: { anchor: { block_id: "r1", orphaned: true }, kind: "block" } },
  ]);
  const rows = rowsOf(buildIssueTree({ comments, issue: issueDetails(), tree }));
  expect(
    ["on-r1", "orphaned-r1"].filter((id) => rows.has(id)),
    "a comment on text is drawn as a row"
  ).toEqual([]);
});

// R1: a post on the issue, tied to no text, stays a row of Conversation with its replies.
test("a post on the issue is not on text", () => {
  const post = readComment("post", 1);
  const reply = readComment("post-reply", 2, "post");
  const tree = treeAnswer([commentNode(post), commentNode(reply)]);
  const comments = [post, reply];

  expect(ruleOver(tree, { comments }, ["post", "post-reply"])).toEqual([null, null]);
  const rows = rowsOf(buildIssueTree({ comments, issue: issueDetails(), tree }));
  expect([rows.get("post"), rows.get("post-reply")]).toEqual([KEY, "post"]);
});

// R1: a reply that names a block of the item it answers with no quote is a reply under that
// item, as today: of a post, a row under the post; of a comment on text, an entry of its thread.
test("a reply that names a block with no quote is a reply", () => {
  const post = readComment("post", 1);
  const toPost = { ...readComment("to-post", 2, "post"), reply_block: replyBlock("post") };
  const on = anchoredComment("on-r1", 3, anchorIn(SPEC_DOCUMENT, "r1"));
  const toOn = { ...readComment("to-on", 4, "on-r1"), reply_block: replyBlock("on-r1") };
  const tree = treeAnswer([
    statement(),
    commentNode(on, "r1"),
    commentNode(toOn),
    commentNode(post),
    commentNode(toPost),
  ]);
  const comments = [post, toPost, on, toOn];

  expect(ruleOver(tree, { comments }, ["to-post", "to-on"])).toEqual([
    null,
    { kind: "reply", root: "on-r1" },
  ]);
  const rows = rowsOf(buildIssueTree({ comments, issue: issueDetails(), tree }));
  expect([rows.get("to-post"), rows.has("to-on")]).toEqual(["post", false]);
});

// R1 (G4): a reply that quotes words of a block of an item that is in no thread on text, here a
// post, is the root of a thread on text, beside the item it quotes, and no row.
test.skipIf(!GATES.REPLY_QUOTE)("a reply with a quote of a row's item is on text", () => {
  const post = readComment("post", 1);
  const quoted = {
    ...readComment("quoted", 2, "post"),
    reply_block: replyBlock("post", { occurrence: 0, quote: "next line" }),
  };
  const tree = treeAnswer([commentNode(post), commentNode(quoted)]);
  const comments = [post, quoted];

  expect(ruleOver(tree, { comments }, ["post", "quoted"])).toMatchObject([
    null,
    { kind: "root", place: { item: "post", kind: "item", occurrence: 0, quote: "next line" } },
  ]);
  const rows = rowsOf(buildIssueTree({ comments, issue: issueDetails(), tree }));
  expect([rows.has("post"), rows.has("quoted")]).toEqual([true, false]);
});

// R1: a reply that quotes a block of a comment that is itself in a thread on text starts no
// thread of its own: it is an entry of that thread (the answer B0 gives too, which reads no
// quote).
test("a reply with a quote inside a thread on text stays in that thread", () => {
  const on = anchoredComment("on-r1", 1, anchorIn(SPEC_DOCUMENT, "r1"));
  const quoted = {
    ...readComment("quoted", 2, "on-r1"),
    reply_block: replyBlock("on-r1", { occurrence: 0, quote: "next line" }),
  };
  const tree = treeAnswer([statement(), commentNode(on, "r1"), commentNode(quoted)]);
  const comments = [on, quoted];

  expect(ruleOver(tree, { comments }, ["quoted"])).toEqual([{ kind: "reply", root: "on-r1" }]);
  expect(rowsOf(buildIssueTree({ comments, issue: issueDetails(), tree })).has("quoted")).toBe(
    false
  );
});

// R1 (G4): a comment whose `answers` chain reaches a reply that quotes a post is in that reply's
// thread, though the server keeps the post, the head of the post's thread, in its `reply_to`.
test.skipIf(!GATES.REPLY_QUOTE)(
  "a reply whose answers chain reaches a quoted reply is in its thread",
  () => {
    const post = readComment("post", 1);
    const quoted = {
      ...readComment("quoted", 2, "post"),
      reply_block: replyBlock("post", { occurrence: 0, quote: "next line" }),
    };
    const followup = { ...readComment("followup", 3, "post"), answers: "quoted" };
    const deeper = { ...readComment("deeper", 4, "post"), answers: "followup" };
    const tree = treeAnswer([
      commentNode(post),
      commentNode(quoted),
      commentNode(followup, "quoted"),
      commentNode(deeper, "followup"),
    ]);
    const comments = [post, quoted, followup, deeper];

    expect(ruleOver(tree, { comments }, ["followup", "deeper"])).toEqual([
      { kind: "reply", root: "quoted" },
      { kind: "reply", root: "quoted" },
    ]);
  }
);

// R1 (G5): a Change, a comment on a decision's words, is a comment on text like any other: here a
// Change of a section of `decisions.md`, which no node of the answer holds and the read hangs
// under the issue, is the root of a thread and no row under its decision.
test.skipIf(!GATES.COMPOSER)("a Change is on text", () => {
  const change = anchoredComment(
    "c-change",
    1,
    anchorIn(DECISIONS_DOCUMENT, "c-change-block", { quote: "Draw it once." })
  );
  const draw = decision("d2", {
    ref: `dispatch://${KEY}/artifact/decisions-md#b-d2`,
    text: "## Draw it\n\nDraw it once.",
  });
  const decisions = withEntries(
    { decisions: [draw], document: `dispatch://${KEY}/artifact/decisions-md`, open_questions: [] },
    KEY
  );
  const tree = treeAnswer([commentNode(change)]);

  expect(ruleOver(tree, { comments: [change] }, ["c-change"])).toMatchObject([
    { kind: "root", place: { anchor: { block_id: "c-change-block" }, kind: "block" } },
  ]);
  const rows = rowsOf(
    buildIssueTree({ comments: [change], decisions, issue: issueWithDecisions(), tree })
  );
  expect([rows.has(blockNodeId("decisions-md", "d2")), rows.has("c-change")]).toEqual([
    true,
    false,
  ]);
});

// R1 (G4): a message that answers a block of a message with a quote is the root of a thread on
// text; the message it quotes, in no thread on text, stays a row.
test.skipIf(!GATES.REPLY_QUOTE)("a message with a quote of a message's block is on text", () => {
  const first = message("first", 1);
  const quoted = {
    ...message("quoted", 2, "first"),
    reply_block: replyBlock("first", { occurrence: 0, quote: "next line" }),
  };
  const messages: MessageRead[] = [{ message: first, replies: [quoted] }];

  expect(ruleOver(undefined, { messages }, ["first", "quoted"], "message")).toMatchObject([
    null,
    { kind: "root", place: { item: "first", kind: "item", quote: "next line" } },
  ]);
});

// R1 (G4): a message whose `in_reply_to` chain reaches a message with a quote is in its thread.
test.skipIf(!GATES.REPLY_QUOTE)(
  "a message whose in_reply_to chain reaches a quoted message is in its thread",
  () => {
    const first = message("first", 1);
    const quoted = {
      ...message("quoted", 2, "first"),
      reply_block: replyBlock("first", { occurrence: 0, quote: "next line" }),
    };
    const answer = message("answer", 3, "quoted");
    const messages: MessageRead[] = [{ message: first, replies: [quoted, answer] }];

    expect(ruleOver(undefined, { messages }, ["answer"], "message")).toEqual([
      { kind: "reply", root: "quoted" },
    ]);
  }
);

// R1 (G2): a comment anchored on a node is the root of a thread on text: by the tree read's
// `anchor.node`, and by the comments read's `node_anchor` where the tree node carries none.
test.skipIf(!GATES.NODE_ANCHOR)("a comment anchored on a node is on text", () => {
  const onNode: Anchor & { readonly node: { readonly id: string }; readonly occurrence: number } = {
    ...anchorIn({ id: "" }, "", { block_id: null, quote: "holds" }),
    node: { id: "lens-1" },
    occurrence: 1,
  };
  const byTree = readComment("by-tree", 1);
  const byRead: Comment & { readonly node_anchor: unknown } = {
    ...readComment("by-read", 2),
    node_anchor: {
      node: { id: "lens-1", kind: "lens", state: "live" },
      occurrence: 0,
      orphaned: false,
      quote: "holds",
    },
  };
  const tree = treeAnswer([
    treeNode("lens-1", "lens", KEY, { attributes: { verdict: "It holds." } }),
    { ...commentNode(byTree, "lens-1"), anchor: onNode },
    commentNode(byRead, "lens-1"),
  ]);

  expect(ruleOver(tree, { comments: [byTree, byRead] }, ["by-tree", "by-read"])).toMatchObject([
    { kind: "root", place: { kind: "node", node: "lens-1", occurrence: 1, quote: "holds" } },
    { kind: "root", place: { kind: "node", node: "lens-1", occurrence: 0, quote: "holds" } },
  ]);
});

// R1 (G5): no anchor that names no drawn node makes a comment a row: a comment anchored in a
// block of a document that no node of the answer stands for, which the read hangs under the
// issue, is on text and no row.
test.skipIf(!GATES.COMPOSER)(
  "a comment whose anchor names no drawn node is on text and no row",
  () => {
    const unplaced = anchoredComment("c-unplaced", 1, anchorIn(DECISIONS_DOCUMENT, "d9"));
    const tree = treeAnswer([statement(), commentNode(unplaced)]);

    expect(ruleOver(tree, { comments: [unplaced] }, ["c-unplaced"])).toMatchObject([
      { kind: "root", place: { anchor: { block_id: "d9" }, kind: "block" } },
    ]);
    const rows = rowsOf(
      buildIssueTree({ comments: [unplaced], issue: issueWithDecisions(), tree })
    );
    expect(rows.has("c-unplaced")).toBe(false);
  }
);

// R4: a reply that quoted words of an item that was later deleted reads with no quote
// (INFRA-5703 leaves it out where `reply_block.deleted_at` is set), so it is no root: it is a
// reply row under that item's deleted row.
test("a reply whose quoted item was deleted is a reply row", () => {
  const post = {
    ...readComment("post", 1),
    deleted_at: "2026-11-05T05:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const reply = {
    ...readComment("reply", 2, "post"),
    reply_block: replyBlock("post", { deleted_at: "2026-11-05T05:00:00Z", text: "" }),
  };
  const tree = treeAnswer([commentNode(post), commentNode(reply)]);
  const comments = [post, reply];

  expect(ruleOver(tree, { comments }, ["reply"])).toEqual([null]);
  const root = buildIssueTree({ comments, issue: issueDetails(), tree });
  const rows = rowsOf(root);
  expect(rows.get("reply")).toBe("post");
  expect(root.children.find((child) => child.id === "post")?.deletedAt).toBe(post.deleted_at);
});

// R1: a chain of answers that runs in a circle ends where it comes round, as part T's rule did.
test("a chain of answers that runs in a circle ends where it comes round", () => {
  const one = readComment("one", 1, "two");
  const two = readComment("two", 2, "one");
  const tree = treeAnswer([commentNode(one, "two"), commentNode(two, "one")]);

  expect(ruleOver(tree, { comments: [one, two] }, ["one", "two"])).toEqual([null, null]);
});

// R1: the rule is one function that `draftTurn` and the margin both call, so over the reads the
// recorder read from the real server, no comment of Conversation's and Graph's answers is both a
// row of the outline and an entry of a thread in the margin, and a comment that is neither is one
// the rule puts on text (a thread whose node the view does not draw stands in no margin, R2).
test("each item the margin draws is an item no outline row draws", () => {
  for (const [view, answer] of [
    ["conversation", CONVERSATION],
    ["graph", GRAPH],
  ] as const) {
    const reads: IssueTreeReads = {
      comments: COMMENTS,
      issue: recordedIssue(),
      messages: MESSAGES,
      tree: answer,
    };
    const rows = new Set(rowsOf(buildIssueTree(reads)).keys());
    const margin = marginItems(answer, reads, rows);
    const rule = onTextReads(answer, reads);
    const comments = (answer.nodes ?? []).flatMap((node) =>
      node.supertag === "comment" ? [node.id] : []
    );

    expect(margin.size, `${view} draws no thread`).toBeGreaterThan(0);
    expect(
      [...margin].filter((id) => rows.has(id)),
      `${view} draws an item of a thread as a row`
    ).toEqual([]);
    expect(
      comments.filter(
        (id) =>
          !rows.has(id) && !margin.has(id) && onTextOf({ id, supertag: "comment" }, rule) === null
      ),
      `${view} leaves out a comment that is on no text`
    ).toEqual([]);
  }
});

// R1: the rule reads the tree read's `anchor`: with no comments read at all, each comment the
// recorded answers anchor stands in a thread beside the node the read hangs it under, the typed
// block whose paragraph its anchor names. The comments anchored in a block no node stands for,
// which the read hangs under the issue, stand in no margin.
test("the tree read's anchor places each comment on text", () => {
  for (const [view, answer] of [
    ["conversation", CONVERSATION],
    ["graph", GRAPH],
  ] as const) {
    const reads: IssueTreeReads = { issue: recordedIssue(), tree: answer };
    const rows = new Set(rowsOf(buildIssueTree(reads)).keys());
    const placed = new Map<string, string>();
    for (const [node, { threads }] of textThreadsOf(answer, reads, rows)) {
      for (const { thread } of threads) placed.set(thread.key, node);
    }
    const idOf = treeIdOf(answer, reads);
    const anchored = (answer.nodes ?? []).filter(
      (node) => node.supertag === "comment" && node.anchor !== undefined
    );
    const underNodes = anchored.filter((node) => node.parent?.supertag !== "issue");

    expect(underNodes.length, `${view} anchors no comment`).toBeGreaterThan(0);
    expect(new Map(underNodes.map(({ id }) => [id, placed.get(id)])), view).toEqual(
      new Map(underNodes.map(({ id, parent }) => [id, idOf(parent?.id ?? "")]))
    );
    expect(
      anchored.filter(({ id, parent }) => parent?.supertag === "issue" && placed.has(id)),
      `${view} draws a thread of a block no node stands for`
    ).toEqual([]);
  }
});

// R1: where the tree node carries no anchor - part T's cases write the anchor in the comments
// read alone - the rule reads the comments read's: a comment it anchors is a root, and the same
// comment with no comments read is none. The recorded Conversation, its nodes' anchors taken
// away, reads with the recorded comments read as it reads with its own anchors.
test("a tree node that carries no anchor is read by the comments read's anchor", () => {
  const on = anchoredComment("on-r1", 1, anchorIn(SPEC_DOCUMENT, "r1"));
  const tree = treeAnswer([statement(), commentNode(on, "r1")]);

  expect(ruleOver(tree, { comments: [on] }, ["on-r1"])).toMatchObject([
    { kind: "root", place: { anchor: { block_id: "r1" }, kind: "block" } },
  ]);
  expect(ruleOver(tree, {}, ["on-r1"])).toEqual([null]);

  const ids = (CONVERSATION.nodes ?? []).flatMap((node) =>
    node.supertag === "comment" ? [node.id] : []
  );
  const bare: TreeAnswer = {
    ...CONVERSATION,
    nodes: (CONVERSATION.nodes ?? []).map(({ anchor: _anchor, ...node }) => node),
  };
  const kinds = (answers: readonly (OnText | null)[]) =>
    Object.fromEntries(ids.map((id, index) => [id, answers[index]?.kind ?? null]));
  const byTree = kinds(ruleOver(CONVERSATION, {}, ids));

  expect(Object.values(byTree)).toContain("root");
  expect(kinds(ruleOver(bare, { comments: COMMENTS }, ids))).toEqual(byTree);
});
