import { beforeAll, expect, test } from "bun:test";

import type { ArtifactBlock, Decision, DecisionLink, DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { type ChangeComment, decisionChanges, inReviewList } from "../decision-review";
import type { DocumentBlocks, TreeNode } from "../issue-tree";
import { NODE_TYPE_NAMES } from "../page-words";
import { presetView } from "../presets";
import { parentLinkWords } from "../projection-fixture";
import { nodeIdOf as issueNodeIdOf } from "../tree-drafts";
import {
  AGENT,
  answeredAsk,
  ask,
  closing,
  DECISIONS_DOCUMENT,
  decision,
  decisionNode,
  decisionRef,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  READER_ACTOR,
  SPEC_SLUG,
  shaped,
  specRef,
  statement,
} from "./decisions-fixtures";
import {
  conversationPath,
  type DecisionsAsk,
  type DecisionsParts,
  decisionsParts,
  waitingOrder,
} from "./decisions-parts";

/**
 * The Decisions filter's four parts, read from the tree, the decisions read and the asks
 * (R2, R9, R35). The model is pure, so each case hands it a tree built by
 * hand and reads what it answers.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** How this issue addresses a node, which is the page's own rule (`tree-drafts.ts`). */
const nodeIdOf = issueNodeIdOf(issueDetails());

/** The decisions document's blocks as the blocks read answers them: each decision's own block,
 *  its heading, in the order the decisions were made. */
function decisionsBlocks(made: readonly Decision[]): DocumentBlocks[] {
  const heading = (id: string, at: number): ArtifactBlock => ({
    attributes: [],
    from: at * 2,
    id,
    references: { asks: 0, comments: 0 },
    to: at * 2 + 1,
    token: id,
    type: "heading",
  });
  return [
    {
      artifactId: DECISIONS_DOCUMENT.id,
      blocks: made.map((one, at) => heading(one.block_id, at)),
    },
  ];
}

function partsOf(
  children: readonly TreeNode[],
  made: readonly Decision[] = [],
  asks: readonly DecisionsAsk[] = [],
  view?: TreeNode,
  comments: readonly ChangeComment[] = []
): DecisionsParts {
  const decisions = decisionsRead(made);
  return decisionsParts({
    asks,
    changes: decisionChanges(decisions, comments, issueDetails().artifacts, decisionsBlocks(made)),
    decisions,
    issueKey: KEY,
    nodeIdOf,
    reader: READER,
    root: issueTree(children),
    ...(view === undefined ? {} : { view }),
  });
}

/** The node ids of each part, in the order the part draws them. */
function idsOf(parts: DecisionsParts): Record<keyof DecisionsParts, string[]> {
  const ids = (entries: readonly { readonly node: TreeNode }[]) =>
    entries.map((entry) => entry.node.id);
  return {
    agents: ids(parts.agents),
    named: ids(parts.named),
    open_questions: ids(parts.open_questions),
    others: ids(parts.others),
    review: ids(parts.review),
    yours: ids(parts.yours),
  };
}

const D = (block: string) => decisionNode(block).id;

// R2: one list in four parts, in this order: the open questions, those whose turn is the
// reader's first; the decisions waiting for review; the reader's own decisions newest first;
// and the agents' decisions no statement rests on.
test("each ask and each decision stands in one of the four parts", () => {
  const parts = partsOf(
    [
      statement("s1"),
      statement("s2"),
      openAsk("q-agent", false),
      openAsk("q-open"),
      answeredAsk("a-old", { at: "2026-11-12T09:00:00Z", text: "The first one." }),
      answeredAsk("a-new", { at: "2026-11-14T09:00:00Z", text: "The second one." }),
      ask("a-declined", { state: "declined" }),
      decisionNode("review"),
      decisionNode("kept"),
      decisionNode("untied"),
      decisionNode("replaced"),
    ],
    [
      decision("review", { links: [shaped(specRef("s1"))] }),
      decision("kept", {
        kept: {
          artifact_id: "artifact-decisions",
          block_id: "kept",
          changed_version: 1,
          kept_at: "2026-11-15T09:00:00Z",
          kept_by: "alice",
        },
        links: [shaped(specRef("s2"))],
        waiting_review: false,
      }),
      decision("untied"),
      decision("replaced", {
        links: [shaped(specRef("s1"))],
        replaced: true,
        waiting_review: false,
      }),
    ],
    [closing("a-declined", { at: "2026-11-13T09:00:00Z", kind: "declined" })]
  );

  expect(idsOf(parts)).toEqual({
    // A replaced decision is one no statement rests on any longer (R5).
    agents: [D("untied"), D("replaced")],
    named: [],
    open_questions: ["q-open", "q-agent"],
    others: [],
    review: [D("review")],
    // The decision the reader kept, by the time they kept it, among the asks by the time each
    // closed.
    yours: [D("kept"), "a-new", "a-declined", "a-old"],
  });
  // No statement, and nothing else of the tree, is an entry.
  const drawn = Object.values(idsOf(parts)).flat();
  expect(drawn).not.toContain(statement("s1").id);
  expect(drawn).not.toContain(KEY);
});

// R35, c-review-list: four agent decisions that all wait: two a `shaped` tie names a statement
// of the issue for, one tied to a node that is no statement, and one tied to nothing. The
// review list holds the two, newest first, and the other two stand at the foot.
test("the review list is read from the shaped ties alone", () => {
  const made = [
    decision("older", {
      changed_at: "2026-11-10T12:00:00Z",
      links: [shaped(specRef("s1"))],
    }),
    decision("newer", {
      changed_at: "2026-11-11T12:00:00Z",
      links: [shaped(specRef("s2"))],
    }),
    decision("note", { links: [shaped(specRef("note"))] }),
    decision("nothing"),
  ];
  const parts = partsOf(
    [
      statement("s1"),
      statement("s2"),
      node({ id: `${SPEC_SLUG}#b-note`, reference: specRef("note"), type: "callout" }),
      decisionNode("older"),
      decisionNode("newer"),
      decisionNode("note"),
      decisionNode("nothing"),
    ],
    made
  );

  expect(idsOf(parts).review).toEqual([D("newer"), D("older")]);
  expect(idsOf(parts).agents).toEqual([D("note"), D("nothing")]);
  expect(idsOf(parts).yours).toEqual([]);
  // The mark on a row is the model's own state, which every one of the four holds (R35: "one
  // rule decides the list and a second rule decides the mark on a row").
  expect([...parts.review, ...parts.agents].map((entry) => entry.standing)).toEqual([
    "waiting_review",
    "waiting_review",
    "waiting_review",
    "waiting_review",
  ]);

  // The one rule, as the count of what waits on the reader reads it too.
  const isStatement = (reference: string) => /#b-s\d$/.test(reference);
  expect(made.map((row) => inReviewList(row, isStatement))).toEqual([true, true, false, false]);
  // A decision that does not wait is no entry of the list, whatever its ties.
  expect(
    inReviewList(
      decision("kept", { links: [shaped(specRef("s1"))], waiting_review: false }),
      isStatement
    )
  ).toBe(false);
});

// R35: "a statement of this issue". A tie that names a statement of another issue, and one
// whose address the page cannot read, each keep the decision out of the list; a tie pinned to
// a version of the spec names the statement all the same.
test("a tie counts where it names a statement of this issue", () => {
  // A tie's far end with no address of its own (`GraphNode.ref` is optional).
  const { ref: _address, ...unaddressed }: DecisionLink["to"] = shaped(specRef("s1")).to;
  const parts = partsOf(
    [statement("s1"), decisionNode("foreign"), decisionNode("unreadable"), decisionNode("pinned")],
    [
      decision("foreign", { links: [shaped("dispatch://CORE-2/spec#b-s1")] }),
      decision("unreadable", { links: [{ ...shaped(specRef("s1")), to: unaddressed }] }),
      decision("pinned", { links: [shaped(`${specRef("s1")}@v3`)] }),
    ]
  );

  expect(idsOf(parts).review).toEqual([D("pinned")]);
  expect(idsOf(parts).agents).toEqual([D("foreign"), D("unreadable")]);
});

// R2, R9: the filter draws no agent's reading of a person's answer. Today's nearest thing is a
// section of `decisions.md` the server marks a person's because its blocks name an ask: where
// that ask is an answered ask of this issue, the ask's own row is the decision, and it carries
// the statements that section's ties name, which is where today's read holds what the person's
// decision shaped.
test("a section that restates a person's answer is no entry, and its ties are the answer's", () => {
  const parts = partsOf(
    [
      statement("s1"),
      statement("s2"),
      answeredAsk(
        "a1",
        { at: "2026-11-12T09:00:00Z", text: "Keep the rail." },
        {
          children: [decisionNode("restated")],
        }
      ),
      openAsk("q1", true, { children: [decisionNode("pending")] }),
      decisionNode("foreign"),
    ],
    [
      decision("restated", {
        asks: ["a1"],
        kind: "person",
        links: [shaped(specRef("s1")), shaped(specRef("s2"))],
        waiting_review: false,
      }),
      // A section that names a question nobody answered restates no answer.
      decision("pending", { asks: ["q1"], kind: "person", waiting_review: false }),
      // And so does one that names an ask this issue does not hold.
      decision("foreign", { asks: ["elsewhere"], kind: "person", waiting_review: false }),
    ]
  );

  const all = Object.values(idsOf(parts)).flat();
  expect(all).not.toContain(D("restated"));
  expect(idsOf(parts).yours).toEqual(["a1"]);
  expect(parts.yours[0]?.shapes.map((shape) => shape.id)).toEqual([
    statement("s1").id,
    statement("s2").id,
  ]);
  // The server's word for whose they are stands, and no read says which person, so they are
  // no decision of the reader's; neither carries a state of an agent's decision.
  expect(idsOf(parts).others).toEqual([D("pending"), D("foreign")]);
  expect(parts.others[0]).toMatchObject({ kind: "decision", shapes: [] });
  expect(parts.others.find((entry) => "standing" in entry)).toBeUndefined();
});

// R2: the third part is the reader's own decisions. What another person answered, declined or
// kept is theirs, and so is a question its asker took back: each is still an entry of the
// filter, under the heading that says it is not the reader's.
test("another person's answer, decline and keep stand with what others decided", () => {
  const bob = { id: "bob", kind: "user" as const };
  const parts = partsOf(
    [
      statement("s1"),
      answeredAsk("a-mine", { at: "2026-11-14T09:00:00Z", text: "Mine." }),
      answeredAsk("a-bobs", { at: "2026-11-13T09:00:00Z", by: bob, text: "Bob's." }),
      ask("declined-by-bob", { state: "declined" }),
      decisionNode("kept-by-bob"),
    ],
    [
      decision("kept-by-bob", {
        kept: {
          artifact_id: "artifact-decisions",
          block_id: "kept-by-bob",
          changed_version: 1,
          kept_at: "2026-11-15T09:00:00Z",
          kept_by: "bob",
        },
        links: [shaped(specRef("s1"))],
        waiting_review: false,
      }),
    ],
    [closing("declined-by-bob", { actor: bob, at: "2026-11-12T09:00:00Z", kind: "declined" })]
  );

  expect(idsOf(parts).yours).toEqual(["a-mine"]);
  expect(idsOf(parts).others).toEqual([D("kept-by-bob"), "a-bobs", "declined-by-bob"]);
});

// The tester's F6: a Change is a comment a person anchored to the decision's words since it
// last changed. The decision then waits on its agent: it says a change was asked for, it leaves
// the review list for the decisions of whoever asked, and it carries what they wrote. A comment
// older than the decision's words is no change of them, and the decision waits for review.
test("a decision the reader asked to change leaves the review list and carries the change", () => {
  const comment = (id: string, block: string, at: string): ChangeComment => ({
    anchor: {
      artifact_id: DECISIONS_DOCUMENT.id,
      block_id: null,
      mark_id: `mark-${id}`,
      orphaned: false,
      quote: `The decision ${block}.`,
      version: 1,
    },
    author: READER_ACTOR,
    body: `Change ${block}, please.`,
    created_at: at,
    deleted_at: null,
    edited_at: null,
    id,
  });
  const parts = partsOf(
    [statement("s1"), decisionNode("asked"), decisionNode("stale"), decisionNode("waits")],
    [
      decision("asked", { links: [shaped(specRef("s1"))] }),
      decision("stale", { links: [shaped(specRef("s1"))] }),
      decision("waits", { links: [shaped(specRef("s1"))] }),
    ],
    [],
    undefined,
    [
      comment("c-new", "asked", "2026-11-12T09:00:00Z"),
      // Made before the decision last changed (2026-11-10T12:00Z): the agent has answered it.
      comment("c-old", "stale", "2026-11-09T09:00:00Z"),
    ]
  );

  expect(idsOf(parts).review).toEqual([D("stale"), D("waits")]);
  expect(idsOf(parts).yours).toEqual([D("asked")]);
  expect(parts.yours[0]).toMatchObject({
    at: "2026-11-12T09:00:00Z",
    change: {
      at: "2026-11-12T09:00:00Z",
      body: "Change asked, please.",
      by: READER_ACTOR,
      editedAt: null,
      id: "c-new",
      path: `/issues/${KEY}/comments/c-new`,
    },
    standing: "change_requested",
  });
  // The one rule the count of what waits on the reader reads too.
  const isStatement = (reference: string) => /#b-s\d$/.test(reference);
  expect(
    inReviewList(decision("asked", { links: [shaped(specRef("s1"))] }), isStatement, true)
  ).toBe(false);
  expect(waitingOrder(parts)).toEqual([D("stale"), D("waits")]);
});

// R2: a declined ask reads as a question the person declined, never as an answer: the entry
// names who declined and carries the note they wrote where they wrote one, and an ask closed
// any other way carries neither.
test("a declined ask names who declined it and carries their note", () => {
  const parts = partsOf(
    [
      ask("with-note", { state: "declined" }),
      ask("no-note", { state: "declined" }),
      ask("taken-back", { state: "retracted" }),
    ],
    [],
    [
      closing("with-note", {
        at: "2026-11-13T09:00:00Z",
        kind: "declined",
        reason: "The release owner decides this one.",
      }),
      closing("no-note", { at: "2026-11-12T09:00:00Z", kind: "declined" }),
      closing("taken-back", {
        actor: AGENT,
        at: "2026-11-11T09:00:00Z",
        kind: "retracted",
        reason: "Asked twice.",
      }),
    ]
  );

  // The two the reader declined are theirs; the one its asker took back is not.
  expect(idsOf(parts).yours).toEqual(["with-note", "no-note"]);
  expect(idsOf(parts).others).toEqual(["taken-back"]);
  const [withNote, noNote] = parts.yours;
  const [takenBack] = parts.others;
  expect(withNote).toMatchObject({
    declined: {
      by: READER_ACTOR,
      note: "The release owner decides this one.",
      source: { id: "with-note", issue: KEY, kind: "ask" },
    },
    kind: "ask",
  });
  expect(withNote?.node.answer).toBeUndefined();
  expect(noNote?.kind === "ask" ? noNote.declined : undefined).toEqual({
    by: READER_ACTOR,
    source: { id: "no-note", issue: KEY, kind: "ask" },
  });
  expect(takenBack?.kind === "ask" ? takenBack.declined : "none").toBeUndefined();
});

// R2, c-decisions: the closed asks stand by the time each closed, newest first: an answered one
// by its answer's time, any other by its resolution's. An entry the model holds no time for
// stands last, and two of one instant keep the order the outline draws them in.
test("closed asks stand by the time each closed, newest first", () => {
  const parts = partsOf(
    [
      ask("untimed", { at: "not a time", state: "resolved" }),
      answeredAsk("answered-12", { at: "2026-11-12T09:00:00Z", text: "Yes." }),
      ask("declined-14", { at: "2026-11-01T09:00:00Z", state: "declined" }),
      answeredAsk("answered-13-first", { at: "2026-11-13T09:00:00.5Z", text: "No." }),
      answeredAsk("answered-13-second", { at: "2026-11-13T09:00:00.500Z", text: "No." }),
      // No resolution was read for it, so it stands by the time it was asked.
      ask("asked-10", { at: "2026-11-10T08:00:00Z", state: "withdrawn" }),
    ],
    [],
    [closing("declined-14", { at: "2026-11-14T09:00:00Z", kind: "declined" })]
  );

  expect(idsOf(parts).yours).toEqual([
    "declined-14",
    "answered-13-first",
    "answered-13-second",
    "answered-12",
  ]);
  expect(parts.yours.map((entry) => entry.at)).toEqual([
    "2026-11-14T09:00:00Z",
    "2026-11-13T09:00:00.5Z",
    "2026-11-13T09:00:00.500Z",
    "2026-11-12T09:00:00Z",
  ]);
  // The two nobody is recorded as closing are no decision of the reader's, in the same order.
  expect(idsOf(parts).others).toEqual(["asked-10", "untimed"]);
  expect(parts.others.map((entry) => entry.at)).toEqual(["2026-11-10T08:00:00Z", undefined]);
});

// R2: a decision a person kept reads "Kept by" with their name, and stands among the reader's
// own decisions by the time it was kept. One whose words changed after it was kept waits again,
// so it is back in the review list.
test("a kept decision names who kept it, and one changed since waits again", () => {
  const keep = {
    artifact_id: "artifact-decisions",
    block_id: "kept",
    changed_version: 1,
    kept_at: "2026-11-15T09:00:00Z",
    kept_by: "alice",
  };
  const parts = partsOf(
    [statement("s1"), decisionNode("kept"), decisionNode("changed"), decisionNode("loose")],
    [
      decision("kept", { kept: keep, links: [shaped(specRef("s1"))], waiting_review: false }),
      decision("changed", {
        kept: { ...keep, block_id: "changed" },
        links: [shaped(specRef("s1"))],
        waiting_review: true,
      }),
      // Kept, and tied to nothing: an agent's decision no statement rests on, which says who
      // kept it where it stands.
      decision("loose", { kept: { ...keep, block_id: "loose" }, waiting_review: false }),
    ]
  );

  expect(idsOf(parts)).toMatchObject({
    agents: [D("loose")],
    review: [D("changed")],
    yours: [D("kept")],
  });
  expect(parts.yours[0]).toMatchObject({
    at: "2026-11-15T09:00:00Z",
    keptBy: { id: "alice", kind: "user" },
    standing: "kept",
  });
  expect(parts.review[0]).toMatchObject({ standing: "waiting_review" });
  expect(parts.review[0]?.keptBy).toBeUndefined();
  expect(parts.agents[0]).toMatchObject({
    keptBy: { id: "alice", kind: "user" },
    standing: "kept",
  });
});

// c-decisions: "The decision that shaped three requirements is one entry with one node id, and
// it reaches all three."
test("a decision that shaped three statements is one entry that reaches all three", () => {
  const parts = partsOf(
    [
      statement("s1", { text: "The first statement holds. And more." }),
      statement("s2"),
      statement("s3"),
      decisionNode("wide"),
    ],
    [
      decision("wide", {
        links: [
          shaped(specRef("s1")),
          shaped(specRef("s2"), true),
          shaped(specRef("s3")),
          // A second block of the decision names the first statement again.
          shaped(specRef("s1")),
        ],
      }),
    ]
  );

  expect(idsOf(parts).review).toEqual([D("wide")]);
  expect(
    parts.review[0]?.shapes.map(({ id, node, path, stale }) => ({
      id,
      line: parentLinkWords(node),
      path,
      stale,
    }))
  ).toEqual([
    // The statement's first line, as R1 cuts a row's, and its place in the conversation.
    {
      id: statement("s1").id,
      line: "The first statement holds.",
      path: `/issues/${KEY}#b-s1`,
      stale: false,
    },
    {
      id: statement("s2").id,
      line: "The page draws s2.",
      path: `/issues/${KEY}#b-s2`,
      stale: true,
    },
    {
      id: statement("s3").id,
      line: "The page draws s3.",
      path: `/issues/${KEY}#b-s3`,
      stale: false,
    },
  ]);
});

// c-decisions: "Each entry links to the node that holds it." An ask's address is its own; a
// decision's is the issue's with the decision's block as its fragment, which opens the outline
// on that node, where the decision's own address opens `decisions.md`. R2: a node's parent is
// one line on its row, since no row of this filter stands under another.
test("each entry carries its address in the conversation and its parent's line", () => {
  const parts = partsOf(
    [
      statement("s1"),
      openAsk("q1", true, {
        children: [decisionNode("under")],
        text: "On a phone, where does the box sit? It is one line today.",
      }),
      decisionNode("top"),
    ],
    [
      decision("under", { links: [shaped(specRef("s1"))] }),
      decision("top", { links: [shaped(specRef("s1"))] }),
    ]
  );

  expect(parts.open_questions[0]).toMatchObject({ path: `/issues/${KEY}/asks/q1` });
  expect(parts.open_questions[0]?.parent).toBeUndefined();
  const under = parts.review.find((entry) => entry.node.id === D("under"));
  expect(under).toMatchObject({
    parent: { id: "q1", type: "ask" },
    path: `/issues/${KEY}#b-under`,
  });
  expect(under?.parent === undefined ? undefined : parentLinkWords(under.parent)).toBe(
    "On a phone, where does the box sit?"
  );
  expect(parts.review.find((entry) => entry.node.id === D("top"))?.parent).toBeUndefined();

  expect(conversationPath({ reference: decisionRef("d1") })).toBe(`/issues/${KEY}#b-d1`);
  expect(conversationPath({})).toBeUndefined();
  expect(conversationPath({ reference: `dispatch://${KEY}` })).toBeUndefined();
});

// R1 and R22: the filter's one line for a parent and for a statement a decision shaped is drawn
// as every one line of a node is (`lineReading`, `drawnLine`): projected from the words of the
// node's first block, so a heading never runs into the paragraph after it; with an ellipsis where
// the cut ends inside a sentence; and a node with no words is named by its kind's word, never by
// its id.
test("a parent's line and a statement's line are cut from the node's first block", () => {
  const words = Array.from({ length: 30 }, (_unused, at) => `word${at + 1}`).join(" ");
  // A question whose reading opens on a heading, then a paragraph.
  const headed: DisplayTree = {
    content: [
      {
        attrs: { level: 2 },
        content: [{ text: "Where the box sits", type: "text" }],
        type: "heading",
      },
      {
        content: [{ text: "On a phone, the box sits under the rows.", type: "text" }],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const parts = partsOf(
    [
      statement("s1", { text: `The page draws ${words} at last.` }),
      statement("s2", { text: "" }),
      openAsk("q1", true, {
        children: [decisionNode("headed")],
        text: "## Where the box sits\n\nOn a phone, the box sits under the rows.",
        tree: headed,
      }),
      openAsk("q2", true, { children: [decisionNode("wordless")], text: "" }),
    ],
    [
      decision("headed", { links: [shaped(specRef("s1")), shaped(specRef("s2"))] }),
      decision("wordless", { links: [shaped(specRef("s1"))] }),
    ]
  );
  const entry = (block: string) => parts.review.find((one) => one.node.id === D(block));

  const parentLine = (block: string) => {
    const parent = entry(block)?.parent;
    return parent === undefined
      ? undefined
      : {
          id: parent.id,
          line: parentLinkWords(parent),
          reference: parent.reference,
          type: parent.type,
        };
  };
  expect(parentLine("headed")).toEqual({
    id: "q1",
    line: "Where the box sits",
    reference: `dispatch://${KEY}/ask/q1`,
    type: "ask",
  });
  expect(parentLine("wordless")).toEqual({
    id: "q2",
    line: NODE_TYPE_NAMES.ask,
    reference: `dispatch://${KEY}/ask/q2`,
    type: "ask",
  });
  expect(entry("headed")?.shapes.map((one) => parentLinkWords(one.node))).toEqual([
    "The page draws word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13…",
    NODE_TYPE_NAMES.requirement,
  ]);
});

// R1, R4: a node drawn under more than one parent is one node, and the filter draws it once, at
// the place it is edited.
test("a decision drawn under two parents is one entry", () => {
  const parts = partsOf(
    [
      statement("s1"),
      openAsk("q1", true, { children: [decisionNode("twice")] }),
      openAsk("q2", true, { children: [decisionNode("twice", { transcluded: true })] }),
    ],
    [decision("twice", { links: [shaped(specRef("s1"))] })]
  );

  expect(idsOf(parts).review).toEqual([D("twice")]);
  expect(parts.review[0]?.parent?.id).toBe("q1");
});

// R20: the filter by words narrows every filter, and a node the address names is kept: the
// model draws the nodes the Decisions filter's own view left, and no other.
test("the parts hold the nodes the filter's view left", () => {
  const children = [
    statement("s1"),
    openAsk("q1"),
    openAsk("q2"),
    decisionNode("one"),
    decisionNode("two"),
  ];
  const made = [
    decision("one", { links: [shaped(specRef("s1"))] }),
    decision("two", { links: [shaped(specRef("s1"))] }),
  ];
  const view = issueTree([openAsk("q2"), decisionNode("two")]);

  expect(idsOf(partsOf(children, made, [], view))).toEqual({
    agents: [],
    named: [],
    open_questions: ["q2"],
    others: [],
    review: [D("two")],
    yours: [],
  });
  // The tie is still read against the whole tree: the view holds no statement at all.
  expect(partsOf(children, made, [], view).review[0]?.shapes).toHaveLength(1);
});

// R13, the tester's F12: the count of what waits on the reader walks the things that wait, in
// the filter's order: each open question whose turn is the reader's, then each decision of the
// review list.
test("what waits is each open question of the reader's, then each decision to review", () => {
  const made = [decision("review", { links: [shaped(specRef("s1"))] })];
  const both = partsOf(
    [statement("s1"), decisionNode("review"), openAsk("q-agent", false), openAsk("q-mine")],
    made
  );
  expect(waitingOrder(both)).toEqual(["q-mine", D("review")]);

  const review = partsOf(
    [statement("s1"), decisionNode("review"), openAsk("q-agent", false)],
    made
  );
  expect(waitingOrder(review)).toEqual([D("review")]);

  expect(waitingOrder(partsOf([openAsk("q-agent", false)]))).toEqual([]);
});

// The tree draws from the reads that have answered: before the decisions read and the asks
// read are in, the model answers with what the tree holds and throws on nothing.
test("the parts are read before every read has answered", () => {
  const parts = decisionsParts({
    asks: undefined,
    decisions: undefined,
    issueKey: KEY,
    nodeIdOf,
    reader: undefined,
    root: issueTree([openAsk("q1"), ask("closed", { state: "declined" }), decisionNode("early")]),
  });

  expect(idsOf(parts)).toEqual({
    agents: [D("early")],
    named: [],
    open_questions: ["q1"],
    // Nobody is known to have closed it yet, so it is no decision of the reader's.
    others: ["closed"],
    review: [],
    yours: [],
  });
  expect(parts.others[0]?.kind === "ask" ? parts.others[0].declined : "none").toBeUndefined();
  expect(parts.agents[0]?.standing).toBeUndefined();
});

// R20: the address outranks the filter (`presets.ts`), so a comment it names is in the
// Decisions filter's view though the filter draws no comment. The parts hold it as an entry of
// its own, with its address and its parent's line, so the filter draws it and the page focuses
// it; the same tree with no address holds no such entry.
test("a comment the address names is an entry of its own", () => {
  const change = node({
    id: "comment-1",
    reference: `dispatch://${KEY}/comment/comment-1`,
    text: "Make the rail narrower.",
    type: "comment",
  });
  const children = [statement("s1"), openAsk("q1"), decisionNode("one", { children: [change] })];
  const made = [decision("one", { links: [shaped(specRef("s1"))] })];
  const view = presetView(issueTree(children), "decisions", "", new Set([change.id])).root;
  const parts = partsOf(children, made, [], view);

  expect(idsOf(parts)).toEqual({
    agents: [],
    named: ["comment-1"],
    open_questions: ["q1"],
    others: [],
    review: [D("one")],
    yours: [],
  });
  expect(parts.named[0]).toMatchObject({
    kind: "named",
    parent: { id: D("one"), type: "decision" },
    path: `/issues/${KEY}/comments/comment-1`,
    shapes: [],
  });
  expect(idsOf(partsOf(children, made)).named).toEqual([]);
});
