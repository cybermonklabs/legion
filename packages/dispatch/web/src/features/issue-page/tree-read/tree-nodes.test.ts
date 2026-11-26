import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { api } from "../../../api/client";
import { routeUrl } from "../../../api/paths";
import { CONVERSATION_KINDS } from "../../../api/queries";
import type {
  Artifact,
  Ask,
  CheckBlock,
  Comment,
  Decision,
  DecisionLink,
  DisplayTree,
  IssueCoverage,
  IssueDetails,
  RequirementCoverage,
} from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { decisionChanges, decisionNodeChanges } from "../decision-review";
import { decisionsRead } from "../decisions-filter/decisions-fixtures";
import {
  assemble,
  buildIssueTree,
  type IssueTreeReads,
  lineWordsRaw,
  nodeWords,
  type TreeNode,
  treeWordsOf,
} from "../issue-tree";
import { GLYPH_TOKENS, NODE_TYPE_NAMES } from "../page-words";
import { presetView } from "../presets";
import { drawnChip, nameOf } from "../projection-fixture";
import { blockNodeId } from "../tree-drafts";
import {
  pagedAnswer,
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
  treeTuple as tuple,
} from "./tree-fixtures";
import {
  blocksOfRole,
  holdsPlanTask,
  nextTreeCursor,
  nodesRemaining,
  type TreeDraftMark,
  treeDraftMarks,
  treeDraftsFrom,
  treeIdOf,
} from "./tree-nodes";
import {
  issueTreeQuery,
  outlineQuery,
  readNextTreePage,
  readOlderComments,
  type TreeAnswer,
  type TreeAnswerNode,
  type TreeTuple,
  viewQuery,
} from "./tree-read";

/**
 * The tree read's answer as the outline's drafts. Each case writes the answer as the route sends
 * it (`tree-fixtures.ts`): every node with its `parent`, in the read's walk order, a typed block
 * with the `document` it is written in, and the tuples between them. The tree is built from the
 * drafts as `buildIssueTree` builds it, with `assemble`, the outline's word rules and its reading
 * of the issue's names.
 */

const KEY = "CORE-1";

// A chip is named from its reading through the shared projection, which loads its engine once.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** A supertag no part's step drafts, so a node of it is drawn by no row whatever later parts add
 *  to `treeDraftSteps` (later work drafts a lens, a finding, a risk, a constraint and a
 *  criterion). */
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

/** The server's reading of a whole typed block, as a read sends it with `Dispatch-Trees: 1`: one
 *  node of the block's `type`, `attrs` on it, holding one paragraph per string. */
function typedReading(
  type: string,
  attrs: Readonly<Record<string, unknown>>,
  ...paragraphs: string[]
): DisplayTree {
  return { content: [{ attrs, content: reading(...paragraphs).content, type }], type: "doc" };
}

/** One node of the answer, its words stored and read alike unless the case says otherwise. */
function node(
  id: string,
  supertag: string,
  parent: string | null,
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  const text = `Words of ${id}.`;
  return treeNode(id, supertag, parent, { text, text_tree: reading(text), ...extra });
}

/** A typed block of one of the issue's documents, at `position` in it. */
function block(
  id: string,
  supertag: string,
  parent: string,
  document: Artifact,
  position: number,
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  return node(id, supertag, parent, {
    document: { artifact_id: document.id, position, slug: document.slug },
    ...extra,
  });
}

/** A decision node of the decisions document, which plays no role, so the read names no
 *  `document` for it. */
function decisionNode(
  id: string,
  parent: string,
  decision: TreeNodeExtra["decision"] = {},
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  return node(id, "decision", parent, { decision, in: DECISIONS.slug, ...extra });
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
  return treeAnswer([ISSUE, ...nodes], tuples, {
    converted: true,
    tree: "artifact-tree",
    ...extra,
  });
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
    changed_since_tie: false,
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

/** A decision of a decisions read of the release before: a section of `decisions.md` under
 *  `heading`, with `words` as its body, which no node of the answer holds. */
function decisionsSection(
  id: string,
  heading: string,
  words: string,
  extra: Partial<Decision> = {}
): Decision {
  return {
    asks: [],
    block_id: id,
    changed_at: "2026-11-05T00:00:00Z",
    changed_version: 1,
    heading,
    kept: null,
    kind: "agent",
    links: [],
    ref: `dispatch://${KEY}/artifact/decisions-md#b-${id}`,
    replaced: false,
    text: `## ${heading}\n\n${words}`,
    text_sha256: "sum",
    unsure: false,
    waiting_review: true,
    ...extra,
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
    return one === undefined ? undefined : [nodeWords(one), lineWordsRaw(one)];
  });
  expect(words).toEqual([
    ["Draw the tree from one read. Then the rest.", "Draw the tree from one read."],
    ["Agreed, ship it.", "Agreed, ship it."],
  ]);
});

// R1: the read sends a typed block's reading as the blocks read does, of its whole stored text:
// one node of the block's kind, its attributes on it. A row draws the block's own content from
// it, and so does a chip that names the block; the kind and the attributes are drawn apart.
test("a typed block's row and its chip draw the block's own content, not its kind and attributes", () => {
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, {
          text_tree: typedReading(
            "requirement",
            { blockId: "r1" },
            "The first statement. Its reason."
          ),
        }),
        block("t1", "plan_task", "r1", PLAN, 0, {
          text_tree: typedReading(
            "plan_task",
            { blockId: "t1", serves: `dispatch://${KEY}/spec#b-r1` },
            "Build the step."
          ),
        }),
      ],
      [tuple("serves", "t1", "r1"), tuple("needs", "t1", "r1")]
    ),
    { coverage: coverage([requirement("r1")]), issue: issueDetails() }
  );

  const step = nodeOf(root, blockNodeId("plan-md", "t1"));
  expect(step === undefined ? undefined : [nodeWords(step), step.tree]).toEqual([
    "Build the step.",
    reading("Build the step."),
  ]);
  expect(step?.chips.map(nameOf)).toEqual(["The first statement."]);
});

/** A typed block of the spec and a person's comment under it, each a new object at each call. */
const records = () =>
  answer([
    block("r1", "requirement", KEY, SPEC, 0, {
      text_tree: typedReading("requirement", { blockId: "r1" }, "The first statement."),
    }),
    personsComment("c1", "r1", "2026-11-05T09:00:00Z"),
  ]);

// Every re-read of the tree builds the drafts again, from an answer whose unchanged objects the
// cache keeps. A row handed a new reading or a new source draws its words again, and a link
// focused in them loses the focus, so drawing the same answer twice hands a typed block's row the
// same cut reading and the same source, and a comment's row the same source.
test("drawing the same answer twice hands a row the same reading and the same source", () => {
  const tree = records();
  const builds = [drawn(tree).drafts, drawn(tree).drafts];

  const [row, again] = builds.map((drafts) => drafts.get(blockNodeId("spec", "r1")));
  expect(row?.tree).toEqual(reading("The first statement."));
  expect(row?.source).toEqual({ id: SPEC.id, issue: KEY, kind: "artifact" });
  expect(again?.tree).toBe(row?.tree);
  expect(again?.source).toBe(row?.source);
  const [turn, turnAgain] = builds.map((drafts) => drafts.get("c1"));
  expect(turn?.source).toEqual({ id: "c1", issue: KEY, kind: "comment" });
  expect(turnAgain?.source).toBe(turn?.source);
});

// A row's source is kept by the object the row draws - the reading of its words - so a re-read
// whose node changed beside its words, as one whose parent was reworded changes its
// `parent.first_line`, and whose reading the cache keeps, hands the row the source it handed it
// before, and its words are not drawn again.
test("a node whose reading stays the same object keeps its source when another of its fields changes", () => {
  const before = records();
  const after: TreeAnswer = {
    ...before,
    nodes: (before.nodes ?? []).map((one) =>
      one.parent === null ? one : { ...one, parent: { ...one.parent, first_line: "Reworded." } }
    ),
  };
  const builds = [drawn(before).drafts, drawn(after).drafts];

  const [row, again] = builds.map((drafts) => drafts.get(blockNodeId("spec", "r1")));
  expect(again?.source).toEqual({ id: SPEC.id, issue: KEY, kind: "artifact" });
  expect(again?.source, "a node changed beside its words is handed a new source").toBe(row?.source);
  const [turn, turnAgain] = builds.map((drafts) => drafts.get("c1"));
  expect(turnAgain?.source).toEqual({ id: "c1", issue: KEY, kind: "comment" });
  expect(turnAgain?.source, "a comment changed beside its words is handed a new source").toBe(
    turn?.source
  );
});

// A row's source lives as long as the object it was kept by, so nothing holds a source for every
// record the session ever drew: an answer holding a new record of an id, with a new reading, as a
// re-read whose record changed does, hands its row a source of its own, never the one made for
// the record before.
test("a new record of an id is handed a new source, not the one made for the record before", () => {
  // Each build draws an answer of its own: the same records, each a new object.
  const builds = Array.from({ length: 2 }, () => drawn(records()).drafts);

  const [row, again] = builds.map((drafts) => drafts.get(blockNodeId("spec", "r1")));
  expect(again?.source).toEqual({ id: SPEC.id, issue: KEY, kind: "artifact" });
  expect(again?.source, "a new block is handed the old block's source").not.toBe(row?.source);
  const [turn, turnAgain] = builds.map((drafts) => drafts.get("c1"));
  expect(turnAgain?.source).toEqual({ id: "c1", issue: KEY, kind: "comment" });
  expect(turnAgain?.source, "a new comment is handed the old comment's source").not.toBe(
    turn?.source
  );
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
    {
      converted: false,
      documents: { plan: PLAN.id, spec: SPEC.id, verification: null },
      tree: null,
    }
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
    source: { id: PLAN.id, issue: KEY, kind: "artifact" },
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

// R4: a reading is an agent's reading of the ask or the decision it reads, and the read gives it
// that ask or decision as its parent: it is drawn as a reading, by its own words, under the
// parent the read names and nowhere else - one row each, whatever document it is written in.
test("a reading stands under the parent the read names, and nowhere else", () => {
  const { root } = drawn(
    answer([
      node("q1", "ask", KEY),
      decisionNode("d1", KEY),
      block("q1-reading", "reading", "q1", SPEC, 0),
      node("d1-reading", "reading", "d1", { in: DECISIONS.slug }),
    ])
  );
  const inSpec = blockNodeId("spec", "q1-reading");

  expect(
    nodesOf(root)
      .filter((one) => one.type === "reading")
      .map((one) => [one.id, nodeWords(one)])
  ).toEqual([
    [inSpec, "Words of q1-reading."],
    ["d1-reading", "Words of d1-reading."],
  ]);
  expect(placementsOf(root, inSpec).map((one) => one.parent)).toEqual(["q1"]);
  expect(placementsOf(root, "d1-reading").map((one) => one.parent)).toEqual(["d1"]);
});

// R10: a statement whose own `from` names one origin and a decision that says it shaped it names
// another is no gap the page draws: a gap says what work someone owes, and the two edges
// disagreeing is none. An agent's decision, an ask a person answered and an ask still open alike
// draw none.
test("a statement whose origin and whose shaping decision disagree draws no gap for it on the new page", () => {
  const origin = { from: `dispatch://${KEY}/spec#b-r-origin` };
  const { drafts } = drawn(
    answer(
      [
        block("r-origin", "requirement", KEY, SPEC, 0),
        block("r-open", "requirement", KEY, SPEC, 1, { attributes: origin }),
        block("r-answered", "requirement", KEY, SPEC, 2, { attributes: origin }),
        block("r-agent", "requirement", KEY, SPEC, 3, { attributes: origin }),
        node("q-open", "ask", KEY),
        node("q-answered", "ask", KEY, { decision: {} }),
        decisionNode("d1", KEY),
      ],
      [
        tuple("shaped", "q-open", "r-open"),
        tuple("shaped", "q-answered", "r-answered"),
        tuple("shaped", "d1", "r-agent"),
      ]
    ),
    {
      coverage: coverage(["r-origin", "r-open", "r-answered", "r-agent"].map(requirement)),
      issue: issueDetails(),
    }
  );

  expect(
    ["r-open", "r-answered", "r-agent"].map((id) => drafts.get(blockNodeId("spec", id))?.gaps)
  ).toEqual([[], [], []]);
});

/** The coverage read's row for statement `id`, which no plan task serves. */
function unplanned(id: string): RequirementCoverage {
  return { ...requirement(id), plan_tasks: 0 };
}

/** The gaps of the spec's statements `ids`, drawn from `tree` with `reads`. */
function statementGaps(
  tree: TreeAnswer,
  reads: IssueTreeReads,
  ids: readonly string[]
): (readonly string[] | undefined)[] {
  const { drafts } = drawn(tree, reads);
  return ids.map((id) => drafts.get(blockNodeId("spec", id))?.gaps);
}

// R10: a statement no plan task serves is a gap once the issue holds a plan task, since a plan is
// written after the spec is approved. Conversation's read holds no plan task, so whether one is
// held is read from the Plan view's answer; what serves the statement is the coverage read's count.
test("a statement no plan task serves is marked once the plan view holds a plan task", () => {
  const tree = answer([
    block("r1", "requirement", KEY, SPEC, 0, { checks: 1 }),
    block("r2", "requirement", KEY, SPEC, 1, { checks: 1 }),
  ]);
  const plan = answer([
    block("t1", "plan_task", KEY, PLAN, 0, {
      attributes: { serves: `dispatch://${KEY}/spec#b-r2` },
      role: "query",
    }),
  ]);
  const reads: IssueTreeReads = {
    coverage: coverage([unplanned("r1"), requirement("r2")]),
    holdsPlanTask: holdsPlanTask(plan),
    issue: issueDetails(),
  };

  expect(statementGaps(tree, reads, ["r1", "r2"])).toEqual([["no_plan_task"], []]);
});

// R10: the Plan view answers the plan tasks of every issue under the issue with its own, and the
// coverage read counts a child's task that serves a statement of this issue's spec: a parent whose
// plan stands wholly in its children marks the statement no task serves, and not the one a child's
// task serves.
test("a plan task of an issue under this one counts", () => {
  const tree = answer([
    block("r1", "requirement", KEY, SPEC, 0, { checks: 1 }),
    block("r2", "requirement", KEY, SPEC, 1, { checks: 1 }),
  ]);
  const plan = answer([
    node("CORE-2", "issue", KEY, { role: "context", text: "A child of the issue" }),
    node("t1", "plan_task", "CORE-2", {
      attributes: { serves: `dispatch://${KEY}/spec#b-r2` },
      document: { artifact_id: "artifact-child-plan", position: 0, slug: "plan-md" },
      issue: "CORE-2",
      role: "query",
    }),
  ]);
  const reads: IssueTreeReads = {
    coverage: coverage([unplanned("r1"), requirement("r2")]),
    holdsPlanTask: holdsPlanTask(plan),
    issue: issueDetails(),
  };

  expect(statementGaps(tree, reads, ["r1", "r2"])).toEqual([["no_plan_task"], []]);
});

// R10: until the issue or an issue under it holds a plan task, a statement no task serves owes
// nothing yet: not where the Plan view's answer holds none - a plan role alone, and a task gone at
// its cut - and not before that view answers, so a mark is drawn late rather than drawn and taken
// away.
test("no statement is marked for a missing plan task while no plan task exists, or before the plan view answers", () => {
  const tree = answer([block("r1", "requirement", KEY, SPEC, 0, { checks: 1 })]);
  const reads: IssueTreeReads = { coverage: coverage([unplanned("r1")]), issue: issueDetails() };
  const none = answer([
    block("role-1", "plan_role", KEY, PLAN, 0, { role: "query" }),
    block("t-gone", "plan_task", KEY, PLAN, 1, { role: "query", state: "gone" }),
  ]);

  expect(holdsPlanTask(none), "a plan role and a task gone at its cut hold a plan task").toBe(
    false
  );
  expect(statementGaps(tree, { ...reads, holdsPlanTask: holdsPlanTask(none) }, ["r1"])).toEqual([
    [],
  ]);
  expect(statementGaps(tree, reads, ["r1"])).toEqual([[]]);
});

// While a document of the issue is still being indexed, the coverage read's counts may not hold
// the plan tasks that serve a statement yet, and the server names no requirement as lacking one:
// nor does the page.
test("no statement is marked for a missing plan task while a document of the issue is still being indexed", () => {
  const owed = "plan.md version 3 is still being indexed; its coverage shows once that ends.";
  const tree = answer([block("r1", "requirement", KEY, SPEC, 0, { checks: 1 })]);
  const plan = answer([block("t1", "plan_task", KEY, PLAN, 0, { role: "query" })]);
  const reads: IssueTreeReads = {
    coverage: { ...coverage([unplanned("r1")]), gaps: [owed] },
    holdsPlanTask: holdsPlanTask(plan),
    issue: issueDetails(),
  };

  expect(statementGaps(tree, reads, ["r1"])).toEqual([[]]);
});

// R10, R27: a proposed statement is a question still open and an overtaken one is closed, so
// neither draws a gap - neither for a missing check, which the answer counts as none on an
// overtaken statement, nor for a missing plan task - where a live one draws both.
test("a proposed or an overtaken statement draws no gap for a missing check or plan task", () => {
  const tree = answer([
    block("r-live", "requirement", KEY, SPEC, 0, { checks: 0 }),
    block("r-proposed", "requirement", KEY, SPEC, 1, { checks: 0, state: "proposed" }),
    block("r-overtaken", "requirement", KEY, SPEC, 2, { checks: 0, state: "overtaken" }),
  ]);
  const plan = answer([block("t1", "plan_task", KEY, PLAN, 0, { role: "query" })]);
  const ids = ["r-live", "r-proposed", "r-overtaken"];
  const reads: IssueTreeReads = {
    coverage: coverage(ids.map((id) => ({ ...unplanned(id), checks: 0 }))),
    holdsPlanTask: holdsPlanTask(plan),
    issue: issueDetails(),
  };

  expect(statementGaps(tree, reads, ids)).toEqual([["no_check", "no_plan_task"], [], []]);
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
        decisionNode("d1", KEY, { kept: null, waiting_review: true }),
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
  expect(nodeOf(root, blockNodeId("plan-md", "t2"))?.chips.map(drawnChip)).toEqual([
    {
      answered: true,
      gone: false,
      label: "needs",
      name: "Words of t1.",
      ref: `dispatch://${KEY}/artifact/plan-md#b-t1`,
    },
  ]);
  expect(placementsOf(root, "d1").map((one) => one.parent)).toEqual([KEY]);
  expect(nodeOf(root, "d1")?.chips.map(drawnChip)).toEqual([
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
        node("constraint-1", "constraint", null, { role: "context" }),
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
    decisionNode("d-node", KEY, { kept: null }),
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

  expect((tree.nodes ?? []).map((one) => treeIdOf(tree, reads)(one.id))).toEqual(ids);
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

  expect(treeIdOf(tree, reads)("CORE-2/verification-md#c9")).toBe("c9");
  expect([...drafts.entries()].map(([id, draft]) => [id, draft.readId])).toEqual([
    [blockNodeId("spec", "r1"), `${KEY}/spec#r1`],
    [blockNodeId("verification-md", "c1"), `${KEY}/verification-md#c1`],
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
  const decision = decisionNode("d1", KEY, {
    changed_at: "2026-11-05T00:00:00Z",
    kept: null,
    links: [linkTo(`dispatch://${KEY}/spec#b-r1`)],
    replaced: false,
    waiting_review: true,
  });
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
    [`${KEY}/decisions-md#d1`, "c-change"],
  ]);
  expect(
    ["c-older", "c-agent", "c-change"].map((id) => placementsOf(root, id).map((one) => one.parent))
  ).toEqual([["d1"], ["d1"], ["d1"]]);
  expect(nodeOf(root, "d1")).toMatchObject({ state: "change_requested", waiting: false });

  const unchanged = answer(earlier);
  expect(decisionNodeChanges(unchanged).size).toBe(0);
  expect(nodeOf(drawn(unchanged, reads).root, "d1")).toMatchObject({
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

  const ask = nodeOf(root, blockNodeId("spec", "q1"));
  expect(ask).toMatchObject({ reference: `dispatch://${KEY}/ask/ask-1`, state: "answered" });
  expect(ask?.chips.map(drawnChip)).toMatchObject([
    {
      answered: true,
      gone: false,
      name: "Words of r1.",
      ref: `dispatch://${KEY}/spec#b-r1`,
    },
    {
      answered: true,
      gone: false,
      name: "Words of r2.",
      ref: `dispatch://${KEY}/spec#b-r2`,
    },
  ]);
});

// R3: a decision no node of the answer holds - a block of `decisions.md`, as the decisions read
// answers its entry (`decisionsRead`) - finds its Change by its words, with no block of the
// decisions document read: the change is the first decision whose words hold the anchor's quote.
// The read hangs the comment under the issue, since no node holds the decision, and the outline
// draws it under the decision it changes, which reads Change requested; a comment quoting words
// no decision holds is no change and stays where the read hangs it.
test("a decisions.md section finds its Change by its words", () => {
  const anchored = (id: string, quote: string): Comment => ({
    anchor: {
      artifact_id: DECISIONS.id,
      block_id: `${id}-block`,
      mark_id: id,
      orphaned: false,
      quote,
      version: 1,
    },
    answers: null,
    artifact_id: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Change ${id}.`,
    created_at: "2026-11-06T00:00:00Z",
    deleted_at: null,
    deleted_by: null,
    deliveries: [],
    edited_at: null,
    id,
    issue_key: KEY,
    mentions: [],
    reply_block: null,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  });
  const keep = decisionsSection("d1", "Keep it", "Keep the model pure.");
  const draw = decisionsSection("d2", "Draw it", "Draw it once.");
  const comments = [
    anchored("c-change", "Draw it once."),
    anchored("c-stray", "Words no decision holds."),
  ];
  const decisions = decisionsRead([keep, draw], DECISIONS);

  const changes = decisionChanges(decisions, comments, issueDetails(), undefined);
  expect([...changes].map(([ref, comment]) => [ref, comment.id])).toEqual([[draw.ref, "c-change"]]);
  const root = buildIssueTree({
    comments,
    decisions,
    issue: issueDetails(),
    tree: answer(
      comments.map((one) =>
        node(one.id, "comment", KEY, { author: [one.author], made_at: one.created_at })
      )
    ),
  });
  expect(
    ["c-change", "c-stray"].map((id) => placementsOf(root, id).map((one) => one.parent))
  ).toEqual([[blockNodeId("decisions-md", "d2")], [KEY]]);
  expect(nodeOf(root, blockNodeId("decisions-md", "d2"))?.state).toBe("change_requested");
  expect(nodeOf(root, blockNodeId("decisions-md", "d1"))?.state).toBe("waiting_review");
});

// A decision of the decisions read that no node of the answer holds is drafted beside the tree
// read's nodes, and a re-read of the same reads hands its row the words and the source it handed
// it before, as a node of the tree read's does.
test("drawing the same decisions read twice hands a section's row the same words and source", () => {
  const section = decisionsSection("d1", "Keep it", "Keep the model pure.", {
    text_tree: typedReading("decision", { blockId: "d1" }, "Keep the model pure."),
  });
  const reads = {
    decisions: decisionsRead([section], DECISIONS),
    issue: issueDetails(),
    tree: answer([]),
  };

  const [row, again] = [buildIssueTree(reads), buildIssueTree(reads)].map((root) =>
    nodeOf(root, blockNodeId("decisions-md", "d1"))
  );
  expect(row?.source).toEqual({ id: DECISIONS.id, issue: KEY, kind: "artifact" });
  expect(row?.tree).toEqual(reading("Keep the model pure."));
  expect(again?.tree, "a re-read hands the section new words").toBe(row?.tree);
  expect(again?.source, "a re-read hands the section a new source").toBe(row?.source);
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
        attributes: { serves: `dispatch://${KEY}/spec#b-r1` },
      }),
      block("r1", "requirement", KEY, SPEC, 0),
      personsComment("c1", "r1", "2026-11-05T00:00:00Z"),
      node("q1", "ask", "r1"),
      block("t1", "plan_task", KEY, PLAN, 0),
    ],
    [],
    {
      converted: false,
      documents: { plan: null, spec: SPEC.id, verification: null },
      tree: null,
    }
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
        context.nodeOf(`${KEY}/tree#other-1`)?.supertag,
        context.treeIdOf(`${KEY}/tree#other-1`),
        context.lineOf(`${KEY}/tree#other-1`),
        context.drafted(`${KEY}/tree#other-1`),
      ]);
    }
  };
  // A later part adds its mark to the list with one line; this case adds the probe for its run.
  const marks = treeDraftMarks as TreeDraftMark[];
  marks.push(probe);
  try {
    const { root } = drawn(tree);

    expect(seen).toEqual([
      [
        `${KEY}/spec#r1`,
        blockNodeId("spec", "r1"),
        "R1. Draw the tree from one read. Then the rest.",
        true,
      ],
      ["c1", "c1", "Words of c1.", true],
      [UNDRAWN, "other-1", undefined, false],
    ]);
    expect([nodeOf(root, blockNodeId("spec", "r1"))?.readId, nodeOf(root, "c1")?.readId]).toEqual([
      `${KEY}/spec#r1`,
      "c1",
    ]);
  } finally {
    marks.splice(marks.indexOf(probe), 1);
  }
});

// R1, R15: a load reads each outline read's first page, which holds the newest comment page, and
// each view's first page, and no next page, though each page names one: the answer is that page,
// which names the next (`cursor`) and how many nodes the read holds after it (`remaining`), and
// the tree is built from it, its issue marked as holding more to read.
test("the load reads one page and no next page", async () => {
  const first = pagedAnswer(
    answer([block("r1", "requirement", KEY, SPEC, 0), node("c1", "comment", "r1")]),
    "page-2",
    3
  );
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.cursor === undefined
      ? first
      : pagedAnswer(treeAnswer([node("c2", "comment", "r1")], [], { converted: true }), null)
  );
  try {
    const client = new QueryClient();
    const whole = await client.fetchQuery(issueTreeQuery(KEY));
    await client.fetchQuery(outlineQuery(KEY, "conversation"));
    await client.fetchQuery(viewQuery(KEY, "plan"));
    expect<unknown>(read.mock.calls.map(([key, query]) => [key, query])).toEqual([
      [KEY, {}],
      [KEY, { kind: [...CONVERSATION_KINDS] }],
      [KEY, { view: "plan" }],
    ]);
    expect(whole).toEqual(first);
    expect([nextTreeCursor(whole), nodesRemaining(whole)]).toEqual(["page-2", 3]);
    const root = buildIssueTree({ issue: issueDetails(), tree: whole });
    expect(nodesOf(root).map((one) => one.id)).toEqual([KEY, blockNodeId("spec", "r1"), "c1"]);
    expect(root.moreToRead).toBe(true);
  } finally {
    read.mockRestore();
  }
});

// R15: a press of the control after the last row reads the page the answer's cursor names, once
// however often it is pressed while that page is out, and merges it into the answer the page
// holds after the pages it holds, in the read's order: a node of the second page stands under its
// parent on the first, a tuple whose near end is on the second page places it, and the answer
// then names the page after it, with its count. A press once no page is left reads nothing, and
// the issue is no longer marked as holding more to read.
test("a press reads the next page and draws its rows in order", async () => {
  const whole = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      node("c1", "comment", "r1"),
      block("t1", "plan_task", "r1", PLAN, 0),
      node("c2", "comment", "r1", { made_at: "2026-11-06T00:00:00Z" }),
      node("c3", "comment", KEY, { made_at: "2026-11-07T00:00:00Z" }),
    ],
    [tuple("serves", "t1", "r1")]
  );
  const nodes = whole.nodes ?? [];
  const first = pagedAnswer({ ...whole, nodes: nodes.slice(0, 3), tuples: [] }, "page-2", 3);
  const second = pagedAnswer({ ...whole, nodes: nodes.slice(3, 5) }, "page-3", 1);
  const third = pagedAnswer({ ...whole, nodes: nodes.slice(5), tuples: [] }, null);
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.cursor === undefined ? first : query.cursor === "page-2" ? second : third
  );
  try {
    const client = new QueryClient();
    const { queryKey } = issueTreeQuery(KEY);
    await client.fetchQuery(issueTreeQuery(KEY));

    await Promise.all([
      readNextTreePage(client, KEY, "graph"),
      readNextTreePage(client, KEY, "graph"),
    ]);
    expect(read.mock.calls.map(([key, query]) => [key, query])).toEqual([
      [KEY, {}],
      [KEY, { cursor: "page-2" }],
    ]);
    const two = client.getQueryData<TreeAnswer>(queryKey);
    expect(two?.nodes?.map((one) => one.id)).toEqual(nodes.slice(0, 5).map((one) => one.id));
    expect(two === undefined ? [] : [nextTreeCursor(two), nodesRemaining(two)]).toEqual([
      "page-3",
      1,
    ]);
    const root = buildIssueTree({ issue: issueDetails(), tree: two });
    expect(nodesOf(root).map((one) => one.id)).toEqual([
      KEY,
      blockNodeId("spec", "r1"),
      "c1",
      blockNodeId("plan-md", "t1"),
      "c2",
    ]);
    expect(root.moreToRead).toBe(true);

    await readNextTreePage(client, KEY, "graph");
    const all = client.getQueryData<TreeAnswer>(queryKey);
    expect(all?.nodes?.map((one) => one.id)).toEqual(nodes.map((one) => one.id));
    expect(all === undefined ? "none" : nextTreeCursor(all)).toBeUndefined();
    expect(rowsOf(buildIssueTree({ issue: issueDetails(), tree: all }))).toEqual([
      `${blockNodeId("spec", "r1")} requirement`,
      "-c1 comment",
      `-${blockNodeId("plan-md", "t1")} plan_task`,
      "-c2 comment",
      "c3 comment",
    ]);
    expect(buildIssueTree({ issue: issueDetails(), tree: all }).moreToRead).toBeUndefined();
    await readNextTreePage(client, KEY, "graph");
    expect(read, "a press with no page left").toHaveBeenCalledTimes(3);
  } finally {
    read.mockRestore();
  }
});

// The tree read honours TanStack's abort signal, so a read given up - a reset, the last observer
// gone - asks for no next page, and a walk of many pages stops there.
test("a read given up asks for no next page", async () => {
  const first = pagedAnswer(answer([block("r1", "requirement", KEY, SPEC, 0)]), "page-2", 1);
  const second = pagedAnswer(
    treeAnswer([node("c1", "comment", "r1")], [], { converted: true }),
    null
  );
  const out = Promise.withResolvers<TreeAnswer>();
  let refetching = false;
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, page) =>
    page?.cursor === undefined ? (refetching ? out.promise : first) : second
  );
  try {
    const client = new QueryClient();
    const query = issueTreeQuery(KEY);
    await client.fetchQuery(query);
    await readNextTreePage(client, KEY, "graph");
    const held = client.getQueryData(query.queryKey);
    refetching = true;
    const reading = client.refetchQueries({ exact: true, queryKey: query.queryKey });
    await client.cancelQueries({ exact: true, queryKey: query.queryKey });
    await reading;
    out.resolve(first);
    // The walk goes on in a promise TanStack no longer hands out, so the case waits for the turn
    // of the event loop after it: every step of the walk up to its next request is a microtask.
    const turn = Promise.withResolvers<void>();
    setTimeout(turn.resolve, 0);
    await turn.promise;
    expect(read.mock.calls.map(([, page]) => page?.cursor)).toEqual([
      undefined,
      "page-2",
      undefined,
    ]);
    expect(client.getQueryData(query.queryKey) === held).toBe(true);
  } finally {
    read.mockRestore();
  }
});

// The rule every walk of the read ends by, the page's own and the e2e helpers' alike
// (`nextTreeCursor`): a page whose cursor is null, left out of the body or empty names no next
// page, and any other cursor names the next.
test("a page's cursor names no next page where it is null, left out or empty", () => {
  const page = answer([block("r1", "requirement", KEY, SPEC, 0)]);
  const { cursor: _none, ...leftOut } = page;
  const pages = [
    { ...page, cursor: null },
    leftOut as TreeAnswer,
    { ...page, cursor: "" },
    { ...page, cursor: "page-2" },
  ];
  expect(pages.map((one) => nextTreeCursor(one))).toEqual([
    undefined,
    undefined,
    undefined,
    "page-2",
  ]);
});

// A server that names a cursor it named already has a fault, and the pages past it are missing:
// the reader's press reads the page the answer names, and where that page names a cursor the read
// has asked for already, the press fails with an error that names the cursor, and the answer stays
// as it was, rather than hold that page as if the next were still to come, or read without end.
test("a cursor the server names twice fails the read, naming the cursor", async () => {
  const first = pagedAnswer(answer([block("r1", "requirement", KEY, SPEC, 0)]), "page-2", 1);
  const again = pagedAnswer(
    treeAnswer([node("c1", "comment", "r1")], [], { converted: true }),
    "page-2",
    1
  );
  const read = spyOn(api, "getIssueTree").mockImplementation(async (_key, query) =>
    query?.cursor === undefined ? first : again
  );
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { queryKey } = outlineQuery(KEY, "conversation");
    await client.fetchQuery(outlineQuery(KEY, "conversation"));
    const outcome = await readNextTreePage(client, KEY, "conversation").then(
      () => "merged",
      (error: unknown) => String(error)
    );
    expect(read.mock.calls.map(([, query]) => query?.cursor)).toEqual([undefined, "page-2"]);
    expect(outcome).toContain('the cursor "page-2" twice');
    expect(client.getQueryData<TreeAnswer>(queryKey)).toEqual(first);
  } finally {
    read.mockRestore();
  }
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
      {
        converted: false,
        documents: { plan: PLAN.id, spec: SPEC.id, verification: null },
        tree: null,
      }
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
    {
      converted: false,
      documents: { plan: PLAN.id, spec: SPEC.id, verification: null },
      tree: null,
    }
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
    {
      converted: false,
      documents: { plan: null, spec: SPEC.id, verification: VERIFICATION.id },
      tree: null,
    }
  );
  const reads = { issue: issueDetails() };

  expect(treeIdOf(tree, reads)(`${KEY}/verification-md#c1`)).toBe(
    blockNodeId("verification-md", "c1")
  );
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

// A tuple names its two ends by each node's store id, so where this issue's plan task `t1` and a
// child's plan task `t1` are both in the read, the child's `serves` tuple is told from the
// parent's, and the parent's task is not placed again under the statement only the child's task
// serves.
test("this issue's plan task is not placed by a child's plan task of the same block id", () => {
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
    [tuple("serves", "t1", "r1"), tuple("serves", "CORE-2/plan-md#t1", "r2")],
    {
      converted: false,
      documents: { plan: PLAN.id, spec: SPEC.id, verification: null },
      tree: null,
    }
  );

  expect(rowsOf(drawn(tree).root)).toEqual([
    `${blockNodeId("spec", "r1")} requirement`,
    `-${blockNodeId("plan-md", "t1")} plan_task`,
    `${blockNodeId("spec", "r2")} requirement`,
    "CORE-2 child_issue",
  ]);
});

// A tuple names its two ends by each node's store id, so where the spec's statement `a1` and the
// plan's task `a1` are both in the read, a check's `serves` tuple to the statement is told from
// one to the task: the check is placed again under the statement it serves, and draws no gap for
// serving no requirement.
test("a check serving a block id two of this issue's documents share stands under the statement it serves", () => {
  const tree = answer(
    [
      block("a1", "plan_task", KEY, PLAN, 0),
      block("r1", "requirement", KEY, SPEC, 0),
      block("c1", "check", "r1", VERIFICATION, 0),
      block("a1", "requirement", KEY, SPEC, 1),
    ],
    [tuple("serves", "c1", "r1"), tuple("serves", "c1", `${KEY}/spec#a1`)],
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
    {
      converted: false,
      documents: { plan: null, spec: SPEC.id, verification: null },
      tree: null,
    }
  );
  const reads: IssueTreeReads = {
    asks: [question("ask-1", { artifact_id: SPEC.id, block_id: "q-block" })],
    issue: issueDetails(),
  };
  const { root } = drawn(tree, reads);
  const row = nodeOf(root, blockNodeId("spec", "q-block"));

  expect({
    children: row?.children.map((child) => child.id),
    chips: row?.chips.map((chip) => chip.ref),
  }).toEqual({
    children: ["c-under-ask"],
    // A chip addresses a block of the spec as `spec#b-<id>`.
    chips: [`dispatch://${KEY}/spec#b-r1`],
  });
});

// R1, root R4: a plan task's `needs` is a chip for each task its attribute names, as the outline
// drew one for every pointer. The read answers a tuple only where it holds the far end, so a
// pointer at a task a write removed has none: its chip is named by the batched reference read,
// which answers the removed task as gone ("Deleted"); a task the read ties is named by its words.
test("a plan task's needs pointer the read ties no node to is still a chip, named by the reference read", () => {
  const needed = `dispatch://${KEY}/artifact/plan-md#b-t1`;
  const removed = `dispatch://${KEY}/artifact/plan-md#b-t9`;
  const tree = answer(
    [
      block("t1", "plan_task", KEY, PLAN, 0),
      block("t2", "plan_task", KEY, PLAN, 1, { attributes: { needs: `${needed} ${removed}` } }),
    ],
    [tuple("needs", "t2", "t1")]
  );
  const { root } = drawn(tree, {
    issue: issueDetails(),
    targets: [{ gone: true, kind: "block", name: "", ref: removed }],
  });

  expect(
    nodeOf(root, blockNodeId("plan-md", "t2"))?.chips.map((chip) => ({
      gone: chip.gone,
      label: chip.label,
      name: nameOf(chip),
      ref: chip.ref,
    }))
  ).toEqual([
    { gone: false, label: "needs", name: "Words of t1.", ref: needed },
    { gone: true, label: "needs", name: "", ref: removed },
  ]);
});

// R1, root R4, as the route answers a plan task: the store states no `block[]` attribute as a
// value, so the read sends no `needs` in a node's `attributes`, and a `needs` tuple only where the
// answer holds its far end by the address the tuple names - none for a task a write removed, and
// none where the tuple names the task by its document's address and the answer keys the node by
// the issue's tree. The pointers are listed on the block's own reading, its one typed node, as the
// blocks read listed them: each is a chip labelled "needs", a task the answer holds named by its
// words and a task a write removed named by the reference read, which answers it as gone
// ("Deleted", no address).
test("a plan task's needs the read sends on its reading alone are chips, a removed task's gone", () => {
  const needed = `dispatch://${KEY}/artifact/plan-md#b-t1`;
  const removed = `dispatch://${KEY}/artifact/plan-md#b-t9`;
  // The reading the route sends of each whole `plan_task` block: its one typed node, the block's
  // attributes on it, a `block[]` attribute as the list of its references.
  const tree = answer([
    block("t1", "plan_task", KEY, PLAN, 0, {
      attributes: {},
      text_tree: typedReading("plan_task", { blockId: "t1" }, "Words of t1."),
    }),
    block("t2", "plan_task", KEY, PLAN, 1, {
      attributes: {},
      text_tree: typedReading(
        "plan_task",
        { blockId: "t2", needs: [needed, removed] },
        "Words of t2."
      ),
    }),
  ]);
  const { root } = drawn(tree, {
    issue: issueDetails(),
    targets: [
      {
        gone: false,
        kind: "block",
        name: "Words of t1.",
        ref: needed,
        text_tree: reading("Words of t1."),
      },
      { gone: true, kind: "block", name: "", ref: removed },
    ],
  });

  expect(
    nodeOf(root, blockNodeId("plan-md", "t2"))?.chips.map((chip) => ({
      gone: chip.gone,
      label: chip.label,
      name: nameOf(chip),
      ref: chip.ref,
    })),
    "t2's needs are no chips"
  ).toEqual([
    { gone: false, label: "needs", name: "Words of t1.", ref: needed },
    { gone: true, label: "needs", name: "", ref: removed },
  ]);
});

// R1: a chip naming a block of the issue's spec addresses it as the server's reads and the
// outline's chips do, `spec#b-<id>`, which the page draws as the spec's own path; a chip naming a
// block of any other document addresses it by its slug, and a block's own row is addressed by its
// document's slug, the spec's too, as the outline addressed every block it drew.
test("a chip naming a block of the issue's spec addresses it as the spec, a row and another document's block by its slug", () => {
  const { root } = drawn(
    answer(
      [
        block("summary", "callout", KEY, SPEC, 0),
        block("t1", "plan_task", KEY, PLAN, 0),
        decisionNode("d1", KEY),
      ],
      [tuple("shaped", "d1", "summary"), tuple("shaped", "d1", "t1")]
    )
  );

  expect(
    nodeOf(root, "d1")?.chips.map((chip) => chip.ref),
    "a chip naming a block of the spec addresses it by its document's slug"
  ).toEqual([`dispatch://${KEY}/spec#b-summary`, `dispatch://${KEY}/artifact/plan-md#b-t1`]);
  expect(nodeOf(root, blockNodeId("spec", "summary"))?.reference).toBe(
    `dispatch://${KEY}/artifact/spec#b-summary`
  );
});

// R1: an `ask` block's row draws its question as the asks read holds it, as the outline drew an
// ask: the read's reading of the block is of its whole content, and the options it lists are the
// asks read's, which the row's body draws apart.
test("an ask block's row draws the asks read's question, not the options its block lists", () => {
  const words = "At which width does the identity block condense?";
  const whole = {
    content: [
      {
        attrs: { blockId: "q-open" },
        content: [
          { content: [{ text: words, type: "text" }], type: "paragraph" },
          {
            content: [
              "1024 px: the phone layout begins there.",
              "1280 px: the row keeps labels.",
            ].map((item) => ({
              content: [{ content: [{ text: item, type: "text" }], type: "paragraph" }],
              type: "list_item",
            })),
            type: "bullet_list",
          },
        ],
        type: "ask",
      },
    ],
    type: "doc",
  };
  const tree = answer([
    block("q-open", "ask", KEY, SPEC, 0, {
      text: `:::ask{#q-open}\n${words}\n:::`,
      text_tree: whole,
    }),
  ]);
  const { root } = drawn(tree, {
    asks: [
      {
        ...question("ask-q", {
          artifact_id: SPEC.id,
          block_id: "q-open",
          options: [
            { description: "the phone layout begins there.", label: "1024 px" },
            { description: "the row keeps labels.", label: "1280 px" },
          ],
          question: words,
        }),
        question_tree: reading(words),
      },
    ],
    issue: issueDetails(),
  });
  const row = nodeOf(root, blockNodeId("spec", "q-open"));

  expect(row === undefined ? undefined : [nodeWords(row), row.tree]).toEqual([
    words,
    reading(words),
  ]);
  expect(row?.options?.map((option) => option.label)).toEqual(["1024 px", "1280 px"]);
});

// R3 and R5: a document is no node, so a requirement whose stored source cites one - its
// `From:` line - carries a chip that opens it, at its own address and named by the page's word
// for a document; the source is the tree read's text of the block, whatever its words drop.
test("a requirement whose From line cites a document carries its chip from the tree read's text", () => {
  const notes = artifact("artifact-notes", "notes-md", "notes.md");
  const cites = `dispatch://${KEY}/artifact/notes-md`;
  const details = issueDetails();
  const issue = { ...details, artifacts: [...(details.artifacts ?? []), notes] };
  const words = "The measurements behind it are kept.";
  const { root } = drawn(
    answer([
      block("r-cites", "requirement", KEY, SPEC, 0, {
        text: `${words}\nFrom: ${cites}`,
        text_tree: reading(words),
      }),
    ]),
    { issue }
  );

  expect(
    nodeOf(root, blockNodeId("spec", "r-cites"))?.chips.map((chip) => ({
      name: nameOf(chip),
      ref: chip.ref,
    })),
    "the requirement draws no chip for the document its From line cites"
  ).toEqual([{ name: NODE_TYPE_NAMES.document, ref: cites }]);
});

/** A comment of the comments read, a person's on the issue, as the route answers it, which
 *  `extra` anchors to text or makes a reply. */
function commentRead(id: string, extra: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ...commentDeliveryFields(),
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: `Words of ${id}.`,
    created_at: "2026-11-05T00:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...extra,
  };
}

/** The anchor of a comment on the words of `block` in `document`. */
function anchorIn(document: Artifact, block: string): NonNullable<Comment["anchor"]> {
  return {
    artifact_id: document.id,
    block_id: block,
    mark_id: `mark-${block}`,
    orphaned: false,
    quote: `Words of ${block}.`,
    version: 1,
  };
}

// R9, R1: Conversation's read repeats `kind` for root R1's kinds, each by the name the read's
// `kind` filter gives it and never by the supertag `block`, in one order, so its key is the same
// in every load; the route writes one `kind=` for each. Graph's read is the tree read, which
// names none.
test("Conversation's read names root R1's kinds and never block", async () => {
  expect(CONVERSATION_KINDS).toEqual([
    "ask",
    "comment",
    "reading",
    "lens",
    "finding",
    "risk",
    "criterion",
    "requirement",
    "check",
    "decision",
    "issue",
    "prose",
    "callout",
  ]);
  const conversation = outlineQuery(KEY, "conversation");
  expect<unknown>(conversation.queryKey).toEqual([
    "issue-tree",
    KEY,
    { kind: [...CONVERSATION_KINDS] },
  ]);
  expect<unknown>(outlineQuery(KEY, "graph").queryKey).toEqual(["issue-tree", KEY]);
  const read = spyOn(api, "getIssueTree").mockResolvedValue(answer([]));
  try {
    const client = new QueryClient();
    await client.fetchQuery(conversation);
    await client.fetchQuery(issueTreeQuery(KEY));
    const [kinds, whole] = read.mock.calls.map(([, query]) => query);
    expect<unknown>(kinds).toEqual({ kind: [...CONVERSATION_KINDS] });
    expect(whole).toEqual({});
    expect(routeUrl("/api/v1/issues/{key}/tree", { key: KEY }, kinds)).toBe(
      `/api/v1/issues/${KEY}/tree?${CONVERSATION_KINDS.map((kind) => `kind=${kind}`).join("&")}`
    );
  } finally {
    read.mockRestore();
  }
});

// R9, R10: a comment on a node's text - one the comments read anchors and the read hangs under
// that node - is no row of either outline, and neither is a reply in its thread: both stay in the
// answer the tree is built from, for the margin to draw. A post on the issue, a comment tied to no
// text, is a row with its replies, and so is a comment anchored in a document whose block the
// answer holds no node for, which the read hangs under the issue. With no comments read nothing
// marks a comment as on text, and the read's placement stands.
test("a comment on a node's text is no row", () => {
  const tree = answer([
    block("r1", "requirement", KEY, SPEC, 0),
    personsComment("c-on", "r1", "2026-11-05T01:00:00Z"),
    personsComment("c-on-reply", "c-on", "2026-11-05T02:00:00Z"),
    personsComment("c-post", KEY, "2026-11-05T03:00:00Z"),
    personsComment("c-post-reply", "c-post", "2026-11-05T04:00:00Z"),
    personsComment("c-unplaced", KEY, "2026-11-05T05:00:00Z"),
  ]);
  const comments = [
    commentRead("c-on", { anchor: anchorIn(SPEC, "r1") }),
    commentRead("c-on-reply", { reply_to: "c-on" }),
    commentRead("c-post"),
    commentRead("c-post-reply", { reply_to: "c-post" }),
    commentRead("c-unplaced", { anchor: anchorIn(DECISIONS, "d9") }),
  ];
  const { drafts, root } = drawn(tree, { comments, issue: issueDetails() });

  expect(["c-on", "c-on-reply"].map((id) => [nodeOf(root, id), drafts.has(id)])).toEqual([
    [undefined, false],
    [undefined, false],
  ]);
  expect(
    ["c-post", "c-post-reply", "c-unplaced"].map((id) =>
      placementsOf(root, id).map((one) => one.parent)
    )
  ).toEqual([[KEY], ["c-post"], [KEY]]);
  expect(placementsOf(drawn(tree).root, "c-on").map((one) => one.parent)).toEqual([
    blockNodeId("spec", "r1"),
  ]);
});

// R15: the older comment page `?comments_before=` answers merges into the outline read's answer
// where the read's order puts each node: each comment under the issue among the issue's children
// by when it was made, after the ranked statement, a reply under its comment, and a reply to an
// ask of the newer page under that ask, which the page alone held as under the issue. The two
// controls pressed while the page is out read it once, and once no older comment remains a press
// reads nothing.
test("an older comment page merges into the answer in the read's order", async () => {
  const newest = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0, { rank: "a" }),
      node("q1", "ask", KEY, { made_at: "2026-11-05T12:00:00Z" }),
      personsComment("c-3", KEY, "2026-11-07T00:00:00Z"),
      personsComment("c-4", KEY, "2026-11-08T00:00:00Z"),
    ],
    [],
    { comments_before: "2026-11-07T00:00:00.000000Z,c-3" }
  );
  const older = treeAnswer(
    [
      personsComment("c-1", KEY, "2026-11-05T00:00:00Z"),
      personsComment("c-1-reply", "c-1", "2026-11-05T06:00:00Z"),
      personsComment("c-q", "q1", "2026-11-05T13:00:00Z"),
      personsComment("c-2", KEY, "2026-11-06T00:00:00Z"),
    ],
    [],
    { comments_before: null, converted: true }
  );
  const read = spyOn(api, "getIssueTree").mockResolvedValue(older);
  try {
    const client = new QueryClient();
    const { queryKey } = outlineQuery(KEY, "conversation");
    client.setQueryData(queryKey, newest);
    await Promise.all([
      readOlderComments(client, KEY, "conversation"),
      readOlderComments(client, KEY, "conversation"),
    ]);
    expect<unknown>(read.mock.calls.map(([key, query]) => [key, query])).toEqual([
      [KEY, { comments_before: "2026-11-07T00:00:00.000000Z,c-3", kind: [...CONVERSATION_KINDS] }],
    ]);
    const merged = client.getQueryData<TreeAnswer>(queryKey);
    expect(merged?.nodes?.map((one) => one.id)).toEqual([
      KEY,
      `${KEY}/spec#r1`,
      "c-1",
      "c-1-reply",
      "q1",
      "c-q",
      "c-2",
      "c-3",
      "c-4",
    ]);
    expect(merged?.comments_before).toBeNull();
    await readOlderComments(client, KEY, "conversation");
    expect(read, "a press with no older comment left").toHaveBeenCalledTimes(1);

    const { root } = drawn(merged ?? newest);
    expect(nodesOf(root).map((one) => one.id)).toEqual([
      KEY,
      blockNodeId("spec", "r1"),
      "c-1",
      "c-1-reply",
      "q1",
      "c-q",
      "c-2",
      "c-3",
      "c-4",
    ]);
  } finally {
    read.mockRestore();
  }
});

// R10: Graph draws every node of its read the page draws as a row, and the tree read's prose node
// - a document's plain paragraph, as the model stores it - is one: it is drafted as a row of its
// own kind, by its own words, under the parent the read gives it, and the page names that kind.
test("a prose node of the tree read is drafted and named in Graph", () => {
  const { root } = drawn(
    answer([block("r1", "requirement", KEY, SPEC, 0), node("p1", "prose", "r1")])
  );

  expect(
    nodesOf(root)
      .filter((one) => one.type === "prose")
      .map((one) => [one.id, nodeWords(one)])
  ).toEqual([["p1", "Words of p1."]]);
  expect(placementsOf(root, "p1").map((one) => one.parent)).toEqual([blockNodeId("spec", "r1")]);
  expect(NODE_TYPE_NAMES.prose).toBe("paragraph");
});

// R9, R10 and root R1: the tree read answers a plain block of the room's fragment - a paragraph,
// a list, a heading: a block whose text opens no typed block - as a block node of the supertag
// `block`, and only its `kind` filter names it prose (the server's `nodeKind`). Graph draws it as
// a prose row, and so does Conversation, whose read asks for prose and answers it so too. A
// finding - a block whose id names its lens, or whose parent is a lens, even one nothing drafts -
// and a typed block are never drafted as prose, and an ask's, a comment's or a message's own
// block stays part of its item. A block of a converted document, which names its `document`, is a
// prose row of Graph and no row of Conversation, and no filter draws a block its view read marks
// as context.
test("a plain block of the room's fragment is drafted and named as prose in Graph", () => {
  const lens = "c3000000-0000-4000-8000-000000000003";
  const orphanLens = "c3000000-0000-4000-8000-000000000004";
  const shared = [
    block("r1", "requirement", KEY, SPEC, 0),
    block("c1", "check", "r1", VERIFICATION, 0),
    node(lens, "lens", KEY),
    { ...node("f1", "block", lens), id: `lens/${lens}#f1` },
    node(orphanLens, "lens", "doc-1"),
    node("f2", "block", orphanLens),
    node("p1", "block", KEY),
    block("d1", "block", KEY, SPEC, 1),
    node("c-1", "comment", KEY),
    { ...node("cb1", "block", "c-1"), id: "comment/c-1#cb1", kind: "comment_block" },
    node("q1", "block", KEY, { issue: "CORE-2" }),
  ];
  const graph = drawn(answer([...shared, block("t1", "plan_task", KEY, PLAN, 0)]));
  const typeOf = (id: string) => graph.drafts.get(id)?.type;
  const proseRows = (root: TreeNode, preset: "conversation" | "graph") =>
    nodesOf(presetView(root, preset, "").root)
      .filter((one) => one.type === "prose")
      .map((one) => [one.id, nodeWords(one)]);

  const drafted = typeOf("p1");
  expect(drafted, "the plain block is drafted as prose").toBe("prose");
  expect(
    drafted && [NODE_TYPE_NAMES[drafted], GLYPH_TOKENS.types[drafted]],
    "the word and the shape its row draws"
  ).toEqual(["paragraph", "prose"]);
  expect(typeOf(blockNodeId("spec", "r1")), "the typed block keeps its step").toBe("requirement");
  expect(typeOf("f1"), "a finding is drafted as prose").not.toBe("prose");
  expect(typeOf("f2"), "a finding under a lens nothing drafts is drafted").toBeUndefined();
  expect(typeOf("cb1"), "a comment's own block is drafted as prose").toBeUndefined();
  expect(typeOf("c-1")).toBe("comment");
  expect(typeOf("q1"), "another issue's plain block is drafted as prose").toBeUndefined();
  expect(proseRows(graph.root, "graph"), "Graph's prose rows").toEqual([
    ["p1", "Words of p1."],
    [blockNodeId("spec", "d1"), "Words of d1."],
  ]);
  expect(placementsOf(graph.root, "p1").map((one) => one.parent)).toEqual([KEY]);

  // Conversation's read: the kinds Conversation names, which hold no plan task.
  const conversation = drawn(answer(shared));
  expect(proseRows(conversation.root, "conversation"), "Conversation's prose rows").toEqual([
    ["p1", "Words of p1."],
  ]);

  // Each filter's view read marks the plain block as context beside a node of its own kind, which
  // the filter draws; drawn from that read or read off the tree, no filter draws the block.
  const context = node("p1", "block", KEY, { role: "context" });
  const query = { role: "query" } as const;
  const own = {
    plan: [block("t1", "plan_task", KEY, PLAN, 0, query), blockNodeId("plan-md", "t1")],
    spec: [block("r1", "requirement", KEY, SPEC, 0, query), blockNodeId("spec", "r1")],
    verification: [
      block("c1", "check", KEY, VERIFICATION, 0, query),
      blockNodeId("verification-md", "c1"),
    ],
  } as const;
  const views = {
    plan: answer([context, own.plan[0]], [], { view: "plan" }),
    spec: answer([context, own.spec[0]], [], { view: "spec" }),
    verification: answer([context, own.verification[0]], [], { view: "verification" }),
  };
  for (const filter of ["spec", "verification", "plan"] as const) {
    const fromView = nodesOf(presetView({ ...graph.root, views }, filter, "").root).map(
      (one) => one.id
    );
    expect(fromView, `${filter} drawn from its view read`).toContain(own[filter][1]);
    expect(fromView, `${filter} drawn from its view read`).not.toContain("p1");
    const offTree = nodesOf(presetView(graph.root, filter, "").root).map((one) => one.id);
    expect(offTree, `${filter} read off the tree`).not.toContain("p1");
  }
});

// R19: on an issue with issues under it the Plan view's read answers each plan task of an issue
// under it, as context marked with its issue, with its place in the plan (`plan`) and its size in
// whole minutes. Built from that answer, the tree drafts the task by its block id alone, addressed
// in its own issue, with a chip for the task of its issue it needs and no gap of its own; the Plan
// view draws it as a row in the walk's order after the issue's own task, with its size and its
// issue. The outline's answer, which carries no `plan`, drafts no task of another issue.
test("a plan task's size and issue come from the plan view's answer", () => {
  const place = (size: string | null) => ({
    cycle: false,
    docs: null,
    needs: [],
    position: 0,
    size,
    skills: null,
  });
  const childPlan = { artifact_id: "artifact-child-plan", slug: "plan-md" };
  const child = (id: string, position: number, size: string | null) =>
    node(id, "plan_task", "CORE-2", {
      document: { ...childPlan, position },
      issue: "CORE-2",
      plan: place(size),
      role: "context",
      text: `Task ${id} of the child.`,
    });
  const plan = answer(
    [
      node("CORE-2", "issue", KEY, { role: "context", text: "A child of the issue" }),
      child("x2", 1, "45"),
      child("x1", 0, "90"),
      block("t1", "plan_task", KEY, PLAN, 0, { plan: place("30"), role: "query" }),
    ],
    [tuple("needs", "x2", "x1")],
    { view: "plan" }
  );
  const { drafts, root } = drawn(plan);
  expect(
    ["x1", "x2"].map((id) => {
      const draft = drafts.get(id);
      return [draft?.reference, draft?.chips.map((one) => one.ref), draft?.gaps];
    })
  ).toEqual([
    ["dispatch://CORE-2/artifact/plan-md#b-x1", [], []],
    ["dispatch://CORE-2/artifact/plan-md#b-x2", ["dispatch://CORE-2/artifact/plan-md#b-x1"], []],
  ]);
  const view = presetView({ ...root, views: { plan, trees: { plan: root } } }, "plan", "");
  expect(view.root.children.map((row) => [row.id, row.size, row.issue])).toEqual([
    [blockNodeId("plan-md", "t1"), 30, undefined],
    ["x1", 90, "CORE-2"],
    ["x2", 45, "CORE-2"],
  ]);
  const outline = answer([
    node("CORE-2", "issue", KEY, { role: "context", text: "A child of the issue" }),
    node("x1", "plan_task", "CORE-2", { document: { ...childPlan, position: 0 }, issue: "CORE-2" }),
  ]);
  expect(drawn(outline).drafts.has("x1")).toBe(false);
});
