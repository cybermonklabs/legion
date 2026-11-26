import { expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Comment, IssueDetails } from "../../api/types";
import { issueDetails } from "./decisions-filter/decisions-fixtures";
import { TWO_THREADS, waitInAct } from "./outline-fixtures";
import * as outlineReads from "./outline-reads";
import { BodySlot } from "./slots/body";
import { ProvidersSlot } from "./slots/providers";
import { AppProviders } from "./slots/stage-fixtures";
import { nodesInOrder } from "./tree-keys";
import { commentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import type { TreeAnswer } from "./tree-read/tree-read";

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
  return treeAnswer(
    [treeNode(key, "issue", null), ...comments.map((comment) => commentNode(comment))],
    [],
    { issue: key }
  );
}

/** Each of the outline's five reads, and the tree read the outline draws its turns from, sent
 *  once. */
const SENT_ONCE = {
  asks: 1,
  comments: 1,
  coverage: 1,
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
    asks: spyOn(api, "listIssueAskThreads").mockResolvedValue([] as never),
    comments: spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
    coverage: spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
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
      <AppProviders client={client}>
        <ProvidersSlot issue={issue} route={route}>
          <BodySlot issue={issue} route={route} />
        </ProvidersSlot>
        {second ? <SecondCaller held={held} issue={issue} /> : null}
      </AppProviders>
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

/** The outline's reads of `issue`, alone under the page's providers, holding what they gave. */
function renderReads(issue: IssueDetails, client: QueryClient, held: Held) {
  const route = { key: issue.key, kind: "issue" } as const;
  return render(
    <MemoryRouter>
      <AppProviders client={client}>
        <ProvidersSlot issue={issue} route={route}>
          <SecondCaller held={held} issue={issue} />
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
}

// The page's first focus waits until every read the rows come from has answered, so it lands
// once where no read still out draws rows above it (`IssueTree`'s opening). A read that answers
// with a failure has answered too: waiting for a read to hold data, a failed one held the first
// focus back for ever. Each of the five reads counts, and so does the tree read, which every node
// of the outline comes from: the case leaves one of each out in turn, answers the other, and then
// fails the one left out.
test("the outline's reads are settled once each read and the tree read have answered, a failure among them", async () => {
  for (const last of ["messages", "tree"] as const) {
    const issue = issueDetails();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
    });
    const messages = Promise.withResolvers<never[]>();
    const tree = Promise.withResolvers<TreeAnswer>();
    const spies = [
      spyOn(api, "listIssueAskThreads").mockResolvedValue([] as never),
      spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
      spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
      spyOn(api, "listIssueMessages").mockReturnValue(messages.promise as never),
      spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
      spyOn(api, "getIssueTree").mockReturnValue(tree.promise),
      spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
      spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
    ];
    const held: Held = {};
    try {
      const view = renderReads(issue, client, held);
      await waitFor(() => expect(api.listIssueMessages).toHaveBeenCalled());
      // The read that is not the last answers.
      if (last === "messages") tree.resolve(treeOf(TWO_THREADS));
      else messages.resolve([]);
      await waitInAct(20);
      expect(held.second?.readsSettled, `settled with the ${last} read still out`).toBe(false);
      // The last fails.
      if (last === "messages") messages.reject(new Error("the messages' read failed"));
      else tree.reject(new Error("the tree read failed"));
      await waitFor(() =>
        expect(held.second?.readsSettled, `a failed ${last} read held the reads unsettled`).toBe(
          true
        )
      );
      expect(held.second?.readsHeld, `a failed ${last} read held its answer`).toBe(false);
      view.unmount();
    } finally {
      for (const spy of spies) spy.mockRestore();
    }
  }
});
