import { expect, test } from "bun:test";
import type { LensInvoked } from "@legion/contracts";

import type { Artifact, DisplayTree } from "../../../api/types";
import { buildDispatchReference } from "../../refs/routes";
import { artifact, READER_ACTOR } from "../decisions-filter/decisions-fixtures";
import { issueAddress, pathToTarget, presetPath } from "../issue-address";
import {
  buildIssueTree,
  drawnLine,
  firstLineOf,
  type IssueTreeReads,
  lineWords,
  nodeWords,
  type TreeNode,
} from "../issue-tree";
import { NODE_TYPE_NAMES } from "../page-words";
import { issueDetails, statementRef } from "../spec-filter/spec-fixtures";
import { blockNodeId, treeContext } from "../tree-drafts";
import {
  commentNode,
  type TreeNodeExtra,
  treeAnswer,
  treeNode,
  treeTuple as tuple,
} from "../tree-read/tree-fixtures";
import { type TreeWords, treeDraftsFrom } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";
import { check } from "../verification-filter/verification-fixtures";
import { type FactParts, factText, kindWords, type LensFacts, lensChipWords } from "./lens-drafts";
import { lensReadId } from "./lens-reads";

/**
 * The rows of lenses, findings, risks and constraints, drafted from the tree read by
 * `lensDrafts` and `lensChips`. Each case writes the answer as the route sends it
 * (`tree-read/tree-fixtures.ts`), as the tree read's own cases write it
 * (`tree-read/tree-nodes.test.ts`): every node with its `parent`, in the read's walk order, a
 * typed block with the `document` it is written in, the catalogue's nodes as context, and the
 * tuples between them. The tree is built by `buildIssueTree`, as the page builds it; a case that
 * asks what was drafted reads the drafts `treeDraftsFrom` makes of the same answer.
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
    rank: `${position}`,
    ...extra,
  });
}

/** A node the read answers with no text of its own: a lens, a risk, a constraint, a kind. */
function bare(
  id: string,
  supertag: string,
  parent: string | null,
  extra: TreeNodeExtra = {}
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
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  return bare(id, "lens", parent, { made_at: madeAt, ...extra });
}

/** A finding of the lens `parent`, its words stored as markdown and read by the server. */
function finding(
  id: string,
  parent: string,
  words: string,
  supertag = "block",
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  return node(id, supertag, parent, {
    author: [READER_ACTOR],
    text: `**${words}**`,
    text_tree: reading(words),
    ...extra,
  });
}

/** A kind of the catalogue, which the read brings in as context for a `has_kind` tuple. */
function lensKind(id: string, attributes: Record<string, string>, text = ""): TreeAnswerNode {
  return bare(id, "lens_kind", null, { attributes, author: [], role: "context", text });
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
  return treeAnswer([ISSUE, ...nodes], tuples, {
    converted: true,
    tree: "artifact-tree",
    ...extra,
  });
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

/** A fact line's words read through `factText`, as its row draws them; `""` for a line the row
 *  does not carry. */
function lineText(line: string | FactParts | undefined): string {
  return line === undefined ? "" : factText(line);
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
    {
      converted: false,
      documents: { plan: null, spec: SPEC.id, verification: null },
      tree: null,
    }
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

/** A store address or its uuid, which no row's words draw. */
const STORE_ADDRESS = /dispatch:\/\/|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/u;

// A stored string that cites a lens, a risk or a constraint by its own address draws no address
// and no uuid: the address reads as words, as a bare address of an issue's block does. A verdict,
// a heading and an early warning are read through the stored string's plain words; a finding's
// line through its reading.
test("a lens's, a risk's and a finding's words draw no address of a lens, a risk or a constraint", () => {
  const lensId = `lens/${FIRST}`;
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = {
    constraint: "dispatch://constraint/e5000000-0000-4000-8000-000000000005",
    lens: `dispatch://lens/${SECOND}`,
    risk: `dispatch://risk/${risk}`,
  };
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(lensId, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: `Keep one region, as ${cited.risk} warns.` },
        }),
        bare(risk, "risk", lensId, {
          attributes: {
            early_warning: `The bill passes ${cited.constraint} first.`,
            heading: `Failover takes minutes, as ${cited.lens} found.`,
            prevention: "Test the failover.",
          },
        }),
        finding(
          `${lensId}#first`,
          lensId,
          `A second region doubles the cost, as ${cited.risk} says.`
        ),
        PRE_MORTEM,
      ],
      [tuple("has_kind", lensId, PRE_MORTEM.id)]
    )
  );
  const drawnWords = {
    earlyWarning: lineText(factsOf(rowOf(root, risk)).earlyWarning),
    finding: lineWords(rowOf(root, `${lensId}#first`)),
    heading: rowOf(root, risk).text,
    verdict: rowOf(root, lensId).text,
  };
  const address = /dispatch:\/\/|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/u;

  expect(
    Object.entries(drawnWords).filter(([, words]) => address.test(words)),
    "a row draws a store address or its uuid"
  ).toEqual([]);
  expect(drawnWords.verdict.startsWith("Keep one region, as ")).toBe(true);
  expect(drawnWords.heading.startsWith("Failover takes minutes, as ")).toBe(true);
  expect(drawnWords.earlyWarning.startsWith("The bill passes ")).toBe(true);
  expect(drawnWords.finding.startsWith("A second region doubles the cost, as ")).toBe(true);
});

/** Each run of `tree` that carries a link mark, as its words and the link's attributes: the
 *  address it links to (`href`), and a named store address's own address with the path of its
 *  node's row as its `title`. */
function linksIn(tree: DisplayTree | undefined): [string, unknown][] {
  if (tree === undefined) return [];
  const marks = Array.isArray(tree.marks) ? (tree.marks as DisplayTree[]) : [];
  const link = marks.find((mark) => mark.type === "link");
  const own: [string, unknown][] =
    typeof tree.text === "string" && link !== undefined ? [[tree.text, link.attrs]] : [];
  const children = Array.isArray(tree.content) ? (tree.content as DisplayTree[]) : [];
  return [...own, ...children.flatMap(linksIn)];
}

/** The address of the row of the node `readId` names on the issue's page, which a link in a
 *  row's words holds as its `title` and its row makes its address (`?node=`). */
function rowPath(readId: string): string {
  return `${presetPath(KEY, "conversation")}?node=${encodeURIComponent(readId)}`;
}

// A statement's words, and a comment's, that cite a lens the answer holds by its own address read
// the lens by its kind's words, as its chip names it, in a link beside the run's own marks that
// holds the lens's address and the path of its row. The comment's stored words, which the read
// sent no reading of, are read to one paragraph of their plain words that holds the link, with no
// emphasis, since a row draws no parse made in the page (R20), and its row draws its words from
// it.
test("a statement's and a comment's words read a lens they cite by its kind's words, linked to its row", () => {
  const cited = `dispatch://lens/${FIRST}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, {
          text: `Keep one region, as ${cited} found.`,
          text_tree: reading(`Keep one region, as ${cited} found.`),
        }),
        lens(FIRST, "r1"),
        commentNode({
          author: READER_ACTOR,
          body: `See **${cited}** first.`,
          created_at: "2026-11-05T10:00:00Z",
          id: "c1",
          reply_to: null,
        }),
        TRADE_OFF,
      ],
      [tuple("has_kind", FIRST, TRADE_OFF.id)]
    )
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));
  const comment = rowOf(root, "c1");

  expect([lineWords(statement), lineWords(comment)]).toEqual([
    "Keep one region, as Trade-off analysis found.",
    "See Trade-off analysis first.",
  ]);
  expect(
    [nodeWords(statement), nodeWords(comment)].filter((words) => STORE_ADDRESS.test(words))
  ).toEqual([]);
  const link = { href: cited, title: rowPath(FIRST) };
  expect([linksIn(statement.tree), linksIn(comment.tree)]).toEqual([
    [["Trade-off analysis", link]],
    [["Trade-off analysis", link]],
  ]);
  expect(comment.tree?.content).toEqual([
    {
      content: [
        { text: "See ", type: "text" },
        { marks: [{ attrs: link, type: "link" }], text: "Trade-off analysis", type: "text" },
        { text: " first.", type: "text" },
      ],
      type: "paragraph",
    },
  ]);
  // The path the link holds opens the lens's row, as the page reads an address.
  const { target } = issueAddress(
    { key: KEY, kind: "issue" },
    new URL(rowPath(FIRST), "https://dispatch.example").search,
    "",
    "spec"
  );
  expect(pathToTarget(root, target)?.at(-1)?.id).toBe(FIRST);
});

// A statement's words that cite a risk the answer holds read it by its heading's plain words,
// linked to its row; so does a lens's verdict, whose row draws its words from the reading that
// holds the link.
test("a statement's and a verdict's words read a risk they cite by its heading, linked to its row", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, {
          text: `Two regions, until ${cited}.`,
          text_tree: reading(`Two regions, until ${cited}.`),
        }),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: `Keep _one_ region: ${cited}` },
        }),
        bare(risk, "risk", FIRST, {
          attributes: { heading: "Failover takes **minutes**", prevention: "Test it." },
        }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));
  const verdict = rowOf(root, FIRST);

  expect([lineWords(statement), verdict.text, lineWords(verdict)]).toEqual([
    "Two regions, until Failover takes minutes.",
    "Keep one region: Failover takes minutes",
    "Keep one region: Failover takes minutes",
  ]);
  const link = { href: cited, title: rowPath(risk) };
  expect([linksIn(statement.tree), linksIn(verdict.tree)]).toEqual([
    [["Failover takes minutes", link]],
    [["Failover takes minutes", link]],
  ]);
});

// A statement's words that cite a constraint of the issue read it by its words, linked to its
// row; a risk's early warning reads it by its words too.
test("a statement's and an early warning's words read a constraint they cite by its words", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const cited = `dispatch://constraint/${constraint}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, {
          text: `Answer as ${cited} says.`,
          text_tree: reading(`Answer as ${cited} says.`),
        }),
        lens(FIRST, "r1"),
        bare("risk-1", "risk", FIRST, {
          attributes: { early_warning: `A reply breaks ${cited} first.`, heading: "It breaks." },
        }),
        bare(constraint, "constraint", KEY, {
          attributes: { reference: "section 9.2.2", standard: "RFC **9110**" },
        }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect([lineWords(statement), lineText(factsOf(rowOf(root, "risk-1")).earlyWarning)]).toEqual([
    "Answer as RFC 9110 says.",
    "A reply breaks RFC 9110 first.",
  ]);
  expect(linksIn(statement.tree)).toEqual([
    ["RFC 9110", { href: cited, title: rowPath(constraint) }],
  ]);
});

// A risk's fact lines that name a node the answer holds are their parts: the text, and a chip for
// each name, which holds the node's address, the words the line names it by and its read id, so
// the line links its row. An early warning's bare address of a constraint is named by the
// constraint's words, and a prevention's author's link to a lens keeps the author's words. A line
// that cites a node the answer does not hold names it by its kind's word and is its words, with
// no parts.
test("a risk's fact lines that name a node the answer holds carry a chip of that node", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const toConstraint = `dispatch://constraint/${constraint}`;
  const toLens = `dispatch://lens/${FIRST}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1"),
        bare("risk-1", "risk", FIRST, {
          attributes: {
            early_warning: `A reply breaks ${toConstraint} first.`,
            heading: "It breaks.",
            prevention: `Run [this lens](${toLens}) again.`,
          },
        }),
        bare("risk-2", "risk", FIRST, {
          attributes: {
            early_warning: "Watch dispatch://risk/f9000000-0000-4000-8000-000000000009 closely.",
            heading: "It drifts.",
          },
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard: "RFC **9110**" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const named = factsOf(rowOf(root, "risk-1"));
  const unheld = factsOf(rowOf(root, "risk-2"));

  expect({ earlyWarning: named.earlyWarning, prevention: named.prevention }).toEqual({
    earlyWarning: [
      "A reply breaks ",
      { address: toConstraint, id: constraint, words: "RFC 9110" },
      " first.",
    ],
    prevention: ["Run ", { address: toLens, id: FIRST, words: "this lens" }, " again."],
  });
  expect(unheld.earlyWarning, "a line that names no held node has no parts").toBe(
    "Watch risk closely."
  );
});

// A risk the answer does not hold has no row to link and no words to read: its address reads as
// the page's word for a risk, and links nothing. An interaction's address is no lens, risk,
// constraint or finding, and stays as written.
test("an address of a risk the answer does not hold reads as the word for a risk", () => {
  const interaction = "dispatch://interaction/a7000000-0000-4000-8000-000000000007";
  const words = `Watch dispatch://risk/f9000000-0000-4000-8000-000000000009 closely, as ${interaction} said.`;
  const { root } = drawn(
    answer([block("r1", "requirement", KEY, SPEC, 0, { text: words, text_tree: reading(words) })])
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect([lineWords(statement), nodeWords(statement)]).toEqual([
    `Watch risk closely, as ${interaction} said.`,
    `Watch risk closely, as ${interaction} said.`,
  ]);
  expect(linksIn(statement.tree)).toEqual([]);
});

// An address in code - a code span, a code block - reads as its author wrote it, as a rendered
// body draws it; the same address outside code is named. A link to anything else that covers an
// address keeps its own target, and the address in its words reads as the lens's name, as plain
// words in that one link, which holds no second link. A reading that cites no store address is
// the very reading the read sent.
test("an address in code stays as written", () => {
  const cited = `dispatch://lens/${FIRST}`;
  const tree: DisplayTree = {
    content: [
      {
        content: [
          { text: "Run ", type: "text" },
          { marks: [{ type: "inlineCode" }], text: `dispatch read ${cited}`, type: "text" },
          { text: ` before ${cited}, or `, type: "text" },
          {
            marks: [{ attrs: { href: "https://example.com/lens" }, type: "link" }],
            text: cited,
            type: "text",
          },
          { text: ".", type: "text" },
        ],
        type: "paragraph",
      },
      { content: [{ text: `dispatch read ${cited}`, type: "text" }], type: "code_block" },
    ],
    type: "doc",
  };
  const quiet = reading("Nothing to name.");
  const { drafts, root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, { text: "Run it.", text_tree: tree }),
        block("r2", "requirement", KEY, SPEC, 1, { text: "Nothing to name.", text_tree: quiet }),
        lens(FIRST, "r1"),
        TRADE_OFF,
      ],
      [tuple("has_kind", FIRST, TRADE_OFF.id)]
    )
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect(lineWords(statement)).toBe(
    `Run dispatch read ${cited} before Trade-off analysis, or Trade-off analysis.`
  );
  expect(linksIn(statement.tree)).toEqual([
    ["Trade-off analysis", { href: cited, title: rowPath(FIRST) }],
    ["Trade-off analysis", { href: "https://example.com/lens" }],
  ]);
  expect((statement.tree?.content as DisplayTree[] | undefined)?.[1]).toBe(
    (tree.content as DisplayTree[])[1]
  );
  expect(drafts.get(blockNodeId("spec", "r2"))?.tree).toBe(quiet);
});

/** The row of a statement whose words the server read as one paragraph holding `runs`, which
 *  cite the risk `risk`, beside the lens and the risk the answer holds. */
function citingRisk(risk: string, runs: DisplayTree[]): TreeNode {
  const tree: DisplayTree = { content: [{ content: runs, type: "paragraph" }], type: "doc" };
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, { text: "Two regions.", text_tree: tree }),
        lens(FIRST, "r1"),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  return rowOf(root, blockNodeId("spec", "r1"));
}

// An autolink to a store address, `<dispatch://risk/<uuid>>`, is a link whose words are its
// address: it reads as a bare address does, by the node's name in a link to its row where the
// answer holds the node, and by its kind's word with no link where it does not, so neither the
// row's words nor its line draw the address.
test("an autolink to a store address reads as a bare address does", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const unheld = "dispatch://risk/f9000000-0000-4000-8000-000000000009";
  const autolink = (address: string): DisplayTree => ({
    marks: [{ attrs: { href: address }, type: "link" }],
    text: address,
    type: "text",
  });
  const statement = citingRisk(risk, [
    { text: "Two regions, until ", type: "text" },
    autolink(cited),
    { text: " or ", type: "text" },
    autolink(unheld),
    { text: ".", type: "text" },
  ]);

  expect([lineWords(statement), nodeWords(statement)]).toEqual([
    "Two regions, until Failover takes minutes or risk.",
    "Two regions, until Failover takes minutes or risk.",
  ]);
  expect(linksIn(statement.tree)).toEqual([
    ["Failover takes minutes", { href: cited, title: rowPath(risk) }],
  ]);
});

// An author's own words linked to a store address, `[the risk](dispatch://risk/<uuid>)`, keep
// their words and their other marks. Where the answer holds the node, the link opens its row as a
// name's link does, in place of any title the author gave it; where it does not, the link is
// dropped, since it would go nowhere.
test("an author's words linked to a store address link the node's row", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const unheld = "dispatch://risk/f9000000-0000-4000-8000-000000000009";
  const statement = citingRisk(risk, [
    { text: "Two regions, until ", type: "text" },
    {
      marks: [
        { type: "strong" },
        { attrs: { href: cited, title: "/issues/OTHER-1?node=x" }, type: "link" },
      ],
      text: "the risk",
      type: "text",
    },
    { text: " or ", type: "text" },
    { marks: [{ attrs: { href: unheld }, type: "link" }], text: "another", type: "text" },
    { text: ".", type: "text" },
  ]);

  expect([lineWords(statement), nodeWords(statement)]).toEqual([
    "Two regions, until the risk or another.",
    "Two regions, until the risk or another.",
  ]);
  expect((statement.tree?.content as DisplayTree[] | undefined)?.[0]?.content).toEqual([
    { text: "Two regions, until ", type: "text" },
    {
      marks: [{ type: "strong" }, { attrs: { href: cited, title: rowPath(risk) }, type: "link" }],
      text: "the risk",
      type: "text",
    },
    { text: " or ", type: "text" },
    { text: "another", type: "text" },
    { text: ".", type: "text" },
  ]);
});

// A verdict that cites a risk is drawn in the shape of the same verdict that cites none: one line
// of the plain words its stored string reads as, a list's items and emphasis among them, the
// risk's name in place of its address. The one that cites is one paragraph of those words, whose
// link opens the risk's row; the one that cites none has no reading, and its row draws its words
// as they are.
test("a verdict draws one line of plain words whether or not it cites a risk", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const verdict = (last: string) => `Keep _one_ region:\n\n- the cost doubles\n- ${last} first`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", { attributes: { verdict: verdict(cited) } }),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z", {
          attributes: { verdict: verdict("Failover takes minutes") },
        }),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id), tuple("has_kind", SECOND, PRE_MORTEM.id)]
    )
  );
  const citing = rowOf(root, FIRST);
  const plain = rowOf(root, SECOND);

  expect(plain.tree).toBeUndefined();
  expect(plain.text).toBe("Keep one region: the cost doubles Failover takes minutes first");
  expect([citing.text, nodeWords(citing), lineWords(citing)]).toEqual([
    plain.text,
    plain.text,
    plain.text,
  ]);
  expect(citing.tree?.content).toEqual([
    {
      content: [
        { text: "Keep one region: the cost doubles ", type: "text" },
        {
          marks: [{ attrs: { href: cited, title: rowPath(risk) }, type: "link" }],
          text: "Failover takes minutes",
          type: "text",
        },
        { text: " first", type: "text" },
      ],
      type: "paragraph",
    },
  ]);
});

// A verdict's code span that holds a store address keeps it as written, as `plainWords` reads a
// code span; the same address outside the code is named, linked to its row, and nothing else of
// the verdict's plain words changes.
test("a verdict's code span keeps a store address as written while its words name it", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: `Run \`dispatch read ${cited}\` before **${cited}**.` },
        }),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const verdict = rowOf(root, FIRST);

  expect(verdict.text).toBe(`Run dispatch read ${cited} before Failover takes minutes.`);
  expect(verdict.tree?.content).toEqual([
    {
      content: [
        { text: `Run dispatch read ${cited} before `, type: "text" },
        {
          marks: [{ attrs: { href: cited, title: rowPath(risk) }, type: "link" }],
          text: "Failover takes minutes",
          type: "text",
        },
        { text: ".", type: "text" },
      ],
      type: "paragraph",
    },
  ]);
});

// A comment the read sent no reading of is drawn as stored, its references linked by its row's
// link pass. Where it cites a node, its reading is one paragraph of its plain words with the store
// address named, and its other addresses stay as written: the address of a block of the issue is
// left for the row's link pass to link, while the row's line reads it short.
test("a comment with no reading keeps its other references as written where it names a risk", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1"),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        commentNode({
          author: READER_ACTOR,
          body: `See **${cited}** and dispatch://CORE-1/spec#b-r1 first.`,
          created_at: "2026-11-05T10:00:00Z",
          id: "c1",
          reply_to: null,
        }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const comment = rowOf(root, "c1");

  expect(lineWords(comment)).toBe("See Failover takes minutes and CORE-1 spec block first.");
  expect(comment.tree?.content).toEqual([
    {
      content: [
        { text: "See ", type: "text" },
        {
          marks: [{ attrs: { href: cited, title: rowPath(risk) }, type: "link" }],
          text: "Failover takes minutes",
          type: "text",
        },
        { text: " and dispatch://CORE-1/spec#b-r1 first.", type: "text" },
      ],
      type: "paragraph",
    },
  ]);
});

// An author's own link to a store address in a verdict, `[the failover risk](dispatch://risk/…)`,
// keeps the author's words, linked to the risk's row where the answer holds it; a link to
// anything else reads as its words, with no link. The row draws its words from one paragraph of
// the verdict's plain words, and its text is that line.
test("a verdict's author's link to a risk the read holds links its words to the risk's row", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", {
          attributes: {
            verdict: `See [the failover risk](${cited}) first, then [the runbook](https://example.com/r).`,
          },
        }),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const verdict = rowOf(root, FIRST);

  expect([verdict.text, lineWords(verdict)]).toEqual([
    "See the failover risk first, then the runbook.",
    "See the failover risk first, then the runbook.",
  ]);
  expect(verdict.tree?.content).toEqual([
    {
      content: [
        { text: "See ", type: "text" },
        {
          marks: [{ attrs: { href: cited, title: rowPath(risk) }, type: "link" }],
          text: "the failover risk",
          type: "text",
        },
        { text: " first, then the runbook.", type: "text" },
      ],
      type: "paragraph",
    },
  ]);
});

// A risk's heading reads an author's link to a constraint and to a lens the answer holds as a
// verdict does: the author's words, each linked to its node's row.
test("a risk's heading links an author's words to a constraint's and a lens's rows", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const toConstraint = `dispatch://constraint/${constraint}`;
  const toLens = `dispatch://lens/${FIRST}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1"),
        bare("risk-1", "risk", FIRST, {
          attributes: {
            heading: `Replies break [the HTTP rule](${toConstraint}), as [this lens](${toLens}) found.`,
          },
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard: "RFC 9110" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const heading = rowOf(root, "risk-1");

  expect([heading.text, nodeWords(heading)]).toEqual([
    "Replies break the HTTP rule, as this lens found.",
    "Replies break the HTTP rule, as this lens found.",
  ]);
  expect(linksIn(heading.tree)).toEqual([
    ["the HTTP rule", { href: toConstraint, title: rowPath(constraint) }],
    ["this lens", { href: toLens, title: rowPath(FIRST) }],
  ]);
});

// A constraint's words that cite a risk the answer holds are read with their names as a verdict
// is: a standard's bare address as the risk's name, and a measurement's value's author's link as
// the author's words, each linked to the risk's row. The row draws its words from that one
// paragraph, a measurement's `<measurement>: <value>`, and its text is the paragraph's words.
test("a constraint's words link a risk they cite to its row", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const standard = "e5000000-0000-4000-8000-000000000005";
  const measurement = "e6000000-0000-4000-8000-000000000006";
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1"),
        bare(risk, "risk", FIRST, { attributes: { heading: "Failover takes minutes" } }),
        bare(standard, "constraint", KEY, {
          attributes: { standard: `Answer as ${cited} allows.` },
        }),
        bare(measurement, "constraint", KEY, {
          attributes: {
            measurement: "p99 *latency*",
            value: `under [the failover risk](${cited})`,
          },
        }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const link = { href: cited, title: rowPath(risk) };

  expect([rowOf(root, standard).text, rowOf(root, measurement).text]).toEqual([
    "Answer as Failover takes minutes allows.",
    "p99 latency: under the failover risk",
  ]);
  expect(linksIn(rowOf(root, standard).tree)).toEqual([["Failover takes minutes", link]]);
  expect(linksIn(rowOf(root, measurement).tree)).toEqual([["the failover risk", link]]);
  expect(
    (rowOf(root, measurement).tree?.content as DisplayTree[] | undefined)?.map(({ type }) => type)
  ).toEqual(["paragraph"]);
});

// A store address inside a link's words reads as its node's name, or as its kind's word where
// the answer does not hold the node, as plain words in that one link, which holds no second
// link: a link to a store address still opens that node's row, and a link to anything else
// keeps its own target. No text run of the row holds an address or a uuid.
test("a store address inside a link's words reads as its node's name in that one link", () => {
  const risk = "d4000000-0000-4000-8000-000000000004";
  const cited = `dispatch://risk/${risk}`;
  const toRisk = [{ attrs: { href: cited }, type: "link" }];
  const elsewhere = [{ attrs: { href: "https://example.com/" }, type: "link" }];
  const statement = citingRisk(risk, [
    { text: "Two regions, until ", type: "text" },
    { marks: toRisk, text: `dispatch://lens/${FIRST}`, type: "text" },
    { text: " or ", type: "text" },
    { marks: toRisk, text: `dispatch://lens/${SECOND}`, type: "text" },
    { text: "; ", type: "text" },
    { marks: elsewhere, text: `see ${cited}`, type: "text" },
    { text: ".", type: "text" },
  ]);
  const link = [{ attrs: { href: cited, title: rowPath(risk) }, type: "link" }];

  expect((statement.tree?.content as DisplayTree[] | undefined)?.[0]?.content).toEqual([
    { text: "Two regions, until ", type: "text" },
    { marks: link, text: kindWords(PRE_MORTEM_WORDS), type: "text" },
    { text: " or ", type: "text" },
    { marks: link, text: "lens", type: "text" },
    { text: "; ", type: "text" },
    { marks: elsewhere, text: "see Failover takes minutes", type: "text" },
    { text: ".", type: "text" },
  ]);
  expect(STORE_ADDRESS.test(nodeWords(statement))).toBe(false);
});

// An author's link whose words are a code span, ``[`RFC 9110`](dispatch://constraint/<uuid>)``,
// keeps its words as written and links the node's row, as an author's other words do: a verdict
// draws it as one link in its one line.
test("a verdict's author's link whose words are code links the constraint's row", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const held = `dispatch://constraint/${constraint}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: `Held to [\`RFC 9110\`](${held}).` },
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard: "HTTP semantics" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const verdict = rowOf(root, FIRST);

  expect(verdict.text).toBe("Held to RFC 9110.");
  expect(linksIn(verdict.tree)).toEqual([["RFC 9110", { href: held, title: rowPath(constraint) }]]);
});

// A server's reading of an author's link whose words are code keeps the run's code mark and its
// words as written, and links the node's row as a name's link does, in place of the author's
// link to the store address, which opens nothing.
test("a server's reading links an author's code words to the constraint's row", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const held = `dispatch://constraint/${constraint}`;
  const tree: DisplayTree = {
    content: [
      {
        content: [
          { text: "Held to ", type: "text" },
          {
            marks: [{ type: "inlineCode" }, { attrs: { href: held }, type: "link" }],
            text: "RFC 9110",
            type: "text",
          },
          { text: ".", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const { root } = drawn(
    answer([
      block("r1", "requirement", KEY, SPEC, 0, { text: "Held to RFC 9110.", text_tree: tree }),
      bare(constraint, "constraint", KEY, { attributes: { standard: "HTTP semantics" } }),
    ])
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect((statement.tree?.content as DisplayTree[] | undefined)?.[0]?.content).toEqual([
    { text: "Held to ", type: "text" },
    {
      marks: [
        { type: "inlineCode" },
        { attrs: { href: held, title: rowPath(constraint) }, type: "link" },
      ],
      text: "RFC 9110",
      type: "text",
    },
    { text: ".", type: "text" },
  ]);
});

// An author's link whose words are a code span of the held constraint's own address,
// ``[`dispatch://constraint/<uuid>`](dispatch://constraint/<uuid>)``, links nothing: a verdict
// draws its words as written, the code's words, with no link, as the body's reference pass draws
// an anchor whose words are its `dispatch://` address.
test("a verdict's author's link whose code words are its own address draws no link", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const held = `dispatch://constraint/${constraint}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z", {
          attributes: { verdict: `Held to [\`${held}\`](${held}).` },
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard: "HTTP semantics" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );
  const verdict = rowOf(root, FIRST);

  expect(verdict.text).toBe(`Held to ${held}.`);
  expect(linksIn(verdict.tree)).toEqual([]);
});

// A server's reading of that link, a run of the held constraint's own address that carries a
// code mark and a link to that address, keeps the code mark and its words, and carries no link.
test("a server's reading of an author's link whose code words are its own address carries no link", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const held = `dispatch://constraint/${constraint}`;
  const tree: DisplayTree = {
    content: [
      {
        content: [
          { text: "Held to ", type: "text" },
          {
            marks: [{ type: "inlineCode" }, { attrs: { href: held }, type: "link" }],
            text: held,
            type: "text",
          },
          { text: ".", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const { root } = drawn(
    answer([
      block("r1", "requirement", KEY, SPEC, 0, {
        text: `Held to [\`${held}\`](${held}).`,
        text_tree: tree,
      }),
      bare(constraint, "constraint", KEY, { attributes: { standard: "HTTP semantics" } }),
    ])
  );
  const statement = rowOf(root, blockNodeId("spec", "r1"));

  expect((statement.tree?.content as DisplayTree[] | undefined)?.[0]?.content).toEqual([
    { text: "Held to ", type: "text" },
    { marks: [{ type: "inlineCode" }], text: held, type: "text" },
    { text: ".", type: "text" },
  ]);
});

// An author's link whose words carry a mark, `[the **HTTP** rule](dispatch://constraint/<uuid>)`,
// is one link: an early warning's parts hold one chip of the constraint, of all its words.
test("an early warning's author's link whose words carry a mark is one chip", () => {
  const constraint = "e5000000-0000-4000-8000-000000000005";
  const held = `dispatch://constraint/${constraint}`;
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1"),
        bare("risk-1", "risk", FIRST, {
          attributes: {
            early_warning: `A reply breaks [the **HTTP** rule](${held}) first.`,
            heading: "It breaks.",
          },
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard: "RFC 9110" } }),
        PRE_MORTEM,
      ],
      [tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  );

  expect(factsOf(rowOf(root, "risk-1")).earlyWarning).toEqual([
    "A reply breaks ",
    { address: held, id: constraint, words: "the HTTP rule" },
    " first.",
  ]);
});

// A lens's row carries a chip for each lens its `triggered_by` names and for each lens whose
// `triggered_by` names it, named by its kind's words as its tag is, linked where a step drafted
// it, and with that lens's state and visibility. Every lens shares one map of every lens of the
// answer, which the chain a lens draws reads.
test("a trigger chip names each lens the read holds by its kind's words", () => {
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
      words: kindWords(PRE_MORTEM_WORDS),
    },
    {
      address: `dispatch://lens/${THIRD}`,
      state: "live",
      visibility: "author",
      words: kindWords(PRE_MORTEM_WORDS),
    },
  ]);
  expect(first.lenses).toBe(second.lenses);
  expect([...(first.lenses ?? new Map()).entries()]).toEqual([
    [
      `dispatch://lens/${FIRST}`,
      { id: FIRST, kindWords: "trade-off analysis", onWords: "Words of r1." },
    ],
    [
      `dispatch://lens/${SECOND}`,
      {
        id: SECOND,
        kindWords: PRE_MORTEM_WORDS,
        onWords: "Words of r1.",
      },
    ],
    [`dispatch://lens/${THIRD}`, { kindWords: PRE_MORTEM_WORDS }],
  ]);
});

// The server answers a tuple with the page that holds its `from` node, wherever the whole answer
// holds its `to` node, so a page read so far can name a node that stands on a page not yet read.
// Its chip is drawn at once: a statement's Constrained by, a lens's Triggered by and a risk's
// Prevented by chip each name it by the page's word for the kind its tuple names, linked by its
// read id to Graph's row. A lens's Triggered chip comes with the lens that names it, on that
// lens's page. Once that page is read, the answer holds the node, and each chip is that node's,
// by its own words.
test("a chip whose far end no page read so far holds names its kind and links its row, and takes its words once that page is read", () => {
  const constraint = "d4000000-0000-4000-8000-000000000004";
  const cause = "f6000000-0000-4000-8000-000000000006";
  const follow = "f7000000-0000-4000-8000-000000000007";
  const checkId = `${KEY}/verification-md#c1`;
  const statement = blockNodeId("spec", "r1");
  /** The answer as the pages read so far hold it, and with `far` and its tuples `farTuples`
   *  once the page that holds them is read. */
  const tree = (far: readonly TreeAnswerNode[] = [], farTuples: readonly TreeTuple[] = []) =>
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z"),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z"),
        bare("risk-a", "risk", SECOND, { attributes: { heading: "The table locks." } }),
        PRE_MORTEM,
        TRADE_OFF,
        ...far,
      ],
      [
        tuple("has_kind", FIRST, TRADE_OFF.id),
        tuple("has_kind", SECOND, PRE_MORTEM.id),
        tuple("constrained_by", "r1", constraint),
        tuple("triggered_by", SECOND, cause),
        tuple("prevented_by", "risk-a", checkId),
        ...farTuples,
      ]
    );
  const unread = drawn(tree()).root;

  expect(factsOf(rowOf(unread, SECOND)).triggeredBy).toStrictEqual([
    {
      address: `dispatch://lens/${cause}`,
      chrome: true,
      graphOnly: true,
      id: cause,
      words: kindWords(NODE_TYPE_NAMES.lens),
    },
  ]);
  expect(
    factsOf(rowOf(unread, FIRST)).triggered,
    "a Triggered chip waits for the page of the lens that names it"
  ).toStrictEqual([]);
  expect(factsOf(rowOf(unread, "risk-a")).preventedBy).toStrictEqual([
    {
      address: checkId,
      chrome: true,
      graphOnly: true,
      id: checkId,
      words: kindWords(NODE_TYPE_NAMES.check),
    },
  ]);
  expect(
    factsOf(rowOf(unread, statement)),
    "the statement draws its Constrained by chip"
  ).toStrictEqual({
    ...NO_FACTS,
    constrainedBy: [
      {
        address: `dispatch://constraint/${constraint}`,
        chrome: true,
        graphOnly: true,
        id: constraint,
        words: kindWords(NODE_TYPE_NAMES.constraint),
      },
    ],
  });

  const read = drawn(
    tree(
      [
        bare(constraint, "constraint", KEY, { attributes: { standard: "RFC **9110**" } }),
        lens(cause, "r1", "2026-11-05T11:00:00Z"),
        lens(follow, "r1", "2026-11-05T12:00:00Z"),
        block("c1", "check", "r1", VERIFICATION, 0),
      ],
      [
        tuple("has_kind", cause, TRADE_OFF.id),
        tuple("triggered_by", follow, FIRST),
        tuple("has_kind", follow, PRE_MORTEM.id),
      ]
    )
  ).root;

  expect(factsOf(rowOf(read, statement)).constrainedBy).toStrictEqual([
    {
      address: `dispatch://constraint/${constraint}`,
      id: constraint,
      state: "live",
      visibility: "project",
      words: "RFC 9110",
    },
  ]);
  expect(factsOf(rowOf(read, SECOND)).triggeredBy).toStrictEqual([
    {
      address: `dispatch://lens/${cause}`,
      id: cause,
      state: "live",
      visibility: "project",
      words: "Trade-off analysis",
    },
  ]);
  expect(factsOf(rowOf(read, FIRST)).triggered).toStrictEqual([
    {
      address: `dispatch://lens/${follow}`,
      id: follow,
      state: "live",
      visibility: "project",
      words: kindWords(PRE_MORTEM_WORDS),
    },
  ]);
  expect(factsOf(rowOf(read, "risk-a")).preventedBy).toStrictEqual([
    {
      address: rowOf(read, blockNodeId("verification-md", "c1")).reference ?? "",
      id: checkId,
      state: "live",
      visibility: "project",
      words: "Words of c1.",
    },
  ]);
});

// A risk's row draws its heading, its early warning and its prevention, each read to
// plain words, and a chip for each check its `prevented_by` names, named by that check's whole
// text in the tree - both its blocks, past the 32 characters part G cuts a chip at, with no cut -
// not by the first block its row draws a line from, and linked to its row; a risk no check
// prevents has none.
test("a check's chip takes its words whole from its node", () => {
  const whole = "C1. The index builds concurrently. Then it is checked.";
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        block("c1", "check", "r1", VERIFICATION, 0, {
          text_tree: reading("C1. The index builds concurrently.", "Then it is checked."),
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
        id: `${KEY}/verification-md#c1`,
        state: "live",
        visibility: "project",
        words: whole,
      },
    ],
  });
  expect(lineWords(checkRow), "the check's row draws its first block as its line").toBe(
    "C1. The index builds concurrently."
  );
  expect(checkRow.reference).toBe(`dispatch://${KEY}/verification#b-c1`);
  expect(factsOf(rowOf(root, "risk-a")).preventedBy).toEqual([]);
  expect(whole.length, "the check's text fits a cut chip").toBeGreaterThan(32);
});

// From Graph's answer each constraint of the issue is a row at the top level, in the read's
// order, its words a standard's or a measurement's, its reference or its place on its own line,
// each read to plain words; a statement with a `constrained_by` tuple carries a chip for each,
// linked to its row. Conversation's answer holds the issue's constraints only as context, and
// one a lens stands on as that lens's ancestor: only that one is a row there, with the lens under
// it. The statement still carries a chip of each, and the one Conversation draws no row of links
// Graph's row of it (`graphOnly`).
test("a constraint is a top-level row in Graph, and in Conversation only as a lens's parent, and its statement carries its chip", () => {
  const standard = "constraint/d4000000-0000-4000-8000-000000000004";
  const measured = "e5000000-0000-4000-8000-000000000005";
  /** The issue's two constraints, each with the role `role` in the answer. */
  const constraints = (role: TreeAnswerNode["role"]) => [
    bare(standard, "constraint", KEY, {
      attributes: {
        reference: "section 9.2.2, as dispatch://CORE-2 reads it",
        standard: "RFC **9110**",
      },
      made_at: "2026-11-05T09:00:00Z",
      role,
    }),
    bare(measured, "constraint", KEY, {
      attributes: {
        measurement: "p99 write latency",
        taken_where: "staging, 2026-11-15",
        value: "41 ms",
      },
      made_at: "2026-11-05T10:00:00Z",
      role,
    }),
  ];
  const constrained = [
    tuple("constrained_by", "r1", standard),
    tuple("constrained_by", "r1", measured),
  ];
  const { root } = drawn(
    answer([block("r1", "requirement", KEY, SPEC, 0), ...constraints("tree")], constrained)
  );
  const standardChip = {
    address: "dispatch://constraint/d4000000-0000-4000-8000-000000000004",
    id: standard,
    state: "live",
    visibility: "project",
    words: "RFC 9110",
  };
  const measuredChip = {
    address: `dispatch://constraint/${measured}`,
    id: measured,
    state: "live",
    visibility: "project",
    words: "p99 write latency: 41 ms",
  };

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
  expect(factsOf(rowOf(root, blockNodeId("spec", "r1"))).constrainedBy).toStrictEqual([
    standardChip,
    measuredChip,
  ]);

  const conversation = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        ...constraints("context"),
        lens(FIRST, measured),
        PRE_MORTEM,
      ],
      [...constrained, tuple("has_kind", FIRST, PRE_MORTEM.id)]
    )
  ).root;
  expect(childIds(conversation), "Conversation draws the constraints no lens stands on").toEqual([
    blockNodeId("spec", "r1"),
    measured,
  ]);
  expect(childIds(rowOf(conversation, measured))).toEqual([FIRST]);
  expect([rowOf(conversation, measured).text, factsOf(rowOf(conversation, measured))]).toEqual([
    "p99 write latency: 41 ms",
    { ...NO_FACTS, takenWhere: "staging, 2026-11-15" },
  ]);
  expect(factsOf(rowOf(conversation, blockNodeId("spec", "r1"))).constrainedBy).toStrictEqual([
    { ...standardChip, graphOnly: true },
    measuredChip,
  ]);
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
// the issue, draws no row; a statement's chip names a catalogue constraint by its whole words,
// and links nothing. A lens kind is no row either.
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
          role: "context",
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
    words: standard,
  });
});

// A chip, a tag and the chain's place draw their node's words whole, however long. Each case
// holds words longer than the 32 characters part G cuts a chip at: a lens's kind, a constraint's
// standard, a check's text over two blocks, a statement's text over two blocks and a risk's
// heading.
test("a lens chip, a check's chip, a constraint's chip and a lens's place hold whole words", () => {
  const constraint = "d4000000-0000-4000-8000-000000000004";
  const standard = "Web Content Accessibility Guidelines 2.2, success criterion 2.4.11";
  const checkWords =
    "C1. The migration builds the index without locking the table. Then it checks.";
  const statementWords =
    "The listener keeps each notice for a day and drops each older one. The next notice arrives.";
  const heading = "Nobody reads the warning, and the table stays locked for a minute.";
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0, {
          text_tree: reading(
            "The listener keeps each notice for a day and drops each older one.",
            "The next notice arrives."
          ),
        }),
        block("c1", "check", "r1", VERIFICATION, 0, {
          text_tree: reading(
            "C1. The migration builds the index without locking the table.",
            "Then it checks."
          ),
        }),
        bare(constraint, "constraint", KEY, { attributes: { standard } }),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z"),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z"),
        bare("risk-a", "risk", FIRST, { attributes: { heading } }),
        lens(THIRD, "risk-a", "2026-11-05T11:00:00Z"),
        PRE_MORTEM,
        TRADE_OFF,
      ],
      [
        tuple("has_kind", FIRST, PRE_MORTEM.id),
        tuple("has_kind", SECOND, PRE_MORTEM.id),
        tuple("has_kind", THIRD, TRADE_OFF.id),
        tuple("triggered_by", SECOND, FIRST),
        tuple("prevented_by", "risk-a", "c1"),
        tuple("constrained_by", "r1", constraint),
      ]
    )
  );
  const kind = kindWords(PRE_MORTEM_WORDS);
  const first = factsOf(rowOf(root, FIRST));
  const chips = [
    ...factsOf(rowOf(root, blockNodeId("spec", "r1"))).constrainedBy,
    ...factsOf(rowOf(root, "risk-a")).preventedBy,
    ...first.triggered,
    ...factsOf(rowOf(root, SECOND)).triggeredBy,
  ];

  expect(chips.map((chip) => chip.words)).toEqual([standard, checkWords, kind, kind]);
  for (const chip of chips) expect(chip.words.length).toBeGreaterThan(32);
  expect(lensChipWords(PRE_MORTEM_WORDS)).toEqual({ words: kind });
  expect(lensChipWords("")).toEqual({ chrome: true, words: kindWords(NODE_TYPE_NAMES.lens) });
  expect([...(first.lenses ?? new Map()).values()].map((place) => place.onWords)).toEqual([
    statementWords,
    statementWords,
    heading,
  ]);
});

// A lens's tag draws its kind's words, which its row carries (`LensFacts.kind`), capitalised as
// `lensChipWords` names a lens, and a chip that names the lens - here the "Triggered by" chip of
// the lens it triggered - draws the same words. Both are whole however long a kind's words run:
// 129 characters here, past the 32 characters part G cuts a chip at.
test("a lens's chip and tag draw its kind's words whole", () => {
  const words =
    "pre-mortem: imagine the work failed a year from now, then name each warning the team missed and say why nobody acted on it at all";
  expect(words).toHaveLength(129);
  const long = lensKind("long-pre-mortem", { words });
  const { root } = drawn(
    answer(
      [
        block("r1", "requirement", KEY, SPEC, 0),
        lens(FIRST, "r1", "2026-11-05T09:00:00Z"),
        lens(SECOND, "r1", "2026-11-05T10:00:00Z"),
        long,
        TRADE_OFF,
      ],
      [
        tuple("has_kind", FIRST, long.id),
        tuple("has_kind", SECOND, TRADE_OFF.id),
        tuple("triggered_by", SECOND, FIRST),
      ]
    )
  );
  const kind = factsOf(rowOf(root, FIRST)).kind;
  const [chip] = factsOf(rowOf(root, SECOND)).triggeredBy;

  expect(kind, "the tag's words are cut").toBe(words);
  expect(lensChipWords(kind)).toEqual({ words: kindWords(words) });
  expect(chip?.words, "the chip's words are cut").toBe(kindWords(words));
  expect(chip?.words).toHaveLength(129);
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
    [`${KEY}/spec#r1`, [KEY, blockNodeId("spec", "r1")]],
    [
      `${KEY}/verification-md#c1`,
      [KEY, blockNodeId("spec", "r1"), blockNodeId("verification-md", "c1")],
    ],
  ] as const) {
    const { target } = issueAddress(
      { key: KEY, kind: "issue" },
      `?node=${encodeURIComponent(readId)}`,
      "",
      "spec"
    );

    expect(target).toEqual({ id: readId, kind: "node" });
    expect(pathToTarget(root, target)?.map((one) => one.id)).toEqual([...chain]);
  }
});

// R7: Graph's own address names a lens's row by `?node=` as the issue's own address does, with
// the preset Graph and the words filter kept, and a zoom address (`/graph/<node id>`) keeps its
// zoom. A filter's address reads no node from `?node=`: it opens its own view, on no node.
test("Graph's address names a row by its ?node= id, and a filter's address names none", () => {
  const { root } = drawn(
    answer(
      [block("r1", "requirement", KEY, SPEC, 0), lens(FIRST, "r1"), TRADE_OFF],
      [tuple("has_kind", FIRST, TRADE_OFF.id)]
    )
  );
  const search = `?q=region&node=${encodeURIComponent(FIRST)}`;
  const graph = issueAddress({ key: KEY, kind: "graph" }, search, "", "spec");
  const zoomed = issueAddress(
    { key: KEY, kind: "graph", node: blockNodeId("spec", "r1") },
    search,
    "",
    "spec"
  );

  expect(graph).toStrictEqual({
    document: undefined,
    documentItem: undefined,
    filter: "region",
    nodeMenu: false,
    preset: "graph",
    target: { id: FIRST, kind: "node" },
    version: undefined,
  });
  expect([zoomed.preset, zoomed.zoom, zoomed.target]).toEqual([
    "graph",
    blockNodeId("spec", "r1"),
    { id: FIRST, kind: "node" },
  ]);
  for (const address of [graph, zoomed]) {
    expect(pathToTarget(root, address.target)?.map((one) => one.id)).toEqual([
      KEY,
      blockNodeId("spec", "r1"),
      FIRST,
    ]);
  }
  for (const kind of ["spec", "verification", "plan", "decisions"] as const) {
    const filter = issueAddress({ key: KEY, kind }, search, "", "spec");
    expect(filter.preset, `/${kind}?node= opens another view`).toBe(kind);
    expect(filter.target, `/${kind}?node= names a node`).toBeUndefined();
    expect(filter.document, `/${kind}?node= opens a document`).toBeUndefined();
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
        id: `${KEY}/verification-md#c1`,
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
