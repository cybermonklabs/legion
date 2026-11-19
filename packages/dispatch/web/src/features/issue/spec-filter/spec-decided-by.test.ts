import { expect, test } from "bun:test";

import type { CheckBlock, Decision } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { presetView } from "../presets";
import { treeContext } from "../tree-drafts";
import {
  COMMIT,
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
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  node,
  READER,
  READER_ACTOR,
  shaped,
  statement,
  statementRef,
} from "./spec-fixtures";

/**
 * What decided each statement of the Spec filter, read as the body draws it
 * (dispatch://INFRA-1196 R5, R17, R35).
 *
 * Each case builds the tree the page would build and hands it to the Spec filter's own view
 * (`presetView`), so the model is read over the rows the filter really draws, with their parent
 * lines, and over the whole tree the checks and the asks sit in. The addressing is the page's
 * own (`treeContext`), so a `shaped` tie is resolved here exactly as the tree resolves one.
 */

/** How this issue addresses a node, which is how a tie finds the statement it names. */
const { nodeIdOf } = treeContext({ issue: issueDetails() });

/** The Spec filter's body over `tree`, as the page reads it. */
function bodyOf(
  tree: TreeNode,
  made: readonly Decision[] = [],
  reader: string | undefined = READER,
  checks: readonly CheckBlock[] = []
): readonly SpecGroup[] {
  return specDecidedBy({
    checks,
    decisions: decisionsRead(made),
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
    decision({
      block_id: "d1",
      kind: "agent",
      links: [shaped(statementRef("r1")), shaped(statementRef("r2"))],
      text: "One read answers every node.",
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
      decision({
        block_id: "d1",
        kind: "agent",
        links: [shaped(statementRef("r1")), shaped(statementRef("r3"))],
      }),
      decision({ block_id: "d2", kind: "agent", links: [shaped(statementRef("r2"))] }),
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
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
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
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
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
    decision({
      asks: ["ask-1"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision?.said).toBe("person");
});

// R5: an agent's decision is drawn in its own text, with how sure it was and how it stands with
// the reader.
test("an agent's decision is quoted by its own words, with how sure it was and how it stands", () => {
  const made = node({
    at: "2026-11-13T07:00:00Z",
    author: { id: "session-planner", kind: "agent" },
    id: "decisions-md#b-d1",
    reference: `dispatch://${KEY}/artifact/decisions-md#b-d1`,
    state: "waiting_review",
    text: "One batched read names every address.",
    type: "decision",
    unsure: true,
    waiting: true,
  });
  const groups = bodyOf(issueTree([statement("r1"), made]), [
    decision({
      block_id: "d1",
      kind: "agent",
      links: [shaped(statementRef("r1"))],
      text: "One batched read names every address.",
      unsure: true,
      waiting_review: true,
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision).toMatchObject({
    at: "2026-11-13T07:00:00Z",
    by: { id: "session-planner", kind: "agent" },
    id: "decisions-md#b-d1",
    quote: "One batched read names every address.",
    said: "agent",
    state: "waiting_review",
    type: "decision",
    unsure: true,
  });
});

// R5: a replaced decision draws no line, so the statement it shaped stands with none.
test("a replaced decision shapes nothing", () => {
  const groups = bodyOf(issueTree([statement("r1")]), [
    decision({
      block_id: "d1",
      kind: "agent",
      links: [shaped(statementRef("r1"))],
      replaced: true,
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision).toBeUndefined();
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
    decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] }),
    decision({ block_id: "d2", kind: "agent", links: [shaped(statementRef("r1"))] }),
  ]);
  expect(groups[0]?.runs[0]?.decision?.id).toBe("decisions-md#b-d1");
  expect(groups[0]?.runs[0]?.statements[0]?.alsoShapedBy.map((one) => one.decision.id)).toEqual([
    "decisions-md#b-d2",
  ]);
});

// R5: "The line is drawn whether or not" other decisions apply: the answered question a
// statement hangs under heads its run, so its parent line and its decision line stay one, and a
// decision whose tie names the statement is named after it, not in its place.
test("a statement under an answered question keeps that answer when a tie names it too", () => {
  const groups = bodyOf(
    issueTree([
      answeredAsk("ask-1", { text: "Put the memo in the pane." }, [statement("r1")]),
      decisionNode("d1"),
    ]),
    [decision({ block_id: "d1", kind: "agent", links: [shaped(statementRef("r1"))] })]
  );
  expect(groups[0]?.runs[0]?.decision).toMatchObject({ id: "ask-1", type: "ask" });
  expect(groups[0]?.runs[0]?.statements[0]?.alsoShapedBy.map((one) => one.decision.id)).toEqual([
    "decisions-md#b-d1",
  ]);
});

// R5: "Changed since" is read from the tie: its far end carries the statement block's own last
// change, and a statement whose words changed after the decision says so on that decision.
test("a statement that changed after its decision says Changed since, and one that did not says nothing", () => {
  const tie = (block: string, changed: string | null) => {
    const link = shaped(statementRef(block));
    return { ...link, to: { ...link.to, changed_at: changed } };
  };
  const groups = bodyOf(
    issueTree([
      statement("r1"),
      statement("r2"),
      statement("r3"),
      decisionNode("d1", { at: "2026-11-13T07:00:00Z" }),
    ]),
    [
      decision({
        block_id: "d1",
        changed_at: "2026-11-13T07:00:00Z",
        kind: "agent",
        links: [
          tie("r1", "2026-11-14T09:00:00Z"),
          tie("r2", "2026-11-12T09:00:00Z"),
          tie("r3", null),
        ],
      }),
    ]
  );
  expect(groups[0]?.runs[0]?.statements.map((one) => [one.node.id, one.changedSince])).toEqual([
    ["spec#b-r1", true],
    ["spec#b-r2", false],
    ["spec#b-r3", false],
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

// R9: no agent's words are drawn under a person's name. A section the server marks a person's
// whose answered question this tree does not hold is the agent's record of that decision, so
// it is drawn as an agent's words and never quoted as the person's.
test("a person's decision whose question the tree does not hold is drawn as the agent's record", () => {
  const groups = bodyOf(issueTree([statement("r1"), decisionNode("d1")]), [
    decision({
      asks: ["an-ask-of-another-issue"],
      block_id: "d1",
      kind: "person",
      links: [shaped(statementRef("r1"))],
    }),
  ]);
  expect(groups[0]?.runs[0]?.decision).toMatchObject({ said: "agent", type: "decision" });
});

// R17: the mark is how the statement stands at the commit the page names, in the Verification
// filter's own reading, so one statement reads the same in both views: Met where every check of
// it passed there, Not met where one failed there, Unknown otherwise, and no check where none
// serves it. A pass recorded at another commit is no Met.
test("a statement's mark is how it stands at the commit the page names", () => {
  const passed = check({ block_id: "c1", results: [result({ id: "p1" })] });
  const failed = check({
    block_id: "c2",
    pass_stands: { answer: "no" },
    results: [result({ created_at: "2026-11-14T01:10:00Z", id: "f1", verdict: "fail" })],
  });
  const elsewhere = check({
    block_id: "c3",
    results: [result({ commit_sha: OTHER_COMMIT, created_at: "2026-11-13T00:00:00Z", id: "o1" })],
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
  expect(COMMIT).not.toBe(OTHER_COMMIT);
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
    checks: [],
    decisions: decisionsRead([]),
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
