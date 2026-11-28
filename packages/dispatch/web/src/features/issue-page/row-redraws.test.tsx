import { afterEach, beforeAll, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import * as reactQuery from "@tanstack/react-query";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import { issueTreeKey } from "../../api/queries";
import type { Comment, DisplayTree } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KEY } from "./decisions-filter/decisions-fixtures";
import { readComment, waitInAct } from "./outline-fixtures";
import { recordRowDraws, treeOfComments } from "./row-draw-fixtures";
import { drawStage, focusedRow, stubReads, treeOf } from "./slots/stage-fixtures";

/**
 * A row of the outline is drawn again only where what it draws changed (`NodeRow`'s `memo`,
 * `TreeRows.tsx`'s authors and handlers, `IssueTree.tsx`'s shared tree and thread marks). At the
 * base every row of Conversation was drawn again at each render of the tree: its `author` a new
 * object and its `onFocus` and `onToggle` new closures each time, and its `node` a new object at
 * each read, since every read builds the tree again. On the seed that was every one of 92 rows
 * four times in one load, each of those renders running the row's layout effects again.
 *
 * What a case counts is each commit that draws a row's words again (`row-draw-fixtures.tsx`'s
 * `recordRowDraws`): the row's `RowText`, under a `Profiler` of its own named by the row's node,
 * records each commit in which it or anything it draws rendered (React calls a `Profiler`'s
 * `onRender` only for a commit that rendered something under it), as `__tests__/commits.tsx`'s
 * `recordCommits` records a control's commits. A row drawn again draws its words again, since
 * `RowText` is no `memo`.
 *
 * No case here holds that a name that arrives draws again only the rows that cite it: the
 * profile of a load at the base found no render of a row caused by a context - none of the four
 * renders of any of Conversation's 92 rows - so in a real load the names do not draw rows again
 * (dispatch://INFRA-5939/artifact/t6-profile-md), and the plan reads the names through a store
 * of their own only where that profile shows they do.
 */

/** A client as the page's tests take one: no read tried again. */
function newClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

/** The issue's comments: three threads, the first with a reply, the reader's (alice's) own. */
const THREADS: readonly Comment[] = [
  readComment("head-a", 1),
  readComment("reply-a", 2, "head-a"),
  readComment("head-b", 3),
  readComment("head-c", 4),
];

/** The comments' node ids, in the outline's order. */
const ROWS = THREADS.map((comment) => comment.id);

/** The server's reading of each text the cases' comments hold, read once for each text across
 *  the cases (`treeOfComments`). */
const READINGS = new Map<string, DisplayTree | undefined>();

// The page's engine reads each comment's words into the reading the read answers with
// (`displayTree`), so it is loaded before any case's first read.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

let stubs: { mockRestore(): void }[] = [];
let restoreViewport: () => void = () => {};
let comments: readonly Comment[] = THREADS;
beforeEach(() => {
  stubs = stubReads();
  comments = THREADS;
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.view === undefined ? treeOfComments(comments, READINGS) : treeOf([])
  );
  restoreViewport = stubViewport(DESKTOP_WIDTH);
});
afterEach(() => {
  cleanup();
  restoreViewport();
  for (const stub of stubs) stub.mockRestore();
});

/** The outline's row of the node `id`. */
function rowOf(view: RenderResult, id: string): HTMLElement | null {
  return view.container.querySelector<HTMLElement>(
    `[data-issue-body] [data-node-row][data-node-id="${id}"]`
  );
}

/** Waits for Conversation to draw a row of each of `ROWS` once no read is out, and then for the
 *  commits the page makes of its own after that - the focus it opens on, the frames its rows
 *  place themselves in - to be done, so a case counts from there only what it does. */
async function drawnAndSettled(view: RenderResult, client: QueryClient): Promise<void> {
  await waitFor(() => {
    expect(client.isFetching(), "a read the page sent is still out").toBe(0);
    for (const id of ROWS) expect(rowOf(view, id), `no row of ${id} was drawn`).not.toBeNull();
  });
  await waitInAct(100);
}

// A read that answers again with one comment's words changed builds the whole tree again: every
// node of it a new object, though only that comment's holds anything new. Only the row of that
// comment draws again, once; the other rows keep what they were given, the reply under the first
// thread its thread's read mark among it, and are not drawn.
test("a read that changes one row draws only that row again", async () => {
  const draws = recordRowDraws();
  const client = newClient();
  const view = drawStage("", { client, preset: "conversation" });
  try {
    await drawnAndSettled(view, client);
    const mark = draws.mark();
    comments = THREADS.map((comment) =>
      comment.id === "head-b" ? { ...comment, body: "Other words of head-b." } : comment
    );
    await act(() => client.invalidateQueries({ queryKey: issueTreeKey(KEY) }));
    await waitFor(() => {
      expect(rowOf(view, "head-b")?.textContent, "the read's new words were not drawn").toContain(
        "Other words of head-b."
      );
      expect(client.isFetching(), "a read the page sent is still out").toBe(0);
    });
    await waitInAct(100);
    expect(draws.since(mark), "the rows drawn again, each with its commits").toEqual({
      "head-b": 1,
    });
  } finally {
    view.unmount();
    draws.restore();
  }
});

/** Whether `value` is one of the outline's own records - a node of the tree it builds
 *  (`TreeNode.alsoUnder`) or a row it draws (`DrawnRow.trail`) - rather than a read's answer,
 *  which TanStack Query compares with the same `replaceEqualDeep` as it shares each answer. */
function isOutlineRecord(value: unknown): boolean {
  return typeof value === "object" && value !== null && ("alsoUnder" in value || "trail" in value);
}

// A read that answers again with the same values in objects that are all new builds the whole
// tree again, and the tree is shared with the one before once, as it is built (`IssueTree.tsx`'s
// `sharedTree`): a render after it that builds no tree - the focus moved to the next row, which
// `j` does - compares no node or row of the outline with the one before it. Compared at every
// render instead, each drawn row's whole subtree was walked again at every move the reader made,
// for as long as that tree stood. The stage draws no `issue` keymap scope, so the case moves the
// focus itself.
test("after a read that builds the tree again with the same values, a move of the focus compares no node", async () => {
  // No answer keeps the object the cache held, as one that changed nothing does not by default.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, structuralSharing: false } },
  });
  const view = drawStage("", { client, preset: "conversation" });
  const compared = spyOn(reactQuery, "replaceEqualDeep");
  // The `beforeEach` answers the tree read with its spy; this case counts the spy's calls.
  const treeReads = api.getIssueTree as Mock<typeof api.getIssueTree>;
  try {
    await drawnAndSettled(view, client);
    const reads = treeReads.mock.calls.length;
    await act(() => client.invalidateQueries({ queryKey: issueTreeKey(KEY) }));
    await waitFor(() => expect(client.isFetching(), "a read the page sent is still out").toBe(0));
    await waitInAct(100);
    expect(treeReads.mock.calls.length, "the tree was not read again").toBeGreaterThan(reads);
    await act(async () => rowOf(view, "head-a")?.focus());
    await waitInAct(100);
    compared.mockClear();
    await act(async () => rowOf(view, "reply-a")?.focus());
    await waitFor(() => expect(focusedRow(), "the focus did not move").toBe("reply-a"));
    await waitInAct(100);
    expect(
      compared.mock.calls.filter(([was]) => isOutlineRecord(was)).length,
      "a node or a row of the outline was compared with the one before it"
    ).toBe(0);
  } finally {
    compared.mockRestore();
    view.unmount();
  }
});

/** The comment a page of older comments brings: written before every one of `THREADS`. */
const OLDER = readComment("old-a", 0);

// A page of older comments puts its threads before the ones drawn (`mergeCommentPage`), so every
// node drawn before it stands at another place among the issue's children. The tree is shared with
// the one before by each node's id, not its place (`sharedTree`), as a row is keyed by its path of
// ids: no row drawn before the page is drawn again, and the page's own row is drawn.
test("a page of older comments draws its own row and none of the rows drawn before it", async () => {
  const first = {
    ...treeOfComments(THREADS, READINGS),
    comments_before: `${THREADS[0]?.created_at},${THREADS[0]?.id}`,
  };
  const older = treeOfComments([OLDER], READINGS);
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.comments_before !== undefined ? older : query.view === undefined ? first : treeOf([])
  );
  const draws = recordRowDraws();
  const client = newClient();
  const view = drawStage("", { client, preset: "conversation" });
  try {
    await drawnAndSettled(view, client);
    const control = await waitFor(() => {
      const found = view.container.querySelector<HTMLButtonElement>(
        "[data-issue-body] [data-older-comments] button"
      );
      if (found === null) throw new Error("no control reads the older comments");
      return found;
    });
    const mark = draws.mark();
    await act(async () => control.click());
    await waitFor(() => {
      expect(rowOf(view, OLDER.id), "the older page's row was not drawn").not.toBeNull();
      expect(client.isFetching(), "a read the page sent is still out").toBe(0);
    });
    await waitInAct(100);
    const drawn = draws.since(mark);
    expect(
      ROWS.filter((id) => drawn[id] !== undefined),
      "rows drawn before the page were drawn again"
    ).toEqual([]);
    expect(drawn[OLDER.id], "the older page's row was not drawn").toBeGreaterThan(0);
  } finally {
    view.unmount();
    draws.restore();
  }
});
