import { expect, test } from "bun:test";
import {
  GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
  GOING_ERROR_ANSWER_FAILED,
  GOING_ERROR_NO_MODEL_CONFIGURED,
  GOING_EVENT_CHANGED,
  GOING_EVENT_CUT,
  GOING_EVENT_END,
  GOING_EVENT_ERROR,
  GOING_EVENT_ITEM,
  GOING_EVENT_NONE,
  GOING_EVENT_TEXT,
  type GraphFrame,
} from "@legion/contracts";

import { graphNode } from "../../../__tests__/graph-fixture";
import type { GraphEdge, GraphReferences, ReferenceTarget } from "../../../api/types";
import { wordsOf } from "../../refs/tree-fragment";
import { targetNaming } from "../issue-tree";
import {
  EMPTY_STATE_LINES,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  STATE_LINES,
  sentenceCase,
} from "../page-words";
import { treeContext } from "../tree-drafts";
import { linkedHref, runView } from "./run-page";

const RUN = "3f2b8c1e-5d4a-4b6f-9e2d-1a7c0b9e8f43";
const ASKED_AT = "2026-11-17T09:00:00Z";
const QUESTION = "When does the release ship?\n\nAnd who **owns** it?";

/** The run's read: its node, carrying `frames` as the server's answer does, and `edges`. */
function answerOf(
  frames: readonly GraphFrame[],
  edges: readonly GraphEdge[] = []
): GraphReferences {
  return {
    edges: [...edges],
    node: graphNode({
      frames: [...frames],
      id: RUN,
      kind: "interaction",
      made_at: ASKED_AT,
      ref: `dispatch://interaction/${RUN}`,
      text: QUESTION,
    }),
  };
}

/** The words of the issue's node the run changed, as its edge's excerpt carries them, and the
 *  words its one line is cut from: its marks gone. The page cuts and names the line
 *  (`RunPage.test.tsx`). */
const CHANGED_ISSUE_TEXT = "Ship the **release** on Friday. Then rest.";
const CHANGED_ISSUE_WORDS = "Ship the release on Friday. Then rest.";

/** An edge from the issue CORE-1, which the run changed. */
const CHANGED_ISSUE: GraphEdge = {
  created_at: "2026-11-17T09:00:05Z",
  direction: "in",
  excerpt: { text: CHANGED_ISSUE_TEXT },
  kind: "changed_by_run",
  node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: "dispatch://CORE-1" }),
  source_seq: null,
};

/** A frame of the answer. */
function frame(sequence: number, name: string, data: GraphFrame["data"] = null): GraphFrame {
  return { data, name, sequence };
}

test("the view holds the question, the time, each frame in order with its drawing, and the changed nodes", () => {
  // The frames as the read may hold them, out of their order.
  const frames = [
    frame(5, GOING_EVENT_CHANGED, { dropped: 2 }),
    frame(2, GOING_EVENT_TEXT, { text: "ships on Friday\\." }),
    frame(6, GOING_EVENT_END, { items: 1, words: 120 }),
    frame(3, GOING_EVENT_ITEM, {
      href: "/issues/CORE-1",
      id: "CORE-1",
      kind: "issue",
      title: "The release plan",
    }),
    frame(1, GOING_EVENT_TEXT, { text: "The release " }),
    frame(4, GOING_EVENT_CUT, { words: 120 }),
  ];
  const view = runView(answerOf(frames, [CHANGED_ISSUE]));

  expect(view.question).toBe(QUESTION);
  expect(view.at).toBe(ASKED_AT);
  // In `sequence` order: the two texts as one answer, the item by its title and its link alone,
  // the cut and the changed frames as their sentences, and nothing for the end.
  expect(view.parts).toEqual([
    { kind: "answer", markdown: "The release ships on Friday\\." },
    { href: "/issues/CORE-1", kind: "item", title: "The release plan" },
    { kind: "line", words: `${STATE_LINES.run_cut.before}120${STATE_LINES.run_cut.after}` },
    {
      kind: "line",
      words: `${STATE_LINES.run_changed.before}2${STATE_LINES.run_changed.after}`,
    },
  ]);
  // The issue it changed, by its kind's word and the words its one line is cut from, linked to
  // it.
  expect(view.changed).toEqual([
    {
      href: "/issues/CORE-1",
      key: "dispatch://CORE-1",
      kind: sentenceCase(NODE_REFERENCES_WORDS.kinds.issue),
      words: CHANGED_ISSUE_WORDS,
    },
  ]);

  // A node two edges name is one changed node.
  expect(runView(answerOf([], [CHANGED_ISSUE, CHANGED_ISSUE])).changed).toHaveLength(1);
  // A changed node whose read sent the server's reading of its words is named by that reading,
  // not by its words as stored.
  const read: NonNullable<GraphReferences["edges"]>[number] = {
    ...CHANGED_ISSUE,
    excerpt: {
      text: CHANGED_ISSUE_TEXT,
      text_tree: {
        content: [
          { content: [{ text: "Ship the release on Monday.", type: "text" }], type: "paragraph" },
        ],
        type: "doc",
      },
    },
  };
  expect(runView(answerOf([], [read])).changed.map((node) => node.words)).toEqual([
    "Ship the release on Monday.",
  ]);
  // A count that is no number draws as 0.
  expect(runView(answerOf([frame(1, GOING_EVENT_CUT, { words: "many" })])).parts).toEqual([
    { kind: "line", words: `${STATE_LINES.run_cut.before}0${STATE_LINES.run_cut.after}` },
  ]);

  // No answer at all, and each way a run fails, by its line; a code the page does not know, and
  // none, draw the answer having failed.
  const lines = (frames: readonly GraphFrame[]) =>
    runView(answerOf(frames)).parts.map((part) => (part.kind === "line" ? part.words : ""));
  expect(lines([frame(1, GOING_EVENT_NONE), frame(2, GOING_EVENT_END)])).toEqual([
    EMPTY_STATE_LINES.run_none,
  ]);
  const errors = [
    GOING_ERROR_NO_MODEL_CONFIGURED,
    GOING_ERROR_ANSWER_DID_NOT_ARRIVE,
    GOING_ERROR_ANSWER_FAILED,
    "SOMETHING_ELSE",
    undefined,
  ];
  expect(errors.map((code) => lines([frame(1, GOING_EVENT_ERROR, { code })]))).toEqual([
    [STATE_LINES.run_no_model],
    [STATE_LINES.run_did_not_arrive],
    [STATE_LINES.run_failed],
    [STATE_LINES.run_failed],
    [STATE_LINES.run_failed],
  ]);
  // A frame of a name the page does not know draws nothing.
  expect(lines([frame(1, "progress", { text: "searching" })])).toEqual([]);
});

test("a changed node is named by the page's words alone, and an edge the page does not know is left out", () => {
  const riskText = "Drop the **release** if the checks fail. Then wait.";
  const risk: GraphEdge = {
    ...CHANGED_ISSUE,
    excerpt: { text: riskText },
    node: graphNode({ id: "4d1e2f3a-6b5c-4d7e-8f90-a1b2c3d4e5f6", kind: "risk" }),
  };
  // A node with neither a kind word nor an excerpt is kept for the view to decide whether
  // to draw, just like a node whose excerpt becomes empty after its first line is cut.
  const silent: GraphEdge = {
    ...risk,
    excerpt: undefined,
    node: graphNode({ id: "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d", kind: "risk" }),
  };
  // An edge kind, and a node kind at its other end, that a newer build answers with.
  const newerEdge: GraphEdge = { ...CHANGED_ISSUE, kind: "rewrote" as GraphEdge["kind"] };
  const newerNode: GraphEdge = {
    ...CHANGED_ISSUE,
    node: graphNode({ id: "9f8e7d6c", kind: "widget" as GraphEdge["node"]["kind"] }),
  };

  // Neither unfamiliar edge is passed to the view. The risks carry no schema token as a kind word.
  expect(runView(answerOf([], [newerEdge, risk, newerNode, silent])).changed).toEqual([
    {
      href: undefined,
      key: "risk:4d1e2f3a-6b5c-4d7e-8f90-a1b2c3d4e5f6",
      kind: "",
      words: "Drop the release if the checks fail. Then wait.",
    },
    {
      href: undefined,
      key: "risk:7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d",
      kind: "",
      words: "",
    },
  ]);
});

test("a run page with no issue names a comment, spec block and document as external targets", () => {
  const comment = "dispatch://CORE-2/comment/44444444-4444-4444-8444-000000000001";
  const block = "dispatch://CORE-2/spec#b-r1";
  const document = "dispatch://CORE-2/artifact/notes";
  const blockTree = {
    content: [{ content: [{ text: "Keep the cache warm.", type: "text" }], type: "paragraph" }],
    type: "doc",
  };
  const targets: ReferenceTarget[] = [
    { gone: false, kind: "comment", name: "**Check the logs**", ref: comment },
    {
      gone: false,
      kind: "block",
      name: "Not the displayed reading.",
      ref: block,
      text_tree: blockTree,
    },
    { gone: false, kind: "artifact", name: "release-notes.md", ref: document },
  ];
  const { readings } = targetNaming({ targets });
  expect(
    [...readings].map(([ref, reading]) => [
      ref,
      "tree" in reading ? wordsOf(reading.tree) : reading.words,
    ])
  ).toEqual([
    [comment, "Check the logs"],
    [block, "Keep the cache warm."],
    [document, "release-notes.md"],
  ]);
  expect(readings.get(block)).toEqual({ tree: blockTree });
  expect(readings.get(document)?.chipWord).toBe(NODE_TYPE_NAMES.document);
  const context = treeContext({ targets });
  expect(context.key).toBeUndefined();
  expect(targets.map(({ ref }) => context.nodeIdOf(ref))).toEqual([
    undefined,
    undefined,
    undefined,
  ]);
});

// The page links an item's href only where it opens it as the reader expects: a path the browser
// reads on Dispatch's own origin, as written, or an absolute `http:` or `https:` URL, in the form
// `safeExternalHref` parses it into, as every link the app makes of a URL from outside it.
test("an item's href is linked only as a path on Dispatch's origin or an http(s) URL", () => {
  for (const [href, linked] of [
    ["/issues/CORE-9", "/issues/CORE-9"],
    ["/", "/"],
    ["/runs/3f2b8c1e?at=1#answer", "/runs/3f2b8c1e?at=1#answer"],
    ["https://example.com/release", "https://example.com/release"],
    ["http://example.com", "http://example.com/"],
    ["HTTPS://example.com/release", "https://example.com/release"],
    // A URL with no `//` after its scheme, and one after a space, each as it parses: written as
    // it came, the first would open a path on the run's own address.
    ["https:example.com", "https://example.com/"],
    [" https://x", "https://x/"],
  ] as const) {
    expect(linkedHref(href), JSON.stringify(href)).toBe(linked);
  }
  for (const href of [
    "javascript:alert(1)",
    "JavaScript:alert(1)",
    "data:text/html,<p>hi</p>",
    "mailto:someone@example.com",
    "dispatch://CORE-9",
    // Another host, as `//host` and as the forms the browser reads as it.
    "//elsewhere.example/plan",
    "/\\elsewhere.example/plan",
    "/\t/elsewhere.example/plan",
    "/\n/elsewhere.example/plan",
    // A path relative to the run's own address, and none at all.
    "issues/CORE-9",
    "",
    // No valid absolute URL, though it starts as one: it names no host.
    "https://",
  ]) {
    expect(linkedHref(href), JSON.stringify(href)).toBeUndefined();
  }

  // An item whose href is not linked keeps its title, and opens nothing; one whose href is a URL
  // opens it in the form it parses into.
  expect(
    runView(
      answerOf([
        frame(1, GOING_EVENT_ITEM, { href: "javascript:alert(1)", title: "Run a script" }),
        frame(2, GOING_EVENT_ITEM, { href: "/issues/CORE-9", title: "The release plan" }),
        frame(3, GOING_EVENT_ITEM, { href: "https:example.com", title: "The release notes" }),
      ])
    ).parts
  ).toEqual([
    { href: undefined, kind: "item", title: "Run a script" },
    { href: "/issues/CORE-9", kind: "item", title: "The release plan" },
    { href: "https://example.com/", kind: "item", title: "The release notes" },
  ]);
});
