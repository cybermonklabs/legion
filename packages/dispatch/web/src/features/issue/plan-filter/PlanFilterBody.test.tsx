import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import type { Artifact } from "../../../api/types";
import { MarginProvider } from "../../margin/margin-context";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import type { NodeChip, ParentLine, TreeNode } from "../issue-tree";
import {
  BLOCK_ATTRIBUTE_LABELS,
  EMPTY_STATE_LINES,
  type NodeType,
  PLAN_WORDS,
} from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { PlanFilterBody } from "./PlanFilterBody";

/**
 * What the Plan filter's body draws (dispatch://INFRA-1196 R26, ruling 5 of build-rulings-md):
 * it opens on the chart of time - one bar per step, after every step it needs, each row saying
 * what it needs, that it holds no size and what it serves - and one control switches to the
 * ordered steps, which are the outline's own rows; a cycle is one line that lists its tasks.
 *
 * The nodes are built as `presets.ts`'s `filterView` leaves them for the Plan filter: each task
 * a row of its own, the statement it serves its parent line, and any further statement a chip
 * naming what it is also under.
 */

const KEY = "CORE-1";
const PLAN = "plan-md";
const SPEC = "spec-md";

/** happy-dom answers every media query against its own window; the tree reads one to tell the
 *  width it is read at (`IssueTree.tsx`'s `ZOOMED_TREE_QUERY`), so a test sets it. */
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

function taskRef(blockId: string): string {
  return `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`;
}

/** The chip the tree draws for one `needs` pointer of a task's block (`tree-drafts.ts`'s
 *  `placeBlocks`): the target's name under the attribute's own word. */
function need(blockId: string): NodeChip {
  return {
    answered: true,
    gone: false,
    label: BLOCK_ATTRIBUTE_LABELS.needs,
    name: `Task ${blockId}`,
    ref: taskRef(blockId),
  };
}

/** One statement, as the line a task's row draws for the parent the filter draws as no row. */
function serves(blockId: string, line: string): ParentLine {
  return {
    id: blockNodeId(SPEC, blockId),
    line,
    reference: `dispatch://${KEY}/spec#b-${blockId}`,
    type: "requirement",
  };
}

/** One plan task, by its block id: what it needs, by their block ids, and the statement it
 *  serves, where the plan gives it one. */
function task(
  blockId: string,
  {
    needs = [],
    parentLine,
    type = "plan_task",
  }: { needs?: readonly string[]; parentLine?: ParentLine; type?: NodeType } = {}
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
    question: false,
    reference: taskRef(blockId),
    replaced: false,
    text: `Task ${blockId}`,
    transcluded: false,
    type,
    waiting: false,
  };
}

function issue(children: readonly TreeNode[]): TreeNode {
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

/** The plan document, as the issue's read names it. */
const PLAN_ARTIFACT = "artifact-plan";

/** Draws the body over `root`. `written` is the block ids of the plan document in the order
 *  they are written, as the page's own blocks read holds them (`useIssueTree.ts`); where a case
 *  gives none the page holds no document, as on a tree drawn before that read answered. */
function renderBody(
  root: TreeNode,
  reveal?: readonly TreeNode[],
  written?: readonly string[]
): RenderResult {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const artifacts = new Map<string, Artifact>();
  if (written !== undefined) {
    artifacts.set(PLAN, { id: PLAN_ARTIFACT, slug: PLAN } as Artifact);
    client.setQueryData(
      ["artifact", PLAN_ARTIFACT, "blocks"],
      written.map((id) => ({ id }))
    );
  }
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <FocusedNodeProvider>
              <IssueScope />
              <Place />
              <PlanFilterBody
                artifacts={artifacts}
                context={new Set()}
                emptyLine={EMPTY_STATE_LINES.plan}
                filtering
                focusReveal
                holdsEscape={false}
                isClosed={false}
                onEditing={() => {}}
                reveal={reveal}
                root={root}
                rootRow={false}
              />
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

const D = (blockId: string): string => blockNodeId(PLAN, blockId);

/** The ids of the rows the body drew, in the order it drew them. */
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

/** Which row holds the focus, by its node's id. */
function focusedRow(): string | null {
  return document.activeElement?.getAttribute("data-node-id") ?? null;
}

async function showSteps(view: RenderResult): Promise<void> {
  await userEvent.click(view.getByRole("button", { name: PLAN_WORDS.steps }));
}

// Ruling 5: "The Plan filter opens on the chart, and R26's control switches to the ordered
// steps." R26: "one bar per task, starting after every task it needs has ended ... each bar
// labelled with the task's first line ... a task with no size is a short bar marked No size".
test("the plan opens on the chart: one bar per step, each after every step it needs", () => {
  setViewport(1440);
  // Written in the plan as t3, t1, t2: t3 needs t1 and t2 needs t3.
  const view = renderBody(
    issue([
      task("t3", { needs: ["t1"], parentLine: serves("r2", "The outline draws one body") }),
      task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
      task("t2", { needs: ["t3"], parentLine: serves("r1", "The page draws the conversation") }),
    ])
  );
  try {
    expect(view.container.querySelectorAll("[data-plan-chart]")).toHaveLength(1);
    expect(view.getByRole("button", { name: PLAN_WORDS.chart }).getAttribute("aria-pressed")).toBe(
      "true"
    );
    expect(view.getByRole("button", { name: PLAN_WORDS.steps }).getAttribute("aria-pressed")).toBe(
      "false"
    );
    expect(rowIds(view.container)).toEqual([D("t1"), D("t3"), D("t2")]);
    // One bar per step, and each starts one unit after the step it needs: today's model holds
    // no size, so a bar is one unit long and its row says so.
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
    // what the step needs and the statement it serves, as a link to that statement.
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
        one.getAttribute("href"),
      ])
    ).toEqual([["The outline draws one body", `/issues/${KEY}/spec#b-r2`]]);
    expect(rowOf(view, "t1").querySelectorAll("[data-plan-need]")).toHaveLength(0);
    // The chart is the one tree of the page, and one row of it is in the tab order (R9).
    expect(view.container.querySelectorAll('[role="tree"]')).toHaveLength(1);
    expect(
      [...view.container.querySelectorAll('[role="treeitem"][tabindex="0"]')].map((row) =>
        row.getAttribute("data-node-id")
      )
    ).toEqual([D("t1")]);
  } finally {
    view.unmount();
  }
});

// R26: the chart's bars are "reached in order with the keyboard". The keys are the outline's.
test("the keys reach the bars in order, and the focused step's facts go to Details", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const view = renderBody(
    issue([
      task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
      task("t2", { needs: ["t1"] }),
      task("t3", { needs: ["t2"] }),
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
    // Enter on a step goes to it in the conversation, where it is edited.
    act(() => rowOf(view, "t2").focus());
    await user.keyboard("{Enter}");
    expect(place?.getAttribute("data-path")).toBe(`/issues/${KEY}#b-t2`);
  } finally {
    view.unmount();
  }
});

// R19: an address that names a step lands on it, in the view the filter opens on.
test("an address naming a step puts the focus on its bar's row", () => {
  setViewport(1440);
  const named = task("t2", { needs: ["t1"] });
  const root = issue([task("t1"), named]);
  const view = renderBody(root, [root, named]);
  try {
    expect(focusedRow()).toBe(D("t2"));
  } finally {
    view.unmount();
  }
});

test("the control switches to the ordered steps, which are the outline's own rows", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t3", { needs: ["t1"], parentLine: serves("r2", "The outline draws one body") }),
      task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
      task("t2", { needs: ["t3"], parentLine: serves("r1", "The page draws the conversation") }),
    ])
  );
  try {
    await showSteps(view);
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
  } finally {
    view.unmount();
  }
});

// R26: "A cycle is one line that lists the tasks in it." The reviewer's finding on the place:
// the cycle stands where the order puts it, after the step its tasks need and before the step
// that waits on one of them, and an address naming one of its tasks lands on a row.
test("tasks that need each other are one line at their place, and rows at that place as steps", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t6", { needs: ["t5"] }),
      task("t4", { needs: ["t5", "t1"] }),
      task("t5", { needs: ["t4"] }),
      task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
    ])
  );
  try {
    // The chart: t1, then the cycle's one line, then the step that waits on it, two units in.
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
});

test("a task whose needs names a task the plan does not hold keeps the plan's own order", () => {
  setViewport(1440);
  const view = renderBody(
    issue([
      task("t2", { needs: ["t9"], parentLine: serves("r1", "The page draws the conversation") }),
      task("t1"),
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

// The tester's F19: the filter's view hands the tasks over grouped by the statement each one
// serves, which is the spec's order. Where the `needs` leave two tasks free to stand in either
// order, the order the plan is written in decides, and each task is drawn once.
test("tasks free to stand in either order keep the order the plan is written in", () => {
  setViewport(1440);
  // The view's order is t3, t1, t2 (by the statements they serve); the plan is written t1, t2,
  // t3, and t3 needs t1.
  const view = renderBody(
    issue([task("t3", { needs: ["t1"] }), task("t1"), task("t2"), task("t1")]),
    undefined,
    ["t1", "t2", "t3"]
  );
  try {
    expect(rowIds(view.container)).toEqual([D("t1"), D("t2"), D("t3")]);
    expect(["t1", "t2", "t3"].map((id) => rowOf(view, id).getAttribute("data-plan-start"))).toEqual(
      ["0", "0", "1"]
    );
  } finally {
    view.unmount();
  }
});
