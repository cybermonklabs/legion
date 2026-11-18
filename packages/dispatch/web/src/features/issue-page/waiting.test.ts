import { expect, test } from "bun:test";

import type {
  Artifact,
  Comment,
  Decision,
  DecisionKeep,
  IssueCoverage,
  IssueDetails,
} from "../../api/types";
import { wordsOf } from "../refs/tree-fragment";
import { decisionNodes } from "./decision-nodes/decision-nodes";
import { buildIssueTree, type TreeNode } from "./issue-tree";
import { blockNodeId, treeContext } from "./tree-drafts";
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

/** One node of the tree read's answer, under the issue unless the case gives it another parent,
 *  with nothing on it but what the case gives it. */
function treeNode(
  id: string,
  supertag: string,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return {
    attributes: {},
    author: [],
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent: KEY,
    rank: null,
    state: "live",
    supertag,
    text: "",
    visibility: "project",
    ...extra,
  };
}

/** The tree read's answer for the issue: its own node, then `nodes` in the read's order. */
function answerOf(nodes: readonly TreeAnswerNode[]): TreeAnswer {
  return {
    converted: false,
    cursor: null,
    nodes: [treeNode(KEY, "issue", { parent: null }), ...nodes],
    tree: null,
    tuples: [],
    view_of: null,
  };
}

/** The spec's statement `r1` as the tree read answers it, a typed block of the spec. */
const statement = treeNode("r1", "requirement", {
  document: { artifact_id: spec.id, position: 0, slug: "spec" },
  text: "The first.",
});

/** The issue's open asks as the tree read answers them, each under the issue. */
const askNodes = (issue.open_asks ?? []).map((ask) =>
  treeNode(ask.id, "ask", { author: [ask.author], made_at: ask.created_at, text: ask.question })
);

/** A person's comment as the tree read answers it, under the node `parent`. */
function commentNode(comment: Comment, parent: string): TreeAnswerNode {
  return treeNode(comment.id, "comment", {
    author: [comment.author],
    made_at: comment.created_at,
    parent,
    text: comment.body,
  });
}

/** An agent's decision as the tree read answers its node: a decision block of the spec, with the
 *  members the read answers on it (`waitingDecision`'s) and the words `The words of <id>.`,
 *  unless the case gives it others. */
function decisionNode(
  id: string,
  shaped: readonly string[],
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  const row = waitingDecision(id, shaped);
  return treeNode(id, "decision", {
    asks: row.asks,
    changed_at: row.changed_at,
    changed_version: row.changed_version,
    document: { artifact_id: spec.id, position: 1, slug: "spec" },
    kept: row.kept,
    kind: row.kind,
    links: row.links,
    replaced: row.replaced,
    text: `The words of ${id}.`,
    text_sha256: row.text_sha256,
    waiting_review: row.waiting_review,
    ...extra,
  });
}

/** Whether `reference` names a statement of the issue, as the header reads it: by the coverage
 *  read's statements, each addressed as the tree addresses it, and none while that read has not
 *  answered. */
const nodeIdOf = treeContext({ issue }, wordsOf).nodeIdOf;
function statementsOf(read: IssueCoverage | undefined): (reference: string) => boolean {
  const statements = new Set(read?.requirements?.map((one) => nodeIdOf(one.ref)));
  return (reference) => statements.has(nodeIdOf(reference));
}
const isStatement = statementsOf(coverage);

/** The decisions of `tree`: its decision nodes (`decisionNodes`). */
function decisionsOf(tree: TreeAnswer) {
  return decisionNodes(tree, { issue });
}

/** How many decisions wait on alice over `tree`, in the count, and the id of each node of the
 *  tree that waits on her, with the statements `read` holds: none while it has not answered. */
function waits(
  tree: TreeAnswer,
  read: IssueCoverage | undefined,
  about: IssueDetails = quiet
): readonly [number, readonly string[]] {
  return [
    waitingOnReader(about, "alice", statementsOf(read), tree).decisions,
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

// R7, R13, R35, and the tester's F4 and F6: what waits on the reader is each open question whose
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
  const counted = waitingOnReader(issue, "alice", isStatement, tree);
  expect(counted).toEqual({ approvals: [spec.id], asks: 1, decisions: 1, total: 3 });
  expect(waitingIn(buildIssueTree({ coverage, issue, tree, viewer: "alice" }))).toEqual([
    "mine",
    "spec#b-listed",
  ]);

  const unknown = waitingOnReader(issue, undefined, isStatement, tree);
  expect(unknown.total).toBe(0);
  expect(waitingIn(buildIssueTree({ coverage, issue, tree }))).toEqual([]);
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
    decisionNode("kept", ["r1"], { kept: keep, waiting_review: false }),
    decisionNode("replaced", ["r1"], { replaced: true, waiting_review: false }),
    decisionNode("untied", []),
  ]);

  expect(waitingOnReader(quiet, "alice", isStatement, tree)).toEqual({
    approvals: [],
    asks: 0,
    decisions: 1,
    total: 1,
  });
  expect(waits(tree, coverage)).toEqual([1, ["spec#b-waits"]]);
  expect(waitingOnReader(quiet, "alice", isStatement, undefined).decisions).toBe(0);
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
// read hangs under the node, and neither reads the comments read: so they agree while that read
// has not answered yet, or answered before the Change was written, and the count does not call
// the decision waiting on the reader while the outline draws it waiting on its agent.
test("the count and the tree agree on a decision node's Change the comments read does not hold", () => {
  const asked: Comment = { ...change, anchor: null, id: "c-noded" };
  const tree = answerOf([statement, decisionNode("noded", ["r1"]), commentNode(asked, "noded")]);

  expect(waits(tree, coverage)).toEqual([0, []]);
});

// A person's comment under a decision is a
// Change only while it is newer than the decision's words and than the Keep the read answers on
// it. One made before the words last changed asked for a change they have since had, and one
// made before that Keep was answered by it, so the decision waits on the reader again; one newer
// than both is a Change, and the decision waits on its agent, in the count as in the tree.
test("a Change older than the decision's last change is no Change", () => {
  // The decision's words last changed at 10:00 (`waitingDecision`).
  const keptAt = (at: string): Partial<TreeAnswerNode> => ({
    kept: { artifact_id: spec.id, block_id: "d1", changed_version: 2, kept_at: at, kept_by: "bob" },
  });
  const madeAt = (at: string, extra: Partial<TreeAnswerNode> = {}) =>
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
  const other = treeNode("d1", "decision", {
    changed_at: changed.changed_at,
    document: { artifact_id: spec.id, position: 1, slug: "spec" },
    kept: null,
    replaced: false,
    waiting_review: true,
  });
  const tree = answerOf([statement, changed, commentNode(asked, "d1"), other]);
  const root = buildIssueTree({ coverage, issue: quiet, tree, viewer: "alice" });

  expect({
    count: waitingOnReader(quiet, "alice", isStatement, tree).decisions,
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
  const plan = issueDocument("artifact-plan", "plan-md");
  const planned: IssueDetails = { ...quiet, artifacts: [spec, plan], plan_artifact_id: plan.id };
  const tiedToPlan = decisionNode("planned", ["p1"]);
  const tree = answerOf([
    statement,
    treeNode("p1", "requirement", {
      document: { artifact_id: plan.id, position: 0, slug: "plan-md" },
    }),
    decisionNode("listed", ["r1"]),
    {
      ...tiedToPlan,
      links: (tiedToPlan.links ?? []).map((link) => ({
        ...link,
        to: { ...link.to, id: plan.id, ref: `dispatch://${KEY}/artifact/plan-md#b-p1` },
      })),
    },
  ]);

  expect(waits(tree, coverage, planned)).toEqual([1, ["spec#b-listed"]]);
  expect(waits(tree, undefined, planned)).toEqual([0, []]);
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
