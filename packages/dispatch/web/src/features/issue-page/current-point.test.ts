import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import type { Actor } from "../../api/types";
import {
  currentPath,
  type PointAim,
  type PointMark,
  pointMarks,
  useCurrentPoint,
} from "./current-point";
import type { TreeNode } from "./issue-tree";
import { type OutlineWriter, OutlineWriterProvider, useOutlineWriter } from "./outline-writer";
import type { NodeType } from "./page-words";
import {
  CHILD_PAGE,
  NOTHING_OPENED,
  type OpenedBranches,
  placeOf,
  treeRows,
  withFold,
} from "./tree-fold";

/**
 * The conversation's current point: the node the reader's next turn answers, and the drawn rows
 * that carry its mark. Each tree is built by hand, as `issue-tree.ts` places nodes, and each set
 * of rows by the real fold (`treeRows`), so every trail a mark is matched by is the one the page
 * draws.
 */

const KEY = "CORE-1";
const BOB: Actor = { id: "bob", kind: "user" };

/** A statement of the issue's spec, by its node id. */
const STATEMENT = "spec#b-r1";

/** A check of the Verification document, which the transclusion cases draw under two
 *  requirements. */
const CHECK = "verification#b-c1";

/** A time on the issue's day, `minute` minutes past ten: every act below is made after the issue
 *  itself, at nine. */
function at(minute: number): string {
  return `2026-11-15T10:${String(minute).padStart(2, "0")}:00Z`;
}

function node(partial: Partial<TreeNode> & { id: string; type: NodeType }): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    question: false,
    replaced: false,
    text: partial.id,
    transcluded: false,
    waiting: false,
    ...partial,
  };
}

/** The issue's own node, made before every act under it. */
function issue(children: readonly TreeNode[]): TreeNode {
  return node({ at: "2026-11-15T09:00:00Z", children, id: KEY, type: "issue" });
}

/** Comment `id`, made at `made`, its replies under it. */
function comment(id: string, made: string, children: readonly TreeNode[] = []): TreeNode {
  return node({
    at: made,
    children,
    id,
    reference: `dispatch://${KEY}/comment/${id}`,
    type: "comment",
  });
}

/** Message `id`, sent at `made`. */
function message(id: string, made: string): TreeNode {
  return node({ at: made, id, reference: `dispatch://${KEY}/message/${id}`, type: "message" });
}

/** Question `id`, asked at `made`: open, and waiting on someone other than the reader unless
 *  `fields` says otherwise. */
function ask(id: string, made: string, fields: Partial<TreeNode> = {}): TreeNode {
  return node({
    at: made,
    id,
    question: true,
    reference: `dispatch://${KEY}/ask/${id}`,
    state: "open",
    type: "ask",
    ...fields,
  });
}

/** Question `id`, asked at `made`, open and waiting on the reader. */
function waitingAsk(id: string, made: string): TreeNode {
  return ask(id, made, { waiting: true });
}

/** The writer with no target set: the line writes a new comment on the issue. */
const NOTHING_AIMED: PointAim = { reply: null, statement: undefined };

/** The writer commenting on the statement whose row is `row`. */
function commentingOn(row: string): PointAim {
  return { ...NOTHING_AIMED, statement: { artifact: "spec", quote: "The page draws a box.", row } };
}

/** The writer replying to the entry `id` of kind `parentKind`, as a reply names it: by the
 *  entry's own id, and never by its row. */
function replyingTo(id: string, parentKind: "ask" | "comment" | "message"): PointAim {
  return { ...NOTHING_AIMED, reply: { actor: BOB, excerpt: "Earlier words", id, parentKind } };
}

/** Folds shut by hand the row at each of `places`, as the row's fold control does (`withFold`): a
 *  fold is kept by the row's place (`placeOf`), never by its node's id. */
function closed(...places: string[]): OpenedBranches {
  return places.reduce((opened, place) => withFold(opened, place, false), NOTHING_OPENED);
}

/** The nodes `path` runs through, each found among its parent's children by its id, which no
 *  two children of one parent share. */
function nodesOn(root: TreeNode, path: readonly string[]): TreeNode[] {
  const found = [root];
  for (const id of path.slice(1)) {
    const below = found.at(-1)?.children.find((child) => child.id === id);
    if (below === undefined) throw new Error(`the tree holds no ${id} under ${found.at(-1)?.id}`);
    found.push(below);
  }
  return found;
}

/** An outline holding one of each target: a message and a comment that share the id `t2`, each
 *  drawn as a node of its own id, an open question, a statement, and, newest of all, a comment
 *  elsewhere, which is the point while nothing is aimed. */
function targets(): TreeNode {
  return issue([
    node({ at: at(1), id: "turn-m", reference: `dispatch://${KEY}/message/t2`, type: "message" }),
    comment("c1", at(2), [
      node({ at: at(3), id: "turn-c", reference: `dispatch://${KEY}/comment/t2`, type: "comment" }),
    ]),
    ask("a1", at(4)),
    node({ id: STATEMENT, statement: true, type: "requirement" }),
    comment("c9", at(9)),
  ]);
}

// While the reader has aimed the line, what it is aimed at is the point, over anything newer: a
// statement it comments on by the statement's row, and an entry it replies to by the entry's own
// id and kind, read from each row's reference.
test("the line's target is the point, a reply's found through its row's reference", () => {
  const root = targets();
  expect(currentPath(root, NOTHING_AIMED)).toEqual([KEY, "c9"]);
  expect(currentPath(root, commentingOn(STATEMENT))).toEqual([KEY, STATEMENT]);
  expect(currentPath(root, replyingTo("t2", "comment"))).toEqual([KEY, "c1", "turn-c"]);
  expect(currentPath(root, replyingTo("t2", "message"))).toEqual([KEY, "turn-m"]);
  expect(currentPath(root, replyingTo("a1", "ask"))).toEqual([KEY, "a1"]);
});

// A target the tree does not hold marks nothing, rather than a node the reader did not aim at.
test("a target the tree does not hold is no point, though a question waits on the reader", () => {
  const root = issue([waitingAsk("a1", at(1)), comment("c9", at(9))]);
  expect(currentPath(root, replyingTo("gone", "ask"))).toBeUndefined();
  expect(currentPath(root, commentingOn("spec#b-gone"))).toBeUndefined();
  expect(currentPath(root, replyingTo("gone", "comment"))).toBeUndefined();
});

test("once the target ends, the point is the question that waits on the reader", () => {
  const root = issue([comment("c1", at(1)), waitingAsk("a1", at(2)), comment("c9", at(9))]);
  expect(currentPath(root, replyingTo("c1", "comment"))).toEqual([KEY, "c1"]);
  // A writer whose reply and other targets have all ended.
  expect(currentPath(root, NOTHING_AIMED)).toEqual([KEY, "a1"]);
});

// The question the page opens on: the first in reading order that waits on the reader, at any
// depth. A question waiting on someone else is no turn of the reader's, and a decision waiting
// for their review is no question.
test("a question waiting on the reader outranks a newer act; one waiting on another does not", () => {
  const review = node({ at: at(0), id: "decision-1", type: "decision", waiting: true });
  const waits = issue([
    review,
    comment("c1", at(1), [waitingAsk("a1", at(2))]),
    waitingAsk("a2", at(3)),
    comment("c9", at(9)),
  ]);
  expect(currentPath(waits, NOTHING_AIMED)).toEqual([KEY, "c1", "a1"]);
  const theirs = issue([review, comment("c1", at(1), [ask("a1", at(2))]), comment("c9", at(9))]);
  expect(currentPath(theirs, NOTHING_AIMED)).toEqual([KEY, "c9"]);
});

/** The issue before a newer act: an answered question, a message, a check's result and a
 *  comment whose reply, at :10, is the newest act. `first` comes before all of them in reading
 *  order, so what moves the point to it is its time and not its place. */
function acts(answered: string, ...first: TreeNode[]): TreeNode {
  return issue([
    ...first,
    ask("a1", at(2), {
      answer: { at: answered, by: BOB, selected: ["Yes"], text: "" },
      question: false,
      state: "answered",
    }),
    message("m1", at(4)),
    node({ children: [node({ at: at(5), id: "r1", type: "result" })], id: CHECK, type: "check" }),
    comment("c1", at(1), [comment("c2", at(10))]),
  ]);
}

test("with nothing aimed and no question waiting, the newest act is the point", () => {
  expect(currentPath(acts(at(3)), NOTHING_AIMED)).toEqual([KEY, "c1", "c2"]);
});

test.each([
  ["comment", acts(at(3), comment("c3", at(11))), [KEY, "c3"]],
  ["message", acts(at(3), message("m2", at(11))), [KEY, "m2"]],
  ["question asked", acts(at(3), ask("a2", at(11))), [KEY, "a2"]],
  // The answer to the older question is an act on that question's node.
  ["answer", acts(at(11)), [KEY, "a1"]],
])("a later %s decides the point", (_kind, root, path) => {
  expect(currentPath(root, NOTHING_AIMED)).toEqual(path);
});

// A check's result and a decision's last change are no turn of the conversation: either, made
// after every act, leaves the point on the newest act.
test.each([
  [
    "result",
    acts(
      at(3),
      node({
        children: [node({ at: at(11), id: "r2", type: "result" })],
        id: "verification#b-c2",
        type: "check",
      })
    ),
  ],
  ["decision changed", acts(at(3), node({ at: at(11), id: "d1", type: "decision" }))],
])("a later %s is no act, and leaves the point where it was", (_kind, root) => {
  expect(currentPath(root, NOTHING_AIMED)).toEqual([KEY, "c1", "c2"]);
});

test("a new act landing moves the point to it", () => {
  const thread = (replies: readonly TreeNode[]) =>
    issue([comment("c1", at(1), [comment("c2", at(2), replies)]), comment("c3", at(3))]);
  expect(currentPath(thread([]), NOTHING_AIMED)).toEqual([KEY, "c3"]);
  expect(currentPath(thread([comment("c4", at(4))]), NOTHING_AIMED)).toEqual([
    KEY,
    "c1",
    "c2",
    "c4",
  ]);
});

// One instant, written two ways: the times are compared as instants and not as text, and the
// node drawn lower wins the tie whichever of the two spellings it carries.
test("a tie goes to the node later in reading order", () => {
  const whole = "2026-11-15T10:05:00Z";
  const fraction = "2026-11-15T10:05:00.000Z";
  const tied = issue([comment("c1", whole, [comment("c2", at(4))]), comment("c3", fraction)]);
  expect(currentPath(tied, NOTHING_AIMED)).toEqual([KEY, "c3"]);
  const swapped = issue([comment("c3", fraction), comment("c1", whole, [comment("c2", at(4))])]);
  expect(currentPath(swapped, NOTHING_AIMED)).toEqual([KEY, "c1"]);
});

/** A check drawn under two requirements, `spec#b-r1` first in reading order and `spec#b-r2`
 *  after it, holding `under`: transcluded under the first where `copyFirst`, else under the
 *  second. The branch under the transcluded placement repeats the edited one's nodes, each of
 *  them no transclusion of its own, as `issue-tree.ts` builds it. */
function placedTwice(copyFirst: boolean, under: readonly TreeNode[]): TreeNode {
  const requirement = (id: string, transcluded: boolean) =>
    node({
      children: [node({ children: under, id: CHECK, transcluded, type: "check" })],
      id,
      type: "requirement",
    });
  return issue([requirement("spec#b-r1", copyFirst), requirement("spec#b-r2", !copyFirst)]);
}

// A node placed twice is the point at its edited placement, whichever placement comes first and
// whichever rule found it: a target, the waiting question, the newest act.
test("a node placed twice is the point at its edited placement, through no transclusion", () => {
  const newest = [comment("c9", at(9))];
  const copyFirst = placedTwice(true, newest);
  const edited = [KEY, "spec#b-r2", CHECK, "c9"];
  expect(currentPath(copyFirst, NOTHING_AIMED)).toEqual(edited);
  expect(currentPath(copyFirst, replyingTo("c9", "comment"))).toEqual(edited);
  expect(nodesOn(copyFirst, edited).some((one) => one.transcluded)).toBe(false);
  const waiting = placedTwice(true, [waitingAsk("a1", at(1))]);
  expect(currentPath(waiting, NOTHING_AIMED)).toEqual([KEY, "spec#b-r2", CHECK, "a1"]);
  const copyLast = placedTwice(false, newest);
  const editedFirst = [KEY, "spec#b-r1", CHECK, "c9"];
  expect(currentPath(copyLast, NOTHING_AIMED)).toEqual(editedFirst);
  expect(nodesOn(copyLast, editedFirst).some((one) => one.transcluded)).toBe(false);
});

/** A question waiting on the reader two replies deep, under a comment newer than any of it. */
function deepQuestion(): TreeNode {
  return issue([
    comment("c1", at(1), [comment("c2", at(2), [waitingAsk("a1", at(3))])]),
    comment("c5", at(5)),
  ]);
}

test("no point marks no row", () => {
  const rows = treeRows(deepQuestion());
  expect(pointMarks(rows, undefined).size).toBe(0);
});

// The point's own row drawn, it alone carries the mark, with or without the issue's row, which
// the page draws none of (`IssueTree.tsx`).
test("the point's own row is marked here, and no other row is marked", () => {
  const root = deepQuestion();
  const path = currentPath(root, NOTHING_AIMED);
  const rows = treeRows(root);
  const here: Readonly<Record<string, PointMark>> = { "CORE-1>c1>c2>a1": "here" };
  expect(Object.fromEntries(pointMarks(rows, path))).toEqual(here);
  expect(Object.fromEntries(pointMarks(rows.slice(1), path))).toEqual(here);
});

// The point folded away, each drawn row above it on its path is marked, down to the row folded
// shut over it.
test("with the point folded away, each drawn row on its path is marked path", () => {
  const root = deepQuestion();
  const path = currentPath(root, NOTHING_AIMED);
  const rows = treeRows(root, closed(placeOf([KEY, "c1"], "c2")));
  expect(Object.fromEntries(pointMarks(rows, path))).toEqual({
    "CORE-1": "path",
    "CORE-1>c1": "path",
    "CORE-1>c1>c2": "path",
  });
  expect(Object.fromEntries(pointMarks(rows.slice(1), path))).toEqual({
    "CORE-1>c1": "path",
    "CORE-1>c1>c2": "path",
  });
  expect(
    Object.fromEntries(pointMarks(treeRows(root, closed(placeOf([KEY], "c1"))), path))
  ).toEqual({
    "CORE-1": "path",
    "CORE-1>c1": "path",
  });
});

// Zoomed into a node, the rows start at that node and their trails start at the issue, as the
// page draws them (`IssueTree.tsx`'s `rootedAbove`), so a mark is matched by its trail and never
// by a row's level or place in the list.
test("the rows of a zoomed outline are marked by their trails", () => {
  const root = deepQuestion();
  const path = currentPath(root, NOTHING_AIMED);
  const [zoom] = root.children;
  if (zoom === undefined) throw new Error("the issue holds no comment to zoom into");
  const zoomed = (opened: OpenedBranches) => treeRows(zoom, opened, [KEY]);
  expect(Object.fromEntries(pointMarks(zoomed(NOTHING_OPENED), path))).toEqual({
    "CORE-1>c1>c2>a1": "here",
  });
  expect(Object.fromEntries(pointMarks(zoomed(closed(placeOf([KEY, "c1"], "c2"))), path))).toEqual({
    "CORE-1>c1": "path",
    "CORE-1>c1>c2": "path",
  });
});

// The rows of a transcluded placement carry the same node ids as the edited one's, and a mark is
// on the edited placement's rows alone, folded or not.
test("a transcluded placement's rows are never marked", () => {
  const root = placedTwice(true, [waitingAsk("a1", at(1))]);
  const path = currentPath(root, NOTHING_AIMED);
  const open = treeRows(root);
  expect(open.filter((row) => row.kind === "node" && row.node.id === "a1")).toHaveLength(2);
  expect(Object.fromEntries(pointMarks(open, path))).toEqual({
    [`CORE-1>spec#b-r2>${CHECK}>a1`]: "here",
  });
  const folded = treeRows(
    root,
    closed(placeOf([KEY, "spec#b-r1"], CHECK), placeOf([KEY, "spec#b-r2"], CHECK))
  );
  expect(folded.filter((row) => row.kind === "node" && row.node.id === CHECK)).toHaveLength(2);
  expect(Object.fromEntries(pointMarks(folded, path))).toEqual({
    "CORE-1": "path",
    "CORE-1>spec#b-r2": "path",
    [`CORE-1>spec#b-r2>${CHECK}`]: "path",
  });
});

// A node's children past its first page draw one row that pages them, which is no node's own
// row. Where the point stands among the children it stands for, or under one of them, that row
// carries the path's mark beside each drawn row above it on the path, and no row is marked here,
// so the reader finds the page that holds the point. Another node's row that pages its own
// children carries nothing. The control: the same tree with the point among the first page's
// children, whose own row is drawn and marked here, gives the row that pages the rest no mark.
test("a row that pages a node's children is marked path where the point stands among them, and not where the point is drawn", () => {
  const older = Array.from({ length: CHILD_PAGE + 1 }, (_, index) =>
    comment(`o${index + 1}`, at(0))
  );
  // The last reply, past the first page, holds the newest act.
  const last = `r${CHILD_PAGE + 1}`;
  const replies = Array.from({ length: CHILD_PAGE + 1 }, (_, index) =>
    index < CHILD_PAGE
      ? comment(`r${index + 1}`, at(index + 1))
      : comment(last, at(index + 1), [comment("deep", at(index + 2))])
  );
  const root = issue([comment("c0", at(0), older), comment("c1", at(0), replies)]);
  const rows = treeRows(root);
  expect(rows.flatMap((row) => (row.kind === "page" ? [row.of.id] : []))).toEqual(["c0", "c1"]);
  const behind: Readonly<Record<string, PointMark>> = {
    "CORE-1": "path",
    "CORE-1>c1": "path",
    "page:CORE-1>c1": "path",
  };
  const under = currentPath(root, NOTHING_AIMED);
  expect(under).toEqual([KEY, "c1", last, "deep"]);
  expect(Object.fromEntries(pointMarks(rows, under))).toEqual(behind);
  const among = currentPath(root, replyingTo(last, "comment"));
  expect(among).toEqual([KEY, "c1", last]);
  expect(Object.fromEntries(pointMarks(rows, among))).toEqual(behind);

  // The control: the point among the first page's children.
  const drawn = currentPath(root, replyingTo("r1", "comment"));
  expect(drawn).toEqual([KEY, "c1", "r1"]);
  expect(Object.fromEntries(pointMarks(rows, drawn))).toEqual({ "CORE-1>c1>r1": "here" });
});

/** The page's writer and the point it holds over `root`, every read the writer makes answered
 *  before it mounts, so nothing asks the network (`statement-target.test.tsx`). */
function renderPoint(root: TreeNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["asks", KEY, "thread"], []);
  for (const read of ["comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  client.setQueryData(["agents"], []);
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(OutlineWriterProvider, { children, issueKey: KEY })
    );
  const view = renderHook(() => ({ point: useCurrentPoint(root), writer: useOutlineWriter() }), {
    wrapper,
  });
  const writer = (): OutlineWriter => {
    const held = view.result.current.writer;
    if (held === undefined) throw new Error("the page holds no writer");
    return held;
  };
  return { view, writer };
}

// The hook reads the writer's two targets and nothing else it holds: an answer the writer holds
// aims nothing (`OutlineWriter.answer`), and a kind switched while the line replies
// leaves the point as it was, the same path and not merely an equal one.
test("the page's point follows the writer's targets and nothing else the writer holds", () => {
  const question = ask("a1", at(1));
  const { view, writer } = renderPoint(issue([question, comment("c9", at(9))]));
  try {
    expect(view.result.current.point).toEqual([KEY, "c9"]);
    act(() => writer().answerNode(question));
    expect(writer().answer?.askId).toBe("a1");
    expect(view.result.current.point).toEqual([KEY, "c9"]);
    act(() => writer().replyTo({ author: BOB, id: "a1", kind: "ask", text: "a1" }));
    expect(view.result.current.point).toEqual([KEY, "a1"]);
    const aimed = view.result.current.point;
    act(() => writer().setKind("ask"));
    expect(writer().kind).toBe("ask");
    expect(view.result.current.point).toBe(aimed);
    act(() => writer().clear());
    expect(view.result.current.point).toEqual([KEY, "c9"]);
  } finally {
    view.unmount();
  }
});
