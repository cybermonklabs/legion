import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Comment } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import * as issueTreeModule from "./issue-tree";
import { TWO_THREADS } from "./outline-fixtures";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, commentNodes, inDocument, stubReads, treeOf } from "./slots/stage-fixtures";
import { treeNode } from "./tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswer } from "./tree-read/tree-read";
import { type IssueTreeView, useIssueTree } from "./useIssueTree";

/**
 * The issue's tree is built outside React's render (`useIssueTree.ts`), in a task of its own
 * (`scheduler.postTask` where the browser has it, else `setTimeout(0)`), and handed to React in a
 * later task, so the render that draws the view's first screen builds no tree: the CPU profile of
 * the longest task (`longest-2.log`) put `builtTree` → `buildIssueTree` at 30 to 116 ms inside
 * the same synchronous React render and commit that drew the first screen.
 *
 * Each case holds `scheduler.postTask` by hand (`tasksByHand`) and counts `buildIssueTree` calls
 * against the tree read's answers (`treeAnswerCalls`).
 */

/** One deferred answer of type `T`. */
interface Held<T> {
  readonly promise: Promise<T>;
  readonly resolve: (value: T) => void;
}

function hold<T>(): Held<T> {
  return Promise.withResolvers<T>();
}

/** Conversation's tree read: the issue's two threads and two statements of the spec. */
const TREE_ANSWER: TreeAnswer = treeOf([
  ...commentNodes(TWO_THREADS),
  ...["r1", "r2"].map((id, position) =>
    treeNode(id, "requirement", KEY, inDocument(SPEC_DOCUMENT, position))
  ),
]);

/** A row of the outline drawn in the stage. */
const OUTLINE_ROW = "[data-issue-body] [data-node-row]";

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
let treeRead: Held<TreeAnswer>;
let commentsRead: Held<Comment[]>;
let builds: ReturnType<typeof spyOn<typeof issueTreeModule, "buildIssueTree">>;

/** How many `buildIssueTree` calls built from an answered tree read (ignoring the empty issue
 *  placeholder built before the tree read answered). */
function treeBuilds(): number {
  return builds.mock.calls.filter(([reads]) => reads.tree !== undefined).length;
}

beforeEach(() => {
  treeRead = hold<TreeAnswer>();
  commentsRead = hold<Comment[]>();
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "listComments").mockImplementation(() => commentsRead.promise),
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) =>
      query.view === undefined ? treeRead.promise : Promise.resolve(treeOf([]))
    ),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
  ];
  builds = spyOn(issueTreeModule, "buildIssueTree");
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  tasks = tasksByHand();
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  builds.mockRestore();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

/** The page's root at Conversation's address, under what the app provides above it. */
function drawPage(preset: "conversation" | "spec" = "conversation"): RenderResult {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, preset)]}>
      <AppProviders client={client}>
        <IssueOutlinePage />
      </AppProviders>
    </MemoryRouter>
  );
}

// When the tree read answers, the render that receives the answer builds no tree and draws no
// row; a task of its own builds the tree, and a separate task commits the first screen of rows
// with zero `buildIssueTree` calls inside that commit.
test("the tree is built in a task of its own, and the first drawing's commit builds none", async () => {
  const view = drawPage();
  await act(async () => commentsRead.resolve([...TWO_THREADS]));
  await waitFor(() => expect(api.getIssueTree).toHaveBeenCalled());
  expect(treeBuilds(), "a tree was built before the tree read answered").toBe(0);

  await act(async () => treeRead.resolve(TREE_ANSWER));
  expect(
    treeBuilds(),
    "buildIssueTree ran synchronously inside the render that received the tree read's answer"
  ).toBe(0);
  expect(
    view.container.querySelector(OUTLINE_ROW),
    "the outline drew rows before its tree was built"
  ).toBeNull();
  expect(tasks.pending(), "no task was scheduled to build the tree").toBeGreaterThan(0);

  // Task 1: builds the tree outside React's render, without drawing the first screen in the same
  // task.
  expect(await tasks.runNext(), "the build task did not run").toBe(true);
  expect(treeBuilds(), "the build task did not build the tree once").toBe(1);
  expect(
    view.container.querySelector(OUTLINE_ROW),
    "the first screen was drawn inside the same task that built the tree"
  ).toBeNull();

  // Task 2: commits the first screen from the already-built tree, calling `buildIssueTree` zero
  // times in the commit.
  const buildsBeforeCommit = treeBuilds();
  expect(await tasks.runNext(), "the publish task did not run").toBe(true);
  expect(
    view.container.querySelectorAll(OUTLINE_ROW).length,
    "the first screen of rows did not draw once the built tree was published"
  ).toBeGreaterThan(0);
  expect(
    treeBuilds() - buildsBeforeCommit,
    "the commit that drew the first screen called buildIssueTree"
  ).toBe(0);
});

// Two reads answering before the build task runs coalesce into one tree build for the combined
// reads, rather than building an intermediate tree and then building again.
test("two reads answering before the build task runs build the tree once for the latest reads", async () => {
  const view = drawPage();
  await waitFor(() => expect(api.getIssueTree).toHaveBeenCalled());

  await act(async () => treeRead.resolve(TREE_ANSWER));
  await act(async () => commentsRead.resolve([...TWO_THREADS]));
  expect(treeBuilds(), "buildIssueTree ran before the build task").toBe(0);

  // Build task runs once for the combined reads, then publish task draws the rows with 0 builds.
  await tasks.runNext();
  expect(treeBuilds(), "two reads before the build task built more than one tree").toBe(1);
  const beforeCommit = treeBuilds();
  await tasks.runNext();
  expect(view.container.querySelectorAll(OUTLINE_ROW).length).toBeGreaterThan(0);
  expect(treeBuilds() - beforeCommit).toBe(0);
});

// A slot drawn with no tree owner above it keeps `useIssueTree`'s contract: it builds its tree in
// its own task and draws its rows once built.
test("a stage drawn with no tree owner above it builds its tree in its own task and draws its rows", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const issue = issueDetails();
  const route = { key: KEY, kind: "issue" } as const;
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <ProvidersSlot issue={issue} route={route}>
          <StageSlot issue={issue} route={route} />
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  await act(async () => {
    commentsRead.resolve([...TWO_THREADS]);
    treeRead.resolve(TREE_ANSWER);
  });
  expect(treeBuilds(), "standalone useIssueTree built synchronously in render").toBe(0);
  await tasks.runAll();
  expect(treeBuilds()).toBeGreaterThanOrEqual(1);
  expect(view.container.querySelectorAll(OUTLINE_ROW).length).toBeGreaterThan(0);
});

test("a cold filter builds its own tree in a task before the commit that draws its rows", async () => {
  const spec = hold<TreeAnswer>();
  spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) =>
    query.view === "spec" ? spec.promise : Promise.resolve(treeOf([]))
  );
  restoreViewport();
  restoreViewport = stubViewport(390);
  const view = drawPage("spec");
  await act(async () => commentsRead.resolve([]));
  await waitFor(() =>
    expect(api.getIssueTree).toHaveBeenCalledWith(
      KEY,
      expect.objectContaining({ view: "spec" }),
      expect.anything()
    )
  );
  await act(async () =>
    spec.resolve({
      ...treeOf([
        {
          ...treeNode("r1", "requirement", KEY, {
            ...inDocument(SPEC_DOCUMENT),
            text: "The first statement.",
          }),
          role: "query",
        },
      ]),
      view: "spec",
    })
  );
  expect(treeBuilds(), "the Spec answer built a tree before a task ran").toBe(0);
  let beforeDrawing = 0;
  const row = "[data-spec-statement]";
  for (let step = 0; step < 15 && view.container.querySelector(row) === null; step += 1) {
    beforeDrawing = treeBuilds();
    expect(await tasks.runNext()).toBe(true);
  }
  expect(view.container.querySelector(row), "the Spec tree drew no statement").not.toBeNull();
  expect(beforeDrawing, "Spec drew a row before its tree was built").toBeGreaterThan(0);
  expect(treeBuilds(), "the first drawing built a tree").toBe(beforeDrawing);
});

// When a read's hook value differs from `client.getQueryData` (for example a disabled or diverged
// cache entry), `useBuiltTree` builds the rendered reads as well and stops scheduling once cached,
// rather than re-publishing in an infinite loop.
test("a diverged cache entry terminates in bounded renders instead of looping", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const divergedComments: Comment[] = [...TWO_THREADS];
  const originalGetQueryData = client.getQueryData.bind(client);
  const getQueryDataSpy = spyOn(client, "getQueryData").mockImplementation((queryKey) =>
    Array.isArray(queryKey) && queryKey[0] === "comments"
      ? (divergedComments as never)
      : originalGetQueryData(queryKey)
  );
  stubs.push(getQueryDataSpy);

  const issue = issueDetails();
  let renders = 0;
  let latest: IssueTreeView | undefined;
  function Probe() {
    renders += 1;
    latest = useIssueTree(issue);
    return null;
  }

  render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <Probe />
      </AppProviders>
    </MemoryRouter>
  );

  await act(async () => {
    commentsRead.resolve([]);
    treeRead.resolve(TREE_ANSWER);
  });

  let steps = 0;
  while (steps < 20 && (await tasks.runNext())) steps += 1;
  expect(steps, "scheduled tasks did not drain — useBuiltTree looped").toBeLessThan(20);
  expect(renders, "useIssueTree rendered without bound").toBeLessThan(20);
  expect(treeBuilds(), "buildIssueTree ran without bound").toBeLessThanOrEqual(4);
  expect(latest?.pending).toBe(false);
  expect(latest?.root.children.length).toBeGreaterThan(0);
});

// A standalone slot mounted with a cached tree answer (`warmMount = true`) rebuilds in its layout
// effect when `setQueryData` updates the cache, so rows dropped inside `act` leave the DOM before
// `act` returns (preventing deferred stale DOM nodes from reaching post-`act` matchers).
test("a warm standalone slot updates inside act when setQueryData replaces the tree", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["comments", KEY], [...TWO_THREADS]);
  client.setQueryData(outlineQuery(KEY, "conversation").queryKey, TREE_ANSWER);

  const issue = issueDetails();
  let latest: IssueTreeView | undefined;
  function Probe() {
    latest = useIssueTree(issue, true);
    return null;
  }

  render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <Probe />
      </AppProviders>
    </MemoryRouter>
  );
  expect(latest?.pending).toBe(false);
  const beforeCount = latest?.root.children.length ?? 0;
  expect(beforeCount).toBeGreaterThan(1);

  const fewer = TWO_THREADS.slice(0, 1);
  await act(async () => {
    client.setQueryData(["comments", KEY], fewer);
    client.setQueryData(outlineQuery(KEY, "conversation").queryKey, treeOf(commentNodes(fewer)));
  });
  expect(latest?.pending).toBe(false);
  expect(latest?.root.children.length).toBe(1);
});
