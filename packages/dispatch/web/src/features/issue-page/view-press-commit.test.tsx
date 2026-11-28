import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor, within } from "@testing-library/react";
import { Profiler } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { DESKTOP_WIDTH, PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "./page-words";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { AppProviders, commentNodes, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { coverageRead } from "./spec-filter/spec-fixtures";
import { treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * A press on the view row draws the view it turns on in one commit (`view-press.tsx`): in the
 * press's own commit where the view's read holds its answer, or is out since the load sent it, and
 * where the press sent it, in the commit its answer brings, the press's own commit having the
 * control read as current and the view's place read busy over the view the press left. No commit
 * draws a part of the view, and none holds the
 * view's rows with the busy mark. Each commit of the page is read as React makes it (`Profiler`'s
 * `onRender`), so a case sees each commit apart, though `act` runs them before it returns.
 */

/** The mark of a view's place while it loads, as the load rows read it. */
const VIEW_LOADS = '[data-issue-page] [aria-busy="true"]';

/** A statement of the Spec view, and a row of the outline. */
const SPEC_ROW = "[data-spec-statement]";
const OUTLINE_ROW = "[data-issue-body] [data-node-row]";

/** The Spec view's read, and Conversation's. */
const SPEC_VIEW: TreeAnswer = treeOf(
  ["r1", "r2"].map((id, position) => ({
    ...treeNode(id, "requirement", KEY, {
      ...inDocument(SPEC_DOCUMENT, position),
      text: `**${id.toUpperCase()}.** The page draws ${id}.`,
    }),
    role: "query" as const,
  }))
);
const OUTLINE: TreeAnswer = treeOf(commentNodes(TWO_THREADS));

/** What the page held at one commit: the control that reads as current, whether the view's
 *  place reads busy, and how many rows of Spec and of the outline it drew. */
interface Commit {
  readonly busy: boolean;
  readonly current: string | undefined;
  readonly outline: number;
  readonly spec: number;
}

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
/** Every commit of the page since the case began. */
let commits: Commit[] = [];
/** The answers of the Spec view's reads sent and not answered yet, in order. */
let specReads: ((answer: TreeAnswer) => void)[] = [];
/** Whether Plan's read, which every load of Conversation sends, never answers. */
let planOut = false;

/** The control of the view row that reads as current now, by its name. */
function currentControl(): string | undefined {
  const pressed = document.querySelector(
    '[data-testid="issue-presets"] button[aria-pressed="true"] [data-preset-name]'
  );
  const named =
    pressed ?? document.querySelector('[data-testid="issue-presets"] [data-preset-name]');
  return named?.textContent ?? undefined;
}

beforeEach(() => {
  commits = [];
  specReads = [];
  planOut = false;
  tasks = tasksByHand();
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueCoverage").mockResolvedValue(coverageRead(["r1", "r2"])),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    // The Spec view's read is held until the case answers it, and Plan's, which every load of
    // Conversation sends, never answers where `planOut`; every other tree read answers.
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) => {
      if (query.view === "plan" && planOut) return new Promise<TreeAnswer>(() => {});
      if (query.view !== "spec") return Promise.resolve(OUTLINE);
      const read = Promise.withResolvers<TreeAnswer>();
      specReads.push(read.resolve);
      return read.promise;
    }),
  ];
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The page's root at Conversation's address at `width`, each of its commits noted. */
function drawPage(width: number): RenderResult {
  restoreViewport = stubViewport(width);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <Profiler
          id="page"
          onRender={() => {
            commits.push({
              busy: document.querySelector(VIEW_LOADS) !== null,
              current: currentControl(),
              outline: document.querySelectorAll(OUTLINE_ROW).length,
              spec: document.querySelectorAll(SPEC_ROW).length,
            });
          }}
        >
          <IssueOutlinePage />
        </Profiler>
      </AppProviders>
    </MemoryRouter>
  );
}

/** Waits until `selector` is drawn in `view`, running the page's tasks - the tree's build, the
 *  slices - as the page asks for them. */
function drawn(view: RenderResult, selector: string): Promise<void> {
  return waitFor(async () => {
    await tasks.runAll();
    expect(view.container.querySelector(selector), `nothing drew ${selector}`).not.toBeNull();
  });
}

/** Answers the Spec view's read the press sent, and runs the tasks its answer asks for. */
async function answerSpec(): Promise<void> {
  const answer = specReads.shift();
  if (answer === undefined) throw new Error("no Spec read is out");
  await act(async () => answer(SPEC_VIEW));
  await tasks.runAll();
}

/** The press on the view row at 1440 px. */
function pressAt1440(view: RenderResult): (preset: Preset) => Promise<void> {
  return async (preset) => {
    const toolbar = await view.findByRole("toolbar", { name: CONTROL_LABELS.presets });
    const button = within(toolbar).getByRole("button", { name: PRESET_NAMES[preset] });
    act(() => button.click());
  };
}

/** The press on a view in the one control's menu at 390 px. */
function pressAt390(view: RenderResult): (preset: Preset) => Promise<void> {
  return async (preset) => {
    const presets = await view.findByTestId("issue-presets");
    const chooser = presets.querySelector<HTMLElement>('button[aria-haspopup="menu"]');
    if (chooser === null) throw new Error("the row drew no one control");
    act(() => chooser.click());
    const item = [...presets.querySelectorAll<HTMLElement>('[role="menuitemradio"]')].find(
      (each) => each.querySelector("span[id]")?.textContent === PRESET_NAMES[preset]
    );
    if (item === undefined) throw new Error(`the menu holds no ${preset}`);
    act(() => item.click());
  };
}

/** The press on Spec with its read out, from `press` on: the press's commit has Spec current,
 *  the place busy and Conversation's rows still drawn, and no commit draws Spec until the read
 *  answers; then one commit draws Spec with the place no longer busy, and no commit held Spec's
 *  rows and the busy mark together. */
async function pressedThenAnswered(
  view: RenderResult,
  press: (preset: Preset) => Promise<void>
): Promise<void> {
  await drawn(view, OUTLINE_ROW);
  const from = commits.length;
  await press("spec");
  const pressed = commits.slice(from).findIndex((commit) => commit.current === "Spec");
  expect(pressed, "no commit of the press had Spec current").toBeGreaterThanOrEqual(0);
  const before = commits.slice(from + pressed);
  expect(before[0]?.busy, "the press's commit has the view's place not busy").toBe(true);
  expect(before[0]?.outline, "the press's commit took the outline off").toBeGreaterThan(0);
  expect(
    before.filter((commit) => commit.spec > 0),
    "a commit drew Spec before its read answered"
  ).toEqual([]);
  const answered = commits.length;
  await answerSpec();
  const after = commits.slice(answered);
  const drew = after.findIndex((commit) => commit.spec > 0);
  expect(drew, "no commit after the answer drew Spec").toBeGreaterThanOrEqual(0);
  expect(after[drew]?.busy, "the commit that drew Spec left the place busy").toBe(false);
  expect(after[drew]?.outline, "the commit that drew Spec left the outline drawn").toBe(0);
  expect(
    commits.slice(from).filter((commit) => commit.spec > 0 && commit.busy),
    "a commit held Spec's rows and the busy mark together"
  ).toEqual([]);
}

test("at 1440 px a press on a view whose read is out commits the control and the busy place, and one commit after the answer draws the view", async () => {
  const view = drawPage(DESKTOP_WIDTH);
  await pressedThenAnswered(view, pressAt1440(view));
});

test("at 390 px a view pressed in the one control's menu with its read out commits the control and the busy place, and one commit after the answer draws the view", async () => {
  const view = drawPage(PHONE_WIDTH);
  await pressedThenAnswered(view, pressAt390(view));
});

test("a press on a view whose read is held draws the view in the press's own commit, with no commit busy", async () => {
  const view = drawPage(DESKTOP_WIDTH);
  const press = pressAt1440(view);
  await drawn(view, OUTLINE_ROW);
  await press("spec");
  await answerSpec();
  await drawn(view, SPEC_ROW);
  await press("conversation");
  await drawn(view, OUTLINE_ROW);
  const from = commits.length;
  await press("spec");
  const pressed = commits.slice(from).findIndex((commit) => commit.current === "Spec");
  expect(pressed, "no commit of the press had Spec current").toBeGreaterThanOrEqual(0);
  expect(commits[from + pressed]?.spec, "the press's commit drew no row of Spec").toBeGreaterThan(
    0
  );
  expect(
    commits.slice(from).filter((commit) => commit.busy),
    "a commit of a press on a view whose read is held read busy"
  ).toEqual([]);
});

test("a press on a view whose read the load sent and is still out commits the control and busy place first, then moves after the loading frame", async () => {
  planOut = true;
  const view = drawPage(DESKTOP_WIDTH);
  await drawn(view, OUTLINE_ROW);
  const from = commits.length;
  await pressAt1440(view)("plan");
  const pressed = commits.slice(from).findIndex((commit) => commit.current === "Plan");
  expect(pressed, "no commit of the press had Plan current").toBeGreaterThanOrEqual(0);
  expect(commits[from + pressed]?.busy, "the press's commit has the view's place not busy").toBe(
    true
  );
  expect(
    commits[from + pressed]?.outline,
    "the press's commit took the outline off before the loading frame painted"
  ).toBeGreaterThan(0);
  await waitFor(async () => {
    await tasks.runAll();
    expect(
      document.querySelectorAll(OUTLINE_ROW).length,
      "the outline was not taken off after the loading frame while Plan's read stayed out"
    ).toBe(0);
  });
  expect(
    document.querySelector("[data-plan-body]"),
    "Plan did not mount after the loading frame"
  ).not.toBeNull();
});
