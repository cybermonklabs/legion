import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { api } from "../../../api/client";
import type {
  Artifact,
  Ask,
  CheckBlock,
  DecisionLink,
  DisplayTree,
  IssueCoverage,
  IssueDetails,
  RequirementCoverage,
} from "../../../api/types";
import { buildDispatchReference } from "../../refs/routes";
import { decisionNodes } from "../decision-nodes/decision-nodes";
import { decisionNodeChanges } from "../decision-review";
import {
  assemble,
  buildIssueTree,
  type IssueTreeReads,
  lineWords,
  nodeWords,
  type TreeNode,
  treeWordsOf,
} from "../issue-tree";
import { blockNodeId } from "../tree-drafts";
import {
  blocksOfRole,
  type TreeDraftMark,
  treeDraftMarks,
  treeDraftsFrom,
  treeIdOf,
} from "./tree-nodes";
import { issueTreeQuery, type TreeAnswer, type TreeAnswerNode, type TreeTuple } from "./tree-read";

/**
 * The tree read's answer as the outline's drafts. Each case writes the answer as the contract
 * describes it: every node with its `parent`, in the read's walk order, a typed block with the
 * `document` it is written in, and the tuples between them. The tree is built from the drafts as
 * `buildIssueTree` builds it, with `assemble`, the outline's word rules and its reading of the
 * issue's names.
 */

const KEY = "CORE-1";

/** A supertag no part's step drafts, so a node of it is drawn by no row whatever later parts add
 *  to `treeDraftSteps` (later work drafts a lens, a finding, a risk, a constraint, a criterion
 *  and a reading). */
const UNDRAWN = "unknown_kind";

function artifact(id: string, slug: string, name: string, primary = false): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  };
}

const SPEC = artifact("artifact-spec", "spec", "spec.md", true);
const PLAN = artifact("artifact-plan", "plan-md", "plan.md");
const VERIFICATION = artifact("artifact-verification", "verification-md", "verification.md");
const DECISIONS = artifact("artifact-decisions", "decisions-md", "decisions.md");

function issueDetails(): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [SPEC, PLAN, VERIFICATION, DECISIONS],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    design_gates: [],
    external_links: [],
    key: KEY,
    labels: [],
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    decisions_artifact_id: "",
    primary_artifact_id: SPEC.id,
    verification_artifact_id: "",
    priority: null,
    project: "CORE",
    progress: { children: null, tasks: null },
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    holder_state: null,
    last_seen: null,
    takeable_at: null,
    status: "todo",
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
  };
}

/** The server's reading of a text of one paragraph per string, as a read sends it with
 *  `Dispatch-Trees: 1`. */
function reading(...paragraphs: string[]): DisplayTree {
  return {
    content: paragraphs.map((text) => ({ content: [{ text, type: "text" }], type: "paragraph" })),
    type: "doc",
  };
}

/** One node of the answer, its words stored and read alike unless the case says otherwise. */
function node(
  id: string,
  supertag: string,
  parent: string | null,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  const text = `Words of ${id}.`;
  return {
    attributes: {},
    author: [],
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent,
    rank: null,
    state: "live",
    supertag,
    text,
    text_tree: reading(text),
    visibility: "project",
    ...extra,
  };
}

/** A typed block of one of the issue's documents, at `position` in it. */
function block(
  id: string,
  supertag: string,
  parent: string,
  document: Artifact,
  position: number,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return node(id, supertag, parent, {
    document: { artifact_id: document.id, position, slug: document.slug },
    ...extra,
  });
}

/** The issue's own node, the root every other node hangs from. */
const ISSUE = node(KEY, "issue", null, { text: "The issue page draws one ordered body" });

/** One answer of the tree read, every page merged: the issue's node, then `nodes` in the walk's
 *  order, on an issue that has converted unless the case says otherwise. */
function answer(
  nodes: readonly TreeAnswerNode[],
  tuples: readonly TreeTuple[] = [],
  extra: Partial<TreeAnswer> = {}
): TreeAnswer {
  return {
    converted: true,
    cursor: null,
    nodes: [ISSUE, ...nodes],
    tree: "artifact-tree",
    tuples: [...tuples],
    view_of: null,
    ...extra,
  };
}

function tuple(kind: string, from: string, to: string): TreeTuple {
  return { from, kind, to };
}

/** The drafts `tree` holds, read by what `buildIssueTree` reads them by (`treeWordsOf`), and the
 *  tree `assemble` builds from them. */
function drawn(tree: TreeAnswer, reads: IssueTreeReads = { issue: issueDetails() }) {
  const context = treeWordsOf(reads);
  const drafts = treeDraftsFrom(tree, reads, context);
  return { drafts, root: assemble(reads.issue, [...drafts.values()], context.chipOf) };
}

/** The node `id` under `root`, wherever it sits; the first placement of a node drawn twice. */
function nodeOf(root: TreeNode, id: string): TreeNode | undefined {
  if (root.id === id) return root;
  for (const child of root.children) {
    const found = nodeOf(child, id);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** Every placement of `id`, with the node each one sits under. */
function placementsOf(root: TreeNode, id: string): { parent: string; node: TreeNode }[] {
  const found: { parent: string; node: TreeNode }[] = [];
  const walk = (one: TreeNode): void => {
    for (const child of one.children) {
      if (child.id === id) found.push({ node: child, parent: one.id });
      walk(child);
    }
  };
  walk(root);
  return found;
}

/** Every node of the tree, the root first, each before its children. */
function nodesOf(root: TreeNode): TreeNode[] {
  return [root, ...root.children.flatMap(nodesOf)];
}

function requirement(id: string): RequirementCoverage {
  return {
    block_id: id,
    checks: 1,
    gaps: [],
    plan_tasks: 1,
    ref: `dispatch://${KEY}/spec#b-${id}`,
    text: `Words of ${id}.`,
  };
}

function coverage(requirements: readonly RequirementCoverage[]): IssueCoverage {
  return {
    gaps: [],
    has_plan: true,
    issue: KEY,
    requirements: [...requirements],
    spec_artifact_id: SPEC.id,
  };
}

/** One question asked of the issue. */
function question(id: string, extra: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { id: "planner", kind: "session" },
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    block_id: null,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: `Question ${id}?`,
    state: "open",
    urgency: "med",
    ...extra,
  };
}

/** A decision's link to the node `ref` addresses, as the decisions read answers one. */
function linkTo(ref: string): DecisionLink {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_version: 1,
    from_block: "d1",
    stale: false,
    to: {
      changed_at: null,
      changed_by: null,
      id: ref,
      kind: "block",
      made_at: null,
      made_by: null,
      parent: null,
      parent_version: null,
      ref,
    },
  };
}

/** A person's comment the read hangs under `parent`, made at `madeAt`. */
function personsComment(id: string, parent: string, madeAt: string): TreeAnswerNode {
  return node(id, "comment", parent, { author: [{ id: "alice", kind: "user" }], made_at: madeAt });
}

// R1, R2: each node stands under the parent the read answers, and siblings stand in the read's
// order: a requirement the read walks first though its document places it second, and two
// comments in the order the read answers them, though their ids sort the other way.
test("a node stands under its parent, in the read's order", () => {
  const { root } = drawn(
    answer([
      block("r2", "requirement", KEY, SPEC, 1),
      node("c-b", "comment", "r2", { made_at: "2026-11-05T10:00:00Z" }),
      node("c-a", "comment", "r2", { made_at: "2026-11-05T11:00:00Z" }),
      block("r1", "requirement", KEY, SPEC, 0),
      node("m1", "message", KEY),
    ])
  );

  expect(root.children.map((child) => child.id)).toEqual([
    blockNodeId("spec", "r2"),
    blockNodeId("spec", "r1"),
    "m1",
  ]);
  expect(nodeOf(root, blockNodeId("spec", "r2"))?.children.map((child) => child.id)).toEqual([
    "c-b",
    "c-a",
  ]);
});

// R1: a node's words are the server's reading of them (`text_tree`), read by the outline's word
// rules, and never its stored text, whose marks are no word of it.
test("a node's words come from its text_tree", () => {
  const { root } = drawn(
    answer([
      block("r1", "requirement", KEY, SPEC, 0, {
        text: "**Draw** the tree from `one` read.\n\nThen the rest.",
        text_tree: reading("Draw the tree from one read.", "Then the rest."),
      }),
      node("c1", "comment", KEY, {
        text: "_Agreed_, ship it.",
        text_tree: reading("Agreed, ship it."),
      }),
    ])
  );

  const words = [blockNodeId("spec", "r1"), "c1"].map((id) => {
    const one = nodeOf(root, id);
    return one === undefined ? undefined : [nodeWords(one), lineWords(one)];
  });
  expect(words).toEqual([
    ["Draw the tree from one read. Then the rest.", "Draw the tree from one read."],
    ["Agreed, ship it.", "Agreed, ship it."],
  ]);
});

// A node's `author` lists every author of its first version, whoever made it first ahead of the
// rest, and a row draws one name: a comment two authors wrote draws the first of them, and a
// message whose list is empty draws none.
test("a node draws the first author its list names, and none where the list is empty", () => {
  const alice = { id: "alice", kind: "user" };
  const planner = { id: "planner", kind: "session" };
  const { drafts, root } = drawn(
    answer([
      node("c1", "comment", KEY, { author: [alice, planner] }),
      node("m1", "message", KEY, { author: [] }),
    ])
  );

  expect(["c1", "m1"].map((id) => drafts.get(id)?.author)).toEqual([alice, undefined]);
  expect(["c1", "m1"].map((id) => nodeOf(root, id)?.author)).toEqual([alice, undefined]);
});

// R1, R2: on an issue that has not converted, a typed block stands under the parent the read
// answers - a plan task under the question it answers, not under the issue as the outline drew
// every block - and its `document` member names the document it is written in and its place
// there, which is its order among that document's blocks whatever its place in the walk.
test("an unconverted issue's typed block stands under the parent the read answers, its document and order from its document member", () => {
  const tree = answer(
    [
      node("q1", "ask", KEY),
      block("t2", "plan_task", "q1", PLAN, 1),
      block("r1", "requirement", KEY, SPEC, 0),
      block("t1", "plan_task", "r1", PLAN, 0),
    ],
    [tuple("serves", "t1", "r1")],
    { converted: false, documents: { plan: PLAN.id, spec: SPEC.id }, tree: null }
  );
  const reads = { issue: issueDetails() };
  const { drafts, root } = drawn(tree, reads);
  const second = blockNodeId("plan-md", "t2");

  expect(placementsOf(root, second).map((one) => one.parent)).toEqual(["q1"]);
  expect(placementsOf(root, blockNodeId("plan-md", "t1")).map((one) => one.parent)).toEqual([
    blockNodeId("spec", "r1"),
  ]);
  expect({
    reference: drafts.get(second)?.reference,
    source: drafts.get(second)?.source,
  }).toEqual({
    reference: `dispatch://${KEY}/artifact/plan-md#b-t2`,
    source: { id: PLAN.id, kind: "artifact" },
  });
  expect(blocksOfRole(tree, "plan", reads.issue)?.map((one) => [one.id, one.from])).toEqual([
    ["t1", 0],
    ["t2", 1],
  ]);
});

// R2: a node whose parent is a kind the page does not draw - here a kind no part's step drafts -
// stands under its nearest ancestor the page draws, and under the issue where it has none.
test("a node whose parent is not drawn stands under its nearest drawn ancestor", () => {
  const { root } = drawn(
    answer([
      block("r1", "requirement", KEY, SPEC, 0),
      node("other-1", UNDRAWN, "r1"),
      node("other-2", UNDRAWN, "other-1"),
      personsComment("c1", "other-2", "2026-11-05T00:00:00Z"),
      node("other-3", UNDRAWN, KEY),
      personsComment("c2", "other-3", "2026-11-05T00:00:00Z"),
    ])
  );

  expect(placementsOf(root, "c1").map((one) => one.parent)).toEqual([blockNodeId("spec", "r1")]);
  expect(placementsOf(root, "c2").map((one) => one.parent)).toEqual([KEY]);
});

// R1, root R4: a check stands under the requirement the read answers and again under each other
// requirement its `serves` tuples name, transcluded there; a plan task's `needs` tuple is a chip
// labelled as the outline labels it; a decision's `shaped` tuple is a chip naming what it shaped
// by that node's line, and places nothing.
test("a tuple between two nodes becomes F's chip and placement", () => {
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0),
        block("t1", "plan_task", "r1", PLAN, 0),
        block("t2", "plan_task", "r1", PLAN, 1),
        block("r2", "requirement", KEY, SPEC, 1, {
          text_tree: reading("R2. The second statement. Its reason."),
        }),
        node("d1", "decision", KEY, { kept: null, waiting_review: true }),
      ],
      [
        tuple("serves", "c1", "r1"),
        tuple("serves", "c1", "r2"),
        tuple("serves", "t1", "r1"),
        tuple("serves", "t2", "r1"),
        tuple("needs", "t2", "t1"),
        tuple("shaped", "d1", "r2"),
      ]
    ),
    { coverage: coverage([requirement("r1"), requirement("r2")]), issue: issueDetails() }
  );
  const first = blockNodeId("spec", "r1");
  const second = blockNodeId("spec", "r2");

  expect(
    placementsOf(root, blockNodeId("verification-md", "c1")).map((one) => [
      one.parent,
      one.node.transcluded,
      one.node.alsoUnder.map((chip) => chip.ref),
    ])
  ).toEqual([
    [first, false, [`dispatch://${KEY}/spec#b-r2`]],
    [second, true, [`dispatch://${KEY}/spec#b-r1`]],
  ]);
  expect(nodeOf(root, blockNodeId("plan-md", "t2"))?.chips).toEqual([
    {
      answered: true,
      gone: false,
      label: "needs",
      name: "Words of t1.",
      ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
    },
  ]);
  expect(placementsOf(root, "d1").map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, "d1")?.chips).toEqual([
    {
      answered: true,
      gone: false,
      name: "R2. The second statement.",
      ref: `dispatch://${KEY}/spec#b-r2`,
      stale: false,
    },
  ]);
});

// R7: a node of a kind no step drafts makes no draft and no row, and the answer the page keeps
// (`reads.tree`) still holds every node whole - a lens, a risk under it and the catalogue's
// constraint among them, each with its state and its visibility - for the parts that draw them.
test("a node of a kind the page does not draw reaches reads.tree and draws no row", () => {
  const reads: IssueTreeReads = {
    issue: issueDetails(),
    tree: answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        node("other-1", UNDRAWN, "r1", { state: "overtaken", visibility: "author" }),
        node("lens-1", "lens", "r1", { visibility: "author" }),
        node("risk-1", "risk", "lens-1", { state: "proposed" }),
        node("constraint-1", "constraint", null, { mark: "context" }),
      ],
      [tuple("constrained_by", "r1", "constraint-1")]
    ),
  };
  const held = structuredClone(reads.tree);
  const root = buildIssueTree(reads);

  expect(nodesOf(root).filter((one) => one.readId === "other-1" || one.id === "other-1")).toEqual(
    []
  );
  expect(reads.tree).toEqual(held);
});

// R1 (plan's map, `id`): a typed block of this issue keeps the outline's `<slug>#b-<id>` from its
// `document`, an ask the outline's id from the block the asks read names, a comment, a message
// and a child issue their own ids, the issue its key, and a node of the issue's own tree with no
// document the read's id, so every reader of a node's id reads the tree as it read the outline's.
test("a block keeps F's id where the read names its document", () => {
  const tree = answer([
    block("r1", "requirement", KEY, SPEC, 0),
    block("c1", "check", "r1", VERIFICATION, 0),
    node("q1", "ask", KEY),
    node("q2", "ask", KEY),
    node("c-1", "comment", KEY),
    node("m-1", "message", KEY),
    node("CORE-2", "issue", KEY, { text: "A child of the issue" }),
    node("d-node", "decision", KEY, { kept: null }),
  ]);
  const reads: IssueTreeReads = {
    asks: [question("q1", { artifact_id: SPEC.id, block_id: "q-block" }), question("q2")],
    issue: issueDetails(),
  };
  const ids = [
    KEY,
    blockNodeId("spec", "r1"),
    blockNodeId("verification-md", "c1"),
    blockNodeId("spec", "q-block"),
    "q2",
    "c-1",
    "m-1",
    "CORE-2",
    "d-node",
  ];

  expect(tree.nodes.map((one) => treeIdOf(tree, reads)(one.id))).toEqual(ids);
  expect([...drawn(tree, reads).drafts.keys()]).toEqual(ids.slice(1));
});

// R1 (plan's map, `id`): a block of another issue keeps the read's id even where its
// document carries this issue's slug, as a child's Verification document does, so it is never
// taken for this issue's block; and a check of an issue under this one, which the read reaches
// through its `serves`, is out of this part's scope and draws no row, here or under the
// requirement it serves.
test("another issue's block keeps the read's id where its document has this issue's slug", () => {
  const tree = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      block("c1", "check", "r1", VERIFICATION, 0),
      node("CORE-2", "issue", KEY, { text: "A child of the issue" }),
      node("c9", "check", "CORE-2", {
        document: {
          artifact_id: "artifact-child-verification",
          position: 0,
          slug: "verification-md",
        },
        issue: "CORE-2",
      }),
      node("result-9", "result", "c9", { issue: "CORE-2" }),
    ],
    [tuple("serves", "c1", "r1"), tuple("serves", "c9", "r1")]
  );
  const reads = { issue: issueDetails() };
  const { drafts, root } = drawn(tree, reads);

  expect(treeIdOf(tree, reads)("c9")).toBe("c9");
  expect([...drafts.entries()].map(([id, draft]) => [id, draft.readId])).toEqual([
    [blockNodeId("spec", "r1"), "r1"],
    [blockNodeId("verification-md", "c1"), "c1"],
    ["CORE-2", "CORE-2"],
  ]);
  expect(nodeOf(root, blockNodeId("spec", "r1"))?.children.map((child) => child.id)).toEqual([
    blockNodeId("verification-md", "c1"),
  ]);
});

// R3: a person's comment the read hangs under a decision node, made after the decision's words
// last changed, is its Change: the decision reads Change requested and waits on its agent, not
// on the reader. A person's comment older than its words, and an agent's, stand under it too,
// and neither is a Change, so without the newer one the decision waits for the reader's review.
test("a Change is the comment the read hangs under its decision", () => {
  const decision = block("d1", "decision", KEY, SPEC, 1, {
    changed_at: "2026-11-05T00:00:00Z",
    kept: null,
    links: [linkTo(`dispatch://${KEY}/spec#b-r1`)],
    replaced: false,
    waiting_review: true,
  });
  const drawnDecision = blockNodeId("spec", "d1");
  const earlier = [
    block("r1", "requirement", KEY, SPEC, 0),
    decision,
    personsComment("c-older", "d1", "2026-11-04T00:00:00Z"),
    node("c-agent", "comment", "d1", {
      author: [{ id: "planner", kind: "session" }],
      made_at: "2026-11-07T00:00:00Z",
    }),
  ];
  const reads: IssueTreeReads = {
    coverage: coverage([requirement("r1")]),
    issue: issueDetails(),
    viewer: "alice",
  };
  const changed = answer([...earlier, personsComment("c-change", "d1", "2026-11-06T00:00:00Z")]);
  const { root } = drawn(changed, reads);

  expect([...decisionNodeChanges(changed)].map(([id, comment]) => [id, comment.id])).toEqual([
    ["d1", "c-change"],
  ]);
  expect(
    ["c-older", "c-agent", "c-change"].map((id) => placementsOf(root, id).map((one) => one.parent))
  ).toEqual([[drawnDecision], [drawnDecision], [drawnDecision]]);
  expect(nodeOf(root, drawnDecision)).toMatchObject({ state: "change_requested", waiting: false });

  const unchanged = answer(earlier);
  expect(decisionNodeChanges(unchanged).size).toBe(0);
  expect(nodeOf(drawn(unchanged, reads).root, drawnDecision)).toMatchObject({
    state: "waiting_review",
    waiting: true,
  });
});

// R2's second difference: an ask block carrying `shaped` ties draws them as chips on its row in
// the outline `buildIssueTree` draws, each naming a node the ask shaped by that node's line,
// beside how the asks read says it stands.
test("an ask's shaped ties are chips on its row, each naming a node the ask shaped", () => {
  const root = buildIssueTree({
    asks: [question("ask-1", { artifact_id: SPEC.id, block_id: "q1", state: "answered" })],
    issue: issueDetails(),
    tree: answer(
      [
        block("q1", "ask", KEY, SPEC, 0),
        block("r1", "requirement", KEY, SPEC, 1),
        block("r2", "requirement", KEY, SPEC, 2),
      ],
      [tuple("shaped", "q1", "r1"), tuple("shaped", "q1", "r2")]
    ),
  });

  expect(nodeOf(root, blockNodeId("spec", "q1"))).toMatchObject({
    chips: [
      {
        answered: true,
        gone: false,
        name: "Words of r1.",
        ref: `dispatch://${KEY}/artifact/spec#b-r1`,
      },
      {
        answered: true,
        gone: false,
        name: "Words of r2.",
        ref: `dispatch://${KEY}/artifact/spec#b-r2`,
      },
    ],
    reference: `dispatch://${KEY}/ask/ask-1`,
    state: "answered",
  });
});

// One rule addresses a node for the outline and for every reader of a decision
// (`decision-nodes.ts`' `nodeReference` and `askOfNode`): a decision's address is the address the
// outline's draft of the same node carries, so Keep and Change, the review list and the count
// find a decision by the address its row was drawn with, and each tie of a person's decision
// names the node it reaches by the address that node's row carries. A decision of the spec, one
// of the decisions document and one of another issue, each addressed in its document; an ask
// block and an ask of the conversation that the asks read says are answered and that carry a
// `shaped` tie, each addressed as the ask it is; an answered ask block whose asks-read row names
// the block's document by its slug alone, its id there being none the answer carries; and an
// open ask block that only the issue read's open asks hold, which a tie names as the ask it is.
test("a decision node's address is the outline's", () => {
  const answered = {
    at: "2026-11-06T12:00:00Z",
    selected: [],
    text: "All of them.",
    user: "alice",
  };
  const tree = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      block("d1", "decision", KEY, SPEC, 1, { kept: null, waiting_review: true }),
      block("d2", "decision", KEY, DECISIONS, 0, { kept: null, waiting_review: true }),
      node("d3", "decision", KEY, {
        document: { artifact_id: "artifact-core-2-spec", position: 0, slug: "spec" },
        issue: "CORE-2",
        kept: null,
        waiting_review: true,
      }),
      block("q1", "ask", KEY, SPEC, 2),
      node("ask-2", "ask", KEY),
      block("q3", "ask", KEY, SPEC, 3),
      block("q4", "ask", KEY, SPEC, 4),
    ],
    [
      tuple("shaped", "q1", "r1"),
      tuple("shaped", "ask-2", "r1"),
      tuple("shaped", "q3", "r1"),
      tuple("shaped", "q3", "q4"),
    ]
  );
  const reads: IssueTreeReads = {
    asks: [
      question("ask-1", {
        answer: answered,
        artifact_id: SPEC.id,
        block_id: "q1",
        state: "answered",
      }),
      question("ask-2", { answer: answered, state: "answered" }),
      question("ask-3", {
        answer: answered,
        block_artifact: { id: "artifact-spec-elsewhere", primary: true, slug: SPEC.slug },
        block_id: "q3",
        state: "answered",
      }),
    ],
    issue: {
      ...issueDetails(),
      open_asks: [
        { ...question("ask-4", { artifact_id: SPEC.id, block_id: "q4" }), last_reply: null },
      ],
    },
  };
  const decisions = decisionNodes(tree, reads);
  const idOf = treeIdOf(tree, reads);
  const { drafts } = drawn(tree, reads);

  expect(decisions.map((one) => one.ref)).toEqual([
    `dispatch://${KEY}/artifact/spec#b-d1`,
    `dispatch://${KEY}/artifact/decisions-md#b-d2`,
    "dispatch://CORE-2/artifact/spec#b-d3",
    `dispatch://${KEY}/ask/ask-1`,
    `dispatch://${KEY}/ask/ask-2`,
    buildDispatchReference({ id: "ask-3", key: KEY, kind: "ask" }),
  ]);
  expect(decisions.map((one) => drafts.get(idOf(one.node.id))?.reference)).toEqual(
    decisions.map((one) => one.ref)
  );
  const ties = decisions.flatMap((one) => one.links ?? []);
  expect(ties.map((link) => link.to.ref)).toContain(
    buildDispatchReference({ id: "ask-4", key: KEY, kind: "ask" })
  );
  expect(ties.map((link) => link.to.ref)).toEqual(
    ties.map((link) => drafts.get(idOf(link.to.id))?.reference)
  );
});

// R5: a role's blocks come from the tree read in the shape the blocks read answered them: the
// blocks of that role's document in their `document.position` order, each with its attributes -
// a list of pointers as `block[]` - and the asks and comments the read hangs under it; and, on an
// issue that has converted whose blocks name no document, the nodes of this issue the role's
// query takes, in the walk's order.
test("blocksOf answers a role's typed blocks in the blocks read's shape", () => {
  const issue = issueDetails();
  const unconverted = answer(
    [
      block("r2", "requirement", KEY, SPEC, 1, {
        attributes: { serves: [`dispatch://${KEY}/spec#b-r1`] },
      }),
      block("r1", "requirement", KEY, SPEC, 0),
      personsComment("c1", "r1", "2026-11-05T00:00:00Z"),
      node("q1", "ask", "r1"),
      block("t1", "plan_task", KEY, PLAN, 0),
    ],
    [],
    { converted: false, documents: { spec: SPEC.id }, tree: null }
  );

  // Each block carries its words beside the blocks read's members, which `ArtifactBlock` lacks.
  expect<unknown>(blocksOfRole(unconverted, "spec", issue)).toEqual([
    {
      attributes: [],
      from: 0,
      id: "r1",
      references: { asks: 1, comments: 1 },
      text: "Words of r1.",
      text_tree: reading("Words of r1."),
      to: 0,
      token: "",
      type: "requirement",
    },
    {
      attributes: [{ kind: "block[]", name: "serves", value: `dispatch://${KEY}/spec#b-r1` }],
      from: 1,
      id: "r2",
      references: { asks: 0, comments: 0 },
      text: "Words of r2.",
      text_tree: reading("Words of r2."),
      to: 1,
      token: "",
      type: "requirement",
    },
  ]);

  const converted = answer([
    node("criterion-1", "criterion", KEY),
    node("r1", "requirement", "criterion-1"),
    node("r-above", "requirement", "CORE-0", { issue: "CORE-0" }),
  ]);
  expect(blocksOfRole(converted, "spec", issue)?.map((one) => [one.id, one.from])).toEqual([
    ["criterion-1", 1],
    ["r1", 2],
  ]);
});

// The read id and the context a later part's step or mark reads: every draft
// carries the answer's own id, which the tree's node keeps on each of its placements, and the
// context answers a node's tree id and its line once every node has been offered, and nothing of
// a node no draft stands for.
test("a draft carries its read id, and the context answers its tree id and its line", () => {
  const tree = answer([
    block("r1", "requirement", KEY, SPEC, 0, {
      text_tree: reading("R1. Draw the tree from one read. Then the rest."),
    }),
    node("other-1", UNDRAWN, "r1"),
    personsComment("c1", "other-1", "2026-11-05T00:00:00Z"),
  ]);
  const seen: unknown[][] = [];
  const probe: TreeDraftMark = (draft, read, _answer, context) => {
    seen.push([
      draft.readId,
      context.treeIdOf(read.id),
      context.lineOf(read.id),
      context.drafted(read.id),
    ]);
    if (read.id === "c1") {
      seen.push([
        context.nodeOf("other-1")?.supertag,
        context.treeIdOf("other-1"),
        context.lineOf("other-1"),
        context.drafted("other-1"),
      ]);
    }
  };
  // A later part adds its mark to the list with one line; this case adds the probe for its run.
  const marks = treeDraftMarks as TreeDraftMark[];
  marks.push(probe);
  try {
    const { root } = drawn(tree);

    expect(seen).toEqual([
      ["r1", blockNodeId("spec", "r1"), "R1. Draw the tree from one read. Then the rest.", true],
      ["c1", "c1", "Words of c1.", true],
      [UNDRAWN, "other-1", undefined, false],
    ]);
    expect([nodeOf(root, blockNodeId("spec", "r1"))?.readId, nodeOf(root, "c1")?.readId]).toEqual([
      "r1",
      "c1",
    ]);
  } finally {
    marks.splice(marks.indexOf(probe), 1);
  }
});

// R1: the tree read asks for its first page, then for the page its cursor names, in turn, and
// answers once the last page has answered, the pages merged, so the tree is built once and never
// from part of the walk: while the second page is out the read holds no answer. The tree then
// holds each node of both pages once, a node of the second page under its parent on the first,
// and a tuple whose near end is on the second page places it.
test("the cursor's pages are read in turn and the tree is built once", async () => {
  const first: TreeAnswer = {
    ...answer([block("r1", "requirement", KEY, SPEC, 0), node("c1", "comment", "r1")]),
    cursor: "page-2",
  };
  const second: TreeAnswer = {
    converted: true,
    cursor: null,
    nodes: [
      block("t1", "plan_task", "r1", PLAN, 0),
      node("c2", "comment", "r1", { made_at: "2026-11-06T00:00:00Z" }),
    ],
    tree: "artifact-tree",
    tuples: [tuple("serves", "t1", "r1")],
    view_of: null,
  };
  // The second page answers when the case releases it, and says when it was asked for.
  const later = Promise.withResolvers<TreeAnswer>();
  const asked = Promise.withResolvers<void>();
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, page) => {
    if (page?.cursor === undefined) return first;
    asked.resolve();
    return later.promise;
  });
  try {
    const client = new QueryClient();
    const query = issueTreeQuery(KEY);
    const reading = client.fetchQuery(query);
    await asked.promise;
    expect(read.mock.calls.map(([key, page]) => [key, page])).toEqual([
      [KEY, {}],
      [KEY, { cursor: "page-2" }],
    ]);
    expect(read.mock.calls.every(([, , init]) => init?.signal instanceof AbortSignal)).toBe(true);
    expect(client.getQueryData(query.queryKey)).toBeUndefined();

    later.resolve(second);
    const root = buildIssueTree({ issue: issueDetails(), tree: await reading });
    expect(read).toHaveBeenCalledTimes(2);
    expect(nodesOf(root).map((one) => one.id)).toEqual([
      KEY,
      blockNodeId("spec", "r1"),
      "c1",
      blockNodeId("plan-md", "t1"),
      "c2",
    ]);
  } finally {
    read.mockRestore();
  }
});

// The tree read honours TanStack's abort signal, so a read given up - a reset, the last observer
// gone - asks for no next page, and a walk of many pages stops there.
test("a read given up asks for no next page", async () => {
  const first: TreeAnswer = {
    ...answer([block("r1", "requirement", KEY, SPEC, 0)]),
    cursor: "page-2",
  };
  const out = Promise.withResolvers<TreeAnswer>();
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, page) =>
    page?.cursor === undefined ? out.promise : answer([])
  );
  try {
    const client = new QueryClient();
    const query = issueTreeQuery(KEY);
    const reading = client.fetchQuery(query).catch(() => undefined);
    await client.cancelQueries({ queryKey: query.queryKey });
    await reading;
    out.resolve(first);
    // The walk goes on in a promise TanStack no longer hands out, so the case waits for the turn
    // of the event loop after it: every step of the walk up to its next request is a microtask.
    const turn = Promise.withResolvers<void>();
    setTimeout(turn.resolve, 0);
    await turn.promise;
    expect(read).toHaveBeenCalledTimes(1);
    expect(client.getQueryData(query.queryKey)).toBeUndefined();
  } finally {
    read.mockRestore();
  }
});

/** Reads the tree once through `issueTreeQuery`, with every page `serve` answers for the cursor
 *  it is asked with. The server answers at most `bound` pages and then refuses, so a walk that
 *  never ends fails the case rather than holding it. */
async function walkWith(serve: (cursor: string | undefined) => TreeAnswer, bound = 10) {
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, page) => {
    if (read.mock.calls.length > bound) throw new Error(`more than ${bound} page reads`);
    return serve(page?.cursor);
  });
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const outcome = await client.fetchQuery(issueTreeQuery(KEY)).then(
      (merged) => ({ nodes: merged.nodes.map((one) => one.id) }),
      (error: unknown) => ({ error: String(error) })
    );
    return { cursors: read.mock.calls.map(([, page]) => page?.cursor), outcome };
  } finally {
    read.mockRestore();
  }
}

// A page that names no next page is the last one, however the server writes "none": a server
// whose paged answers leave an empty cursor out of the body, as Go's `omitzero` does, or write it
// as the empty string, sends one page, and the walk ends there instead of asking for the first
// page again, without end.
test("a page that names no next page ends the walk, the cursor left out or empty", async () => {
  const only = answer([block("r1", "requirement", KEY, SPEC, 0)]);
  const { cursor: _none, ...leftOut } = only;
  for (const last of [leftOut as TreeAnswer, { ...only, cursor: "" }]) {
    expect(await walkWith(() => last)).toEqual({
      cursors: [undefined],
      outcome: { nodes: [KEY, "r1"] },
    });
  }
});

// A server that names the same next page again never ends a walk that follows it: the walk asks
// for each page at most once and ends, with an answer or a failure, instead of asking without
// end.
test("a cursor the server names twice ends the walk", async () => {
  const { cursors, outcome } = await walkWith((cursor) =>
    cursor === undefined
      ? { ...answer([block("r1", "requirement", KEY, SPEC, 0)]), cursor: "page-2" }
      : { ...answer([]), cursor: "page-2", nodes: [node("c1", "comment", "r1")] }
  );
  expect(cursors).toEqual([undefined, "page-2"]);
  expect(JSON.stringify(outcome)).not.toContain("page reads");
});

/** Each row of `root` as its depth in dashes, its id and its kind, every branch open. */
function rowsOf(root: TreeNode, depth = 0): string[] {
  return root.children.flatMap((child) => [
    `${"-".repeat(depth)}${child.id} ${child.type}`,
    ...rowsOf(child, depth + 1),
  ]);
}

// A block id is unique only inside its document, and an issue that has not converted can hold
// one id in two documents: each block is a row of its own, by its document's id for it, and a
// comment the read hangs under the second stands under that one, the nearest node before it of
// the id its parent names.
test("two documents' blocks with one block id are two rows, each its own document's", () => {
  const { root } = drawn(
    answer(
      [
        block("a1", "requirement", KEY, SPEC, 0),
        block("a1", "plan_task", KEY, PLAN, 0),
        personsComment("c1", "a1", "2026-11-06T00:00:00Z"),
      ],
      [],
      { converted: false, documents: { plan: PLAN.id, spec: SPEC.id }, tree: null }
    )
  );

  expect(rowsOf(root)).toEqual([
    `${blockNodeId("spec", "a1")} requirement`,
    `${blockNodeId("plan-md", "a1")} plan_task`,
    "-c1 comment",
  ]);
});

// The same two blocks, and a comment the read hangs under the plan's task: the blocks of each
// role count the comments hung under that very node, so the spec's statement of the same id
// counts none.
test("a role's block counts the comments hung under it, never another document's block of its id", () => {
  const tree = answer(
    [
      block("a1", "requirement", KEY, SPEC, 0),
      block("a1", "plan_task", KEY, PLAN, 0),
      personsComment("c1", "a1", "2026-11-06T00:00:00Z"),
    ],
    [],
    { converted: false, documents: { plan: PLAN.id, spec: SPEC.id }, tree: null }
  );
  const issue = issueDetails();
  const comments = (role: "plan" | "spec") =>
    blocksOfRole(tree, role, issue)?.map((one) => [one.id, one.references.comments]);

  expect({ plan: comments("plan"), spec: comments("spec") }).toEqual({
    plan: [["a1", 1]],
    spec: [["a1", 0]],
  });
});

// A child's Verification document carries its parent's slug, and the parent's read holds the
// child's check beside the parent's own when they share a block id: the parent's check keeps its
// document's id and its place under the statement it serves, and the child's draws no row.
test("this issue's check keeps its document's id beside a child's check of the same block id", () => {
  const tree = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      block("c1", "check", "r1", VERIFICATION, 0),
      node("CORE-2", "issue", KEY, { text: "A child of the issue" }),
      node("c1", "check", "CORE-2", {
        document: {
          artifact_id: "artifact-child-verification",
          position: 0,
          slug: "verification-md",
        },
        issue: "CORE-2",
      }),
    ],
    [tuple("serves", "c1", "r1")],
    { converted: false, documents: { spec: SPEC.id, verification: VERIFICATION.id }, tree: null }
  );
  const reads = { issue: issueDetails() };

  expect(treeIdOf(tree, reads)("c1")).toBe(blockNodeId("verification-md", "c1"));
  expect(rowsOf(drawn(tree, reads).root)).toEqual([
    `${blockNodeId("spec", "r1")} requirement`,
    `-${blockNodeId("verification-md", "c1")} check`,
    "CORE-2 child_issue",
  ]);
});

// A node's role is read node by node: on an issue that has converted, a statement written in the
// spec carries the spec's `document`, and a criterion, written into the issue's own tree, names
// none and plays the role whose query takes its kind, so the spec's blocks hold both, in the
// walk's order; a node of another issue plays no role of this one.
test("a converted issue's spec blocks hold its criteria beside the statements its spec names", () => {
  const tree = answer([
    node("criterion-1", "criterion", KEY),
    block("r1", "requirement", "criterion-1", SPEC, 0),
    node("r-above", "requirement", "CORE-0", { issue: "CORE-0" }),
    block("t1", "plan_task", KEY, PLAN, 0),
  ]);
  const issue = { ...issueDetails(), plan_artifact_id: PLAN.id };

  expect(blocksOfRole(tree, "spec", issue)?.map((one) => [one.id, one.from])).toEqual([
    ["criterion-1", 1],
    ["r1", 2],
  ]);
  expect(blocksOfRole(tree, "plan", issue)?.map((one) => [one.id, one.from])).toEqual([["t1", 0]]);
});

// A node that names its document plays that document's role and no other: a requirement block
// of the decisions document, which plays no role a view reads, is no block of the spec's, though
// the spec's query takes its kind, while a criterion that names no document plays the spec's
// role by its kind alone.
test("a block of a document that plays no role is no role's block, whatever its kind", () => {
  const tree = answer([
    block("r9", "requirement", KEY, DECISIONS, 0),
    node("criterion-1", "criterion", KEY),
    block("r1", "requirement", "criterion-1", SPEC, 0),
  ]);

  expect(blocksOfRole(tree, "spec", issueDetails())?.map((one) => [one.id, one.from])).toEqual([
    ["criterion-1", 2],
    ["r1", 3],
  ]);
});

// On an issue that has converted, a check that names no document plays Verification by its kind,
// so it is a Verification block, and the results read's row of its block in the Verification
// document tells about it: its row carries that row's address, keyed by the check's own id.
test("a converted issue's check that names no document is Verification's and draws its results row", () => {
  const issue = { ...issueDetails(), verification_artifact_id: VERIFICATION.id };
  const check: CheckBlock = {
    artifact_id: VERIFICATION.id,
    block_id: "c1",
    compared_with: null,
    issue_key: KEY,
    negative_control: "",
    pass_stands: { answer: "yes" },
    ref: `dispatch://${KEY}/artifact/verification-md#b-c1`,
    results: [],
    run: "bun test",
    sha256: "sum",
    surface: "web",
    text: "Words of c1.",
    version: 1,
    when: "always",
  };
  const tree = answer([block("r1", "requirement", KEY, SPEC, 0), node("c1", "check", "r1")]);
  const reads: IssueTreeReads = {
    issue,
    results: { checks: [check], issue: KEY, verification_artifact_id: VERIFICATION.id },
  };

  expect({
    blocks: blocksOfRole(tree, "verification", issue)?.map((one) => one.id),
    reference: nodeOf(drawn(tree, reads).root, "c1")?.reference,
  }).toEqual({ blocks: ["c1"], reference: check.ref });
});

// On an issue that has converted, a statement carries the spec's `document`, so it is drawn by the
// spec's id for it and the coverage read's row of that block tells about it: the row keeps that
// row's counts and address.
test("a converted issue's statement keeps the coverage read's counts and address", () => {
  const tree = answer([block("r1", "requirement", KEY, SPEC, 0)]);
  const reads: IssueTreeReads = {
    coverage: coverage([requirement("r1")]),
    issue: issueDetails(),
  };
  const statement = nodeOf(drawn(tree, reads).root, blockNodeId("spec", "r1"));

  expect({
    counts: statement?.counts,
    reference: statement?.reference,
    statement: statement?.statement,
  }).toEqual({
    counts: { checks: 1, planTasks: 1 },
    reference: `dispatch://${KEY}/spec#b-r1`,
    statement: true,
  });
});

// Owed: a tuple names its two ends by the read's bare id, so where this issue's plan task `t1`
// and a child's plan task `t1` are both in the read, the child's `serves` tuple cannot be told
// from the parent's, and the parent's task is placed again under the statement only the child's
// task serves. It waits on the model naming a tuple's ends by each node's own identity.
test.skip("this issue's plan task is not placed by a child's plan task of the same block id @owed", () => {
  const tree = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      block("t1", "plan_task", "r1", PLAN, 0),
      block("r2", "requirement", KEY, SPEC, 1),
      node("CORE-2", "issue", KEY, { text: "A child of the issue" }),
      node("t1", "plan_task", "CORE-2", {
        document: { artifact_id: "artifact-child-plan", position: 0, slug: "plan-md" },
        issue: "CORE-2",
      }),
    ],
    [tuple("serves", "t1", "r1"), tuple("serves", "t1", "r2")],
    { converted: false, documents: { plan: PLAN.id, spec: SPEC.id }, tree: null }
  );

  expect(rowsOf(drawn(tree).root)).toEqual([
    `${blockNodeId("spec", "r1")} requirement`,
    `-${blockNodeId("plan-md", "t1")} plan_task`,
    `${blockNodeId("spec", "r2")} requirement`,
    "CORE-2 child_issue",
  ]);
});

// Owed: a tuple names its two ends by the read's bare id, so where the spec's statement `a1` and
// the plan's task `a1` are both in the read, a check's `serves` tuple to `a1` cannot be told from
// one to the task, the first the walk reaches: the check is not placed again under the statement
// it serves, and draws a gap for serving no requirement. It waits on the model naming a tuple's
// ends by each node's own identity.
test.skip("a check serving a block id two of this issue's documents share stands under the statement it serves @owed", () => {
  const tree = answer(
    [
      block("a1", "plan_task", KEY, PLAN, 0),
      block("r1", "requirement", KEY, SPEC, 0),
      block("c1", "check", "r1", VERIFICATION, 0),
      block("a1", "requirement", KEY, SPEC, 1),
    ],
    [tuple("serves", "c1", "r1"), tuple("serves", "c1", "a1")],
    {
      converted: false,
      documents: { plan: PLAN.id, spec: SPEC.id, verification: VERIFICATION.id },
      tree: null,
    }
  );
  const check = blockNodeId("verification-md", "c1");
  const { root } = drawn(tree);

  expect({
    gaps: nodeOf(root, check)?.gaps,
    parents: placementsOf(root, check).map((one) => one.parent),
  }).toEqual({ gaps: [], parents: [blockNodeId("spec", "r1"), blockNodeId("spec", "a1")] });
});

// The asks read says the ask `ask-1` was indexed from the spec's ask block `q-block`, so the two
// are one row, in the block's place: what the ask node shaped is named on that row as chips, as
// what the block shaped is, and what the read hangs under the ask stands under that row.
test("an ask drawn in its block's row names what the ask shaped there", () => {
  const tree = answer(
    [
      block("q-block", "ask", KEY, SPEC, 0),
      block("r1", "requirement", KEY, SPEC, 1),
      node("ask-1", "ask", KEY),
      personsComment("c-under-ask", "ask-1", "2026-11-06T00:00:00Z"),
    ],
    [tuple("shaped", "ask-1", "r1")],
    { converted: false, documents: { spec: SPEC.id }, tree: null }
  );
  const reads: IssueTreeReads = {
    asks: [question("ask-1", { artifact_id: SPEC.id, block_id: "q-block" })],
    issue: issueDetails(),
  };
  const { root } = drawn(tree, reads);
  const row = nodeOf(root, blockNodeId("spec", "q-block"));
  const shaped = nodeOf(root, blockNodeId("spec", "r1"))?.reference ?? "";

  expect(shaped, "the statement has an address").not.toBe("");
  expect({
    children: row?.children.map((child) => child.id),
    chips: row?.chips.map((chip) => chip.ref),
  }).toEqual({
    children: ["c-under-ask"],
    chips: [shaped],
  });
});
