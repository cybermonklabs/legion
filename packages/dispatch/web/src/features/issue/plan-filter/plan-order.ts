import { parseNodeReference } from "../../refs/routes";
import type { TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS } from "../page-words";

/**
 * The plan's tasks as ordered steps (dispatch://INFRA-1196 R26).
 *
 * A step stands after every task in its `needs`. The order is stable: where the `needs` leave
 * two tasks free to stand in either order, the plan's own order decides, so a plan already
 * written in an order its `needs` allow is drawn in that order and a reader who knows the plan
 * document recognises it.
 *
 * Tasks that need each other, directly or through the rest, are one cycle: no order can put any
 * of them after every task it needs, so they are one group, which the page draws as one line
 * that lists them (R26). Everything that waits on a cycle keeps its place after it.
 *
 * Today's reads hold no position of their own. `needs` is a `block[]` attribute of the plan's
 * own blocks (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:85-89`), which the
 * tree draws as a chip on the task's row (`tree-drafts.ts`'s `placeBlocks`), and the position
 * the plan read gives each task (R26, dispatch://INFRA-1253 R11) waits on that read. So the
 * order is worked out here, from the edges the model does hold, and a task whose `needs` names a
 * task this plan does not hold waits on nothing: its row says the target is gone or names an
 * address nothing answers to (`NodeRow.tsx`'s `Chip`), and the order is the plan's own.
 *
 * The module is pure: it holds no React, reads no model of its own and makes no request.
 */

/** One step of the plan: a task the order puts after every task in its `needs`. */
export interface PlanStep {
  readonly kind: "step";
  readonly task: TreeNode;
}

/** The tasks of one cycle, in the plan's own order. The page draws them as one line that lists
 *  them, and none of them is a step, because no step's place can be given to any of them. */
export interface PlanCycle {
  readonly kind: "cycle";
  readonly tasks: readonly TreeNode[];
}

/** One place in the plan's order: a step, or a cycle the page draws as one line. */
export type PlanEntry = PlanCycle | PlanStep;

/** The document part of a block node's id, `<slug>#b-<block id>` (`tree-drafts.ts`'s
 *  `blockNodeId`), and `""` for a node that is no block of a document. */
function documentOf(id: string): string {
  const at = id.lastIndexOf("#b-");
  return at === -1 ? "" : id.slice(0, at);
}

/**
 * The ids of the nodes `task` needs, as this issue addresses them: one per `needs` pointer of
 * its block. The tree draws each as a chip that says it is a need (`tree-drafts.ts`'s
 * `placeBlocks`, `NodeChip.label`), which is what tells it from a chip for anything else the
 * task names - a document its words cite, a statement it is also under - so a chip naming a
 * block of another document is no need, whatever that block's id is.
 *
 * `needs` points at a plan task of the task's own document (`blocks.json`: `targets:
 * ["plan_task"]`, `scope: "document"`), so the block id the reference carries is looked up in
 * `task`'s own document. The reference's own document part is not read, because the issue's
 * primary document is spelled `spec` in a reference and by its slug in a node's id, and one
 * block id is unique inside one document alone.
 */
function neededIds(task: TreeNode): readonly string[] {
  if (task.type !== "plan_task") return [];
  return task.chips.flatMap((chip) => {
    if (chip.label !== BLOCK_ATTRIBUTE_LABELS.needs) return [];
    const route = parseNodeReference(chip.ref);
    const block = route !== undefined && "block" in route ? route.block : undefined;
    return block === undefined ? [] : [`${documentOf(task.id)}#b-${block.id}`];
  });
}

/** Puts `group` into `ready`, which is kept in the plan's own order: the next group taken is the
 *  one holding the earliest task, so two groups free to stand in either order keep that order. */
function readyFor(ready: number[], group: number): void {
  const at = ready.findIndex((held) => held > group);
  ready.splice(at === -1 ? ready.length : at, 0, group);
}

/**
 * `nodes` as the plan's order draws them: each a step of its own, or, for tasks that need each
 * other, one cycle holding them all. `nodes` are the rows the Plan filter drew, in the plan's
 * own order, and a node that is no plan task needs nothing and is needed by nothing, so it keeps
 * the place the plan gave it. A task the filter drew under two statements is one task here, at
 * its first place: a plan holds each task once, and its second statement is its "Also under"
 * chip (the tester's F19: task 1 was drawn twice).
 */
export function planOrder(drawn: readonly TreeNode[]): readonly PlanEntry[] {
  const seen = new Set<string>();
  const nodes = drawn.filter((node) => {
    if (seen.has(node.id)) return false;
    seen.add(node.id);
    return true;
  });
  const at = new Map(nodes.map((node, index) => [node.id, index]));
  // What each node needs, by index. A need naming a task this plan does not hold is no edge,
  // and puts the task under no order.
  const needs = nodes.map((node) =>
    neededIds(node).flatMap((needed) => {
      const index = at.get(needed);
      return index === undefined || nodes[index]?.type !== "plan_task" ? [] : [index];
    })
  );
  // Every task each task needs, through the rest as well as directly, which is what tells a
  // cycle from a chain: a task is in a cycle with every task it needs that needs it back.
  const reaches = nodes.map((_node, from) => {
    const found = new Set<number>();
    const walking = [...(needs[from] ?? [])];
    while (walking.length > 0) {
      const next = walking.pop();
      if (next === undefined || found.has(next)) continue;
      found.add(next);
      walking.push(...(needs[next] ?? []));
    }
    return found;
  });
  // The group each task is in, named by the earliest task in it: a task on its own, or every
  // task of one cycle, which all answer to the same name because needing each other both ways
  // holds between every pair of them.
  const groupOf = nodes.map((_node, index) => {
    let earliest = index;
    for (const other of reaches[index] ?? []) {
      if (other < earliest && reaches[other]?.has(index) === true) earliest = other;
    }
    return earliest;
  });
  const members = new Map<number, number[]>();
  groupOf.forEach((group, index) => {
    const held = members.get(group);
    if (held === undefined) members.set(group, [index]);
    else held.push(index);
  });
  // Which groups each group waits on, and which wait on it. An edge inside one group is no
  // wait: that is what being one group means.
  const waitsOn = new Map<number, Set<number>>();
  const holdsUp = new Map<number, Set<number>>();
  needs.forEach((needed, index) => {
    const group = groupOf[index] ?? index;
    for (const one of needed) {
      const before = groupOf[one] ?? one;
      if (before === group) continue;
      const waiting = waitsOn.get(group) ?? new Set<number>();
      waiting.add(before);
      waitsOn.set(group, waiting);
      const holding = holdsUp.get(before) ?? new Set<number>();
      holding.add(group);
      holdsUp.set(before, holding);
    }
  });
  const ready = [...members.keys()]
    .filter((group) => (waitsOn.get(group)?.size ?? 0) === 0)
    .sort((one, other) => one - other);
  const entries: PlanEntry[] = [];
  while (ready.length > 0) {
    const group = ready.shift();
    if (group === undefined) break;
    const tasks = (members.get(group) ?? []).flatMap((index) => {
      const node = nodes[index];
      return node === undefined ? [] : [node];
    });
    const alone = tasks.length === 1 && (needs[group] ?? []).every((one) => one !== group);
    const first = tasks[0];
    if (alone && first !== undefined) entries.push({ kind: "step", task: first });
    else entries.push({ kind: "cycle", tasks });
    for (const next of holdsUp.get(group) ?? []) {
      const waiting = waitsOn.get(next);
      if (waiting === undefined) continue;
      waiting.delete(group);
      if (waiting.size === 0) readyFor(ready, next);
    }
  }
  return entries;
}

/** One bar of the chart of time: one step and the unit it starts at. Every bar is one unit
 *  long, because today's model holds no size for a task (R26: "a task with no size is a short
 *  bar marked No size"); the length joins this once dispatch://INFRA-1253 gives the size. */
export interface PlanBar {
  readonly kind: "bar";
  /** The tasks drawn above it that it needs, steps and tasks of a cycle alike. */
  readonly needs: readonly TreeNode[];
  /** Where it starts: after every task it needs has ended (R26). */
  readonly start: number;
  readonly task: TreeNode;
}

/** One cycle on the chart: the tasks that need each other, as one line at the place the order
 *  gives them. */
export interface PlanKnot {
  readonly kind: "cycle";
  readonly tasks: readonly TreeNode[];
}

/** One row of the chart, in the plan's order. */
export type PlanChartRow = PlanBar | PlanKnot;

/** The chart of time, as `planChart` answers it. */
export interface PlanChart {
  /** Where the last bar ends, which is how many units the chart's axis spans. */
  readonly end: number;
  readonly rows: readonly PlanChartRow[];
}

/**
 * The chart of time over the plan's order (R26): one bar per step, starting after every task it
 * needs has ended, and one line per cycle at its place in the order.
 *
 * Where a bar starts is worked out here from the `needs` the plan holds, since today's reads
 * give no position (dispatch://INFRA-1253 R8 and R11), and every bar is one unit long, since
 * they give no size. A cycle takes one unit after whatever its tasks need from outside it,
 * since the order cannot say more of tasks that need each other.
 */
export function planChart(entries: readonly PlanEntry[]): PlanChart {
  const ends = new Map<string, number>();
  const tasks = new Map<string, TreeNode>();
  const endOf = (ids: readonly string[]): number =>
    Math.max(0, ...ids.map((id) => ends.get(id) ?? 0));
  const rows = entries.map((entry): PlanChartRow => {
    if (entry.kind === "cycle") {
      const inside = new Set(entry.tasks.map((task) => task.id));
      const start = endOf(
        entry.tasks.flatMap((task) => neededIds(task).filter((id) => !inside.has(id)))
      );
      for (const task of entry.tasks) {
        ends.set(task.id, start + 1);
        tasks.set(task.id, task);
      }
      return { kind: "cycle", tasks: entry.tasks };
    }
    const needs = neededIds(entry.task).flatMap((id) => {
      const needed = tasks.get(id);
      return needed === undefined ? [] : [needed];
    });
    const start = endOf(needs.map((needed) => needed.id));
    ends.set(entry.task.id, start + 1);
    tasks.set(entry.task.id, entry.task);
    return { kind: "bar", needs, start, task: entry.task };
  });
  return { end: Math.max(1, ...ends.values()), rows };
}
