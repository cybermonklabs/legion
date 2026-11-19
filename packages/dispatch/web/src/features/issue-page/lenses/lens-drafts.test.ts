import { expect, test } from "bun:test";
import type { LensInvoked } from "@legion/contracts";

import type { Artifact, DisplayTree } from "../../../api/types";
import { buildDispatchReference } from "../../refs/routes";
import { artifact, READER_ACTOR } from "../decisions-filter/decisions-fixtures";
import { issueAddress, pathToTarget } from "../issue-address";
import {
  buildIssueTree,
  drawnLine,
  firstLineOf,
  type IssueTreeReads,
  lineWords,
  nodeWords,
  type TreeNode,
} from "../issue-tree";
import { chipWords } from "../node-row-chip";
import { issueDetails, statementRef } from "../spec-filter/spec-fixtures";
import { blockNodeId, treeContext } from "../tree-drafts";
import { type TreeWords, treeDraftsFrom } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import { check } from "../verification-filter/verification-fixtures";
import { kindWords, type LensFacts } from "./lens-drafts";
import { lensReadId } from "./lens-reads";

/**
 * The rows of lenses, findings, risks and constraints, drafted from the tree read by
 * `lensDrafts` and `lensChips`. Each case writes the answer as the tree read's own cases write
 * it (`tree-read/tree-nodes.test.ts`), from the contract's prose: every node with its `parent`,
 * in the read's walk order, a typed block with the `document` it is written in, the catalogue's
 * nodes as context, and the tuples between them. The tree is built by `buildIssueTree`, as the
 * page builds it; a case that asks what was drafted reads the drafts `treeDraftsFrom` makes of
 * the same answer.
 */

const KEY = "CORE-1";

const SPEC = artifact("artifact-spec", "spec", true);
const PLAN = artifact("artifact-plan", "plan-md");
const VERIFICATION = artifact("artifact-verification", "verification-md");

/** The server's reading of a text of one paragraph per string. */
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
    rank: `${position}`,
    ...extra,
  });
}

/** A node the read answers with no text of its own: a lens, a risk, a constraint, a kind. */
function bare(
  id: string,
  supertag: string,
  parent: string | null,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return node(id, supertag, parent, {
    author: [READER_ACTOR],
    text: "",
    text_tree: undefined,
    ...extra,
  });
}

/** A lens on `parent`, invoked at `madeAt`. */
function lens(
  id: string,
  parent: string,
  madeAt = "2026-11-05T09:00:00Z",
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return bare(id, "lens", parent, { made_at: madeAt, ...extra });
}

/** A finding of the lens `parent`, its words stored as markdown and read by the server. */
function finding(
  id: string,
  parent: string,
  words: string,
  supertag = "block",
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return node(id, supertag, parent, {
    author: [READER_ACTOR],
    text: `**${words}**`,
    text_tree: reading(words),
    ...extra,
  });
}

/** A kind of the catalogue, which the read brings in as context for a `has_kind` tuple. */
function lensKind(id: string, attributes: Record<string, unknown>, text = ""): TreeAnswerNode {
  return bare(id, "lens_kind", null, { attributes, author: [], mark: "context", text });
}

/** The words of the pre-mortem kind, longer than a chip holds. */
const PRE_MORTEM_WORDS = "pre-mortem: imagine the work failed, then say why it did";
const PRE_MORTEM = lensKind("pre-mortem", { words: PRE_MORTEM_WORDS });
const TRADE_OFF = lensKind("trade-off", {}, "trade-off analysis");

/** The issue's own node, the root every other node hangs from. */
const ISSUE = node(KEY, "issue", null, { text: "The issue page draws its lenses" });

/** One answer of the tree read: the issue's node, then `nodes` in the walk's order, on an
 *  issue that has converted unless the case says otherwise. */
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

/** What `buildIssueTree` hands `treeDraftsFrom`, as the tree read's own cases build it. */
function naming(reads: IssueTreeReads): TreeWords {
  const chipLine = (tree: DisplayTree): string =>
    drawnLine(firstLineOf(lineWords({ text: "", tree })));
  return { ...treeContext(reads, chipLine), chipLine, lineWords };
}

/** The tree `buildIssueTree` builds from `tree`, and the drafts `treeDraftsFrom` makes of it, on
 *  the spec fixtures' issue (`spec-filter/spec-fixtures.ts`) holding the three documents the
 *  cases write their blocks in, unless a case gives reads of its own. */
function drawn(
  tree: TreeAnswer,
  reads: IssueTreeReads = { issue: { ...issueDetails(), artifacts: [SPEC, PLAN, VERIFICATION] } }
) {
  return {
    drafts: treeDraftsFrom(tree, reads, naming(reads)),
    root: buildIssueTree({ ...reads, tree }),
  };
}

/** The node `id` under `root`, wherever it sits. */
function nodeOf(root: TreeNode, id: string): TreeNode | undefined {
  if (root.id === id) return root;
  for (const child of root.children) {
    const found = nodeOf(child, id);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** The node `id` under `root`, which the case has drawn. */
function rowOf(root: TreeNode, id: string): TreeNode {
  const found = nodeOf(root, id);
  if (found === undefined) throw new Error(`no row ${id}`);
  return found;
}

/** The id of the row each placement of `id` sits under. */
function parentsOf(root: TreeNode, id: string): string[] {
  const found: string[] = [];
  const walk = (one: TreeNode): void => {
    for (const child of one.children) {
      if (child.id === id) found.push(one.id);
      walk(child);
    }
  };
  walk(root);
  return found;
}

/** Every node of the tree, the root first, each before its children. */
function idsOf(root: TreeNode): string[] {
  return [root.id, ...root.children.flatMap(idsOf)];
}

function childIds(row: TreeNode): string[] {
  return row.children.map((child) => child.id);
}

/** The lens facts a row carries, which the case expects it to carry. */
function factsOf(row: TreeNode): LensFacts {
  if (row.lens === undefined) throw new Error(`no lens facts on ${row.id}`);
  return row.lens;
}

/** The lens facts of a node that carries no chip and no fact of its own. */
const NO_FACTS = { constrainedBy: [], preventedBy: [], triggered: [], triggeredBy: [] };

/** Three lenses' ids, which sort against the order they were invoked in. */
const FIRST = "c3000000-0000-4000-8000-000000000003";
const SECOND = "b2000000-0000-4000-8000-000000000002";
const THIRD = "a1000000-0000-4000-8000-000000000001";

// A node's lenses stand one level under it, after its ranked children, in the read's order,
// which is the order they were invoked in, though their ids sort the other way; a lens's
// findings in the order they were written; a lens on the issue at the top level.
test("a lens hangs under its parent, after its other children, in invocation order", () => {
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0),
        block("t1", "plan_task", "r1", PLAN, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z"),
        finding(`lens/${FIRST}#f-b`, FIRST, "The bill doubles."),
        finding(`lens/${FIRST}#f-a`, FIRST, "Failover takes minutes."),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z"),
        lens(THIRD, "r1", "2026-11-05T11:00:00Z"),
        lens("on-issue", KEY),
        TRADE_OFF,
      ],
      [FIRST, SECOND, THIRD, "on-issue"].map((id) => tuple("has_kind", id, TRADE_OFF.id))
    )
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect(childIds(root)).toEqual([blockNodeId("spec", "r1"), "on-issue"]);
  expect(childIds(statement)).toEqual([
    blockNodeId("verification-md", "c1"),
    blockNodeId("plan-md", "t1"),
    FIRST,
    SECOND,
    THIRD,
  ]);
  expect(childIds(rowOf(root, FIRST))).toEqual([`lens/${FIRST}#f-b`, `lens/${FIRST}#f-a`]);
  expect(
    statement.children.slice(2).map((row) => [row.type, row.readId, row.at, row.author])
  ).toEqual([
    ["lens", FIRST, "2026-11-05T09:00:00Z", READER_ACTOR],
    ["lens", SECOND, "2026-11-05T10:00:00Z", READER_ACTOR],
    ["lens", THIRD, "2026-11-05T11:00:00Z", READER_ACTOR],
  ]);
});

// On an issue the conversion has not reached, a lens stands under the row its parent's id
// gives that parent: a typed block's `document` makes it `<slug>#b-<id>` (`blockNodeId`).
test("a lens is found by its parent's id on an unconverted issue", () => {
  const tree = answer(
    [
      block("r1", "requirement", KEY, SPEC, 0),
      lens(FIRST, "r1", "2026-11-05T09:00:00Z", { attributes: { verdict: "Keep one region." } }),
      TRADE_OFF,
    ],
    [tuple("has_kind", FIRST, TRADE_OFF.id)],
    { converted: false, documents: { spec: SPEC.id }, tree: null }
  );
  const { drafts, root } = drawn(tree);

  expect(parentsOf(root, FIRST)).toEqual([blockNodeId("spec", "r1")]);
  expect(drafts.get(FIRST)?.parents).toEqual([blockNodeId("spec", "r1")]);
  expect([rowOf(root, FIRST).text, rowOf(root, FIRST).reference]).toEqual([
    "Keep one region.",
    `dispatch://lens/${FIRST}`,
  ]);
});

// A finding is a block whose read parent is a lens, by either spelling of its kind; its
// words are its reading's, and its address is its lens's with its block id. A block under
// anything else is no finding. A lens's read id may carry its kind, which its address does not
// repeat.
test("a finding is a block whose parent is a lens", () => {
  const lensId = `lens/${FIRST}`;
  const { drafts, root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(lensId, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: "_Keep_ one region, as dispatch://CORE-2 does." },
        }),
        finding(`${lensId}#first`, lensId, "A second region doubles the cost."),
        finding("second", lensId, "Failover takes minutes.", "lens_block", { state: "proposed" }),
        finding("stray", KEY, "A block of no lens."),
        TRADE_OFF,
      ],
      [tuple("has_kind", lensId, TRADE_OFF.id)]
    )
  );
  const row = rowOf(root, lensId);

  expect([row.type, row.text, row.reference]).toEqual([
    "lens",
    "Keep one region, as CORE-2 does.",
    `dispatch://lens/${FIRST}`,
  ]);
  expect(
    row.children.map((one) => [
      one.id,
      one.type,
      one.reference,
      nodeWords(one),
      lineWords(one),
      one.lens,
    ])
  ).toEqual([
    [
      `${lensId}#first`,
      "finding",
      `dispatch://lens/${FIRST}#b-first`,
      "A second region doubles the cost.",
      "A second region doubles the cost.",
      NO_FACTS,
    ],
    [
      "second",
      "finding",
      `dispatch://lens/${FIRST}#b-second`,
      "Failover takes minutes.",
      "Failover takes minutes.",
      NO_FACTS,
    ],
  ]);
  expect(drafts.has("stray")).toBe(false);
  expect(idsOf(root)).not.toContain("stray");
});

// A lens's row carries a chip for each lens its `triggered_by` names and for each lens whose
// `triggered_by` names it, only for a lens the read holds, named by its kind's words as its tag
// is, linked where a step drafted it, and with that lens's state and visibility. Every lens
// shares one map of every lens of the answer, which a focused lens's chain reads.
test("a trigger chip is drawn only for a lens the read holds", () => {
  const unseen = "f9000000-0000-4000-8000-000000000009";
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("p1", "paragraph", KEY, SPEC, 1),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z"),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z", { state: "proposed" }),
        lens(THIRD, "p1", "2026-11-05T11:00:00Z", { visibility: "author" }),
        PRE_MORTEM,
        TRADE_OFF,
      ],
      [
        tuple("has_kind", FIRST, TRADE_OFF.id),
        tuple("has_kind", SECOND, PRE_MORTEM.id),
        tuple("has_kind", THIRD, PRE_MORTEM.id),
        tuple("triggered_by", SECOND, FIRST),
        tuple("triggered_by", SECOND, unseen),
        tuple("triggered_by", THIRD, FIRST),
      ]
    )
  );
  const first = factsOf(rowOf(root, FIRST));
  const second = factsOf(rowOf(root, SECOND));

  expect(nodeOf(root, THIRD)).toBeUndefined();
  expect([first.kind, second.kind]).toEqual(["trade-off analysis", PRE_MORTEM_WORDS]);
  expect(second.triggeredBy).toEqual([
    {
      address: `dispatch://lens/${FIRST}`,
      id: FIRST,
      state: "live",
      visibility: "project",
      words: "Trade-off analysis",
    },
  ]);
  expect(second.triggered).toEqual([]);
  expect(first.triggeredBy).toEqual([]);
  expect(first.triggered).toStrictEqual([
    {
      address: `dispatch://lens/${SECOND}`,
      id: SECOND,
      state: "proposed",
      visibility: "project",
      words: chipWords(kindWords(PRE_MORTEM_WORDS)),
    },
    {
      address: `dispatch://lens/${THIRD}`,
      state: "live",
      visibility: "author",
      words: chipWords(kindWords(PRE_MORTEM_WORDS)),
    },
  ]);
  expect(first.lenses).toBe(second.lenses);
  expect([...(first.lenses ?? new Map()).entries()]).toEqual([
    [
      `dispatch://lens/${FIRST}`,
      { id: FIRST, kindWords: "trade-off analysis", onLine: "Words of r1." },
    ],
    [
      `dispatch://lens/${SECOND}`,
      {
        id: SECOND,
        kindWords: PRE_MORTEM_WORDS,
        onLine: "Words of r1.",
      },
    ],
    [`dispatch://lens/${THIRD}`, { kindWords: PRE_MORTEM_WORDS }],
  ]);
});

// A risk's row draws its heading, its early warning and its prevention, each read to
// plain words, and a chip for each check its `prevented_by` names, named by that check's line
// in the tree (`chipWords(lineWords(node))`) and linked to its row; a risk no check prevents has
// none.
test("a check's chip takes its words from its node", () => {
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0, {
          text_tree: reading("C1. The index builds concurrently. Then it is checked."),
        }),
        lens(FIRST, "r1"),
        bare("risk-b", "risk", FIRST, {
          attributes: {
            early_warning: "Writes _queue_.",
            heading: "The **migration** locks the table.",
            prevention: "Build the index `CONCURRENTLY`.",
          },
        }),
        bare("risk-a", "risk", FIRST, {
          attributes: { early_warning: "None.", heading: "Nobody reads it.", prevention: "Ask." },
        }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id), tuple("prevented_by", "risk-b", "c1")]
    ),
    {
      issue: { ...issueDetails(), artifacts: [SPEC, PLAN, VERIFICATION] },
      results: {
        checks: [check({ block_id: "c1", ref: `dispatch://${KEY}/verification#b-c1` })],
        issue: KEY,
        verification_artifact_id: VERIFICATION.id,
      },
    }
  );
  const checkRow = rowOf(root, blockNodeId("verification-md", "c1"));
  const risks = rowOf(root, FIRST).children;

  expect(risks.map((one) => [one.id, one.type, one.text, one.reference])).toEqual([
    ["risk-b", "risk", "The migration locks the table.", "dispatch://risk/risk-b"],
    ["risk-a", "risk", "Nobody reads it.", "dispatch://risk/risk-a"],
  ]);
  expect(factsOf(rowOf(root, "risk-b"))).toEqual({
    ...NO_FACTS,
    earlyWarning: "Writes queue.",
    prevention: "Build the index CONCURRENTLY.",
    preventedBy: [
      {
        address: checkRow.reference ?? "",
        id: "c1",
        state: "live",
        visibility: "project",
        words: chipWords(lineWords(checkRow)),
      },
    ],
  });
  expect(checkRow.reference).toBe(`dispatch://${KEY}/verification#b-c1`);
  expect(factsOf(rowOf(root, "risk-a")).preventedBy).toEqual([]);
});

// Each constraint of the issue is a row at the top level, in the read's order, its
// words a standard's or a measurement's, its reference or its place on its own line, each read
// to plain words; a statement with a `constrained_by` tuple carries a chip for each, linked to
// its row.
test("a constraint is a top-level row and its statement carries its chip", () => {
  const standard = "constraint/d4000000-0000-4000-8000-000000000004";
  const measured = "e5000000-0000-4000-8000-000000000005";
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        bare(standard, "constraint", KEY, {
          attributes: {
            reference: "section 9.2.2, as dispatch://CORE-2 reads it",
            standard: "RFC **9110**",
          },
          made_at: "2026-11-05T09:00:00Z",
        }),
        bare(measured, "constraint", KEY, {
          attributes: {
            measurement: "p99 write latency",
            taken_where: "staging, 2026-11-15",
            value: "41 ms",
          },
          made_at: "2026-11-05T10:00:00Z",
        }),
      ],
      [tuple("constrained_by", "r1", standard), tuple("constrained_by", "r1", measured)]
    )
  );

  expect(childIds(root)).toEqual([blockNodeId("spec", "r1"), standard, measured]);
  expect(
    root.children.slice(1).map((row) => [row.type, row.text, row.reference, row.author, row.at])
  ).toEqual([
    [
      "constraint",
      "RFC 9110",
      "dispatch://constraint/d4000000-0000-4000-8000-000000000004",
      READER_ACTOR,
      "2026-11-05T09:00:00Z",
    ],
    [
      "constraint",
      "p99 write latency: 41 ms",
      `dispatch://constraint/${measured}`,
      READER_ACTOR,
      "2026-11-05T10:00:00Z",
    ],
  ]);
  expect([factsOf(rowOf(root, standard)), factsOf(rowOf(root, measured))]).toEqual([
    { ...NO_FACTS, reference: "section 9.2.2, as CORE-2 reads it" },
    { ...NO_FACTS, takenWhere: "staging, 2026-11-15" },
  ]);
  expect(factsOf(rowOf(root, blockNodeId("spec", "r1")))).toEqual({
    ...NO_FACTS,
    constrainedBy: [
      {
        address: "dispatch://constraint/d4000000-0000-4000-8000-000000000004",
        id: standard,
        state: "live",
        visibility: "project",
        words: "RFC 9110",
      },
      {
        address: `dispatch://constraint/${measured}`,
        id: measured,
        state: "live",
        visibility: "project",
        words: "p99 write latency: 41 ms",
      },
    ],
  });
});

// A lens on an ask's block stands under the ask, which the server stores it under the
// block of; a lens on a node no step drafts - a paragraph of the spec, a document - is not drawn,
// and neither is anything under it.
test("a lens on an ask's block stands under the ask, and one on no drafted parent is not drawn", () => {
  const { drafts, root } = drawn(
    answer(
      [
        block("p1", "paragraph", KEY, SPEC, 0),
        lens(SECOND, "p1"),
        finding("lost-finding", SECOND, "Nobody reads this."),
        bare("lost-risk", "risk", SECOND, { attributes: { heading: "Unseen." } }),
        node("doc-1", "document", KEY),
        lens(THIRD, "doc-1"),
        node("q1", "ask", KEY, { author: [READER_ACTOR] }),
        node("q1-words", "paragraph", "q1"),
        lens(FIRST, "q1-words"),
        finding("kept-finding", FIRST, "The question has two answers."),
        bare("kept-risk", "risk", FIRST, { attributes: { heading: "It stays open." } }),
        PRE_MORTEM,
      ],
      [SECOND, THIRD, FIRST].map((id) => tuple("has_kind", id, PRE_MORTEM.id))
    )
  );

  expect(parentsOf(root, FIRST)).toEqual(["q1"]);
  expect(childIds(rowOf(root, FIRST))).toEqual(["kept-finding", "kept-risk"]);
  for (const lost of [SECOND, "lost-finding", "lost-risk", THIRD]) {
    expect(drafts.has(lost)).toBe(false);
    expect(idsOf(root)).not.toContain(lost);
  }
});

// A lens on a comment's or a message's block stands under that item, however deep the block
// it reads sits in the item's words.
test("a lens on a comment's block stands under the comment", () => {
  const { root } = drawn(
    answer(
      [
        node("c1", "comment", KEY, { author: [READER_ACTOR] }),
        node("c1-words", "paragraph", "c1"),
        lens(FIRST, "c1-words"),
        node("m1", "message", KEY, { author: [READER_ACTOR] }),
        node("m1-list", "list", "m1"),
        node("m1-item", "paragraph", "m1-list"),
        lens(SECOND, "m1-item"),
        TRADE_OFF,
      ],
      [FIRST, SECOND].map((id) => tuple("has_kind", id, TRADE_OFF.id))
    )
  );

  expect(parentsOf(root, FIRST)).toEqual(["c1"]);
  expect(parentsOf(root, SECOND)).toEqual(["m1"]);
  expect(childIds(root)).toEqual(["c1", "m1"]);
});

// A catalogue constraint the read brings in as context, and a constraint under anything but
// the issue, draws no row; a statement's chip names a catalogue constraint by its words, cut as
// a tag is, and links nothing. A lens kind is no row either.
test("a catalogue constraint is no row and its chip has no link", () => {
  const catalogue = "f6000000-0000-4000-8000-000000000006";
  const standard = "WCAG 2.2 AA, success criterion 1.4.3 contrast minimum";
  const elsewhere = "a7000000-0000-4000-8000-000000000007";
  const { drafts, root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        bare(elsewhere, "constraint", "r1", {
          attributes: { reference: "here", standard: "Mine" },
        }),
        bare(catalogue, "constraint", null, {
          attributes: {
            reference: "w3.org/TR/WCAG22",
            standard,
          },
          author: [],
          mark: "context",
        }),
        PRE_MORTEM,
      ],
      [tuple("constrained_by", "r1", catalogue)]
    )
  );
  const [chip] = factsOf(rowOf(root, blockNodeId("spec", "r1"))).constrainedBy;

  expect(idsOf(root)).toEqual([KEY, blockNodeId("spec", "r1")]);
  expect([drafts.has(catalogue), drafts.has(elsewhere), drafts.has(PRE_MORTEM.id)]).toEqual([
    false,
    false,
    false,
  ]);
  expect(chip).toStrictEqual({
    address: `dispatch://constraint/${catalogue}`,
    state: "live",
    visibility: "project",
    words: chipWords(standard),
  });
});

// `?node=` with the id the 201 of an invocation gives (`lensReadId`) opens the path to that
// lens's row, and the row's reference, which Copy reference copies, is the lens's address. A
// statement's and a check's read ids, which their tree ids (`<slug>#b-<id>`) are not, open the
// paths to their rows alike: the links of a risk's Prevented by chip and of a statement's address.
test("a lens row is reached by its ?node= id", () => {
  const invoked: LensInvoked = {
    address: `dispatch://lens/${FIRST}`,
    author: { id: "alice", kind: "person" },
    created_at: "2026-11-05T09:00:00Z",
    findings: [],
    id: FIRST,
    kind: TRADE_OFF.id,
    on: statementRef("r1"),
    state: "live",
    triggered_by: null,
    verdict: null,
    visibility: "project",
  };
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0),
        lens(FIRST, "r1"),
        lens(SECOND, "r1"),
        TRADE_OFF,
      ],
      [tuple("has_kind", FIRST, TRADE_OFF.id), tuple("has_kind", SECOND, TRADE_OFF.id)]
    )
  );
  const address = issueAddress(
    { key: KEY, kind: "issue" },
    `?q=region&node=${encodeURIComponent(lensReadId(invoked))}`,
    "#b-r1",
    "spec"
  );
  const path = pathToTarget(root, address.target);

  expect(address.target).toEqual({ id: FIRST, kind: "node" });
  expect(path?.map((one) => one.id)).toEqual([KEY, blockNodeId("spec", "r1"), FIRST]);
  expect(path?.at(-1)?.reference).toBe(invoked.address);
  for (const [readId, chain] of [
    ["r1", [KEY, blockNodeId("spec", "r1")]],
    ["c1", [KEY, blockNodeId("spec", "r1"), blockNodeId("verification-md", "c1")]],
  ] as const) {
    const { target } = issueAddress({ key: KEY, kind: "issue" }, `?node=${readId}`, "", "spec");

    expect(target).toEqual({ id: readId, kind: "node" });
    expect(pathToTarget(root, target)?.map((one) => one.id)).toEqual([...chain]);
  }
});

// `assemble` copies each draft's lens facts onto its tree node whole - a lens's, a risk's, a
// constraint's and a statement's chips alike.
test("a draft's lens facts reach the tree whole", () => {
  const constraint = "d4000000-0000-4000-8000-000000000004";
  const { drafts, root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", { attributes: { verdict: "Ship it." } }),
        lens(SECOND, FIRST, "2026-11-05T10:00:00Z"),
        bare("risk-1", "risk", SECOND, {
          attributes: { early_warning: "Writes queue.", heading: "It locks.", prevention: "Ask." },
        }),
        bare(constraint, "constraint", KEY, {
          attributes: { reference: "9.2.2", standard: "RFC 9110" },
        }),
        TRADE_OFF,
        PRE_MORTEM,
      ],
      [
        tuple("has_kind", FIRST, TRADE_OFF.id),
        tuple("has_kind", SECOND, PRE_MORTEM.id),
        tuple("triggered_by", SECOND, FIRST),
        tuple("prevented_by", "risk-1", "c1"),
        tuple("constrained_by", "r1", constraint),
      ]
    )
  );
  const ids = [blockNodeId("spec", "r1"), FIRST, SECOND, "risk-1", constraint];
  // The results read holds no row for the check, so its chip's address is its block's in the
  // document the read says it is written in, as the check's own row has it.
  const checkAddress = buildDispatchReference({
    block: { id: "c1" },
    key: KEY,
    kind: "artifact",
    slug: VERIFICATION.slug,
  });

  for (const id of ids) {
    const facts = drafts.get(id)?.lens;
    expect(facts).toBeDefined();
    expect(rowOf(root, id).lens).toEqual(facts);
  }
  expect(rowOf(root, blockNodeId("verification-md", "c1")).lens).toBeUndefined();
  expect(rowOf(root, blockNodeId("verification-md", "c1")).reference).toBe(checkAddress);
  expect(rowOf(root, "risk-1").lens).toEqual({
    ...NO_FACTS,
    earlyWarning: "Writes queue.",
    preventedBy: [
      {
        address: checkAddress,
        id: "c1",
        state: "live",
        visibility: "project",
        words: "Words of c1.",
      },
    ],
    prevention: "Ask.",
  });
  expect(rowOf(root, SECOND).lens).toEqual({
    ...NO_FACTS,
    kind: PRE_MORTEM_WORDS,
    lenses: rowOf(root, FIRST).lens?.lenses,
    triggeredBy: [
      {
        address: `dispatch://lens/${FIRST}`,
        id: FIRST,
        state: "live",
        visibility: "project",
        words: "Trade-off analysis",
      },
    ],
  });
});
