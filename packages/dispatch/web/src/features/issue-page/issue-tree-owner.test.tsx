import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, render, waitFor, within } from "@testing-library/react";
import { type ReactNode, useRef } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { parseIssuePath } from "../refs/routes";
import { IssueContentsPane } from "./contents-frame";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { IssueTreeOwnerProvider, useOwnedIssueTree } from "./issue-tree-owner";
import { TWO_THREADS } from "./outline-fixtures";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "./page-words";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";
import { ViewRowSlot } from "./slots/view-row";
import * as issueTreeHook from "./useIssueTree";

/**
 * The tree's owner (`issue-tree-owner.tsx`): the page's root mounts it around its slots
 * (`IssueOutlinePage.tsx`), it runs `useIssueTree` once for the page, and every reader of the
 * tree under it reads the owner's answer. So a load of Conversation runs `useIssueTree` in one
 * place rather than in each of its three readers - the view row, the Contents pane and the body -
 * and a press on a view mounts no reader that runs it again. A slot drawn with no owner above it,
 * as a case that mounts one slot draws it, runs its own `useIssueTree`, as every reader did
 * before the owner.
 */

// The hook as its module defines it, read before a case's spy stands in for the module's export,
// which every import of it - the owner's and each reader's - reads from then on.
const useIssueTreeItself = issueTreeHook.useIssueTree;

/**
 * Counts `useIssueTree`'s runs from now until `restore`: `runs` holds, for each run, the number
 * of the mounted caller that ran it, each caller numbered from 1 in the order of its first run
 * by a ref of its own, so the same caller has the same number on every run. The spy adds that
 * ref to each caller's hooks, so a case restores it only once it has unmounted what it drew.
 */
function countRuns(): { readonly runs: number[]; restore(): void } {
  const runs: number[] = [];
  let callers = 0;
  const spy = spyOn(issueTreeHook, "useIssueTree").mockImplementation(
    function useCountedTree(issue) {
      const caller = useRef(0);
      if (caller.current === 0) {
        callers += 1;
        caller.current = callers;
      }
      runs.push(caller.current);
      return useIssueTreeItself(issue);
    }
  );
  return { restore: () => spy.mockRestore(), runs };
}

/** The callers that ran among `runs`, each once, in the order of their first runs. */
function callersOf(runs: readonly number[]): number[] {
  return [...new Set(runs)];
}

/** The issue the cases draw: the fixtures' one, which holds its spec. */
const ISSUE: IssueDetails = issueDetails();

/** A client as the page's tests take one: no read tried again. */
function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
// Every read the slots reach, answered (`stubReads`): the comments with the issue's two threads,
// and the tree read with them, each view's read (`?view=`) with an empty view. Drawn at 1440 px
// (`DESKTOP_WIDTH`), where the Contents pane stands beside the stage and the view row draws each
// view's control.
beforeEach(() => {
  stubs = stubReads();
  spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]);
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
  );
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs) stub.mockRestore();
});

/**
 * The page's slots that read the issue's tree, as the page's root draws them
 * (`IssueOutlinePage.tsx`) at the address the router is at: the view row, the Contents pane and
 * the stage, under the page's providers, the route read from the address at every render, so a
 * press of a view's control moves every slot to that view. The tree's owner stands around them
 * where `owned` says so, as the page mounts it, and none where it does not.
 */
function PageSlots({
  issue,
  owned,
}: {
  readonly issue: IssueDetails;
  readonly owned: boolean;
}): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) throw new Error(`${pathname} names no route`);
  const props = { issue, route };
  const slots = (
    <>
      <ViewRowSlot {...props} />
      <IssueContentsPane {...props} />
      <StageSlot {...props} />
    </>
  );
  return (
    <ProvidersSlot {...props}>
      {owned ? <IssueTreeOwnerProvider issue={issue}>{slots}</IssueTreeOwnerProvider> : slots}
    </ProvidersSlot>
  );
}

/** `PageSlots` at Conversation's address, under what the app provides above the page. */
function drawSlots(client: QueryClient, owned: boolean): RenderResult {
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <PageSlots issue={ISSUE} owned={owned} />
      </AppProviders>
    </MemoryRouter>
  );
}

/** Waits for each of the three readers to have drawn from the tree once no read is out: the view
 *  row's count of the reader's two threads, the Contents list's entry of each thread's head and
 *  the outline's row of it. */
async function drawnFromTheTree(view: RenderResult, client: QueryClient): Promise<void> {
  await waitFor(() => {
    expect(client.isFetching(), "a read the slots sent is still out").toBe(0);
    expect(
      view.container.querySelector('[data-preset-count="conversation"]')?.textContent,
      "the view row drew no count of the reader's threads"
    ).toBe("2");
    for (const head of ["head-a", "head-b"]) {
      expect(
        view.container.querySelector(
          `[data-testid="issue-contents"] [data-contents-entry="${head}"]`
        ),
        `the Contents list drew no entry of ${head}`
      ).not.toBeNull();
      expect(
        view.container.querySelector(`[data-issue-body] [data-node-row][data-node-id="${head}"]`),
        `the outline drew no row of ${head}`
      ).not.toBeNull();
    }
  });
}

// At the base each of the three readers of Conversation's load ran `useIssueTree` itself, each
// on every render of its own. Under the owner only the owner runs it: every run is the first
// caller's, the owner, which renders above them, and each reader draws from its answer.
test("three readers under one owner run one useIssueTree", async () => {
  const counted = countRuns();
  const client = newClient();
  const view = drawSlots(client, true);
  try {
    await drawnFromTheTree(view, client);
    expect(
      callersOf(counted.runs),
      "a reader under the owner ran a useIssueTree of its own"
    ).toEqual([1]);
  } finally {
    view.unmount();
    counted.restore();
  }
});

/** The views a case presses after Conversation's load, in the order it presses them. */
const PRESSED = ["plan", "verification", "decisions", "graph"] as const satisfies readonly Preset[];

/** The element each pressed view draws in the stage: each filter's body, and Graph's outline. */
const VIEW_DRAWN: Readonly<Record<(typeof PRESSED)[number], string>> = {
  decisions: "[data-decisions-body]",
  graph: "[data-issue-body]",
  plan: "[data-plan-body]",
  verification: "[data-verification-body]",
};

// A press on a view draws that view's slot in the stage, which reads the tree through the
// outline's reads (`useOutlineReads`), and, in Graph, the outline's body as well. At the base each
// of those slots ran a `useIssueTree` of its own as it mounted, beside the view row's and the
// Contents pane's, which ran theirs again as the address moved. Under the owner no reader the
// press draws, or draws again, runs one: in the press's commit and after the view's reads have
// answered, every run is the owner's, which reads the address too and builds the tree again only
// where a read it is built from changed (`useIssueTree.ts`'s `builtTree`), as Graph's own read
// does.
test("a press on Plan, Verification, Decisions or Graph builds no tree", async () => {
  const counted = countRuns();
  const client = newClient();
  const view = drawSlots(client, true);
  try {
    await drawnFromTheTree(view, client);
    const controls = within(view.getByRole("toolbar", { name: CONTROL_LABELS.presets }));
    for (const preset of PRESSED) {
      const name = PRESET_NAMES[preset];
      const control = controls.getByRole("button", { name });
      const before = counted.runs.length;
      act(() => control.click());
      expect(
        callersOf(counted.runs.slice(before)).filter((caller) => caller !== 1),
        `the press on ${name} ran a useIssueTree beside the owner's in its commit`
      ).toEqual([]);
      await waitFor(() => {
        expect(control, `the press on ${name} left its control off`).toHaveAttribute(
          "aria-pressed",
          "true"
        );
        expect(
          view.container.querySelector(VIEW_DRAWN[preset]),
          `the stage drew no ${name} view`
        ).not.toBeNull();
        expect(client.isFetching(), `a read ${name} sent is still out`).toBe(0);
      });
      expect(
        callersOf(counted.runs),
        `a reader of ${name}'s view ran a useIssueTree of its own once its reads answered`
      ).toEqual([1]);
    }
  } finally {
    view.unmount();
    counted.restore();
  }
});

/** Holds what each reader of `TreeReader` was last given, by its name. */
type ReadTrees = Partial<Record<string, issueTreeHook.IssueTreeView>>;

/** A reader of `issue`'s tree (`useOwnedIssueTree`) that draws nothing, holding what it was
 *  given under `name` in `held`. */
function TreeReader({
  held,
  issue,
  name,
}: {
  readonly held: ReadTrees;
  readonly issue: IssueDetails;
  readonly name: string;
}): ReactNode {
  held[name] = useOwnedIssueTree(issue);
  return null;
}

// With no owner above them - a case that mounts a slot alone under the page's providers, as the
// existing cases of each slot do - the same three readers each run their own `useIssueTree`, as
// at the base, and draw from it: three callers, which send Conversation's tree read once between
// them, shared by its key.
// An owner of another issue's tree is no owner either: a reader of another issue under it runs
// its own, and is given that issue's tree, while a reader of the owner's issue beside it runs
// none.
test("a slot with no owner reads its own tree as before", async () => {
  const counted = countRuns();
  const client = newClient();
  const view = drawSlots(client, false);
  try {
    await drawnFromTheTree(view, client);
    expect(callersOf(counted.runs), "the three readers with no owner shared a tree").toEqual([
      1, 2, 3,
    ]);
    expect(
      spyOn(api, "getIssueTree").mock.calls.filter(([, query]) => query?.view === undefined),
      "the readers sent Conversation's tree read more than once"
    ).toHaveLength(1);
  } finally {
    view.unmount();
    counted.restore();
  }

  const other: IssueDetails = { ...issueDetails(), key: "CORE-2", number: 2 };
  const route = { key: KEY, kind: "issue" } as const;
  const held: ReadTrees = {};
  const underOwner = countRuns();
  const anotherClient = newClient();
  const owned = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={anotherClient}>
        <ProvidersSlot issue={ISSUE} route={route}>
          <IssueTreeOwnerProvider issue={ISSUE}>
            <TreeReader held={held} issue={ISSUE} name="same" />
            <TreeReader held={held} issue={other} name="other" />
          </IssueTreeOwnerProvider>
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(anotherClient.isFetching(), "a read of either tree is still out").toBe(0)
    );
    expect(
      callersOf(underOwner.runs),
      "the owner and the reader of another issue are not the two callers"
    ).toEqual([1, 2]);
    expect(held.same?.root.id, "the reader of the owner's issue holds no tree of it").toBe(KEY);
    expect(held.other?.root.id, "the reader of another issue holds no tree of it").toBe(other.key);
  } finally {
    owned.unmount();
    underOwner.restore();
  }
});
