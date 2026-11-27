import { parseNodeReference } from "../../refs/routes";
import type { NodeChip, TreeNode } from "../issue-tree";
import { BLOCK_ATTRIBUTE_LABELS, PLAN_WORDS } from "../page-words";
import { blockNodeId } from "../tree-drafts";

/**
 * The plan's tasks as ordered steps.
 *
 * The order is the one the Plan view's read answers: each task's place in it
 * (`TreeNode.position`, the read's `plan.position`). The server orders the whole tree of the
 * issue, each task after every task it needs that it gives a place, so the page draws the tasks in
 * that order and holds no rule of its own for it. On an issue with issues under it the read
 * answers the tasks of every issue under it beside the issue's own, each marked with its issue
 * (`TreeNode.issue`), each at its place in the same one order.
 *
 * Tasks that need each other, directly or through the rest, are one cycle: no order can put any
 * of them after every task it needs, so the read gives none of them a place, and they are one
 * group, which the page draws as one line that lists them. Which tasks are one cycle is worked out
 * here, from the `needs` each task's chips name. The line stands just before the first task with a
 * place that waits on the cycle, directly or through another cycle, and after every task with a
 * place where none does, as the read sorts every node it gives no place after every node it
 * places. The read places each task that waits on a cycle after everything the cycle waits on, so
 * what a cycle holds up stands after its line and what it waits on stands before.
 *
 * A task the read gives no place and that is in no cycle - one it leaves out of its order, as it
 * does an overtaken task - stands after every task with a place, in the order the view hands it
 * over, as the read sorts it too.
 *
 * `needs` is a `block[]` attribute of the plan's own blocks
 * (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json`), which the tree draws as a chip
 * on the task's row (`tree-read/tree-nodes.ts`' `needsChips`). A task whose `needs` names a task
 * this plan does not hold waits on nothing here: its row says the target is gone or names an
 * address nothing answers to (`NodeRow.tsx`'s `Chip`).
 *
 * The module is pure: it holds no React, reads no model of its own and makes no request.
 */

/** One step of the plan: a task the order puts after every task in its `needs`. */
export interface PlanStep {
  readonly kind: "step";
  readonly task: TreeNode;
}

/** The tasks of one cycle, in the order the view hands them over. The page draws them as one line
 *  that lists them, and none of them is a step, because no step's place can be given to any of
 *  them. */
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
 * (`tree-read/tree-nodes.ts`' `needsChips`, `NodeChip.label`), which is what tells it from a chip
 * for anything else the task names - a document its words cite, a statement it is also under - so
 * a chip naming a block of another document is no need, whatever that block's id is.
 *
 * `needs` points at a plan task of the task's own document (`blocks.json`: `targets:
 * ["plan_task"]`, `scope: "document"`), so the block id the reference carries is looked up in
 * `task`'s own document. The reference's own document part is not read, because the issue's
 * primary document is spelled `spec` in a reference and by its slug in a node's id, and one
 * block id is unique inside one document alone. Nor is its pin: the server refuses a pointer
 * named twice by its raw value alone (`pmdoc/schema.go`), so `…#b-<id>@v1` and `…#b-<id>` can
 * stand in one `needs`, and they name one task, which the chart then names and joins by one
 * curve once.
 *
 * A task of another issue (`TreeNode.issue`), which the Plan view of an issue above it draws, is
 * drawn by its block id alone, as every node of another issue is (`tree-read/tree-nodes.ts`'
 * `treeIdOfNode`), so a pointer of it names the task of that block id where the reference is of
 * the task's own issue, and nothing where it names another.
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
  if (block === undefined) return undefined;
  if (task.issue === undefined) return blockNodeId(documentOf(task.id), block.id);
  return route !== undefined && "key" in route && route.key === task.issue ? block.id : undefined;
}

/**
 * `drawn` as the plan's order draws it: each task a step of its own at the place the read gives
 * it, and the tasks that need each other one cycle, at the place its line stands. `drawn` are the
 * rows the Plan filter draws with no words filter on, in the order the view hands them over: the
 * whole plan, since a words filter chooses which places the view draws and not where they stand.
 * A node that is no plan task needs nothing and is needed by nothing; the Plan view hands over
 * none, since it keeps the address's node only where it is a task (`slots/view-plan.tsx`). A task
 * the filter drew under two statements is one task here, at its first place: a plan holds each
 * task once, and its second statement is its "Also under" chip, so the task is not drawn twice.
 */
export function planOrder(drawn: readonly TreeNode[]): readonly PlanEntry[] {
  const seen = new Set<string>();
  const nodes = drawn.filter((node) => {
    if (seen.has(node.id)) return false;
    seen.add(node.id);
    return true;
  });
  const at = new Map(nodes.map((node, index) => [node.id, index]));
  // What each node needs, by index. A need naming a task this plan does not hold is no edge.
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
  // Which groups wait on each group. An edge inside one group is no wait: that is what being one
  // group means.
  const waitedOnBy = new Map<number, Set<number>>();
  needs.forEach((needed, index) => {
    const group = groupOf[index] ?? index;
    for (const one of needed) {
      const before = groupOf[one] ?? one;
      if (before === group) continue;
      const waiting = waitedOnBy.get(before) ?? new Set<number>();
      waiting.add(group);
      waitedOnBy.set(before, waiting);
    }
  });
  const entries = new Map<number, PlanEntry>();
  for (const [group, indexes] of members) {
    const tasks = indexes.flatMap((index) => {
      const node = nodes[index];
      return node === undefined ? [] : [node];
    });
    const alone = tasks.length === 1 && (needs[group] ?? []).every((one) => one !== group);
    const first = tasks[0];
    entries.set(
      group,
      alone && first !== undefined ? { kind: "step", task: first } : { kind: "cycle", tasks }
    );
  }
  // Where each group stands: a place in the read's order, then a rank among the groups at that
  // place. A step stands at its task's place, or after every place where its task holds none. A
  // cycle stands just before the first task with a place that waits on it, directly or through
  // another cycle, or after every place where none does; it ranks before that task and before
  // each cycle that waits on it, one rank lower than the lowest of those cycles. Groups never wait
  // on each other in a circle: the tasks of such a circle would be one group.
  const places = new Map<number, readonly [number, number]>();
  const placeOf = (group: number): readonly [number, number] => {
    const held = places.get(group);
    if (held !== undefined) return held;
    const entry = entries.get(group);
    let place: readonly [number, number];
    if (entry?.kind === "step") {
      place = [entry.task.position ?? Number.POSITIVE_INFINITY, 0];
    } else {
      let before = Number.POSITIVE_INFINITY;
      let rank = -1;
      for (const after of waitedOnBy.get(group) ?? []) {
        const waiting = entries.get(after);
        if (waiting?.kind === "step") {
          before = Math.min(before, waiting.task.position ?? Number.POSITIVE_INFINITY);
          continue;
        }
        const [otherBefore, otherRank] = placeOf(after);
        before = Math.min(before, otherBefore);
        rank = Math.min(rank, otherRank - 1);
      }
      place = [before, rank];
    }
    places.set(group, place);
    return place;
  };
  // Groups of one place and rank stand in the order the view hands them over.
  return [...entries.keys()]
    .sort((one, other) => {
      const [place, rank] = placeOf(one);
      const [otherPlace, otherRank] = placeOf(other);
      return (place === otherPlace ? 0 : place - otherPlace) || rank - otherRank || one - other;
    })
    .flatMap((group) => {
      const entry = entries.get(group);
      return entry === undefined ? [] : [entry];
    });
}

/**
 * A size in the words a person reads it in: minutes below an hour ("45 min"), and from an hour
 * on whole hours and the minutes left ("1 h", "1 h 30 min", "10 h"). `minutes` is a whole number
 * of minutes, as the read answers a task's size (`TreeNode.size`). Every size the page draws, a
 * step's and a line of the chart's axis, is written by this one function.
 */
export function sizeWords(minutes: number): string {
  if (minutes < 60) return `${minutes} ${PLAN_WORDS.minutes}`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest === 0
    ? `${hours} ${PLAN_WORDS.hours}`
    : `${hours} ${PLAN_WORDS.hours} ${rest} ${PLAN_WORDS.minutes}`;
}

/** One bar of the chart of time: one step, the unit it starts at and how many units long it is:
 *  its task's size where the task holds one, and one unit of the axis (`PlanAxis.step`) for a
 *  task with no size, which is a short bar marked No size. */
export interface PlanBar {
  readonly kind: "bar";
  /** How many of the chart's units it is long. */
  readonly length: number;
  /** The tasks drawn above it that it needs, steps and tasks of a cycle alike. */
  readonly needs: readonly TreeNode[];
  /** Where it starts: after every task it needs has ended. */
  readonly start: number;
  readonly task: TreeNode;
}

/** One row of the chart, in the plan's order. */
export type PlanChartRow = PlanBar | PlanCycle;

/** The chart's one scale, which its axis draws as lines. Where a task of the chart holds a
 *  size, a unit is a minute and the lines stand a whole number of minutes apart; where none
 *  does, a unit is one task, as long as each bar is, and a line stands at each. */
export interface PlanAxis {
  /** Whether each line carries the time it stands at (`sizeWords`): only where every task of
   *  the chart holds a size, since a line past a task with no size would put a number on a
   *  length nobody wrote. */
  readonly numbered: boolean;
  /** How many units the chart's width spans: where the last bar ends, and with sizes that
   *  rounded up to the next line, so the last line stands at the right edge. */
  readonly span: number;
  /** How many units stand between two lines, which is also the one unit a task with no size,
   *  and a cycle, is drawn as. */
  readonly step: number;
}

/** The chart of time, as `planChart` answers it. */
export interface PlanChart {
  readonly axis: PlanAxis;
  readonly rows: readonly PlanChartRow[];
}

/** The most lines the axis draws past its first where the tasks hold sizes, so the time each
 *  carries has room beside the next at the narrowest column the chart draws an axis in. */
const AXIS_LINES = 6;

/** The minutes two lines of a sized axis may stand apart, from the shortest up; past the last,
 *  each step is twice the one before it. */
const AXIS_STEPS = [5, 10, 15, 30, 60, 120, 180, 240, 480, 720, 1440];

/** The shortest step of `AXIS_STEPS` (or past it) that spans `minutes` in `AXIS_LINES` lines. */
function axisStep(minutes: number): number {
  const fits = (step: number): boolean => Math.ceil(minutes / step) <= AXIS_LINES;
  let step = AXIS_STEPS.find(fits) ?? AXIS_STEPS.at(-1) ?? 1440;
  while (!fits(step)) step *= 2;
  return step;
}

/**
 * The chart's rows over `entries`: each step's bar, starting where every task it needs has ended
 * and as long as `lengthOf` its task, and each cycle `cycleLength` long after whatever its tasks
 * need from outside it, since the order cannot say more of tasks that need each other.
 */
function placeBars(
  entries: readonly PlanEntry[],
  lengthOf: (task: TreeNode) => number,
  cycleLength: number
): { readonly end: number; readonly rows: readonly PlanChartRow[] } {
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
        ends.set(task.id, start + cycleLength);
        tasks.set(task.id, task);
      }
      return entry;
    }
    const needs = neededIds(entry.task).flatMap((id) => {
      const needed = tasks.get(id);
      return needed === undefined ? [] : [needed];
    });
    const start = endOf(needs.map((needed) => needed.id));
    const length = lengthOf(entry.task);
    ends.set(entry.task.id, start + length);
    tasks.set(entry.task.id, entry.task);
    return { kind: "bar", length, needs, start, task: entry.task };
  });
  return { end: Math.max(0, ...ends.values()), rows };
}

/**
 * The chart of time over the plan's order: one bar per step, starting after every task it
 * needs has ended, and one line per cycle at its place in the order.
 *
 * Where a bar starts is worked out here from the `needs` its task's chips name (`neededIds`), and
 * how long it is from its task's size (`TreeNode.size`), on one scale for the whole chart
 * (`PlanAxis`). Where no task holds a size, a unit is one task and every bar is one unit long, as
 * is every cycle. Where any task holds one, a unit is a minute: a bar is as long as its task's
 * size, and a task with no size and a cycle are each one line of the axis long, the line's step
 * chosen from the sizes alone (`axisStep`) so the tasks with no size do not stretch it; each
 * counts as that long for the bars after it.
 */
export function planChart(entries: readonly PlanEntry[]): PlanChart {
  const all = entries.flatMap((entry) => (entry.kind === "step" ? [entry.task] : entry.tasks));
  const sized = all.filter((task) => task.size !== undefined).length;
  if (sized === 0) {
    const { end, rows } = placeBars(entries, () => 1, 1);
    return { axis: { numbered: false, span: Math.max(1, end), step: 1 }, rows };
  }
  const step = axisStep(placeBars(entries, (task) => task.size ?? 0, 0).end);
  const { end, rows } = placeBars(entries, (task) => task.size ?? step, step);
  const span = Math.max(step, Math.ceil(end / step) * step);
  return { axis: { numbered: sized === all.length, span, step }, rows };
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
