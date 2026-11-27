import { beforeAll, expect, test } from "bun:test";

import { CONVERSATION_KINDS } from "../../../api/queries";
import type { Actor, IssueDetails } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { parseNodeReference } from "../../refs/routes";
import { artifact, issueDetails, KEY, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { artifactsBySlug } from "../issue-artifacts";
import {
  assemble,
  buildIssueTree,
  type IssueTreeReads,
  type TreeNode,
  treeWordsOf,
} from "../issue-tree";
import { blockNodeId } from "../tree-drafts";
import { pathTo } from "../tree-fold";
import {
  commentNode,
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
  treeTuple,
} from "../tree-read/tree-fixtures";
import { treeDraftsFrom } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import { viewTree } from "../useIssueTree";
import { roomTarget } from "./room-target";

/**
 * The model's rows from the tree read (`model-drafts.ts`), drafted as the page drafts them: each
 * case writes an answer of the tree read with the tree read's fixtures (`tree-fixtures.ts`), runs
 * `treeDraftsFrom` over it, whose steps hold the model's first, and builds the outline from the
 * drafts with `assemble`, as `buildIssueTree` does.
 */

/** The child issue whose checks and plan tasks the issue borrows. */
const CHILD = "CORE-2";
/** The issue's tree artifact, which the tree read names as `tree` once it has converted. */
const TREE = "artifact-tree";

const BOB: Actor = { id: "bob", kind: "user" };
const AGENT: Actor = { id: "session-planner", kind: "session" };

const VERIFICATION = artifact("artifact-verification", "verification-md");

/** The issue, with its spec and its Verification document. */
function issue(): IssueDetails {
  return {
    ...issueDetails(),
    artifacts: [SPEC_DOCUMENT, VERIFICATION],
    verification_artifact_id: VERIFICATION.id,
  };
}

// A chip is named from its reading through the shared projection, which loads its engine once.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The issue's own node, the root every other node hangs from. */
const ISSUE_NODE = treeNode(KEY, "issue", null, { text: "The issue page draws one ordered body" });

/** One answer of the tree read of a converted issue: the issue's node, then `nodes`. */
function answer(nodes: readonly TreeAnswerNode[], tuples: readonly TreeTuple[] = []): TreeAnswer {
  return treeAnswer([ISSUE_NODE, ...nodes], tuples, { converted: true, tree: TREE });
}

/** The drafts `tree` holds and the outline `assemble` builds from them, drafted as the answer of
 *  a read that asked for `kinds`, of every kind where none is given, as Graph's read is. */
function drawn(tree: TreeAnswer, kinds?: readonly string[]) {
  const reads: IssueTreeReads = { issue: issue(), ...(kinds === undefined ? {} : { kinds }) };
  const context = treeWordsOf(reads);
  const drafts = treeDraftsFrom(tree, reads, context);
  return { drafts, root: assemble(reads.issue, [...drafts.values()], context.chipOf) };
}

/** Every placement of `id`, with the node each one sits under. */
function placementsOf(root: TreeNode, id: string): { parent: string; node: TreeNode }[] {
  return root.children.flatMap((child) => [
    ...(child.id === id ? [{ node: child, parent: root.id }] : []),
    ...placementsOf(child, id),
  ]);
}

/** Every node of the tree, the root first, each before its children. */
function nodesOf(root: TreeNode): TreeNode[] {
  return [root, ...root.children.flatMap(nodesOf)];
}

/** A statement of the issue's spec, at `position` in it. */
function statement(id: string, position: number): TreeAnswerNode {
  return treeNode(id, "requirement", KEY, {
    document: { artifact_id: SPEC_DOCUMENT.id, position, slug: SPEC_DOCUMENT.slug },
    text: `Words of ${id}.`,
  });
}

/** The child issue's node, under the issue. */
const CHILD_NODE = treeNode(CHILD, "issue", KEY, { text: "A child of the issue" });

/** A check of the child's Verification document, which carries the parent's slug, under the
 *  child's node, its words summing to `words` where the read answers that. */
function childCheck(id: string, extra: TreeNodeExtra = {}): TreeAnswerNode {
  return treeNode(id, "check", CHILD, {
    document: { artifact_id: "artifact-child-verification", position: 0, slug: VERIFICATION.slug },
    issue: CHILD,
    text: `Words of ${id}.`,
    ...extra,
  });
}

/** The words the child's check reads now, and the words it read before they moved. */
const WORDS_NOW = "sum-of-the-words-now";
const WORDS_BEFORE = "sum-of-the-words-before";

/** A result of the child's check `check`, recorded by the agent at `madeAt` as `verdict` against
 *  the check's words summing to `words`, as the read answers a result node's `result`. */
function childResult(
  id: string,
  check: string,
  verdict: "fail" | "pass",
  madeAt: string,
  words: string = WORDS_NOW,
  retracted = false
): TreeAnswerNode {
  return treeNode(id, "result", check, {
    attributes: { commit: "abc123", failed: verdict === "fail" ? "1" : "0", surface: "unit" },
    author: [AGENT],
    issue: CHILD,
    made_at: madeAt,
    result: {
      check_sha256: words,
      check_version: 1,
      verdict,
      ...(retracted
        ? { retracted: { actor: BOB, created_at: madeAt, reason: "Ran on the wrong branch." } }
        : {}),
    },
  });
}

// R1: a criterion is drafted from the tree read under the conversation node the read names as
// its parent, with its author and its time, a question and a comment alike.
test("a criterion stands under the conversation node it was written in", () => {
  const { drafts, root } = drawn(
    answer([
      commentNode({
        author: BOB,
        body: "Can I see which checks a child holds?",
        created_at: "2026-11-05T09:00:00Z",
        id: "c-1",
        reply_to: null,
      }),
      treeNode("q1", "ask", KEY, { author: [BOB], text: "Which child holds it?" }),
      treeNode("k1", "criterion", "c-1", {
        author: [BOB],
        made_at: "2026-11-05T10:00:00Z",
        text: "I can see a child's checks under my statements.",
      }),
      treeNode("k2", "criterion", "q1", { author: [BOB], made_at: "2026-11-05T11:00:00Z" }),
    ])
  );

  expect(
    ["k1", "k2"].map((id) => placementsOf(root, id).map(({ node, parent }) => [parent, node.type]))
  ).toEqual([[["c-1", "criterion"]], [["q1", "criterion"]]]);
  expect([drafts.get("k1")?.author, drafts.get("k1")?.at]).toEqual([BOB, "2026-11-05T10:00:00Z"]);
  expect(drafts.get("k2")?.author).toEqual(BOB);
});

// R1: a statement the read places under a criterion stands under it, so the path to the
// statement passes the criterion and then the comment it was written under.
test("a statement under a criterion walks up to it", () => {
  const { root } = drawn(
    answer([
      commentNode({
        author: BOB,
        body: "Can I see which checks a child holds?",
        created_at: "2026-11-05T09:00:00Z",
        id: "c-1",
        reply_to: null,
      }),
      treeNode("k1", "criterion", "c-1", { author: [BOB] }),
      treeNode("r9", "requirement", "k1", {
        document: { artifact_id: SPEC_DOCUMENT.id, position: 0, slug: SPEC_DOCUMENT.slug },
      }),
    ])
  );

  expect(pathTo(root, blockNodeId("spec", "r9"))?.map((one) => one.id)).toEqual([
    KEY,
    "c-1",
    "k1",
    blockNodeId("spec", "r9"),
  ]);
});

// R3: a reading stands under the decision or the answered question it reads, which the read
// gives it as its parent, with the agent that wrote it and when.
test("a reading stands under its decision or its answered ask", () => {
  const { drafts, root } = drawn(
    answer([
      treeNode("d1", "decision", KEY, { in: SPEC_DOCUMENT.slug, text: "Keep one room." }),
      treeNode("q1", "ask", KEY, { author: [BOB], decision: {}, text: "One room or two?" }),
      treeNode("rd-decision", "reading", "d1", {
        author: [AGENT],
        in: SPEC_DOCUMENT.slug,
        made_at: "2026-11-05T12:00:00Z",
      }),
      treeNode("rd-question", "reading", "q1", {
        author: [AGENT],
        made_at: "2026-11-05T13:00:00Z",
      }),
    ])
  );

  expect(
    ["rd-decision", "rd-question"].map((id) => [
      placementsOf(root, id).map(({ parent }) => parent),
      drafts.get(id)?.type,
      drafts.get(id)?.author,
      drafts.get(id)?.at,
    ])
  ).toEqual([
    [["d1"], "reading", AGENT, "2026-11-05T12:00:00Z"],
    [["q1"], "reading", AGENT, "2026-11-05T13:00:00Z"],
  ]);
});

// R4: a child's check that serves two statements of the issue is one node under each, by the
// read's id, each placement naming the other, addressed in the child, and nothing of it stands
// under the child issue's own row; its result stands under it, with who recorded it and when.
test("a borrowed check is one node under each statement it serves, with its issue's key", () => {
  const { drafts, root } = drawn(
    answer(
      [
        statement("r1", 0),
        statement("r2", 1),
        CHILD_NODE,
        childCheck("c-child"),
        childResult("res-1", "c-child", "pass", "2026-11-05T14:00:00Z"),
      ],
      [treeTuple("serves", "c-child", "r2"), treeTuple("serves", "c-child", "r1")]
    )
  );
  const [r1, r2] = [blockNodeId("spec", "r1"), blockNodeId("spec", "r2")];
  const placements = placementsOf(root, "c-child");

  expect(drafts.get("c-child")?.parents).toEqual([r2, r1]);
  expect(placements.map(({ parent }) => parent)).toEqual([r1, r2]);
  expect(placements.map(({ node }) => node.alsoUnder.map((chip) => chip.ref))).toEqual([
    [`dispatch://${KEY}/artifact/spec#b-r2`],
    [`dispatch://${KEY}/artifact/spec#b-r1`],
  ]);
  const reference = drafts.get("c-child")?.reference ?? "";
  expect(reference).toBe(`dispatch://${CHILD}/artifact/verification-md#b-c-child`);
  expect(parseNodeReference(reference)).toMatchObject({ key: CHILD });
  expect(placements.map(({ node }) => node.children.map((child) => child.id))).toEqual([
    ["res-1"],
    ["res-1"],
  ]);
  expect([drafts.get("res-1")?.author, drafts.get("res-1")?.at]).toEqual([
    AGENT,
    "2026-11-05T14:00:00Z",
  ]);
  expect(placementsOf(root, CHILD).map(({ node }) => nodesOf(node).map((one) => one.id))).toEqual([
    [CHILD],
  ]);
});

/** How the child's check stands where the read answers `results` under it, its own words'
 *  sum answered as `check` unless `check` is `false`, the read asking for `kinds`, every kind
 *  where none is given. */
function standingOf(results: readonly TreeAnswerNode[], check = true, kinds?: readonly string[]) {
  const tree = answer(
    [
      statement("r1", 0),
      CHILD_NODE,
      childCheck("c-child", check ? { check: { changed_version: 2, text_sha256: WORDS_NOW } } : {}),
      ...results,
    ],
    [treeTuple("serves", "c-child", "r1")]
  );
  return drawn(tree, kinds).drafts.get("c-child")?.state;
}

// R4, how a borrowed check stands in a read of every kind, as Graph's is: the read answers no
// results read for another issue, so the check stands by the one shared reading of a check
// (`checkStanding`) over the result nodes the read answers under it, from the words' sum the read
// answers on the check and the `result` on each result node - the newest result nobody withdrew
// over the check's words now decides; where none is, one over moved words reads out of date,
// every result withdrawn retracted, and none at all no result. Without that sum or a node's
// `result` the check draws no standing word, since its standing cannot be shown current.
test("a borrowed check's standing comes from its results", () => {
  const passThenFail = [
    childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z"),
    childResult("res-2", "c-child", "fail", "2026-11-05T11:00:00Z"),
  ];
  const failThenPass = [
    childResult("res-1", "c-child", "fail", "2026-11-05T10:00:00Z"),
    childResult("res-2", "c-child", "pass", "2026-11-05T11:00:00Z"),
  ];
  const movedUnder = [
    childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z", WORDS_BEFORE),
    childResult("res-2", "c-child", "pass", "2026-11-05T11:00:00Z", WORDS_NOW, true),
  ];
  const allRetracted = [
    childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z", WORDS_NOW, true),
    childResult("res-2", "c-child", "fail", "2026-11-05T11:00:00Z", WORDS_BEFORE, true),
  ];
  const unanswered = { ...childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z") };
  delete unanswered.result;

  expect({
    "a later fail over the same words": standingOf(passThenFail),
    "a later pass over the same words": standingOf(failThenPass),
    "none over the words now": standingOf(movedUnder),
    "every result withdrawn": standingOf(allRetracted),
    "no result": standingOf([]),
    "no words' sum on the check": standingOf(passThenFail, false),
    "no result member on a result": standingOf([unanswered]),
  }).toEqual({
    "a later fail over the same words": "failed",
    "a later pass over the same words": "passed",
    "none over the words now": "older",
    "every result withdrawn": "retracted",
    "no result": "no-result",
    "no words' sum on the check": undefined,
    "no result member on a result": undefined,
  });
});

// R4: Conversation's read names its kinds (`CONVERSATION_KINDS`), `result` none of them, so its
// answer holds no result node of any check, whatever results the check has: a borrowed check it
// answers with its words' sum and no result node draws no standing, where the same answer read as
// a read of every kind reads no result.
test("a borrowed check of Conversation's read, which names no result, draws no standing", () => {
  expect({
    "Conversation's read": standingOf([], true, CONVERSATION_KINDS),
    "a read of every kind": standingOf([]),
  }).toEqual({ "Conversation's read": undefined, "a read of every kind": "no-result" });
});

// R4, each tree as `useIssueTree.ts` builds it: the outline's from the reads it sets the kinds
// its read named on (`IssueTreeReads.kinds`), Conversation's `CONVERSATION_KINDS` and Graph's
// none, and a view's by `viewTree`, which drafts the view's answer, over Conversation's answer as
// its context, as the view's own read asked. So the child's check draws no standing in
// Conversation's tree, and in Graph's tree and the Verification view's it stands as their result
// nodes say: no result where they hold none, passed where they hold a pass. The Spec view's read
// names no result either, so its tree draws the check, from Conversation's context, no standing.
test("each tree the page builds reads a borrowed check's standing by what its read named", () => {
  const check = childCheck("c-child", { check: { changed_version: 2, text_sha256: WORDS_NOW } });
  const pass = childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z");
  const serves = [treeTuple("serves", "c-child", "r1")];
  const outline = answer([statement("r1", 0), CHILD_NODE, check], serves);
  const view = (name: string, nodes: readonly TreeAnswerNode[]) =>
    treeAnswer([ISSUE_NODE, ...nodes], name === "verification" ? serves : [], {
      converted: true,
      tree: TREE,
      view: name,
    });
  const conversation: IssueTreeReads = {
    issue: issue(),
    kinds: CONVERSATION_KINDS,
    tree: outline,
  };
  const stateIn = (root: TreeNode) =>
    nodesOf(root).find((node) => node.id === "c-child" && node.type === "check")?.state;

  expect({
    conversation: stateIn(buildIssueTree(conversation)),
    graph: stateIn(buildIssueTree({ issue: issue(), tree: outline })),
    "graph, with a pass": stateIn(
      buildIssueTree({
        issue: issue(),
        tree: answer([statement("r1", 0), CHILD_NODE, check, pass], serves),
      })
    ),
    spec: stateIn(viewTree(conversation, view("spec", [statement("r1", 0)]))),
    verification: stateIn(
      viewTree(conversation, view("verification", [statement("r1", 0), check]))
    ),
    "verification, with a pass": stateIn(
      viewTree(conversation, view("verification", [statement("r1", 0), check, pass]))
    ),
  }).toEqual({
    conversation: undefined,
    graph: "no-result",
    "graph, with a pass": "passed",
    spec: undefined,
    verification: "no-result",
    "verification, with a pass": "passed",
  });
});

// R4: each node the model drafts of the child - its check, its result, its plan task - carries the
// child's key (`Draft.issue`, `TreeNode.issue`), so the check is a check of the page whatever its
// standing (`isBorrowedCheck`): in Conversation's tree, whose read names no result and no plan
// task and so holds the check alone with no state, and in Graph's, which names every kind.
test("the child's check, result and plan task carry the child's key in Conversation's tree and in Graph's", () => {
  const check = childCheck("c-child", { check: { changed_version: 2, text_sha256: WORDS_NOW } });
  const pass = childResult("res-1", "c-child", "pass", "2026-11-05T10:00:00Z");
  const task = treeNode("t-child", "plan_task", CHILD, {
    document: { artifact_id: "artifact-child-plan", position: 0, slug: "plan-md" },
    issue: CHILD,
  });
  const serves = [treeTuple("serves", "c-child", "r1"), treeTuple("serves", "t-child", "r1")];
  const issuesIn = (root: TreeNode) =>
    nodesOf(root).flatMap((node) =>
      ["c-child", "res-1", "t-child"].includes(node.id) ? [[node.id, node.type, node.issue]] : []
    );

  expect({
    conversation: issuesIn(
      buildIssueTree({
        issue: issue(),
        kinds: CONVERSATION_KINDS,
        tree: answer([statement("r1", 0), CHILD_NODE, check], serves),
      })
    ),
    graph: issuesIn(
      buildIssueTree({
        issue: issue(),
        tree: answer([statement("r1", 0), CHILD_NODE, check, pass, task], serves),
      })
    ),
  }).toEqual({
    conversation: [["c-child", "check", CHILD]],
    graph: [
      ["c-child", "check", CHILD],
      ["res-1", "result", CHILD],
      ["t-child", "plan_task", CHILD],
    ],
  });
});

// R4: a statement counts a child's check that serves it among the nodes it holds, each
// statement it serves alike; the same check serving neither counts for neither.
test("a borrowed check counts for each statement it serves", () => {
  const nodes = [
    statement("r1", 0),
    statement("r2", 1),
    CHILD_NODE,
    childCheck("c-child"),
    childResult("res-1", "c-child", "pass", "2026-11-05T14:00:00Z"),
  ];
  const held = (tuples: readonly TreeTuple[]) => {
    const { root } = drawn(answer(nodes, tuples));
    return ["r1", "r2"].map((id) => placementsOf(root, blockNodeId("spec", id))[0]?.node.held);
  };

  expect(
    held([treeTuple("serves", "c-child", "r1"), treeTuple("serves", "c-child", "r2")])
  ).toEqual([2, 2]);
  // The negative control: the check serves neither, and neither counts it.
  expect(held([])).toEqual([0, 0]);
});

// R4: a child's check whose `serves` names no statement of this issue - only the child's own
// statement - is drawn nowhere, and neither is its result.
test("a borrowed check that serves no statement of this issue is drawn nowhere", () => {
  const { drafts, root } = drawn(
    answer(
      [
        statement("r1", 0),
        CHILD_NODE,
        treeNode("r-child", "requirement", CHILD, {
          document: { artifact_id: "artifact-child-spec", position: 0, slug: SPEC_DOCUMENT.slug },
          issue: CHILD,
        }),
        childCheck("c-child"),
        childResult("res-1", "c-child", "pass", "2026-11-05T14:00:00Z"),
      ],
      [treeTuple("serves", "c-child", "r-child")]
    )
  );

  expect(["c-child", "res-1"].map((id) => drafts.has(id))).toEqual([false, false]);
  expect(
    nodesOf(root)
      .map((one) => one.id)
      .filter((id) => id === "c-child" || id === "res-1")
  ).toEqual([]);
});

// R4: the child's Verification document carries the parent's slug, so a child's check of the
// same block id as this issue's check keeps the read's id, and each stands under the statement
// its own `serves` names.
test("a child's check named like this issue's keeps its own id", () => {
  const { drafts, root } = drawn(
    answer(
      [
        statement("r1", 0),
        treeNode("c1", "check", "r1", {
          document: { artifact_id: VERIFICATION.id, position: 0, slug: VERIFICATION.slug },
        }),
        statement("r2", 1),
        CHILD_NODE,
        childCheck("c1"),
      ],
      [
        treeTuple("serves", `${KEY}/verification-md#c1`, "r1"),
        treeTuple("serves", `${CHILD}/verification-md#c1`, "r2"),
      ]
    )
  );

  expect(
    [blockNodeId("verification-md", "c1"), "c1"].map((id) => [
      drafts.get(id)?.readId,
      placementsOf(root, id).map(({ parent }) => parent),
    ])
  ).toEqual([
    [`${KEY}/verification-md#c1`, [blockNodeId("spec", "r1")]],
    [`${CHILD}/verification-md#c1`, [blockNodeId("spec", "r2")]],
  ]);
});

// R1, R5: a criterion written into the issue names no document, so the tree draws it by its
// block id, the read's id with nothing around it, and gives it no address; the room edits it in
// the tree artifact at that id.
test("a criterion's draft carries no address and its bare read id", () => {
  const { drafts, root } = drawn(
    answer([
      commentNode({
        author: BOB,
        body: "Can I see which checks a child holds?",
        created_at: "2026-11-05T09:00:00Z",
        id: "c-1",
        reply_to: null,
      }),
      treeNode("k1", "criterion", "c-1", { author: [BOB] }),
    ])
  );
  const draft = drafts.get("k1");
  const row = placementsOf(root, "k1")[0]?.node;

  expect([draft?.id, draft?.readId, draft?.reference]).toEqual(["k1", `${KEY}/tree#k1`, undefined]);
  expect(
    row === undefined
      ? undefined
      : roomTarget(row, artifactsBySlug(issue()), {
          converted: true,
          issueKey: KEY,
          tree: TREE,
          viewOf: null,
        })
  ).toEqual({ artifactId: TREE, blockId: "k1" });
});
