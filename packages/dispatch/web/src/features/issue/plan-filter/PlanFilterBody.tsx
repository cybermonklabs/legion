import { useQueries } from "@tanstack/react-query";
import {
  type ComponentProps,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useNavigate } from "react-router-dom";

import { artifactBlocksQuery } from "../../../api/queries";

import {
  focusVisibleRing,
  pageHairBorder,
  pageInkBg,
  pageInkBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
} from "../../../theme/classes";
import { DETAILS_ATTRIBUTE } from "../../margin/margin-slot";
import { buildReferencePath, parseNodeReference } from "../../refs/routes";
import { useKeymap } from "../../shell/keymap";
import { conversationPath } from "../decisions-filter/decisions-parts";
import { useSetFocusedNode } from "../focused-node";
import { IssueTree } from "../IssueTree";
import { documentOfNode } from "../issue-artifacts";
import { firstLineOf, nodeWords, type TreeNode } from "../issue-tree";
import { chipWords } from "../node-row-chip";
import { authorText, pageChrome } from "../page-structure";
import { CONTROL_LABELS, PLAN_WORDS, PRESET_NAMES } from "../page-words";
import { type PlanBar, type PlanCycle, type PlanEntry, planChart, planOrder } from "./plan-order";

/**
 * The Plan filter's body (dispatch://INFRA-1196 R26, ruling 5 of build-rulings-md, the picture
 * `page-v3-plan-1440.png` and the prototype's `.plan`).
 *
 * It opens on the chart of time: one bar for each step, starting after every task it needs has
 * ended, with the lines between a step and the steps it needs, the step's words beside its bar
 * and the statements it serves at the end of its row. One control switches to the ordered
 * steps, which are the outline's own rows in the same order (`IssueTree`): each task after every
 * task in its `needs`, each row drawing the task's words, the chips naming what it needs, and
 * the line naming the statement it serves, which is the parent the filter draws as no row (R2).
 *
 * Tasks that need each other can be given no place of their own, so the order gives the cycle
 * one place (`plan-order.ts`): the chart draws it there as one line that lists its tasks, and
 * the steps draw that line over the tree and the cycle's tasks as rows at its place, so a step
 * that waits on one of them stands after it and an address naming one lands on a row (R26).
 *
 * ## What today's reads give, and what waits
 *
 * - **The plan's own order** is the order the tasks are written in, read from the blocks of
 *   the plan document the page already holds (`useIssueTree.ts` reads them under the same key,
 *   so this costs no request): the filter's view hands the tasks over grouped by the statement
 *   each serves, which is the spec's order and not the plan's (the tester's F19).
 * - **A bar's place** is worked out here from the `needs` the plan document holds. The place
 *   the server's plan read gives each task waits on dispatch://INFRA-1253 (R8, R11).
 * - **A bar's length** is the task's size, and the `plan_task` block declares none today, so
 *   every bar is one unit long and its row says "No size" (R26). Sizes, and with them the
 *   points of work and the longest path the picture names, wait on dispatch://INFRA-1253; the
 *   axis draws its units as lines and no numbers, since a number would be a size nobody
 *   recorded.
 * - **A step's state** is drawn nowhere: "the model holds none today, so the page draws none,
 *   and the states the pictures show are made up" (R26). dispatch://INFRA-1253 R7 and
 *   dispatch://INFRA-1950 R5.
 * - **Who carries a step out**, and a parent's children's tasks in its own Plan filter, wait on
 *   dispatch://INFRA-3577 and dispatch://INFRA-1253.
 */
export function PlanFilterBody({ root, ...tree }: ComponentProps<typeof IssueTree>): ReactNode {
  // Which of the two the reader is looking at: the chart, which the filter opens on, or the
  // ordered steps.
  const [steps, setSteps] = useState(false);
  const { artifacts } = tree;
  // The documents the tasks are blocks of, and each one's block ids in the order it is written.
  const documentIds = useMemo(
    () => [
      ...new Set(
        root.children.flatMap((node) => {
          const held = documentOfNode(node.id, artifacts);
          return held === undefined ? [] : [held.artifact.id];
        })
      ),
    ],
    [artifacts, root]
  );
  const written = useQueries({
    combine: blockIdsOf,
    queries: documentIds.map((id) => artifactBlocksQuery(id)),
  });
  const entries = useMemo(() => {
    const places = new Map<string, number>();
    documentIds.forEach((id, rank) => {
      written[rank]?.forEach((block, index) => {
        places.set(`${id} ${block}`, rank * 1_000_000 + index);
      });
    });
    const placeOf = (node: TreeNode): number => {
      const held = documentOfNode(node.id, artifacts);
      const place =
        held === undefined ? undefined : places.get(`${held.artifact.id} ${held.blockId}`);
      return place ?? Number.POSITIVE_INFINITY;
    };
    // A task the page holds no place for keeps the place the view gave it, after the rest.
    const tasks = root.children
      .map((node, index) => ({ index, node, place: placeOf(node) }))
      .sort((one, other) =>
        one.place === other.place ? one.index - other.index : one.place - other.place
      )
      .map((held) => held.node);
    return planOrder(tasks);
  }, [artifacts, documentIds, root, written]);
  const cycles = useMemo(
    () => entries.flatMap((entry) => (entry.kind === "cycle" ? [entry] : [])),
    [entries]
  );
  // The steps, in the plan's order, with the tasks of a cycle at the cycle's place.
  const ordered = useMemo(() => {
    const rows = entries.flatMap((entry) => (entry.kind === "step" ? [entry.task] : entry.tasks));
    return { ...root, children: rows, held: rows.reduce((held, row) => held + row.held + 1, 0) };
  }, [entries, root]);
  const view = (pressed: boolean): string =>
    `rounded-full border px-3 py-0.5 text-sm leading-5 font-semibold ${focusVisibleRing} ${
      pressed
        ? `${pageInkBg} ${pageInkBorder} text-page-paper`
        : `${pageHairBorder} ${pagePaperBg} ${pageMutedText}`
    }`;
  const empty = root.children.length === 0;
  return (
    <div className="mt-[22px] flex w-full min-w-0 flex-col" data-plan-body="">
      {/* R26's one control: the chart of time, or the ordered steps. A plan with no task has
          nothing to switch between. */}
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
            onClick={() => setSteps(false)}
            type="button"
            {...pageChrome}
          >
            {PLAN_WORDS.chart}
          </button>
          <button
            aria-pressed={steps}
            className={view(steps)}
            data-plan-view="steps"
            onClick={() => setSteps(true)}
            type="button"
            {...pageChrome}
          >
            {PLAN_WORDS.steps}
          </button>
        </div>
      )}
      {steps || empty ? (
        <>
          {cycles.map((cycle) => (
            <CycleLine cycle={cycle} key={cycle.tasks.map((one) => one.id).join(" ")} />
          ))}
          <IssueTree {...tree} root={ordered} />
        </>
      ) : (
        <PlanChart entries={entries} reveal={tree.reveal?.at(-1)?.id} />
      )}
    </div>
  );
}

/** Each document's block ids in the order they are written, or undefined while its read is
 *  out. It is one function for every render, so the reads' answer is one value until a read
 *  changes (`useQueries`' `combine`). */
function blockIdsOf(
  answers: readonly { readonly data?: readonly { readonly id: string }[] | undefined }[]
): readonly (readonly string[] | undefined)[] {
  return answers.map((answer) => answer.data?.map((block) => block.id));
}

/** One cycle, as one line that lists the tasks in it, each a link that lands on that task
 *  (R26). The line is the prototype's own line above the plan (`.psum`, proto-v3.html :490). */
function CycleLine({ cycle }: { cycle: Pick<PlanCycle, "tasks"> }): ReactNode {
  return (
    <p
      className={`mb-2.5 flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm leading-5 ${pageMutedText}`}
      data-plan-cycle=""
    >
      <span {...pageChrome}>{PLAN_WORDS.cycle}</span>
      {cycle.tasks.map((task) => (
        <TaskLink key={task.id} task={task} />
      ))}
    </p>
  );
}

/** The task's own first line, which is the one line the page draws for it wherever it draws
 *  one (R1, R26: "each bar labelled with the task's first line"). */
function lineOf(task: TreeNode): string {
  return task.text === "" ? task.id : firstLineOf(nodeWords(task)).line;
}

/** One task of a cycle, by its own first line, as the link a reader follows to it. */
function TaskLink({ task }: { task: TreeNode }): ReactNode {
  const route = task.reference === undefined ? undefined : parseNodeReference(task.reference);
  const words = lineOf(task);
  return route === undefined ? (
    <span className="min-w-0 break-words" {...authorText}>
      {words}
    </span>
  ) : (
    <Link
      className={`dispatch-inline-link min-w-0 break-words underline decoration-page-hair underline-offset-[3px] ${pageMutedText}`}
      tabIndex={-1}
      to={buildReferencePath(route)}
      {...authorText}
    >
      {words}
    </Link>
  );
}

/** The chart's three columns, as the prototype's `.prow` divides them: the step, the time, and
 *  what the step serves; one column at 760 px and below, where each row is a card (`.pitem`). */
const CHART_COLUMNS =
  "grid min-w-0 grid-cols-[330px_minmax(0,1fr)_320px] max-[1240px]:grid-cols-[260px_minmax(0,1fr)_220px] max-[760px]:grid-cols-[minmax(0,1fr)]";

/** A column heading, in the prototype's `.cap`. */
const CAP = `text-xs leading-4 font-semibold tracking-[0.08em] uppercase ${pageQuietText}`;

/** The row the reader is on, as the prototype's `.prow.focused` draws it. */
const FOCUSED = "bg-page-step-tint shadow-[inset_3px_0_0_var(--color-page-step)]";

/** A bar's fill, the prototype's hatch in the step's own colours. */
const HATCH =
  "bg-[repeating-linear-gradient(135deg,var(--color-page-step-tint)_0_5px,color-mix(in_srgb,var(--color-page-step)_14%,var(--color-page-step-tint))_5px_10px)]";

/**
 * The chart of time (R26, the prototype's `.chart`): one row per place in the plan's order. A
 * step's row draws its first line and the steps it needs, its bar, and the statements it
 * serves; a cycle's row lists the tasks that need each other. The lines between a bar and the
 * bars it needs are drawn over the rows from where the bars stand.
 *
 * It is the outline the page draws under this view, so it is one tree named as the outline is,
 * and its rows are its items: the focused one is its one tab stop, and the outline's own keys
 * work it, under the ids the outline registers them with (`useTreeKeymap.ts`), so `?` names
 * them here too: the arrows and `j`/`k` reach the bars in order, Home and End the first and the
 * last, `f` steps through the focused row's links, and Enter goes to the step in the
 * conversation, where it is edited. The focus on a row puts its facts in Details.
 */
function PlanChart({
  entries,
  reveal,
}: {
  entries: readonly PlanEntry[];
  /** The node the address names, which takes the focus once per address (R19). */
  reveal: string | undefined;
}): ReactNode {
  const navigate = useNavigate();
  const setFocusedNode = useSetFocusedNode();
  const chart = useRef<HTMLDivElement | null>(null);
  const { end, rows } = useMemo(() => planChart(entries), [entries]);
  const bars = useMemo(() => rows.flatMap((row) => (row.kind === "bar" ? [row] : [])), [rows]);
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the chart's one tab stop rests, which outlives the focus leaving it (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  const tabStop =
    stop !== undefined && bars.some((bar) => bar.task.id === stop) ? stop : bars[0]?.task.id;
  const unit = 100 / end;

  const takeFocus = useCallback(
    (task: TreeNode) => {
      setFocused(task.id);
      setStop(task.id);
      setFocusedNode({
        ...(task.author === undefined ? {} : { author: task.author }),
        id: task.id,
        ...(task.reference === undefined ? {} : { reference: task.reference }),
        title: lineOf(task),
        type: task.type,
      });
    },
    [setFocusedNode]
  );
  // An address that names a step puts the focus on its row, once per address.
  const taken = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (reveal === undefined || taken.current === reveal) return;
    const row = chart.current?.querySelector<HTMLElement>(
      `[data-plan-bar-row][data-node-id="${CSS.escape(reveal)}"]`
    );
    if (row == null) return;
    taken.current = reveal;
    row.focus();
  }, [reveal]);

  // The lines between a bar and the bars it needs, from where the browser laid the bars out:
  // from the end of the needed bar to the start of the one that needs it (the prototype's
  // `drawPlanLinks`). They are read again whenever the chart's box changes.
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
      const box = (id: string) => {
        const bar = element.querySelector(`[data-plan-bar="${CSS.escape(id)}"]`);
        if (bar === null) return undefined;
        const rect = bar.getBoundingClientRect();
        return {
          left: rect.left - frame.left,
          right: rect.right - frame.left,
          y: rect.top - frame.top + rect.height / 2,
        };
      };
      const paths = bars.flatMap((bar) =>
        bar.needs.flatMap((needed) => {
          const from = box(needed.id);
          const to = box(bar.task.id);
          if (from === undefined || to === undefined) return [];
          const bend = Math.max(18, (to.left - from.right) / 2);
          return [
            {
              d: `M${from.right} ${from.y} C${from.right + bend} ${from.y} ${to.left - bend} ${to.y} ${to.left - 1} ${to.y}`,
              of: [needed.id, bar.task.id],
            },
          ];
        })
      );
      setLinks({ height: frame.height, paths, width: frame.width });
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const resized = new ResizeObserver(measure);
    resized.observe(element);
    return () => resized.disconnect();
  }, [bars]);

  /** The bars' rows as the browser holds them, in the order the keys move through them. */
  const items = (): HTMLElement[] => [
    ...(chart.current?.querySelectorAll<HTMLElement>("[data-plan-bar-row]") ?? []),
  ];
  /** Which row the reader is on, a link inside it included, or -1 when the focus is on none. */
  const at = (): number => {
    const active = document.activeElement;
    return active === null
      ? -1
      : items().findIndex((row) => row === active || row.contains(active));
  };
  /** Whether the reader is reading: on the page itself, or on a row of the chart. */
  const reading = (): boolean =>
    document.activeElement === null || document.activeElement === document.body || at() !== -1;
  const moveTo = (index: number): void => {
    const held = items();
    held[Math.min(Math.max(index, 0), held.length - 1)]?.focus();
  };
  useKeymap("issue", [
    {
      id: "tree-next",
      keys: ["j", "ArrowDown"],
      label: "Move to the next row",
      palette: false,
      run: () => moveTo(at() + 1),
      when: () => reading() && items().length > 0,
    },
    {
      id: "tree-previous",
      keys: ["k", "ArrowUp"],
      label: "Move to the previous row",
      palette: false,
      run: () => moveTo(at() - 1),
      when: () => reading() && items().length > 0,
    },
    {
      id: "tree-first",
      keys: "Home",
      label: "Move to the first row",
      palette: false,
      run: () => moveTo(0),
      when: () => at() !== -1,
    },
    {
      id: "tree-last",
      keys: "End",
      label: "Move to the last row",
      palette: false,
      run: () => moveTo(Number.MAX_SAFE_INTEGER),
      when: () => at() !== -1,
    },
    {
      // The statements a step serves are links, and the chart is one tab stop (R9), so a key
      // reaches them; Enter on the link it left the focus on is the browser's own.
      id: "tree-link",
      keys: "f",
      label: "Move to the next link on the focused row",
      palette: false,
      run: () => {
        const links = [...(items()[at()]?.querySelectorAll<HTMLElement>("a[href]") ?? [])];
        const on = links.indexOf(document.activeElement as HTMLElement);
        links[(on + 1) % links.length]?.focus();
      },
      when: () => (items()[at()]?.querySelector("a[href]") ?? null) !== null,
    },
    {
      // Enter on a step goes to it in the conversation, where it is edited: the chart draws
      // each step and edits none. On a link inside the row Enter is the link's own.
      id: "tree-edit",
      keys: "Enter",
      label: "Go to the focused step in the conversation",
      palette: false,
      run: () => {
        const task = bars[items().indexOf(document.activeElement as HTMLElement)]?.task;
        const path = task === undefined ? undefined : conversationPath(task);
        if (path !== undefined) navigate(path);
      },
      when: () => items().includes(document.activeElement as HTMLElement),
    },
  ]);

  return (
    <div
      className={`relative mb-[120px] min-w-0 rounded-[10px] border pb-2 max-[760px]:rounded-none max-[760px]:border-0 max-[760px]:pb-0 ${pageLineBorder} ${pagePaperBg}`}
      data-plan-chart=""
      ref={chart}
    >
      <div
        className={`${CHART_COLUMNS} min-h-10 items-center max-[760px]:hidden`}
        data-plan-head=""
      >
        <span className={`px-3.5 ${CAP}`} {...pageChrome}>
          {PLAN_WORDS.steps}
        </span>
        <Units end={end} unit={unit} />
        <span className={`px-3.5 ${CAP}`} {...pageChrome}>
          {PLAN_WORDS.serves}
        </span>
      </div>
      <div
        aria-label={CONTROL_LABELS.tree}
        className="min-w-0"
        onBlur={(event) => {
          const to = event.relatedTarget;
          if (to instanceof Node && event.currentTarget.contains(to)) return;
          setFocused(undefined);
          if (to instanceof Element && to.closest(`[${DETAILS_ATTRIBUTE}]`) !== null) return;
          setFocusedNode(undefined);
        }}
        role="tree"
      >
        {rows.map((row) =>
          row.kind === "cycle" ? (
            <div
              aria-level={1}
              className={`border-t px-3.5 pt-2 max-[760px]:border-0 max-[760px]:px-0 ${pageLineBorder}`}
              key={row.tasks.map((task) => task.id).join(" ")}
              role="treeitem"
              tabIndex={-1}
            >
              <CycleLine cycle={row} />
            </div>
          ) : (
            <BarRow
              bar={row}
              end={end}
              focused={focused === row.task.id}
              key={row.task.id}
              onFocus={() => takeFocus(row.task)}
              tabStop={tabStop === row.task.id}
              unit={unit}
            />
          )
        )}
      </div>
      {/* Decoration over the rows: the words on each row say what a step needs. The lines of
          the step the reader is on are drawn in the step's colour, as the prototype's are. */}
      <svg
        aria-hidden="true"
        className="pointer-events-none absolute top-0 left-0 overflow-visible max-[760px]:hidden"
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
              stroke={mine ? "var(--color-page-step)" : "var(--color-page-ink)"}
              strokeWidth={mine ? 2 : 1.4}
            />
          );
        })}
      </svg>
    </div>
  );
}

/** The axis: one faint line per unit of the chart (the prototype's `.grid-l`). It draws no
 *  number: a unit is a size, and today's plan holds none (R26). */
function Units({ end, unit }: { end: number; unit: number }): ReactNode {
  return (
    <span
      aria-hidden="true"
      className="absolute inset-0 block max-[760px]:hidden"
      data-plan-axis=""
    >
      {Array.from({ length: end + 1 }, (_unused, tick) => (
        <i
          className="absolute inset-y-0 w-px bg-page-line"
          // biome-ignore lint/suspicious/noArrayIndexKey: a line is its unit and nothing else.
          key={tick}
          style={{ left: `${tick * unit}%` }}
        />
      ))}
    </span>
  );
}

/**
 * One step of the chart (the prototype's `.prow`, and its `.pitem` card at 760 px and below):
 * its first line, that the model holds no size for it and the steps it needs; its bar; and the
 * statements it serves. The row's name is what it draws, which begins with the task's first
 * line, so each bar is labelled with it (R26).
 */
function BarRow({
  bar,
  end,
  focused,
  onFocus,
  tabStop,
  unit,
}: {
  bar: PlanBar;
  /** How many units the chart's axis spans. */
  end: number;
  focused: boolean;
  onFocus: () => void;
  tabStop: boolean;
  /** How wide one unit of the chart is, as a percentage of its column. */
  unit: number;
}): ReactNode {
  const task = bar.task;
  const serves = [
    ...(task.parentLine === undefined || task.parentLine.type !== "requirement"
      ? []
      : [{ line: task.parentLine.line, reference: task.parentLine.reference }]),
    ...task.alsoUnder
      .filter((chip) => chip.name !== "")
      .map((chip) => ({ line: chip.name, reference: chip.ref })),
  ];
  return (
    <div
      aria-level={1}
      className={`${CHART_COLUMNS} min-h-[54px] items-center border-t outline-none max-[760px]:my-2 max-[760px]:rounded-lg max-[760px]:border max-[760px]:border-l-4 max-[760px]:border-l-page-step max-[760px]:px-3 max-[760px]:py-2.5 ${pageLineBorder} ${
        focused ? FOCUSED : ""
      }`}
      data-node-id={task.id}
      data-node-row=""
      data-node-type={task.type}
      data-plan-bar-row=""
      data-plan-start={bar.start}
      onFocus={(event) => {
        if (event.target === event.currentTarget) onFocus();
      }}
      role="treeitem"
      tabIndex={tabStop ? 0 : -1}
    >
      <div className="min-w-0 px-3.5 py-2 max-[760px]:p-0">
        <p
          className={`text-[15px] leading-[22px] break-words ${pageInkText}`}
          data-node-words=""
          {...authorText}
        >
          {lineOf(task)}
        </p>
        <p
          className={`mt-px flex min-w-0 flex-wrap gap-x-1.5 text-xs leading-[18px] ${pageQuietText}`}
          data-plan-facts=""
        >
          <span data-plan-no-size="" {...pageChrome}>
            {PLAN_WORDS.no_size}
          </span>
          {bar.needs.length === 0 ? null : (
            <>
              <span {...pageChrome}>· {PLAN_WORDS.needs}</span>
              {bar.needs.map((needed) => (
                <span data-plan-need={needed.id} key={needed.id} {...authorText}>
                  {chipWords(lineOf(needed))}
                </span>
              ))}
            </>
          )}
        </p>
      </div>
      <div className="relative h-[54px] min-w-0 max-[760px]:my-2 max-[760px]:h-2.5 max-[760px]:rounded-[5px] max-[760px]:bg-page-mark">
        <Units end={end} unit={unit} />
        <span
          aria-hidden="true"
          className={`absolute top-4 block h-[22px] rounded-[5px] border-[1.5px] border-page-step max-[760px]:top-0 max-[760px]:h-full ${HATCH}`}
          data-plan-bar={task.id}
          style={{ left: `${bar.start * unit}%`, width: `${unit}%` }}
        />
      </div>
      <div
        className="flex min-w-0 flex-wrap items-start gap-[5px] px-3.5 py-2 max-[760px]:p-0"
        data-plan-serves=""
      >
        {serves.map((one) => {
          const route = one.reference === undefined ? undefined : parseNodeReference(one.reference);
          const look = `rounded-[5px] border px-1.5 text-xs leading-[18px] font-semibold text-page-statement ${pageHairBorder}`;
          return route === undefined ? (
            <span className={look} data-plan-serve="" key={one.line} {...authorText}>
              {chipWords(one.line)}
            </span>
          ) : (
            <Link
              className={`dispatch-inline-link ${look}`}
              data-plan-serve=""
              key={one.reference}
              tabIndex={-1}
              to={buildReferencePath(route)}
              {...authorText}
            >
              {chipWords(one.line)}
            </Link>
          );
        })}
      </div>
    </div>
  );
}
