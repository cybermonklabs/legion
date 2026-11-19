import { expect, test } from "bun:test";

import type { NodeChip, TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS, type NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { type PlanEntry, planChart, planOrder } from "./plan-order";

/**
 * The plan's order, read off the edges today's model holds (dispatch://INFRA-1196 R26).
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

test("a task stands after every task in its needs, and the plan's own order decides the rest", () => {
  // Written in the document as t3, t1, t2: t3 needs t2, t2 needs t1, and t4 needs nothing, so
  // the chain is drawn as a chain and t4 keeps the place the plan gave it.
  const entries = planOrder([task("t3", ["t2"]), task("t4"), task("t1"), task("t2", ["t1"])]);
  expect(drawn(entries)).toEqual(["t4", "t1", "t2", "t3"]);
});

test("a plan whose order its needs allow is drawn in the plan's own order", () => {
  const entries = planOrder([task("t1"), task("t2", ["t1"]), task("t3", ["t2"]), task("t4")]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3", "t4"]);
});

test("a task needed by two tasks stands before both, each of them in the plan's own order", () => {
  const entries = planOrder([task("t2", ["t1"]), task("t3", ["t1"]), task("t1")]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3"]);
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
  // t1 is needed by the cycle, and t4 waits on it: the cycle is one place in the order like any
  // other, so t1 stands before it and t4 after.
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

test("a node that is no plan task keeps its place and holds nothing up", () => {
  const entries = planOrder([task("r1", [], "requirement"), task("t2", ["t1"]), task("t1")]);
  expect(drawn(entries)).toEqual(["r1", "t1", "t2"]);
});

test("a plan with no tasks draws nothing", () => {
  expect(planOrder([])).toEqual([]);
});

// R26's chart of time: "one bar per task, starting after every task it needs has ended ... a
// task with no size is a short bar marked No size". Today's model holds no size, so each bar is
// one unit long.
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
