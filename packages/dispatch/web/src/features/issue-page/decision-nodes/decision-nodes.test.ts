import { expect, test } from "bun:test";

import type { Ask, Decision, DecisionKeep, DecisionLink, DisplayTree } from "../../../api/types";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import {
  type DecisionNode,
  type DecisionReads,
  decisionNodes,
  decisionQuote,
  nodesById,
} from "./decision-nodes";

/**
 * The decisions of a tree read's answer. Each
 * case writes the answer as the tree read's contract describes it: every node with
 * its `parent`, in the read's walk order, a typed block with the `document` it is written in, a
 * decision node with the members the decisions read answers for it, and the tuples
 * between them, `shaped` and `decided_by` among them, with the person a `decided_by` names held
 * as context.
 */

const KEY = "CORE-1";

/** A document of the issue, as a typed block's `document` member names it. */
interface Document {
  readonly artifact_id: string;
  readonly slug: string;
}

const SPEC: Document = { artifact_id: "artifact-spec", slug: "spec" };
const DECISIONS: Document = { artifact_id: "artifact-decisions", slug: "decisions-md" };

/** The server's reading of a text: a paragraph for each string, and each other block as given. */
function reading(...blocks: readonly (string | DisplayTree)[]): DisplayTree {
  return {
    content: blocks.map((one) =>
      typeof one === "string" ? { content: [{ text: one, type: "text" }], type: "paragraph" } : one
    ),
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
  document: Document,
  position: number,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return node(id, supertag, KEY, { document: { ...document, position }, ...extra });
}

/** A decision block of the spec, with the members the read answers on it: an agent's decision
 *  that waits for review, as the decisions read answers one, unless the case says
 *  otherwise. */
function decision(id: string, extra: Partial<TreeAnswerNode> = {}): TreeAnswerNode {
  return block(id, "decision", SPEC, 1, {
    asks: [],
    changed_at: "2026-11-06T00:00:00Z",
    changed_version: 2,
    kept: null,
    kind: "agent",
    links: [],
    replaced: false,
    text_sha256: `sum-${id}`,
    unsure: false,
    waiting_review: true,
    ...extra,
  });
}

/** The issue's own node, the root every other node hangs from. */
const ISSUE = node(KEY, "issue", null, { text: "The issue page draws one ordered body" });

/** The person a `decided_by` tuple names, which the read answers as context. */
const ALICE = node("alice", "person", null, { mark: "context", text: "alice" });

/** One answer of the tree read: the issue's node, then `nodes` in the walk's order. */
function answer(nodes: readonly TreeAnswerNode[], tuples: readonly TreeTuple[] = []): TreeAnswer {
  return {
    converted: false,
    cursor: null,
    nodes: [ISSUE, ...nodes],
    tree: null,
    tuples: [...tuples],
    view_of: null,
  };
}

function tuple(kind: string, from: string, to: string): TreeTuple {
  return { from, kind, to };
}

/** The address of block `id` of the document `slug` of this issue. */
function blockRef(slug: string, id: string): string {
  return `dispatch://${KEY}/artifact/${slug}#b-${id}`;
}

/** A decision's link to the node `ref` addresses, as the decisions read answers one. */
function linkTo(ref: string, stale = false): DecisionLink {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_version: 1,
    from_block: "d1",
    stale,
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

/** One question asked of the issue, as the asks read answers it. */
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

/** What every case reads beside the answer: the issue's key, and no asks. */
const READS: DecisionReads = { issue: { key: KEY } };

// A decision node answers the members the decisions read answers, by the same
// names and values, so every reader of a `Decision` - the Decisions filter's parts, the review
// list, the Spec filter's column, Keep and Change - reads it with no change; with the document it
// is a block of, which Keep and a Change's anchor are written against, and the node itself. No
// other node is a decision: a statement, a comment under the decision, and an open question that
// carries `shaped` ties answer none.
test("a decision node answers the shape decisionsParts reads", () => {
  const words = "Keep the model pure.\n\nThe tree reads one answer.";
  const d1 = decision("d1", {
    links: [linkTo(blockRef("spec", "r1"))],
    text: words,
    text_tree: reading("Keep the model pure.", "The tree reads one answer."),
  });
  const tree = answer(
    [
      block("r1", "requirement", SPEC, 0),
      d1,
      node("c1", "comment", "d1", { author: [{ id: "alice", kind: "user" }] }),
      block("q1", "ask", SPEC, 2),
      ALICE,
    ],
    [tuple("shaped", "d1", "r1"), tuple("decided_by", "d1", "alice"), tuple("shaped", "q1", "r1")]
  );
  const reads: DecisionReads = {
    asks: [question("ask-1", { artifact_id: SPEC.artifact_id, block_id: "q1" })],
    issue: { key: KEY },
  };

  // Each one is one of the decisions read's rows, which is what each reader takes today.
  expect(decisionNodes(tree, reads) satisfies readonly Decision[]).toEqual([
    {
      asks: [],
      block_id: "d1",
      changed_at: "2026-11-06T00:00:00Z",
      changed_version: 2,
      document: SPEC.artifact_id,
      heading: "Keep the model pure.",
      kept: null,
      kind: "agent",
      links: [linkTo(blockRef("spec", "r1"))],
      node: d1,
      ref: blockRef("spec", "d1"),
      replaced: false,
      text: words,
      text_sha256: "sum-d1",
      text_tree: reading("Keep the model pure.", "The tree reads one answer."),
      unsure: false,
      waiting_review: true,
    },
  ]);
});

// A decision's `shaped` ties are its links, which the read answers on the node
// with each link's `stale`, and which the review list and each statement's decisions are read
// from alone.
test("a decision's links are its shaped ties", () => {
  const ties = [linkTo(blockRef("spec", "r1")), linkTo(blockRef("spec", "r2"), true)];
  const tree = answer(
    [
      block("r1", "requirement", SPEC, 0),
      block("r2", "requirement", SPEC, 1),
      decision("d1", { links: ties }),
    ],
    [tuple("shaped", "d1", "r1"), tuple("shaped", "d1", "r2")]
  );

  const [d1] = decisionNodes(tree, READS);
  expect(d1?.links).toEqual(ties);
  expect(d1?.links?.map((link) => [link.to.ref, link.stale])).toEqual([
    [blockRef("spec", "r1"), false],
    [blockRef("spec", "r2"), true],
  ]);
});

// A decision's review state is the one the read answers on it, never one worked out
// here: waiting for review, kept with who kept it and when, replaced; and a decision kept at
// words it no longer reads waits again with its old Keep still answered.
test("its review state is the read's", () => {
  const keep = (id: string, keptAt: string): DecisionKeep => ({
    artifact_id: SPEC.artifact_id,
    block_id: id,
    changed_version: 2,
    kept_at: keptAt,
    kept_by: "alice",
  });
  const tree = answer([
    decision("d-waiting"),
    decision("d-kept", { kept: keep("d-kept", "2026-11-07T00:00:00Z"), waiting_review: false }),
    decision("d-replaced", { replaced: true, waiting_review: false }),
    decision("d-rewritten", { kept: keep("d-rewritten", "2026-11-04T00:00:00Z") }),
  ]);

  expect(
    decisionNodes(tree, READS).map(({ block_id, kept, replaced, waiting_review }) => ({
      block_id,
      kept,
      replaced,
      waiting_review,
    }))
  ).toEqual([
    { block_id: "d-waiting", kept: null, replaced: false, waiting_review: true },
    {
      block_id: "d-kept",
      kept: keep("d-kept", "2026-11-07T00:00:00Z"),
      replaced: false,
      waiting_review: false,
    },
    { block_id: "d-replaced", kept: null, replaced: true, waiting_review: false },
    {
      block_id: "d-rewritten",
      kept: keep("d-rewritten", "2026-11-04T00:00:00Z"),
      replaced: false,
      waiting_review: true,
    },
  ]);
});

// A person's decision is the question they answered, where it carries
// `shaped` ties. It is a decision as today's decisions read answers a person's: of the kind
// `person`, naming the ask it is, with its ties as links that are not stale, waiting for nobody's
// review, never kept nor replaced, and made when it was answered. An ask block is that ask by the
// block the asks read names, in the document that holds it (`block_artifact`, by its slug); an ask
// of the conversation is its own. An open question with ties, and an answered one with none, are
// no decision.
test("a person's decision is its answered ask", () => {
  const answered = { at: "2026-11-06T12:00:00Z", selected: ["REST"], text: null, user: "alice" };
  const q1 = block("q1", "ask", SPEC, 1, {
    text: "Which transport do we expose?",
    text_tree: reading("Which transport do we expose?"),
  });
  const ask4 = node("ask-4", "ask", KEY);
  const tree = answer(
    [
      block("r1", "requirement", SPEC, 0),
      q1,
      block("q2", "ask", SPEC, 2),
      block("q3", "ask", SPEC, 3),
      ask4,
      ALICE,
    ],
    [
      tuple("shaped", "q1", "r1"),
      tuple("decided_by", "q1", "alice"),
      tuple("shaped", "q2", "r1"),
      tuple("shaped", "ask-4", "r1"),
    ]
  );
  /** Where an ask block of the spec stands, as the asks read names it. */
  const inSpec = {
    artifact_id: SPEC.artifact_id,
    block_artifact: { id: SPEC.artifact_id, primary: true, slug: SPEC.slug },
  };
  const reads: DecisionReads = {
    asks: [
      question("ask-1", { ...inSpec, answer: answered, block_id: "q1", state: "answered" }),
      question("ask-2", { ...inSpec, block_id: "q2" }),
      question("ask-3", { ...inSpec, answer: answered, block_id: "q3", state: "answered" }),
      question("ask-4", { answer: answered, state: "answered" }),
    ],
    issue: { key: KEY },
  };
  /** The link an ask's `shaped` tie to `r1` is, as the decisions read answers a link. */
  const shapedR1 = (from: TreeAnswerNode): DecisionLink => ({
    created_at: from.made_at,
    created_version: 0,
    from_block: from.id,
    stale: false,
    to: {
      changed_at: null,
      changed_by: null,
      id: "r1",
      kind: "block",
      made_at: null,
      made_by: null,
      parent: null,
      parent_version: null,
      ref: blockRef("spec", "r1"),
    },
  });
  const person = {
    changed_at: answered.at,
    changed_version: 0,
    kept: null,
    kind: "person",
    replaced: false,
    text_sha256: "",
    unsure: false,
    waiting_review: false,
  };

  expect(decisionNodes(tree, reads)).toEqual([
    {
      ...person,
      asks: ["ask-1"],
      block_id: "q1",
      document: SPEC.artifact_id,
      heading: "Which transport do we expose?",
      links: [shapedR1(q1)],
      node: q1,
      ref: `dispatch://${KEY}/ask/ask-1`,
      text: "Which transport do we expose?",
      text_tree: reading("Which transport do we expose?"),
    },
    {
      ...person,
      asks: ["ask-4"],
      block_id: "ask-4",
      document: undefined,
      heading: "Words of ask-4.",
      links: [shapedR1(ask4)],
      node: ask4,
      ref: `dispatch://${KEY}/ask/ask-4`,
      text: "Words of ask-4.",
      text_tree: reading("Words of ask-4."),
    },
  ]);
});

// A decision is addressed in the document its `document` member names, and
// Keep and a Change's anchor go to that document: the spec's, the decisions document's, and a
// document of the issue the read marks the node with.
test("a decision of another document names that document", () => {
  const tree = answer([
    decision("d1"),
    decision("d2", { document: { ...DECISIONS, position: 0 } }),
    decision("d3", {
      document: { artifact_id: "artifact-core-2-spec", position: 0, slug: "spec" },
      issue: "CORE-2",
    }),
  ]);

  expect(
    decisionNodes(tree, READS).map((one: DecisionNode) => [one.ref, one.block_id, one.document])
  ).toEqual([
    [blockRef("spec", "d1"), "d1", SPEC.artifact_id],
    [blockRef("decisions-md", "d2"), "d2", DECISIONS.artifact_id],
    ["dispatch://CORE-2/artifact/spec#b-d3", "d3", "artifact-core-2-spec"],
  ]);
});

// Two documents of an issue that has not converted can each hold a block of one id, and a tie
// names its target by that id alone. A person's decision's `shaped` tie then names the first of
// the two the answer holds, the block the outline's chip names (`nodesById`), so the decision and
// the outline's chip name the same block.
test("a shaped tie to an id two blocks share names the first, as the outline does", () => {
  const tree = answer(
    [
      block("r1", "requirement", SPEC, 0),
      block("r1", "callout", DECISIONS, 0),
      block("q1", "ask", SPEC, 1),
    ],
    [tuple("shaped", "q1", "r1")]
  );
  const reads: DecisionReads = {
    asks: [
      question("ask-1", {
        answer: { at: "2026-11-06T12:00:00Z", selected: ["REST"], text: null, user: "alice" },
        artifact_id: SPEC.artifact_id,
        block_artifact: { id: SPEC.artifact_id, primary: true, slug: SPEC.slug },
        block_id: "q1",
        state: "answered",
      }),
    ],
    issue: { key: KEY },
  };

  expect(nodesById(tree).get("r1")?.document?.slug).toBe(SPEC.slug);
  expect(decisionNodes(tree, reads).map((one) => one.links?.map((link) => link.to.ref))).toEqual([
    [blockRef("spec", "r1")],
  ]);
});

// A decision is unsure where its agent wrote its uncertainty as high -
// the attribute the conversion writes for an `Unsure:` paragraph - or where the read says so; a
// medium uncertainty, and none, leave it sure.
test("an unsure decision is one whose uncertainty is high", () => {
  const tree = answer([
    decision("d-high", {
      attributes: { uncertainty: "high", uncertainty_reason: "A holiday moves it." },
    }),
    decision("d-read", { unsure: true }),
    decision("d-medium", { attributes: { uncertainty: "medium" } }),
    decision("d-sure"),
  ]);

  expect(decisionNodes(tree, READS).map(({ block_id, unsure }) => [block_id, unsure])).toEqual([
    ["d-high", true],
    ["d-read", true],
    ["d-medium", false],
    ["d-sure", false],
  ]);
});

// A Change quotes the words of the decision's first paragraph, cut as the server reads
// a quote: a hard break as one space, a list's items one space apart, a heading passed over, and
// the stored first paragraph where the read sent no reading. A converted decision's first
// paragraph is its old heading, which its heading is read from too.
test("a decision's quote is the words of its first paragraph", () => {
  const run = (text: string): DisplayTree => ({ text, type: "text" });
  const item = (text: string): DisplayTree => ({
    content: [{ content: [run(text)], type: "paragraph" }],
    type: "list_item",
  });
  const tree = answer([
    decision("d-break", {
      text_tree: reading(
        { content: [run("Keep it"), { type: "hardbreak" }, run("pure.")], type: "paragraph" },
        "Why it holds."
      ),
    }),
    decision("d-list", {
      text_tree: reading(
        { content: [item("Hold the deploy."), item("Then ship.")], type: "bullet_list" },
        "Why it holds."
      ),
    }),
    decision("d-heading", {
      text_tree: reading(
        { attrs: { level: 2 }, content: [run("Purity")], type: "heading" },
        "Keep the model pure."
      ),
    }),
    decision("d-stored", { text: "Keep the model pure.\n\nWhy it holds.", text_tree: undefined }),
  ]);
  const nodes = decisionNodes(tree, READS);
  const quotes = [
    "Keep it pure.",
    "Hold the deploy. Then ship.",
    "Keep the model pure.",
    "Keep the model pure.",
  ];

  expect(nodes.map(decisionQuote)).toEqual(quotes);
  expect(nodes.map((one) => one.heading)).toEqual(quotes);
});
