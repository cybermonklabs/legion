import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { artifact, issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { EMPTY_STATE_LINES, FILTER_SIGNAL_WORDS } from "./page-words";
import * as presetsModule from "./presets";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { AppProviders, commentNodes, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { coverageRead } from "./spec-filter/spec-fixtures";
import { treeNode, treeTuple } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The view row's signals (`slots/view-row.tsx`) and the "In short" rows' counts
 * (`IssueThreeThings.tsx`, `contents-frame.tsx`, `slots/contents.tsx`, `slots/view-spec.tsx`) are
 * computed in a task of their own after the first drawing (R1: `longest-2.log` put
 * `ViewRowSlot` → `filterSignals` → `heldFilterView` at 20 to 80 ms, plus up to 120 ms of
 * `root.views.trees.plan` built inside it, in the same synchronous commit that drew the first
 * screen). Each control already holds its signal's room from the first drawing
 * (`IssuePresets.tsx`'s `Signal`) and each "In short" row holds its counts' line
 * (`IssueThreeThings.tsx`), so computing them in a later task moves nothing and leaves every
 * count drawn once the load has ended.
 */

const PLAN_DOCUMENT = artifact("artifact-plan", "plan-md");

function plannedIssue(): IssueDetails {
  const issue = issueDetails();
  return {
    ...issue,
    artifacts: [...(issue.artifacts ?? []), PLAN_DOCUMENT],
    plan_artifact_id: PLAN_DOCUMENT.id,
    queries: [
      {
        artifact_id: PLAN_DOCUMENT.id,
        query: "plan",
        subtree: `issue:${KEY}`,
        takes: ["plan_task", "plan_role"],
      },
    ],
  };
}

/** Conversation's tree read: the two threads and two statements of the spec. */
const OUTLINE: TreeAnswer = treeOf([
  ...commentNodes(TWO_THREADS),
  ...["r1", "r2"].map((id, position) =>
    treeNode(id, "requirement", KEY, inDocument(SPEC_DOCUMENT, position))
  ),
]);

/** Plan's view read: one plan task serving r1. */
const PLAN_VIEW: TreeAnswer = treeOf(
  [
    {
      ...treeNode("t1", "plan_task", "r1", {
        ...inDocument(PLAN_DOCUMENT, 0),
        text: "Build the tree outside render.",
      }),
      role: "query" as const,
    },
  ],
  [treeTuple("serves", "t1", "r1")]
);

/** A row of the outline drawn in the stage. */
const OUTLINE_ROW = "[data-issue-body] [data-node-row]";

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
let signalsCalls: ReturnType<typeof spyOn<typeof presetsModule, "filterSignals">>;

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(plannedIssue()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead(["r1", "r2"])),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === "plan" ? PLAN_VIEW : query.view === undefined ? OUTLINE : treeOf([])
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
  signalsCalls = spyOn(presetsModule, "filterSignals");
  tasks = tasksByHand();
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  signalsCalls.mockRestore();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

function drawPage(width: number): RenderResult {
  restoreViewport = stubViewport(width);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <IssueOutlinePage />
      </AppProviders>
    </MemoryRouter>
  );
}

/** Runs queued tasks one at a time until the outline's first screen of rows is drawn, and stops
 *  right after that commit so the case can read the page before later tasks run. */
async function drawFirstScreen(view: RenderResult): Promise<void> {
  await waitFor(() =>
    expect(tasks.pending(), "no task was queued to build the tree").toBeGreaterThan(0)
  );
  for (let step = 0; step < 10 && view.container.querySelector(OUTLINE_ROW) === null; step += 1) {
    expect(await tasks.runNext(), "the queue emptied before the first screen drew").toBe(true);
  }
  expect(view.container.querySelector(OUTLINE_ROW), "the first screen drew no row").not.toBeNull();
}

// At 1440 px, the view row's signals are computed in a task of their own after the first drawing:
// `filterSignals` has not run when the first screen of rows is drawn, the controls hold their
// signal rooms empty, and once the queued tasks run every count is drawn.
test("at 1440 px the view row's signals are computed after the first drawing and drawn once the load ends", async () => {
  const view = drawPage(DESKTOP_WIDTH);
  await drawFirstScreen(view);

  expect(
    signalsCalls,
    "filterSignals ran during or before the commit that drew the first screen"
  ).not.toHaveBeenCalled();
  expect(
    view.container.querySelectorAll("[data-preset-signal]").length,
    "the view row held no signal rooms at the first drawing"
  ).toBe(4);
  expect(
    view.container.querySelector("[data-preset-count]"),
    "a view control drew a count during the first drawing"
  ).toBeNull();

  await tasks.runAll();
  expect(signalsCalls, "filterSignals never ran after the first drawing").toHaveBeenCalled();
  expect(
    view.container.querySelector('[data-preset-count="spec"]')?.textContent,
    "Spec's count was not drawn once the signals task ran"
  ).toBe("2");
  expect(
    view.container.querySelector('[data-preset-count="plan"]')?.textContent,
    "Plan's count was not drawn once the signals task ran"
  ).toBe("1");
});

// At 390 px, `ThreeThingsAbove` stands above the rows beside the view row: neither computes
// `filterSignals` during the first drawing, the "In short" rows hold their counts' lines empty,
// and once the queued tasks run both draw their counts.
test("at 390 px neither the view row nor In short computes signals during the first drawing, and both draw them after", async () => {
  const view = drawPage(PHONE_WIDTH);
  await drawFirstScreen(view);

  expect(
    signalsCalls,
    "filterSignals ran during or before the commit that drew the first screen at 390 px"
  ).not.toHaveBeenCalled();
  const lines = [...view.container.querySelectorAll<HTMLElement>("[data-brief-counts]")];
  expect(lines.length, "In short did not hold its two counts' lines").toBe(2);
  expect(
    lines.map((line) => line.textContent),
    "In short drew counts during the first drawing"
  ).toEqual(["", ""]);

  await tasks.runAll();
  expect(signalsCalls, "filterSignals never ran after the first drawing").toHaveBeenCalled();
  expect(
    lines.map((line) => line.textContent),
    "In short did not draw its counts once its task ran"
  ).toEqual([`1 ${FILTER_SIGNAL_WORDS.steps}`, EMPTY_STATE_LINES.verification]);
  // Open the phone's menu to verify the view row's counts are drawn there too.
  const chooser = view.container.querySelector<HTMLElement>(
    '[data-testid="issue-presets"] button[aria-haspopup="menu"]'
  );
  if (chooser === null) throw new Error("the phone row drew no one control");
  act(() => chooser.click());
  expect(
    view.container.querySelector('[data-preset-count="spec"]')?.textContent,
    "Spec's count was not drawn in the phone menu"
  ).toBe("2");
});
