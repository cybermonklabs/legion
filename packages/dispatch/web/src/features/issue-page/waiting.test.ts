import { expect, test } from "bun:test";

import type {
  Artifact,
  Ask,
  Comment,
  Decision,
  DecisionKeep,
  IssueCoverage,
  IssueDetails,
} from "../../api/types";
import { decisionNodes } from "./decision-nodes/decision-nodes";
import { buildIssueTree, isStatement, type TreeNode } from "./issue-tree";
import { blockNodeId, coverageStatements } from "./tree-drafts";
import { nodesInOrder } from "./tree-keys";
import {
  commentNode,
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
  treeTuple,
} from "./tree-read/tree-fixtures";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";
import { waitingOnReader } from "./waiting";

/**
 * What waits on the reader, read two ways from the same reads: the header's count
 * (`waitingOnReader`) and the tree's own `waiting` on each node. The two must agree, or the
 * count puts the focus on a node that does not wait, or counts one it can never reach. Both read
 * the tree read's answer, which each case writes as the contract gives it, and its decisions are
 * that answer's decision nodes (`decisionNodes`).
 */

const KEY = "CORE-1";

/** One open ask as the issue read carries it. */
type OpenAsk = NonNullable<IssueDetails["open_asks"]>[number];

function issueDocument(id: string, slug: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name: slug,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

const spec = issueDocument("artifact-spec", "spec", true);

/** One open ask of the issue, waiting on the person `person`. */
function openAsk(id: string, kind: "approval" | "question", person: string): OpenAsk {
  return {
    anchor: null,
    answer: null,
    artifact_id: kind === "approval" ? spec.id : null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author: { id: "planner", kind: "session" },
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind,
    last_reply: null,
    multiple: false,
    options: [],
    question: `Ask ${id}?`,
    state: "open",
    urgency: "med",
    waiting_on: "human",
    waiting_on_person: person,
  };
}

const issue: IssueDetails = {
  advances: [],
  advances_revision: 0,
  artifacts: [spec],
  assignee: "alice",
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  design_gates: [],
  external_links: [],
  holder_state: null,
  key: KEY,
  labels: [],
  last_seen: null,
  last_seq: 1,
  number: 1,
  open_asks: [
    openAsk("mine", "question", "alice"),
    openAsk("theirs", "question", "bob"),
    openAsk("approve-spec", "approval", "alice"),
  ],
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: spec.id,
  priority: null,
  progress: { children: null, tasks: null },
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  takeable_at: null,
  title: "The issue page draws one ordered body",
  updated_at: "2026-11-05T00:00:00Z",
  verification_artifact_id: "",
};

/** The issue with no open ask, so a case's count is its decisions' alone. */
const quiet: IssueDetails = { ...issue, open_asks: [] };

const coverage: IssueCoverage = {
  gaps: [],
  has_plan: false,
  issue: KEY,
  requirements: [
    {
      block_id: "r1",
      checks: 1,
      gaps: [],
      plan_tasks: 0,
      ref: `dispatch://${KEY}/spec#b-r1`,
      text: "The first.",
    },
  ],
  spec_artifact_id: spec.id,
};

/** An agent's decision that waits for a person's review, tied to the statements `shaped`. */
function waitingDecision(blockId: string, shaped: readonly string[]): Decision {
  return {
    asks: [],
    block_id: blockId,
    changed_at: "2026-11-05T10:00:00Z",
    changed_version: 2,
    heading: `Decision ${blockId}`,
    kept: null,
    kind: "agent",
    links: shaped.map((one) => ({
      changed_since_tie: false,
      created_at: "2026-11-05T00:00:00Z",
      created_version: 1,
      from_block: blockId,
      stale: false,
      to: {
        block_id: one,
        changed_at: null,
        changed_by: null,
        id: spec.id,
        kind: "block",
        made_at: null,
        made_by: null,
        parent: null,
        parent_version: null,
        ref: `dispatch://${KEY}/spec#b-${one}`,
      },
    })),
    ref: `dispatch://${KEY}/artifact/decisions-md#b-${blockId}`,
    replaced: false,
    text: `## Decision ${blockId}\n\nThe words of ${blockId}.`,
    text_sha256: "sum",
    unsure: false,
    waiting_review: true,
  };
}

/** A person's comment on the decision `changed`, anchored to its words, after they last
 *  changed. */
const change: Comment = {
  anchor: {
    artifact_id: spec.id,
    block_id: "changed",
    mark_id: "change-1",
    orphaned: false,
    quote: "The words of changed.",
    version: 3,
  },
  answers: null,
  artifact_id: spec.id,
  ask_id: null,
  author: { id: "alice", kind: "user" },
  body: "Say which pages this covers.",
  created_at: "2026-11-05T11:00:00Z",
  deleted_at: null,
  deleted_by: null,
  deliveries: [],
  edited_at: null,
  id: "change-1",
  issue_key: KEY,
  mentions: [],
  reply_block: null,
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
};

/** One node of the tree read's answer, as the route sends it (`treeNode`), under the issue unless
 *  the case gives it another parent, with nothing on it but what the case gives it. */
function answerNode(
  id: string,
  supertag: string,
  extra: TreeNodeExtra = {},
  parent: string = KEY
): TreeAnswerNode {
  return treeNode(id, supertag, parent, extra);
}

/** The tree read's answer for the issue: its own node, then `nodes` in the read's order. */
function answerOf(nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return treeAnswer([treeNode(KEY, "issue", null), ...nodes]);
}

/** The spec's statement `r1` as the tree read answers it, a typed block of the spec. */
const statement = answerNode("r1", "requirement", {
  document: { artifact_id: spec.id, position: 0, slug: "spec" },
  text: "The first.",
});

/** The issue's open asks as the tree read answers them, each under the issue. */
const askNodes = (issue.open_asks ?? []).map((ask) =>
  answerNode(ask.id, "ask", { author: [ask.author], made_at: ask.created_at, text: ask.question })
);

/** An agent's decision as the tree read answers its node: a decision block of the spec, carrying
 *  the model's decision (`decision`, `waitingDecision`'s members) and the words
 *  `The words of <id>.`, unless the case gives it others. */
function decisionNode(
  id: string,
  shaped: readonly string[],
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  const { decision, ...rest } = extra;
  const row = waitingDecision(id, shaped);
  return answerNode(id, "decision", {
    document: { artifact_id: spec.id, position: 1, slug: "spec" },
    text: `The words of ${id}.`,
    ...rest,
    decision: {
      asks: row.asks,
      changed_at: row.changed_at,
      changed_version: row.changed_version,
      kept: row.kept,
      kind: row.kind,
      links: row.links,
      replaced: row.replaced,
      text_sha256: row.text_sha256,
      waiting_review: row.waiting_review,
      ...decision,
    },
  });
}

/** `node`, a decision node, with the model's decision the read answers on it changed by
 *  `members`. */
function withDecision(node: TreeAnswerNode, members: Partial<Decision>): TreeAnswerNode {
  return node.decision === undefined
    ? node
    : { ...node, decision: { ...node.decision, ...members } };
}

/** Which addresses name a statement of the issue, as the header's count is handed them: by the
 *  one rule the tree's review list reads (`coverageStatements`). */
const isCoverageStatement = coverageStatements(issue, coverage);

/** The issue's plan, whose `requirement` blocks no coverage row holds, and the issue with it. */
const plan = issueDocument("artifact-plan", "plan-md");
const planned: IssueDetails = {
  ...quiet,
  artifacts: [spec, plan],
  plan_artifact_id: plan.id,
  queries: [
    {
      query: "plan",
      artifact_id: plan.id,
      takes: ["plan_task", "plan_role"],
      subtree: `issue:${KEY}`,
    },
  ],
};

/** The decisions of `tree`: its decision nodes (`decisionNodes`). */
function decisionsOf(tree: TreeAnswer) {
  return decisionNodes(tree, { issue });
}

/** How many decisions wait on alice over `tree`, in the count, and the id of each node of the
 *  tree that waits on her, with the statements `read` holds (`coverageStatements`): none while it
 *  has not answered. */
function waits(
  tree: TreeAnswer,
  read: IssueCoverage | undefined,
  about: IssueDetails = quiet
): readonly [number, readonly string[]] {
  return [
    waitingOnReader(about, "alice", coverageStatements(about, read), { tree }).decisions,
    waitingIn(
      buildIssueTree({
        ...(read === undefined ? {} : { coverage: read }),
        issue: about,
        tree,
        viewer: "alice",
      })
    ),
  ];
}

/** The id of every node of `root` that waits on the reader. */
function waitingIn(root: TreeNode): string[] {
  return [
    ...(root.waiting ? [root.id] : []),
    ...root.children.flatMap((child) => waitingIn(child)),
  ];
}

/** The id of each node of `root` that a placement of the node `id` sits under. */
function parentsOf(root: TreeNode, id: string): string[] {
  return root.children.flatMap((child) => [
    ...(child.id === id ? [root.id] : []),
    ...parentsOf(child, id),
  ]);
}

// R7, R13, R35: what waits on the reader is each open question whose
// turn is theirs, each decision of the review list - an agent's decision that waits, that a tie
// names a statement for, and that the reader has not asked to change since it changed - and each
// open approval request whose turn is theirs. The tree marks the same question and the same
// decision; an approval request is no node of it. While the login is not known, nothing waits.
test("what waits on the reader is their open questions, the review list and their approval requests", () => {
  const tree = answerOf([
    statement,
    ...askNodes,
    decisionNode("listed", ["r1"]),
    decisionNode("untied", []),
    decisionNode("changed", ["r1"]),
    commentNode(change, "changed"),
  ]);
  const counted = waitingOnReader(issue, "alice", isCoverageStatement, { tree });
  expect(counted).toEqual({ approvals: [spec.id], asks: 1, decisions: 1, total: 3 });
  expect(waitingIn(buildIssueTree({ coverage, issue, tree, viewer: "alice" }))).toEqual([
    "mine",
    "spec#b-listed",
  ]);

  const unknown = waitingOnReader(issue, undefined, isCoverageStatement, { tree });
  expect(unknown.total).toBe(0);
  expect(waitingIn(buildIssueTree({ coverage, issue, tree }))).toEqual([]);
});

// R7 and R13, and R3 of dispatch://INFRA-1523: a Change its author deleted asks nothing any
// more, so the decision it was asked of is back on the review list and in the header's count,
// in the tree as in the count. The tree read answers it with no body and no mark of the delete,
// so the comments read tells it. Anchored on that decision's words, the deleted Change is no row
// of the outline (R9): the margin beside the decision draws it.
test("a Change its author deleted leaves its decision on the review list and in the count", () => {
  const deleted: Comment = {
    ...change,
    body: "",
    deleted_at: "2026-11-05T12:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const tree = answerOf([
    statement,
    ...askNodes,
    decisionNode("listed", ["r1"]),
    decisionNode("untied", []),
    decisionNode("changed", ["r1"]),
    commentNode(deleted, "changed"),
  ]);
  const counted = waitingOnReader(issue, "alice", isCoverageStatement, {
    comments: [deleted],
    tree,
  });
  expect(counted).toEqual({ approvals: [spec.id], asks: 1, decisions: 2, total: 4 });

  const root = buildIssueTree({ comments: [deleted], coverage, issue, tree, viewer: "alice" });
  expect(waitingIn(root)).toEqual(["mine", "spec#b-listed", "spec#b-changed"]);
  expect(parentsOf(root, deleted.id)).toEqual([]);
});

// A decision the count counts is a decision node of the tree read, by the review
// state the read answers on it: one that waits for a person's review and that a link ties to a
// statement. One kept as it reads now, one replaced, and one that waits but is tied to nothing
// do not count, and the tree marks the same one waiting. Before the tree read answers there is
// no decision to count.
test("waitingOnReader counts a waiting decision node", () => {
  const keep: DecisionKeep = {
    artifact_id: spec.id,
    block_id: "kept",
    changed_version: 2,
    kept_at: "2026-11-05T12:00:00Z",
    kept_by: "alice",
  };
  const tree = answerOf([
    statement,
    decisionNode("waits", ["r1"]),
    decisionNode("kept", ["r1"], {
      decision: {
        kept: keep,
        waiting_review: false,
      },
    }),
    decisionNode("replaced", ["r1"], {
      decision: {
        replaced: true,
        waiting_review: false,
      },
    }),
    decisionNode("untied", []),
  ]);

  expect(waitingOnReader(quiet, "alice", isCoverageStatement, { tree })).toEqual({
    approvals: [],
    asks: 0,
    decisions: 1,
    total: 1,
  });
  expect(waits(tree, coverage)).toEqual([1, ["spec#b-waits"]]);
  expect(waitingOnReader(quiet, "alice", isCoverageStatement, { tree: undefined }).decisions).toBe(
    0
  );
});

// INFRA-1196's decision 516: a decision whose `decided_by` names a person is that person's
// decision, which never waits for review. Though the read answers it waiting and its tie names a
// statement the coverage read holds, the count does not count it and the tree marks it waiting on
// nobody, while the agent's decision beside it, whose `decided_by` names a session, waits in the
// count as in the tree.
test("a decision a person decided is not counted, whatever its waiting_review says", () => {
  const tree = treeAnswer(
    [
      treeNode(KEY, "issue", null),
      statement,
      decisionNode("alices", ["r1"]),
      decisionNode("agents", ["r1"]),
      treeNode("alice", "person", null, { role: "context", text: "alice" }),
      treeNode("planner", "session", null, { role: "context", text: "planner" }),
    ],
    [treeTuple("decided_by", "alices", "alice"), treeTuple("decided_by", "agents", "planner")]
  );

  expect(decisionsOf(tree).map((one) => [one.block_id, one.waiting_review])).toEqual([
    ["alices", true],
    ["agents", true],
  ]);
  expect(waits(tree, coverage)).toEqual([1, ["spec#b-agents"]]);
});

// R2: an overtaken decision stays where its kind puts it and counts in no count of what waits.
// One the read answers `overtaken`, though it waits for review and a tie names a statement, is
// not in the header's count, and the tree marks it waiting on nobody, while the decision beside
// it that is not overtaken still waits, in the count as in the tree.
test("an overtaken waiting decision tied to a statement is not counted", () => {
  const tree = answerOf([
    statement,
    decisionNode("overtaken", ["r1"], { state: "overtaken" }),
    decisionNode("waits", ["r1"]),
  ]);

  expect(waits(tree, coverage)).toEqual([1, ["spec#b-waits"]]);
});

// R2: an overtaken question counts in no count of what waits either. An open ask whose turn is
// the reader's, which the read answers `overtaken`, is not in the header's count, and the tree
// marks it waiting on nobody, though it is still an open question.
test("an overtaken open question is not counted", () => {
  const tree = answerOf(
    askNodes.map((node) => (node.id === "mine" ? { ...node, state: "overtaken" as const } : node))
  );

  expect(waitingOnReader(issue, "alice", isCoverageStatement, { tree })).toEqual({
    approvals: [spec.id],
    asks: 0,
    decisions: 0,
    total: 1,
  });
  const root = buildIssueTree({ coverage, issue, tree, viewer: "alice" });
  expect(waitingIn(root)).toEqual([]);
  expect(nodeIn(root, "mine")).toMatchObject({ question: true, waiting: false });
});

// A Change is the comment the tree read hangs under its decision, which is where the
// read hangs a comment anchored inside the decision's words. A person's comment anchored to the
// second decision's words that the read hangs elsewhere - under the issue, or under the first
// decision, whose words hold the same sentence - is no Change of it, and the second decision
// still waits on the reader; once the read hangs it under the second decision, that decision
// waits on its agent, in the count as in the tree. No quote is matched for it.
test("a Change is only the comment under its decision", () => {
  const first = decisionNode("d1", [], { text: "Keep the model pure. Draw it once." });
  const second = decisionNode("d2", ["r1"], { text: "Draw it once." });
  const asked: Comment = {
    ...change,
    anchor: {
      artifact_id: spec.id,
      block_id: "d2",
      mark_id: "c-asked",
      orphaned: false,
      quote: "Draw it once.",
      version: 3,
    },
    id: "c-asked",
  };
  const hungUnder = (parent: string) => {
    const tree = answerOf([statement, first, second, commentNode(asked, parent)]);
    const root = buildIssueTree({ coverage, issue: quiet, tree, viewer: "alice" });
    return { drawnUnder: parentsOf(root, asked.id), waits: waits(tree, coverage) };
  };

  expect(hungUnder(KEY)).toEqual({ drawnUnder: [KEY], waits: [1, ["spec#b-d2"]] });
  expect(hungUnder("d1")).toEqual({ drawnUnder: ["spec#b-d1"], waits: [1, ["spec#b-d2"]] });
  expect(hungUnder("d2")).toEqual({ drawnUnder: ["spec#b-d2"], waits: [0, []] });
});

// The count and the tree find a decision node's Change in the same place, the comment the tree
// read hangs under the node, and neither finds it in the comments read, which says only which
// Changes their author deleted: so they agree while that read has not answered yet, or answered
// before the Change was written, and the count does not call the decision waiting on the reader
// while the outline draws it waiting on its agent.
test("the count and the tree agree on a decision node's Change the comments read does not hold", () => {
  const asked: Comment = { ...change, anchor: null, id: "c-noded" };
  const tree = answerOf([statement, decisionNode("noded", ["r1"]), commentNode(asked, "noded")]);

  expect(waits(tree, coverage)).toEqual([0, []]);
});

// Decision 337: a Change its author deleted asks nothing any more, and the tree read answers a
// deleted comment with no mark of the delete, so the comments read tells it. The decision whose
// node the read hangs that Change under is back on the review list, in the count as in the tree,
// and the deleted Change keeps its place under the node, drawn as the deleted line.
test("a deleted Change under its decision node leaves the decision on the review list and in the count", () => {
  const asked: Comment = { ...change, anchor: null, id: "c-noded" };
  const deleted: Comment = {
    ...asked,
    body: "",
    deleted_at: "2026-11-05T12:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const tree = answerOf([statement, decisionNode("noded", ["r1"]), commentNode(deleted, "noded")]);
  const root = buildIssueTree({
    comments: [deleted],
    coverage,
    issue: quiet,
    tree,
    viewer: "alice",
  });
  expect([
    waitingOnReader(quiet, "alice", isCoverageStatement, { comments: [deleted], tree }).decisions,
    waitingIn(root),
  ]).toEqual([1, ["spec#b-noded"]]);
  expect(parentsOf(root, deleted.id)).toEqual(["spec#b-noded"]);
  const node = nodesInOrder(root).find((one) => one.id === deleted.id);
  expect({ deletedAt: node?.deletedAt, text: node?.text }).toEqual({
    deletedAt: "2026-11-05T12:00:00Z",
    text: "Deleted by alice · 2026-11-05T12:00:00Z",
  });
  // The same Change not deleted is counted, as above.
  expect(
    waitingOnReader(quiet, "alice", isCoverageStatement, { comments: [asked], tree }).decisions
  ).toBe(0);
});

// A person's comment under a decision is a
// Change only while it is newer than the decision's words and than the Keep the read answers on
// it. One made before the words last changed asked for a change they have since had, and one
// made before that Keep was answered by it, so the decision waits on the reader again; one newer
// than both is a Change, and the decision waits on its agent, in the count as in the tree.
test("a Change older than the decision's last change is no Change", () => {
  // The decision's words last changed at 10:00 (`waitingDecision`).
  const keptAt = (at: string): TreeNodeExtra => ({
    decision: {
      kept: {
        artifact_id: spec.id,
        block_id: "d1",
        changed_version: 2,
        kept_at: at,
        kept_by: "bob",
      },
    },
  });
  const madeAt = (at: string, extra: TreeNodeExtra = {}) =>
    waits(
      answerOf([
        statement,
        decisionNode("d1", ["r1"], extra),
        commentNode({ ...change, created_at: at }, "d1"),
      ]),
      coverage
    );

  expect(madeAt("2026-11-05T09:00:00Z")).toEqual([1, ["spec#b-d1"]]);
  expect(madeAt("2026-11-05T11:00:00Z")).toEqual([0, []]);
  expect(madeAt("2026-11-05T11:00:00Z", keptAt("2026-11-05T12:00:00Z"))).toEqual([
    1,
    ["spec#b-d1"],
  ]);
  expect(madeAt("2026-11-05T13:00:00Z", keptAt("2026-11-05T12:00:00Z"))).toEqual([0, []]);
});

// R3, root R35: a decision with a Change waits on its agent, and once its words change after that
// Change it waits for the reader's review again, in the count as in the tree. The Change was made
// at 11:00; the decision's words changed at 10:00, and then again at 12:00, after it.
test("a decision waits again once its words change after its Change", () => {
  const asked = commentNode(change, "d1");
  const before = answerOf([statement, decisionNode("d1", ["r1"]), asked]);
  const after = answerOf([
    statement,
    decisionNode("d1", ["r1"], {
      decision: { changed_at: "2026-11-05T12:00:00Z", changed_version: 3 },
      text: "The new words of d1.",
    }),
    asked,
  ]);
  const stateOf = (tree: TreeAnswer) =>
    nodeIn(buildIssueTree({ coverage, issue: quiet, tree, viewer: "alice" }), "spec#b-d1")?.state;

  expect(waits(before, coverage)).toEqual([0, []]);
  expect(waits(after, coverage)).toEqual([1, ["spec#b-d1"]]);
  expect([stateOf(before), stateOf(after)]).toEqual(["change_requested", "waiting_review"]);
});

// Two documents of an issue may share a block id. A person's comment on the statement r1 is no
// Change of a decision whose own block id is r1 too: the read's walk puts a parent before its
// children, and it puts this comment after the statement and before the decision, so the comment
// hangs under the statement, as the outline draws it, and the decision still waits on the
// reader, in the count as in the tree.
test("a comment on a statement is no Change of a decision of another document with the same block id", () => {
  const remark: Comment = { ...change, anchor: null, id: "c-on-statement" };
  const decided = decisionNode("r1", ["r1"], {
    document: { artifact_id: "artifact-decisions", position: 0, slug: "decisions-md" },
  });
  const tree = answerOf([statement, commentNode(remark, "r1"), decided]);

  expect(waits(tree, coverage)).toEqual([1, [blockNodeId("decisions-md", "r1")]]);
});

/** The node `id` under `root`, wherever it sits. */
function nodeIn(root: TreeNode, id: string): TreeNode | undefined {
  if (root.id === id) return root;
  for (const child of root.children) {
    const found = nodeIn(child, id);
    if (found !== undefined) return found;
  }
  return undefined;
}

// Two documents of an issue may each hold a decision block of one id, and each is a decision of
// its own. The decisions document's decision d1 comes first in the read and the spec's decision
// block d1 later, and a person's Change the read hangs under the first is its Change alone: that
// decision waits on its agent, in the count as in the tree, and the spec's decision of the same
// id still waits for review.
test("a Change of a decision node is no Change of another document's decision of the same block id", () => {
  const asked: Comment = { ...change, anchor: null, id: "c-on-decision" };
  const changed = decisionNode("d1", ["r1"], {
    document: { artifact_id: "artifact-decisions", position: 0, slug: "decisions-md" },
  });
  const other = answerNode("d1", "decision", {
    decision: { changed_at: changed.decision?.changed_at ?? null, waiting_review: true },
    document: { artifact_id: spec.id, position: 1, slug: "spec" },
  });
  const tree = answerOf([statement, changed, commentNode(asked, "d1"), other]);
  const root = buildIssueTree({ coverage, issue: quiet, tree, viewer: "alice" });

  expect({
    count: waitingOnReader(quiet, "alice", isCoverageStatement, { tree }).decisions,
    states: [blockNodeId("decisions-md", "d1"), blockNodeId("spec", "d1")].map(
      (id) => nodeIn(root, id)?.state
    ),
    waiting: waitingIn(root),
  }).toEqual({ count: 0, states: ["change_requested", "waiting_review"], waiting: [] });
});

// R7, R13 and R35: a statement is a requirement the coverage read holds, as the header's count is
// handed the statements. A decision tied only to a `requirement` block of the plan, which no
// coverage row holds, is on no review list; nor is one tied to the spec's `r1` while the coverage
// read has not answered, though the tree read answers that block already. The tree marks neither
// and the count counts neither, and the decision tied to `r1` once the coverage read holds it
// waits, in the tree as in the count.
test("a tie to a requirement the coverage read does not hold puts no decision on the review list", () => {
  const tiedToPlan = decisionNode("planned", ["p1"]);
  const tree = answerOf([
    statement,
    answerNode("p1", "requirement", {
      document: { artifact_id: plan.id, position: 0, slug: "plan-md" },
    }),
    decisionNode("listed", ["r1"]),
    withDecision(tiedToPlan, {
      links: (tiedToPlan.decision?.links ?? []).map((link) => ({
        ...link,
        to: { ...link.to, id: plan.id, ref: `dispatch://${KEY}/artifact/plan-md#b-p1` },
      })),
    }),
  ]);

  expect(waits(tree, coverage, planned)).toEqual([1, ["spec#b-listed"]]);
  expect(waits(tree, undefined, planned)).toEqual([0, []]);
});

// R7, R13 and R35: the count and the tree read a tie by one rule for a statement
// (`coverageStatements`), whatever address the tie writes. A tie to the spec's `r1`, which the
// coverage read holds, puts its decision on the review list whether it writes the block short
// (`spec#b-r1`) or by the spec's own slug. A tie to the spec's `r2` puts it on none: the tree
// read answers `r2` as a `requirement` block of the spec, whose row is a statement as `r1`'s is,
// but the coverage read does not hold it. Nor does a tie to another issue's `r1`, or one to a
// `requirement` block of the plan. Each decision waits on the reader in the tree where the count
// counts it, and nowhere else.
test("the count and the tree read a tie by one rule for a statement, whatever address it writes", () => {
  const tiedTo = (id: string, block: string, documentId: string, ref: string): TreeAnswerNode => {
    const decided = decisionNode(id, [block]);
    return withDecision(decided, {
      links: (decided.decision?.links ?? []).map((link) => ({
        ...link,
        to: { ...link.to, id: documentId, ref },
      })),
    });
  };
  const tied = [
    tiedTo("short", "r1", spec.id, `dispatch://${KEY}/spec#b-r1`),
    tiedTo("slugged", "r1", spec.id, `dispatch://${KEY}/artifact/spec#b-r1`),
    tiedTo("unlisted", "r2", spec.id, `dispatch://${KEY}/spec#b-r2`),
    tiedTo("elsewhere", "r1", "artifact-other-spec", "dispatch://ACME-2/spec#b-r1"),
    tiedTo("planned", "p1", plan.id, `dispatch://${KEY}/artifact/plan-md#b-p1`),
  ];
  const requirements = [
    statement,
    answerNode("r2", "requirement", {
      document: { artifact_id: spec.id, position: 1, slug: "spec" },
    }),
    answerNode("p1", "requirement", {
      document: { artifact_id: plan.id, position: 0, slug: "plan-md" },
    }),
  ];
  const root = buildIssueTree({
    coverage,
    issue: planned,
    tree: answerOf([...requirements, ...tied]),
    viewer: "alice",
  });
  expect(
    nodesInOrder(root)
      .filter(isStatement)
      .map((node) => node.id)
      .sort()
  ).toEqual(["spec#b-r1", "spec#b-r2"]);

  const waitingDecisions = waitingIn(root);
  expect(waitingDecisions).toEqual(["spec#b-short", "spec#b-slugged"]);
  const listed = coverageStatements(planned, coverage);
  expect(tied).toHaveLength(5);
  for (const decided of tied) {
    const counted = waitingOnReader(planned, "alice", listed, {
      tree: answerOf([...requirements, decided]),
    });
    const block = decided.block_id ?? decided.id;
    const marked = waitingDecisions.includes(blockNodeId("spec", block));
    expect(counted.decisions, block).toBe(marked ? 1 : 0);
  }
});

// A decision node no document holds has no address and no document a Keep could be written
// against, so it is no decision of the page's (`decisionNodes`): though the read answers it
// waiting and tied to a statement, the count does not count it and the tree does not mark it
// waiting.
test("a decision node no document holds waits on nobody", () => {
  const tree = answerOf([statement, decisionNode("loose", ["r1"], { document: undefined })]);

  expect(decisionsOf(tree)).toEqual([]);
  expect(waits(tree, coverage)).toEqual([0, []]);
});

/** The parent a case names by `name`, as `tree-fixtures.ts`'s `treeParent` gives it, which
 *  `treeAnswer` names as the route does. */
function parentNamed(name: string): NonNullable<TreeAnswerNode["parent"]> {
  return { first_line: "", id: name, kind: "", supertag: "" };
}

/** What the count of `about` over `tree` counts for alice, and the id of each node of the tree
 *  that waits on her: the two read the same open asks, the same asks read `asks` and the same
 *  decision nodes. */
function countedAndDrawn(tree: TreeAnswer, about: IssueDetails, asks?: readonly Ask[]) {
  return {
    counted: waitingOnReader(about, "alice", isCoverageStatement, { asksRead: asks, tree }),
    drawn: waitingIn(buildIssueTree({ asks, coverage, issue: about, tree, viewer: "alice" })),
  };
}

/** A question alice is asked in block `q1` of the spec, as the issue read carries it. */
const inSpec: OpenAsk = {
  ...openAsk("in-spec", "question", "alice"),
  block_artifact: { id: spec.id, primary: true, slug: "spec" },
  block_id: "q1",
};

/** The issue with the question asked in the spec beside its other open asks. */
const asking: IssueDetails = { ...issue, open_asks: [...(issue.open_asks ?? []), inSpec] };

/** The block of the spec the question is written as, as the tree read answers it. */
function askBlock(extra: Partial<TreeAnswerNode> = {}): TreeAnswerNode {
  return answerNode("q1", "ask", {
    document: { artifact_id: spec.id, position: 2, slug: "spec" },
    text: inSpec.question,
    ...extra,
  });
}

/** The question's own node, the ask's, as the tree read answers it beside its block. */
function inSpecAsk(extra: Partial<TreeAnswerNode> = {}): TreeAnswerNode {
  return answerNode(inSpec.id, "ask", { text: inSpec.question, ...extra });
}

/** The issue's open asks as the tree read answers them (`askNodes`), the one alice is asked
 *  (`mine`) given `extra`. */
function mine(extra: Partial<TreeAnswerNode>): TreeAnswerNode[] {
  return askNodes.map((node) => (node.id === "mine" ? { ...node, ...extra } : node));
}

// No reader is waited on for an open question at or under an overtaken node, since a later node
// took its place: the count leaves it out, as the tree draws it in no waiting tone.
// That holds for a question that is itself overtaken, for one the read hangs under an overtaken
// decision, for one asked in a block of the spec whose ask's node is overtaken, and for one the
// read hangs under that block, whatever order the read answers the ask, its block and what
// stands under the block in. The approval request, which is no node of the tree, still waits,
// and so does each question nothing overtook.
test("no reader is waited on for an overtaken ask", () => {
  expect(countedAndDrawn(answerOf([statement, ...askNodes, askBlock()]), asking)).toEqual({
    counted: { approvals: [spec.id], asks: 2, decisions: 0, total: 3 },
    drawn: ["mine", "spec#b-q1"],
  });
  expect(
    countedAndDrawn(answerOf([statement, ...mine({ state: "overtaken" }), askBlock()]), asking)
  ).toEqual({
    counted: { approvals: [spec.id], asks: 1, decisions: 0, total: 2 },
    drawn: ["spec#b-q1"],
  });
  expect(
    countedAndDrawn(
      answerOf([
        statement,
        decisionNode("old", [], { state: "overtaken" }),
        ...mine({ parent: parentNamed("old") }),
        askBlock(),
      ]),
      asking
    )
  ).toEqual({
    counted: { approvals: [spec.id], asks: 1, decisions: 0, total: 2 },
    drawn: ["spec#b-q1"],
  });
  expect(
    countedAndDrawn(
      answerOf([statement, ...askNodes, askBlock(), inSpecAsk({ state: "overtaken" })]),
      asking
    )
  ).toEqual({
    counted: { approvals: [spec.id], asks: 1, decisions: 0, total: 2 },
    drawn: ["mine"],
  });
  for (const order of [
    [...mine({ parent: parentNamed("q1") }), askBlock(), inSpecAsk({ state: "overtaken" })],
    [inSpecAsk({ state: "overtaken" }), askBlock(), ...mine({ parent: parentNamed("q1") })],
  ]) {
    expect(countedAndDrawn(answerOf([statement, ...order]), asking)).toEqual({
      counted: { approvals: [spec.id], asks: 0, decisions: 0, total: 1 },
      drawn: [],
    });
  }
});

// A question asked in a block of the spec is its ask's node, and the block only its place in the
// spec, so a state the block's node holds takes no wait away: with its block's node alone
// overtaken, the question still waits on the reader, in the count as in the tree.
test("an open question whose block's node alone is overtaken is still waited on", () => {
  const tree = answerOf([statement, ...askNodes, askBlock({ state: "overtaken" }), inSpecAsk()]);

  expect(countedAndDrawn(tree, asking)).toEqual({
    counted: { approvals: [spec.id], asks: 2, decisions: 0, total: 3 },
    drawn: ["mine", "spec#b-q1"],
  });
});

// A decision at or under an overtaken node waits on nobody: one waiting for review and tied to a
// statement is on no review list once it is overtaken, nor once the node the read hangs it under
// is, in the count as in the tree. The decision nothing overtook still waits.
test("an overtaken decision waits on nobody", () => {
  const decided = (old: Partial<TreeAnswerNode>) =>
    answerOf([
      statement,
      decisionNode("old", ["r1"], old),
      { ...decisionNode("under", ["r1"]), parent: parentNamed("old") },
      decisionNode("waits", ["r1"]),
    ]);

  expect(waits(decided({}), coverage)).toEqual([3, ["spec#b-old", "spec#b-under", "spec#b-waits"]]);
  expect(waits(decided({ state: "overtaken" }), coverage)).toEqual([1, ["spec#b-waits"]]);
});

// A proposed node is no overtaken one, since no node took its place yet, so a proposed question
// whose turn is the reader's and a proposed decision waiting for review still wait on the reader,
// in the count as in the tree.
test("a proposed ask and a proposed decision still wait on the reader", () => {
  const tree = answerOf([
    statement,
    ...mine({ state: "proposed" }),
    decisionNode("listed", ["r1"], { state: "proposed" }),
  ]);

  expect(countedAndDrawn(tree, issue)).toEqual({
    counted: { approvals: [spec.id], asks: 1, decisions: 1, total: 3 },
    drawn: ["mine", "spec#b-listed"],
  });
});

/** The question asked in block `q1` of the spec once alice answered it, as the asks read carries
 *  it: no longer one of the issue's open asks. */
const answered: Ask = {
  ...inSpec,
  answer: { at: "2026-11-05T09:00:00Z", selected: [], text: "1280 px.", user: "alice" },
  state: "answered",
};

// Decision 447: the count pairs a question written as a block of the spec with its ask as the
// tree does (`askOfNode`), through the issue's open asks and the asks read alike. Once alice
// answered it, the question is no open ask of the issue, and only the asks read names the block
// it is written as. With its ask's node overtaken, neither the open question the read hangs
// under its block nor the decision waiting for review there waits on the reader: the count counts
// neither, and the tree draws the block's row and both of them closed and none waiting. With its
// ask's node live, both wait, in the count as in the tree.
test("an answered question whose ask is overtaken counts nothing under it", () => {
  const underBlock = (ask: Partial<TreeAnswerNode>) =>
    answerOf([
      statement,
      askBlock(),
      inSpecAsk(ask),
      ...mine({ parent: parentNamed("q1") }),
      { ...decisionNode("under", ["r1"]), parent: parentNamed("q1") },
    ]);

  expect(countedAndDrawn(underBlock({}), issue, [answered])).toEqual({
    counted: { approvals: [spec.id], asks: 1, decisions: 1, total: 3 },
    drawn: ["mine", "spec#b-under"],
  });
  const overtaken = underBlock({ state: "overtaken" });
  expect(countedAndDrawn(overtaken, issue, [answered])).toEqual({
    counted: { approvals: [spec.id], asks: 0, decisions: 0, total: 1 },
    drawn: [],
  });
  const root = buildIssueTree({
    asks: [answered],
    coverage,
    issue,
    tree: overtaken,
    viewer: "alice",
  });
  expect(["spec#b-q1", "mine", "spec#b-under"].map((id) => nodeIn(root, id)?.closed)).toEqual([
    true,
    true,
    true,
  ]);
});
