import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS, PRESET_NAMES } from "./page-words";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { AppProviders, commentNodes, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * A press whose view's read is out (`view-press.tsx`) takes the view it left off the page in a
 * task of its own, its place still busy, and draws the view pressed in the next task: the old
 * view's take-down and the new view's first drawing are never one task, and no view is kept
 * hidden in the document between them.
 */

/** The mark of a view's place while its read is out, as the load rows read it. */
const VIEW_LOADS = '[data-issue-page] [aria-busy="true"]';

/** A statement of the Spec view, drawn from its view read. */
const SPEC_ROW = "[data-spec-statement]";

/** A row of the outline. */
const OUTLINE_ROW = "[data-issue-body] [data-node-row]";

/** The two statements of the spec, as the Spec view's read answers them. */
const SPEC_VIEW: TreeAnswer = treeOf(
  ["r1", "r2"].map((id, position) => ({
    ...treeNode(id, "requirement", KEY, {
      ...inDocument(SPEC_DOCUMENT, position),
      text: `**${id.toUpperCase()}.** The page draws ${id}.`,
    }),
    role: "query" as const,
  }))
);

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
/** The Spec view's read, held until the case answers it. */
let answerSpec: (answer: TreeAnswer) => void = () => {};

beforeEach(() => {
  tasks = tasksByHand();
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) => {
      if (query.view === undefined) return Promise.resolve(treeOf(commentNodes(TWO_THREADS)));
      if (query.view !== "spec") return Promise.resolve(treeOf([]));
      const held = Promise.withResolvers<TreeAnswer>();
      answerSpec = held.resolve;
      return held.promise;
    }),
  ];
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

function drawPage(): RenderResult {
  restoreViewport = stubViewport(DESKTOP_WIDTH);
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

test("a press whose read answers takes the view it left off in one task and draws the view pressed in the next", async () => {
  const view = drawPage();
  await waitFor(async () => {
    await tasks.runAll();
    expect(view.container.querySelector(OUTLINE_ROW), "Conversation drew no row").not.toBeNull();
  });
  const toolbar = await view.findByRole("toolbar", { name: CONTROL_LABELS.presets });
  const spec = within(toolbar).getByRole("button", { name: PRESET_NAMES.spec });
  act(() => spec.click());
  expect(view.container.querySelector(VIEW_LOADS), "the press drew no loading mark").not.toBeNull();
  expect(
    view.container.querySelector(OUTLINE_ROW),
    "the press took Conversation down"
  ).not.toBeNull();

  // The read answers before the press's frame: the task of the answer takes Conversation off.
  await act(async () => answerSpec(SPEC_VIEW));
  expect(
    view.container.querySelector(OUTLINE_ROW),
    "the answer's task left the view the press left on the page"
  ).toBeNull();
  expect(
    view.container.querySelector(SPEC_ROW),
    "the task that took Conversation down drew Spec's rows too"
  ).toBeNull();
  expect(
    view.container.querySelector(VIEW_LOADS),
    "the view's place stopped showing that it loads before Spec was drawn"
  ).not.toBeNull();

  // The next task draws Spec, and takes the loading mark off.
  expect(await tasks.runNext(), "no task was asked for to draw the view pressed").toBe(true);
  await tasks.runAll();
  expect(
    view.container.querySelectorAll(SPEC_ROW).length,
    "no task after the take-down drew Spec"
  ).toBe(2);
  expect(view.container.querySelector(VIEW_LOADS), "the loading mark stayed").toBeNull();
});
