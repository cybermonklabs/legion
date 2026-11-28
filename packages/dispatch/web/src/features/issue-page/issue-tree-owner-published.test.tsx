import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
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
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The tree's owner publishes each tree it builds to the stage's readers in the commit that
 * receives it, and to every other reader of the page a task after that commit
 * (`issue-tree-owner.tsx`'s `usePublishedLater`): the commit that draws the stage's view from a
 * read's answer holds none of the work of the Contents list, the view row's counts or Details.
 */

const ISSUE: IssueDetails = issueDetails();

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
/** The tree read's answer, held until the case gives it. */
let answerTree: () => void = () => {};

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getIssueTree").mockImplementation((_key, query = {}) => {
      if (query.view !== undefined) return Promise.resolve(treeOf([]));
      const held = Promise.withResolvers<TreeAnswer>();
      answerTree = () => held.resolve(treeOf(commentNodes(TWO_THREADS)));
      return held.promise;
    }),
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

/** The root each reader was last drawn with, by its name. */
type DrawnRoots = Partial<Record<"page" | "stage", TreeNode>>;

/** A reader of the issue's tree that draws nothing, holding the root it was drawn with. */
function RootReader({
  drawn,
  name,
}: {
  readonly drawn: DrawnRoots;
  readonly name: keyof DrawnRoots;
}): ReactNode {
  drawn[name] = useOwnedIssueTree(ISSUE).root;
  return null;
}

/** A reader under a stage, as the stage's slot stands its readers under its builds
 *  (`slots/stage.tsx`). */
function StageReader({ drawn }: { readonly drawn: DrawnRoots }): ReactNode {
  const [builds] = useState(newTreeBuilds);
  return (
    <TreeBuildsContext.Provider value={builds}>
      <RootReader drawn={drawn} name="stage" />
    </TreeBuildsContext.Provider>
  );
}

/** Whether `root` holds the row of the first thread's head. */
function holdsThreads(root: TreeNode | undefined): boolean {
  return root?.children.some((child) => child.id === "head-a") === true;
}

test("a tree reaches the stage's readers in the commit that receives it and the page's other readers a task after", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const drawn: DrawnRoots = {};
  const route = { key: KEY, kind: "issue" } as const;
  const view = render(
    <MemoryRouter initialEntries={[presetPath(KEY, "conversation")]}>
      <AppProviders client={client}>
        <ProvidersSlot issue={ISSUE} route={route}>
          <IssueTreeOwnerProvider issue={ISSUE}>
            <StageReader drawn={drawn} />
            <RootReader drawn={drawn} name="page" />
          </IssueTreeOwnerProvider>
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    await waitFor(() => expect(client.isFetching(), "the tree read was never sent").toBe(1));
    answerTree();
    // Each task the page asks for, one at a time, until the stage's reader holds the threads: the
    // tree's build and the commit that publishes it.
    for (let ran = 0; !holdsThreads(drawn.stage); ran += 1) {
      expect(ran, "no task drew the tree's threads on the stage").toBeLessThan(50);
      await waitFor(() => expect(tasks.pending()).toBeGreaterThan(0));
      await tasks.runNext();
    }
    expect(
      holdsThreads(drawn.page),
      "the reader beside the stage took the tree in the commit that drew it on the stage"
    ).toBe(false);
    await tasks.runAll();
    expect(
      drawn.page,
      "the tasks after that commit published no tree to the reader beside the stage"
    ).toBe(drawn.stage);
  } finally {
    view.unmount();
  }
});
