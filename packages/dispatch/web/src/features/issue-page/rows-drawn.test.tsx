import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { nextFrame } from "../../__tests__/frames";
import { api } from "../../api/client";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { IssueOutlinePage } from "./IssueOutlinePage";
import { presetPath } from "./issue-address";
import { readComment, TWO_THREADS, waitInAct } from "./outline-fixtures";
import { type TasksByHand, tasksByHand } from "./slices-fixtures";
import {
  AppProviders,
  answerCommentReads,
  commentNodes,
  stubReads,
  treeOf,
} from "./slots/stage-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The page reads no page the outline read has not loaded before its stage has drawn its view's
 * first drawing (`tree-read.ts`'s `useViewDrawn`, decision 538). The tree is built in a task of its
 * own and handed to React in
 * a later one (`useIssueTree.ts`), so a frame after the render that holds the tree read's answer
 * the page holds no row yet: the browser row `once the page has drawn it reads the older comment
 * pages that hold a decision's Change or an address's comment` read an older comment page asked
 * for while the page drew none (CI run 46028555823, `e2e/issue-page-t-pages.e2e.ts:468`).
 *
 * The case holds `scheduler.postTask` by hand (`tasksByHand`), so the tree is built, and its rows
 * drawn, only when the case runs those tasks.
 */

/** A row of the issue the page draws. */
const ROW = "[data-issue-page] [data-node-row]";

/** A comment older than the outline read's first page, which an address names. */
const OLDEST = readComment("head-old", 0);

/** The outline read's first page: the two threads, and the older comment page it names. */
const FIRST: TreeAnswer = {
  ...treeOf(commentNodes(TWO_THREADS)),
  comments_before: `${TWO_THREADS[0]?.created_at},${TWO_THREADS[0]?.id}`,
};

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let tasks: TasksByHand;
let tree: Mock<typeof api.getIssueTree>;

/** How many older comment pages the page has asked for. */
function pagesSent(): number {
  return tree.mock.calls.filter(([, query]) => query?.comments_before !== undefined).length;
}

beforeEach(() => {
  stubs = [
    ...stubReads(),
    spyOn(api, "getIssue").mockResolvedValue(issueDetails()),
    spyOn(api, "listComments").mockResolvedValue([...TWO_THREADS]),
    spyOn(api, "getMyState").mockResolvedValue({}),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    answerCommentReads([...TWO_THREADS, OLDEST]),
  ];
  // The first page at once, the older page never, and each view never.
  tree = spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) => {
    const asked = query as { readonly comments_before?: string; readonly view?: string };
    return asked.view === undefined && asked.comments_before === undefined
      ? FIRST
      : Promise.withResolvers<never>().promise;
  });
  restoreViewport = stubViewport(DESKTOP_WIDTH);
  tasks = tasksByHand();
});
afterEach(() => {
  cleanup();
  tasks.restore();
  restoreViewport();
  for (const stub of stubs.reverse()) stub.mockRestore();
});

// An address names a comment the first page does not hold, so the page wants the older comment
// page from the render that holds the first page's answer on. With every read answered and the
// tree's tasks held, frames pass and the page draws no row: it asks for no older page. Once the
// tasks run and a row is drawn, it asks for the older page, once.
test("the page asks for an older comment page only once it has drawn a row of the issue", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <MemoryRouter initialEntries={[`${presetPath(KEY, "conversation")}?comment=${OLDEST.id}`]}>
      <AppProviders client={client}>
        <IssueOutlinePage />
      </AppProviders>
    </MemoryRouter>
  );
  await waitFor(() => expect(tree).toHaveBeenCalled());
  // Every read has answered, and the frame the reading would have waited for has passed.
  await waitInAct(50);
  await act(() => nextFrame());
  await waitInAct(50);
  expect(view.container.querySelector(ROW), "the page drew a row before its tasks ran").toBeNull();
  expect(pagesSent(), "an older comment page was asked for before the page drew a row").toBe(0);

  for (let ran = 0; ran < 20 && view.container.querySelector(ROW) === null; ran += 1) {
    expect(await tasks.runNext(), "the page asked for no task that draws its rows").toBe(true);
  }
  expect(view.container.querySelector(ROW), "the page drew no row").not.toBeNull();
  await waitFor(() =>
    expect(pagesSent(), "no older comment page was asked for once a row was drawn").toBe(1)
  );
});
