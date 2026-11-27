import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import { issueAskThreadsQuery } from "../../api/queries";
import type { Artifact } from "../../api/types";
import { specArtifact } from "../margin/margin-fixture";
import { type MarginOwner, type MarkPlacement, useMarginItems } from "../margin/useMarginItems";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference } from "../refs/routes";
import { issueDetails, KEY, SPEC_DOCUMENT } from "./decisions-filter/decisions-fixtures";
import { buildIssueTree, type TreeNode } from "./issue-tree";
import { type MarginDrawer, MarginSlotProvider } from "./margin-slot";
import { readComment } from "./outline-fixtures";
import { blockNodeId } from "./tree-drafts";
import { commentNode, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { appClient, focusPastStaleness, restoreWindowFocus } from "./window-focus-fixture";

const OWNER: MarginOwner = { key: "CORE-1", kind: "issue" };

/** No card of the margin is placed: what is asked here is which reads go out, not where a card
 *  stands. */
const NO_PLACEMENTS: ReadonlyMap<string, MarkPlacement> = new Map();

/** The Details drawer, which the new issue page's shell hands the margin and no other page does. */
const drawer: MarginDrawer = { open: true, setOpen: () => {}, setPage: () => {} };

let restore = () => {};

afterEach(() => {
  restore();
  restore = () => {};
  restoreWindowFocus();
});

/** Every read the margin of an issue sends on its Comments tab with no pin: the inbox, the
 *  reader's state, the issue's comments and its asks (`useAnsweredAsks`). */
interface MarginReads {
  readonly getInbox: Mock<typeof api.getInbox>;
  readonly getMyState: Mock<typeof api.getMyState>;
  readonly listComments: Mock<typeof api.listComments>;
  readonly listIssueAsks: Mock<typeof api.listIssueAsks>;
  readonly listIssueAskThreads: Mock<typeof api.listIssueAskThreads>;
}

/** Each of the margin's reads, answered empty. */
function stubMarginReads(): MarginReads {
  const reads: MarginReads = {
    getInbox: spyOn(api, "getInbox").mockResolvedValue([]),
    getMyState: spyOn(api, "getMyState").mockResolvedValue({}),
    listComments: spyOn(api, "listComments").mockResolvedValue([]),
    listIssueAsks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
    listIssueAskThreads: spyOn(api, "listIssueAskThreads").mockResolvedValue([]),
  };
  restore = () => {
    for (const read of Object.values(reads)) read.mockRestore();
  };
  return reads;
}

/** The decisions beside the issue's spec, answered empty: the margin reads them only while the
 *  spec is shown (`useMarginItems`' `showsDecisions`). */
function stubDecisionsRead(): Mock<typeof api.getIssueDecisions> {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue({
    decisions: [],
    document: null,
    entries: [],
    open_questions: [],
  });
  const before = restore;
  restore = () => {
    before();
    read.mockRestore();
  };
  return read;
}

/** The margin's reads of the issue (`useMarginItems`, which reads the asks through
 *  `useAnsweredAsks`): in the Details drawer where `inDrawer` is given, as the new page's shell
 *  draws the margin, and with no drawer otherwise, as every other page draws it. Where `spec` is
 *  given, the margin is beside that document, its editor up and shown, and so reads its decisions
 *  too; `documentUp` says whether a document's editor is up, the spec's or another's. */
function renderMargin(
  client: QueryClient,
  inDrawer: MarginDrawer | undefined,
  spec?: Artifact,
  documentUp = spec !== undefined
) {
  return renderHook(
    ({ up }: { up: boolean }) =>
      useMarginItems(OWNER, "comments", spec, NO_PLACEMENTS, NO_PLACEMENTS, undefined, {
        documentShown: spec !== undefined,
        documentUp: up,
      }),
    {
      initialProps: { up: documentUp },
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>
          <MarginSlotProvider drawer={inDrawer}>{children}</MarginSlotProvider>
        </QueryClientProvider>
      ),
    }
  );
}

/** Waits until each read, and the decisions' read where it is given, has gone out once and every
 *  one has answered: the comments' once where the margin reads them, which in Details it does
 *  only beside a document (`commentsRead`). */
async function firstAnswers(
  client: QueryClient,
  reads: MarginReads,
  inDrawer: boolean,
  decisions?: Mock<typeof api.getIssueDecisions>,
  commentsRead = true
): Promise<void> {
  await waitFor(() => {
    expect(reads.getInbox).toHaveBeenCalledTimes(1);
    expect(reads.getMyState).toHaveBeenCalledTimes(1);
    expect(reads.listComments).toHaveBeenCalledTimes(commentsRead ? 1 : 0);
    expect(reads.listIssueAsks).toHaveBeenCalledTimes(inDrawer ? 0 : 1);
    expect(reads.listIssueAskThreads).toHaveBeenCalledTimes(inDrawer ? 1 : 0);
    if (decisions !== undefined) expect(decisions).toHaveBeenCalledTimes(1);
    expect(client.isFetching()).toBe(0);
  });
}

/** What the margin is beside in the cases below: no document, and the issue's spec, shown, beside
 *  which it reads the spec's decisions too. */
const BESIDE = [
  ["no document", undefined],
  ["the shown spec", specArtifact],
] as const;

// On the new issue page the outline reads the issue's asks under the margin's keys and keeps them
// fresh by events (`useIssueTree.ts`), so the margin in the Details drawer holds them the same
// way: a window focus past the app's 30 s staleness reads them no more. It reads the issue's
// comments only beside a document, and holds them the same way. Beside the shown spec it reads
// the decisions too, which the outline no longer reads, since its decisions are nodes of the tree
// read, but which the margin keeps fresh by events under the page's same rule
// (`useNewIssuePageFreshness`), and the same focus reads them again no more than it reads the
// asks and the comments. The inbox and the reader's state keep the app's defaults there too, so
// their second read shows the focus was handled.
test.each(
  BESIDE
)("in Details beside %s, a focus past 30 s sends no read of the margin's asks, comments or decisions", async (_beside, spec) => {
  const reads = stubMarginReads();
  const decisions = spec === undefined ? undefined : stubDecisionsRead();
  const client = appClient();
  const view = renderMargin(client, drawer, spec);
  try {
    await firstAnswers(client, reads, true, decisions, spec !== undefined);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
    });
    expect(reads.listIssueAsks).not.toHaveBeenCalled();
    expect(reads.listIssueAskThreads).toHaveBeenCalledTimes(1);
    expect(reads.listComments).toHaveBeenCalledTimes(spec === undefined ? 0 : 1);
    if (decisions !== undefined) expect(decisions).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

// Every other page has no drawer, and there the margin keeps the app's defaults: the same focus
// reads the asks and the comments again, and the decisions beside the shown spec too.
test.each(
  BESIDE
)("with no drawer beside %s, a focus past 30 s reads the margin's asks, comments and decisions again", async (_beside, spec) => {
  const reads = stubMarginReads();
  const decisions = spec === undefined ? undefined : stubDecisionsRead();
  const client = appClient();
  const view = renderMargin(client, undefined, spec);
  try {
    await firstAnswers(client, reads, false, decisions);
    await focusPastStaleness(client);
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(2);
      expect(reads.getMyState).toHaveBeenCalledTimes(2);
      expect(reads.listIssueAsks).toHaveBeenCalledTimes(2);
      expect(reads.listComments).toHaveBeenCalledTimes(2);
      if (decisions !== undefined) expect(decisions).toHaveBeenCalledTimes(2);
    });
  } finally {
    view.unmount();
  }
});

test("the new page's margin sends no plain asks read when the outline holds the thread read", async () => {
  const reads = stubMarginReads();
  const client = appClient();
  const { queryKey } = issueAskThreadsQuery(OWNER.key);
  client.setQueryData(queryKey, []);
  const view = renderMargin(client, drawer);
  try {
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    expect(reads.listIssueAsks).not.toHaveBeenCalled();
    expect(reads.listIssueAskThreads).not.toHaveBeenCalled();
    expect(client.getQueryCache().find({ exact: true, queryKey })?.getObserversCount()).toBe(1);
  } finally {
    view.unmount();
    client.clear();
  }
});

// The new page reads no comment list at load: in Details the margin's cards are drawn only while
// a document's editor is up (`slots/details.tsx`), so the margin reads the issue's comments then,
// once, and not before; every other page has no drawer and reads them at mount, as it always has.
test("in Details the margin reads the issue's comments only once a document's editor is up, and once", async () => {
  const reads = stubMarginReads();
  const client = appClient();
  const view = renderMargin(client, drawer, undefined, false);
  try {
    await waitFor(() => {
      expect(reads.getInbox).toHaveBeenCalledTimes(1);
      expect(reads.listIssueAskThreads).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    expect(
      reads.listComments,
      "the margin read the comments with no document up"
    ).not.toHaveBeenCalled();
    expect(
      client.getQueryCache().find({ exact: true, queryKey: ["comments", OWNER.key] }),
      "the margin observes the issue's comments list with no document up"
    ).toBeUndefined();
    view.rerender({ up: true });
    await waitFor(() => expect(reads.listComments).toHaveBeenCalledTimes(1));
    view.rerender({ up: false });
    view.rerender({ up: true });
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(reads.listComments, "a document up again read the comments again").toHaveBeenCalledTimes(
      1
    );
  } finally {
    view.unmount();
    client.clear();
  }
  reads.listComments.mockClear();
  const outside = appClient();
  const elsewhere = renderMargin(outside, undefined, undefined, false);
  try {
    await waitFor(() => expect(reads.listComments).toHaveBeenCalledTimes(1));
  } finally {
    elsewhere.unmount();
    outside.clear();
  }
});

// Referenced by takes the new page's freshness from the drawer as the margin does
// (`useNewIssuePageFreshness`), so a page with no drawer keeps the app's defaults for it too: the
// same focus reads what references the record again. In Details it reads it no second time
// (`IssueMargin.test.tsx`'s window-focus case).
test("with no drawer, a focus past 30 s reads Referenced by's list again", async () => {
  const reference = buildDispatchReference({ key: "CORE-1", kind: "issue" });
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: "CORE-1", issue_key: "CORE-1", kind: "issue", ref: reference }),
  });
  restore = () => getReferences.mockRestore();
  const client = appClient();
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginSlotProvider drawer={undefined}>
          <ReferencedBy reference={reference} />
        </MarginSlotProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() => {
      expect(getReferences).toHaveBeenCalledTimes(1);
      expect(client.isFetching()).toBe(0);
    });
    await focusPastStaleness(client);
    await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
  }
});

/** The id of each node `id` is drawn under in the tree `root`, one for each of its rows. */
function parentsOf(root: TreeNode, id: string): string[] {
  const found: string[] = [];
  const walk = (node: TreeNode): void => {
    for (const child of node.children) {
      if (child.id === id) found.push(node.id);
      walk(child);
    }
  };
  walk(root);
  return found;
}

// INFRA-1196's decision 513: a decision whose `from_ask` names a question a person answered
// restates that answer, and the outline draws no row for it (`tree-read/tree-nodes.ts`'
// `firstDraft`). Nothing written on it is lost. A comment on its words is a comment on a node's
// text, which is no row of the outline (`onNodeText`), and the margin in Details draws it as it
// draws a comment on any other block of the shown spec: its thread, quoting the block's words as
// they are stored (`anchoredThreadComments`). A comment on the decision that is tied to no text
// hangs under the question, the decision's nearest drawn ancestor. The row beside which the
// margin draws a thread is INFRA-5706's.
test("a comment on the words of a decision that restates a person's answer is in the margin with its quote, and one tied to no text hangs under the question", async () => {
  const words = "Keep every address the old page answered.";
  const onWords = {
    ...readComment("c-on-words", 0),
    anchor: {
      artifact_id: SPEC_DOCUMENT.id,
      block_id: "d-restates",
      mark_id: "mark-c-on-words",
      orphaned: false,
      quote: words,
      version: 1,
    },
  };
  const loose = readComment("c-loose", 0);
  // The read answers a person's decision on the question, as it does on every ask a person
  // answered, and hangs both comments under the decision.
  const tree = treeAnswer([
    treeNode("a-answered", "ask", KEY, {
      decision: {},
      text: "Which addresses must the new page still answer?",
    }),
    treeNode("d-restates", "decision", "a-answered", {
      decision: { asks: ["a-answered"], waiting_review: true },
      document: { artifact_id: SPEC_DOCUMENT.id, position: 1, slug: SPEC_DOCUMENT.slug },
      text: words,
    }),
    commentNode(onWords, "d-restates"),
    commentNode(loose, "d-restates"),
  ]);
  const root = buildIssueTree({ issue: issueDetails(), tree });
  const decision = blockNodeId(SPEC_DOCUMENT.slug, "d-restates");
  expect(
    ["a-answered", decision, onWords.id, loose.id].map((id) => [id, parentsOf(root, id)])
  ).toEqual([
    ["a-answered", [KEY]],
    [decision, []],
    [onWords.id, []],
    [loose.id, ["a-answered"]],
  ]);

  const reads = stubMarginReads();
  reads.listComments.mockResolvedValue([onWords, loose]);
  const decisions = stubDecisionsRead();
  const client = appClient();
  const view = renderMargin(client, drawer, SPEC_DOCUMENT);
  try {
    await firstAnswers(client, reads, true, decisions);
    expect(
      view.result.current.threads.map((thread) => [thread.key, thread.anchor?.quote]),
      "the margin beside the spec does not draw the comment on the restating decision's words"
    ).toEqual([[onWords.id, words]]);
  } finally {
    view.unmount();
  }
});
