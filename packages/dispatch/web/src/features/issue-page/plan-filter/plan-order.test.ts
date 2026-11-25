import { expect, test } from "bun:test";

import type { Artifact } from "../../../api/types";
import type { NodeChip, TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS, type NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { type PlanEntry, planChart, planOrder, writtenOrder } from "./plan-order";

/**
 * The plan's order, read off the edges today's model holds.
 *
 * The nodes are built as `issue-tree.ts` builds them from the plan document's blocks: a task's
 * id is `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`), and its `needs` are its chips,
 * each naming its target by that target's own `dispatch://` address (`placeBlocks`).
 */

const KEY = "CORE-1";
const PLAN = "plan-md";

/** The address of one block of the plan, as a `needs` pointer holds it. */
function taskRef(blockId: string): string {
  return `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`;
}

function chip(ref: string, name = ""): NodeChip {
  return { answered: name !== "", gone: false, name, ref };
}

/** The chip the tree draws for one `needs` pointer of a task's block (`tree-drafts.ts`'s
 *  `placeBlocks`): the target's name under the attribute's own word. */
function need(blockId: string): NodeChip {
  return { ...chip(taskRef(blockId), blockId), label: BLOCK_ATTRIBUTE_LABELS.needs };
}

/** One plan task, by its block id, with the block ids of the tasks it needs. */
function task(
  blockId: string,
  needs: readonly string[] = [],
  type: NodeType = "plan_task"
): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: needs.map(need),
    facts: [],
    gaps: [],
    held: 0,
    id: blockNodeId(PLAN, blockId),
    question: false,
    reference: taskRef(blockId),
    replaced: false,
    text: `Task ${blockId}`,
    transcluded: false,
    type,
    waiting: false,
  };
}

/** What the order draws, in order: a step by its block id, a cycle by the block ids in it. */
function drawn(entries: readonly PlanEntry[]): string[] {
  return entries.map((entry) =>
    entry.kind === "step"
      ? blockOf(entry.task.id)
      : `cycle ${entry.tasks.map((one) => blockOf(one.id)).join(" ")}`
  );
}

function blockOf(id: string): string {
  return id.slice(id.lastIndexOf("#b-") + 3);
}

test("a task stands after every task in its needs, wherever the plan writes it", () => {
  // Written as the chain's last task, a free task, then the chain's first two: the walk starts
  // from the chain's last task, which nothing needs, and places the chain above it first to last;
  // then from the free task, which nothing needs either.
  const entries = planOrder([task("t3", ["t2"]), task("t4"), task("t1"), task("t2", ["t1"])]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3", "t4"]);
});

test("a chain written in the order its needs allow is drawn in that order", () => {
  const entries = planOrder([task("t1"), task("t2", ["t1"]), task("t3", ["t2"]), task("t4")]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3", "t4"]);
});

test("a task needed by two tasks stands before both, each of them in the plan's own order", () => {
  const entries = planOrder([task("t2", ["t1"]), task("t3", ["t1"]), task("t1")]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3"]);
});

test("a task stands just above the first task that needs it, so a chain reads down the page", () => {
  // Written a to e. The walk starts from d, the first task nothing needs, and places what it
  // needs above it: c, and above c, b. Then it starts from e, the next task nothing needs, and
  // places a above it. So a, which needs nothing, stands just above e and not at the top.
  const needs: Record<string, readonly string[]> = {
    c: ["b"],
    d: ["c"],
    e: ["a"],
  };
  const order = drawn(planOrder(["a", "b", "c", "d", "e"].map((id) => task(id, needs[id]))));
  expect(order).toEqual(["b", "c", "d", "a", "e"]);
  for (const [later, needed] of Object.entries(needs)) {
    for (const earlier of needed) {
      expect(order.indexOf(earlier), `${later} stands after ${earlier}`).toBeLessThan(
        order.indexOf(later)
      );
    }
  }
  // Each task something needs is the row right before the first task that needs it.
  for (const earlier of new Set(Object.values(needs).flat())) {
    const firstNeeding = Math.min(
      ...Object.entries(needs)
        .filter(([, needed]) => needed.includes(earlier))
        .map(([later]) => order.indexOf(later))
    );
    expect(order.indexOf(earlier) + 1, `${earlier} stands right before what first needs it`).toBe(
      firstNeeding
    );
  }
});

test("tasks that need each other are one cycle, and nothing of theirs is a step", () => {
  const entries = planOrder([task("t1", ["t2"]), task("t2", ["t1"])]);
  expect(drawn(entries)).toEqual(["cycle t1 t2"]);
  expect(entries.every((entry) => entry.kind === "cycle")).toBe(true);
});

test("a cycle of three holds all three, in the plan's own order", () => {
  const entries = planOrder([task("t1", ["t3"]), task("t2", ["t1"]), task("t3", ["t2"])]);
  expect(drawn(entries)).toEqual(["cycle t1 t2 t3"]);
});

test("a task that needs itself is a cycle of one, and no step", () => {
  const entries = planOrder([task("t1", ["t1"]), task("t2")]);
  expect(drawn(entries)).toEqual(["cycle t1", "t2"]);
});

test("what a cycle holds up keeps its place after the cycle, and what it waits on stands before", () => {
  // The first task is needed by the cycle, and the fourth waits on it: the cycle is one place in
  // the order like any other, so the first task stands before it and the fourth after.
  const entries = planOrder([
    task("t4", ["t2"]),
    task("t2", ["t1", "t3"]),
    task("t3", ["t2"]),
    task("t1"),
  ]);
  expect(drawn(entries)).toEqual(["t1", "cycle t2 t3", "t4"]);
});

test("a task whose needs names a task the plan does not hold waits on nothing", () => {
  // The pointer reads, and nothing in this plan answers to it: the task keeps the place the
  // plan gave it, and the task it does need still stands before it.
  const entries = planOrder([task("t2", ["t9"]), task("t3", ["t1", "t9"]), task("t1")]);
  expect(drawn(entries)).toEqual(["t2", "t1", "t3"]);
});

test("a chip that is no needs pointer is no need, whatever it names", () => {
  // The chip a task's own words earn, which names a document and no block of it
  // (`tree-drafts.ts`'s `addDocumentChips`); a chip naming a block of another document whose
  // block id a task of this plan also has; and a chip naming a task of this plan that the
  // task's words cite and its `needs` do not. None of them says "needs", so none is one.
  const cited = task("t2");
  const needing: TreeNode = {
    ...cited,
    chips: [
      chip(`dispatch://${KEY}/artifact/${PLAN}`, "The plan"),
      chip(`dispatch://${KEY}/spec#b-r1`, "A statement"),
      chip(taskRef("t1"), "Task t1"),
    ],
  };
  expect(drawn(planOrder([needing, task("r1"), task("t1")]))).toEqual(["t2", "r1", "t1"]);
});

test("a node that is no plan task needs nothing and holds nothing up", () => {
  const entries = planOrder([task("r1", [], "requirement"), task("t2", ["t1"]), task("t1")]);
  expect(drawn(entries)).toEqual(["r1", "t1", "t2"]);
});

// The filter's view hands the tasks over grouped by the statement each one serves, which is the
// spec's order, and the plan's own order is the one its document writes.
test("tasks given grouped by statement come back in the order the plan writes them", () => {
  const artifacts = new Map<string, Artifact>([
    [PLAN, { id: "artifact-plan", slug: PLAN } as Artifact],
    ["notes-md", { id: "artifact-notes", slug: "notes-md" } as Artifact],
  ]);
  // A task of a document whose blocks read has not answered, which `written` leaves out: the
  // page holds no place for it.
  const unread: TreeNode = { ...task("n1"), id: blockNodeId("notes-md", "n1") };
  const given = [task("t3", ["t1"]), unread, task("t1"), task("t2"), task("t1")];
  const written: ReadonlyMap<string, readonly string[]> = new Map([
    ["artifact-plan", ["t1", "t2", "t3"]],
  ]);
  expect(writtenOrder(given, artifacts, written).map((one) => blockOf(one.id))).toEqual([
    "t1",
    "t1",
    "t2",
    "t3",
    "n1",
  ]);
  // Both documents read: they stand in the order `written` holds them, the notes first.
  const both: ReadonlyMap<string, readonly string[]> = new Map([
    ["artifact-notes", ["n1"]],
    ["artifact-plan", ["t1", "t2", "t3"]],
  ]);
  expect(writtenOrder(given, artifacts, both).map((one) => blockOf(one.id))).toEqual([
    "n1",
    "t1",
    "t1",
    "t2",
    "t3",
  ]);
  // With no document read, every task keeps the place it was given.
  expect(writtenOrder(given, artifacts, new Map()).map((one) => blockOf(one.id))).toEqual([
    "t3",
    "n1",
    "t1",
    "t2",
    "t1",
  ]);
});

test("a plan with no tasks draws nothing", () => {
  expect(planOrder([])).toEqual([]);
});

// The chart of time: one bar per task, starting after every task it needs has ended, and a task
// with no size a short bar marked No size. Today's model holds no size, so each bar is one unit
// long.
test("a bar starts where every task it needs has ended", () => {
  const chart = planChart(
    planOrder([task("t1"), task("t2", ["t1"]), task("t3", ["t1"]), task("t4", ["t2", "t3"])])
  );
  expect(
    chart.rows.map((row) => (row.kind === "bar" ? [blockOf(row.task.id), row.start] : ["cycle"]))
  ).toEqual([
    ["t1", 0],
    ["t2", 1],
    ["t3", 1],
    ["t4", 2],
  ]);
  expect(chart.end).toBe(3);
  // Each bar names the tasks it needs, which is what the lines between them are drawn from.
  expect(
    chart.rows.map((row) => (row.kind === "bar" ? row.needs.map((one) => blockOf(one.id)) : []))
  ).toEqual([[], ["t1"], ["t1"], ["t2", "t3"]]);
  // A pinned and an unpinned pointer to one task (`…#b-<id>@v1`, `…#b-<id>`) name one need: the
  // server refuses a pointer named twice by its raw value alone (`pmdoc/schema.go`), and the
  // chart names each need, and draws its curve, once.
  const twice: TreeNode = {
    ...task("t2"),
    chips: [{ ...need("t1"), ref: `${taskRef("t1")}@v1` }, need("t1")],
  };
  const pinned = planChart(planOrder([task("t1"), twice]));
  expect(
    pinned.rows.map((row) => (row.kind === "bar" ? row.needs.map((one) => blockOf(one.id)) : []))
  ).toEqual([[], ["t1"]]);
});

test("a cycle is one row of the chart at its place, and what waits on it starts after it", () => {
  const chart = planChart(
    planOrder([task("t4", ["t3"]), task("t2", ["t3", "t1"]), task("t3", ["t2"]), task("t1")])
  );
  expect(
    chart.rows.map((row) =>
      row.kind === "bar"
        ? `${blockOf(row.task.id)}@${row.start}`
        : `cycle ${row.tasks.map((one) => blockOf(one.id)).join(" ")}`
    )
  ).toEqual(["t1@0", "cycle t2 t3", "t4@2"]);
  // The step that waits on the cycle names the task of it that it needs.
  const last = chart.rows.at(-1);
  expect(last?.kind === "bar" ? last.needs.map((one) => blockOf(one.id)) : []).toEqual(["t3"]);
});

test("a plan with no tasks is a chart with no rows", () => {
  expect(planChart([])).toEqual({ end: 1, rows: [] });
});
