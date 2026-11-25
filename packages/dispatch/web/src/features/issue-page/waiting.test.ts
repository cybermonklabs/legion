import { expect, test } from "bun:test";

import type {
  Artifact,
  ArtifactBlock,
  Comment,
  Decision,
  DisplayTree,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
} from "../../api/types";
import { anchorQuote, changeAnchor } from "../decision-card/change-anchor";
import { wordsOf } from "../refs/tree-fragment";
import { decisionChangeComments } from "./decision-review";
import { buildIssueTree, type DocumentBlocks, isStatement, type TreeNode } from "./issue-tree";
import { coverageStatements } from "./tree-drafts";
import { nodesInOrder } from "./tree-keys";
import { waitingOnReader } from "./waiting";

/**
 * What waits on the reader, read two ways from the same reads: the header's count
 * (`waitingOnReader`) and the tree's own `waiting` on each node. The two must agree, or the
 * count puts the focus on a node that does not wait, or counts one it can never reach.
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
const made = issueDocument("artifact-decisions", "decisions-md");

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
  artifacts: [spec, made],
  assignee: "alice",
  blocked_by: [],
  children: [],
  claim: null,
  closed_at: null,
  components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: made.id,
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

const decisions: IssueDecisions = {
  decisions: [
    waitingDecision("listed", ["r1"]),
    waitingDecision("untied", []),
    waitingDecision("changed", ["r1"]),
  ],
  document: `dispatch://${KEY}/artifact/decisions-md`,
  open_questions: [],
};

/** The Change a person asked of the decision `changed`, after its words last changed. */
const change: Comment = {
  anchor: {
    artifact_id: made.id,
    block_id: "changed-words",
    mark_id: "change-1",
    orphaned: false,
    quote: "The words of changed.",
    version: 3,
  },
  answers: null,
  artifact_id: made.id,
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

/** Which addresses name a statement of the issue, as the header's count is handed them: by the
 *  one rule the tree's review list reads (`coverageStatements`). */
const isCoverageStatement = coverageStatements(issue, coverage);

/** The issue's plan, whose `requirement` blocks no coverage row holds, and the issue with it. */
const plan = issueDocument("artifact-plan", "plan-md");
const planned: IssueDetails = {
  ...issue,
  artifacts: [spec, made, plan],
  plan_artifact_id: plan.id,
};

/** A `requirement` block of a document, as the blocks read answers it. */
function requirementBlock(id: string): ArtifactBlock {
  return {
    attributes: [],
    from: 0,
    id,
    references: { asks: 0, comments: 0 },
    to: 1,
    token: id,
    type: "requirement",
  };
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

/** A run of text of a reading (`Decision.text_tree`), as the server writes one. */
function run(text: string): DisplayTree {
  return { text, type: "text" };
}

/** A paragraph of a reading, holding `text`. */
function paragraph(text: string): DisplayTree {
  return { content: [run(text)], type: "paragraph" };
}

/** A heading of a reading, of level `level`, holding `text`. */
function headingNode(level: number, text: string): DisplayTree {
  return { attrs: { level }, content: [run(text)], type: "heading" };
}

/** A bullet list of a reading, each item one paragraph holding its words. */
function bulletList(items: readonly string[]): DisplayTree {
  return {
    content: items.map((item) => ({
      attrs: { spread: false },
      content: [paragraph(item)],
      type: "list_item",
    })),
    type: "bullet_list",
  };
}

/** A decision whose section is the heading `heading` and then `markdown`, with the server's
 *  reading of it (`text_tree`): the heading, then the blocks `body`. */
function readDecision(
  blockId: string,
  heading: string,
  markdown: string,
  body: readonly DisplayTree[]
): Decision {
  return {
    ...waitingDecision(blockId, []),
    heading,
    text: `## ${heading}\n\n${markdown}`,
    text_tree: {
      content: [headingNode(2, heading), ...body],
      type: "doc",
    },
  };
}

/** The top-level nodes `row`'s section adds to the reading of the whole decisions document: its
 *  heading, at `level` (`##`, or `###` under `## Replaced`), then the blocks of its own reading
 *  below its heading - the very nodes, so `changePlaces` finds its first block among them. */
function sectionOf(row: Decision, level = 2): DisplayTree[] {
  const reading = row.text_tree?.content;
  return [headingNode(level, row.heading), ...(Array.isArray(reading) ? reading.slice(1) : [])];
}

/** The server's reading of a whole decisions document (`markdown_tree` of its text read): its
 *  title, the paragraph `intro` below it, then `nodes`. */
function decisionsDocument(intro: string, nodes: readonly DisplayTree[]): DisplayTree {
  return {
    content: [headingNode(1, "Decisions of CORE-1"), paragraph(intro), ...nodes],
    type: "doc",
  };
}

/** The first block below its heading of `row`'s own reading: the block a Change of it quotes. */
function firstBlockOf(row: Decision): DisplayTree {
  const reading = row.text_tree?.content;
  const block: DisplayTree | undefined = Array.isArray(reading)
    ? reading.find((node: DisplayTree) => node.type !== "heading")
    : undefined;
  if (block === undefined) throw new Error(`${row.block_id} holds no block below its heading`);
  return block;
}

/** Where the server puts a Change of each of `rows` written with the anchor `changeAnchor` gives
 *  it over `document`, the reading of the whole decisions document (`chosen`), and where that
 *  row's own first block below its heading starts (`own`), both in the document's text as
 *  `pmdoc.FindQuote` reads it: the words of `document`. The server lists every match of the
 *  quote, overlapping ones included, and takes the one the occurrence names; an anchor with no
 *  occurrence chooses nothing here. A row's first block is one of `document`'s own top-level nodes
 *  (`sectionOf`), and its words are the last of the document's words up to and including it. */
function changePlaces(
  rows: readonly Decision[],
  document: DisplayTree
): { chosen: (number | undefined)[]; own: number[] } {
  const text = wordsOf(document);
  const nodes: readonly DisplayTree[] = Array.isArray(document.content) ? document.content : [];
  const chosen = rows.map((row) => {
    const anchor = changeAnchor(row, rows, document);
    if (anchor?.occurrence === undefined) return undefined;
    const matches: number[] = [];
    for (let at = text.indexOf(anchor.quote); at !== -1; at = text.indexOf(anchor.quote, at + 1)) {
      matches.push(at);
    }
    return matches[anchor.occurrence];
  });
  const own = rows.map((row) => {
    const block = firstBlockOf(row);
    const upTo = nodes.slice(0, nodes.indexOf(block) + 1);
    return wordsOf({ ...document, content: upTo }).length - wordsOf(block).length;
  });
  return { chosen, own };
}

// R7, R13, R35: what waits on the reader is each open question whose
// turn is theirs, each decision of the review list - an agent's decision that waits, that a tie
// names a statement for, and that the reader has not asked to change since it changed - and each
// open approval request whose turn is theirs. The tree marks the same question and the same
// decision; an approval request is no node of it. While the login is not known, nothing waits.
test("what waits on the reader is their open questions, the review list and their approval requests", () => {
  const counted = waitingOnReader(
    issue,
    "alice",
    decisions,
    isCoverageStatement,
    [change],
    undefined
  );
  expect(counted).toEqual({ approvals: [spec.id], asks: 1, decisions: 1, total: 3 });

  const root = buildIssueTree({ comments: [change], coverage, decisions, issue, viewer: "alice" });
  // The decisions no question holds are drawn above the conversation (`ROOT_ORDER`).
  expect(waitingIn(root)).toEqual(["decisions-md#b-listed", "mine"]);

  const unknown = waitingOnReader(
    issue,
    undefined,
    decisions,
    isCoverageStatement,
    [change],
    undefined
  );
  expect(unknown.total).toBe(0);
  expect(waitingIn(buildIssueTree({ comments: [change], coverage, decisions, issue }))).toEqual([]);
});

// R7 and R13, and R3 of dispatch://INFRA-1523: a Change its author deleted asks nothing any
// more, so the decision it was asked of is back on the review list and in the header's count,
// in the tree as in the count. The deleted Change keeps its place under that decision, drawn as
// the deleted line, and holds no word of what it asked.
test("a Change its author deleted leaves its decision on the review list and in the count", () => {
  const deleted: Comment = {
    ...change,
    body: "",
    deleted_at: "2026-11-05T12:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const counted = waitingOnReader(
    issue,
    "alice",
    decisions,
    isCoverageStatement,
    [deleted],
    undefined
  );
  expect(counted).toEqual({ approvals: [spec.id], asks: 1, decisions: 2, total: 4 });

  const root = buildIssueTree({ comments: [deleted], coverage, decisions, issue, viewer: "alice" });
  expect(waitingIn(root)).toEqual(["decisions-md#b-listed", "decisions-md#b-changed", "mine"]);
  expect(parentsOf(root, deleted.id)).toEqual(["decisions-md#b-changed"]);
  const node = nodesInOrder(root).find((one) => one.id === deleted.id);
  expect({ deletedAt: node?.deletedAt, text: node?.text }).toEqual({
    deletedAt: "2026-11-05T12:00:00Z",
    text: "Deleted by alice · 2026-11-05T12:00:00Z",
  });
});

// R3, R7 and R13: a Change belongs to the decision whose section of the decisions document holds
// the block the person wrote it in (`Anchor.block_id`), though another decision holds its words
// too. Here the first decision holds the second's words and is tied to no statement, so it is on
// no review list, and the Change is written in the second's paragraph: the second decision then
// waits on its agent, and nothing waits on the reader, in the count as in the tree. A count that
// read the quote alone would put the Change on the first decision and count the second as
// waiting. Where the anchor stores no block, the quote alone decides: the first decision whose
// words hold it.
test("waitingOnReader counts a Change under the decision whose block holds its anchor", () => {
  const worded = (blockId: string, shaped: readonly string[], text: string): Decision => ({
    ...waitingDecision(blockId, shaped),
    text,
  });
  const first = worded("d1", [], "## Keep the model pure\n\nKeep the model pure. Draw it once.");
  const second = worded("d2", ["r1"], "## Draw it once\n\nDraw it once.");
  const both: IssueDecisions = { ...decisions, decisions: [first, second] };
  const anchor = {
    artifact_id: made.id,
    block_id: "d2-words",
    mark_id: "c-change",
    orphaned: false,
    quote: "Draw it once.",
    version: 3,
  };
  const moved: Comment = { ...change, anchor, id: "c-change" };
  // The decisions document's blocks in their order, as the blocks read answers them: each
  // decision's heading, then its paragraph.
  const placed = (id: string, type: string, from: number, to: number): ArtifactBlock => ({
    attributes: [],
    from,
    id,
    references: { asks: 0, comments: 0 },
    to,
    token: id,
    type,
  });
  const documents: DocumentBlocks[] = [
    {
      artifactId: made.id,
      blocks: [
        placed("d1", "heading", 0, 24),
        placed("d1-words", "paragraph", 26, 62),
        placed("d2", "heading", 64, 80),
        placed("d2-words", "paragraph", 82, 96),
      ],
    },
  ];
  const counted = waitingOnReader(issue, "alice", both, isCoverageStatement, [moved], documents);
  const root = buildIssueTree({
    comments: [moved],
    coverage,
    decisions: both,
    documents,
    issue,
    viewer: "alice",
  });
  const waitingDecisions = waitingIn(root).filter((id) => id.startsWith("decisions-md#"));

  expect(counted.decisions).toBe(0);
  expect(waitingDecisions).toEqual([]);
  expect(counted.decisions).toBe(waitingDecisions.length);
  expect(parentsOf(root, moved.id)).toEqual(["decisions-md#b-d2"]);
  expect(decisionChangeComments(both, [moved], issue.artifacts, documents)).toEqual(
    new Map([["c-change", second.ref]])
  );

  const unplaced: Comment = { ...moved, anchor: { ...anchor, block_id: null } };
  expect(decisionChangeComments(both, [unplaced], issue.artifacts, documents)).toEqual(
    new Map([["c-change", first.ref]])
  );
});

// R7, R13 and R35: a statement is a requirement the coverage read holds, as the header's count is
// handed the statements. A decision tied only to a `requirement` block of the plan, which no
// coverage row holds, is on no review list; nor is one tied to the spec's `r1` while the coverage
// read has not answered, though the spec's blocks read has drawn that block already. The tree
// marks neither and the count counts neither, and the decision tied to `r1` once the coverage
// read holds it waits, in the tree as in the count.
test("a tie to a requirement the coverage read does not hold puts no decision on the review list", () => {
  const documents: DocumentBlocks[] = [
    { artifactId: spec.id, blocks: [requirementBlock("r1")] },
    { artifactId: plan.id, blocks: [requirementBlock("p1")] },
  ];
  const tiedToPlan = waitingDecision("planned", ["p1"]);
  const toPlan: Decision = {
    ...tiedToPlan,
    links: (tiedToPlan.links ?? []).map((link) => ({
      ...link,
      to: { ...link.to, id: plan.id, ref: `dispatch://${KEY}/artifact/plan-md#b-p1` },
    })),
  };
  const both: IssueDecisions = {
    ...decisions,
    decisions: [waitingDecision("listed", ["r1"]), toPlan],
  };
  const waitingDecisions = (read: IssueCoverage | undefined): string[] =>
    waitingIn(
      buildIssueTree({
        ...(read === undefined ? {} : { coverage: read }),
        decisions: both,
        documents,
        issue: planned,
        viewer: "alice",
      })
    ).filter((id) => id.startsWith("decisions-md#"));

  expect(waitingDecisions(coverage)).toEqual(["decisions-md#b-listed"]);
  const listed = coverageStatements(planned, coverage);
  expect(waitingOnReader(planned, "alice", both, listed, [], documents).decisions).toBe(1);

  expect(waitingDecisions(undefined)).toEqual([]);
  const unanswered = coverageStatements(planned, undefined);
  expect(waitingOnReader(planned, "alice", both, unanswered, [], documents).decisions).toBe(0);
});

// R7, R13 and R35: the count and the tree read a tie by one rule for a statement
// (`coverageStatements`), whatever address the tie writes. A tie to the spec's `r1`, which the
// coverage read holds, puts its decision on the review list whether it writes the block short
// (`spec#b-r1`) or by the spec's own slug. A tie to the spec's `r2` puts it on none: the spec's
// blocks read drew `r2` as a `requirement` block, whose row is a statement as `r1`'s is, but the
// coverage read does not hold it. Nor does a tie to another issue's `r1`, or one to a
// `requirement` block of the plan. Each decision waits on the reader in the tree where the count
// counts it, and nowhere else.
test("the count and the tree read a tie by one rule for a statement, whatever address it writes", () => {
  const tiedTo = (blockId: string, block: string, documentId: string, ref: string): Decision => {
    const decision = waitingDecision(blockId, [block]);
    return {
      ...decision,
      links: (decision.links ?? []).map((link) => ({
        ...link,
        to: { ...link.to, id: documentId, ref },
      })),
    };
  };
  const reviewed: IssueDecisions = {
    ...decisions,
    decisions: [
      tiedTo("short", "r1", spec.id, `dispatch://${KEY}/spec#b-r1`),
      tiedTo("slugged", "r1", spec.id, `dispatch://${KEY}/artifact/spec#b-r1`),
      tiedTo("unlisted", "r2", spec.id, `dispatch://${KEY}/spec#b-r2`),
      tiedTo("elsewhere", "r1", "artifact-other-spec", "dispatch://ACME-2/spec#b-r1"),
      tiedTo("planned", "p1", plan.id, `dispatch://${KEY}/artifact/plan-md#b-p1`),
    ],
  };
  const documents: DocumentBlocks[] = [
    { artifactId: spec.id, blocks: [requirementBlock("r1"), requirementBlock("r2")] },
    { artifactId: plan.id, blocks: [requirementBlock("p1")] },
  ];
  const root = buildIssueTree({
    coverage,
    decisions: reviewed,
    documents,
    issue: planned,
    viewer: "alice",
  });
  expect(
    nodesInOrder(root)
      .filter(isStatement)
      .map((node) => node.id)
      .sort()
  ).toEqual(["spec#b-r1", "spec#b-r2"]);

  const waitingDecisions = waitingIn(root).filter((id) => id.startsWith("decisions-md#"));
  expect(waitingDecisions).toEqual(["decisions-md#b-short", "decisions-md#b-slugged"]);
  const listed = coverageStatements(planned, coverage);
  const tied = reviewed.decisions ?? [];
  expect(tied).toHaveLength(5);
  for (const decision of tied) {
    const alone: IssueDecisions = { ...reviewed, decisions: [decision] };
    const counted = waitingOnReader(planned, "alice", alone, listed, [], documents);
    const marked = waitingDecisions.includes(`decisions-md#b-${decision.block_id}`);
    expect(counted.decisions, decision.block_id).toBe(marked ? 1 : 0);
  }
});

/** An intro of the decisions document that holds none of the quotes below. */
const PLAIN_INTRO = "The decisions this issue made.";

// A Change is a comment anchored to the decision's first
// block below its heading, and the server refuses a quote that matches more than one place in the
// decisions document unless the write names which one. Here the first and the third decision open
// with the same sentence: each Change names the occurrence that is its own block's, counted over
// the whole document, and the server, choosing among the quote's matches as `pmdoc.FindQuote`
// does, puts each Change on its own decision's block. Where the document's intro quotes that
// sentence too, the intro's match comes first and moves both decisions' occurrences up by one. An
// occurrence of 0 for every decision would put the third's Change on the first decision. A
// decision whose heading is all it holds has no block to anchor a Change to.
test("a Change of a decision whose first sentence another decision also opens with names its own occurrence", () => {
  const first = readDecision("d1", "Purity", "Keep the model pure.\n\nDraw it once.", [
    paragraph("Keep the model pure."),
    paragraph("Draw it once."),
  ]);
  const second = readDecision("d2", "One read", "Read it from one place.", [
    paragraph("Read it from one place."),
  ]);
  const third = readDecision("d3", "Purity again", "Keep the model pure.\n\nTest it alone.", [
    paragraph("Keep the model pure."),
    paragraph("Test it alone."),
  ]);
  const rows = [first, second, third];
  const sections = rows.flatMap((row) => sectionOf(row));

  for (const [intro, occurrences] of [
    [PLAIN_INTRO, [0, 0, 1]],
    ["Keep the model pure.", [1, 0, 2]],
  ] as const) {
    const document = decisionsDocument(intro, sections);
    expect(rows.map((row) => changeAnchor(row, rows, document))).toEqual([
      { occurrence: occurrences[0], quote: "Keep the model pure." },
      { occurrence: occurrences[1], quote: "Read it from one place." },
      { occurrence: occurrences[2], quote: "Keep the model pure." },
    ]);
    const places = changePlaces(rows, document);
    expect(places.chosen).toEqual(places.own);
  }

  const headingOnly = readDecision("d4", "Heading only", "", []);
  expect(changeAnchor(headingOnly, rows, decisionsDocument(PLAIN_INTRO, sections))).toBeUndefined();
});

// The server counts a quote's places over the
// whole decisions document, the text above its first decision included, which no row of the
// decisions read holds. Here the intro quotes the first decision's opening sentence at the end of
// its own words: that is the quote's first match, so the first decision's own block is its
// second, occurrence 1. A count that began at the first decision's heading would name 0, and the
// server would put the Change on the intro, outside every decision.
test("a Change counts the quote's match in the decisions document's intro above the first decision", () => {
  const first = readDecision("d1", "Purity", "Keep the model pure.", [
    paragraph("Keep the model pure."),
  ]);
  const second = readDecision("d2", "One read", "Read it from one place.", [
    paragraph("Read it from one place."),
  ]);
  const rows = [first, second];
  const document = decisionsDocument(
    "Each decision below serves one rule. Keep the model pure.",
    rows.flatMap((row) => sectionOf(row))
  );

  expect(changeAnchor(first, rows, document)).toEqual({
    occurrence: 1,
    quote: "Keep the model pure.",
  });
  expect(changeAnchor(second, rows, document)).toEqual({
    occurrence: 0,
    quote: "Read it from one place.",
  });
  const places = changePlaces(rows, document);
  expect(places.chosen).toEqual(places.own);
});

// The server counts over the whole document,
// the `## Replaced` section and a `#` section included, and the decisions read holds every
// decision in document order, the replaced ones in their place. The decisions are found by the
// server's section rule: a `##` heading outside `## Replaced`, a `###` one under it, and a heading
// inside an open decision that does not close it - "Purity"'s own `### Why` - is part of it, no
// decision. Here "Purity" opens with "Keep the model pure."; "Purity, first draft", replaced,
// opens with it under `## Replaced`; a `#` section after it quotes it; and "Purity again", after
// that section, opens with it too. With the intro quoting it as well, the three decisions name
// occurrences 1, 2 and 4, the `#` section's match being the fourth, and the server puts each
// Change on its own decision's block.
test("a Change counts the quote's matches in a replaced decision and in a # section", () => {
  const purity = readDecision("d1", "Purity", "Keep the model pure.\n\n### Why\n\nOne read.", [
    paragraph("Keep the model pure."),
    headingNode(3, "Why"),
    paragraph("One read draws it."),
  ]);
  const firstDraft: Decision = {
    ...readDecision("d2", "Purity, first draft", "Keep the model pure.\n\nDraw it twice.", [
      paragraph("Keep the model pure."),
      paragraph("Draw it twice."),
    ]),
    replaced: true,
  };
  const again = readDecision("d3", "Purity again", "Keep the model pure.\n\nTest it alone.", [
    paragraph("Keep the model pure."),
    paragraph("Test it alone."),
  ]);
  const rows = [purity, firstDraft, again];
  const document = decisionsDocument("Each decision below serves one rule. Keep the model pure.", [
    ...sectionOf(purity),
    headingNode(2, "Replaced"),
    ...sectionOf(firstDraft, 3),
    headingNode(1, "Appendix"),
    paragraph("Keep the model pure. Every decision above says so."),
    ...sectionOf(again),
  ]);

  expect(rows.map((row) => changeAnchor(row, rows, document))).toEqual([
    { occurrence: 1, quote: "Keep the model pure." },
    { occurrence: 2, quote: "Keep the model pure." },
    { occurrence: 4, quote: "Keep the model pure." },
  ]);
  const places = changePlaces(rows, document);
  expect(places.chosen).toEqual(places.own);
});

// The server's text holds one space between two list items
// (`buildFlattenedText`), so a decision whose first block below its heading is a list is quoted by
// the list's words, its items one space apart, and the same words in an earlier decision's list
// are a match above it. The second decision's list holds "Hold the deploy." and "Then ship.":
// where the first decision's list holds only "Hold the deploy.", the quote matches nowhere in it;
// where it holds both items, the quote matches there first and the occurrence is one more. An
// intro quoting the list's words adds one more again. Either way the server puts the Change on
// the second decision's list.
test("a Change of a decision whose first block is a list counts the same words inside an earlier decision's list item", () => {
  const second = readDecision("d2", "Ship after the hold", "- Hold the deploy.\n- Then ship.", [
    bulletList(["Hold the deploy.", "Then ship."]),
  ]);
  expect(anchorQuote(second)).toBe("Hold the deploy. Then ship.");

  for (const [items, inFirst] of [
    [["Hold the deploy."], 0],
    [["Hold the deploy.", "Then ship."], 1],
  ] as const) {
    const first = readDecision("d1", "Freeze", items.map((item) => `- ${item}`).join("\n"), [
      bulletList(items),
    ]);
    const rows = [first, second];
    for (const [intro, inIntro] of [
      [PLAIN_INTRO, 0],
      ["Hold the deploy. Then ship.", 1],
    ] as const) {
      const document = decisionsDocument(
        intro,
        rows.flatMap((row) => sectionOf(row))
      );
      expect(changeAnchor(second, rows, document)).toEqual({
        occurrence: inIntro + inFirst,
        quote: "Hold the deploy. Then ship.",
      });
      const places = changePlaces(rows, document);
      expect(places.chosen).toEqual(places.own);
    }
  }
});

// The server lists every match of a quote, overlapping ones
// included (`findAll`), so a match that starts in a decision's heading and runs into its first
// block is listed before the block's own. The second decision's heading ends with "go" and its
// first paragraph is "go go go": its words, "Ready, set, go go go go", hold the quote where the
// heading's "go" starts and again at the paragraph, and the first decision's "go go go go" holds
// it twice more, overlapping. The paragraph's own match is the fourth, occurrence 3; in a document
// holding no decision above it, the second. An intro of "go go go" adds one match above each.
test("a Change counts a match that starts in the decision's heading and runs into its first block", () => {
  const first = readDecision("d1", "Chant", "go go go go", [paragraph("go go go go")]);
  const second = readDecision("d2", "Ready, set, go", "go go go", [paragraph("go go go")]);
  const rows = [first, second];

  for (const [intro, inIntro] of [
    [PLAIN_INTRO, 0],
    ["go go go", 1],
  ] as const) {
    const document = decisionsDocument(
      intro,
      rows.flatMap((row) => sectionOf(row))
    );
    expect(changeAnchor(second, rows, document)).toEqual({
      occurrence: 3 + inIntro,
      quote: "go go go",
    });
    const places = changePlaces(rows, document);
    expect(places.chosen).toEqual(places.own);
    const alone = decisionsDocument(intro, sectionOf(second));
    expect(changeAnchor(second, [second], alone)).toEqual({
      occurrence: 1 + inIntro,
      quote: "go go go",
    });
  }
});

// The occurrence is counted only where the decisions document's reading lines up with the
// decisions read: the heading the server's section rule finds at the decision's own index in the
// read reads as the decision's heading, and the node right below it is a block that reads as the
// quote. Where the reading is not in hand, where it is of another version than the read - a
// decision the read holds that the reading lacks, a heading or a first block worded otherwise -
// where a sub-heading stands right below the decision's heading, or where the read does not hold
// the decision, the anchor is the quote alone: the server anchors it where the quote stands once
// and refuses it where the quote stands more than once, rather than land it on another place.
test("a Change names no occurrence where the decisions document's reading does not line up with the decisions read", () => {
  const first = readDecision("d1", "Purity", "Keep the model pure.", [
    paragraph("Keep the model pure."),
  ]);
  const second = readDecision("d2", "One read", "Read it from one place.", [
    paragraph("Read it from one place."),
  ]);
  const third = readDecision("d3", "Purity again", "Keep the model pure.\n\nTest it alone.", [
    paragraph("Keep the model pure."),
    paragraph("Test it alone."),
  ]);
  const rows = [first, second, third];
  const intro = "Keep the model pure.";
  const quoteAlone = { quote: "Keep the model pure." };
  const sectionsWith = (thirdSection: readonly DisplayTree[]): DisplayTree =>
    decisionsDocument(intro, [...sectionOf(first), ...sectionOf(second), ...thirdSection]);

  expect(changeAnchor(third, rows, sectionsWith(sectionOf(third)))).toEqual({
    occurrence: 2,
    quote: "Keep the model pure.",
  });

  expect(changeAnchor(third, rows, undefined)).toStrictEqual(quoteAlone);
  const lacking = decisionsDocument(intro, [...sectionOf(first), ...sectionOf(third)]);
  expect(changeAnchor(third, rows, lacking)).toStrictEqual(quoteAlone);
  expect(changeAnchor(second, rows, lacking)).toStrictEqual({ quote: "Read it from one place." });
  const renamed = sectionsWith([
    headingNode(2, "Purity, again"),
    paragraph("Keep the model pure."),
    paragraph("Test it alone."),
  ]);
  expect(changeAnchor(third, rows, renamed)).toStrictEqual(quoteAlone);
  const reworded = sectionsWith([
    headingNode(2, "Purity again"),
    paragraph("Keep the model simple."),
    paragraph("Test it alone."),
  ]);
  expect(changeAnchor(third, rows, reworded)).toStrictEqual(quoteAlone);

  const underWhy = readDecision("d3", "Purity again", "### Why\n\nKeep the model pure.", [
    headingNode(3, "Why"),
    paragraph("Keep the model pure."),
  ]);
  expect(anchorQuote(underWhy)).toBe("Keep the model pure.");
  expect(
    changeAnchor(underWhy, [first, second, underWhy], sectionsWith(sectionOf(underWhy)))
  ).toStrictEqual(quoteAlone);

  const document = sectionsWith(sectionOf(third));
  expect(changeAnchor(third, [first, second], document)).toStrictEqual(quoteAlone);
  expect(changeAnchor(third, undefined, document)).toStrictEqual(quoteAlone);
});

/** The reading of a decisions document whose intro quotes "Keep the model pure.", and whose
 *  decisions "Purity" and "Purity again" each open with it. */
const PURITY_DOCUMENT = decisionsDocument("Keep the model pure.", [
  headingNode(2, "Purity"),
  paragraph("Keep the model pure."),
  headingNode(2, "Purity again"),
  paragraph("Keep the model pure."),
  paragraph("Test it alone."),
]);

// A Change's occurrence, from a pod of the build before during a rolling deploy, whose read sends
// no reading of a decision's text: the decision is then read by its stored text's reading
// (`storedReading`), so the quote is the words of its first block below the heading
// (`anchorQuote`), and the count still comes from the document's reading. The two decisions
// opening with the paragraph name its places after the intro's. A paragraph whose markdown holds
// markup - `**` - quotes its words, not its markdown, so the document's reading lines up with it
// and its anchor names its occurrence, as a read reading's would.
test("a Change of a decision read with no reading is read by its stored text's reading, so its quote is its words and names its occurrence where a read reading's would", () => {
  const worded = (blockId: string, heading: string, text: string): Decision => ({
    ...waitingDecision(blockId, []),
    heading,
    text,
  });
  const first = worded("d1", "Purity", "## Purity\n\nKeep the model pure.");
  const second = worded(
    "d2",
    "Purity again",
    "## Purity again\n\nKeep the model pure.\n\nTest it alone."
  );
  const rows = [first, second];

  expect(rows.map((row) => changeAnchor(row, rows, PURITY_DOCUMENT))).toEqual([
    { occurrence: 1, quote: "Keep the model pure." },
    { occurrence: 2, quote: "Keep the model pure." },
  ]);

  const marked = worded("d1", "Purity", "## Purity\n\n**Keep** the model pure.");
  expect(anchorQuote(marked)).toBe("Keep the model pure.");
  const strong = decisionsDocument(PLAIN_INTRO, [
    headingNode(2, "Purity"),
    {
      content: [
        { marks: [{ type: "strong" }], text: "Keep", type: "text" },
        run(" the model pure."),
      ],
      type: "paragraph",
    },
  ]);
  expect(changeAnchor(marked, [marked], strong)).toStrictEqual({
    occurrence: 0,
    quote: "Keep the model pure.",
  });
});

// One rule reads a decision for a Change: a reading that holds no blocks - a `text_tree` with no
// `content` - is no reading, so the decision is read by its stored text's reading
// (`storedReading`). Its quote is then the words of its first paragraph, which the document's
// reading lines up with here, and a Change quoting its words is placed on it. Read as the blockless
// reading's words, which are none, the decision would hold no block to quote, and a Change quoting
// the first decision's words would be placed on none.
test("a decision whose reading holds no blocks is read by its stored markdown", () => {
  const worded = (blockId: string, heading: string, text: string): Decision => ({
    ...waitingDecision(blockId, []),
    heading,
    text,
    text_tree: { type: "doc" },
  });
  const first = worded("d1", "Purity", "## Purity\n\nKeep the model pure.");
  const second = worded(
    "d2",
    "Purity again",
    "## Purity again\n\nKeep the model pure.\n\nTest it alone."
  );
  const rows = [first, second];

  expect(anchorQuote(first)).toBe("Keep the model pure.");
  expect(rows.map((row) => changeAnchor(row, rows, PURITY_DOCUMENT))).toEqual([
    { occurrence: 1, quote: "Keep the model pure." },
    { occurrence: 2, quote: "Keep the model pure." },
  ]);
  const quoted: Comment = {
    ...change,
    anchor: {
      artifact_id: made.id,
      block_id: null,
      mark_id: "c-quoted",
      orphaned: false,
      quote: "Keep the model pure.",
      version: 3,
    },
    id: "c-quoted",
  };
  expect(
    decisionChangeComments({ ...decisions, decisions: rows }, [quoted], issue.artifacts, undefined)
  ).toEqual(new Map([["c-quoted", first.ref]]));
});
