import {
  type ComponentProps,
  memo,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";

import type { ArtifactBlock } from "../../../api/types";

import {
  focusVisibleRing,
  pageHairBorder,
  pageInkBorder,
  pageInkFill,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  textMutedOnCanvas,
} from "../../../theme/classes";
import { buildReferencePath, parseNodeReference } from "../../refs/routes";
import { useKeymap } from "../../shell/keymap";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { useSetFocusedNode } from "../focused-node";
import { focusedNodeOf, IssueTree, landOnRow, useReleaseFocus } from "../IssueTree";
import { drawnLine, firstLineOf, heldBy, lineWords, type TreeNode } from "../issue-tree";
import { useRowOnScreen } from "../NodeRow";
import { chipWords } from "../node-row-chip";
import type { FailedRead } from "../outline-reads";
import { authorText, pageChrome } from "../page-structure";
import {
  CONTROL_LABELS,
  NODE_TYPE_NAMES,
  PLAN_WORDS,
  PRESET_NAMES,
  STATE_LINES,
} from "../page-words";
import { FailedLineLeaves, FailedReadLine } from "../slots/failed-read-line";
import { moveToRow, treeEnterKey } from "../useTreeKeymap";
import {
  needsChip,
  type PlanBar,
  type PlanChartRow,
  type PlanCycle,
  type PlanEntry,
  planChart,
  planOrder,
  taskDocumentIds,
  writtenOrder,
} from "./plan-order";

/** The two reads the Plan view is drawn from (`slots/view-plan.tsx`): the plan document's
 *  blocks, which are its tasks, and the spec's blocks, which are the statements a task serves. */
export type PlanRead = "plan" | "statements";

/** A read that failed before it first answered, with the press that asks that read alone again:
 *  whether it runs again (`retrying`), and which failure it is (`failure`), as the slot reads
 *  them (`slots/view-plan.tsx`). Those three mean what they mean on the outline's line
 *  (`outline-reads.ts`'s `FailedRead`); `read` says which of the view's two reads failed. */
export type PlanFailedRead = Pick<FailedRead, "failure" | "retry" | "retrying"> & {
  readonly read: PlanRead;
};

/** How the reads the body is drawn from stand: one still out, both answered, or one or both
 *  failed before they first answered, in the order the slot gives them (`slots/view-plan.tsx`). */
export type PlanReads =
  | { readonly state: "waiting" }
  | { readonly state: "answered" }
  | {
      readonly state: "failed";
      readonly failed: readonly PlanFailedRead[];
      /** Whether a read of the view's is still out, waiting for its first answer or running
       *  again: a refused read running again is out, and one that waits for its Retry or for the
       *  network is not. */
      readonly out: boolean;
    };

/**
 * The Plan filter's body (the picture `page-v3-plan-1440.png` and the prototype's `.plan`).
 *
 * It opens on the chart of time: one bar for each step, starting after every task it needs has
 * ended, with the lines between a step and the steps it needs, the step's words beside its bar
 * and the statements it serves at the end of its row. One control switches to the ordered
 * steps, which are the outline's own rows in the same order (`IssueTree`): each task after every
 * task in its `needs`, each row drawing the task's words, the chips naming what it needs by the
 * names the tree gives them (`buildIssueTree`), and the line naming the statement it serves,
 * which is the parent the filter draws as no row.
 *
 * Tasks that need each other can be given no place of their own, so the order gives the cycle
 * one place (`plan-order.ts`): the chart draws it there as one line that lists its tasks, and
 * the steps draw that line over the tree and the cycle's tasks as rows at its place, so a step
 * that waits on one of them stands after it and an address naming one lands on a row.
 *
 * The order and the chart's places are the whole plan's (`plan`, the Plan view with no words
 * filter on), and the view's own tasks (`root`) choose which places are drawn: a step where its
 * task is in the view, a cycle where any of its tasks is, and under Steps the view's tasks as
 * rows in that order. So under a words filter a drawn bar starts where it does on the whole
 * plan and names, after "needs", every task it needs that the plan holds, drawn or not, and a
 * curve joins two drawn bars alone. Ordered from the view's tasks alone, a task the filter left
 * would stand at 0 and need nothing.
 *
 * The node the address names takes the focus once per address for the whole view, which is
 * held here rather than in the chart or the tree, since each is drawn anew at every press of
 * the switch: a press on "Chart" or "Steps" is the reader taking over, and leaves the focus on
 * the button pressed. Held by the chart, the address would take the focus back from "Chart"
 * each time the chart is drawn again.
 *
 * What it draws while its reads are out or refused is the slot's rule (`slots/view-plan.tsx`).
 *
 * ## What the reads give, and what they do not
 *
 * - **The plan's own order** is the order the tasks are written in (`plan-order.ts`'s
 *   `writtenOrder`), read from the blocks of the plan document the page already holds
 *   (`useIssueTree`'s `blocksOf("plan")`, so this costs no request): the filter's
 *   view hands the tasks over grouped by the statement each serves, which is the spec's order
 *   and not the plan's.
 * - **A bar's place** is worked out here from the `needs` the plan document holds, since no read
 *   gives a task its place.
 * - **A bar's length** is the task's size, and the `plan_task` block declares none, so every
 *   bar is one unit long and its row says "No size". The points of work and the longest path
 *   the picture names need sizes, so the view draws neither; the axis draws its units as lines
 *   and no numbers, since a number would be a size nobody recorded.
 * - **A step's state** is drawn nowhere: the model holds none, so the page draws none, and the
 *   states the pictures show are made up.
 * - **Who carries a step out**, and a parent's children's tasks in its own Plan filter, are
 *   drawn nowhere: no read gives them.
 */
export function PlanFilterBody({
  emptyLine,
  plan,
  planBlocks,
  reads,
  root,
  ...tree
}: ComponentProps<typeof IssueTree> & {
  /** The whole plan's tasks, as the Plan view gives them with no words filter on
   *  (`slots/view-plan.tsx`): the order and the chart's places are worked out from them, and
   *  `root`, the view, chooses which of those places are drawn. */
  plan: readonly TreeNode[];
  /** The plan document's typed blocks in the order it writes them, or undefined while the tree
   *  read is out (`slots/view-plan.tsx`, from `useIssueTree`'s `blocksOf("plan")`): the plan's
   *  own order (`writtenOrder`). */
  planBlocks: readonly Pick<ArtifactBlock, "id">[] | undefined;
  /** How the plan's and the spec's blocks reads stand, which the body draws its tasks after. */
  reads: PlanReads;
}): ReactNode {
  // Which of the two the reader is looking at: the chart, which the filter opens on, or the
  // ordered steps.
  const [steps, setSteps] = useState(false);
  const { artifacts } = tree;
  // The documents the plan's tasks are blocks of, and each one's block ids in the order it is
  // written (`writtenOrder`): every task of the plan, so a words filter changes no task's place.
  // A document whose read has not answered is left out, so its tasks keep the places the view
  // gives them.
  const documentIds = useMemo(() => taskDocumentIds(plan, artifacts), [artifacts, plan]);
  // The tree read answers the order of the plan document alone (`planBlocks`), so each document
  // is read against the plan's block ids.
  const written = useMemo(
    () => blockIdsOf(documentIds.map(() => ({ data: planBlocks }))),
    [documentIds, planBlocks]
  );
  const entries = useMemo(() => {
    const held = new Map<string, readonly string[]>();
    for (const [rank, id] of documentIds.entries()) {
      const blocks = written[rank];
      if (blocks !== undefined) held.set(id, blocks);
    }
    return planOrder(writtenOrder(plan, artifacts, held));
  }, [artifacts, documentIds, plan, written]);
  // The view's tasks by id, each as the view gives it and once, as the order holds it: the
  // places drawn are those holding one of them, and none until the reads have answered
  // (`slots/view-plan.tsx`).
  const answered = reads.state === "answered";
  const inView = useMemo(() => {
    const held = new Map<string, TreeNode>();
    if (!answered) return held;
    for (const node of root.children) if (!held.has(node.id)) held.set(node.id, node);
    return held;
  }, [answered, root]);
  const cycles = useMemo(
    () =>
      entries.flatMap((entry) =>
        entry.kind === "cycle" && entry.tasks.some((task) => inView.has(task.id)) ? [entry] : []
      ),
    [entries, inView]
  );
  // The steps, in the plan's order, with the tasks of a cycle at the cycle's place: the view's
  // tasks alone. Each row is its task as the filter's view gives it: its `needs` chips carry the
  // tree's names - the needed task's first line, and the page's word for its kind where that
  // task holds no words - which are the names the chart draws for the same needs (`needsChip`),
  // so the switch changes the drawing alone.
  const ordered = useMemo(() => {
    const rows = entries
      .flatMap((entry) => (entry.kind === "step" ? [entry.task] : entry.tasks))
      .flatMap((task) => {
        const node = inView.get(task.id);
        return node === undefined ? [] : [node];
      });
    return { ...root, children: rows, held: heldBy(rows) };
  }, [entries, inView, root]);
  // The node the address names, and the one the view has put the focus on or the reader went
  // past with a press of the switch: the address's node takes the focus while the two differ.
  const revealed = tree.reveal?.at(-1)?.id;
  const [taken, setTaken] = useState<string | undefined>(undefined);
  const landing = revealed !== undefined && revealed !== taken;
  const show = (next: boolean): void => {
    setTaken(revealed);
    setSteps(next);
  };
  const view = (pressed: boolean): string =>
    `rounded-full border px-3 py-0.5 text-sm leading-5 font-semibold ${focusVisibleRing} ${
      pressed
        ? `${pageInkFill} ${pageInkBorder}`
        : `${pageHairBorder} ${pagePaperBg} ${pageMutedText}`
    }`;
  // A view with no task to draw - none in the view, or the reads not yet answered - draws no
  // switch, having nothing to switch between, and draws its own tree (below), with the line the
  // slot hands on (`emptyLine`) once the reads have answered.
  const empty = inView.size === 0;
  // Each read that failed before it first answered, which the body says over the tree in the
  // order the slot hands them on.
  const unreadable = reads.state === "failed" ? reads.failed : [];
  // Whether the empty tree says the view holds no task: once the reads have answered, in the
  // slot's line, which describes the tree.
  const saysEmpty = empty && answered && emptyLine !== undefined;
  const emptyLineId = useId();
  // A copy of `FailedReadLine`'s ref callback (`slots/failed-read-line.tsx`), for a box that is
  // no failed line: when the empty tree leaves the page with the focus on it - a reader rested
  // there, then tasks came - it calls the failed-line context (`FailedLineLeaves`) as a failed
  // line leaving with the focus does, and leans on the stage's order of fallbacks for that line
  // (`slots/stage.tsx`'s `StageBox`) to put the focus back in the view: on the tree's tab-stop
  // row, since no Retry is left once tasks came. It stays a copy until that ref callback is
  // exported for other boxes.
  const leaves = useContext(FailedLineLeaves);
  const treeBox = useCallback(
    (element: HTMLDivElement | null) =>
      element === null
        ? undefined
        : () => {
            if (element.contains(document.activeElement)) leaves?.();
          },
    [leaves]
  );
  return (
    <div
      className="mt-3 mb-[120px] flex w-full min-w-0 flex-col max-[761px]:mt-0"
      data-plan-body=""
    >
      {/* Each read that failed before it first answered, one line in the page's words with a
          Retry that asks that read alone again, in the order the slot hands them on, over the
          tree (`FailedReadLine`). The stage the view is drawn in keeps the focus in the view when
          a line leaves with the focus in it, and hands a focus on the empty tree to a Retry or to
          the rows once they come (`slots/stage.tsx`). */}
      {unreadable.map(({ failure, read, retry, retrying }) => (
        <div className="mb-2.5" data-plan-unreadable={read} key={read}>
          <FailedReadLine
            failure={failure}
            message={UNREADABLE_LINES[read]}
            onRetry={retry}
            retrying={retrying}
          />
        </div>
      ))}
      {/* The one control: the chart of time, or the ordered steps. A plan with no task has
          nothing to switch between. The box is 22 px under the view row, and 10 px at 760 px
          and below, as the prototype's `.plan` is: the issue's column already stands 10 px
          under the row (`pt-2.5` of `[data-testid="issue-column"]`), so the box adds 12 px, and
          nothing at 760 px and below. The 120 px above the page's foot are the box's, so the
          steps keep them as the chart does. */}
      {empty ? null : (
        // biome-ignore lint/a11y/useSemanticElements: two toggles of one control; a fieldset would draw a frame the picture has none of.
        <div
          aria-label={PRESET_NAMES.plan}
          className="mb-2.5 flex min-w-0 flex-wrap gap-1.5"
          data-plan-switch=""
          role="group"
        >
          <button
            aria-pressed={!steps}
            className={view(!steps)}
            data-plan-view="chart"
            onClick={() => show(false)}
            type="button"
            {...pageChrome}
          >
            {PLAN_WORDS.chart}
          </button>
          <button
            aria-pressed={steps}
            className={view(steps)}
            data-plan-view="steps"
            onClick={() => show(true)}
            type="button"
            {...pageChrome}
          >
            {PLAN_WORDS.steps}
          </button>
        </div>
      )}
      {empty ? (
        // Where the view holds no task to draw its tree is its own, in the box `IssueTree` draws
        // its tree in and under the outline's name, which is how `t p` finds it
        // (`useIssueKeymap.ts`'s `reachPart`): it holds nothing, and once the reads have
        // answered the empty line stands under it, as `IssueTree` draws that line, and describes
        // it. `IssueTree`'s tree takes no focus; this one does, so the chord and the stage have a
        // tree in the view to put the focus on. It reads busy while a read is out, still waiting
        // for its first answer or running again (`PlanReads`' `out`), and not while each read
        // not yet answered is refused and waits, for its Retry or for the network.
        <div className="mb-3 flex min-w-0 flex-col" ref={treeBox}>
          <div
            aria-busy={reads.state === "waiting" || (reads.state === "failed" && reads.out)}
            aria-describedby={saysEmpty ? emptyLineId : undefined}
            aria-label={CONTROL_LABELS.tree}
            className="flex min-w-0 flex-col"
            role="tree"
            tabIndex={-1}
          />
          {saysEmpty ? (
            <p className={`py-1 text-sm ${textMutedOnCanvas}`} id={emptyLineId} {...pageChrome}>
              {emptyLine}
            </p>
          ) : null}
        </div>
      ) : steps ? (
        <>
          {cycles.map((cycle) => (
            <CycleLine cycle={cycle} key={cycleIds(cycle.tasks)} />
          ))}
          {/* The tree takes the address's node only while the view has not: drawn after a
              press of the switch, it leaves the focus on the button pressed. */}
          <IssueTree {...tree} focusReveal={tree.focusReveal && landing} root={ordered} />
        </>
      ) : (
        <PlanChart
          entries={entries}
          holdsEscape={tree.holdsEscape}
          inView={inView}
          land={landing ? revealed : undefined}
          onLanded={setTaken}
          reveal={revealed}
        />
      )}
    </div>
  );
}

/** What the body says of each read that failed before it first answered: the outline's own
 *  words for that read, since the outline reads the same blocks and draws the same failure on
 *  load (`outline-reads.ts`), the plan for the plan document's blocks, which hold its tasks, and
 *  the spec for the spec's blocks, which hold the statements. */
const UNREADABLE_LINES: Readonly<Record<PlanRead, string>> = {
  plan: STATE_LINES.plan_unreadable,
  statements: STATE_LINES.spec_unreadable,
};

/** Each document's block ids in the order they are written, or undefined while its read is
 *  out. */
function blockIdsOf(
  answers: readonly { readonly data?: readonly { readonly id: string }[] | undefined }[]
): readonly (readonly string[] | undefined)[] {
  return answers.map((answer) => answer.data?.map((block) => block.id));
}

/** The key of a cycle's place: its tasks' ids, one space between two. */
function cycleIds(tasks: readonly TreeNode[]): string {
  return tasks.map((one) => one.id).join(" ");
}

/** One cycle, as one line that lists the tasks in it, each a link that lands on that task.
 *  The line is the prototype's own line above the plan (`.psum`, proto-v3.html :490).
 *
 *  In the chart the line is a row of the tree, which is one stop of Tab, so its links take
 *  none and `f` reaches them. Above the steps it is outside the tree, so its links take Tab as
 *  any link does: a task of a cycle is reached by the keyboard in both drawings. */
function CycleLine({ cycle, inChart = false }: { cycle: PlanCycle; inChart?: boolean }): ReactNode {
  return (
    <p
      className={`mb-2.5 flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm leading-5 ${pageMutedText}`}
      data-plan-cycle=""
    >
      <span {...pageChrome}>{PLAN_WORDS.cycle}</span>
      {cycle.tasks.map((task) => (
        <TaskLink inChart={inChart} key={task.id} task={task} />
      ))}
    </p>
  );
}

/** The task's one line, the line the page draws for it wherever it draws one and the label of
 *  its bar, by the page's one-line rule: the first line of the words of its first block that
 *  holds any (`lineWords`), so a heading is never run into the paragraph after it, drawn with an
 *  ellipsis where the cut falls inside a sentence (`drawnLine`). It is the line the outline's
 *  row draws for the same task under "Steps" (`NodeRow.tsx`) and the tree's name of a `needs`
 *  chip pointing at it (`buildIssueTree`), so the switch changes the drawing alone. A task that
 *  holds no words is named by the page's word for its kind (`NODE_TYPE_NAMES`), never by its
 *  id, as a parent's line under a filter is (`presets.ts`'s `parentLineOf`). The marks say whose
 *  words the line is, and the tree's name of a `needs` chip pointing at the task: the author's,
 *  or the page's own for a task that holds none, since both are then its kind's word. */
function taskLine(task: TreeNode): {
  readonly marks: Readonly<Record<string, string>>;
  readonly words: string;
} {
  const words = lineWords(task);
  return words === ""
    ? { marks: pageChrome, words: NODE_TYPE_NAMES[task.type] }
    : { marks: authorText, words: drawnLine(firstLineOf(words)) };
}

/** One task of a cycle, by its own first line, as the link a reader follows to it: in no tab
 *  order of its own where the line is a row of the chart (`CycleLine`). */
function TaskLink({ inChart, task }: { inChart: boolean; task: TreeNode }): ReactNode {
  const route = task.reference === undefined ? undefined : parseNodeReference(task.reference);
  const line = taskLine(task);
  return route === undefined ? (
    <span className="min-w-0 break-words" {...line.marks}>
      {line.words}
    </span>
  ) : (
    <Link
      className={`dispatch-inline-link min-w-0 break-words underline decoration-page-hair underline-offset-[3px] ${pageMutedText}`}
      {...(inChart ? { tabIndex: -1 } : {})}
      to={buildReferencePath(route)}
      {...line.marks}
    >
      {line.words}
    </Link>
  );
}

/** The chart's three columns, as the prototype's `.prow` divides them: the step, the time, and
 *  what the step serves; at 1240 px and below the checkpoint's narrower outer columns. At 760 px
 *  and below the head row is not drawn and each row is a card that lays its parts out itself
 *  (`BarRow`, `.pitem`). "And below" is one pixel over the width: Tailwind writes an arbitrary
 *  `max-[N]:` as `(width < N)`, so `max-[1241px]:` and `max-[761px]:` take 1240 px and 760 px
 *  themselves, as the header's `(max-width: 760px)` does (`NodeRow.tsx`'s `NODE_ROW_LAYOUT`). */
const CHART_COLUMNS =
  "grid min-w-0 grid-cols-[330px_minmax(0,1fr)_320px] max-[1241px]:grid-cols-[260px_minmax(0,1fr)_220px]";

/** A column heading, in the prototype's `.cap`. */
const CAP = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** The row the reader is on, a step's or a cycle's, as the prototype's `.prow.focused` draws
 *  it, and at 760 px and below its card as `.pitem.focused` does: a ring of ink outside a gap of
 *  paper, on the card's own paper, so the mark is not the tint alone. */
const FOCUSED =
  "bg-page-step-tint shadow-[inset_3px_0_0_var(--color-page-step)] max-[761px]:bg-page-paper max-[761px]:shadow-[0_0_0_2px_var(--color-page-paper),0_0_0_4px_var(--color-page-ink)]";

/** Every row of the chart, a step's and a cycle's, in the order it is drawn. */
const CHART_ROW = "[data-plan-bar-row], [data-plan-cycle-row]";

/** One place the chart draws: its row, the key its focus and its tab stop are held under, and
 *  the task it stands for, which is the step's own, or for a cycle the task its row gives to
 *  Details and to Enter. */
interface DrawnPlace {
  readonly key: string;
  readonly row: PlanChartRow;
  readonly task: TreeNode;
}

/**
 * The chart of time (the prototype's `.chart`): one row per place in the plan's order. A
 * step's row draws its first line and the steps it needs, its bar, and the statements it
 * serves; a cycle's row lists the tasks that need each other. The lines between a bar and the
 * bars it needs are drawn over the rows from where the bars stand.
 *
 * The places and the axis are the whole plan's (`entries`), and the chart draws those that hold
 * a task of the view (`inView`): a bar a words filter leaves starts where it does on the whole
 * plan, and its curves are drawn to the bars drawn beside it.
 *
 * It is the outline the page draws under this view, so it is one tree named as the outline is,
 * and its rows are its items: the outline's own keys work it, under the ids the outline
 * registers them with (`useTreeKeymap.ts`), so `?` names each once. Each takes the outline's
 * label but `tree-link` and `tree-edit`, whose labels say what they do here ("Move to the next
 * link on the focused row", "Go to the focused step in the conversation"). Tab from the page
 * itself enters the chart at its one tab stop, the arrows and `j`/`k` reach the bars in order
 * and pass over a cycle's row, Home and End the first and the last bar, `f` steps through the
 * focused row's links, Escape gives a row back from its link and leaves the chart from the row,
 * and Enter goes to the row's task in the conversation, where it is edited. The focus on a row
 * puts its task in Details. A focus anywhere inside a row - the row's own, or a link of it that
 * a pointer went down on - is the focus on that row, as it is on the outline's (`NodeRow.tsx`):
 * the row takes the mark, the heavier curves, Details and the Tab stop, and the keys, which find
 * the row that holds the focus (`at`), move from there. The focus leaving the chart lets go of
 * its row by the outline's rule (`releaseFocus`, below).
 *
 * A cycle's row takes the focus too, as a bar's does: a press on it, an address naming one of
 * its tasks, and the Tab stop where the chart draws no bar. It stands for the task the address
 * names, else its first, and from it the arrows go to the step after it and the step before it.
 * Were it to take no focus, an address naming a task of a cycle would focus nothing and a plan
 * whose every task is in a cycle would have no Tab stop.
 *
 * The one Tab stop is the row the reader last stood on, else the row the address names, else the
 * first bar's, else the first cycle's. The address's row is the stop so that the stage, which
 * gives a focus nobody holds to the tree's tab-stop row (`slots/stage.tsx`), gives it to that row,
 * and the landing keeps it (`landOnRow`): given to the first bar, it would end the landing, which
 * leaves the focus where it is moved before its frame. It is the address's row for as long as the
 * address names it, and not only until the landing: the view says it has landed before the stage
 * takes the focus where the rows are drawn once the reads answer, on a fresh load, a link, or a
 * Retry. Each row places itself on screen by the outline's one hook (`useRowOnScreen`), so the
 * keys move the focus without the browser's own scroll, which knows nothing of the band that
 * stays at the top of the page.
 */
function PlanChart({
  entries,
  holdsEscape,
  inView,
  land,
  onLanded,
  reveal,
}: {
  entries: readonly PlanEntry[];
  /** Whether the page takes Escape from a row before the chart is left: the words filter's,
   *  which comes first in the key table's order (`slots/view-plan.tsx`). */
  holdsEscape: boolean;
  /** The view's tasks by id: a place is drawn where it holds one of them. */
  inView: ReadonlyMap<string, unknown>;
  /** The node the address names while it is the view's to put the focus on: once per address
   *  for the whole view, and not past a press of the switch (`PlanFilterBody`), since the chart
   *  is drawn anew at every press. */
  land: string | undefined;
  /** Says the address's node has taken the focus. */
  onLanded: (id: string) => void;
  /** The node the address names: the task a cycle's row stands for where it is one of its own. */
  reveal: string | undefined;
}): ReactNode {
  const navigate = useNavigate();
  const setFocusedNode = useSetFocusedNode();
  const chart = useRef<HTMLDivElement | null>(null);
  const { end, rows: places } = useMemo(() => planChart(entries), [entries]);
  const rows = useMemo(
    () =>
      places.flatMap((row): DrawnPlace[] => {
        if (row.kind === "bar") {
          return inView.has(row.task.id) ? [{ key: row.task.id, row, task: row.task }] : [];
        }
        const task = row.tasks.find((one) => one.id === reveal) ?? row.tasks[0];
        return task === undefined || !row.tasks.some((one) => inView.has(one.id))
          ? []
          : [{ key: cycleIds(row.tasks), row, task }];
      }),
    [inView, places, reveal]
  );
  const bars = useMemo(
    () => places.flatMap((row) => (row.kind === "bar" && inView.has(row.task.id) ? [row] : [])),
    [inView, places]
  );
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the chart's one tab stop rests, which outlives the focus leaving it.
  const [stop, setStop] = useState<string | undefined>(undefined);
  const tabStop = rows.some((place) => place.key === stop)
    ? stop
    : (
        rows.find((place) => place.task.id === reveal) ??
        rows.find((place) => place.row.kind === "bar") ??
        rows[0]
      )?.key;

  // The focused row gives its task to Details (`focused-node.tsx`) as the outline gives a focused
  // node, by the outline's own facts of it (`focusedNodeOf`: who wrote it, the item it is, its
  // address, its state, its first line as its title, its kind), so a step's facts in Details
  // are the same in the chart and under "Steps".
  const takeFocus = useCallback(
    (key: string, task: TreeNode) => {
      setFocused(key);
      setStop(key);
      setFocusedNode(focusedNodeOf(task));
    },
    [setFocusedNode]
  );
  // The chart lets go of its focused row by the outline's own rule (`IssueTree.tsx`'s
  // `useReleaseFocus`): the focus leaving the chart with no press under way runs `leave` at
  // once, and a press that takes it elsewhere runs it once that press has ended, unless the
  // focus is back in the chart by then, so the row's mark and its heavier curves go as the
  // outline's row lets go of its look.
  // Details goes back to the issue's own facts as the focus leaves, unless it goes to Details or
  // the control that opens it. A focus moving within the chart is no leave. The chart edits no
  // row (Enter goes to the conversation), so no edit holds the focus.
  const leave = useCallback(() => setFocused(undefined), []);
  const releaseFocus = useReleaseFocus({ editing: false, onLeave: leave });
  // An address that names a task puts the focus on its row, a cycle's included, and holds the
  // row on screen until the reader acts, by the outline's own landing (`IssueTree.tsx`'s
  // `landOnRow`): the rows around it are still arriving and push it down the page.
  const landingHold = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => landingHold.current?.abort(), []);
  useEffect(() => {
    if (land === undefined || !rows.some((place) => place.task.id === land)) return;
    const row = chart.current?.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${CSS.escape(land)}"]`
    );
    if (row == null) return;
    onLanded(land);
    landOnRow(row, landingHold);
  }, [land, onLanded, rows]);

  // The lines between a bar and the bars it needs, from where the browser laid the bars out:
  // from the end of the needed bar to the start of the one that needs it (the prototype's
  // `drawPlanLinks`). They are read again whenever the chart's box changes. A need whose bar is
  // not drawn - a cycle's task, or a step the words filter left - has no line.
  const [links, setLinks] = useState<{
    readonly height: number;
    readonly paths: readonly { readonly d: string; readonly of: readonly string[] }[];
    readonly width: number;
  }>({ height: 0, paths: [], width: 0 });
  useLayoutEffect(() => {
    const element = chart.current;
    if (element === null) return;
    const measure = () => {
      const frame = element.getBoundingClientRect();
      // Where each drawn bar stands, read once in a pass for every line that meets it.
      const boxes = new Map<string, { left: number; right: number; y: number }>();
      for (const drawn of element.querySelectorAll("[data-plan-bar]")) {
        const rect = drawn.getBoundingClientRect();
        boxes.set(drawn.getAttribute("data-plan-bar") ?? "", {
          left: rect.left - frame.left,
          right: rect.right - frame.left,
          y: rect.top - frame.top + rect.height / 2,
        });
      }
      const paths = bars.flatMap((bar) => {
        const to = boxes.get(bar.task.id);
        if (to === undefined) return [];
        return bar.needs.flatMap((needed) => {
          const from = boxes.get(needed.id);
          if (from === undefined) return [];
          const bend = Math.max(18, (to.left - from.right) / 2);
          return [
            {
              d: `M${from.right} ${from.y} C${from.right + bend} ${from.y} ${to.left - bend} ${to.y} ${to.left - 1} ${to.y}`,
              of: [needed.id, bar.task.id],
            },
          ];
        });
      });
      // A pass that finds every line where it was keeps the lines held, and draws nothing anew.
      setLinks((previous) =>
        previous.height === frame.height &&
        previous.width === frame.width &&
        previous.paths.length === paths.length &&
        paths.every((path, index) => {
          const held = previous.paths[index];
          return (
            held !== undefined &&
            held.d === path.d &&
            held.of.length === path.of.length &&
            held.of.every((id, at) => id === path.of[at])
          );
        })
          ? previous
          : { height: frame.height, paths, width: frame.width }
      );
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const resized = new ResizeObserver(measure);
    resized.observe(element);
    return () => resized.disconnect();
  }, [bars]);

  /** The chart's rows as the browser holds them, bars and cycles, in the order of `rows`. */
  const items = (): HTMLElement[] => [
    ...(chart.current?.querySelectorAll<HTMLElement>(CHART_ROW) ?? []),
  ];
  /** Which of `held` the reader is on, a link inside it included, or -1 when on none. */
  const at = (held: readonly HTMLElement[]): number => {
    const active = document.activeElement;
    return active === null ? -1 : held.findIndex((row) => row === active || row.contains(active));
  };
  /** Whether the reader is reading: on the page itself, or on a row of the chart. */
  const reading = (): boolean =>
    document.activeElement === null ||
    document.activeElement === document.body ||
    at(items()) !== -1;
  /** The first bar's row of `held` past `from`, going `by` rows at a time, or `from` itself
   *  where there is none, so a move key at the chart's end stays on the row it is on, as the
   *  outline's move keys stop at its ends: the keys that move the reader pass over a cycle's
   *  row. */
  const barFrom = (held: readonly HTMLElement[], from: number, by: 1 | -1): number => {
    for (let index = from + by; index >= 0 && index < held.length; index += by) {
      if (held[index]?.hasAttribute("data-plan-bar-row") === true) return index;
    }
    return from;
  };
  /** The outline's move to the `index`th of `held` (`useTreeKeymap.ts`'s `moveToRow`). */
  const moveTo = (held: readonly HTMLElement[], index: number): void => {
    const target = held[index];
    if (target !== undefined) moveToRow(target, { preventScroll: true });
  };
  /** The outline's move keys on the chart (`useTreeKeymap.ts`'s `tree-next`, `tree-previous` and
   *  `tree-arrows`): from a row to the nearest bar's row the way `by` goes (`barFrom`), and from
   *  the page itself to the first bar, whichever way the key goes. */
  const step = (by: 1 | -1): void => {
    const held = items();
    const on = at(held);
    moveTo(held, on === -1 ? barFrom(held, -1, 1) : barFrom(held, on, by));
  };
  useKeymap("issue", [
    // The chart is one tab stop: Tab from the page itself enters it at that stop, the row the
    // reader last stood on, by the outline's own key (`useTreeKeymap.ts`'s `treeEnterKey`), so
    // after Escape the same Tab goes back to the row under either drawing.
    treeEnterKey({ moveTo: (index) => moveTo(items(), index), rows: items }),
    {
      id: "tree-next",
      keys: "j",
      label: "Move to the next row",
      palette: false,
      run: () => step(1),
      when: () => reading() && items().length > 0,
    },
    {
      // `k` from the page itself goes to the first bar, as `j` does.
      id: "tree-previous",
      keys: "k",
      label: "Move to the previous row",
      palette: false,
      run: () => step(-1),
      when: () => reading() && items().length > 0,
    },
    {
      // Down and Up scroll the page from the page itself, so they move through the chart only
      // while the reader is on a row or on a link inside one, as the outline's do.
      id: "tree-arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: "Move to the next / previous row while one is focused",
      palette: false,
      run: (event) => step(event.key === "ArrowDown" ? 1 : -1),
      when: () => at(items()) !== -1,
    },
    {
      id: "tree-first",
      keys: "Home",
      label: "Move to the first row",
      palette: false,
      run: () => {
        const held = items();
        moveTo(held, barFrom(held, -1, 1));
      },
      when: () => at(items()) !== -1,
    },
    {
      id: "tree-last",
      keys: "End",
      label: "Move to the last row",
      palette: false,
      run: () => {
        const held = items();
        moveTo(held, barFrom(held, held.length, -1));
      },
      when: () => at(items()) !== -1,
    },
    {
      // The statements a step serves and the tasks a cycle lists are links, and the chart is
      // one tab stop, so a key reaches them; Enter on the link it left the focus on is the
      // browser's own. The link is focused as the outline's f focuses one, letting the browser
      // bring it on screen, so a row the reader scrolled away does not keep its link off screen.
      id: "tree-link",
      keys: "f",
      label: "Move to the next link on the focused row",
      palette: false,
      run: () => {
        const held = items();
        const links = [...(held[at(held)]?.querySelectorAll<HTMLElement>("a[href]") ?? [])];
        const on = links.indexOf(document.activeElement as HTMLElement);
        links[(on + 1) % links.length]?.focus();
      },
      when: () => {
        const held = items();
        return (held[at(held)]?.querySelector("a[href]") ?? null) !== null;
      },
    },
    {
      // Enter on a row goes to its task in the conversation, where it is edited: the chart
      // draws each step and edits none. A cycle's row goes to the task it stands for. On a link
      // inside the row Enter is the link's own.
      id: "tree-edit",
      keys: "Enter",
      label: "Go to the focused step in the conversation",
      palette: false,
      run: () => {
        const task = rows[items().indexOf(document.activeElement as HTMLElement)]?.task;
        const path = task === undefined ? undefined : conversationPath(task);
        if (path !== undefined) navigate(path);
      },
      when: () => items().includes(document.activeElement as HTMLElement),
    },
    {
      // From a link the reader reached inside a row, Escape gives the row back, since the row
      // is where the keys that move through the chart are; from the row itself it leaves the
      // chart, once the page has cleared its words filter, which comes first in the key table's
      // order. The outline's own Escape, under its id (`useTreeKeymap.ts`'s `tree-leave`).
      id: "tree-leave",
      keys: "Escape",
      label: "Leave the control, then the tree",
      palette: false,
      run: () => {
        const held = items();
        const active = document.activeElement as HTMLElement | null;
        if (active !== null && held.includes(active)) {
          active.blur();
          return;
        }
        moveTo(held, at(held));
      },
      when: () => {
        const held = items();
        const onRow = held.includes(document.activeElement as HTMLElement);
        return at(held) !== -1 && !(holdsEscape && onRow);
      },
    },
  ]);

  return (
    <div
      className={`relative min-w-0 rounded-[10px] border pb-2 max-[761px]:rounded-none max-[761px]:border-0 max-[761px]:pb-0 ${pageLineBorder} ${pagePaperBg}`}
      data-plan-chart=""
      ref={chart}
    >
      {/* The head row's three columns are "Steps", the axis and "Serves": the axis is drawn in
          a cell of its own, as tall as the row, which holds it as a bar's track holds its own,
          so "Serves" stands over the statements' column and every line of the axis inside the
          time's column. Drawn straight into the grid, the axis would be no cell of it: "Serves"
          would take the second column and the lines would span the chart. */}
      <div
        className={`${CHART_COLUMNS} min-h-10 items-center max-[761px]:hidden`}
        data-plan-head=""
      >
        <span className={`px-3.5 ${CAP}`} {...pageChrome}>
          {PLAN_WORDS.steps}
        </span>
        <div className="relative min-w-0 self-stretch">
          <Units end={end} />
        </div>
        <span className={`px-3.5 ${CAP}`} {...pageChrome}>
          {PLAN_WORDS.serves}
        </span>
      </div>
      <div aria-label={CONTROL_LABELS.tree} className="min-w-0" onBlur={releaseFocus} role="tree">
        {rows.map((place) =>
          place.row.kind === "cycle" ? (
            <CycleRow
              cycle={place.row}
              focused={focused === place.key}
              key={place.key}
              onFocus={() => takeFocus(place.key, place.task)}
              tabStop={tabStop === place.key}
              task={place.task}
            />
          ) : (
            <BarRow
              bar={place.row}
              end={end}
              focused={focused === place.key}
              key={place.key}
              onFocus={() => takeFocus(place.key, place.task)}
              tabStop={tabStop === place.key}
            />
          )
        )}
      </div>
      {/* Decoration over the rows: the words on each row say what a step needs. The lines of
          the step the reader is on are drawn heavier in the step's colour, as the prototype's
          are, and the rest in the quiet grey: the prototype's ink marks its longest path, which
          today's model holds no sizes to find. */}
      <svg
        aria-hidden="true"
        className="pointer-events-none absolute top-0 left-0 overflow-visible max-[761px]:hidden"
        data-plan-links=""
        height={links.height}
        width={links.width}
      >
        {links.paths.map((path) => {
          const mine = focused !== undefined && path.of.includes(focused);
          return (
            <path
              d={path.d}
              data-plan-link={path.of.join(" ")}
              fill="none"
              key={path.of.join(" ")}
              stroke={mine ? "var(--color-page-step)" : "var(--color-page-quiet)"}
              strokeWidth={mine ? 2 : 1.4}
            />
          );
        })}
      </svg>
    </div>
  );
}

/**
 * One cycle's row of the chart: the line for the tasks that need each other, and a row the
 * focus stands on as it does on a step's. It stands for `task` - the task the address names
 * where it is one of the cycle's, else its first - which is its node id, so the page's
 * `[data-node-row]` readers (`t p`, Details) reach it as any row, and which it gives to Details
 * when it takes the focus: a focus inside it, on one of its task links as well as on the row,
 * takes the focus for it (`PlanChart`). `data-plan-cycle-row` holds every task of it. It places
 * itself on screen as every row does (`useRowOnScreen`), and its focus is marked as a step's
 * is, not by colour alone.
 */
function CycleRow({
  cycle,
  focused,
  onFocus,
  tabStop,
  task,
}: {
  cycle: PlanCycle;
  focused: boolean;
  onFocus: () => void;
  tabStop: boolean;
  task: TreeNode;
}): ReactNode {
  const ref = useRef<HTMLDivElement | null>(null);
  const press = useRowOnScreen(ref, focused);
  return (
    <div
      aria-level={1}
      className={`border-t px-3.5 pt-2 outline-none max-[761px]:border-0 max-[761px]:px-0 ${pageLineBorder} ${
        focused ? FOCUSED : ""
      }`}
      data-node-id={task.id}
      data-node-row=""
      data-node-type={task.type}
      data-plan-cycle-row={cycleIds(cycle.tasks)}
      onFocus={onFocus}
      onPointerDown={press}
      ref={ref}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <CycleLine cycle={cycle} inChart />
    </div>
  );
}

/** The axis: one faint line per unit of the chart (the prototype's `.grid-l`). It draws no
 *  number: a unit is a size, and today's plan holds none. The last line is drawn against the
 *  column's right edge, so every line stands inside the column that holds the axis: at
 *  `left: 100%` it would be one pixel past it. */
const Units = memo(function Units({ end }: { end: number }): ReactNode {
  const unit = 100 / end;
  return (
    <span
      aria-hidden="true"
      className="absolute inset-0 block max-[761px]:hidden"
      data-plan-axis=""
    >
      {Array.from({ length: end + 1 }, (_unused, tick) => (
        <i
          className="absolute inset-y-0 w-px bg-page-line"
          // biome-ignore lint/suspicious/noArrayIndexKey: a line is its unit and nothing else.
          key={tick}
          style={tick === end ? { right: 0 } : { left: `${tick * unit}%` }}
        />
      ))}
    </span>
  );
});

/**
 * One step of the chart (the prototype's `.prow`, and its `.pitem` card at 760 px and below):
 * its first line, that the model holds no size for it and the steps it needs; its bar; and the
 * statements it serves. The row's name is what it draws, which begins with the task's first
 * line, so each bar is labelled with it. The bar is the step's plain tint inside a dashed
 * border: the prototype's hatch is its legend's "Not started", a state today's model holds
 * none of, and the dash says the length is no size anybody wrote.
 *
 * A need is named by the task's own `needs` chip for it (`needsChip`), as the tree names it and
 * the steps' row draws it, in the needed task's marks: the author's words, or the page's own
 * where that task holds none, since the tree names it then by its kind's word. A chip the tree
 * has not named, or names by no words, names no task, so the row draws no need for it, names
 * none from the needed task's own node, and draws "· needs" only before a need it draws. The
 * curves are the chart's, and join two drawn bars whatever the names.
 *
 * At 761 px and above the row is the chart's three columns. At 760 px and below it is one card
 * that draws, from the top, the task's first line, the track with the bar at its place and
 * length, and one line, wrapping where it must, with "No size", the needs and the statements.
 * Each part is drawn once, in the order the row's name reads them: the card is a wrapping flex
 * box, the columns' own boxes give their parts to it (`display: contents`), and the first line
 * and the track each take a line of their own ahead of the rest (`order`), so the track, which
 * says nothing to a screen reader, is the one part drawn out of the document's order. The track
 * stands 8 px under the first line and over the line under it: 3 px of its own margin and the
 * card's 5 px between two lines, which is also what parts the chips' lines where they wrap.
 *
 * A focus inside the row, on one of its statement links as well as on the row, takes the focus
 * for it (`PlanChart`). The focused row places itself on screen, clear of the band that stays at
 * the top, by the outline's one hook, and a press on it is left where the reader pressed
 * (`useRowOnScreen`).
 */
function BarRow({
  bar,
  end,
  focused,
  onFocus,
  tabStop,
}: {
  bar: PlanBar;
  /** How many units the chart's axis spans. */
  end: number;
  focused: boolean;
  onFocus: () => void;
  tabStop: boolean;
}): ReactNode {
  const task = bar.task;
  const line = taskLine(task);
  // How wide one unit of the chart is, as a percentage of its column.
  const unit = 100 / end;
  const ref = useRef<HTMLDivElement | null>(null);
  const press = useRowOnScreen(ref, focused);
  // Each chip's React key, unique within the task and the same on every render. Two chips of one
  // task can carry the same words, or the same reference: the server refuses a pointer named
  // twice only by its raw value, so two written differently can name one statement. A chip is
  // keyed by its reference, or by its words where it has none, each under its kind, and a repeat
  // of that key by the count of chips before it that hold it: `ref:` or `words:` begins the first,
  // and a count and a space begin each repeat, so no repeat takes another chip's key.
  const keyed = new Map<string, number>();
  const serves = [
    ...(task.parentLine === undefined || task.parentLine.type !== "requirement"
      ? []
      : [{ line: task.parentLine.line, reference: task.parentLine.reference }]),
    ...task.alsoUnder
      .filter((chip) => chip.name !== "")
      .map((chip) => ({ line: chip.name, reference: chip.ref })),
  ].map((one) => {
    const base = one.reference === undefined ? `words:${one.line}` : `ref:${one.reference}`;
    const before = keyed.get(base) ?? 0;
    keyed.set(base, before + 1);
    return { ...one, key: before === 0 ? base : `${before} ${base}` };
  });
  // The needs the row names, each by the words of its chip. `planChart` reads each need from one
  // of the task's `needs` chips, so every need has its chip; one with no words is drawn as none.
  const needs = bar.needs.flatMap((needed) => {
    const words = chipWords(needsChip(task, needed)?.name ?? "");
    return words === "" ? [] : [{ needed, words }];
  });
  return (
    <div
      aria-level={1}
      className={`${CHART_COLUMNS} min-h-[54px] items-center border-t outline-none max-[761px]:my-2 max-[761px]:flex max-[761px]:flex-wrap max-[761px]:gap-x-1.5 max-[761px]:gap-y-[5px] max-[761px]:rounded-lg max-[761px]:border max-[761px]:border-l-4 max-[761px]:border-l-page-step max-[761px]:px-3 max-[761px]:py-2.5 ${pageLineBorder} ${
        focused ? FOCUSED : ""
      }`}
      data-node-id={task.id}
      data-node-row=""
      data-node-type={task.type}
      data-plan-bar-row=""
      data-plan-start={bar.start}
      onFocus={onFocus}
      onPointerDown={press}
      ref={ref}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <div className="min-w-0 px-3.5 py-2 max-[761px]:contents">
        <p
          className={`text-[15px] leading-[22px] break-words ${pageInkText} max-[761px]:-order-2 max-[761px]:min-w-0 max-[761px]:basis-full`}
          data-node-words=""
          {...line.marks}
        >
          {line.words}
        </p>
        <p
          className={`mt-px flex min-w-0 flex-wrap gap-x-1.5 text-xs leading-[18px] ${pageQuietText} max-[761px]:contents`}
          data-plan-facts=""
        >
          <span data-plan-no-size="" {...pageChrome}>
            {PLAN_WORDS.no_size}
          </span>
          {needs.length === 0 ? null : (
            <>
              <span {...pageChrome}>· {PLAN_WORDS.needs}</span>
              {needs.map(({ needed, words }) => (
                <span data-plan-need={needed.id} key={needed.id} {...taskLine(needed).marks}>
                  {words}
                </span>
              ))}
            </>
          )}
        </p>
      </div>
      <div className="relative h-[54px] min-w-0 max-[761px]:-order-1 max-[761px]:my-[3px] max-[761px]:h-2.5 max-[761px]:basis-full max-[761px]:rounded-[5px] max-[761px]:bg-page-mark">
        <Units end={end} />
        <span
          aria-hidden="true"
          className="absolute top-4 block h-[22px] rounded-[5px] border-[1.5px] border-dashed border-page-step bg-page-step-tint max-[761px]:top-0 max-[761px]:h-full"
          data-plan-bar={task.id}
          style={{ left: `${bar.start * unit}%`, width: `${unit}%` }}
        />
      </div>
      <div
        className="flex min-w-0 flex-wrap items-start gap-[5px] px-3.5 py-2 max-[761px]:contents"
        data-plan-serves=""
      >
        {serves.map((one) => {
          const route = one.reference === undefined ? undefined : parseNodeReference(one.reference);
          // The chip is a box of its own, `data-plan-serve` for either kind, which draws its
          // border and padding and wraps a long first word inside the column rather than run past
          // it. A linked chip's link inside it draws none: below 1280 px a
          // `dispatch-inline-link` takes 6 px of padding above and below (`styles.css`), which an
          // inline link inside the box's 18 px line takes without growing the box. The chip draws
          // a border and padding around its link, so the link is stretched over the whole box, its
          // border included (the link's `::after`, laid over the box it is `relative` to): a press
          // anywhere on the chip opens the statement, where a press on its edge would land on the
          // row and give the row the focus.
          const look = `min-w-0 rounded-[5px] border px-1.5 text-xs leading-[18px] font-semibold text-page-statement [overflow-wrap:anywhere] ${pageHairBorder}`;
          return route === undefined ? (
            <span className={look} data-plan-serve="" key={one.key} {...authorText}>
              {chipWords(one.line)}
            </span>
          ) : (
            <span className={`relative ${look}`} data-plan-serve="" key={one.key}>
              <Link
                className="dispatch-inline-link after:absolute after:-inset-px after:content-['']"
                tabIndex={-1}
                to={buildReferencePath(route)}
                {...authorText}
              >
                {chipWords(one.line)}
              </Link>
            </span>
          );
        })}
      </div>
    </div>
  );
}
