import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import * as issueTreeModule from "./issue-tree";
import {
  IssueTreeOwnerProvider,
  newTreeBuilds,
  TreeBuildsContext,
  useOwnedIssueTree,
} from "./issue-tree-owner";
import { TWO_THREADS } from "./outline-fixtures";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import { ProvidersSlot } from "./slots/providers";
import { AppProviders, commentNodes, stubReads, treeOf } from "./slots/stage-fixtures";
import { viewQuery } from "./tree-read/tree-read";

/**
 * Each filter's view the page holds has its tree built in a background task of its own once the
 * tree's reads have settled (`issue-tree-owner.tsx`), so a later press on that view draws its
 * first screen from a built tree and its commit builds none (`useIssueTree.ts`' `trees`).
 */

const ISSUE: IssueDetails = issueDetails();

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
let builds: ReturnType<typeof spyOn<typeof issueTreeModule, "buildIssueTree">>;

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    ),
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
  for (const stub of stubs) stub.mockRestore();
});

function StageReader({ drawn }: { readonly drawn: { root?: TreeNode } }): ReactNode {
  const [stageBuilds] = useState(newTreeBuilds);
  return (
    <TreeBuildsContext.Provider value={stageBuilds}>
      <RootReader drawn={drawn} />
    </TreeBuildsContext.Provider>
  );
}

function RootReader({ drawn }: { readonly drawn: { root?: TreeNode } }): ReactNode {
  drawn.root = useOwnedIssueTree(ISSUE).root;
  return null;
}

test("a held view's tree is built in a background task once the reads settle, so reading it builds none", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // The Verification view's answer, held before the load's reads answer, as a load sends it.
  client.setQueryData(viewQuery(KEY, "verification").queryKey, treeOf([]));
  const drawn: { root?: TreeNode } = {};
  const route = { key: KEY, kind: "issue" } as const;
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <ProvidersSlot issue={ISSUE} route={route}>
          <IssueTreeOwnerProvider issue={ISSUE}>
            <StageReader drawn={drawn} />
          </IssueTreeOwnerProvider>
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    await waitFor(async () => {
      await tasks.runAll();
      expect(client.isFetching(), "a read of the load is still out").toBe(0);
      expect(
        drawn.root?.children.some((child) => child.id === "head-a"),
        "the outline was never built from the load's reads"
      ).toBe(true);
    });
    await act(async () => {
      await tasks.runAll();
    });
    const trees = drawn.root?.views?.trees;
    expect(trees, "the root holds no view's tree").toBeDefined();
    const before = builds.mock.calls.length;
    expect(trees?.verification, "the Verification view's tree is missing").toBeDefined();
    expect(
      builds.mock.calls.length - before,
      "reading the held view's tree built it then: no task built it before"
    ).toBe(0);
  } finally {
    view.unmount();
  }
});
