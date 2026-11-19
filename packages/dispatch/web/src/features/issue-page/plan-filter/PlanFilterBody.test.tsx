import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../../api/client";
import { issueCoverageQuery, issueResultsQuery } from "../../../api/queries";
import type { Artifact, IssueDetails } from "../../../api/types";
import { pageFocusedRow } from "../../../theme/classes";
import { MarginProvider } from "../../margin/margin-context";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { KeymapProvider } from "../../shell/KeymapProvider";
import { useKeymapScope } from "../../shell/keymap";
import { FocusedNodeProvider, useFocusedNode } from "../focused-node";
import {
  firstLineOf,
  type NodeChip,
  nodeWords,
  type ParentLine,
  type TreeNode,
} from "../issue-tree";
import {
  ANSWER_LINES,
  BLOCK_ATTRIBUTE_LABELS,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  NODE_TYPE_NAMES,
  type NodeType,
  PLAN_WORDS,
} from "../page-words";
import { ProvidersSlot } from "../slots/providers";
import { ViewPlanSlot } from "../slots/view-plan";
import { blockNodeId } from "../tree-drafts";
import { issueTreeQuery, type TreeAnswerNode } from "../tree-read/tree-read";
import { PlanFilterBody, type PlanRead, type PlanReads } from "./PlanFilterBody";

/**
 * What the Plan filter's body draws: it opens on the chart of time - one bar per step, after
 * every step it needs, each row saying what it needs, that it holds no size and what it serves
 * - and one control switches to the ordered steps, which are the outline's own rows; a cycle is
 * one line that lists its tasks.
 *
 * The nodes are built as `presets.ts`'s `filterView` leaves them for the Plan filter: each task
 * a row of its own, the statement it serves its parent line, and any further statement a chip
 * naming what it is also under; and each `needs` chip is named as the tree names it, from the
 * task it points at (`issue`), since the view draws the tree's names and names no edge itself.
 *
 * Two cases draw the Plan view's slot itself (`slots/view-plan.tsx`), over a query client that
 * holds the reads the issue's tree is built from, for what the slot hands the body as the reads
 * change: the body kept mounted, and a refused read's line and Retry.
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

// The steps are the outline's rows, whose chips draw nothing until the markdown engine has
// answered (`NodeRow.tsx`'s `Chip`): loaded once before the tests, as the row's own tests load
// it, so a reading of a chip under Steps reads the chip and not the engine's first load.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function taskRef(blockId: string): string {
  return `dispatch://${KEY}/artifact/${PLAN}#b-${blockId}`;
}

/** The chip the tree draws for one `needs` pointer of a task's block (`tree-drafts.ts`'s
 *  `placeBlocks`), under the attribute's own word; `issue` names it from the task it points at,
 *  once a test has built every task. */
function need(blockId: string): NodeChip {
  return {
    answered: true,
    gone: false,
    label: BLOCK_ATTRIBUTE_LABELS.needs,
    name: "",
    ref: taskRef(blockId),
  };
}

/** `chip` named as the tree names a `needs` chip: by the needed task's first line, by the page's
 *  word for its kind where that task holds no words ("plan task"), and as gone where the plan
 *  holds no such task, which is how the reference read answers for a block a write removed
 *  (`tree-drafts.ts`'s `chipOf`). */
function namedBy(chip: NodeChip, needed: TreeNode | undefined): NodeChip {
  if (needed === undefined) return { ...chip, gone: true, name: "" };
  const line = firstLineOf(nodeWords(needed)).line;
  return { ...chip, name: line === "" ? NODE_TYPE_NAMES[needed.type] : line };
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

/** The issue's root over a test's tasks, each `needs` chip named from the task it points at
 *  among them (`namedBy`), as the filter's view hands the tree over. */
function issue(tasks: readonly TreeNode[]): TreeNode {
  const byReference = new Map(tasks.map((one) => [one.reference, one]));
  const children = tasks.map((one) => ({
    ...one,
    chips: one.chips.map((chip) =>
      chip.label === BLOCK_ATTRIBUTE_LABELS.needs ? namedBy(chip, byReference.get(chip.ref)) : chip
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

/** The plan document, as the issue's read names it. */
const PLAN_ARTIFACT = "artifact-plan";

/** Both reads answered, which every case draws under unless it gives another. */
const ANSWERED: PlanReads = { state: "answered" };

/** A body `renderBody` drew, with `redraw`, which draws it again over another `root` and `reads`. */
type DrawnBody = RenderResult & { readonly redraw: (root: TreeNode, reads: PlanReads) => void };

/** Draws the body over `root`, the view's tasks. `plan` is the whole plan's tasks, which the
 *  order and the chart's places are worked out from (`slots/view-plan.tsx`): the view's own
 *  tasks unless a case draws the view a words filter leaves. `reveal` is the chain down to the
 *  node the address names. `written` is the block ids of the plan document in the order they
 *  are written, as the tree read holds them (`useIssueTree`'s `blocksOf("plan")`); where a case
 *  gives none the page holds no document, as on a tree drawn before that read answered. `reads` is
 *  how the plan's and the spec's blocks reads stand, as the slot hands them on
 *  (`slots/view-plan.tsx`): answered unless a case gives another. `client` is the query client,
 *  by default one whose reads never go stale. `redraw` draws the same body again where it is
 *  mounted, over another `root` and `reads`, as the slot does when the issue's reads change. */
function renderBody(
  root: TreeNode,
  {
    client: given,
    plan,
    reads = ANSWERED,
    reveal,
    written,
  }: {
    client?: QueryClient;
    plan?: readonly TreeNode[];
    reads?: PlanReads;
    reveal?: readonly TreeNode[];
    written?: readonly string[];
  } = {}
): DrawnBody {
  const client =
    given ??
    new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
  const artifacts = new Map<string, Artifact>();
  if (written !== undefined) {
    artifacts.set(PLAN, { id: PLAN_ARTIFACT, slug: PLAN } as Artifact);
  }
  const draw = (shown: TreeNode, held: PlanReads): ReactNode => (
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
                plan={plan ?? shown.children}
                planBlocks={written?.map((id) => ({ id }))}
                reads={held}
                reveal={reveal}
                root={shown}
                rootRow={false}
              />
            </FocusedNodeProvider>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(root, reads));
  return { ...view, redraw: (shown, held) => view.rerender(draw(shown, held)) };
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
  // The plan writes the third task first: the third needs the first, and the second the third.
  const whole = issue([
    task("t3", { needs: ["t1"], parentLine: serves("r2", "The outline draws one body") }),
    task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
    task("t2", { needs: ["t3"], parentLine: serves("r1", "The page draws the conversation") }),
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
  // A need whose chip the tree's names read has not answered yet (`answered: false`, no name):
  // the row names no need and draws no "needs" with nothing after it, and the curves still join
  // the bars, whatever the names.
  const unanswered = renderBody({
    ...whole,
    children: whole.children.map((one) => ({
      ...one,
      chips: one.chips.map((chip) => ({ ...chip, answered: false, name: "" })),
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
      { ...task("t1"), text: long("t1") },
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

// The chart's bars are reached in order with the keyboard, by the outline's keys.
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

// An address that names a step lands on it, in the view the filter opens on. The landing focuses
// the row in the frame after the commit that draws it (`IssueTree.tsx`'s `landOnRow`).
test("an address naming a step puts the focus on its bar's row", async () => {
  setViewport(1440);
  const named = task("t2", { needs: ["t1"] });
  const root = issue([task("t1"), named]);
  const view = renderBody(root, { reveal: [root, named] });
  try {
    await waitFor(() => expect(focusedRow()).toBe(D("t2")));
  } finally {
    view.unmount();
  }
});

test("the control switches to the ordered steps, which are the outline's own rows", async () => {
  setViewport(1440);
  // The address names the first task's step.
  const named = task("t1", { parentLine: serves("r1", "The page draws the conversation") });
  const root = issue([
    task("t3", { needs: ["t1"], parentLine: serves("r2", "The outline draws one body") }),
    named,
    task("t2", { needs: ["t3"], parentLine: serves("r1", "The page draws the conversation") }),
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

/** The spec and the Verification document, by the ids the issue read names them by. */
const SPEC_ARTIFACT = "artifact-spec";
const VERIFICATION_ARTIFACT = "artifact-verification";

/** One document of the issue, with whatever the case gives it: a role, or being the spec. */
function issueDocument(id: string, slug: string, extra: Partial<Artifact>): Artifact {
  return {
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: KEY,
    kind: "doc",
    name: slug,
    primary: false,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
    ...extra,
  };
}

/** The issue as its read hands it to the slot: its spec and its plan, and the Verification
 *  document where a case gives one. */
function issueRead(verification?: Artifact): IssueDetails {
  return {
    advances: [],
    advances_revision: 0,
    artifacts: [
      issueDocument(SPEC_ARTIFACT, SPEC, { primary: true }),
      issueDocument(PLAN_ARTIFACT, PLAN, { role: "plan" }),
      ...(verification === undefined ? [] : [verification]),
    ],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: "",
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: KEY,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: PLAN_ARTIFACT,
    primary_artifact_id: SPEC_ARTIFACT,
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: verification?.id ?? "",
  };
}

/** A plan task of the plan document as the tree read answers it, at `position` in it. */
function taskBlock(id: string, position: number): TreeAnswerNode {
  return {
    attributes: {},
    author: [],
    document: { artifact_id: PLAN_ARTIFACT, position, slug: PLAN },
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent: KEY,
    rank: null,
    state: "live",
    supertag: "plan_task",
    text: "",
    visibility: "project",
  };
}

/** A query client holding every read the issue's tree is built from (`useIssueTree.ts`), each
 *  answered and never stale, so none is sent: the plan's two tasks, and the spec's blocks, which
 *  a case may leave unheld for the tree read, which holds both, to go to `api.getIssueTree`. */
function slotClient({ specHeld = true }: { specHeld?: boolean } = {}): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
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
  client.setQueryData(["decisions", KEY], { decisions: [], document: null, open_questions: [] });
  for (const read of ["asks", "comments", "issue-messages"]) client.setQueryData([read, KEY], []);
  if (specHeld) {
    client.setQueryData(issueTreeQuery(KEY).queryKey, {
      converted: false,
      cursor: null,
      nodes: [taskBlock("t1", 0), taskBlock("t2", 1)],
      tree: null,
      tuples: [],
      view_of: null,
    });
  }
  return client;
}

/** A slot `renderSlot` drew, with `redraw`, which draws it again for another read of the issue. */
type DrawnSlot = RenderResult & { readonly redraw: (issue: IssueDetails) => void };

/** Draws the Plan view's slot (`slots/view-plan.tsx`) for `shown` over `client`, with the
 *  providers the page draws every slot under (`slots/providers.tsx`). */
function renderSlot(client: QueryClient, shown: IssueDetails): DrawnSlot {
  const route = { key: KEY, kind: "issue" } as const;
  const draw = (issue: IssueDetails): ReactNode => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              <IssueScope />
              <ViewPlanSlot issue={issue} route={route} />
            </ProvidersSlot>
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(draw(shown));
  return { ...view, redraw: (issue) => view.rerender(draw(issue)) };
}

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
      issueRead(issueDocument(VERIFICATION_ARTIFACT, "verification-md", { role: "verification" }))
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

// The spec's blocks read refused before it first answered: the view says, in the page's words,
// that it could not load the statements, with a Retry that asks the spec's blocks again.
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
    expect(line.querySelector("p")?.textContent).toBe(ANSWER_LINES.statements_unreadable);
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

/** The reads as the slot hands them on where `reads` failed before they first answered, in the
 *  order given, each Retry noting its read in `asked`. */
function failed(asked: PlanRead[], ...reads: PlanRead[]): PlanReads {
  return {
    failed: reads.map((read) => ({ read, retry: () => asked.push(read) })),
    state: "failed",
  };
}

// The view waits on the plan's and the spec's blocks reads (`slots/view-plan.tsx`): while one is
// out its tree holds nothing, and a read refused before it first answered is one line over the
// tree, in the page's words, with a Retry that asks that read again. Neither draws a row, the
// switch or the empty line, and the tree itself stays. With the focus on nothing, the first Retry
// takes it, and Enter on it, once the reads answer, leaves the focus on the chart's first row.
test("while a read is out the view draws nothing, and a read that failed says so with a Retry that takes the focus", async () => {
  setViewport(1440);
  const root = issue([task("t1"), task("t2", { needs: ["t1"] })]);
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
    expect(drawn()).toEqual(nothing);
    expect(
      lines().map((line) => [
        line.getAttribute("data-plan-unreadable"),
        line.querySelector("p")?.textContent,
        line.closest('[role="tree"]'),
      ])
    ).toEqual([
      ["plan", ANSWER_LINES.plan_unreadable, null],
      ["statements", ANSWER_LINES.statements_unreadable, null],
    ]);
    const retry = lines()[0]?.querySelector("button");
    if (retry == null) throw new Error("the plan's line draws no Retry");
    expect(document.activeElement, "the first line's Retry does not take the focus").toBe(retry);
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["plan"]);

    view.redraw(root, ANSWERED);
    expect(lines()).toHaveLength(0);
    expect(rowIds(view.container)).toEqual([D("t1"), D("t2")]);
    expect(view.container.querySelectorAll("[data-plan-switch]")).toHaveLength(1);
    expect(focusedRow(), "Enter on Retry does not leave the focus on the first row").toBe(D("t1"));
  } finally {
    view.unmount();
  }
});

// A read asked again that holds no answer is out again rather than failed (TanStack Query's
// `pending`), so the slot hands the body `waiting`, and the failed line leaves the page with the
// Retry that held the focus: after a press of that Retry, and after the page asked the read again
// with no press, on the window's focus or on an event. The focus stays in the view all the while,
// on its tree, and goes on to the Retry of a read refused again, or to the first row once the
// reads answer; with both reads refused, the second line's Retry gives it to the first's.
test("the focus stays in the view while a retried read is out, and goes on to a Retry or the first row", async () => {
  setViewport(1440);
  const root = issue([task("t1"), task("t2", { needs: ["t1"] })]);
  const waiting: PlanReads = { state: "waiting" };
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

  // Failed, then waiting once its Retry is pressed, then answered.
  await from(["plan"], "plan", async (view, asked) => {
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["plan"]);
    view.redraw(root, waiting);
    inTheView(view, "the read Retry asked again is out");
    view.redraw(root, ANSWERED);
    inTheView(view, "the read Retry asked again has answered");
    expect(
      focusedRow(),
      "the read Retry asked again answered leaves the focus off the first row"
    ).toBe(D("t1"));
  });
  // Failed, then waiting once its Retry is pressed, then refused again: the Retry drawn again
  // takes the focus back.
  await from(["plan"], "plan", async (view) => {
    await userEvent.keyboard("{Enter}");
    view.redraw(root, waiting);
    inTheView(view, "the read Retry asked again is out");
    view.redraw(root, failed([], "plan"));
    inTheView(view, "the read Retry asked again is refused again");
    expect(document.activeElement, "the read refused again leaves the focus off its Retry").toBe(
      retryOf(view, "plan")
    );
  });
  // Failed, then waiting once its Retry is pressed, then answered with no task: the view draws
  // the issue's empty line, and the focus stays on its tree, which is all the view holds.
  await from(["plan"], "plan", async (view) => {
    await userEvent.keyboard("{Enter}");
    view.redraw(root, waiting);
    inTheView(view, "the read Retry asked again is out");
    view.redraw(issue([]), ANSWERED);
    inTheView(view, "the read Retry asked again has answered with no task");
    expect(
      document.activeElement,
      "the read Retry asked again answered with no task leaves the focus off the view's tree"
    ).toBe(view.container.querySelector(`[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`));
    expect(view.container.textContent).toContain(EMPTY_STATE_LINES.plan);
  });
  // Both refused, and the statements line's Retry pressed: that read is out and its line goes,
  // and the plan's line stays.
  await from(["plan", "statements"], "statements", async (view, asked) => {
    await userEvent.keyboard("{Enter}");
    expect(asked).toEqual(["statements"]);
    view.redraw(root, failed([], "plan"));
    inTheView(view, "the statements read Retry asked again is out");
    expect(
      document.activeElement,
      "the statements line's Retry pressed leaves the focus off the plan's Retry"
    ).toBe(retryOf(view, "plan"));
  });
  // Asked again with no press, with the focus on its Retry - on the window's focus, or on an
  // event - and answered.
  await from(["plan"], "plan", async (view, asked) => {
    view.redraw(root, waiting);
    inTheView(view, "the read the page asked again is out");
    view.redraw(root, ANSWERED);
    inTheView(view, "the read the page asked again has answered");
    expect(asked).toEqual([]);
    expect(
      focusedRow(),
      "the read the page asked again answered leaves the focus off the first row"
    ).toBe(D("t1"));
  });
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
// `reachPart`), which holds nothing while the reads have not answered and hands the focus on to
// the first Retry, from wherever the reader stood: here a control outside the view.
test("the view's tree focused with a read refused hands the focus to its Retry", () => {
  setViewport(1440);
  const control = document.createElement("button");
  document.body.append(control);
  const view = renderBody(issue([task("t1")]), { reads: failed([], "plan") });
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

// The focus leaving the chart lets go of its row by the outline's own rule (`IssueTree.tsx`'s
// `useReleaseFocus`): with no press under way, the row's mark and its heavier curves go at once,
// and Details goes back to the issue's facts with them; a press that takes the focus elsewhere
// keeps the row while it is under way, and lets go of it once it has ended, in the task after
// its release, where its click has landed.
test("a row lets go of its mark when the focus leaves the chart for another control, at once or once the press has ended", async () => {
  setViewport(1440);
  const view = renderBody(
    issue([task("t3", { needs: ["t1"] }), task("t1"), task("t2", { needs: ["t3"] })])
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

// A cycle is one line that lists the tasks in it. It stands where the order puts it, after the
// step its tasks need and before the step that waits on one of them, and an address naming one
// of its tasks lands on a row.
test("tasks that need each other are one line at their place, and rows at that place as steps", async () => {
  setViewport(1440);
  const user = userEvent.setup();
  const second = task("t5", { needs: ["t4"] });
  const root = issue([
    task("t6", { needs: ["t5"] }),
    task("t4", { needs: ["t5", "t1"] }),
    second,
    task("t1", { parentLine: serves("r1", "The page draws the conversation") }),
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

// The filter's view hands the tasks over grouped by the statement each one serves, which is the
// spec's order. Where the `needs` leave two tasks free to stand in either order, the order the
// plan is written in decides, and each task is drawn once.
test("tasks free to stand in either order keep the order the plan is written in", () => {
  setViewport(1440);
  // The view hands the third task over first and the first twice, by the statements they serve;
  // the plan writes the three in their numbers' order, and the third needs the first.
  const view = renderBody(
    issue([task("t3", { needs: ["t1"] }), task("t1"), task("t2"), task("t1")]),
    { written: ["t1", "t2", "t3"] }
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

// The written order reads the plan's blocks under the key `useIssueTree` fills, and sends no
// read of its own: on a client that counts every read stale at once, an enabled observer would
// fetch them again on mount. The order follows those blocks when the tree's read changes them.
test("the written order reads the cached blocks and sends no blocks read of its own", async () => {
  setViewport(1440);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } });
  const read = spyOn(api, "getArtifactBlocks").mockResolvedValue([]);
  const root = issue([task("t3", { needs: ["t1"] }), task("t1"), task("t2")]);
  // The plan's blocks as the tree read holds them, which the slot hands the body again when the
  // tree's read changes them.
  const written = ["t1", "t2", "t3"];
  const view = renderBody(root, { client, written });
  try {
    expect(read, "the body sends a read of the plan's blocks of its own").not.toHaveBeenCalled();
    expect(rowIds(view.container)).toEqual([D("t1"), D("t2"), D("t3")]);
    await act(async () => {
      written.splice(0, written.length, "t2", "t1", "t3");
      view.redraw(root, ANSWERED);
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 0);
      await promise;
    });
    expect(rowIds(view.container)).toEqual([D("t2"), D("t1"), D("t3")]);
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
  const blank = (blockId: string, needs: readonly string[] = []): TreeNode => ({
    ...task(blockId, { needs }),
    text: "",
  });
  const view = renderBody(
    issue([
      blank("t1"),
      task("t2", { needs: ["t1"] }),
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

// The view names no need from its own node. The tree hands a `needs` chip over with no name while
// the names read is out, and where the server's name of the target ends inside its directive's
// opening line, as for a task whose `needs` holds eleven pointers on one line. While a chip has
// no name the row draws no need for it, and "· needs" only when it draws a need: an empty need
// after "· needs" names no task at all.
test("a need the tree has not named draws no empty need and no '· needs' with nothing after it", () => {
  setViewport(1440);
  // The third task needs the first two, the fourth the first alone; the tree names the second
  // and not the first.
  const root = issue([
    task("t1"),
    task("t2"),
    task("t3", { needs: ["t1", "t2"] }),
    task("t4", { needs: ["t1"] }),
  ]);
  const unnamed: TreeNode = {
    ...root,
    children: root.children.map((one) => ({
      ...one,
      chips: one.chips.map((chip) => (chip.ref.endsWith("#b-t1") ? { ...chip, name: "" } : chip)),
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
