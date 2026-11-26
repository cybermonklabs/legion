import { beforeAll, expect, test } from "bun:test";

import type { DecisionLink, DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { decisionNodes } from "../decision-nodes/decision-nodes";
import { inReviewList } from "../decision-review";
import type { TreeNode } from "../issue-tree";
import { NODE_TYPE_NAMES } from "../page-words";
import { presetView } from "../presets";
import { parentLinkWords } from "../projection-fixture";
import { nodeIdOf as issueNodeIdOf } from "../tree-drafts";
import { treeNode } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
import { changesOf } from "./decisions-entries";
import {
  AGENT,
  answeredAsk,
  ask,
  askRef,
  changeComment,
  closing,
  decision,
  decisionNode,
  decisionOf,
  decisionsOf,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  personDecision,
  READER,
  READER_ACTOR,
  SPEC_DOCUMENT,
  SPEC_SLUG,
  shaped,
  shapedTuples,
  specRef,
  statement,
  treeAnswer,
} from "./decisions-fixtures";
import {
  conversationPath,
  type DecisionsAsk,
  type DecisionsParts,
  decisionsParts,
  waitingOrder,
} from "./decisions-parts";
import { entryStanding } from "./entry-standing";

/**
 * The Decisions filter's four parts, read from the tree, the decision nodes of its decisions
 * view and the asks. The model is pure, so each case hands
 * it a tree built by hand and reads what it answers.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** How this issue addresses a node, which is the page's own rule (`tree-drafts.ts`). */
const nodeIdOf = issueNodeIdOf(issueDetails());

/** The parts of the tree that holds `children`, with the decision nodes of `made` and the
 *  comments the tree read hangs under them (`comments`), which are the Changes a person asked of
 *  them. */
function partsOf(
  children: readonly TreeNode[],
  made: readonly TreeAnswerNode[] = [],
  asks: readonly DecisionsAsk[] = [],
  view?: TreeNode,
  comments: readonly TreeAnswerNode[] = []
): DecisionsParts {
  const decisions = decisionsOf(made);
  return decisionsParts({
    asks,
    changes: changesOf(treeAnswer([...made, ...comments]), decisions),
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
      decision("review", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
      decision("kept", {
        decision: {
          kept: {
            artifact_id: SPEC_DOCUMENT.id,
            block_id: "kept",
            changed_version: 1,
            kept_at: "2026-11-15T09:00:00Z",
            kept_by: "alice",
          },
          links: [shaped(specRef("s2"))],
          waiting_review: false,
        },
      }),
      decision("untied"),
      decision("replaced", {
        decision: {
          links: [shaped(specRef("s1"))],
          replaced: true,
          waiting_review: false,
        },
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
// of the issue for, one tied to a note of the spec, which is no statement, and one tied to
// nothing. The review list holds the two, newest first, and the other two stand at the foot.
test("the review list is read from the shaped ties alone", () => {
  const made = [
    decision("older", {
      decision: {
        changed_at: "2026-11-10T12:00:00Z",
        links: [shaped(specRef("s1"))],
      },
    }),
    decision("newer", {
      decision: {
        changed_at: "2026-11-11T12:00:00Z",
        links: [shaped(specRef("s2"))],
      },
    }),
    // Tied to the spec's summary, a callout, which is no statement.
    decision("note", {
      decision: {
        links: [shaped(specRef("summary"))],
      },
    }),
    decision("nothing"),
  ];
  const parts = partsOf(
    [
      statement("s1"),
      statement("s2"),
      node({ id: `${SPEC_SLUG}#b-summary`, reference: specRef("summary"), type: "callout" }),
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
  expect(decisionsOf(made).map((row) => inReviewList(row, isStatement))).toEqual([
    true,
    true,
    false,
    false,
  ]);
  // A decision that does not wait is no entry of the list, whatever its ties.
  expect(
    inReviewList(
      decisionOf(
        decision("kept", {
          decision: {
            links: [shaped(specRef("s1"))],
            waiting_review: false,
          },
        })
      ),
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
      decision("foreign", {
        decision: {
          links: [shaped("dispatch://CORE-2/spec#b-s1")],
        },
      }),
      decision("unreadable", {
        decision: {
          links: [{ ...shaped(specRef("s1")), to: unaddressed }],
        },
      }),
      decision("pinned", {
        decision: {
          links: [shaped(`${specRef("s1")}@v3`)],
        },
      }),
    ]
  );

  expect(idsOf(parts).review).toEqual([D("pinned")]);
  expect(idsOf(parts).agents).toEqual([D("foreign"), D("unreadable")]);
});

// R2, R9: a person's decision is the answered ask itself, which the decisions read names by the
// ask's own address: the ask's entry is that decision, and it carries the statements its
// `shaped` ties name. An agent's reading of the answer is a `reading` block under the ask, and
// the filter draws none (R9: "No restatement of a person's answer appears anywhere in the
// view"), whether it is handed the whole tree or the Decisions filter's own view of it. An open
// question has no decision yet, so it carries no ties, and its reading is no entry either.
test("a person's decision is the answered ask's entry with its ties, and a reading is none", () => {
  const reading = (id: string) =>
    node({
      author: AGENT,
      id: `${SPEC_SLUG}#b-${id}`,
      reference: specRef(id),
      text: "The planner reads the answer as one rail for every width.",
      type: "reading",
    });
  const children = [
    statement("s1"),
    statement("s2"),
    answeredAsk(
      "a1",
      { at: "2026-11-12T09:00:00Z", text: "Keep the rail." },
      { children: [reading("g1")] }
    ),
    openAsk("q1", true, { children: [reading("g2")] }),
  ];
  const made = [
    personDecision("a1", {
      decision: {
        links: [shaped(specRef("s1")), shaped(specRef("s2"))],
      },
    }),
  ];
  const views = [
    undefined,
    presetView(issueTree(children), "decisions", "", new Set<string>()).root,
  ];

  for (const view of views) {
    const parts = partsOf(children, made, [], view);
    const all = Object.values(idsOf(parts)).flat();
    expect(all, "the filter drew an agent's reading").not.toContain(reading("g1").id);
    expect(all, "the filter drew an agent's reading").not.toContain(reading("g2").id);
    expect(idsOf(parts).yours).toEqual(["a1"]);
    expect(parts.yours[0]?.shapes.map((shape) => shape.id)).toEqual([
      statement("s1").id,
      statement("s2").id,
    ]);
    expect(idsOf(parts).open_questions).toEqual(["q1"]);
    expect(parts.open_questions[0]?.shapes).toEqual([]);
    // No decision node stands for the person's decision: the ask is the only entry of it.
    expect([...idsOf(parts).agents, ...idsOf(parts).review, ...idsOf(parts).others]).toEqual([]);
  }
});

// R2: the Decisions view holds asks, decisions and readings and no statement, and the route sends
// a tuple only where the answer holds its far end (`issue_tree_read.go`'s `answer`), so the view's
// answer holds no `shaped` tuple from an answered ask. The person's decision the read answers on
// the ask (`decision`) is a decision all the same, whose links are its ties: once the view
// answers, the question's row keeps the statements it drew from the tree read before.
test("a person's decision keeps its statements once the Decisions view answers", () => {
  const children = [
    statement("s1"),
    answeredAsk("a1", { at: "2026-11-12T09:00:00Z", text: "Keep the rail." }),
  ];
  const decided = personDecision("a1", { decision: { links: [shaped(specRef("s1"))] } });
  // The statement as the tree read answers it: a block of the spec.
  const held = treeNode("s1", "requirement", KEY, {
    document: { artifact_id: SPEC_DOCUMENT.id, position: 0, slug: SPEC_SLUG },
  });
  const tree = treeAnswer([held, decided], shapedTuples([held, decided]));
  const view = { ...treeAnswer([decided], shapedTuples([decided])), view: "decisions" };
  expect(view.tuples, "the view holds a tuple the route never sends").toEqual([]);

  for (const [read, answer] of [
    ["the tree read", tree],
    ["the Decisions view", view],
  ] as const) {
    const decisions = decisionNodes(answer, { asks: undefined, issue: { key: KEY } });
    expect(
      decisions.map((one) => one.ref),
      `${read} answers no person's decision`
    ).toEqual([askRef("a1")]);
    const parts = decisionsParts({
      asks: [],
      decisions,
      issueKey: KEY,
      nodeIdOf,
      reader: READER,
      root: issueTree(children),
    });
    expect(idsOf(parts).yours).toEqual(["a1"]);
    expect(
      parts.yours[0]?.shapes.map((shape) => shape.id),
      `the question lost its statements in ${read}`
    ).toEqual([statement("s1").id]);
  }
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
        decision: {
          kept: {
            artifact_id: SPEC_DOCUMENT.id,
            block_id: "kept-by-bob",
            changed_version: 1,
            kept_at: "2026-11-15T09:00:00Z",
            kept_by: "bob",
          },
          links: [shaped(specRef("s1"))],
          waiting_review: false,
        },
      }),
    ],
    [closing("declined-by-bob", { actor: bob, at: "2026-11-12T09:00:00Z", kind: "declined" })]
  );

  expect(idsOf(parts).yours).toEqual(["a-mine"]);
  expect(idsOf(parts).others).toEqual([D("kept-by-bob"), "a-bobs", "declined-by-bob"]);
});

// A Change is a comment a person wrote that the tree read hangs under the
// decision, since it last changed. The decision then waits on its agent: it says a change was
// asked for, it leaves the review list for the decisions of whoever asked, and it carries what
// they wrote. A comment older than the decision's words is no change of them, and the decision
// waits for review.
test("a decision the reader asked to change leaves the review list and carries the change", () => {
  const parts = partsOf(
    [statement("s1"), decisionNode("asked"), decisionNode("stale"), decisionNode("waits")],
    [
      decision("asked", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
      decision("stale", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
      decision("waits", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
    ],
    [],
    undefined,
    [
      changeComment("c-new", "asked", "2026-11-12T09:00:00Z", "Change asked, please."),
      // Made before the decision last changed (2026-11-10T12:00Z): the agent has answered it.
      changeComment("c-old", "stale", "2026-11-09T09:00:00Z", "Change stale, please."),
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
    inReviewList(
      decisionOf(
        decision("asked", {
          decision: {
            links: [shaped(specRef("s1"))],
          },
        })
      ),
      isStatement,
      true
    )
  ).toBe(false);
  expect(waitingOrder(parts)).toEqual([D("stale"), D("waits")]);
});

// R2, R3: a decision the reader asked to change reads "Changed by" with the reader's own name, as
// the row's margin and the panel's facts draw how an entry stands (`entryStanding`), where it read
// "Change requested"; and it stands among the reader's own decisions, since a statement rests on
// it.
test("a decision the reader asked to change reads Changed by with the reader's name", () => {
  const parts = partsOf(
    [statement("s1"), decisionNode("asked")],
    [decision("asked", { decision: { links: [shaped(specRef("s1"))] } })],
    [],
    undefined,
    [changeComment("c-asked", "asked", "2026-11-12T09:00:00Z", "Change asked, please.")]
  );

  expect(idsOf(parts).yours).toEqual([D("asked")]);
  const [asked] = parts.yours;
  if (asked === undefined) throw new Error("the changed decision stands in none of her parts");
  expect(
    entryStanding(asked, (actor) => (actor.id === READER ? "Alice" : actor.id)),
    "the decision the reader asked to change does not read Changed by with her name"
  ).toEqual({ token: "change_requested", words: "Changed by Alice" });
});

// R2: the reader's own part, and what others decided, hold an agent decision a person asked to
// change only where a statement rests on it, as they hold a kept one. One no statement rests on
// stands at the foot, whoever asked, and still carries the Change and says it was asked.
test("a changed decision no statement rests on stands at the foot", () => {
  const bob = { id: "bob", kind: "user" as const };
  const parts = partsOf(
    [statement("s1"), decisionNode("tied"), decisionNode("loose"), decisionNode("bobs")],
    [
      decision("tied", { decision: { links: [shaped(specRef("s1"))] } }),
      decision("loose"),
      decision("bobs"),
    ],
    [],
    undefined,
    [
      changeComment("c-tied", "tied", "2026-11-12T09:00:00Z", "Change the tied one."),
      changeComment("c-loose", "loose", "2026-11-13T09:00:00Z", "Change the loose one."),
      changeComment("c-bobs", "bobs", "2026-11-14T09:00:00Z", "Change Bob's one.", bob),
    ]
  );

  expect(idsOf(parts)).toMatchObject({
    agents: [D("loose"), D("bobs")],
    others: [],
    review: [],
    yours: [D("tied")],
  });
  expect(parts.agents.map((entry) => [entry.change?.id, entry.change?.by, entry.standing])).toEqual(
    [
      ["c-loose", READER_ACTOR, "change_requested"],
      ["c-bobs", bob, "change_requested"],
    ]
  );
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
    artifact_id: SPEC_DOCUMENT.id,
    block_id: "kept",
    changed_version: 1,
    kept_at: "2026-11-15T09:00:00Z",
    kept_by: "alice",
  };
  const parts = partsOf(
    [statement("s1"), decisionNode("kept"), decisionNode("changed"), decisionNode("loose")],
    [
      decision("kept", {
        decision: {
          kept: keep,
          links: [shaped(specRef("s1"))],
          waiting_review: false,
        },
      }),
      decision("changed", {
        decision: {
          kept: { ...keep, block_id: "changed" },
          links: [shaped(specRef("s1"))],
          waiting_review: true,
        },
      }),
      // Kept, and tied to nothing: an agent's decision no statement rests on, which says who
      // kept it where it stands.
      decision("loose", {
        decision: {
          kept: { ...keep, block_id: "loose" },
          waiting_review: false,
        },
      }),
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
        decision: {
          links: [
            shaped(specRef("s1")),
            shaped(specRef("s2"), true),
            shaped(specRef("s3")),
            // A second block of the decision names the first statement again.
            shaped(specRef("s1")),
          ],
        },
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
// on that node, where the decision's own address opens the spec at its block. R2: a node's
// parent is one line on its row, since no row of this filter stands under another.
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
      decision("under", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
      decision("top", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
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

  expect(conversationPath({ reference: specRef("d1") })).toBe(`/issues/${KEY}#b-d1`);
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
      decision("headed", {
        decision: {
          links: [shaped(specRef("s1")), shaped(specRef("s2"))],
        },
      }),
      decision("wordless", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
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
    [
      decision("twice", {
        decision: {
          links: [shaped(specRef("s1"))],
        },
      }),
    ]
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
    decision("one", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
    decision("two", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
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
  const made = [
    decision("review", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
  ];
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

// R2: an overtaken decision stands in the part its kind puts it in - here the review list, since
// it waits and a statement rests on it - and counts in no count of what waits, so the header's
// count walks past it to the next decision that waits.
test("an overtaken decision stays in its part and is not in waitingOrder", () => {
  const parts = partsOf(
    [statement("s1"), decisionNode("overtaken"), decisionNode("waits")],
    [
      decision("overtaken", { decision: { links: [shaped(specRef("s1"))] }, state: "overtaken" }),
      decision("waits", { decision: { links: [shaped(specRef("s1"))] } }),
    ]
  );

  expect(idsOf(parts).review).toEqual([D("overtaken"), D("waits")]);
  expect(parts.review[0]).toMatchObject({ overtaken: true, standing: "waiting_review" });
  expect(parts.review[1]).not.toHaveProperty("overtaken");
  expect(waitingOrder(parts)).toEqual([D("waits")]);
});

// The tree draws from the reads that have answered: before the decisions view and the asks
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
  const made = [
    decision("one", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
  ];
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
