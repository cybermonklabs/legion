import { beforeAll, expect, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import type { CheckBlock, DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { decisionNodes } from "../decision-nodes/decision-nodes";
import { issueTree, KEY, node, READER, READER_ACTOR } from "../decisions-filter/decisions-fixtures";
import type { TreeNode } from "../issue-tree";
import { CHIP_LIMIT } from "../node-row-chip";
import { type LineNames, useIssueNames } from "../one-line";
import { presetView } from "../presets";
import { publishingNames } from "../projection-fixture";
import { SENTENCE_TABLE } from "../sentence-table";
import { treeContext } from "../tree-drafts";
import { treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import {
  check,
  checkNode,
  OTHER_COMMIT,
  result,
} from "../verification-filter/verification-fixtures";
import { firstSentenceOf, type SpecGroup, specDecidedBy } from "./spec-decided-by";
import {
  answeredAsk,
  decision,
  decisionNode,
  issueDetails,
  shaped,
  specViewAnswer,
  statement,
  statementRef,
} from "./spec-fixtures";

/**
 * What decided each statement of the Spec filter, read as the body draws it
 * (R5, R17, R35).
 *
 * Each case builds the tree the page would build and hands it to the Spec filter's own view
 * (`presetView`), so the model is read over the rows the filter really draws, with their parent
 * lines, and over the whole tree the checks and the asks sit in. The decisions are the decision
 * nodes of the Spec view read's answer (`decisionNodes`), as the body reads them. The addressing
 * is the page's own (`treeContext`), so a `shaped` tie is resolved here exactly as the tree
 * resolves one.
 */

/** How this issue addresses a node, which is how a tie finds the statement it names. Only the
 *  context's addresses are read here. */
const { nodeIdOf } = treeContext({ issue: issueDetails() });

// A chip is named from the reading the page published for it, which the store projects.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** What a page that published `lines`, the line a chip names each block by, and no other name
 *  names a plain line's references by (`useIssueNames`): a reference with no line reads as its
 *  author wrote it. */
function lineNamesOf(lines: ReadonlyMap<string, string> = new Map()): LineNames {
  return {
    ambiguous: new Set(),
    author: () => "",
    deleted: new Map(),
    edited: new Set(),
    issue: KEY,
    issues: [],
    lines,
    names: new Map(),
  };
}

/** A page that has published no name, so `referenceStandIns` names a reference as its author
 *  wrote it. */
const NO_NAMES = lineNamesOf();

/** The Spec filter's body over `tree`, as the page reads it once the asks read holds its answer,
 *  with `made` as the decisions the Spec view read holds; `lineNames` is what a reference a plain
 *  line cites is named by (`useIssueNames`). */
function bodyOf(
  tree: TreeNode,
  made: readonly TreeAnswerNode[] = [],
  reader: string | undefined = READER,
  checks: readonly CheckBlock[] = [],
  lineNames: LineNames = NO_NAMES
): readonly SpecGroup[] {
  return specDecidedBy({
    asksHeld: true,
    checks,
    decisions: decisionNodes(specViewAnswer(made), { issue: { key: KEY } }),
    lineNames,
    nodeIdOf,
    reader,
    root: tree,
    view: presetView(tree, "spec", "").root,
  }).groups;
}

// R5: the first sentence is word for word, and a sentence as long as the person wrote it: the
// row's 100-character cut is R1's, and no cut of an answer.
test("a first sentence ends at its first sentence end, and holds the whole text where there is none", () => {
  expect(firstSentenceOf("The node types are all. You have to click.")).toBe(
    "The node types are all."
  );
  expect(firstSentenceOf("Does the built page work? Yes.")).toBe("Does the built page work?");
  expect(firstSentenceOf("And I can't even write!  Like, where do I write the text?")).toBe(
    "And I can't even write!"
  );
  expect(firstSentenceOf("No end at all")).toBe("No end at all");
  expect(firstSentenceOf("")).toBe("");
  // A sentence of 140 characters is drawn whole: R5 sets no limit of characters.
  const long = `${"word ".repeat(30)}end.`;
  expect(firstSentenceOf(`${long} And more.`)).toBe(long);
  // Line breaks inside the sentence are spaces, because the line it is drawn on is one line.
  expect(firstSentenceOf("One line\nand the next. Then more.")).toBe("One line and the next.");
});

// R5: the same decision stands once in the column beside the statements it shaped, however many
// of them there are.
test("one decision stands once beside the run of statements it shaped", () => {
  const groups = bodyOf(issueTree([statement("r1"), statement("r2"), decisionNode("d1")]), [
    decision("d1", {
      text: "One read answers every node.",
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      },
    }),
  ]);
  expect(groups).toHaveLength(1);
  expect(groups[0]?.runs).toHaveLength(1);
  expect(groups[0]?.runs[0]?.decision?.id).toBe("decisions-md#b-d1");
  expect(groups[0]?.runs[0]?.statements.map((one) => one.node.id)).toEqual([
    "spec#b-r1",
    "spec#b-r2",
  ]);
});

// A run is the statements one decision shaped, one after another: a statement another decision
// shaped opens a run of its own, and the first decision opens a second run after it.
test("a statement another decision shaped opens a run of its own", () => {
  const groups = bodyOf(
    issueTree([
      statement("r1"),
      statement("r2"),
      statement("r3"),
      decisionNode("d1"),
      decisionNode("d2"),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1")), shaped(statementRef("r3"))],
        },
      }),
      decision("d2", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r2"))],
        },
      }),
    ]
  );
  expect(
    groups[0]?.runs.map((run) => [
      run.decision?.id,
      run.statements.map((one) => one.node.id).join(" "),
    ])
  ).toEqual([
    ["decisions-md#b-d1", "spec#b-r1"],
    ["decisions-md#b-d2", "spec#b-r2"],
    ["decisions-md#b-d1", "spec#b-r3"],
  ]);
});

// R5: a person's decision is drawn in their own answer's first sentence, word for word, with
// the question it answers, who answered and when, and the options they chose.
test("a person's decision quotes their answer's first sentence, with the question, who and when", () => {
  const ask = answeredAsk("ask-1", {
    selected: ["Changes needed"],
    text: "The node types are all, like, completely so you have to click. And I can't even write.",
  });
  const groups = bodyOf(issueTree([ask, statement("r1")]), [
    decision("d1", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  const made = groups[0]?.runs[0]?.decision;
  expect(made?.quote).toBe("The node types are all, like, completely so you have to click.");
  expect(made?.question).toBe("Does the built page work for you?");
  expect(made?.chose).toEqual(["Changes needed"]);
  expect(made?.at).toBe("2026-11-14T09:00:00Z");
  expect(made?.by).toEqual(READER_ACTOR);
  expect(made?.id).toBe("ask-1");
  expect(made?.reference).toBe(`dispatch://${KEY}/ask/ask-1`);
  expect(made?.said).toBe("you");
  expect(made?.type).toBe("ask");
});

// R5: where the person wrote nothing beside their choice, the option they chose is what the
// column quotes, since there are no words of theirs to quote.
test("an answer with no words of its own is quoted by the option the person chose", () => {
  const ask = answeredAsk("ask-1", { selected: ["1024 px"], text: "" });
  const groups = bodyOf(issueTree([ask, statement("r1")]), [
    decision("d1", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision?.quote).toBe("1024 px");
});

// R34: the reader's own words are theirs; another person's are a person's, and the page says so
// rather than telling the reader they said something they did not.
test("another person's answer is not drawn as the reader's own words", () => {
  const ask = answeredAsk("ask-1", {
    by: { id: "bob", kind: "user" },
    text: "Keep it at six lines.",
  });
  const groups = bodyOf(issueTree([ask, statement("r1")]), [
    decision("d1", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision?.said).toBe("person");
});

// R5: the decisions of the Spec view read are its decision nodes (`decisionNodes`): the model's
// decision on each decision block and on each question a person answered. Each is drawn beside
// the statements its `shaped` links name, a person's as their answer to the ask it is, an
// agent's by its own words, with how sure its agent was.
test("the decisions of the Spec view read stand beside the statements they shaped", () => {
  const tree = issueTree([
    answeredAsk("ask-1", { text: "Keep both statements." }),
    statement("r1"),
    statement("r2"),
    decisionNode("d1"),
  ]);
  const groups = bodyOf(tree, [
    decision("d1", {
      attributes: { uncertainty: "high" },
      decision: { links: [shaped(statementRef("r1"))] },
      text: "One read answers every node.",
    }),
    treeNode("ask-1", "ask", KEY, {
      ask: { multiple: false, options: [], urgency: "med" },
      decision: { links: [shaped(statementRef("r2"))] },
      role: "context",
      text: "Does the built page work for you?",
    }),
  ]);
  expect(
    groups
      .flatMap((group) => group.runs)
      .map((run) => [
        run.decision?.id,
        run.decision?.said,
        run.decision?.unsure,
        run.statements.map((one) => one.node.id).join(" "),
      ])
  ).toEqual([
    ["decisions-md#b-d1", "agent", true, "spec#b-r1"],
    ["ask-1", "you", false, "spec#b-r2"],
  ]);
});

// R5: an agent's decision is drawn in its own text, with how sure it was and how it stands with
// the reader. A text drawn with its own links stands in no link, so the cell draws its words
// whole, as stored and as the server read them, and the line under a statement quotes their
// first line.
test("an agent's decision is quoted by its own words, with how sure it was and how it stands", () => {
  const text = "One batched read names every address. The page asks the server once for them all.";
  const tree = { content: [{ content: [{ text, type: "text" }], type: "paragraph" }], type: "doc" };
  const source = { id: "artifact-dec", issue: KEY, kind: "artifact" } as const;
  const made = node({
    at: "2026-11-13T07:00:00Z",
    author: { id: "session-planner", kind: "agent" },
    id: "decisions-md#b-d1",
    reference: `dispatch://${KEY}/artifact/decisions-md#b-d1`,
    source,
    state: "waiting_review",
    text,
    tree,
    type: "decision",
    unsure: true,
    waiting: true,
  });
  const groups = bodyOf(issueTree([statement("r1"), made]), [
    decision("d1", {
      text,
      text_tree: tree,
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
        unsure: true,
        waiting_review: true,
      },
    }),
  ]);
  const decided = groups[0]?.runs[0]?.decision;
  expect(decided).toMatchObject({
    at: "2026-11-13T07:00:00Z",
    by: { id: "session-planner", kind: "agent" },
    id: "decisions-md#b-d1",
    quote: "One batched read names every address.",
    said: "agent",
    state: "waiting_review",
    type: "decision",
    unsure: true,
  });
  expect(decided?.words).toEqual({ markdown: text, source, tree });
});

// INFRA-1196's decision 516: a decision block whose `decided_by` names a person is that person's
// decision, in their own words: the cell says the reader said it where that person is the reader,
// and a person said it otherwise, and it heads the run of a statement an agent's decision shaped
// too, as a person's answer does, the agent's named after it. One whose `decided_by` names a
// session is an agent's, as it always was.
test("a decision a person decided is drawn as that person's words, before an agent's", () => {
  const bob = { id: "bob", kind: "user" as const };
  const tree = issueTree([
    statement("r1"),
    statement("r2"),
    decisionNode("agents"),
    decisionNode("mine", { author: READER_ACTOR }),
    decisionNode("bobs", { author: bob }),
  ]);
  const made = specViewAnswer(
    [
      decision("agents", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
          waiting_review: true,
        },
      }),
      decision("mine", {
        decision: { kind: "agent", links: [shaped(statementRef("r1"))], waiting_review: true },
      }),
      decision("bobs", {
        decision: { kind: "agent", links: [shaped(statementRef("r2"))], waiting_review: true },
      }),
      treeNode(READER, "person", null, { role: "context", text: READER }),
      treeNode(bob.id, "person", null, { role: "context", text: bob.id }),
      treeNode("session-planner", "session", null, { role: "context", text: "planner" }),
    ],
    [
      treeTuple("decided_by", "agents", "session-planner"),
      treeTuple("decided_by", "mine", READER),
      treeTuple("decided_by", "bobs", bob.id),
    ]
  );
  const groups = specDecidedBy({
    asksHeld: true,
    checks: [],
    decisions: decisionNodes(made, { issue: { key: KEY } }),
    lineNames: NO_NAMES,
    nodeIdOf,
    reader: READER,
    root: tree,
    view: presetView(tree, "spec", "").root,
  }).groups;

  expect(
    groups.flatMap((group) =>
      group.runs.map((run) => ({
        by: run.decision?.by,
        head: run.decision?.id,
        informed: run.statements.flatMap((one) => one.alsoShapedBy.map((other) => other.id)),
        said: run.decision?.said,
      }))
    )
  ).toEqual([
    {
      by: READER_ACTOR,
      head: "decisions-md#b-mine",
      informed: ["decisions-md#b-agents"],
      said: "you",
    },
    { by: bob, head: "decisions-md#b-bobs", informed: ["decisions-md#b-agents"], said: "person" },
  ]);
});

/** The type of every node of `tree`, its own first, each before its children's. */
function typesIn(tree: DisplayTree | undefined): string[] {
  if (tree === undefined) return [];
  const content = Array.isArray(tree.content) ? (tree.content as DisplayTree[]) : [];
  return [tree.type, ...content.flatMap(typesIn)];
}

// R5, the tester's finding at e55b07e9 on the real stack: the tree read answers a decision
// node's text as its whole stored block, the lines that open and close its `decision` directive
// included, and its reading as that block's, one `decision` node that carries the block's
// attributes. The cell drew that node, its attributes with it ("DECISION shaped: … replaced
// false"). It draws the decision's own words and their reading, as the outline's node does
// (`tree-read/tree-nodes.ts`' `readingOf`), and the because line quotes their first block.
test("an agent's decision the tree read answers as its whole stored block draws its own words, never its block's attributes", () => {
  const heading = {
    attrs: { level: 3 },
    content: [{ text: "Keep it", type: "text" }],
    type: "heading",
  };
  const paragraph = {
    content: [{ text: "Keep the model pure. The page reads it.", type: "text" }],
    type: "paragraph",
  };
  const text =
    `:::decision{#d1 shaped="${statementRef("r1")}" replaced="false"}\n` +
    "### Keep it\n\nKeep the model pure. The page reads it.\n:::";
  const cut = { content: [heading, paragraph], type: "doc" };
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1", { text, tree: cut })]), [
    decision("d1", {
      text,
      text_tree: {
        content: [
          {
            attrs: { id: "d1", replaced: "false", shaped: statementRef("r1") },
            content: [heading, paragraph],
            type: "decision",
          },
        ],
        type: "doc",
      },
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  const decided = groups[0]?.runs[0]?.decision;
  expect(decided?.words, "the cell draws the decision's whole stored block").toEqual({
    markdown: "### Keep it\n\nKeep the model pure. The page reads it.\n",
    tree: cut,
  });
  expect(
    typesIn(decided?.words?.tree),
    "the cell draws the block's `decision` node, its attributes with it"
  ).not.toContain("decision");
  expect(decided?.quote, "the because line quotes the block's own lines").toBe("Keep it");
});

// R5: an agent's decision is quoted on the
// because line by its first line, by R1's one-line rule, and that rule cuts a node's line from
// its reading's first block (`lineWordsRaw`, as the page's every other one-line place reads). A
// decision's reading opens with its own `##` heading, and the whole reading joins that heading
// and the paragraph under it with one space, so a quote cut from it runs the two together into
// a line nobody wrote (seen on a running build at 8caa4357: "Keep every address the old page
// answered Chosen so that no link a person keeps breaks, as R5.").
test("an agent's decision is quoted by its first block's line, never its heading run into its words", () => {
  const text = "## Keep it\n\nKeep the model pure. The page reads it.";
  const tree = {
    content: [
      { attrs: { level: 2 }, content: [{ text: "Keep it", type: "text" }], type: "heading" },
      {
        content: [{ text: "Keep the model pure. The page reads it.", type: "text" }],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1", { text, tree })]), [
    decision("d1", {
      text,
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  const quote = groups[0]?.runs[0]?.decision?.quote;
  expect(quote, "the because line runs the decision's heading into its words").not.toContain(
    "Keep it Keep"
  );
  expect(quote).toBe("Keep it");
});

// R5 with the tester's F20: a reference the quote cites is named in plain words, and naming it
// changes no sentence its author wrote. A statement's name is its own first words, which on a
// spec that numbers its statements open with a label ("R2."), so a sentence end read after the
// naming ends the quote inside the name (seen on a running build at 8caa4357: "... no link a
// person keeps breaks, as R5.", where the author wrote "as dispatch://ACME-1/spec#b-r-home says.").
test("a reference named in an agent's quote ends no sentence at its name's label", () => {
  const cited = statementRef("r2");
  const text = `Keep every address, as ${cited} says. Then more follows.`;
  const groups = bodyOf(
    issueTree([statement("r1"), statement("r2"), decisionNode("d1", { text })]),
    [
      decision("d1", {
        text,
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ],
    READER,
    [],
    lineNamesOf(new Map([[cited, "R2. Every address…"]]))
  );
  expect(groups[0]?.runs[0]?.decision?.quote).toBe(
    "Keep every address, as R2. Every address… says."
  );
});

// R5 and R8: a reference the page published no name for - another issue's comment it has not
// answered for, or answered is gone - is quoted as the words its author wrote, as the cell's own
// words draw it, and never as its short form ("OTHER-1 comment").
test("an agent's quote draws a reference the page published no name for as written, never as its short form", () => {
  const gone = "dispatch://OTHER-1/comment/0f8e2c3a-6b1d-4e5f-9a7c-2d3e4f5a6b7c";
  const text = `Keep it as ${gone} says. Then more follows.`;
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1", { text })]), [
    decision("d1", { text, decision: { kind: "agent", links: [shaped(statementRef("r1"))] } }),
  ]);
  const quote = groups[0]?.runs[0]?.decision?.quote;
  expect(quote).toBe(`Keep it as ${gone} says.`);
  expect(quote).not.toContain("OTHER-1 comment");
});

// R5: every decision after the first that shaped a statement is named after "informed by" on
// its line by its own words, cut as a chip names its target, each reference in them named after
// the cut and never an address.
test("a statement informed by two decisions names the second by its words within 32 characters", () => {
  const cited = statementRef("r2");
  const words = `Name each tie by ${cited} and never by its address. Then say why.`;
  const groups = bodyOf(
    issueTree([
      statement("r1"),
      statement("r2"),
      decisionNode("d1"),
      decisionNode("d2", { text: words }),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
      decision("d2", {
        text: words,
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ],
    READER,
    [],
    lineNamesOf(new Map([[cited, "the second statement"]]))
  );
  const informed = groups[0]?.runs[0]?.statements[0]?.alsoShapedBy ?? [];
  expect(informed.map((one) => one.id)).toEqual(["decisions-md#b-d2"]);
  expect(informed[0]?.quote).toBe(
    "Name each tie by the second statement and never by its address."
  );
  // The name does not fit in a chip's 32 characters. Each word of it is a token of its own, so
  // the cut falls at the last space inside the chip's room, between two of the name's words, and
  // never inside one: the line is cut with the reference unnamed, and named after the cut.
  const named = informed[0]?.chip ?? "";
  expect(named).toBe("Name each tie by the second…");
  expect(named.length).toBeLessThanOrEqual(CHIP_LIMIT);
});

// R5: the name a statement's line gives another decision is cut at a chip's length before the
// references it cites are named, as every one-line place cuts one, so no cut falls inside a word
// of a name: a first line whose name runs past the limit is cut at a word inside that name.
test("the name of a decision that informed a statement is cut before its references are named", () => {
  const cited = statementRef("r2");
  const words = `See ${cited} as the reader reads it.`;
  const groups = bodyOf(
    issueTree([
      statement("r1"),
      statement("r2"),
      decisionNode("d1"),
      decisionNode("d2", { text: words }),
    ]),
    [
      decision("d1", { decision: { kind: "agent", links: [shaped(statementRef("r1"))] } }),
      decision("d2", {
        decision: { kind: "agent", links: [shaped(statementRef("r1"))] },
        text: words,
      }),
    ],
    READER,
    [],
    lineNamesOf(new Map([[cited, "R2. Every address is drawn by…"]]))
  );
  const informed = groups[0]?.runs[0]?.statements[0]?.alsoShapedBy[0];
  expect(informed?.quote).toBe("See R2. Every address is drawn by… as the reader reads it.");
  expect(informed?.chip).toBe("See R2. Every address is drawn…");
});

// R5: a replaced decision draws no line, so the statement it shaped stands with none.
test("a replaced decision shapes nothing", () => {
  const groups = bodyOf(issueTree([statement("r1")]), [
    decision("d1", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
        replaced: true,
      },
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision).toBeUndefined();
});

// R5: a statement's decisions are its ties in order, the first heads its run, and each other
// one is named after "informed by". While the asks read is out, a person's decision is drawn
// as nothing, but it is still the first of the statements it shaped. Found by the tester on a
// running build at d9344476, with the asks read refused on the seeded issue: the person's
// decision shaped three statements and an agent's decision informed the second and the third,
// and the view drew those two each in a run of its own, headed "Because an agent decided".
test("a person's decision that waits on the asks read still heads its run, and no decision that only informed a statement takes its place", () => {
  const tree = issueTree([
    statement("r1"),
    statement("r2"),
    decisionNode("d1"),
    decisionNode("d2"),
  ]);
  const { groups } = specDecidedBy({
    asksHeld: false,
    checks: [],
    decisions: decisionNodes(
      specViewAnswer([
        decision("d1", {
          decision: {
            asks: ["ask-1"],
            kind: "person",
            links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
          },
        }),
        decision("d2", {
          decision: {
            kind: "agent",
            links: [shaped(statementRef("r2"))],
          },
        }),
      ]),
      { issue: { key: KEY } }
    ),
    lineNames: NO_NAMES,
    nodeIdOf,
    reader: READER,
    root: tree,
    view: presetView(tree, "spec", "").root,
  });
  const runs = groups.flatMap((group) => group.runs);
  const runOf = (block: string) =>
    runs.find((run) => run.statements.some((one) => one.node.id === `spec#b-${block}`));
  expect(
    runOf("r2")?.decision?.id,
    "a decision that only informed statement r2 heads its run while the person's decision waits"
  ).not.toBe("decisions-md#b-d2");
  expect(
    runOf("r2"),
    "statement r2 leaves the run of the person's decision that shaped it and r1"
  ).toBe(runOf("r1"));
});

// R5: a statement that hangs under an answered question carries that answer the same way, with
// no tuple, since that decision is its parent.
test("a statement under an answered question carries that answer with no tie", () => {
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Put the memo in the pane. Not above the outline." }, [
        statement("r1"),
      ]),
    ])
  );
  expect(groups[0]?.parent).toMatchObject({
    id: "ask-1",
    line: "Does the built page work for you?",
    type: "ask",
  });
  expect(groups[0]?.runs[0]?.decision).toMatchObject({
    id: "ask-1",
    quote: "Put the memo in the pane.",
    said: "you",
    type: "ask",
  });
});

// R35: what makes a missing tie visible is the Spec filter, where a statement with no decision
// beside it is drawn as such.
test("a statement no decision shaped stands in a run with no decision", () => {
  const groups = bodyOf(issueTree([statement("r1")]));
  expect(groups[0]?.runs).toHaveLength(1);
  expect(groups[0]?.runs[0]?.decision).toBeUndefined();
  expect(groups[0]?.runs[0]?.statements).toHaveLength(1);
});

// R5 draws "the decisions that informed it": the first heads the run, and every other one is
// named on the statement's own line.
test("every other decision that shaped a statement is named after the first", () => {
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1"), decisionNode("d2")]), [
    decision("d1", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
    decision("d2", {
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision?.id).toBe("decisions-md#b-d1");
  expect(groups[0]?.runs[0]?.statements[0]?.alsoShapedBy.map((one) => one.id)).toEqual([
    "decisions-md#b-d2",
  ]);
});

// R5: a person's decision stands beside every statement it shaped. The Spec view read answers an
// answered ask as context after the spec's decision blocks, so an agent's decision that shaped
// the same statement comes first in it, and is named after the person's all the same.
test("a person's decision heads every statement it shaped, before an agent's the read answers first", () => {
  const ask = answeredAsk("ask-1", { text: "Keep every address the old page answered." });
  const groups = bodyOf(issueTree([ask, statement("r1"), statement("r2"), decisionNode("d1")]), [
    decision("d1", { decision: { kind: "agent", links: [shaped(statementRef("r2"))] } }),
    decision("d2", {
      decision: {
        asks: ["ask-1"],
        kind: "person",
        links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      },
    }),
  ]);
  const runs = groups[0]?.runs ?? [];
  expect(runs.map((run) => [run.decision?.id, run.statements.length])).toEqual([["ask-1", 2]]);
  expect(runs[0]?.statements.map((one) => one.alsoShapedBy.map((by) => by.id))).toEqual([
    [],
    ["decisions-md#b-d1"],
  ]);
});

// R5: "The line is drawn whether or not" other decisions apply: the answered question a
// statement hangs under heads its run, and a decision whose tie names the statement is named
// after it, not in its place.
test("a statement under an answered question keeps that answer when a tie names it too", () => {
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Put the memo in the pane." }, [statement("r1")]),
      decisionNode("d1"),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [shaped(statementRef("r1"))],
        },
      }),
    ]
  );
  expect(groups[0]?.runs[0]?.decision).toMatchObject({ id: "ask-1", type: "ask" });
  expect(groups[0]?.runs[0]?.statements[0]?.alsoShapedBy.map((one) => one.id)).toEqual([
    "decisions-md#b-d1",
  ]);
});

// R5: the focus on the line draws the whole answer word for word, so the model carries it
// beside the first sentence, and carries none where the first sentence is all the person wrote.
test("a person's decision carries their whole answer where it says more than its first sentence", () => {
  const long = "Put the memo in the pane.\n\n  Not above the outline -  ever.";
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: long }, [statement("r1")]),
      answeredAsk("ask-2", { text: "Keep it short." }, [statement("r2")]),
    ])
  );
  expect(groups[0]?.runs[0]?.decision?.whole).toBe(long);
  expect(groups[1]?.runs[0]?.decision?.whole).toBeUndefined();
});

// R5: the focus draws the whole answer character for character. A quote names each reference in
// words, so an answer of one sentence that cites a
// block is not what its quote draws, and the stored text is carried beside it for the focus.
test("a person's answer that cites a block carries its whole text for the focus, though it is one sentence", () => {
  const cited = statementRef("r2");
  const stored = `Keep it as ${cited} says.`;
  const tree = issueTree([
    answeredAsk("ask-1", { text: stored }, [statement("r1")]),
    // The tree names the block by a chip of the batched read (`tree-drafts.ts`), which is the
    // name the page gives a reference a plain line cites.
    decisionNode("d1", {
      chips: [{ answered: true, gone: false, reading: { words: "the one-line rule" }, ref: cited }],
    }),
  ]);
  // The page publishes the chip's name to the names store, which `useIssueNames` reads.
  const names = renderHook(() => useIssueNames(KEY), {
    wrapper: publishingNames(new Map([[cited, "the one-line rule"]])),
  });
  try {
    const made = bodyOf(tree, [], READER, [], names.result.current)[0]?.runs[0]?.decision;
    expect(made?.quote).toBe("Keep it as the one-line rule says.");
    expect(made?.whole).toBe(stored);
  } finally {
    names.unmount();
  }
});

// R9: no agent's words are drawn under a person's name. A section the server marks a person's
// whose answered question this tree does not hold is the agent's record of that decision, so
// it is drawn as an agent's words and never quoted as the person's.
test("a person's decision whose question the tree does not hold is drawn as the agent's record", () => {
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1")]), [
    decision("d1", {
      decision: {
        asks: ["an-ask-of-another-issue"],
        kind: "person",
        links: [shaped(statementRef("r1"))],
      },
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision).toMatchObject({ said: "agent", type: "decision" });
});

// R5, R9 and INFRA-1253 R8: Conversation's read is paged, so the page's tree may not have reached
// the question a person answered. The Spec view read holds that answered question as context of
// the statement its `shaped` tie names, and the tree it builds (`TreeViews.trees.spec`) holds its
// node: the person's decision is drawn by their own answer, never as the agent's record of it.
test("a person's decision whose question only the Spec view read's tree holds is drawn by their answer", () => {
  const tree = issueTree([statement("r1"), decisionNode("d1")]);
  const groups = specDecidedBy({
    asksHeld: true,
    checks: [],
    decisions: decisionNodes(
      specViewAnswer([
        decision("d1", {
          decision: {
            asks: ["ask-1"],
            kind: "person",
            links: [shaped(statementRef("r1"))],
          },
        }),
      ]),
      { issue: { key: KEY } }
    ),
    lineNames: NO_NAMES,
    nodeIdOf,
    reader: READER,
    root: tree,
    view: presetView(tree, "spec", "").root,
    viewTree: issueTree([
      statement("r1"),
      answeredAsk("ask-1", { text: "Draw the rail once. Then stop." }),
    ]),
  }).groups;
  expect(groups[0]?.runs[0]?.decision).toMatchObject({
    id: "ask-1",
    quote: "Draw the rail once.",
    said: "you",
    type: "ask",
  });
});

// R17: the mark is how the statement stands in the Verification filter's own reading, so one
// statement reads the same in both views: Met where every check of it passed, Not met where one
// failed, Unknown otherwise, and no check where none serves it. Each check is read at the commit
// the server names for it, so a pass the server reads at
// another commit (`other_commit`, with no pass standing) is no Met.
test("a statement's mark is how its checks stand at the commit the server names for each", () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const failed = check({
    block_id: "c2",
    pass_stands: { answer: "no" },
    results: [result({ created_at: "2026-11-14T01:10:00Z", id: "f1", verdict: "fail" })],
  });
  const elsewhere = check({
    block_id: "c3",
    pass_stands: { answer: "no" },
    results: [
      result({
        commit_sha: OTHER_COMMIT,
        commit_stands: { answer: "other_commit" },
        created_at: "2026-11-13T00:00:00Z",
        id: "o1",
      }),
    ],
  });
  const unrun = check({ block_id: "c4", pass_stands: { answer: "no" }, results: [] });
  const groups = bodyOf(
    issueTree([
      statement("r1", { children: [checkNode(passed)] }),
      statement("r2", { children: [checkNode(passed), checkNode(failed)] }),
      statement("r3", { children: [checkNode(elsewhere)] }),
      statement("r4", { children: [checkNode(unrun)] }),
      statement("r5", { counts: { checks: 0, planTasks: 0 }, gaps: ["no_check"] }),
      // A check of a child issue serves it: the coverage read counts it and this issue's tree
      // holds no node for it, so it is covered and the page cannot say how it stands.
      statement("r6", { counts: { checks: 1, planTasks: 0 } }),
    ]),
    [],
    READER,
    [passed, failed, elsewhere, unrun]
  );
  expect(groups[0]?.runs[0]?.statements.map((one) => [one.node.id, one.mark])).toEqual([
    ["spec#b-r1", "met"],
    ["spec#b-r2", "not_met"],
    ["spec#b-r3", "unknown"],
    ["spec#b-r4", "unknown"],
    ["spec#b-r5", "no_check"],
    ["spec#b-r6", "unknown"],
  ]);
});

// R20: a node an address names outranks the filter, whatever its kind. It is no statement: it
// enters no run and carries no mark, and the body draws it by its kind and its words.
test("a row the filter kept that is no statement is in no run", () => {
  const tree = issueTree([
    statement("r1"),
    node({ id: "comment-1", text: "A remark the address names.", type: "comment" }),
  ]);
  const read = specDecidedBy({
    asksHeld: true,
    checks: [],
    decisions: [],
    lineNames: NO_NAMES,
    nodeIdOf,
    reader: READER,
    root: tree,
    view: presetView(tree, "spec", "", new Set(["comment-1"])).root,
  });
  expect(
    read.groups.flatMap((group) =>
      group.runs.flatMap((run) => run.statements.map((one) => one.node.id))
    )
  ).toEqual(["spec#b-r1"]);
  expect(read.others.map((one) => one.id)).toEqual(["comment-1"]);
});

// R2: a statement's parent is no row of the Spec filter, so it heads the group its statements
// stand in, with who made it and when.
test("statements under different parents stand in groups of their own", () => {
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Keep the memo in the pane." }, [statement("r1")]),
      answeredAsk("ask-2", { text: "Draw the decisions beside the statements." }, [
        statement("r2"),
        statement("r3"),
      ]),
      statement("r4"),
    ])
  );
  expect(
    groups.map((group) => [
      group.parent?.id,
      group.runs.flatMap((run) => run.statements.map((one) => one.node.id)).join(" "),
    ])
  ).toEqual([
    ["ask-1", "spec#b-r1"],
    ["ask-2", "spec#b-r2 spec#b-r3"],
    [undefined, "spec#b-r4"],
  ]);
  expect(groups[0]?.parent).toMatchObject({
    at: "2026-11-14T08:00:00Z",
    by: { id: "session-architect", kind: "agent" },
  });
});

// R4, review finding 4590606145: a group's heading is its parent's first line drawn by R1's
// rule, so a line cut inside a sentence ends with an ellipsis, as every other first line of the
// page does.
test("a group's heading cut inside its parent's first sentence ends with an ellipsis", () => {
  const words = `This comment ${"runs on and on ".repeat(8)}until it ends.`;
  const groups = bodyOf(
    issueTree([
      node({ children: [statement("r1")], id: "comment-1", text: words, type: "comment" }),
    ])
  );
  const line = groups[0]?.parent?.line ?? "";
  expect(line.endsWith("…")).toBe(true);
  expect(line.length).toBeGreaterThan(1);
  expect(words.startsWith(line.slice(0, -1))).toBe(true);
});

// R5, review finding 4590606153: a person's quote is their words, read from the stored markdown
// to plain words before its first sentence is taken - a link as its words, a mark as nothing -
// and never the syntax they were stored in; the focus still draws the answer as stored.
test("a person's quote is the first sentence of their answer's plain words", () => {
  const stored = "Use [the one-line rule](dispatch://CORE-1/spec#b-r1). **Always** so.";
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: stored }, [statement("r1")]),
      answeredAsk("ask-2", { text: "Keep `snake_case` *as is*. Then more." }, [statement("r2")]),
    ])
  );
  expect(groups[0]?.runs[0]?.decision?.quote).toBe("Use the one-line rule.");
  expect(groups[0]?.runs[0]?.decision?.whole).toBe(stored);
  expect(groups[1]?.runs[0]?.decision?.quote).toBe("Keep snake_case as is.");
});

// R5, of the tester's finding 1's kind, found by the implementer in test round 2: a person's quote
// draws a private-use character they wrote of their own - an icon from an icon font or a
// terminal - as they wrote it, and never as the punctuation that a code span's characters are
// held as while the marks are taken off.
test("a private-use character in a person's answer is quoted as written, beside a code span", () => {
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Keep \ue02a and \ue05f, and `a*b` too." }, [statement("r1")]),
    ])
  );
  expect(groups[0]?.runs[0]?.decision?.quote).toBe("Keep \ue02a and \ue05f, and a*b too.");
});

// R5, review finding 4590606160: a leading label is no sentence end, so an answer that opens with
// a numbered label is quoted with the sentence after it, never as the label alone.
test("a first sentence that opens with a numbered label holds the sentence after it", () => {
  expect(firstSentenceOf("1. Keep the old page. 2. Then drop it.")).toBe("1. Keep the old page.");
  expect(firstSentenceOf("R1. stands as written.")).toBe("R1. stands as written.");
  // A first word with no digit, or longer than five characters, is a sentence of its own.
  expect(firstSentenceOf("Done. Then more.")).toBe("Done.");
  expect(firstSentenceOf("R12345. Then more.")).toBe("R12345.");
});

// R5: a person's first sentence ends where the table of sentence ends (`SENTENCE_TABLE`) ends
// each text's, read by the one function every reader splits words by, over the text as written.
// A row whose first sentence depends on the blocks its text is stored as gives the function no
// answer to meet, and is read by the quote alone.
test("a first sentence ends where every row of the sentence table ends it", () => {
  for (const row of SENTENCE_TABLE) {
    if (row.first === undefined) continue;
    expect(firstSentenceOf(row.text), row.id).toBe(row.first);
  }
});

// R5: a person's quote is the first sentence of the answer they stored, as the table states it
// for a stored text, in a decision tied to one statement: the quote reads the answer's first
// block alone, and an answer that opens with a list is quoted from its first item's own words,
// whose marker markdown reads as no word of it.
test("a person's quote of each row of the sentence table is the row's stored first sentence", () => {
  for (const row of SENTENCE_TABLE) {
    const groups = bodyOf(issueTree([answeredAsk("ask-1", { text: row.text }), statement("r1")]), [
      decision("d1", {
        decision: { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] },
      }),
    ]);
    expect(groups[0]?.runs[0]?.decision?.quote, row.id).toBe(row.stored);
  }
});

// R5: a person's quote is their first sentence, word for word, so an abbreviation followed by a
// number ends none of it. Review 5939401884's finding D measured "Use the old page (see p. 4)
// for now." quoted as "Use the old page (see p."; on a running build at e28c9092 an answer
// "Keep the memo, e.g. in the pane, and draw it whole (see p. 4). Then more words follow." was
// drawn in its cell and its because line as "Keep the memo, e.g. in the pane, and draw it whole
// (see p.".
test("a first sentence runs past an abbreviation that a number follows", () => {
  expect(firstSentenceOf("Use the old page (see p. 4) for now. Then more.")).toBe(
    "Use the old page (see p. 4) for now."
  );
  expect(
    firstSentenceOf("Keep the memo, e.g. in the pane, and draw it whole (see p. 4). Then more.")
  ).toBe("Keep the memo, e.g. in the pane, and draw it whole (see p. 4).");
});

// R5: a full stop before a number ends no sentence only after a listed abbreviation, and a number
// after one opens no label, so "p. 1" and "fig. 1" stay inside their sentence. After any other
// word the full stop ends the sentence, and a "1." after it is the next sentence's label, as a
// "1." that opens the text is the first's. At ba0574f3 the rule cut "Draw it whole, as on p. 1.
// Then more words follow." to "Draw it whole, as on p.", which a running build quoted so in a
// person's because line, and "See fig. 1. It shows the cut." to "See fig.".
test("a first sentence runs past an abbreviation that 1 follows, and a list of two items starts at its 1", () => {
  expect(firstSentenceOf("Draw it whole, as on p. 1. Then more words follow.")).toBe(
    "Draw it whole, as on p. 1."
  );
  expect(firstSentenceOf("See fig. 1. It shows the cut.")).toBe("See fig. 1.");
  expect(firstSentenceOf("Two steps follow. 1. Open it. 2. Close it.")).toBe("Two steps follow.");
  expect(firstSentenceOf("1. Keep the old page. 2. Then drop it.")).toBe("1. Keep the old page.");
});

/** The page's name for the statement r2, which opens with its label ("R2."). */
const LABELLED = lineNamesOf(new Map([[statementRef("r2"), "R2. Every address…"]]));

// R5 with the tester's F20: a person's quote is the first sentence they wrote, cut with each
// reference in it still unnamed, so a name that opens with its label ends no sentence of theirs.
test("a person's quote that cites a labelled statement ends where their sentence ends", () => {
  const stored = `Keep it as ${statementRef("r2")} says. Then more.`;
  const groups = bodyOf(
    issueTree([answeredAsk("ask-1", { text: stored }, [statement("r1")])]),
    [],
    READER,
    [],
    LABELLED
  );
  expect(groups[0]?.runs[0]?.decision?.quote).toBe("Keep it as R2. Every address… says.");
});

// R5: an answer the person wrote as a numbered list on one line, "1. Keep the old page. 2. Then
// drop it.", ends its first sentence at "page.": markdown reads its "1." as a list's marker, which
// is no word, and a full stop before a number ends the sentence wherever no listed abbreviation
// stands before it. On a running build at b0a0892f that answer was quoted "Keep the old page.
// 2.": the quote was cut from the answer's plain words, where the list's "1." is no longer a
// word, so the "2." after the first item read as no label.
test("a person's answer written as a numbered list on one line is quoted to its first item", () => {
  const stored = "1. Keep the old page. 2. Then drop it.";
  const groups = bodyOf(issueTree([answeredAsk("ask-1", { text: stored }, [statement("r1")])]));
  const quote = groups[0]?.runs[0]?.decision?.quote ?? "";
  expect(quote, "the quote of a numbered answer ends at its first item's full stop").toMatch(
    /Keep the old page\.$/
  );
});

// R5: a person's answer written as a list is quoted from its first item alone, never from the
// whole list run together, though the item ends at no full stop: "- keep it" over "- drop it" is
// quoted "keep it".
test("a person's answer written as a bulleted list is quoted by its first item alone", () => {
  const groups = bodyOf(
    issueTree([answeredAsk("ask-1", { text: "- keep it\n- drop it" }), statement("r1")]),
    [
      decision("d1", {
        decision: { asks: ["ask-1"], kind: "person", links: [shaped(statementRef("r1"))] },
      }),
    ]
  );
  expect(groups[0]?.runs[0]?.decision?.quote).toBe("keep it");
});

// R1 and R5: the question is given by its first sentence, cut from its first block's words
// (`lineWordsRaw`) with each reference unnamed: a second block never runs into it, and a name's
// label ends none of it.
test("a question line is its first block's line, cut before the reference in it is named", () => {
  const ask = {
    ...answeredAsk("ask-1", { text: "Keep it." }, [statement("r1")]),
    text: `Does ${statementRef("r2")} keep its address\n\nThe old page kept it. Then more.`,
  };
  const groups = bodyOf(issueTree([ask]), [], READER, [], LABELLED);
  expect(groups[0]?.runs[0]?.decision?.question).toBe("Does R2. Every address… keep its address");
});

// R5: the question an answer answers is given by its first sentence whole, ending at its first
// sentence end as the answer's quote does, with no limit of characters: R1's 100-character cut
// is a row's first line, and no cut of the question the cell and the because line give, also
// where the statement hangs under that question.
test("a question is given by its first sentence whole, past 100 characters", () => {
  const first =
    "Should the Spec view draw every statement of the spec whole, with each of its words, rather than cut it at six lines?";
  expect(first.length).toBeGreaterThan(100);
  const text = `${first} Then a second sentence follows it.`;
  const groups = bodyOf(
    issueTree([
      { ...answeredAsk("ask-1", { text: "Keep it whole." }, [statement("r1")]), text },
      { ...answeredAsk("ask-2", { text: "Keep it whole." }), text },
      statement("r2"),
    ]),
    [
      decision("d1", {
        decision: { asks: ["ask-2"], kind: "person", links: [shaped(statementRef("r2"))] },
      }),
    ]
  );
  expect(groups.map((group) => group.runs[0]?.decision?.question)).toEqual([first, first]);
});

// R5: where a statement's words changed after the tie that names the decision its run is headed
// by, it carries Changed since: the tie's own `changed_since_tie`, which the server reads. The
// tie's far end's `changed_at` decides nothing: the read answers it null on a decision's links,
// and a time there later than the tie's `created_at` makes no change of a tie the server says is
// unchanged.
// A decision that names a statement in two of its blocks holds two ties to it, and the statement
// changed since that decision only where it changed after both. Only the head's tie decides it,
// never that of a decision that only informed the statement, and the answered question a
// statement hangs under has no tie, so a run it heads never says it.
test("a statement changed after the tie of the decision heading its run, and only that tie, carries Changed since", () => {
  const tie = (block: string, changed: boolean, changedAt: string | null = null) => {
    const made = shaped(statementRef(block));
    return {
      ...made,
      changed_since_tie: changed,
      created_at: "2026-11-14T10:00:00Z",
      to: { ...made.to, changed_at: changedAt },
    };
  };
  const after = "2026-11-14T11:00:00Z";
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Keep it whole." }, [statement("r6")]),
      ...["r1", "r2", "r3", "r4", "r5", "r7"].map((block) => statement(block)),
      ...["d1", "d2", "d3", "d4"].map((block) => decisionNode(block)),
    ]),
    [
      decision("d1", {
        decision: {
          kind: "agent",
          links: [
            tie("r1", true),
            tie("r2", false),
            // A far end's time after the tie's, of a tie the server says is unchanged.
            tie("r3", false, after),
            tie("r4", false),
            // Named in two of its blocks: changed after the first tie and not after the second.
            tie("r5", true),
            { ...tie("r5", false), from_block: "d1-again" },
            tie("r7", false),
          ],
        },
      }),
      // A decision that only informs r2, which changed after this decision's tie to it.
      decision("d2", { decision: { kind: "agent", links: [tie("r2", true)] } }),
      // An agent's decision that only informs r6, which changed after its tie, under the
      // answered question that heads r6's run.
      decision("d3", { decision: { kind: "agent", links: [tie("r6", true)] } }),
      // The person's own decision for the question r6 hangs under, changed after its tie.
      decision("d4", {
        decision: { asks: ["ask-1"], kind: "person", links: [tie("r6", true)] },
      }),
    ]
  );
  const changed = Object.fromEntries(
    groups.flatMap((group) =>
      group.runs.flatMap((run) =>
        run.statements.map((one) => [one.node.id.replace("spec#b-", ""), one.changedSince])
      )
    )
  );
  expect(changed).toEqual({
    r1: true,
    r2: false,
    r3: false,
    r4: false,
    r5: false,
    r6: false,
    r7: false,
  });
});

// Review thread 4592007145: `presets.ts` cuts a parent's line by R1's rule with each reference
// written as its short form (`lineWords`), so the heading is read from the parent node, where
// each reference is named by the page's name after the cut and counted as the name it draws. 68
// characters, the name's 18 and
// " says." are 92: the first sentence is the heading. With the address's 27 it would be 101,
// which cuts the line inside the sentence; named first, the label ends the sentence at "R2.".
test("a group's heading counts a reference in its parent's line as its name, which ends no sentence", () => {
  const before = "The heading keeps every word its author wrote whole on one line, as ";
  const groups = bodyOf(
    issueTree([
      node({
        children: [statement("r1")],
        id: "comment-1",
        text: `${before}${statementRef("r2")} says. Then more.`,
        type: "comment",
      }),
    ]),
    [],
    READER,
    [],
    LABELLED
  );
  expect(groups[0]?.parent?.line).toBe(`${before}R2. Every address… says.`);
});

// R5 and R1: an agent's decision the tree holds no node for is quoted from its own reading, else
// its stored text, by the first block that holds any words, as `lineWordsRaw` reads a node's: its
// `##` heading never runs into the paragraph under it, and an image or a rule is passed over.
test("an agent's decision with no node is quoted by the first block of its own words", () => {
  const text = "## Keep it\n\nKeep the model pure.";
  const tree = {
    content: [
      { attrs: { level: 2 }, content: [{ text: "Read once", type: "text" }], type: "heading" },
      { content: [{ text: "Keep the model pure.", type: "text" }], type: "paragraph" },
    ],
    type: "doc",
  };
  const groups = bodyOf(issueTree([statement("r1"), statement("r2"), statement("r3")]), [
    decision("d7", {
      text,
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r1"))],
      },
    }),
    decision("d8", {
      text,
      text_tree: tree,
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r2"))],
      },
    }),
    decision("d9", {
      text: "![The tree](tree.png)\n\n---\n\nKeep the model pure. The page reads it.",
      decision: {
        kind: "agent",
        links: [shaped(statementRef("r3"))],
      },
    }),
  ]);
  expect(groups[0]?.runs.map((run) => [run.decision?.node, run.decision?.quote])).toEqual([
    [undefined, "Keep it"],
    [undefined, "Read once"],
    [undefined, "Keep the model pure."],
  ]);
});
