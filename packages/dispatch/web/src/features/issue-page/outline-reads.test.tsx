import { expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Comment, IssueDetails } from "../../api/types";
import { MarginProvider } from "../margin/margin-context";
import { KeymapProvider } from "../shell/KeymapProvider";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { TWO_THREADS, waitInAct } from "./outline-fixtures";
import * as outlineReads from "./outline-reads";
import { BodySlot } from "./slots/body";
import { ProvidersSlot } from "./slots/providers";
import { nodesInOrder } from "./tree-keys";
import type { TreeAnswer, TreeAnswerNode } from "./tree-read/tree-read";

/**
 * The outline's reads (`useOutlineReads`) called from two places of one page: the body, and a
 * second caller beside it, which stands in for Details' agents-only turn - a later part's caller,
 * which reads them while an address names an item beside the outline. The reads and how they are
 * kept fresh are in one place, so each read is one request shared by its key, a caller mounted
 * again reads none of them again, and both callers hold one tree.
 */

// The hook as its module defines it, read before the case's spy stands in for the module's
// export, which every import of it - this file's own included - reads from then on.
const useOutlineReadsItself = outlineReads.useOutlineReads;

/** What each caller's last render of the outline's reads gave it. */
interface Held {
  body?: outlineReads.OutlineReads;
  second?: outlineReads.OutlineReads;
}

/** A caller of the outline's reads beside the body, standing in for a later part's, holding
 *  what they gave it. */
function SecondCaller({ held, issue }: { held: Held; issue: IssueDetails }): ReactNode {
  held.second = useOutlineReadsItself(issue);
  return null;
}

/** The outline's reads as the body calls them, holding what they gave it in `held`. */
function bodyReads(held: Held): typeof useOutlineReadsItself {
  return function useBodyReads(issue) {
    const given = useOutlineReadsItself(issue);
    held.body = given;
    return given;
  };
}

/** The tree read's answer for an issue that holds `comments` and nothing else, as
 *  `issue-tree.test.ts` answers a comments read: the issue, and each comment a node under the
 *  comment it replies to, else under the issue. The outline draws its turns from it. */
function treeOf(comments: readonly Comment[]): TreeAnswer {
  const key = issueDetails().key;
  const node = (id: string, supertag: string, parent: string | null): TreeAnswerNode => ({
    attributes: {},
    author: [],
    id,
    made_at: "2026-11-05T00:00:00Z",
    parent,
    rank: null,
    state: "live",
    supertag,
    text: "",
    visibility: "project",
  });
  return {
    converted: false,
    cursor: null,
    nodes: [
      node(key, "issue", null),
      ...comments.map((comment) => ({
        ...node(comment.id, "comment", comment.reply_to ?? key),
        author: [comment.author],
        made_at: comment.created_at,
        text: comment.body,
      })),
    ],
    tree: null,
    tuples: [],
    view_of: null,
  };
}

/** Each of the outline's six reads, and the tree read the outline draws its turns from, sent
 *  once. */
const SENT_ONCE = {
  asks: 1,
  comments: 1,
  coverage: 1,
  decisions: 1,
  messages: 1,
  results: 1,
  tree: 1,
};

test("the body and a second caller of the outline's reads send each read once and hold one tree", async () => {
  const issue: IssueDetails = { ...issueDetails(), artifacts: [] };
  const route = { key: issue.key, kind: "issue" } as const;
  // The app's client's `staleTime` (`main.tsx`), which a read that sets none of its own takes.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  const reads = {
    asks: spyOn(api, "listIssueAsks").mockResolvedValue([] as never),
    comments: spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
    coverage: spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    decisions: spyOn(api, "getIssueDecisions").mockResolvedValue({
      decisions: [],
      document: null,
      open_questions: [],
    } as never),
    messages: spyOn(api, "listIssueMessages").mockResolvedValue([] as never),
    results: spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
    tree: spyOn(api, "getIssueTree").mockResolvedValue(treeOf(TWO_THREADS)),
  };
  const sent = () =>
    Object.fromEntries(Object.entries(reads).map(([name, read]) => [name, read.mock.calls.length]));
  // The reader and the names, answered so that neither reaches the network.
  const others = [
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
  ];
  const held: Held = {};
  const body = spyOn(outlineReads, "useOutlineReads").mockImplementation(bodyReads(held));
  const draw = (second: boolean) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginProvider>
            <ProvidersSlot issue={issue} route={route}>
              <BodySlot issue={issue} route={route} />
            </ProvidersSlot>
            {second ? <SecondCaller held={held} issue={issue} /> : null}
          </MarginProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const view = render(draw(true));
    await waitFor(() => {
      expect(held.body?.readsHeld, "the body's reads were never held").toBe(true);
      expect(held.second?.readsHeld, "the second caller's reads were never held").toBe(true);
    });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(sent(), "a read of the outline was sent for each of its two callers").toEqual(SENT_ONCE);
    const { body: bodyGave, second: secondGave } = held;
    if (bodyGave === undefined || secondGave === undefined) {
      throw new Error("a caller drew no tree");
    }
    expect(
      nodesInOrder(bodyGave.root).map((node) => node.id),
      "the body's tree holds no turn of the comments' read"
    ).toEqual(expect.arrayContaining(TWO_THREADS.map((comment) => comment.id)));
    expect(secondGave.root, "the second caller holds another tree than the body's").toEqual(
      bodyGave.root
    );

    // The second caller mounted again more than 30 s after the reads answered reads none of
    // them again: it keeps them fresh by events, as the body does.
    view.rerender(draw(false));
    setSystemTime(Date.now() + 31_000);
    view.rerender(draw(true));
    // Long enough for a read the mount started to have been sent.
    await waitInAct(50);
    expect(
      sent(),
      "the second caller mounted again past 30 s read the outline's reads again"
    ).toEqual(SENT_ONCE);
  } finally {
    setSystemTime();
    body.mockRestore();
    for (const spy of [...Object.values(reads), ...others]) spy.mockRestore();
  }
});
