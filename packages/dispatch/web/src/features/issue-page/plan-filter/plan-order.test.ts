import { expect, test } from "bun:test";

import type { NodeChip, TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS, type NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { needsChip, type PlanEntry, planChart, planOrder, sizeWords } from "./plan-order";

/**
 * The plan's order, drawn from the places the Plan view's read answers.
 *
 * The nodes are built as `issue-tree.ts` builds them from the plan document's blocks: a task's
 * id is `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`), and its `needs` are its chips,
 * each naming its target by that target's own `dispatch://` address
 * (`tree-read/tree-nodes.ts`' `needsChips`). A task carries its place in the order the read
 * answers where the read gives it one (`TreeNode.position`, `presets.ts`' `planFactsOf`); a task
 * of a cycle carries none.
 */

const KEY = "CORE-1";
const PLAN = "plan-md";

/** The address of one block of the plan, as a `needs` pointer holds it. */
function taskRef(blockId: string): string {
  return `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`;
}

function chip(ref: string, name = ""): NodeChip {
  return {
    answered: name !== "",
    gone: false,
    ...(name === "" ? {} : { reading: { words: name } }),
    ref,
  };
}

/** The chip the tree draws for one `needs` pointer of a task's block (`tree-read/tree-nodes.ts`'
 *  `needsChips`): the target's name under the attribute's own word. */
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

/** `one` holding a size of `minutes`, as the Plan view's read answers it (`TreeNode.size`). */
function sized(one: TreeNode, minutes: number): TreeNode {
  return { ...one, size: minutes };
}

/** `one` at `position` in the order the Plan view's read answers (`TreeNode.position`). */
function placed(one: TreeNode, position: number): TreeNode {
  return { ...one, position };
}

/** A plan task of the issue `issue` under this one, by its block id, as this issue's Plan view
 *  draws it: by its block id alone (`tree-read/tree-nodes.ts`' `treeIdOfNode`), marked with its
 *  issue, each `needs` pointer addressed in that issue's own plan. */
function otherTask(issue: string, blockId: string, needs: readonly string[] = []): TreeNode {
  const address = (id: string) => `dispatch://${issue}/artifact/${PLAN}#b-${id}`;
  return {
    ...task(blockId),
    chips: needs.map((id) => ({ ...chip(address(id), id), label: BLOCK_ATTRIBUTE_LABELS.needs })),
    id: blockId,
    issue,
    reference: address(blockId),
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

/** The block id a node of the plan is drawn by: the part of its id after `#b-`, and the whole id
 *  of a task of another issue, which its block id alone is. */
function blockOf(id: string): string {
  const at = id.lastIndexOf("#b-");
  return at === -1 ? id : id.slice(at + 3);
}

/** Where each bar of the chart over `entries` starts, and -1 for a cycle's row. */
function starts(entries: readonly PlanEntry[]): number[] {
  return planChart(entries).rows.map((row) => (row.kind === "bar" ? row.start : -1));
}

// The server's walk places a need just before the first task that needs it and answers that
// order (`refs.PlanOrder`), so for tasks written a, b and c, where c needs a, it places a after b.
// The page draws the order the read answers, whatever order the view hands the tasks in.
test("the read's order of three tasks written a, b and c, where c needs a, is drawn b, a, c", () => {
  const entries = planOrder([
    placed(task("a"), 1),
    placed(task("b"), 0),
    placed(task("c", ["a"]), 2),
  ]);
  expect(drawn(entries)).toEqual(["b", "a", "c"]);
});

// The page holds no walk of its own: for the same needs, where the read answers places a walk
// from the order the tasks are handed in would not give, the page draws the read's.
test("the page draws the read's order where a walk of its own would give another", () => {
  const entries = planOrder([
    placed(task("a"), 0),
    placed(task("b"), 1),
    placed(task("c", ["a"]), 2),
  ]);
  expect(drawn(entries)).toEqual(["a", "b", "c"]);
});

test("the tasks are drawn in the order the read answers, whatever order the view hands them in", () => {
  // Handed as the chain's last task, a free task, then the chain's first two: the read places the
  // chain first to last, then the free task.
  const entries = planOrder([
    placed(task("t3", ["t2"]), 2),
    placed(task("t4"), 3),
    placed(task("t1"), 0),
    placed(task("t2", ["t1"]), 1),
  ]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3", "t4"]);
});

test("tasks that need each other are one cycle, and nothing of theirs is a step", () => {
  const entries = planOrder([task("t1", ["t2"]), task("t2", ["t1"])]);
  expect(drawn(entries)).toEqual(["cycle t1 t2"]);
  expect(entries.every((entry) => entry.kind === "cycle")).toBe(true);
});

test("a cycle of three holds all three, in the order the view hands them over", () => {
  const entries = planOrder([task("t2", ["t1"]), task("t3", ["t2"]), task("t1", ["t3"])]);
  expect(drawn(entries)).toEqual(["cycle t2 t3 t1"]);
});

test("a task that needs itself is a cycle of one, and no step", () => {
  // No task the read gives a place waits on the cycle, so its line stands after every one, as the
  // read sorts every node it gives no place.
  const entries = planOrder([task("t1", ["t1"]), placed(task("t2"), 0)]);
  expect(drawn(entries)).toEqual(["t2", "cycle t1"]);
});

test("what a cycle holds up keeps its place after the cycle, and what it waits on stands before", () => {
  // The read places the first task, which the cycle needs, and the fourth, which waits on the
  // cycle, after it, and gives the cycle's tasks no place. The cycle's line stands just before the
  // first task with a place that waits on it: the first task before it and the fourth after.
  const entries = planOrder([
    placed(task("t4", ["t2"]), 1),
    task("t2", ["t1", "t3"]),
    task("t3", ["t2"]),
    placed(task("t1"), 0),
  ]);
  expect(drawn(entries)).toEqual(["t1", "cycle t2 t3", "t4"]);
});

test("a cycle stands before each cycle that waits on it, and after every task where nothing with a place waits on it", () => {
  // Cycle a waits on t1, cycle b on cycle a, and t5 on cycle b, which the read places after t1;
  // nothing waits on cycle c. The view hands c over first and b before a: a and b stand just
  // before t5, a first, and c after every task, t6 among them.
  const entries = planOrder([
    task("c1", ["c2"]),
    task("c2", ["c1"]),
    placed(task("t1"), 0),
    task("b1", ["b2", "a1"]),
    task("b2", ["b1"]),
    task("a1", ["a2", "t1"]),
    task("a2", ["a1"]),
    placed(task("t5", ["b1"]), 1),
    placed(task("t6"), 2),
  ]);
  expect(drawn(entries)).toEqual(["t1", "cycle a1 a2", "cycle b1 b2", "t5", "t6", "cycle c1 c2"]);
});

test("a task whose needs names a task the plan does not hold waits on nothing", () => {
  // The pointer reads, and nothing in this plan answers to it: the task's bar starts where the
  // tasks it does need have ended, and the bar of the task that needs nothing else at the first
  // unit.
  const entries = planOrder([
    placed(task("t2", ["t9"]), 0),
    placed(task("t3", ["t1", "t9"]), 2),
    placed(task("t1"), 1),
  ]);
  expect(drawn(entries)).toEqual(["t2", "t1", "t3"]);
  expect(starts(entries)).toEqual([0, 0, 1]);
});

test("a chip that is no needs pointer is no need, whatever it names", () => {
  // The chip a task's own words earn, which names a document and no block of it
  // (`tree-drafts.ts`'s `addDocumentChips`); a chip naming a block of another document whose
  // block id a task of this plan also has; and a chip naming a task of this plan that the
  // task's words cite and its `needs` do not. None of them says "needs", so none is one: the
  // task's bar names no need and starts at the first unit, though both tasks stand above it.
  const cited = task("t1");
  const needing: TreeNode = {
    ...placed(task("t2"), 2),
    chips: [
      chip(`dispatch://${KEY}/artifact/${PLAN}`, "The plan"),
      chip(`dispatch://${KEY}/spec#b-r1`, "A statement"),
      chip(taskRef("t1"), "Task t1"),
    ],
  };
  const entries = planOrder([needing, placed(task("r1"), 1), placed(cited, 0)]);
  expect(drawn(entries)).toEqual(["t1", "r1", "t2"]);
  const last = planChart(entries).rows.at(-1);
  expect(last?.kind === "bar" ? [last.start, last.needs] : undefined).toEqual([0, []]);
  expect(needsChip(needing, cited)).toBeUndefined();
});

test("a node that is no plan task needs nothing and holds nothing up", () => {
  // A requirement among the tasks, which the read gives no place, stands after every task that
  // has one. It names t1 as a need and t1 names it, which between two tasks would be a cycle.
  const entries = planOrder([
    task("r1", ["t1"], "requirement"),
    placed(task("t2", ["t1"]), 1),
    placed(task("t1", ["r1"]), 0),
  ]);
  expect(drawn(entries)).toEqual(["t1", "t2", "r1"]);
  expect(starts(entries)).toEqual([0, 1, 0]);
});

// The filter's view hands a task over once for each statement it serves, and the plan holds it
// once: its second statement is its "Also under" chip.
test("a task the view hands over under two statements is one step, at the place the read gives it", () => {
  const twice = placed(task("t1"), 0);
  const entries = planOrder([placed(task("t3", ["t1"]), 2), twice, placed(task("t2"), 1), twice]);
  expect(drawn(entries)).toEqual(["t1", "t2", "t3"]);
});

test("a plan with no tasks draws nothing", () => {
  expect(planOrder([])).toEqual([]);
});

// The chart of time: one bar per task, starting after every task it needs has ended, and a task
// with no size a short bar marked No size. Where no task holds a size each bar is one unit long.
test("a bar starts where every task it needs has ended", () => {
  const chart = planChart(
    planOrder([
      placed(task("t1"), 0),
      placed(task("t2", ["t1"]), 1),
      placed(task("t3", ["t1"]), 2),
      placed(task("t4", ["t2", "t3"]), 3),
    ])
  );
  expect(
    chart.rows.map((row) => (row.kind === "bar" ? [blockOf(row.task.id), row.start] : ["cycle"]))
  ).toEqual([
    ["t1", 0],
    ["t2", 1],
    ["t3", 1],
    ["t4", 2],
  ]);
  expect(chart.axis).toEqual({ numbered: false, span: 3, step: 1 });
  // Each bar names the tasks it needs, which is what the lines between them are drawn from.
  expect(
    chart.rows.map((row) => (row.kind === "bar" ? row.needs.map((one) => blockOf(one.id)) : []))
  ).toEqual([[], ["t1"], ["t1"], ["t2", "t3"]]);
  // A pinned and an unpinned pointer to one task (`…#b-<id>@v1`, `…#b-<id>`) name one need: the
  // server refuses a pointer named twice by its raw value alone (`pmdoc/schema.go`), and the
  // chart names each need, and draws its curve, once.
  const twice: TreeNode = {
    ...placed(task("t2"), 1),
    chips: [{ ...need("t1"), ref: `${taskRef("t1")}@v1` }, need("t1")],
  };
  const pinned = planChart(planOrder([placed(task("t1"), 0), twice]));
  expect(
    pinned.rows.map((row) => (row.kind === "bar" ? row.needs.map((one) => blockOf(one.id)) : []))
  ).toEqual([[], ["t1"]]);
});

test("a cycle is one row of the chart at its place, and what waits on it starts after it", () => {
  const chart = planChart(
    planOrder([
      placed(task("t4", ["t3"]), 1),
      task("t2", ["t3", "t1"]),
      task("t3", ["t2"]),
      placed(task("t1"), 0),
    ])
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
  expect(planChart([])).toEqual({ axis: { numbered: false, span: 1, step: 1 }, rows: [] });
});

// Every size the page draws is written by one function: minutes below an hour, and from an hour
// on whole hours and the minutes left.
test("a size reads in minutes below an hour, and in hours and minutes from there", () => {
  expect([5, 45, 60, 90, 600].map(sizeWords)).toEqual([
    "5 min",
    "45 min",
    "1 h",
    "1 h 30 min",
    "10 h",
  ]);
});

// R26: a bar is as long as its task's size, on one scale for the whole chart, and starts where
// every task it needs has ended, which their sizes decide. A task with no size is one line of the
// axis long, the line's step chosen from the sizes alone (here 60 minutes, so 195 minutes take
// four lines), and it counts as that long for the bars after it. The axis carries the time at
// each line only where every task holds a size.
test("a bar is as long as its task's size and starts where the tasks it needs end", () => {
  const chart = planChart(
    planOrder([
      placed(sized(task("t1"), 90), 0),
      placed(sized(task("t2", ["t1"]), 45), 1),
      placed(task("t3", ["t1"]), 2),
      placed(sized(task("t4", ["t2", "t3"]), 60), 3),
    ])
  );
  expect(
    chart.rows.map((row) =>
      row.kind === "bar" ? [blockOf(row.task.id), row.start, row.length] : ["cycle"]
    )
  ).toEqual([
    ["t1", 0, 90],
    ["t2", 90, 45],
    ["t3", 90, 60],
    ["t4", 150, 60],
  ]);
  expect(chart.axis).toEqual({ numbered: false, span: 240, step: 60 });
  const whole = planChart(
    planOrder([placed(sized(task("t1"), 90), 0), placed(sized(task("t2", ["t1"]), 45), 1)])
  );
  expect(whole.axis).toEqual({ numbered: true, span: 150, step: 30 });
});

// R19 and R26: on an issue with issues under it the read answers their tasks beside the issue's
// own, each at its place in the one order of the whole tree, and the page draws them all in it.
// Here each issue writes `a`, `b`, `c` and `c` needs `a`: the read places `a` just before `c`, on
// the child's own page and inside the parent's plan, where the child's tasks stand after the
// parent's own. The places leave room for each issue's start and done, which the read places too
// and the view hands over as no task.
test("a parent's own tasks and its children's stand in the order the read answers, each chain as on its own page", () => {
  const own = [placed(task("a"), 2), placed(task("b"), 1), placed(task("c", ["a"]), 3)];
  expect(drawn(planOrder(own))).toEqual(["b", "a", "c"]);
  const childsPage = [placed(task("xa"), 2), placed(task("xb"), 1), placed(task("xc", ["xa"]), 3)];
  expect(drawn(planOrder(childsPage))).toEqual(["xb", "xa", "xc"]);
  const parent = [
    ...own,
    placed(otherTask("CORE-2", "xa"), 6),
    placed(otherTask("CORE-2", "xb"), 5),
    placed(otherTask("CORE-2", "xc", ["xa"]), 7),
  ];
  expect(drawn(planOrder(parent))).toEqual(["b", "a", "c", "xb", "xa", "xc"]);
  // A task of another issue starts where the task of its own issue it needs ends.
  expect(starts(planOrder(parent))).toEqual([0, 0, 1, 0, 0, 1]);
});
