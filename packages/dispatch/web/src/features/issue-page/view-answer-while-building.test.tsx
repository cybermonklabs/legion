import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { issueCoverageQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
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
 * A filter's view answer that comes while the outline is built again for another read
 * (`useIssueTree.ts`, its views held beside the built tree) is published at once, over the tree
 * the outline holds and the reads that tree was built from: the view the reader pressed draws its
 * first screen from its answer in the commit that receives it, before the outline's new build
 * runs, rather than after that build and its publication.
 */

const ISSUE: IssueDetails = issueDetails();

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
      query.view === undefined ? treeOf(commentNodes(TWO_THREADS)) : treeOf([])
    ),
  ];
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  tasks = tasksByHand();
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs) stub.mockRestore();
});

/** A reader of the tree under a stage, as the stage's view reads it, holding the root it was
 *  last drawn with. */
function StageReader({ drawn }: { readonly drawn: { root?: TreeNode } }): ReactNode {
  const [builds] = useState(newTreeBuilds);
  return (
    <TreeBuildsContext.Provider value={builds}>
      <RootReader drawn={drawn} />
    </TreeBuildsContext.Provider>
  );
}

function RootReader({ drawn }: { readonly drawn: { root?: TreeNode } }): ReactNode {
  drawn.root = useOwnedIssueTree(ISSUE).root;
  return null;
}

test("a view's answer that comes while the outline is built again draws before that build", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
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
    // The load: every read answered, and the outline built from them.
    await waitFor(async () => {
      await tasks.runAll();
      expect(client.isFetching(), "a read of the load is still out").toBe(0);
      expect(
        drawn.root?.children.some((child) => child.id === "head-a"),
        "the outline was never built from the load's reads"
      ).toBe(true);
    });
    await tasks.runAll();
    const built = drawn.root;

    // Another read the outline is built from answers anew, so the outline is built again, in a
    // task the case holds.
    const coverage = issueCoverageQuery(KEY).queryKey;
    await act(async () => {
      client.setQueryData(coverage, (held) =>
        held === undefined ? held : { ...held, has_plan: !held.has_plan }
      );
    });
    expect(tasks.pending(), "no task was asked for to build the outline again").toBeGreaterThan(0);

    // The Verification view's answer comes while that build waits.
    const answer = treeOf([]);
    await act(async () => {
      client.setQueryData(viewQuery(KEY, "verification").queryKey, answer);
    });
    expect(
      drawn.root?.views?.verification,
      "the view's answer waited for the outline's build before it reached the stage"
    ).toBe(answer);
    expect(
      drawn.root?.children,
      "the view's answer was published over a tree other than the one the outline holds"
    ).toBe(built?.children);

    await tasks.runAll();
    expect(drawn.root?.views?.verification, "the outline's build dropped the view's answer").toBe(
      answer
    );
  } finally {
    view.unmount();
  }
});
