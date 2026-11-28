import type { Artifact } from "../../../api/types";
import { parseNodeReference } from "../../refs/routes";
import { documentOfNode } from "../issue-artifacts";
import type { NodeChip, TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS } from "../page-words";
import { blockNodeId } from "../statement-rules";

/**
 * The plan's tasks as ordered steps.
 *
 * A step stands after every task in its `needs`. The order is a walk over the `needs`: it starts
 * from each task no other task needs, in the plan's own order, and places a task by first placing
 * each task it needs, the same way and in the plan's own order, and then the task itself; a task
 * placed already keeps its place. So each task something needs stands above the first task that
 * needs it, with nothing between them but that task's other needs and what they need in turn,
 * and a chain reads down the page: a reader sees the step that follows a step next to it rather
 * than far down the chart, and a task that needs nothing stands just above the first task that
 * needs it, not at the top. Where the `needs` leave a choice the plan's own order makes it, so a
 * reader who knows the plan document recognises it.
 *
 * Tasks that need each other, directly or through the rest, are one cycle: no order can put any
 * of them after every task it needs, so they are one group, which the page draws as one line
 * that lists them. Everything that waits on a cycle keeps its place after it.
 *
 * Today's reads hold no position of their own. `needs` is a `block[]` attribute of the plan's
 * own blocks (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:123-127`), which the
 * tree draws as a chip on the task's row (`tree-drafts.ts`'s `placeBlocks`), and no read gives a
 * task its position. So the order is worked out here, from the edges the model does hold, and a
 * task whose `needs` names a task this plan does not hold waits on nothing: its row says the
 * target is gone or names an address nothing answers to (`NodeRow.tsx`'s `Chip`), and the order
 * is the plan's own.
 *
 * The plan's own order is the order its tasks are written in (`writtenOrder`), read off the
 * blocks the page already holds. The module is pure: it holds no React, reads no model of its
 * own and makes no request.
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
 * The ids of the nodes `task` needs, as this issue addresses them: one per task its block's
 * `needs` pointers name, each once. The tree draws each pointer as a chip that says it is a need
 * (`tree-drafts.ts`'s `placeBlocks`, `NodeChip.label`), which is what tells it from a chip for
 * anything else the task names - a document its words cite, a statement it is also under - so a
 * chip naming a block of another document is no need, whatever that block's id is.
 *
 * `needs` points at a plan task of the task's own document (`blocks.json`: `targets:
 * ["plan_task"]`, `scope: "document"`), so the block id the reference carries is looked up in
 * `task`'s own document. The reference's own document part is not read, because the issue's
 * primary document is spelled `spec` in a reference and by its slug in a node's id, and one
 * block id is unique inside one document alone. Nor is its pin: the server refuses a pointer
 * named twice by its raw value alone (`pmdoc/schema.go`), so `…#b-<id>@v1` and `…#b-<id>` can
 * stand in one `needs`, and they name one task, which the chart then names and joins by one
 * curve once.
 */
function neededIds(task: TreeNode): readonly string[] {
  return [
    ...new Set(
      task.chips.flatMap((chip) => {
        const id = neededId(task, chip);
        return id === undefined ? [] : [id];
      })
    ),
  ];
}

/** The id of the node one chip of `task` names as a need (`neededIds`, `needsChip`), or
 *  undefined for a chip that is no `needs` pointer. */
function neededId(task: TreeNode, chip: NodeChip): string | undefined {
  if (task.type !== "plan_task" || chip.label !== BLOCK_ATTRIBUTE_LABELS.needs) return undefined;
  const route = parseNodeReference(chip.ref);
  const block = route !== undefined && "block" in route ? route.block : undefined;
  return block === undefined ? undefined : blockNodeId(documentOf(task.id), block.id);
}

/**
 * The documents the plan's tasks are blocks of (`issue-artifacts.ts`'s `documentOfNode`, which
 * reads a block node's id against the issue's artifacts by slug), each once, in the order of its
 * first task in `tasks`: the documents whose block lists give the plan's own order
 * (`writtenOrder`). A node that is no block of a document the issue holds names none.
 */
export function taskDocumentIds(
  tasks: readonly TreeNode[],
  artifacts: ReadonlyMap<string, Artifact>
): readonly string[] {
  return [
    ...new Set(
      tasks.flatMap((task) => {
        const held = documentOfNode(task.id, artifacts);
        return held === undefined ? [] : [held.artifact.id];
      })
    ),
  ];
}

/**
 * `tasks` in the order the plan is written in, which is the plan's own order: the order
 * `planOrder` follows wherever the `needs` leave it a choice. The Plan filter's view hands the
 * tasks over grouped by the statement each one serves, which is the spec's order and not the
 * plan's, so the view hands them to `planOrder` through this.
 *
 * `written` holds the block ids of each document the tasks are blocks of, by the document's id,
 * in the order the document writes them, as the page's blocks read holds them (`useIssueTree.ts`
 * reads them under the same key, so the view sends no request of its own). The documents stand
 * in the map's own order, and a document whose read has not answered is not in it: a task the
 * page holds no place for - its document unread or not the issue's, its block not in the list -
 * keeps the place `tasks` gives it, after the rest.
 */
export function writtenOrder(
  tasks: readonly TreeNode[],
  artifacts: ReadonlyMap<string, Artifact>,
  written: ReadonlyMap<string, readonly string[]>
): readonly TreeNode[] {
  const places = new Map<string, number>();
  let place = 0;
  for (const [id, blocks] of written) {
    for (const block of blocks) {
      places.set(`${id} ${block}`, place);
      place += 1;
    }
  }
  const placeOf = (task: TreeNode): number => {
    const held = documentOfNode(task.id, artifacts);
    const at = held === undefined ? undefined : places.get(`${held.artifact.id} ${held.blockId}`);
    return at ?? Number.POSITIVE_INFINITY;
  };
  return tasks
    .map((task, index) => ({ index, place: placeOf(task), task }))
    .sort((one, other) =>
      one.place === other.place ? one.index - other.index : one.place - other.place
    )
    .map((held) => held.task);
}

/**
 * `nodes` as the plan's order draws them: each a step of its own, or, for tasks that need each
 * other, one cycle holding them all. `nodes` are the rows the Plan filter draws with no words
 * filter on, in the plan's own order (`writtenOrder`): the whole plan, since a words filter
 * chooses which places the view draws and not where they stand. A node that is no plan task
 * needs nothing and is needed by nothing, so the walk starts from it at its place in the plan's
 * own order; the Plan view hands over none, since it keeps the address's node only where it is a
 * task (`slots/view-plan.tsx`). A task the filter drew under two statements is one task here, at
 * its first place: a plan holds each task once, and its second statement is its "Also under" chip,
 * so the task is not drawn twice.
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
  // Which groups each group waits on, and every group some other group waits on. An edge inside
  // one group is no wait: that is what being one group means.
  const waitsOn = new Map<number, Set<number>>();
  const waitedOn = new Set<number>();
  needs.forEach((needed, index) => {
    const group = groupOf[index] ?? index;
    for (const one of needed) {
      const before = groupOf[one] ?? one;
      if (before === group) continue;
      const waiting = waitsOn.get(group) ?? new Set<number>();
      waiting.add(before);
      waitsOn.set(group, waiting);
      waitedOn.add(before);
    }
  });
  // The order is a depth-first walk that places the groups a group needs, each the same way,
  // before the group itself, so a need stands above the first group that needs it, past only
  // that group's other needs, and a chain reads down the page. It starts from each group no other
  // group needs, in the plan's own order, and takes the groups one needs in that order too. A
  // group placed already keeps its place. Every group is reached from a start, since groups never
  // wait on each other in a circle: the tasks of such a circle would be one group.
  const entries: PlanEntry[] = [];
  const placed = new Set<number>();
  const place = (group: number): void => {
    if (placed.has(group)) return;
    placed.add(group);
    for (const before of [...(waitsOn.get(group) ?? [])].sort((one, other) => one - other)) {
      place(before);
    }
    const tasks = (members.get(group) ?? []).flatMap((index) => {
      const node = nodes[index];
      return node === undefined ? [] : [node];
    });
    const alone = tasks.length === 1 && (needs[group] ?? []).every((one) => one !== group);
    const first = tasks[0];
    if (alone && first !== undefined) entries.push({ kind: "step", task: first });
    else entries.push({ kind: "cycle", tasks });
  };
  for (const group of [...members.keys()].sort((one, other) => one - other)) {
    if (!waitedOn.has(group)) place(group);
  }
  return entries;
}

/** One bar of the chart of time: one step and the unit it starts at. Every bar is one unit
 *  long, because the model holds no size for a task, and a task with no size is a short bar
 *  marked No size. */
export interface PlanBar {
  readonly kind: "bar";
  /** The tasks drawn above it that it needs, steps and tasks of a cycle alike. */
  readonly needs: readonly TreeNode[];
  /** Where it starts: after every task it needs has ended. */
  readonly start: number;
  readonly task: TreeNode;
}

/** One row of the chart, in the plan's order. */
export type PlanChartRow = PlanBar | PlanCycle;

/** The chart of time, as `planChart` answers it. */
export interface PlanChart {
  /** Where the last bar ends, which is how many units the chart's axis spans. */
  readonly end: number;
  readonly rows: readonly PlanChartRow[];
}

/**
 * The chart of time over the plan's order: one bar per step, starting after every task it
 * needs has ended, and one line per cycle at its place in the order.
 *
 * Where a bar starts is worked out here from the `needs` the plan holds, since the reads give
 * no position, and every bar is one unit long, since they give no size. A cycle takes one unit
 * after whatever its tasks need from outside it, since the order cannot say more of tasks that
 * need each other.
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
      return entry;
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

/**
 * The chip of `task` that names `needed` as a need: the first `needs` pointer of `task` that
 * names it (`neededId`), or undefined where no chip of `task` points at `needed`.
 *
 * The chart names each need by this chip's own name, as the tree names it (`buildIssueTree`):
 * the needed task's first line, and the page's word for its kind where that task holds no
 * words. The steps' rows draw the same chip through `NodeRow.tsx`'s `Chip`, so the switch
 * changes the drawing alone and no edge is named twice: the view gives the rows no chips
 * renamed by the chart's line for the task, which would be a second naming of what the tree
 * already names.
 */
export function needsChip(task: TreeNode, needed: TreeNode): NodeChip | undefined {
  return task.chips.find((chip) => neededId(task, chip) === needed.id);
}
