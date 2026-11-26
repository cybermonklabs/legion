import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { displayTree } from "../../../__tests__/display-trees";
import { nextFrame } from "../../../__tests__/frames";
import { api } from "../../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../../api/queries";
import type { Artifact, IssueDetails } from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import * as referenceNames from "../../refs/reference-names";
import { type PublishedReferences, ReferenceNamesProvider } from "../../refs/reference-names";
import { buildIssuePath, type IssueRoute } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { artifact, issueDetails } from "../decisions-filter/decisions-fixtures";
import { firstLineOf } from "../first-line";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import type { IssueTreeReads, NodeChip, ParentLine, TreeNode } from "../issue-tree";
import { NodeFacts } from "../NodeFacts";
import { FirstLine } from "../node-line";
import { NO_FAILED_READS } from "../outline-reads";
import {
  BLOCK_ATTRIBUTE_LABELS,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  NODE_FACT_LABELS,
  NODE_TYPE_NAMES,
  type NodeType,
  PLAN_WORDS,
  STATE_LINES,
} from "../page-words";
import { presetView } from "../presets";
import {
  blockReads,
  Publisher,
  publishedChips,
  publishedNames,
  publishingNames,
} from "../projection-fixture";
import { ProvidersSlot } from "../slots/providers";
import { ViewStage } from "../slots/stage";
import { ViewPlanSlot } from "../slots/view-plan";
import { blockNodeId, treeContext } from "../tree-drafts";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswerNode, viewQuery } from "../tree-read/tree-read";
import { PlanFilterBody, type PlanRead, type PlanReads } from "./PlanFilterBody";

/**
 * What the Plan filter's body draws: it opens on the chart of time - one bar per step, after
 * every step it needs and as long as its size, each row saying what it needs, its size or that it
 * holds none, and what it serves - and one control switches to the ordered steps, which are the
 * outline's own rows; a cycle is one line that lists its tasks. The steps stand in the order the
 * Plan view's read answers, each task at the place the read gives it; on an issue with issues
 * under it the tasks of each stand in the same order, each marked with its issue's key.
 *
 * The nodes are built as `presets.ts`'s `filterView` leaves them for the Plan filter: each task
 * a row of its own, the statement it serves its parent line, and any further statement a chip
 * naming what it is also under; and each chip carries the reading the tree names its target by,
 * made by the page's own rule (`tree-drafts.ts`'s `treeContext`) from the reads the page names
 * the tasks and the statements from (`planReads`). The body is drawn under the names store, with
 * what the page publishes for those tasks, their statements and their chips, by that rule too
 * (`publishedPlan`): the view draws the names the store projects from it and names no edge
 * itself. A parent's plan is drawn from the view `presetView` makes of a held Plan read.
 *
 * Some cases draw the Plan view's slot itself (`slots/view-plan.tsx`), over a query client that
 * holds the reads the issue's tree is built from, under the names store, for what the slot hands
 * the body as the reads change and what it publishes: the body kept mounted, a refused read's
 * line and Retry, and the names of a step's needs and statements.
 */

const KEY = "CORE-1";
const PLAN = "plan-md";
const SPEC = "spec-md";

/** happy-dom answers every media query against its own window; the page reads them to tell the
 *  width it is read at, so a test sets it. */
function setViewport(width: number): void {
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
});

// The steps are the outline's rows, whose chips draw nothing until the markdown engine has
// answered (`NodeRow.tsx`'s `Chip`): loaded once before the tests, as the row's own tests load
// it, so a reading of a chip under Steps reads the chip and not the engine's first load.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function taskRef(blockId: string): string {
  return `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`;
}

/** The chip the tree draws for one `needs` pointer of a task's block (`tree-read/tree-nodes.ts`'
 *  `needsChips`), under the attribute's own word; `issue` names it as the tree names the task it
 *  points at, once a test has built every task. */
function need(blockId: string): NodeChip {
  return {
    answered: true,
    gone: false,
    label: BLOCK_ATTRIBUTE_LABELS.needs,
    ref: taskRef(blockId),
  };
}

/** One statement, as a task's row holds the parent the filter draws as no row: its words, and
 *  the server's reading of them, which a line under Steps is projected from. */
function serves(blockId: string, words: string): ParentLine {
  const tree = displayTree(words);
  return {
    id: blockNodeId(SPEC, blockId),
    reference: `dispatch://${KEY}/spec#b-${blockId}`,
    text: words,
    ...(tree === undefined ? {} : { tree }),
    type: "requirement",
  };
}

/** The reads the page names `nodes` from (`blockReads`): the issue's read, which holds the spec
 *  and the plan (`issueRead`); the server's reading of each node's words, the node's own where
 *  it holds one, as a statement does (`serves`), and that of its text where it holds none, since
 *  the server sends one for every block it answers for; and each of `gone` answered as gone, as
 *  the read answers for a block a write removed. */
function planReads(
  nodes: readonly (ParentLine | TreeNode)[],
  gone: readonly string[] = []
): IssueTreeReads {
  const reads = blockReads(
    issueRead(),
    nodes.map((one) => ({ ...one, tree: one.tree ?? displayTree(one.text) }))
  );
  return {
    ...reads,
    targets: [
      ...(reads.targets ?? []),
      ...gone.map((ref) => ({ gone: true, kind: "block", name: "", ref })),
    ],
  };
}

/** The reads the page names a plan's `tasks` from (`planReads`): each task, each statement a
 *  task serves as its parent, and each task a chip of theirs points at that none of them is,
 *  which the read answers gone. */
function tasksReads(tasks: readonly TreeNode[]): IssueTreeReads {
  const held = new Set(tasks.map((one) => one.reference));
  return planReads(
    tasks.flatMap((one) => (one.parentLine === undefined ? [one] : [one, one.parentLine])),
    tasks.flatMap((one) => one.chips.map((chip) => chip.ref)).filter((ref) => !held.has(ref))
  );
}

/** The chip of `statement` on a task that is also under it, as the tree names the statement
 *  (`treeContext`'s `chipOf`) from the reads the page names it from (`planReads`). */
function alsoUnder(statement: ParentLine): NodeChip {
  return treeContext(planReads([statement])).chipOf(statement.reference ?? "");
}

/** One plan task, by its block id: what it needs, by their block ids, the statement it serves,
 *  where the plan gives it one, and its place in the order the Plan view's read answers
 *  (`TreeNode.position`), where the read gives it one: a task of a cycle holds none. */
function task(
  blockId: string,
  {
    needs = [],
    parentLine,
    position,
    type = "plan_task",
  }: {
    needs?: readonly string[];
    parentLine?: ParentLine;
    position?: number;
    type?: NodeType;
  } = {}
): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: needs.map(need),
    facts: [],
    gaps: [],
    held: 0,
    id: blockNodeId(PLAN, blockId),
    ...(parentLine === undefined ? {} : { parentLine }),
    ...(position === undefined ? {} : { position }),
    question: false,
    reference: taskRef(blockId),
    replaced: false,
    text: `Task ${blockId}`,
    transcluded: false,
    type,
    waiting: false,
  };
}

/** The issue's root over a test's tasks, each `needs` chip named as the tree names it, from the
 *  reads the page names the tasks from (`tasksReads`, `treeContext`'s `chipOf`), with its label
 *  spread into a chip of its own, as the filter's view hands the tree over. */
function issue(tasks: readonly TreeNode[]): TreeNode {
  const { chipOf } = treeContext(tasksReads(tasks));
  const children = tasks.map((one) => ({
    ...one,
    chips: one.chips.map((chip) =>
      chip.label === BLOCK_ATTRIBUTE_LABELS.needs
        ? { ...chipOf(chip.ref), label: chip.label }
        : chip
    ),
  }));
  return {
    alsoUnder: [],
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: children.length,
    id: KEY,
    question: false,
    replaced: false,
    text: `${KEY} The issue page draws one ordered body`,
    transcluded: false,
    type: "issue",
    waiting: false,
  };
}

/** The page's own `issue` scope, which every key of the body is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** What the page would hold of the reader's place: the address, and the node Details draws. */
function Place(): ReactNode {
  const { hash, pathname } = useLocation();
  const focused = useFocusedNode();
  return <output data-focused={focused?.id ?? ""} data-path={`${pathname}${hash}`} data-place="" />;
}

/** What the page publishes for `tasks`, by its own rule (`issue-tree.ts`'s `chipNaming`, as
 *  `targetNaming` names the read's targets): each task, and each statement a task serves as its
 *  parent, by the chip the tree names it by from the reads the page names them from
 *  (`tasksReads`, `treeContext`'s `chipOf`), whose reading the names store projects the line a
 *  chip of the chart names it by from (`KnownReferences.lines`); each chip the tasks hold, a
 *  `needs` chip and one of a statement a task is also under, by the reading it carries; and every
 *  one of their addresses as one the page asks for. */
function publishedPlan(tasks: readonly TreeNode[]): PublishedReferences {
  const { chipOf } = treeContext(tasksReads(tasks));
  return publishedChips(
    tasks.flatMap((one) => [
      ...(one.reference === undefined ? [] : [chipOf(one.reference)]),
      ...(one.parentLine?.reference === undefined ? [] : [chipOf(one.parentLine.reference)]),
      ...one.chips,
      ...one.alsoUnder,
    ])
  );
}

/** The plan document, as the issue's read names it. */
const PLAN_ARTIFACT = "artifact-plan";

/** Both reads answered, which every case draws under unless it gives another. */
const ANSWERED: PlanReads = { state: "answered" };

/** A body `renderBody` drew, with `redraw`, which draws it again over another `root` and `reads`,
 *  and where a case gives it, another `published`. */
type DrawnBody = RenderResult & {
  readonly redraw: (root: TreeNode, reads: PlanReads, published?: PublishedReferences) => void;
};

/** Draws the body over `root`, the view's tasks, in the stage the page draws a view in
 *  (`slots/stage.tsx`), which keeps the focus in the view. `plan` is the whole plan's tasks, which the
 *  order and the chart's places are drawn from (`slots/view-plan.tsx`): the view's own tasks
 *  unless a case draws the view a words filter leaves. `reveal` is the chain down to the node the
 *  address names. `reads` is how the plan's and the spec's blocks reads stand, as the slot hands
 *  them on (`slots/view-plan.tsx`): answered unless a case gives another. `published` is what the
 *  page publishes of its batched names read, which is published to the names store the body is
 *  drawn under and handed to the body, as the slot hands it: what it publishes for the whole plan
 *  (`publishedPlan`) where a case gives nothing else, one value for as long as the tree it is
 *  drawn over is the same, as the page's is. That value names by hand what the page and the
 *  Plan view's own read publish together, so no case drawn here proves the slot publishes it: the
 *  case drawn through the slot itself ("a step names the step it needs and the statements it
 *  serves from the view's own tree") hands nothing by hand. `client` is the query client, by
 *  default one whose reads never go stale. `container` is the element the body is drawn in, by
 *  default one the test adds to the page. `redraw` draws the same body again where it is
 *  mounted, over another `root` and `reads`, as the slot does when the issue's reads change. */
function renderBody(
  root: TreeNode,
  {
    client: given,
    container,
    plan,
    published,
    reads = ANSWERED,
    reveal,
  }: {
    client?: QueryClient;
    container?: HTMLElement;
    plan?: readonly TreeNode[];
    published?: PublishedReferences;
    reads?: PlanReads;
    reveal?: readonly TreeNode[];
  } = {}
): DrawnBody {
  const client =
    given ??
    new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
  const artifacts = new Map<string, Artifact>();
  const publications = new WeakMap<TreeNode, PublishedReferences>();
  const publishedFor = (shown: TreeNode): PublishedReferences => {
    const held = publications.get(shown) ?? publishedPlan(plan ?? shown.children);
    publications.set(shown, held);
    return held;
  };
  const draw = (shown: TreeNode, held: PlanReads, names: PublishedReferences): ReactNode => (
    <ReferenceNamesProvider>
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <KeymapProvider>
            <MarginProvider>
              <FocusedNodeProvider>
                <IssueScope />
                <Place />
                <Publisher published={names} />
                <ViewStage>
                  <PlanFilterBody
                    artifacts={artifacts}
                    context={new Set()}
                    emptyLine={EMPTY_STATE_LINES.plan}
                    focusReveal
                    holdsEscape={false}
                    isClosed={false}
                    namesLine={NO_FAILED_READS}
                    onEditing={() => {}}
                    plan={plan ?? shown.children}
                    published={names}
                    reads={held}
                    reveal={reveal}
                    root={shown}
                    rootRow={false}
                  />
                </ViewStage>
              </FocusedNodeProvider>
            </MarginProvider>
          </KeymapProvider>
        </QueryClientProvider>
      </MemoryRouter>
    </ReferenceNamesProvider>
  );
  const view = render(
    draw(root, reads, published ?? publishedFor(root)),
    container === undefined ? {} : { container }
  );
  return {
    ...view,
    redraw: (shown, held, names) => view.rerender(draw(shown, held, names ?? publishedFor(shown))),
  };
}

const D = (blockId: string): string => blockNodeId(PLAN, blockId);

/** The ids of the rows the body drew, in the order it drew them: in the chart a cycle's row too,
 *  by the task it stands for. */
function rowIds(container: HTMLElement): (string | null)[] {
  return [...container.querySelectorAll("[data-node-row]")].map((row) =>
    row.getAttribute("data-node-id")
  );
}

function rowOf(view: RenderResult, blockId: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(
    `[data-node-row][data-node-id="${D(blockId)}"]`
  );
  if (row === null) throw new Error(`the body draws no row for ${blockId}`);
  return row;
}

/** Which row holds the focus, by its node's id: for a cycle's row, the task it stands for. */
function focusedRow(): string | null {
  return document.activeElement?.getAttribute("data-node-id") ?? null;
}

async function showSteps(view: RenderResult): Promise<void> {
  await userEvent.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
}

// The Plan filter opens on the chart, and its one control switches to the ordered steps: one bar
// per task, starting after every task it needs has ended, each bar labelled with the task's
// first line, and a task with no size a short bar marked No size.
test("the plan opens on the chart: one bar per step, each after every step it needs", () => {
  setViewport(1440);
  // The view hands the third task over first, and the read places it second: the third needs the
  // first, and the second the third.
  const whole = issue([
    task("t3", {
      needs: ["t1"],
      parentLine: serves("r2", "The outline draws one body"),
      position: 1,
    }),
    task("t1", { parentLine: serves("r1", "The page draws the conversation"), position: 0 }),
    task("t2", {
      needs: ["t3"],
      parentLine: serves("r1", "The page draws the conversation"),
      position: 2,
    }),
  ]);
  const view = renderBody(whole);
  let start: string | null = null;
  try {
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(1);
    expect(view.getByRole("button", { name: PLAN_WORDS.chart }).getAttribute("aria-pressed")).toBe(
      "true"
    );
    expect(view.getByRole("button", { name: PLAN_WORDS.steps }).getAttribute("aria-pressed")).toBe(
      "false"
    );
    expect(rowIds(view.container)).toEqual([D("t1"), D("t3"), D("t2")]);
    // One bar per step, and each starts one unit after the step it needs: no task here holds a
    // size, so a bar is one unit long and its row says so.
    expect(
      ["t1", "t3", "t2"].map((id) => [
        rowOf(view, id).getAttribute("data-plan-start"),
        rowOf(view, id).querySelectorAll("[data-plan-bar]").length,
        rowOf(view, id).querySelector("[data-plan-no-size]")?.textContent,
        rowOf(view, id).querySelector("[data-plan-bar]")?.getAttribute("style"),
      ])
    ).toEqual([
      ["0", 1, PLAN_WORDS.no_size, expect.stringContaining("left: 0%")],
      ["1", 1, PLAN_WORDS.no_size, expect.stringContaining("left: 33.3")],
      ["2", 1, PLAN_WORDS.no_size, expect.stringContaining("left: 66.6")],
    ]);
    // The row is named by what it draws, which begins with the task's first line, and says
    // what the step needs and the statement it serves, as a chip holding a link to that
    // statement.
    const step = rowOf(view, "t3");
    expect(step.querySelector("[data-node-words]")?.textContent).toBe("Task t3");
    expect(
      [...step.querySelectorAll("[data-plan-need]")].map((one) => [
        one.getAttribute("data-plan-need"),
        one.textContent,
      ])
    ).toEqual([[D("t1"), "Task t1"]]);
    expect(
      [...step.querySelectorAll("[data-plan-serve]")].map((one) => [
        one.textContent,
        one.querySelector("a")?.getAttribute("href"),
      ])
    ).toEqual([["The outline draws one body", `/issues/${KEY}/spec#b-r2`]]);
    expect(rowOf(view, "t1").querySelectorAll("[data-plan-need]")).toHaveLength(0);
    // The chart is the one tree of the page, and one row of it is in the tab order.
    expect(view.container.querySelectorAll('[role="tree"]')).toHaveLength(1);
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-node-id")
      )
    ).toEqual([D("t1")]);
    // A curve for each need between two bars.
    expect(view.container.querySelectorAll("[data-plan-link]")).toHaveLength(2);
    start = rowOf(view, "t2").getAttribute("data-plan-start");
  } finally {
    view.unmount();
  }
  // The words filter leaves the view the last step alone: the chart draws that step alone, where
  // it stands on the whole plan, and names after "needs" the step it needs, which it does not
  // draw; no curve joins it to a bar that is not drawn.
  const filtered = renderBody(
    { ...whole, children: whole.children.filter((one) => one.id === D("t2")), held: 1 },
    { plan: whole.children }
  );
  try {
    expect(rowIds(filtered.container)).toEqual([D("t2")]);
    const last = rowOf(filtered, "t2");
    expect(last.getAttribute("data-plan-start")).toBe(start);
    expect(last.querySelector("[data-plan-bar]")?.getAttribute("style")).toContain("left: 66.6");
    expect(
      [...last.querySelectorAll("[data-plan-need]")].map((one) => [
        one.getAttribute("data-plan-need"),
        one.textContent,
      ])
    ).toEqual([[D("t3"), "Task t3"]]);
    expect(filtered.container.querySelectorAll("[data-plan-link]")).toHaveLength(0);
  } finally {
    filtered.unmount();
  }
  // A need whose chip the tree's names read has not answered yet (`answered: false`, no
  // reading): the row names no need and draws no "needs" with nothing after it, and the curves
  // still join the bars, whatever the names.
  const unanswered = renderBody({
    ...whole,
    children: whole.children.map((one) => ({
      ...one,
      chips: one.chips.map((chip) => ({ ...chip, answered: false, reading: undefined })),
    })),
  });
  try {
    for (const blockId of ["t3", "t2"]) {
      const row = rowOf(unanswered, blockId);
      expect(row.querySelectorAll("[data-plan-need]")).toHaveLength(0);
      expect(row.querySelector("[data-plan-facts]")?.textContent).not.toContain(PLAN_WORDS.needs);
    }
    expect(unanswered.container.querySelectorAll("[data-plan-link]")).toHaveLength(2);
  } finally {
    unanswered.unmount();
  }
  // A first line `firstLineOf` cuts inside its sentence is drawn with an ellipsis, on the bar's
  // row and in a cycle's line, as the outline's row draws the same task under "Steps"
  // (`drawnLine`).
  const long = (blockId: string): string =>
    `Task ${blockId} ${"carries its one sentence on ".repeat(5)}to its end`;
  const cut = (blockId: string): string => {
    const first = firstLineOf(long(blockId));
    expect(first.cut).toBe(true);
    return `${first.line}…`;
  };
  const cutView = renderBody(
    issue([
      { ...task("t1", { position: 0 }), text: long("t1") },
      { ...task("t2", { needs: ["t3"] }), text: long("t2") },
      task("t3", { needs: ["t2"] }),
    ])
  );
  try {
    expect(rowOf(cutView, "t1").querySelector("[data-node-words]")?.textContent).toBe(cut("t1"));
    expect(
      [...cutView.container.querySelectorAll("[data-plan-cycle] a")].map((one) => one.textContent)
    ).toEqual([cut("t2"), "Task t3"]);
  } finally {
    cutView.unmount();
  }
});

// A task is named by its own one line, projected from its words as the outline projects its
// folded line (`useTaskLine`), not by its stored text: the cycle line names a task whose words
// hold markdown by those words as they read, with no syntax of the source. Every other row's
// tasks have plain words, which read the same either way.
test("a cycle names a task by its projected line, not its stored marks", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      { ...task("t2", { needs: ["t3"] }), text: "**Draw** the `rows` first" },
      task("t3", { needs: ["t2"] }),
    ])
  );
  try {
    await waitFor(() =>
      expect(
        [...view.container.querySelectorAll("[data-plan-cycle] a")].map((one) => one.textContent)
      ).toEqual(["Draw the rows first", "Task t3"])
    );
  } finally {
    view.unmount();
  }
});

// A step's line is the task's own one line, read as the outline reads the same task's folded
// line (`useTaskLine`, `node-line.tsx`'s `useNodeLine`), and no title over one record: an
// address of another node in it reads by the name the page published for that node from its
// batched read, an issue's key and then its title, and not as its short form, as the line the
// names store projects beside the task's name reads it (`KnownReferences.lines`).
test("a step's line reads a reference by its target's name, as the task's folded line does", async () => {
  setViewport(1440);
  const cited = "dispatch://CORE-2";
  const names = new Map([[cited, "Ship the release"]]);
  const words = `Wait for ${cited} to land.`;
  const tree = displayTree(words);
  const waiting: TreeNode = {
    ...task("t1", { position: 0 }),
    text: words,
    ...(tree === undefined ? {} : { tree }),
  };
  const root = issue([waiting]);
  // What the page publishes for the plan, and the name its batched read gave the issue the task
  // cites, as one value, as the page's one read publishes both.
  const plan = publishedPlan(root.children);
  const issueName = publishedNames(names);
  const published: PublishedReferences = {
    ...plan,
    asked: new Set([...plan.asked, ...issueName.asked]),
    readings: new Map([...plan.readings, ...issueName.readings]),
  };
  const line = "Wait for CORE-2 Ship the release to land.";
  const view = renderBody(root, { published });
  try {
    await waitFor(() =>
      expect(rowOf(view, "t1").querySelector("[data-node-words]")?.textContent).toBe(line)
    );
  } finally {
    view.unmount();
  }
  const folded = render(<FirstLine node={waiting} />, { wrapper: publishingNames(names) });
  try {
    await waitFor(() =>
      expect(folded.container.querySelector("[data-node-first-line]")?.textContent).toBe(line)
    );
  } finally {
    folded.unmount();
  }
});

// A task's serves chips can repeat: two chips of one task can carry the same words, or the same
// reference, since the server refuses a pointer named twice only by its raw value. The row draws
// each of them, and React is never handed two chips under one key, which would drop or reorder
// one of them as the row is drawn again. A chip's words are the line the names store projects
// for its statement (`KnownReferences.lines`), so two chips carry the same words where two
// statements read the same.
test("a bar draws every serves chip, two with the same words or the same reference included", () => {
  setViewport(1440);
  // A chip of a statement the task is also under, carrying the reading the tree names the
  // statement by (`alsoUnder`), which the page publishes for it (`publishedPlan`).
  const same = serves("r3", "The same statement");
  const errors = spyOn(console, "error");
  const view = renderBody(
    issue([
      {
        ...task("t1", { position: 0 }),
        alsoUnder: [
          alsoUnder(serves("r4", "Same words")),
          alsoUnder(serves("r5", "Same words")),
          alsoUnder(same),
          alsoUnder(same),
        ],
      },
    ])
  );
  try {
    expect(
      [...rowOf(view, "t1").querySelectorAll("[data-plan-serve]")].map((one) => [
        one.textContent,
        one.querySelector("a")?.getAttribute("href"),
      ])
    ).toEqual([
      ["Same words", `/issues/${KEY}/spec#b-r4`],
      ["Same words", `/issues/${KEY}/spec#b-r5`],
      ["The same statement", `/issues/${KEY}/spec#b-r3`],
      ["The same statement", `/issues/${KEY}/spec#b-r3`],
    ]);
    expect(
      errors.mock.calls
        .map((call) => call.map(String).join(" "))
        .filter((said) => said.includes("same key")),
      "React was handed two serves chips of one task under one key"
    ).toEqual([]);
  } finally {
    view.unmount();
    errors.mockRestore();
  }
});

// The chart's bars are reached in order with the keyboard, by the outline's keys.
test("the keys reach the bars in order, and the focused step's facts go to Details", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderBody(
    issue([
      task("t1", { parentLine: serves("r1", "The page draws the conversation"), position: 0 }),
      task("t2", { needs: ["t1"], position: 1 }),
      task("t3", { needs: ["t2"], position: 2 }),
    ])
  );
  try {
    const place = view.container.querySelector("[data-place]");
    act(() => rowOf(view, "t1").focus());
    expect(place?.getAttribute("data-focused")).toBe(D("t1"));
    await user.keyboard("{ArrowDown}");
    expect(focusedRow()).toBe(D("t2"));
    expect(place?.getAttribute("data-focused")).toBe(D("t2"));
    await user.keyboard("j");
    expect(focusedRow()).toBe(D("t3"));
    await user.keyboard("j");
    expect(focusedRow()).toBe(D("t3"));
    await user.keyboard("k");
    expect(focusedRow()).toBe(D("t2"));
    await user.keyboard("{Home}");
    expect(focusedRow()).toBe(D("t1"));
    await user.keyboard("{End}");
    expect(focusedRow()).toBe(D("t3"));
    // The tab stop follows the reader, so Tab back into the chart lands where they stood.
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-node-id")
      )
    ).toEqual([D("t3")]);
    // `f` reaches the statement a step serves, which is in no tab order of its own.
    await user.keyboard("{Home}f");
    expect(document.activeElement?.getAttribute("href")).toBe(`/issues/${KEY}/spec#b-r1`);
    // Escape gives the row back from its link, and leaves the chart from the row, as the
    // outline's Escape does.
    await user.keyboard("{Escape}");
    expect(focusedRow()).toBe(D("t1"));
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(document.body);
    // Tab from the page itself enters the chart at its one tab stop, the row the reader last
    // stood on, as Tab enters the outline (`useTreeKeymap.ts`'s `tree-enter`): after Escape the
    // same Tab goes back to the row under either drawing.
    await user.keyboard("{Tab}");
    expect(focusedRow()).toBe(D("t1"));
    await user.keyboard("{End}{Escape}");
    expect(document.activeElement).toBe(document.body);
    await user.keyboard("{Tab}");
    expect(focusedRow()).toBe(D("t3"));
    // A focus inside a row takes that row, as a pointer going down on a statement's link does
    // while another row holds the focus: Details and the one tab stop go with it.
    act(() => rowOf(view, "t2").focus());
    expect(place?.getAttribute("data-focused")).toBe(D("t2"));
    const link = rowOf(view, "t1").querySelector<HTMLElement>("a[href]");
    expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/spec#b-r1`);
    act(() => link?.focus());
    expect(document.activeElement).toBe(link);
    expect(place?.getAttribute("data-focused")).toBe(D("t1"));
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-node-id")
      )
    ).toEqual([D("t1")]);
    // Enter on a step goes to it in the conversation, where it is edited.
    act(() => rowOf(view, "t2").focus());
    await user.keyboard("{Enter}");
    expect(place?.getAttribute("data-path")).toBe(`/issues/${KEY}#b-t2`);
  } finally {
    view.unmount();
  }
});

// Down and Up scroll the page from the page's body, as the outline's do: the chart takes them
// only from a row or a link inside one, where they move as `j` and `k` do, while `j` from the
// page's body still goes to the first bar.
test("Plan: an arrow with the focus on the page's body is not taken", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderBody(
    issue([
      task("t1", { position: 0 }),
      task("t2", { needs: ["t1"], position: 1 }),
      task("t3", { needs: ["t2"], position: 2 }),
    ])
  );
  try {
    // The stage gives the chart's first bar the focus nobody holds; Escape leaves the chart from
    // there for the page's body.
    await waitFor(() => expect(focusedRow()).toBe(D("t1")));
    await user.keyboard("{Escape}");
    expect(document.activeElement).toBe(document.body);
    for (const key of ["ArrowDown", "ArrowUp"]) {
      const event = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key });
      act(() => {
        document.body.dispatchEvent(event);
      });
      expect(event.defaultPrevented, `the chart took ${key} from the page's body`).toBe(false);
      expect(document.activeElement, `${key} from the page's body moved the focus`).toBe(
        document.body
      );
    }
    await user.keyboard("j");
    expect(focusedRow(), "j from the page's body did not go to the first bar").toBe(D("t1"));
    await user.keyboard("{ArrowDown}");
    expect(focusedRow(), "Down from a bar did not go to the next").toBe(D("t2"));
  } finally {
    view.unmount();
  }
});

// An address that names a step lands on it, in the view the filter opens on. The landing focuses
// the row in the frame after the commit that draws it (`IssueTree.tsx`'s `landOnRow`).
test("an address naming a step puts the focus on its bar's row", async () => {
  setViewport(1440);
  const named = task("t2", { needs: ["t1"], position: 1 });
  const root = issue([task("t1", { position: 0 }), named]);
  const view = renderBody(root, { reveal: [root, named] });
  try {
    await waitFor(() => expect(focusedRow()).toBe(D("t2")));
  } finally {
    view.unmount();
  }
});

// An address opened before the view's reads answer - a fresh load, or a link - and one under a
// refused read that a Retry asks again: once the reads answer, the focus lands on the step the
// address names and not on the first bar, which the stage gives a focus nobody holds to where the
// address's row is not the tree's tab stop (`slots/stage.tsx`).
test("an address opened before the reads answer lands on its step once they answer", async () => {
  setViewport(1440);
  const named = task("t2", { needs: ["t1"], position: 1 });
  const root = issue([task("t1", { position: 0 }), named]);
  const opened = renderBody(root, { reads: { state: "waiting" }, reveal: [root, named] });
  try {
    opened.redraw(root, ANSWERED);
    await settled();
    await aFrame();
    expect(
      focusedRow(),
      "an address opened before the reads answer lands on the first bar once they answer"
    ).toBe(D("t2"));
  } finally {
    opened.unmount();
  }
  const asked: PlanRead[] = [];
  const refused = renderBody(root, { reads: failed(asked, "plan"), reveal: [root, named] });
  try {
    await settled();
    expect(document.activeElement).toBe(retryOf(refused, "plan"));
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["plan"]);
    refused.redraw(root, failedAs([], [{ failure: 1, read: "plan", retrying: true }], true));
    refused.redraw(root, ANSWERED);
    await settled();
    await aFrame();
    expect(
      focusedRow(),
      "an address under a refused read lands on the first bar once a Retry has it answer"
    ).toBe(D("t2"));
  } finally {
    refused.unmount();
  }
});

test("the control switches to the ordered steps, which are the outline's own rows", async () => {
  setViewport(1440);
  // The address names the first task's step.
  const named = task("t1", {
    parentLine: serves("r1", "The page draws the conversation"),
    position: 0,
  });
  const root = issue([
    task("t3", {
      needs: ["t1"],
      parentLine: serves("r2", "The outline draws one body"),
      position: 1,
    }),
    named,
    task("t2", {
      needs: ["t3"],
      parentLine: serves("r1", "The page draws the conversation"),
      position: 2,
    }),
  ]);
  const view = renderBody(root, { reveal: [root, named] });
  try {
    // The address's step takes the focus once for the whole view: a press on the switch is the
    // reader's, and leaves the focus on the button pressed, since neither drawing takes the
    // address's step again.
    await waitFor(() => expect(focusedRow()).toBe(D("t1")));
    await showSteps(view);
    expect(document.activeElement).toBe(view.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(0);
    expect(view.getByRole("button", { name: PLAN_WORDS.steps }).getAttribute("aria-pressed")).toBe(
      "true"
    );
    expect(rowIds(view.container)).toEqual([D("t1"), D("t3"), D("t2")]);
    const step = rowOf(view, "t3");
    expect(step.textContent).toContain("Task t1");
    const line = step.querySelector("[data-node-parent-line]");
    expect(line?.getAttribute("data-parent-id")).toBe(blockNodeId(SPEC, "r2"));
    expect(line?.textContent).toContain("The outline draws one body");
    expect(view.container.querySelectorAll('[role="tree"]')).toHaveLength(1);
    // And back to the chart.
    await userEvent.click(view.getByRole("button", { name: PLAN_WORDS.chart }));
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(1);
    expect(document.activeElement).toBe(view.getByRole("button", { name: PLAN_WORDS.chart }));
  } finally {
    view.unmount();
  }
});

// A step's serves chips name each statement it serves as every chip of the page names a block
// (`chipLine`, `chipName`), the statement its parent is (`parentLine`) as the statements it is
// also under: by the line the names store projects from the reading the page published for it,
// its first block that holds words (`KnownReferences.lines`), with no mark the author typed and
// no markdown pass over the words, so words that open on "1." keep it. A statement the store
// holds no line for yet is named by nothing, and draws no chip, until it does.
test("a step's serves chips name each statement by the line the store projects for it, and nothing before", async () => {
  setViewport(1440);
  const other = `dispatch://${KEY}/spec#b-r2`;
  const root = issue([
    {
      ...task("t1", {
        parentLine: serves("r1", "**Stand** the page up first.\n\nThen the rest."),
        position: 0,
      }),
      alsoUnder: [alsoUnder(serves("r2", "1\\. Number the steps."))],
    },
  ]);
  const asked = [taskRef("t1"), `dispatch://${KEY}/spec#b-r1`, other];
  const view = renderBody(root, { published: publishedNames(new Map(), { asked }) });
  const chips = () =>
    [...rowOf(view, "t1").querySelectorAll("[data-plan-serve]")].map((one) => [
      one.textContent,
      one.querySelector("a")?.getAttribute("href"),
    ]);
  try {
    expect(chips(), "a serves chip was drawn before the store held its statement's line").toEqual(
      []
    );
    view.redraw(root, ANSWERED);
    await waitFor(() =>
      expect(chips()).toEqual([
        ["Stand the page up first.", `/issues/${KEY}/spec#b-r1`],
        ["1. Number the steps.", `/issues/${KEY}/spec#b-r2`],
      ])
    );
  } finally {
    view.unmount();
  }
});

// A statement whose words hold no block of words is named by the page's word for its kind, as the
// tree names it (`tree-drafts.ts`'s `treeContext`) and a tie's chip draws it (`NodeRow.tsx`'s
// `Chip`): the names store holds that word as the statement's name and projects no line for it.
// A step's serves chip names it by that word, as the row's needs are named (`chipLine`), the
// statement its parent is (`parentLine`) as one it is also under, and draws no chip of it short.
test("a task that serves a statement with no worded block draws a serves chip named by the statement's kind word", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      {
        ...task("t1", { parentLine: serves("r1", ""), position: 0 }),
        alsoUnder: [alsoUnder(serves("r2", ""))],
      },
    ])
  );
  try {
    await waitFor(() =>
      expect(
        [...rowOf(view, "t1").querySelectorAll("[data-plan-serve]")].map((one) => [
          one.textContent,
          one.querySelector("a")?.getAttribute("href"),
        ])
      ).toEqual([
        [NODE_TYPE_NAMES.requirement, `/issues/${KEY}/spec#b-r1`],
        [NODE_TYPE_NAMES.requirement, `/issues/${KEY}/spec#b-r2`],
      ])
    );
  } finally {
    view.unmount();
  }
});

// The chart's chips, a step's needs and the statements it serves, each name a block as a tie's
// chip names one (`chipLine`, the reading list's `node-row-chip`): by the line the names store
// projects for it, the first block of its words that holds any, so a heading is never run into
// the paragraph after it and no mark the author typed is drawn, cut at a word to a chip's room
// with an ellipsis (`chipName`). A need is words alone, and a serves chip a link to its statement.
test("a step's needs and serves chips name each block by its first worded block, cut to a chip's room", async () => {
  setViewport(1440);
  const root = issue([
    { ...task("t1", { position: 0 }), text: "# Stand the **harness** up\n\nThen read the rows." },
    {
      ...task("t2", { position: 1 }),
      text: "Read every row of the harness and say which one failed.",
    },
    {
      ...task("t3", {
        needs: ["t1", "t2"],
        parentLine: serves("r1", "# Draw the **chart**\n\nThen the rest."),
        position: 2,
      }),
      alsoUnder: [alsoUnder(serves("r2", "Draw each bar of the chart after every bar it needs."))],
    },
  ]);
  const view = renderBody(root);
  try {
    await waitFor(() => {
      expect(
        [...rowOf(view, "t3").querySelectorAll("[data-plan-need]")].map((one) => [
          one.getAttribute("data-plan-need"),
          one.textContent,
        ])
      ).toEqual([
        [D("t1"), "Stand the harness up"],
        [D("t2"), "Read every row of the harness…"],
      ]);
      expect(
        [...rowOf(view, "t3").querySelectorAll("[data-plan-serve]")].map((one) => [
          one.textContent,
          one.querySelector("a")?.getAttribute("href"),
        ])
      ).toEqual([
        ["Draw the chart", `/issues/${KEY}/spec#b-r1`],
        ["Draw each bar of the chart…", `/issues/${KEY}/spec#b-r2`],
      ]);
    });
  } finally {
    view.unmount();
  }
});

// The body hands its rows what the page publishes of its batched names read in the render that
// draws them (`published`), the steps' rows through the outline's tree (`IssueTree`'s
// `published`), and the page publishes to the names store only after the rows have rendered
// (`Publisher`): so a reference the page asks for waits on the page's answer from a row's first
// render, drawn as its author wrote it meanwhile, and is never read by the row; a row that read
// the store alone read it itself first.
test("a step's row waits on the page's read for a reference the page asks for", async () => {
  setViewport(1440);
  const address = "dispatch://CORE-2/spec#b-r1";
  const words = `See ${address} first.`;
  const name = "the queue's drain";
  const getReferences = spyOn(api, "getReferences").mockImplementation(() => new Promise(() => {}));
  const root = issue([{ ...task("t1", { position: 0 }), text: words }]);
  const view = renderBody(root, { published: publishedNames(new Map(), { asked: [address] }) });
  const rowWords = () => rowOf(view, "t1").querySelector("[data-node-words]")?.textContent ?? "";
  try {
    await showSteps(view);
    await waitFor(() => expect(rowWords()).toContain(words));
    expect(getReferences, "a row read the reference the page asks for").not.toHaveBeenCalled();
    view.redraw(root, ANSWERED, publishedNames(new Map([[address, name]])));
    await waitFor(() => expect(rowWords()).toContain(`See ${name} first.`));
    expect(getReferences, "a row read the reference the page named").not.toHaveBeenCalled();
  } finally {
    getReferences.mockRestore();
    view.unmount();
  }
});

/** The spec and the Verification document, by the ids the issue read names them by. */
const SPEC_ARTIFACT = "artifact-spec";
const VERIFICATION_ARTIFACT = "artifact-verification";

/** The issue as its read hands it to the slot (`decisions-fixtures.ts`'s `issueDetails`, with
 *  this file's documents): its spec and its plan, and the Verification document where a case
 *  gives one. */
function issueRead(verification?: Artifact): IssueDetails {
  return {
    ...issueDetails(),
    artifacts: [
      artifact(SPEC_ARTIFACT, SPEC, true),
      { ...artifact(PLAN_ARTIFACT, PLAN), role: "plan" },
      ...(verification === undefined ? [] : [verification]),
    ],
    decisions_artifact_id: "",
    plan_artifact_id: PLAN_ARTIFACT,
    primary_artifact_id: SPEC_ARTIFACT,
    verification_artifact_id: verification?.id ?? "",
    queries: [
      ...(verification === undefined
        ? []
        : [
            {
              query: "verification" as const,
              artifact_id: verification.id,
              takes: ["check", "result"],
              subtree: `issue:${KEY}`,
            },
          ]),
      {
        query: "plan",
        artifact_id: PLAN_ARTIFACT,
        takes: ["plan_task", "plan_role"],
        subtree: `issue:${KEY}`,
      },
    ],
  };
}

/** A plan task of the plan document as the tree read answers it, at `position` in it. */
function taskBlock(id: string, position: number): TreeAnswerNode {
  return treeNode(id, "plan_task", KEY, {
    document: { artifact_id: PLAN_ARTIFACT, position, slug: PLAN },
  });
}

/** What the Plan view's read answers of a task beside its node (`plan`): its place in the order
 *  the read answers, and no size, need, skill or doc. */
function planAt(position: number): NonNullable<TreeAnswerNode["plan"]> {
  return { cycle: false, docs: [], needs: [], position, size: null, skills: [] };
}

/** A query client holding every read the issue's tree is built from (`useIssueTree.ts`), each
 *  answered and never stale, so none is sent: the plan's two tasks, and the spec's blocks, which
 *  a case may leave unheld for the tree read, which holds both, to go to `api.getIssueTree`. */
function slotClient({ specHeld = true }: { specHeld?: boolean } = {}): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(issueCoverageQuery(KEY).queryKey, {
    gaps: [],
    has_plan: true,
    issue: KEY,
    requirements: [],
    spec_artifact_id: SPEC_ARTIFACT,
  });
  client.setQueryData(issueResultsQuery(KEY).queryKey, {
    checks: [],
    issue: KEY,
    verification_artifact_id: "",
  });
  client.setQueryData(issueDecisionsQuery(KEY, true).queryKey, {
    decisions: [],
    document: null,
    open_questions: [],
  });
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  for (const read of ["comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  if (specHeld) {
    client.setQueryData(
      outlineQuery(KEY, "conversation").queryKey,
      treeAnswer([taskBlock("t1", 0), taskBlock("t2", 1)])
    );
  }
  return client;
}

/** A slot `renderSlot` drew, with `redraw`, which draws it again for another read of the issue. */
type DrawnSlot = RenderResult & { readonly redraw: (issue: IssueDetails) => void };

/** Draws the Plan view's slot (`slots/view-plan.tsx`) for `shown` over `client`, in the stage the
 *  page draws a view in (`slots/stage.tsx`), with the providers the page draws every slot under
 *  (`slots/providers.tsx`), under the one names store the application draws every page under
 *  (`app.tsx`'s `ReferenceNamesProvider`), so what the slot publishes names what it draws and
 *  nothing names it by hand. `at` is the address the router holds and the slot's route for it,
 *  where a case gives one; where none is given the router holds no address the page reads, and
 *  the slot is handed the issue's own route. */
function renderSlot(
  client: QueryClient,
  shown: IssueDetails,
  at?: { readonly path: string; readonly route: IssueRoute }
): DrawnSlot {
  const route = at?.route ?? ({ key: KEY, kind: "issue" } as const);
  const draw = (issue: IssueDetails): ReactNode => (
    <MemoryRouter {...(at === undefined ? {} : { initialEntries: [at.path] })}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ReferenceNamesProvider>
              <ProvidersSlot issue={issue} route={route}>
                <IssueScope />
                <ViewStage>
                  <ViewPlanSlot issue={issue} route={route} />
                </ViewStage>
              </ProvidersSlot>
            </ReferenceNamesProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(shown));
  return { ...view, redraw: (issue) => view.rerender(draw(issue)) };
}

test("a Plan block address focuses the task from the view even when the outline has no task", async () => {
  setViewport(1440);
  const client = slotClient({ specHeld: false });
  client.setQueryData(
    outlineQuery(KEY, "conversation").queryKey,
    treeAnswer([treeNode(KEY, "issue", null)])
  );
  client.setQueryData(
    viewQuery(KEY, "plan").queryKey,
    treeAnswer([
      { ...taskBlock("t1", 0), plan: planAt(0), role: "query" },
      { ...taskBlock("t2", 1), plan: planAt(1), role: "query" },
    ])
  );
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = renderSlot(client, issueRead(), {
    path: `/issues/${KEY}/plan#b-t2`,
    route: { key: KEY, kind: "plan" },
  });
  try {
    await waitFor(() => expect(rowOf(view, "t2")).toHaveFocus());
    await userEvent.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
    expect(view.getByRole("button", { name: PLAN_WORDS.steps })).toHaveFocus();
    expect(rowOf(view, "t2")).not.toHaveFocus();
  } finally {
    view.unmount();
    names.mockRestore();
    client.clear();
  }
});

// A Verification or decisions document made while the reader is on Plan rebuilds the issue's
// tree and opens a read of its blocks that holds nothing yet, and the slot draws the same body
// again over the same tasks, still mounted (`slots/view-plan.tsx`): the reader's press of
// "Steps", the focused row and its mark stay.
test("the body drawn again over the same tasks keeps Steps pressed and the focused row marked", async () => {
  setViewport(1440);
  const held = Promise.withResolvers<never>();
  const blocks = spyOn(api, "getArtifactBlocks").mockReturnValue(held.promise as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = renderSlot(slotClient(), issueRead());
  try {
    const steps = () => view.getByRole("button", { name: PLAN_WORDS.steps });
    const card = () => rowOf(view, "t2").querySelector("[data-node-card]")?.className;
    await showSteps(view);
    act(() => rowOf(view, "t2").focus());
    expect([steps().getAttribute("aria-pressed"), focusedRow()]).toEqual(["true", D("t2")]);
    expect(card()).toContain(pageFocusedRow);
    view.redraw(
      issueRead({ ...artifact(VERIFICATION_ARTIFACT, "verification-md"), role: "verification" })
    );
    expect(
      blocks,
      "a new document opens no read of its blocks: the tree read holds them"
    ).not.toHaveBeenCalled();
    expect(
      steps().getAttribute("aria-pressed"),
      "the body drawn again over the same tasks is back on the chart"
    ).toBe("true");
    expect(focusedRow()).toBe(D("t2"));
    expect(card()).toContain(pageFocusedRow);
  } finally {
    view.unmount();
    blocks.mockRestore();
    names.mockRestore();
  }
});

// The Plan view's own read is the one read that holds the plan's tasks and the statement they
// stand under: the outline's read, Conversation's, holds neither, so the page's names read names
// none of them. The view publishes the reading of each block a chip or a parent line of its rows
// names (`presets.ts`' `PresetView.readings`, `slots/view-readings.ts`), and the chart names a
// step's need and the statement it serves by the line the names store projects from it. The case
// draws the slot itself, so nothing hands the body a name the slot does not publish. The view
// worked out again over the same answers names each block by equal readings, and publishes
// nothing new: the store keeps the readings it holds.
test("a step names the step it needs and the statements it serves from the view's own tree", async () => {
  setViewport(1440);
  const client = slotClient({ specHeld: false });
  client.setQueryData(
    outlineQuery(KEY, "conversation").queryKey,
    treeAnswer([treeNode(KEY, "issue", null)])
  );
  const words = (text: string) => ({ text, text_tree: displayTree(text) });
  // A step at `position` in the plan document and at `place` in the order the read answers.
  const step = (id: string, position: number, place: number, text: string): TreeAnswerNode =>
    treeNode(id, "plan_task", "r1", {
      document: { artifact_id: PLAN_ARTIFACT, position, slug: PLAN },
      plan: planAt(place),
      role: "query",
      ...words(text),
    });
  client.setQueryData(
    viewQuery(KEY, "plan").queryKey,
    treeAnswer(
      [
        treeNode("r1", "requirement", KEY, {
          document: { artifact_id: SPEC_ARTIFACT, position: 0, slug: SPEC },
          role: "context",
          ...words("Draw the chart of the plan."),
        }),
        step("t2", 1, 0, "Stand the harness up."),
        step("t1", 0, 1, "Read every row of the harness."),
      ],
      [
        treeTuple("needs", "t1", "t2"),
        treeTuple("serves", "t1", "r1"),
        treeTuple("serves", "t2", "r1"),
      ],
      { view: "plan" }
    )
  );
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const publishes = spyOn(referenceNames, "usePublishReferenceNames");
  // The values the view publishes: the page's own names read answers no target here, so its
  // value names no reading.
  const viewValues = () =>
    publishes.mock.calls.flatMap(([value]) =>
      value === undefined || value.readings.size === 0 ? [] : [value]
    );
  const view = renderSlot(client, issueRead(), {
    path: `/issues/${KEY}/plan`,
    route: { key: KEY, kind: "plan" },
  });
  try {
    await waitFor(() => {
      expect(
        [...rowOf(view, "t1").querySelectorAll("[data-plan-need]")].map((one) => [
          one.getAttribute("data-plan-need"),
          one.textContent,
        ]),
        "t1 does not name the step it needs"
      ).toEqual([[D("t2"), "Stand the harness up."]]);
      expect(
        [...rowOf(view, "t1").querySelectorAll("[data-plan-serve]")].map((one) => [
          one.textContent,
          one.querySelector("a")?.getAttribute("href"),
        ]),
        "t1 does not name the statement it serves"
      ).toEqual([["Draw the chart of the plan.", expect.stringMatching(/#b-r1$/)]]);
    });

    const before = viewValues();
    const held = before.at(-1)?.readings;
    expect(held, "the view publishes no reading").toBeDefined();
    view.redraw(issueRead());
    await waitFor(() => expect(viewValues().length).toBeGreaterThan(before.length));
    expect(
      viewValues()
        .slice(before.length)
        .filter((value) => value.readings !== held),
      "the view worked out again over the same answers published its readings anew"
    ).toEqual([]);
  } finally {
    view.unmount();
    names.mockRestore();
    publishes.mockRestore();
    client.clear();
  }
});

// The spec's blocks read refused before it first answered: the view says, in the page's words,
// that it could not load the spec, with a Retry that asks the spec's blocks again.
test("a refused read of the spec's blocks says so, with a Retry that reads them again", async () => {
  setViewport(1440);
  const blocks = spyOn(api, "getIssueTree").mockRejectedValue(new Error("refused"));
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = renderSlot(slotClient({ specHeld: false }), issueRead());
  try {
    const line = await waitFor(() => {
      const found = view.container.querySelector('[data-plan-unreadable="statements"]');
      if (found === null) throw new Error("the view says nothing of the refused statements read");
      return found;
    });
    expect(line.querySelector("p")?.textContent).toBe(STATE_LINES.spec_unreadable);
    expect(view.container.querySelectorAll("[data-plan-unreadable]")).toHaveLength(1);
    const asked = () => blocks.mock.calls.filter(([, query]) => query?.view === undefined).length;
    const before = asked();
    expect(before, "the tree sends no read of the spec's blocks").toBe(1);
    await userEvent.click(retryOf(view, "statements"));
    await waitFor(() =>
      expect(asked(), "Retry on the statements line does not read the spec's blocks").toBe(
        before + 1
      )
    );
  } finally {
    view.unmount();
    blocks.mockRestore();
    names.mockRestore();
  }
});

// At the Plan view's own address the plan's blocks are Plan's own view read and the spec's the
// outline read (`tree-read/tree-read.ts`'s `blocksOf`). With both refused the view draws one line,
// the statements', whose one Retry reads each of the two again, and is busy while either runs:
// while Plan's view read runs on after the outline read was refused again.
test("at the Plan view's address the one Retry reads both refused reads again, busy while either runs", async () => {
  setViewport(1440);
  const outlineAgain = Promise.withResolvers<never>();
  const viewAgain = Promise.withResolvers<unknown>();
  const sent = { outline: 0, view: 0 };
  // Each read's first answer is a refusal, and each later read of it is out until the case ends it.
  const blocks = spyOn(api, "getIssueTree").mockImplementation(((
    _key: string,
    query?: { readonly view?: string }
  ) => {
    const read = query?.view === undefined ? "outline" : "view";
    sent[read] += 1;
    if (sent[read] === 1) return Promise.reject(new Error("refused"));
    return read === "outline" ? outlineAgain.promise : viewAgain.promise;
  }) as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const client = slotClient({ specHeld: false });
  // Plan's view read refused before the view is drawn, as the load leaves it, which the stage
  // sends (`ViewReadProvider`) and this case draws no stage for.
  await client.prefetchQuery(viewQuery(KEY, "plan"));
  const view = renderSlot(client, issueRead(), {
    path: `/issues/${KEY}/plan`,
    route: { key: KEY, kind: "plan" },
  });
  try {
    await waitFor(() => {
      if (view.container.querySelector('[data-plan-unreadable="statements"]') === null) {
        throw new Error("the view says nothing of the refused statements read");
      }
    });
    expect(view.container.querySelectorAll("[data-plan-unreadable]")).toHaveLength(1);
    expect(sent).toEqual({ outline: 1, view: 1 });
    await userEvent.click(retryOf(view, "statements"));
    await waitFor(() =>
      expect(sent, "Retry did not read each refused read again").toEqual({ outline: 2, view: 2 })
    );
    expect(retryOf(view, "statements")).toHaveAttribute("aria-busy", "true");
    await act(async () => outlineAgain.reject(new Error("refused")));
    await waitFor(() =>
      expect(client.getQueryState(outlineQuery(KEY, "conversation").queryKey)?.fetchStatus).toBe(
        "idle"
      )
    );
    expect(
      retryOf(view, "statements"),
      "the Retry is not busy while Plan's view read runs"
    ).toHaveAttribute("aria-busy", "true");
    await act(async () =>
      viewAgain.resolve(treeAnswer([{ ...taskBlock("t1", 0), plan: planAt(0) }]))
    );
    await waitFor(() =>
      expect(retryOf(view, "statements"), "the Retry is busy with no read out").not.toHaveAttribute(
        "aria-busy"
      )
    );
  } finally {
    view.unmount();
    blocks.mockRestore();
    names.mockRestore();
  }
});

// A read asked again that holds no answer is out again rather than failed while it runs, and its
// line stays drawn through that run, its Retry busy and still holding the focus, so a second
// press asks nothing and the focus never leaves the line (`slots/view-plan.tsx`'s
// `standsFailed`, `FailedReadLine`).
test("a refused read's line stays drawn while its Retry reads again, its Retry busy and focused", async () => {
  setViewport(1440);
  const again = Promise.withResolvers<never>();
  let specReads = 0;
  // The spec's blocks are the tree read's (`useIssueTree`'s `blocksOf`): its first read is the
  // one refused, and every later read of it is out.
  const blocks = spyOn(api, "getIssueTree").mockImplementation(((
    _key: string,
    query?: { readonly view?: string }
  ) => {
    if (query?.view !== undefined) return again.promise;
    specReads += 1;
    return specReads === 1 ? Promise.reject(new Error("refused")) : again.promise;
  }) as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const view = renderSlot(slotClient({ specHeld: false }), issueRead());
  try {
    const line = () => view.container.querySelector('[data-plan-unreadable="statements"]');
    await waitFor(() => {
      if (line() === null) throw new Error("the view says nothing of the refused statements read");
    });
    await userEvent.click(retryOf(view, "statements"));
    await waitFor(() =>
      expect(specReads, "Retry on the statements line does not read the spec's blocks").toBe(2)
    );
    await settled();
    expect(line(), "the statements line leaves while its read runs again").not.toBeNull();
    const retry = retryOf(view, "statements");
    expect(retry, "the Retry of a read running again is not busy").toHaveAttribute(
      "aria-busy",
      "true"
    );
    expect(
      view.container.querySelector(`[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`),
      "the view's empty tree is not busy while its refused read runs again"
    ).toHaveAttribute("aria-busy", "true");
    expect(retry, "the focus leaves the Retry while its read runs again").toHaveFocus();
    await userEvent.click(retry);
    await settled();
    expect(specReads, "a second press of a busy Retry asks the spec's blocks again").toBe(2);
  } finally {
    view.unmount();
    blocks.mockRestore();
    names.mockRestore();
  }
});

// A refused read asked again while the browser is offline is held, not sent: TanStack Query
// pauses its fetch until the network is back, and the read, which holds no answer, goes to
// `pending` with its error cleared and its fetch `paused`, so it is neither in error nor running,
// and keeps its error count. It still stands failed (`refs/reference-target.ts`'s `standsFailed`),
// so its line stays drawn with its Retry.
test("a refused read's line stays drawn while its Retry waits for the network", async () => {
  setViewport(1440);
  const again = Promise.withResolvers<never>();
  let specReads = 0;
  // The spec's blocks are the tree read's, as above.
  const blocks = spyOn(api, "getIssueTree").mockImplementation(((
    _key: string,
    query?: { readonly view?: string }
  ) => {
    if (query?.view !== undefined) return again.promise;
    specReads += 1;
    return specReads === 1 ? Promise.reject(new Error("refused")) : again.promise;
  }) as never);
  const names = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never);
  const client = slotClient({ specHeld: false });
  const view = renderSlot(client, issueRead());
  try {
    const line = () => view.container.querySelector('[data-plan-unreadable="statements"]');
    await waitFor(() => {
      if (line() === null) throw new Error("the view says nothing of the refused statements read");
    });
    onlineManager.setOnline(false);
    await userEvent.click(retryOf(view, "statements"));
    const state = () => client.getQueryState(outlineQuery(KEY, "conversation").queryKey);
    await waitFor(() =>
      expect(state()?.fetchStatus, "Retry offline does not hold the spec's blocks read").toBe(
        "paused"
      )
    );
    await settled();
    const read = state();
    expect(
      {
        data: read?.data,
        errorUpdateCount: read?.errorUpdateCount,
        fetchStatus: read?.fetchStatus,
        status: read?.status,
      },
      "the spec's blocks read asked again offline is not a paused refetch that holds no answer"
    ).toEqual({ data: undefined, errorUpdateCount: 1, fetchStatus: "paused", status: "pending" });
    expect(specReads, "Retry offline sent the spec's blocks read").toBe(1);
    expect(
      line(),
      "the statements line leaves while its Retry waits for the network"
    ).not.toBeNull();
    expect(
      line()?.querySelector("button")?.textContent,
      "the statements line drops its Retry while its Retry waits for the network"
    ).toBe(CONTROL_LABELS.retry);
    expect(
      view.container.querySelector(`[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`),
      "the view's empty tree reads busy while its refused read waits for the network"
    ).toHaveAttribute("aria-busy", "false");
  } finally {
    view.unmount();
    // Back online once the view is gone: its query client no longer listens for the network
    // then, so the held read is never sent.
    onlineManager.setOnline(true);
    blocks.mockRestore();
    names.mockRestore();
  }
});

/** The reads as the slot hands them on where `reads` failed before they first answered, in the
 *  order given, each after its first failure and not running again, with no read out, each Retry
 *  noting its read in `asked`. */
function failed(asked: PlanRead[], ...reads: PlanRead[]): PlanReads {
  return failedAs(
    asked,
    reads.map((read) => ({ failure: 1, read, retrying: false })),
    false
  );
}

/** The reads as the slot hands them on where each of `lines` failed before it first answered:
 *  which failure it is, and whether it runs again, as a read asked again does; and whether a
 *  read is still out (`out`), as a line that runs again is. */
function failedAs(
  asked: PlanRead[],
  lines: readonly { failure: number; read: PlanRead; retrying: boolean }[],
  out: boolean
): PlanReads {
  return {
    failed: lines.map((line) => ({ ...line, retry: () => asked.push(line.read) })),
    out,
    state: "failed",
  };
}

/** Lets the stage place the focus after a render: it looks again once the page's elements
 *  change, and once a failed line has left with the focus in it, each in a microtask after the
 *  render (`slots/stage.tsx`). */
async function settled(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

/** Lets one frame pass, in which the address's landing focuses its row (`landOnRow`). */
async function aFrame(): Promise<void> {
  await act(async () => {
    await nextFrame();
  });
}

// The view waits on the plan's and the spec's blocks reads (`slots/view-plan.tsx`): while one is
// out its tree holds nothing, and a read refused before it first answered is one line over the
// tree, in the page's words, with a Retry that asks that read again. Neither draws a row, the
// switch or the empty line, and the tree itself stays. With the focus on nothing, the first Retry
// takes it, and Enter on it, once the reads answer, leaves the focus on the chart's first row.
test("while a read is out the view draws nothing, and a read that failed says so with a Retry that takes the focus", async () => {
  setViewport(1440);
  const root = issue([task("t1", { position: 0 }), task("t2", { needs: ["t1"], position: 1 })]);
  const asked: PlanRead[] = [];
  const view = renderBody(root, { reads: { state: "waiting" } });
  try {
    const drawn = () => ({
      charts: view.container.querySelectorAll("[data-plan-chart]").length,
      empty: view.container.textContent?.includes(EMPTY_STATE_LINES.plan),
      rows: rowIds(view.container),
      switches: view.container.querySelectorAll("[data-plan-switch]").length,
      trees: view.container.querySelectorAll('[role="tree"]').length,
    });
    const nothing = { charts: 0, empty: false, rows: [], switches: 0, trees: 1 };
    const lines = () => [...view.container.querySelectorAll("[data-plan-unreadable]")];
    expect(drawn()).toEqual(nothing);
    expect(lines()).toHaveLength(0);

    view.redraw(root, failed(asked, "plan", "statements"));
    await settled();
    expect(drawn()).toEqual(nothing);
    expect(
      lines().map((line) => [
        line.getAttribute("data-plan-unreadable"),
        line.querySelector("p")?.textContent,
        line.closest('[role="tree"]'),
      ])
    ).toEqual([
      ["plan", STATE_LINES.plan_unreadable, null],
      ["statements", STATE_LINES.spec_unreadable, null],
    ]);
    const retry = lines()[0]?.querySelector("button");
    if (retry == null) throw new Error("the plan's line draws no Retry");
    expect(document.activeElement, "the first line's Retry does not take the focus").toBe(retry);
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["plan"]);

    view.redraw(root, ANSWERED);
    await settled();
    expect(lines()).toHaveLength(0);
    expect(rowIds(view.container)).toEqual([D("t1"), D("t2")]);
    expect(view.container.querySelectorAll("[data-plan-switch]")).toHaveLength(1);
    expect(focusedRow(), "Enter on Retry does not leave the focus on the first row").toBe(D("t1"));
  } finally {
    view.unmount();
  }
});

// A read asked again that holds no answer runs again with its line still drawn
// (`slots/view-plan.tsx`), and its Retry reads busy and keeps the focus: after a press of that
// Retry, and after the page asked the read again with no press, on the window's focus or on an
// event. The focus stays in the view all the while, and goes on to the Retry of a read refused
// again, to the first row once the reads answer, or to the view's tree where they answer with no
// task; with both reads refused, the second line's Retry gives it to the first's once its read
// answers. The stage the view is drawn in moves it (`slots/stage.tsx`).
test("the focus stays in the view while a retried read is out, and goes on to a Retry or the first row", async () => {
  setViewport(1440);
  const root = issue([task("t1", { position: 0 }), task("t2", { needs: ["t1"], position: 1 })]);
  /** The plan's read, after its first failure, running again. */
  const planRunsAgain = failedAs([], [{ failure: 1, read: "plan", retrying: true }], true);
  /** Draws the view with `reads` refused and the focus on the Retry of `on`, then runs `steps`. */
  const from = async (
    reads: readonly PlanRead[],
    on: PlanRead,
    steps: (view: DrawnBody, asked: readonly PlanRead[]) => Promise<void>
  ): Promise<void> => {
    const asked: PlanRead[] = [];
    const view = renderBody(root, { reads: failed(asked, ...reads) });
    try {
      act(() => retryOf(view, on).focus());
      expect(document.activeElement).toBe(retryOf(view, on));
      await steps(view, asked);
    } finally {
      view.unmount();
    }
  };
  /** Fails the case where the focus is on the page's body, or anywhere outside the view. */
  const inTheView = (view: RenderResult, step: string): void => {
    expect(document.activeElement, `${step}: the focus fell to the page's body`).not.toBe(
      document.body
    );
    expect(
      view.container.contains(document.activeElement),
      `${step}: the focus left the view`
    ).toBe(true);
  };
  /** Fails the case where the line of `read`, running again, is not drawn, its Retry does not
   *  read busy, or the focus is off that Retry. */
  const runningAgain = (view: RenderResult, read: PlanRead): void => {
    const retry = retryOf(view, read);
    expect(retry.getAttribute("aria-busy"), `the ${read} read's Retry does not read busy`).toBe(
      "true"
    );
    expect(document.activeElement, `the ${read} read running again moved the focus`).toBe(retry);
  };

  // Failed, then running again once its Retry is pressed, then answered.
  await from(["plan"], "plan", async (view, asked) => {
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["plan"]);
    view.redraw(root, planRunsAgain);
    inTheView(view, "the read Retry asked again is out");
    runningAgain(view, "plan");
    view.redraw(root, ANSWERED);
    await settled();
    inTheView(view, "the read Retry asked again has answered");
    expect(
      focusedRow(),
      "the read Retry asked again answered leaves the focus off the first row"
    ).toBe(D("t1"));
  });
  // Failed, then running again once its Retry is pressed, then refused again: the Retry, the same
  // control all the while, keeps the focus.
  await from(["plan"], "plan", async (view) => {
    await userEvent.keyboard("{Enter}");
    view.redraw(root, planRunsAgain);
    inTheView(view, "the read Retry asked again is out");
    runningAgain(view, "plan");
    view.redraw(root, failedAs([], [{ failure: 2, read: "plan", retrying: false }], false));
    inTheView(view, "the read Retry asked again is refused again");
    expect(document.activeElement, "the read refused again leaves the focus off its Retry").toBe(
      retryOf(view, "plan")
    );
  });
  // Failed, then running again once its Retry is pressed, then answered with no task: the view
  // draws the issue's empty line, and the focus goes to its tree, which is all the view holds.
  await from(["plan"], "plan", async (view) => {
    await userEvent.keyboard("{Enter}");
    view.redraw(root, planRunsAgain);
    inTheView(view, "the read Retry asked again is out");
    runningAgain(view, "plan");
    view.redraw(issue([]), ANSWERED);
    await settled();
    inTheView(view, "the read Retry asked again has answered with no task");
    expect(
      document.activeElement,
      "the read Retry asked again answered with no task leaves the focus off the view's tree"
    ).toBe(view.container.querySelector(`[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`));
    expect(view.container.textContent).toContain(EMPTY_STATE_LINES.plan);
    // The tree the focus rests on is described by the empty line, so a screen reader says it.
    const tree = document.activeElement;
    const described = tree?.getAttribute("aria-describedby") ?? "";
    expect(
      described === "" ? undefined : document.getElementById(described)?.textContent,
      "the view's tree is not described by the empty line"
    ).toBe(EMPTY_STATE_LINES.plan);
    expect(tree?.getAttribute("aria-busy"), "the tree of a view whose reads answered is busy").toBe(
      "false"
    );
  });
  // Both refused, and the statements line's Retry pressed: that read runs again with its line
  // drawn, and once it answers its line goes, and the plan's line stays.
  await from(["plan", "statements"], "statements", async (view, asked) => {
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["statements"]);
    view.redraw(
      root,
      failedAs(
        [],
        [
          { failure: 1, read: "plan", retrying: false },
          { failure: 1, read: "statements", retrying: true },
        ],
        true
      )
    );
    inTheView(view, "the statements read Retry asked again is out");
    runningAgain(view, "statements");
    view.redraw(root, failed([], "plan"));
    await settled();
    expect(
      document.activeElement,
      "the statements line's Retry pressed leaves the focus off the plan's Retry"
    ).toBe(retryOf(view, "plan"));
  });
  // Asked again with no press, with the focus on its Retry - on the window's focus, or on an
  // event - and answered.
  await from(["plan"], "plan", async (view, asked) => {
    view.redraw(root, planRunsAgain);
    inTheView(view, "the read the page asked again is out");
    runningAgain(view, "plan");
    view.redraw(root, ANSWERED);
    await settled();
    inTheView(view, "the read the page asked again has answered");
    expect(asked).toEqual([]);
    expect(
      focusedRow(),
      "the read the page asked again answered leaves the focus off the first row"
    ).toBe(D("t1"));
  });
});

// A failed line says its failure in an alert, which a screen reader reads each time the alert is
// drawn anew. A refused read running again that stops with no new refusal - its run paused,
// waiting for the network - keeps the line's alert as it was, so nothing is said again; a new
// refusal draws the alert anew, so the new failure is said. The line counts by the read's own
// failures (`failure`), not by the runs it saw end.
test("a failed line says its failure again only on a new refusal, not when a run stops without one", () => {
  setViewport(1440);
  const root = issue([task("t1", { position: 0 })]);
  const view = renderBody(root, {
    reads: failedAs([], [{ failure: 1, read: "plan", retrying: true }], true),
  });
  try {
    const alert = () =>
      view.container.querySelector('[data-plan-unreadable="plan"] [role="alert"]');
    const running = alert();
    if (running === null) throw new Error("the plan's failed line draws no alert");
    view.redraw(root, failedAs([], [{ failure: 1, read: "plan", retrying: false }], false));
    expect(alert(), "a run that stopped with no new refusal says the failure again").toBe(running);
    view.redraw(root, failedAs([], [{ failure: 2, read: "plan", retrying: false }], false));
    const refused = alert();
    expect(refused, "a new refusal leaves the plan's failed line").not.toBeNull();
    expect(refused, "a new refusal says nothing again").not.toBe(running);
  } finally {
    view.unmount();
  }
});

/** The Retry of the line that says `read` failed. */
function retryOf(view: RenderResult, read: PlanRead): HTMLElement {
  const retry = view.container.querySelector<HTMLElement>(
    `[data-plan-unreadable="${read}"] button`
  );
  if (retry === null) throw new Error(`the line saying the ${read} read failed draws no Retry`);
  return retry;
}

// `t p` with a read refused: the page's chord focuses the view's tree (`useIssueKeymap.ts`'s
// `reachPart`), which holds nothing while the reads have not answered, and the stage hands the
// focus on to the first Retry, from wherever the reader stood: here a control outside the view.
test("the view's tree focused with a read refused hands the focus to its Retry", () => {
  setViewport(1440);
  const control = document.createElement("button");
  document.body.append(control);
  const view = renderBody(issue([task("t1", { position: 0 })]), { reads: failed([], "plan") });
  try {
    act(() => control.focus());
    expect(document.activeElement).toBe(control);
    const tree = view.container.querySelector<HTMLElement>(
      `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`
    );
    act(() => tree?.focus({ preventScroll: true }));
    expect(document.activeElement, "the tree focused with a read refused keeps the focus").toBe(
      retryOf(view, "plan")
    );
  } finally {
    view.unmount();
    control.remove();
  }
});

// A focus that moves onto the view's tree from inside the view - a reader moving on from the
// Retry, or a screen reader's cursor, which moves the focus onto what it reads - is the reader's
// own, and stays on the tree: only a focus from outside the view is handed on to the Retry.
test("a focus moved onto the view's tree from its Retry stays on the tree", async () => {
  setViewport(1440);
  const view = renderBody(issue([task("t1", { position: 0 })]), { reads: failed([], "plan") });
  try {
    const tree = view.container.querySelector<HTMLElement>(
      `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`
    );
    if (tree === null) throw new Error("the view draws no tree");
    // The stage gives the Retry the focus as the view is drawn, and counts a focus moved off it
    // onto the tree in that same frame as the chord's (`slots/stage.tsx`'s `placed`): a reader
    // moves a frame later at the soonest.
    await aFrame();
    act(() => retryOf(view, "plan").focus());
    expect(document.activeElement).toBe(retryOf(view, "plan"));
    act(() => tree.focus({ preventScroll: true }));
    expect(
      document.activeElement,
      "a focus moved from the Retry onto the tree is sent back to the Retry"
    ).toBe(tree);
    // The tree holds nothing while its reads have not answered, and reads busy only while a read
    // is out: a refused read that is not running again leaves it not busy.
    expect(
      tree.getAttribute("aria-busy"),
      "the tree of a view whose refused read is not running reads busy"
    ).toBe("false");
  } finally {
    view.unmount();
  }
});

// A read still out beside one that was refused - here the spec's blocks not yet answered, and
// the plan's refused - leaves the view's tree busy: it holds nothing because a read has not
// answered yet. Once the spec's answer is in, only the refused read is left, waiting for its
// Retry, and the tree is not busy. The plan's and the spec's blocks are the one tree read on the
// page (`slots/view-plan.tsx`), so the body is drawn with the reads as a slot hands them on.
test("the view's tree reads busy while a read is out beside a refused one, and not once it answers", async () => {
  setViewport(1440);
  const root = issue([task("t1", { position: 0 })]);
  const view = renderBody(root, {
    reads: failedAs([], [{ failure: 1, read: "plan", retrying: false }], true),
  });
  try {
    const tree = view.container.querySelector(`[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`);
    const lines = () =>
      [...view.container.querySelectorAll("[data-plan-unreadable]")].map((line) =>
        line.getAttribute("data-plan-unreadable")
      );
    // jest-dom: the failed lines are read as the list of the reads they name, in their order.
    expect(lines(), "the view says nothing of the refused plan read").toEqual(["plan"]);
    await settled();
    expect(
      tree,
      "the tree of a view whose spec read is out beside a refused one is not busy"
    ).toHaveAttribute("aria-busy", "true");
    view.redraw(root, failed([], "plan"));
    await settled();
    expect(
      tree,
      "the tree of a view whose one read not answered is refused reads busy"
    ).toHaveAttribute("aria-busy", "false");
    // jest-dom: the failed lines are read as the list of the reads they name, in their order.
    expect(lines(), "the refused plan read's line leaves once the spec's blocks answer").toEqual([
      "plan",
    ]);
  } finally {
    view.unmount();
  }
});

// A reader who moved the focus from the Retry onto the view's tree keeps it there, and the tree
// leaves the page once the reads answer with tasks: the focus goes on to the first row, as it
// does from a failed line that leaves with the focus in it (`slots/stage.tsx`).
test("the view's tree leaving the page with the focus on it gives the focus to the first row", async () => {
  setViewport(1440);
  const root = issue([task("t1", { position: 0 }), task("t2", { needs: ["t1"], position: 1 })]);
  const view = renderBody(root, { reads: failed([], "plan") });
  try {
    const tree = view.container.querySelector<HTMLElement>(
      `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`
    );
    if (tree === null) throw new Error("the view draws no tree");
    // A reader moves a frame after the stage gave the Retry the focus at the soonest, as above.
    await aFrame();
    act(() => retryOf(view, "plan").focus());
    act(() => tree.focus({ preventScroll: true }));
    expect(document.activeElement).toBe(tree);
    view.redraw(root, ANSWERED);
    await settled();
    expect(
      document.activeElement,
      "the view's tree leaving the page with the focus on it drops the focus to the page's body"
    ).not.toBe(document.body);
    expect(
      focusedRow(),
      "the view's tree leaving the page with the focus on it leaves the focus off the first row"
    ).toBe(D("t1"));
  } finally {
    view.unmount();
  }
});

// A page opened at the view's address from another issue starts with the focus on the app's main
// element (`app.tsx`), which holds the view: nothing holds that focus, so a Retry drawn then
// takes it, as it takes a focus on the page itself.
test("a Retry drawn while the element around the view holds the focus takes it", async () => {
  setViewport(1440);
  const main = document.createElement("div");
  main.tabIndex = -1;
  document.body.append(main);
  const root = issue([task("t1", { position: 0 })]);
  const view = renderBody(root, { container: main, reads: { state: "waiting" } });
  try {
    act(() => main.focus());
    expect(document.activeElement).toBe(main);
    view.redraw(root, failed([], "plan"));
    await settled();
    expect(
      document.activeElement,
      "a read refused while the element around the view holds the focus leaves the focus off its Retry"
    ).toBe(retryOf(view, "plan"));
  } finally {
    view.unmount();
    main.remove();
  }
});

// The focus leaving the chart lets go of its row by the outline's own rule (`IssueTree.tsx`'s
// `useReleaseFocus`): with no press under way, the row's mark and its heavier curves go at once,
// and Details goes back to the issue's facts with them; a press that takes the focus elsewhere
// keeps the row while it is under way, and lets go of it once it has ended, in the task after
// its release, where its click has landed.
test("a row lets go of its mark when the focus leaves the chart for another control, at once or once the press has ended", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t3", { needs: ["t1"], position: 1 }),
      task("t1", { position: 0 }),
      task("t2", { needs: ["t3"], position: 2 }),
    ])
  );
  try {
    const chart = view.getByRole("button", { name: PLAN_WORDS.chart });
    const place = view.container.querySelector("[data-place]");
    const marked = () => rowOf(view, "t2").classList.contains("bg-page-step-tint");
    const curve = () =>
      view.container
        .querySelector(`[data-plan-link="${D("t3")} ${D("t2")}"]`)
        ?.getAttribute("stroke-width");
    act(() => rowOf(view, "t2").focus());
    expect([marked(), curve(), place?.getAttribute("data-focused")]).toEqual([true, "2", D("t2")]);
    act(() => chart.focus());
    expect([marked(), curve(), place?.getAttribute("data-focused")]).toEqual([false, "1.4", ""]);

    const user = userEvent.setup();
    act(() => rowOf(view, "t2").focus());
    await user.pointer({ keys: "[MouseLeft>]", target: chart });
    expect(document.activeElement).toBe(chart);
    expect([marked(), curve()]).toEqual([true, "2"]);
    await user.pointer({ keys: "[/MouseLeft]", target: chart });
    await act(async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect([marked(), curve(), place?.getAttribute("data-focused")]).toEqual([false, "1.4", ""]);
  } finally {
    view.unmount();
  }
});

// A cycle is one line that lists the tasks in it. The read gives its tasks no place, and the line
// stands just before the step that waits on one of them, which the read places after the step the
// cycle's tasks need; an address naming one of its tasks lands on a row.
test("tasks that need each other are one line at their place, and rows at that place as steps", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const second = task("t5", { needs: ["t4"] });
  const root = issue([
    task("t6", { needs: ["t5"], position: 1 }),
    task("t4", { needs: ["t5", "t1"] }),
    second,
    task("t1", { parentLine: serves("r1", "The page draws the conversation"), position: 0 }),
  ]);
  // The address names the cycle's second task.
  const view = renderBody(root, { reveal: [root, second] });
  try {
    // The chart: the step the cycle needs, the cycle's line, and two units in the step after it.
    const drawn = [
      ...view.container.querySelectorAll(
        "[data-plan-chart] :is([data-plan-bar-row], [data-plan-cycle])"
      ),
    ].map((row) => row.getAttribute("data-node-id") ?? "cycle");
    expect(drawn).toEqual([D("t1"), "cycle", D("t6")]);
    expect(rowOf(view, "t6").getAttribute("data-plan-start")).toBe("2");
    expect(rowOf(view, "t6").querySelector("[data-plan-need]")?.textContent).toBe("Task t5");
    const lines = [...view.container.querySelectorAll("[data-plan-cycle]")];
    expect(lines).toHaveLength(1);
    expect(lines[0]?.textContent).toContain(PLAN_WORDS.cycle);
    // Each task in the line is the link a reader follows to that task.
    expect(
      [...(lines[0]?.querySelectorAll("a") ?? [])].map((link) => [
        link.textContent,
        link.getAttribute("href"),
      ])
    ).toEqual([
      ["Task t4", `/issues/${KEY}/artifacts/${PLAN}#b-t4`],
      ["Task t5", `/issues/${KEY}/artifacts/${PLAN}#b-t5`],
    ]);
    // The address puts the focus on the cycle's row, which stands for the task it names in
    // Details, and the arrows go from it to the steps on either side of it, as they pass over it.
    const cycle = view.container.querySelector<HTMLElement>("[data-plan-cycle-row]");
    await waitFor(() => expect(document.activeElement).toBe(cycle));
    expect(cycle?.getAttribute("data-plan-cycle-row")).toBe(`${D("t4")} ${D("t5")}`);
    expect(focusedRow()).toBe(D("t5"));
    expect(view.container.querySelector("[data-place]")?.getAttribute("data-focused")).toBe(
      D("t5")
    );
    await user.keyboard("j");
    expect(focusedRow()).toBe(D("t6"));
    act(() => cycle?.focus());
    await user.keyboard("k");
    expect(focusedRow()).toBe(D("t1"));
    // A focus inside the cycle's row, on one of its task links, takes the focus for the row from
    // the step that held it, as a focus inside a step's row does: the row is the one tab stop and
    // Details draws the task the row stands for, the one the address names, not the link's
    // (`CycleRow`).
    const place = view.container.querySelector("[data-place]");
    expect(place?.getAttribute("data-focused")).toBe(D("t1"));
    const link = cycle?.querySelector<HTMLElement>("a[href]") ?? null;
    expect(link?.getAttribute("href")).toBe(`/issues/${KEY}/artifacts/${PLAN}#b-t4`);
    act(() => link?.focus());
    expect(document.activeElement).toBe(link);
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-plan-cycle-row")
      )
    ).toEqual([`${D("t4")} ${D("t5")}`]);
    expect(place?.getAttribute("data-focused")).toBe(D("t5"));

    // The steps: the same line, and the cycle's tasks as rows where the order puts the cycle.
    await showSteps(view);
    expect(rowIds(view.container)).toEqual([D("t1"), D("t4"), D("t5"), D("t6")]);
    expect(view.container.querySelectorAll("[data-plan-cycle]")).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("a plan whose every task is in a cycle draws the line, and its tasks as steps", async () => {
  setViewport(1440);
  const view = renderBody(issue([task("t1", { needs: ["t2"] }), task("t2", { needs: ["t1"] })]));
  try {
    expect(view.container.querySelectorAll("[data-plan-bar-row]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-plan-cycle]")).toHaveLength(1);
    expect(view.container.querySelectorAll('[role="tree"]')).toHaveLength(1);
    // The chart draws no bar, so the cycle's row is its one Tab stop.
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-plan-cycle-row")
      )
    ).toEqual([`${D("t1")} ${D("t2")}`]);
    expect(view.container.textContent).not.toContain(EMPTY_STATE_LINES.plan);
    await showSteps(view);
    expect(rowIds(view.container)).toEqual([D("t1"), D("t2")]);
    expect(view.container.textContent).not.toContain(EMPTY_STATE_LINES.plan);
  } finally {
    view.unmount();
  }
});

test("a plan with no tasks says so in the one line the page has for it, and draws no control", () => {
  setViewport(1440);
  const view = renderBody(issue([]));
  try {
    expect(view.container.querySelectorAll("[data-plan-switch]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-plan-cycle]")).toHaveLength(0);
    expect(view.container.textContent).toContain(EMPTY_STATE_LINES.plan);
  } finally {
    view.unmount();
  }
  // While its reads are out (`slots/view-plan.tsx`) a view with no task is one whose tasks have
  // not arrived, and it draws nothing - no line, no switch and no chart.
  const held = renderBody(issue([]), { reads: { state: "waiting" } });
  try {
    expect(held.container.querySelectorAll("[data-plan-switch]")).toHaveLength(0);
    expect(held.container.querySelectorAll("[data-plan-chart]")).toHaveLength(0);
    expect(held.container.querySelector("[data-plan-body]")?.textContent).toBe("");
  } finally {
    held.unmount();
  }
});

test("a task whose needs names a task the plan does not hold stands where the read places it and waits on nothing", () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t2", {
        needs: ["t9"],
        parentLine: serves("r1", "The page draws the conversation"),
        position: 0,
      }),
      task("t1", { position: 1 }),
    ])
  );
  try {
    expect(rowIds(view.container)).toEqual([D("t2"), D("t1")]);
    expect(["t2", "t1"].map((id) => rowOf(view, id).getAttribute("data-plan-start"))).toEqual([
      "0",
      "0",
    ]);
    expect(view.container.querySelectorAll("[data-plan-need]")).toHaveLength(0);
    expect(view.container.querySelectorAll("[data-plan-cycle]")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

// The filter's view hands the tasks over grouped by the statement each one serves, the first
// twice. The read places the second first, and the first just above the third, which needs it:
// the body draws them in that order, each task once.
test("tasks the view hands over by statement stand in the read's order, each drawn once", () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t3", { needs: ["t1"], position: 2 }),
      task("t1", { position: 1 }),
      task("t2", { position: 0 }),
      task("t1", { position: 1 }),
    ])
  );
  try {
    expect(rowIds(view.container)).toEqual([D("t2"), D("t1"), D("t3")]);
    expect(["t2", "t1", "t3"].map((id) => rowOf(view, id).getAttribute("data-plan-start"))).toEqual(
      ["0", "0", "1"]
    );
  } finally {
    view.unmount();
  }
});

// The order is the read's, which the slot hands the body again when the read changes it, and the
// body sends no read of its own for it: on a client that counts every read stale at once, an
// enabled observer would fetch on mount.
test("the order follows the read's places when the read changes them, and the body sends no blocks read of its own", async () => {
  setViewport(1440);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } });
  const read = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  // The three tasks at the places the read gives them; the third needs the first.
  const placedAt = (first: number, second: number, third: number): TreeNode =>
    issue([
      task("t3", { needs: ["t1"], position: third }),
      task("t1", { position: first }),
      task("t2", { position: second }),
    ]);
  const view = renderBody(placedAt(1, 0, 2), { client });
  try {
    expect(read, "the body sends a read of the plan's blocks of its own").not.toHaveBeenCalled();
    expect(rowIds(view.container)).toEqual([D("t2"), D("t1"), D("t3")]);
    await act(async () => {
      view.redraw(placedAt(0, 2, 1), ANSWERED);
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect(rowIds(view.container)).toEqual([D("t1"), D("t3"), D("t2")]);
    expect(read, "the body sends a read of the plan's blocks of its own").not.toHaveBeenCalled();
  } finally {
    view.unmount();
    read.mockRestore();
  }
});

// A task that holds no words is named by the page's word for its kind wherever the view names it
// - its own row, the line of a cycle it is in and the needs of a task that waits on it - and
// never by its id, which is nobody's word.
test("a task with no words is named by its kind, never by its id", async () => {
  setViewport(1440);
  const blank = (blockId: string, needs: readonly string[] = [], position?: number): TreeNode => ({
    ...task(blockId, { needs, position }),
    text: "",
  });
  const view = renderBody(
    issue([
      blank("t1", [], 0),
      task("t2", { needs: ["t1"], position: 1 }),
      blank("t3", ["t4"]),
      task("t4", { needs: ["t3"] }),
    ])
  );
  try {
    const kind = NODE_TYPE_NAMES.plan_task;
    expect(rowOf(view, "t1").querySelector("[data-node-words]")?.textContent).toBe(kind);
    expect(rowOf(view, "t2").querySelector("[data-plan-need]")?.textContent).toBe(kind);
    const cycles = [...view.container.querySelectorAll("[data-plan-cycle]")];
    expect(cycles).toHaveLength(1);
    expect([...(cycles[0]?.querySelectorAll("a") ?? [])].map((link) => link.textContent)).toEqual([
      kind,
      "Task t4",
    ]);
    for (const id of ["t1", "t3"]) expect(view.container.textContent).not.toContain(D(id));
    // Under "Steps" the row that waits on it names it after "needs" by the same word: the tree's
    // name of its needs chip, read on the chip, since the outline's row draws the kind's tag
    // "plan task" on every task row.
    await showSteps(view);
    expect(rowOf(view, "t2").textContent).toContain(kind);
    expect(rowOf(view, "t2").querySelector("[data-node-chip] [data-chip-name]")?.textContent).toBe(
      kind
    );
    for (const id of ["t1", "t3"]) expect(view.container.textContent).not.toContain(D(id));
  } finally {
    view.unmount();
  }
});

// The view names no need from its own node. The tree hands a `needs` chip over with no reading
// while the names read is out, and where the server's name of the target ends inside its
// directive's opening line, as for a task whose `needs` holds eleven pointers on one line. While
// a chip has no reading the row draws no need for it, whatever the names store holds for the
// task it points at, and "· needs" only when it draws a need: an empty need after "· needs"
// names no task at all.
test("a need the tree has not named draws no empty need and no '· needs' with nothing after it", () => {
  setViewport(1440);
  // The third task needs the first two, the fourth the first alone; the tree names the second
  // and not the first.
  const root = issue([
    task("t1", { position: 0 }),
    task("t2", { position: 1 }),
    task("t3", { needs: ["t1", "t2"], position: 2 }),
    task("t4", { needs: ["t1"], position: 3 }),
  ]);
  const unnamed: TreeNode = {
    ...root,
    children: root.children.map((one) => ({
      ...one,
      chips: one.chips.map((chip) =>
        chip.ref.endsWith("#b-t1") ? { ...chip, reading: undefined } : chip
      ),
    })),
  };
  const view = renderBody(unnamed);
  try {
    const [both, alone] = ["t3", "t4"].map((blockId) => rowOf(view, blockId));
    expect(
      [...(both?.querySelectorAll("[data-plan-need]") ?? [])].map((one) => [
        one.getAttribute("data-plan-need"),
        one.textContent,
      ])
    ).toEqual([[D("t2"), "Task t2"]]);
    expect(both?.querySelector("[data-plan-facts]")?.textContent).toContain(PLAN_WORDS.needs);
    expect(alone?.querySelectorAll("[data-plan-need]")).toHaveLength(0);
    expect(alone?.querySelector("[data-plan-facts]")?.textContent).not.toContain(PLAN_WORDS.needs);
  } finally {
    view.unmount();
  }
});

/** A drawn row by its node's id, whatever the id's shape: a task of another issue is drawn by its
 *  block id alone. */
function rowById(view: RenderResult, id: string): HTMLElement {
  const row = view.container.querySelector<HTMLElement>(
    `[data-node-row][data-node-id="${CSS.escape(id)}"]`
  );
  if (row === null) throw new Error(`the body draws no row for ${id}`);
  return row;
}

/** Each fact a row's body draws under Steps (`NodeFacts`), as its label and its value. */
function stepFactsOf(row: HTMLElement): [string, string][] {
  return [...row.querySelectorAll("[data-node-body] dt")].map((term) => [
    term.textContent ?? "",
    term.nextElementSibling?.textContent ?? "",
  ]);
}

/** How wide a row's bar is, as a share of the chart's time column. */
function barWidth(row: HTMLElement): number {
  const style = row.querySelector("[data-plan-bar]")?.getAttribute("style") ?? "";
  return Number.parseFloat(/width: ([\d.]+)%/.exec(style)?.[1] ?? "NaN");
}

// R19, R26: a step draws its size in words where the read holds one, beside its bar in the chart
// and as a fact of its row under Steps, and "No size" only for the task that holds none. Each bar
// is as long as its task's size on the chart's one scale, and the bar of the task with no size is
// one line of the axis long and dashed. The axis carries the time at each line only where every
// task holds a size.
test("a step draws its size where the read holds one, and No size only where it holds none", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      { ...task("t1", { position: 0 }), size: 90 },
      task("t2", { needs: ["t1"], position: 1 }),
      { ...task("t3", { needs: ["t1"], position: 2 }), size: 45 },
    ])
  );
  try {
    expect(
      ["t1", "t2", "t3"].map((id) => [
        rowOf(view, id).querySelector("[data-plan-size]")?.textContent ?? null,
        rowOf(view, id).querySelector("[data-plan-no-size]")?.textContent ?? null,
      ])
    ).toEqual([
      ["1 h 30 min", null],
      [null, PLAN_WORDS.no_size],
      ["45 min", null],
    ]);
    expect(barWidth(rowOf(view, "t1")) / barWidth(rowOf(view, "t3"))).toBeCloseTo(2);
    expect(rowOf(view, "t2").querySelector("[data-plan-bar]")?.className).toContain(
      "border-dashed"
    );
    expect(rowOf(view, "t1").querySelector("[data-plan-bar]")?.className).toContain("border-solid");
    expect(view.container.querySelectorAll("[data-plan-axis-time]")).toHaveLength(0);
    await showSteps(view);
    expect(["t1", "t2", "t3"].map((id) => stepFactsOf(rowOf(view, id)))).toEqual([
      [[BLOCK_ATTRIBUTE_LABELS.size, "1 h 30 min"]],
      [[BLOCK_ATTRIBUTE_LABELS.size, PLAN_WORDS.no_size]],
      [[BLOCK_ATTRIBUTE_LABELS.size, "45 min"]],
    ]);
  } finally {
    view.unmount();
  }
  const sized = renderBody(
    issue([
      { ...task("t1", { position: 0 }), size: 90 },
      { ...task("t3", { needs: ["t1"], position: 1 }), size: 45 },
    ])
  );
  try {
    expect(
      [...sized.container.querySelectorAll("[data-plan-head] [data-plan-axis-time]")].map(
        (one) => one.textContent
      )
    ).toEqual(["30 min", "1 h", "1 h 30 min", "2 h", "2 h 30 min"]);
  } finally {
    sized.unmount();
  }
});

// R19: on an issue with issues under it the Plan read answers the tasks of every issue under it
// beside the issue's own, as context marked with their issue, and the view takes each as a row
// (`presets.ts`' `heldFilterView`). The chart and the steps draw them in the order the read
// answers, whatever order the view hands them in: the issue's own first and then the child's,
// whose `c` needs its `a`, as on the child's own page. The places leave room for each issue's
// start and done, which the read places too and the view hands over as no task. Each task of the
// child is marked with its issue's key, a link to that issue (`ChildIssueTag`), on its bar's row
// and as a fact of its row under Steps; the issue's own tasks carry none. The key is a control
// inside a row: out of the tab order on every row, the focused one included, as every control of
// a row of the outline or the chart is (the chart's and the outline's `f` reach it), and in it in
// a view whose rows take tab stops (`tabStops`).
test("a parent's plan draws the tasks of each issue under it in the order the read answers, each marked with its issue's key, and its own with none", async () => {
  setViewport(1440);
  const child = "CORE-2";
  const own = (id: string): TreeNode => ({ ...task(id), readId: `${KEY}/${PLAN}#${id}` });
  const childTask = (id: string, needs: readonly string[] = []): TreeNode => ({
    ...task(id),
    chips: needs.map((one) => ({
      ...need(one),
      reading: { words: `Task ${one}` },
      ref: `dispatch://${child}/artifact/${PLAN}#b-${one}`,
    })),
    id,
    readId: `${child}/${PLAN}#${id}`,
    reference: `dispatch://${child}/artifact/${PLAN}#b-${id}`,
  });
  const answer = treeAnswer(
    [
      treeNode("a", "plan_task", KEY, { in: PLAN, plan: planAt(1), role: "query" }),
      treeNode("xa", "plan_task", child, {
        in: PLAN,
        issue: child,
        plan: planAt(5),
        role: "context",
      }),
      treeNode("xb", "plan_task", child, {
        in: PLAN,
        issue: child,
        plan: planAt(4),
        role: "context",
      }),
      treeNode("xc", "plan_task", child, {
        in: PLAN,
        issue: child,
        plan: planAt(6),
        role: "context",
      }),
      treeNode("b", "plan_task", KEY, { in: PLAN, plan: planAt(2), role: "query" }),
    ],
    [],
    { view: "plan" }
  );
  const outline = issue([own("a"), own("b")]);
  const root: TreeNode = {
    ...outline,
    views: {
      plan: answer,
      trees: {
        plan: {
          ...outline,
          children: [childTask("xa"), childTask("xb"), childTask("xc", ["xa"])],
        },
      },
    },
  };
  const whole = presetView(root, "plan", "");
  const view = renderBody(whole.root, { plan: whole.root.children });
  const order = [D("a"), D("b"), "xb", "xa", "xc"];
  const href = buildIssuePath({ key: child, kind: "issue" });
  const marks = () =>
    order.map((id) => {
      const key = rowById(view, id).querySelector("[data-child-key]");
      return key === null
        ? null
        : [key.textContent, key.getAttribute("href"), key.getAttribute("tabindex")];
    });
  const mark = [child, href, "-1"];
  const marked = [null, null, mark, mark, mark];
  try {
    expect(rowIds(view.container)).toEqual(order);
    expect(marks(), "the chart marks the tasks with their issue's key").toEqual(marked);
    await showSteps(view);
    expect(rowIds(view.container)).toEqual(order);
    expect(marks(), "the steps mark the tasks with their issue's key").toEqual(marked);
    expect(stepFactsOf(rowById(view, "xa"))).toEqual([
      [NODE_FACT_LABELS.issue, child],
      [BLOCK_ATTRIBUTE_LABELS.size, PLAN_WORDS.no_size],
    ]);
    act(() => rowById(view, "xa").focus());
    expect(focusedRow()).toBe("xa");
    expect(marks()).toEqual(marked);
  } finally {
    view.unmount();
  }
  const facts = render(
    <MemoryRouter>
      <NodeFacts facts={[{ draw: "issue", name: "issue", value: child }]} tabStops />
    </MemoryRouter>
  );
  try {
    const key = facts.container.querySelector("[data-child-key]");
    expect([key?.getAttribute("href"), key?.getAttribute("tabindex")]).toEqual([href, null]);
  } finally {
    facts.unmount();
  }
});
