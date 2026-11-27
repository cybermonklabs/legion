import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, cleanup, type RenderResult, waitFor } from "@testing-library/react";
import { Profiler } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
import { issueTreeKey } from "../../api/queries";
import type { Comment, DisplayTree } from "../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../margin/margin-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KEY } from "./decisions-filter/decisions-fixtures";
import { readComment, waitInAct } from "./outline-fixtures";
import * as rowText from "./RowText";
import { drawStage, stubReads, treeOf } from "./slots/stage-fixtures";
import { commentNode } from "./tree-read/tree-fixtures";

/**
 * A row of the outline is drawn again only where what it draws changed (`NodeRow`'s `memo`,
 * `TreeRows.tsx`'s authors and handlers, `IssueTree.tsx`'s kept rows and thread marks). At the
 * base every row of Conversation was drawn again at each render of the tree: its `author` a new
 * object and its `onFocus` and `onToggle` new closures each time, and its `node` a new object at
 * each read, since every read builds the tree again. On the seed that was every one of 92 rows
 * four times in one load, each of those renders running the row's layout effects again.
 *
 * What a case counts is each commit that draws a row's words again: the row's `RowText`, under a
 * `Profiler` of its own named by the row's node, records each commit in which it or anything it
 * draws rendered (React calls a `Profiler`'s `onRender` only for a commit that rendered something
 * under it), as `__tests__/commits.tsx`'s `recordCommits` records a control's commits. A row
 * drawn again draws its words again, since `RowText` is no `memo`.
 *
 * No case here holds that a name that arrives draws again only the rows that cite it: the
 * profile of a load at the base found no render of a row caused by a context - none of the four
 * renders of any of Conversation's 92 rows - so in a real load the names do not draw rows again
 * (dispatch://INFRA-5939/artifact/t6-profile-md), and the plan reads the names through a store
 * of their own only where that profile shows they do.
 */

/** `RowText` as its module defines it, read before a case's spy stands in for the module's
 *  export, which `NodeRow.tsx`'s import reads from then on. */
const RowTextItself = rowText.RowText;

/** Each commit that draws a row's words, from now until `restore`. */
interface RowDraws {
  /** How many commits drew each row's words since `mark` was called, by the row's node id. */
  readonly since: (mark: number) => Readonly<Record<string, number>>;
  /** The point in the record a later `since` counts from. */
  readonly mark: () => number;
  readonly restore: () => void;
}

/** Records each commit that draws a row's words (`RowText`), by the row's node id: the id of the
 *  record its words are (`TextSource.id`), which is the node's for a comment. */
function recordRowDraws(): RowDraws {
  const draws: string[] = [];
  const spy = spyOn(rowText, "RowText").mockImplementation(function RecordedRowText(props) {
    return (
      <Profiler id={props.source?.id ?? props.markdown} onRender={(id) => draws.push(id)}>
        <RowTextItself {...props} />
      </Profiler>
    );
  });
  return {
    mark: () => draws.length,
    restore: () => spy.mockRestore(),
    since: (mark) => {
      const counts: Record<string, number> = {};
      for (const id of draws.slice(mark)) counts[id] = (counts[id] ?? 0) + 1;
      return counts;
    },
  };
}

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

/** The server's reading of each text the cases' comments hold, read once for each text: the
 *  page's engine gives each block a new id at every parse, where the server answers the reading
 *  it stored, the same at every read of the same words. */
const READINGS = new Map<string, DisplayTree | undefined>();

/** `comments` as Conversation's tree read answers them, each with the server's reading of its
 *  words, as the route sends it, so the rows draw from the reading and parse nothing. */
function treeOfComments(comments: readonly Comment[]) {
  return treeOf(
    comments.map((comment) => {
      if (!READINGS.has(comment.body)) READINGS.set(comment.body, displayTree(comment.body));
      return { ...commentNode(comment), text_tree: READINGS.get(comment.body) };
    })
  );
}

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
  spyOn(api, "listComments").mockImplementation(async () => [...comments]);
  spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.view === undefined ? treeOfComments(comments) : treeOf([])
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
