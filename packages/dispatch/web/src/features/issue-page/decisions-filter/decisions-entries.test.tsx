import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { api } from "../../../api/client";
import { conversationTreeKey } from "../../../api/queries";
import type { Actor, Comment } from "../../../api/types";
import { presetPath } from "../issue-address";
import type { TreeNode } from "../issue-tree";
import { DECISIONS_WORDS } from "../page-words";
import { filterSignals, presetView } from "../presets";
import { inDocument } from "../slots/stage-fixtures";
import { pagedAnswer, treeNode } from "../tree-read/tree-fixtures";
import {
  outlineQuery,
  type TreeAnswer,
  useDecisionsView,
  viewKey,
  viewQuery,
} from "../tree-read/tree-read";
import { useDecisionsEntries, useDecisionsTree } from "./decisions-entries";
import {
  AGENT,
  answeredAsk,
  changeComment,
  closing,
  decision,
  decisionNode,
  issueDetails,
  issueTree,
  KEY,
  node,
  openAsk,
  READER,
  READER_ACTOR,
  SPEC_DOCUMENT,
  SPEC_SLUG,
  shaped,
  specRef,
  statement,
  treeAnswer,
} from "./decisions-fixtures";
import type { DecisionsAsk, DecisionsParts } from "./decisions-parts";
import { entryStanding } from "./entry-standing";

/**
 * The seam between the page's reads and the Decisions view.
 * Each case holds the decisions view on the tree's root,
 * as `useIssueTree.ts` sets it there (`TreeNode.views`), and puts the tree read and the asks in
 * the cache under the keys the page's tree reads them by, as the cache holds them once the tree
 * is drawn, and no query reads again on mount, so any request the seam made would be one of its
 * own. The tree read's answer holds the comments, each Change under its decision with its
 * author's marks. The view is the one the slot passes: the tree as the Decisions view leaves it
 * (`presetView`).
 */

const BOB: Actor = { id: "bob", kind: "user" };

/** A client that holds the page's reads as the cache holds them once the tree is drawn: the
 *  tree read's answer `tree` and `asks`, neither of which is read again on mount. */
function heldClient(asks: readonly DecisionsAsk[], tree: TreeAnswer): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(conversationTreeKey(KEY), tree);
  client.setQueryData(["asks", KEY, "thread"], asks);
  return client;
}

/** A hook wrapper over `client` at the Decisions view's own address, where the page draws the
 *  view's entries: the seam reads the view the address draws, as every reader of the decisions
 *  view does (`useDecisionsView`). */
function decisionsAddress(client: QueryClient) {
  return function DecisionsAddress({ children }: { children: ReactNode }): ReactNode {
    return (
      <MemoryRouter initialEntries={[presetPath(KEY, "decisions")]}>
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      </MemoryRouter>
    );
  };
}

/** The seam mounted over `client` for the issue whose tree holds `children`, with `decisions`
 *  the decisions view held on its root (undefined for a view that holds no answer), until the
 *  case unmounts it. */
async function mountOver(
  client: QueryClient,
  children: readonly TreeNode[],
  decisions: TreeAnswer | undefined
) {
  const issue = issueDetails();
  const whole = issueTree(children);
  const root: TreeNode = decisions === undefined ? whole : { ...whole, views: { decisions } };
  const view = presetView(root, "decisions", "").root;
  const hook = renderHook(() => useDecisionsEntries({ issue, reader: READER, root, view }), {
    wrapper: decisionsAddress(client),
  });
  // A read the mount started has had its turn before the parts are read.
  await act(() => Promise.resolve());
  return hook;
}

/**
 * The seam's parts over `client` as the view's slot hands it its tree (`slots/view-decisions.tsx`),
 * for a case that reads a decision past Conversation's first page, which only that tree holds:
 * the tree the decisions view read builds (`useDecisionsTree`) from `decisions`, held on the page
 * tree's root and in `client`, and from the reads `client` holds, Conversation's among them, with
 * the view as the slot leaves it (`presetView`); and the Decisions control's signal over that
 * tree (`filterSignals`).
 */
async function slotOver(client: QueryClient, decisions: TreeAnswer) {
  const issue = issueDetails();
  const page: TreeNode = { ...issueTree([]), views: { decisions } };
  client.setQueryData(viewKey(KEY, "decisions"), decisions);
  const hook = renderHook(
    () => {
      const root = useDecisionsTree(issue, page, READER);
      const view = useMemo(() => presetView(root, "decisions", "").root, [root]);
      const { parts } = useDecisionsEntries({ issue, reader: READER, root, view });
      return { parts, signal: filterSignals(root, "").decisions };
    },
    { wrapper: decisionsAddress(client) }
  );
  // A read the mount started has had its turn before the parts are read.
  await act(() => Promise.resolve());
  const drawn = hook.result.current;
  hook.unmount();
  return drawn;
}

/** Statement `s1` of the spec as Conversation's read answers it: the statement a decision's
 *  `shaped` tie names, which the decisions view read leaves to that read. */
const S1 = treeNode("s1", "requirement", KEY, inDocument(SPEC_DOCUMENT));

/** The decision members of a decision block tied to `S1`. */
const TIED = { decision: { links: [shaped(specRef("s1"))] } };

/** The same decision as the decisions view read answers it, a node its query took. */
const VIEW_TIED = { ...TIED, role: "query" } as const;

/** The seam's parts over `client` for the issue whose tree holds `children`, with `decisions`
 *  the decisions view held on its root (undefined for a view that holds no answer), read once. */
async function partsOver(
  client: QueryClient,
  children: readonly TreeNode[],
  decisions: TreeAnswer | undefined
): Promise<DecisionsParts> {
  const hook = await mountOver(client, children, decisions);
  const { parts } = hook.result.current;
  hook.unmount();
  return parts;
}

/** The seam's parts for the issue whose tree holds `children`, with `decisions` the decisions
 *  view held on its root (undefined for a view that has not answered), and `asks` and the tree
 *  read's answer `tree` as the cache holds them. */
function entriesOf(
  children: readonly TreeNode[],
  decisions: TreeAnswer | undefined,
  asks: readonly DecisionsAsk[] = [],
  tree: TreeAnswer = treeAnswer([])
): Promise<DecisionsParts> {
  return partsOver(heldClient(asks, tree), children, decisions);
}

/** The node ids of each part, in the order the part draws them. */
function idsOf(parts: DecisionsParts): Record<keyof DecisionsParts, string[]> {
  const ids = (entries: readonly { readonly node: TreeNode }[]) =>
    entries.map((entry) => entry.node.id);
  return {
    agents: ids(parts.agents),
    named: ids(parts.named),
    open_questions: ids(parts.open_questions),
    others: ids(parts.others),
    review: ids(parts.review),
    yours: ids(parts.yours),
  };
}

const D = (block: string) => decisionNode(block).id;

/** Two decisions that wait open with one paragraph, the reader's Change was asked on the second,
 *  and answers of the reader, of Bob and of one moment stand around them: the reads of the
 *  page's tree for the two cases below, with the Change's anchor quoting `quote`. */
async function seamOf(quote: string) {
  const tie = "2026-11-11T09:00:00Z";
  const opening = "The rail stays 56 px wide.";
  const reviewWords = `${opening}\n\nThe outline needs the room.`;
  const askedWords = `${opening}\n\nA phone draws it over the body.`;
  const children = [
    statement("s1", {
      children: [answeredAsk("a-tie-bob", { at: tie, by: BOB, text: "Bob's, at that moment." })],
    }),
    openAsk("q-open"),
    answeredAsk("a-early", { at: "2026-11-12T09:00:00Z", text: "My first." }),
    answeredAsk("a-bob", { at: "2026-11-13T09:00:00Z", by: BOB, text: "Bob's." }),
    answeredAsk("a-late", { at: "2026-11-14T09:00:00Z", text: "My second." }),
    answeredAsk("a-tie-mine", { at: tie, text: "Mine, at that moment." }),
    decisionNode("review", { text: reviewWords }),
    decisionNode("asked", { text: askedWords }),
    decisionNode("minor"),
  ];
  const made = [
    decision("review", {
      text: reviewWords,
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
    decision("asked", {
      text: askedWords,
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
    decision("minor"),
  ];
  // The reader's Change on the second decision, made after both last changed, anchored on the
  // spec with its anchor quoting `quote`. The decisions view and the tree read each hang it under
  // the second decision, as the server's view carries each comment on the text of a node it
  // returns.
  const change: Comment = {
    ...commentDeliveryFields(),
    anchor: {
      artifact_id: SPEC_DOCUMENT.id,
      block_id: null,
      mark_id: "mark-asked",
      orphaned: false,
      quote,
      version: 1,
    },
    ask_id: null,
    author: READER_ACTOR,
    body: "Draw it under the body on a phone.",
    created_at: "2026-11-15T09:00:00Z",
    edited_at: null,
    id: "change-asked",
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
  const asks = [
    closing("a-tie-bob", { actor: BOB, at: tie, kind: "answered" }),
    closing("a-early", { at: "2026-11-12T09:00:00Z", kind: "answered" }),
    closing("a-bob", { actor: BOB, at: "2026-11-13T09:00:00Z", kind: "answered" }),
    closing("a-late", { at: "2026-11-14T09:00:00Z", kind: "answered" }),
    closing("a-tie-mine", { at: tie, kind: "answered" }),
  ];
  const read = treeAnswer([
    ...made,
    changeComment(change.id, "asked", change.created_at, change.body, change.author),
  ]);
  const parts = await entriesOf(children, read, asks, read);
  return { change, parts };
}

// The seam gives the view the model's parts from the reads the page's tree already made and the
// decisions view the page holds. Bob's answers stand in a part of their own, what others decided,
// newest first, and not among the reader's decisions (decision 502). The decisions view hangs the
// reader's Change under the second decision, so the second leaves the review list for her
// decisions and the first still waits.
test("the seam gives the model's parts from today's reads and makes no request", async () => {
  const getIssueDecisions = spyOn(api, "getIssueDecisions");
  const getIssueTree = spyOn(api, "getIssueTree");
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads");
  const listComments = spyOn(api, "listComments");
  // Any request at all, past the reads the page's tree makes.
  const request = spyOn(globalThis, "fetch");
  try {
    const { change, parts } = await seamOf("A phone draws it over the body.");
    const ids = idsOf(parts);
    expect(ids.yours).toEqual([D("asked"), "a-late", "a-early", "a-tie-mine"]);
    expect(ids.others).toEqual(["a-bob", "a-tie-bob"]);
    expect(ids.open_questions).toEqual(["q-open"]);
    expect(ids.review).toEqual([D("review")]);
    expect(ids.agents).toEqual([D("minor")]);
    expect(ids.named).toEqual([]);
    // The second decision carries the reader's Change, and the first carries none.
    expect(parts.yours[0]).toMatchObject({
      change: { id: change.id },
      kind: "decision",
      standing: "change_requested",
    });
    expect(parts.review[0]).not.toHaveProperty("change");

    expect(getIssueDecisions).not.toHaveBeenCalled();
    expect(getIssueTree).not.toHaveBeenCalled();
    expect(listIssueAskThreads).not.toHaveBeenCalled();
    expect(listComments).not.toHaveBeenCalled();
    expect(request).not.toHaveBeenCalled();
  } finally {
    getIssueDecisions.mockRestore();
    getIssueTree.mockRestore();
    listIssueAskThreads.mockRestore();
    listComments.mockRestore();
    request.mockRestore();
  }
});

// A Change stands under the decision it was asked on, though another decision holds its quote
// too: here the Change quotes the paragraph both decisions open with, as the page's Change
// quotes a decision's first paragraph. The Decisions view read hangs the Change under the
// decision it was asked on, so no quote is matched, and the first decision, whose words hold the
// same quote, carries none.
test("a Change stands under the decision it was asked on, where another decision holds its quote too", async () => {
  const { change, parts } = await seamOf("The rail stays 56 px wide.");
  expect(idsOf(parts).yours[0]).toBe(D("asked"));
  expect(parts.yours[0]).toMatchObject({ change: { id: change.id }, kind: "decision" });
  expect(parts.review[0]).not.toHaveProperty("change");
});

// R3 of dispatch://INFRA-1523: a Change its author deleted asks nothing any more. The decisions
// view still hangs it under its decision, with no body, and marks the delete on its node, as a
// delete's correction writes it into the answer the cache holds (`api/corrected-post.ts`): the
// decision is back on the review list and carries no Change. The same Change not deleted moves
// the decision to the reader's own.
test("a Change its author deleted asks nothing, and its decision waits for review again", async () => {
  const children = [statement("s1"), decisionNode("asked")];
  const made = [decision("asked", { decision: { links: [shaped(specRef("s1"))] } })];
  const at = "2026-11-15T09:00:00Z";
  const asked = changeComment("change-asked", "asked", at, "");
  const tree = treeAnswer([...made, asked]);
  const deleted = { ...asked, deleted_at: "2026-11-15T10:00:00Z", deleted_by: READER_ACTOR };
  const goneTree = treeAnswer([...made, deleted]);

  const standing = await entriesOf(children, tree, [], tree);
  expect([idsOf(standing).review, idsOf(standing).yours]).toEqual([[], [D("asked")]]);
  const gone = await entriesOf(children, goneTree, [], goneTree);
  expect([idsOf(gone).review, idsOf(gone).yours]).toEqual([[D("asked")], []]);
  expect(gone.review[0]).not.toHaveProperty("change");
});

// With the tree read answering a post's marks on its node (`edited_at`, `deleted_at`,
// `deleted_by`): a decision's Change says when its author last edited it, read from the node the
// read hangs under the decision.
test("a Change the seam takes from the tree read carries when its author last edited it", async () => {
  const editedAt = "2026-11-15T10:00:00Z";
  const children = [statement("s1"), decisionNode("asked")];
  const made = [decision("asked", { decision: { links: [shaped(specRef("s1"))] } })];
  const asked = {
    ...changeComment("change-asked", "asked", "2026-11-15T09:00:00Z", "Name the read."),
    edited_at: editedAt,
  };
  const tree = treeAnswer([...made, asked]);
  const parts = await entriesOf(children, tree, [], tree);
  expect(
    parts.yours.map((one) => (one.kind === "decision" ? one.change?.editedAt : undefined))
  ).toEqual([editedAt]);
});

// A reading of an answered question is a person's write-up of that answer, and no entry of its
// own: the question it restates is the person's decision. The tree places the reading under the
// answered ask it reads (`tree-read/tree-nodes.ts`'s `draftReading`), so the seam tells it a
// write-up by where it stands, and which question it restates, and no part draws a row for it.
// An agent's decision stays.
test("a reading under an answered question is a write-up, which no part draws and the seam tells, and an agent's decision stays", async () => {
  const writeUp = node({
    author: AGENT,
    id: `${SPEC_SLUG}#b-writeup`,
    reference: specRef("writeup"),
    text: "The reader keeps it as it is.",
    type: "reading",
  });
  const at = "2026-11-12T09:00:00Z";
  const made = [decision("minor")];
  const hook = await mountOver(
    heldClient([closing("a-answered", { at, kind: "answered" })], treeAnswer(made)),
    [
      answeredAsk("a-answered", { at, text: "Mine." }, { children: [writeUp], held: 1 }),
      decisionNode("minor"),
    ],
    treeAnswer(made)
  );
  const { parts, writeUps } = hook.result.current;
  hook.unmount();
  expect(idsOf(parts)).toEqual({
    agents: [D("minor")],
    named: [],
    open_questions: [],
    others: [],
    review: [],
    yours: ["a-answered"],
  });
  expect(writeUps).toEqual(new Map([[writeUp.id, new Set(["a-answered"])]]));
});

// R8: each read the seam watches takes the page's freshness: events refresh it, never its age.
// The stage mounts the view again on every press of Decisions, so under the app's own defaults,
// whose `staleTime` is 30 s (`main.tsx`), a press more than 30 s after the page loaded would
// otherwise read both again. Here the two answers - the tree read, which the decisions and their
// Changes come from, and the asks - were given 31 s ago; the seam reads no document's blocks.
test("answers older than the app's staleTime are not read again as the seam mounts", async () => {
  const made = [decision("minor")];
  const names = ["getIssueTree", "listIssueAskThreads"] as const;
  // Each of the two reads, held, so a read the mount started reaches no server.
  const reads = names.map((read) =>
    spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise)
  );
  // Any request at all, past those two.
  const request = spyOn(globalThis, "fetch");
  try {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
    });
    const given = { updatedAt: Date.now() - 31_000 };
    client.setQueryData(conversationTreeKey(KEY), treeAnswer(made), given);
    client.setQueryData(["asks", KEY, "thread"], [], given);
    const hook = await mountOver(
      client,
      [statement("s1"), openAsk("q-open"), decisionNode("minor")],
      treeAnswer(made)
    );
    expect(hook.result.current.readsHeld).toBe(true);
    expect(
      Object.fromEntries(names.map((read, index) => [read, reads[index]?.mock.calls]))
    ).toEqual({
      getIssueTree: [],
      listIssueAskThreads: [],
    });
    expect(request).not.toHaveBeenCalled();
    hook.unmount();
  } finally {
    for (const read of reads) read.mockRestore();
    request.mockRestore();
  }
});

// Every reader of the decisions view takes it through `useDecisionsView`, whose read is tracked
// only as far as each reader reads it (`useLoadView`'s getters): a reader of the answer alone -
// the page's tree, the header's count, each Decisions row's Keep and Change - is drawn again only
// as the answer or its refusal changes, not as a read of the view starts and ends. Here the view
// is read again, and its read answers what the view held, which TanStack's structural sharing
// keeps as the same answer.
test("a reader of the decisions view's answer draws once, and not again when the view is read again to the same answer", async () => {
  const held = treeAnswer([decision("d1")]);
  const read = spyOn(api, "getIssueTree").mockImplementation(async () =>
    treeAnswer([decision("d1")])
  );
  try {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    client.setQueryData(viewKey(KEY, "decisions"), held);
    let draws = 0;
    const hook = renderHook(
      () => {
        draws += 1;
        return useDecisionsView(KEY).answer;
      },
      { wrapper: decisionsAddress(client) }
    );
    await act(() => Promise.resolve());
    expect(hook.result.current, "the reader holds no answer of the decisions view").toBe(held);
    expect(draws, "the reader drew more than once as it mounted").toBe(1);
    await act(async () => {
      await client.fetchQuery({ ...viewQuery(KEY, "decisions"), staleTime: 0 });
    });
    expect(read, "the decisions view was not read again").toHaveBeenCalledTimes(1);
    expect(hook.result.current, "the read again changed the answer the reader holds").toBe(held);
    expect(draws, "the reader of the answer drew again as the view was read again").toBe(1);
    hook.unmount();
  } finally {
    read.mockRestore();
  }
});

// A decisions view that holds no decision gives no decision and no error, and the view draws
// the questions alone; so does a view that has not answered yet over a tree read that holds no
// decision node, which is the read the page's tree is built from. Such a read holds its answer
// all the same (`readsHeld`). While any one of the seam's reads has not answered, the questions
// are drawn all the same. The view knows what the issue holds once the tree read and the asks
// have answered, which say which questions and decisions there are; the decisions view only
// stands over the tree read's decisions, so the view knows while it is out. Either of the first
// two refused before it first answers is the seam's to name, with a retry of its own, so the body
// can say so without naming a read; one refused only on a later read keeps the answer it gave,
// and is none.
test("an empty decisions member gives the questions alone and no error", async () => {
  const children = [statement("s1"), openAsk("q-open")];
  for (const [read, decisions] of [
    ["an empty view", treeAnswer([])],
    ["no view yet", undefined],
  ] as const) {
    const drawn = entriesOf(children, decisions);
    // The hook throws nothing on this read: the promise of its answer settles with it.
    await expect(drawn).resolves.toBeDefined();
    expect({ parts: idsOf(await drawn), read }).toEqual({
      parts: {
        agents: [],
        named: [],
        open_questions: ["q-open"],
        others: [],
        review: [],
        yours: [],
      },
      read,
    });
  }

  // Each read in turn is left out of the cache, and a fetch of it would never settle; its answer
  // comes into the cache later, as a read that answers puts it there. `knownWhileOut` is whether
  // the view knows what the issue holds while that read is out. The seam watches each read and
  // sends none of its own, so the fetch is never started.
  const held = { asks: [], tree: treeAnswer([]) };
  const cold = [
    {
      answer: held.tree,
      key: conversationTreeKey(KEY),
      knownWhileOut: false,
      read: "getIssueTree",
    },
    {
      answer: held.asks,
      key: ["asks", KEY, "thread"],
      knownWhileOut: false,
      read: "listIssueAskThreads",
    },
    {
      answer: held.tree,
      key: viewKey(KEY, "decisions"),
      knownWhileOut: true,
      read: "getIssueTree",
    },
  ] as const;
  for (const { answer, key, knownWhileOut, read } of cold) {
    const started = spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise);
    try {
      const client = heldClient(held.asks, held.tree);
      client.removeQueries({ exact: true, queryKey: key });
      const hook = await mountOver(client, children, undefined);
      const reading = () => ({
        open_questions: idsOf(hook.result.current.parts).open_questions,
        out: key,
        readsHeld: hook.result.current.readsHeld,
      });
      expect(reading()).toEqual({ open_questions: ["q-open"], out: key, readsHeld: knownWhileOut });
      expect(started).not.toHaveBeenCalled();
      await act(async () => client.setQueryData(key, answer));
      expect(reading()).toEqual({ open_questions: ["q-open"], out: key, readsHeld: true });
      hook.unmount();
    } finally {
      started.mockRestore();
    }
  }

  // Each of the two reads the view's failed-read lines name is refused in turn before it first
  // answers, with the other read in the cache: the tree read, which the view's rows come from,
  // and the asks. The page's own read refuses it, since the seam sends none. The refused read
  // holds no answer, so the seam says it is unreadable and the view does not know what the issue
  // holds; its retry asks that read again, which is refused again, and asks the other read
  // nothing. The read stays refused while the retry runs, and once that run is refused too; no
  // entry moves, so the parts are the very object they were before the retry.
  const refused = [
    {
      key: conversationTreeKey(KEY),
      other: "listIssueAskThreads",
      read: "decisions",
      refuse: (client: QueryClient) => client.prefetchQuery(outlineQuery(KEY, "conversation")),
      request: "getIssueTree",
    },
    {
      key: ["asks", KEY, "thread"],
      other: "getIssueTree",
      read: "asks",
      refuse: (client: QueryClient) =>
        client.prefetchQuery({
          queryFn: () => api.listIssueAskThreads(KEY, "all"),
          queryKey: ["asks", KEY, "thread"],
        }),
      request: "listIssueAskThreads",
    },
  ] as const;
  for (const { key, other, read, refuse, request } of refused) {
    const asked = spyOn(api, request).mockRejectedValue(new Error("refused"));
    const notAsked = spyOn(api, other);
    try {
      const client = heldClient(held.asks, held.tree);
      client.removeQueries({ exact: true, queryKey: key });
      await refuse(client);
      const hook = await mountOver(client, children, undefined);
      const reading = () => ({
        read,
        readsHeld: hook.result.current.readsHeld,
        unreadable: hook.result.current.unreadable,
      });
      const unreadable = { read, readsHeld: false, unreadable: [read] };
      await waitFor(() => expect(reading()).toEqual(unreadable));
      expect(asked).toHaveBeenCalledTimes(1);
      const before = hook.result.current.parts;
      await act(async () => hook.result.current.retry(read));
      await waitFor(() => expect(asked).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(reading()).toEqual(unreadable));
      expect(hook.result.current.parts).toBe(before);
      expect(notAsked).not.toHaveBeenCalled();
      hook.unmount();
    } finally {
      asked.mockRestore();
      notAsked.mockRestore();
    }
  }

  // Each of the two reads has answered, and a refetch of it then fails. While the refetch runs
  // the seam says that read is being read again, and the parts are the very object they were
  // before it started, so nothing the body works out from them is worked out again; once it has
  // failed, the read still holds the answer it gave, which the view draws from, so it is not
  // unreadable, draws no line, and the reads still hold.
  const refetched = [
    { queryKey: conversationTreeKey(KEY), read: "decisions", request: "getIssueTree" },
    { queryKey: ["asks", KEY, "thread"], read: "asks", request: "listIssueAskThreads" },
  ] as const;
  for (const { queryKey, read, request } of refetched) {
    const answer = Promise.withResolvers<never>();
    const asked = spyOn(api, request).mockReturnValue(answer.promise);
    try {
      const client = heldClient(held.asks, held.tree);
      const hook = await mountOver(client, children, treeAnswer([]));
      const reading = () => ({
        read,
        readsHeld: hook.result.current.readsHeld,
        retrying: hook.result.current.retrying,
        unreadable: hook.result.current.unreadable,
      });
      const holds = { read, readsHeld: true, retrying: [], unreadable: [] };
      expect(reading()).toEqual(holds);
      const before = hook.result.current.parts;
      await act(async () => hook.result.current.retry(read));
      await waitFor(() => expect(reading()).toEqual({ ...holds, retrying: [read] }));
      expect(hook.result.current.parts).toBe(before);
      await act(async () => {
        answer.reject(new Error("refused"));
        await answer.promise.catch(() => undefined);
      });
      await waitFor(() => expect(hook.result.current.retrying).toEqual([]));
      expect(client.getQueryState(queryKey)?.status).toBe("error");
      expect(reading()).toEqual(holds);
      expect(asked).toHaveBeenCalledTimes(1);
      hook.unmount();
    } finally {
      asked.mockRestore();
    }
  }
});

// Until the decisions view answers - its first read, made the first time the filter is on in a
// load, or a read of it made again after a change of the issue - and wherever that read fails,
// the decisions are the decision nodes of the tree read the page holds: a decision that waits and
// is tied to a statement stands in the review list with its state, and one tied to nothing in the
// folded foot, as they stand once the view answers, never all in the foot with no state.
test("a decision stands in its part with its state from the tree read until the view answers, and where it fails", async () => {
  const children = [statement("s1"), decisionNode("review"), decisionNode("minor")];
  const made = [
    decision("review", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
    decision("minor"),
  ];
  const tree = treeAnswer(made);
  const failing = heldClient([], tree);
  await failing.prefetchQuery({
    ...viewQuery(KEY, "decisions"),
    queryFn: () => Promise.reject(new Error("offline")),
  });
  expect(failing.getQueryState(viewKey(KEY, "decisions"))?.status).toBe("error");
  const readings = [
    ["the view answered", await entriesOf(children, treeAnswer(made), [], tree)],
    ["the view not answered yet", await entriesOf(children, undefined, [], tree)],
    ["the view's read failed", await partsOver(failing, children, undefined)],
  ] as const;

  for (const [read, parts] of readings) {
    expect({ parts: idsOf(parts), read }).toEqual({
      parts: {
        agents: [D("minor")],
        named: [],
        open_questions: [],
        others: [],
        review: [D("review")],
        yours: [],
      },
      read,
    });
    expect({ entry: parts.review[0], read }).toMatchObject({
      entry: { kind: "decision", standing: "waiting_review" },
      read,
    });
  }
});

// The Decisions view's parts are made from the Decisions view read's nodes (spec R2). Conversation's
// tree read is paged: on a large issue its first page ends before the last decisions of the spec
// (measured on a preview of 776d9d85: INFRA-1196's first page held 467 nodes with 462 remaining,
// and no node of a person's answered question and the agent's decision under it, which the
// decisions view held). A decision the view read holds stands in its part with its state, whether
// or not the tree the page has read so far holds its node. The seam is handed the tree that read
// builds, as the slot hands it (`slotOver`).
test("a decision the view read holds stands in its part where the tree read has not reached it yet", async () => {
  const view = treeAnswer([decision("first", VIEW_TIED), decision("later", VIEW_TIED)]);
  const { parts } = await slotOver(heldClient([], treeAnswer([S1, decision("first", TIED)])), view);

  expect(
    [...idsOf(parts).review].sort(),
    "the review list holds each waiting decision the Decisions view read holds"
  ).toEqual([D("first"), D("later")].sort());
});

// R3 (dispatch://INFRA-4097/spec#b-r-change): a Change is the comment the tree read hangs under
// its decision node, and the Decisions view read carries each comment on the text of a node it
// returns, as context. Conversation's read is paged, and its first page here ends before the
// later decision, which the view read holds with the reader's Change under it. The Change is read
// from the view read, so that decision stands with the reader's own decisions, reads "Changed by"
// with her name and is not on the review list, as a decision on the first page with its Change
// does. The seam is handed the tree that read builds, as the slot hands it (`slotOver`).
test("a waiting decision past Conversation's first page with the reader's Change in the view read stands with her decisions", async () => {
  const asked = {
    ...changeComment("change-later", "later", "2026-11-15T09:00:00Z", "Name the read."),
    role: "context" as const,
  };
  const view = treeAnswer([decision("first", VIEW_TIED), decision("later", VIEW_TIED), asked]);
  const firstPage = pagedAnswer(treeAnswer([S1, decision("first", TIED)]), "next-page", 30);
  const { parts } = await slotOver(heldClient([], firstPage), view);

  expect(
    { review: idsOf(parts).review, yours: idsOf(parts).yours },
    "the later decision, its Change in the view read, does not stand with the reader's decisions"
  ).toEqual({ review: [D("first")], yours: [D("later")] });
  const later = parts.yours[0];
  expect(later).toMatchObject({
    change: { by: READER_ACTOR, id: asked.id },
    kind: "decision",
    standing: "change_requested",
  });
  expect(
    later === undefined ? undefined : entryStanding(later, (actor) => actor.id)?.words,
    "the later decision does not read Changed by with the reader's name"
  ).toBe(`${DECISIONS_WORDS.changed_by} ${READER}`);
});

// R2, R15: the Decisions view's parts and its count are the Decisions view read's. Conversation's
// pages held so far hold two waiting decisions tied to a statement; the Decisions view read's
// answer, a first page that names a next one, holds the first alone. The tree that read builds
// holds the later decision as Conversation's context alone (`useIssueTree.ts`'
// `viewWithContext`), so the view counts the first alone, with "+", its review list holds the
// first, and its foot holds nothing: the later decision stands in no part until a page of the
// Decisions view read holds it.
test("a waiting decision Conversation's pages hold and the Decisions view read's page does not is no entry, count or part of the view", async () => {
  const conversation = treeAnswer([S1, decision("first", TIED), decision("later", TIED)]);
  const decisions = pagedAnswer(treeAnswer([decision("first", VIEW_TIED)]), "next-page", 30);
  const { parts, signal } = await slotOver(heldClient([], conversation), decisions);

  expect(
    `${signal.count}${signal.more === true ? "+" : ""}`,
    "the Decisions count reads a decision the Decisions view read's page does not hold"
  ).toBe("1+");
  expect({ agents: idsOf(parts).agents, review: idsOf(parts).review }).toEqual({
    agents: [],
    review: [D("first")],
  });
});

// The seam's reads are refreshed by the events that change them alone, as every read of the page
// is: mounted over reads older than the app's 30 s `staleTime` (`main.tsx`), as Decisions pressed
// a minute after the load mounts it, the seam sends no request of its own.
test("the seam mounted over reads older than the app's stale time sends no request", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { refetchOnWindowFocus: true, retry: false, staleTime: 30_000 } },
  });
  const minuteAgo = Date.now() - 60_000;
  const made = [
    decision("review", {
      decision: {
        links: [shaped(specRef("s1"))],
      },
    }),
  ];
  client.setQueryData(conversationTreeKey(KEY), treeAnswer(made), { updatedAt: minuteAgo });
  client.setQueryData(["asks", KEY, "thread"], [], { updatedAt: minuteAgo });
  const never = () => new Promise<never>(() => {});
  const getIssueTree = spyOn(api, "getIssueTree").mockImplementation(never);
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads").mockImplementation(never);
  const listComments = spyOn(api, "listComments").mockImplementation(never);
  const request = spyOn(globalThis, "fetch");
  try {
    const parts = await partsOver(
      client,
      [statement("s1"), decisionNode("review")],
      treeAnswer(made)
    );
    expect(idsOf(parts).review).toEqual([D("review")]);
    expect(getIssueTree).not.toHaveBeenCalled();
    expect(listIssueAskThreads).not.toHaveBeenCalled();
    expect(listComments).not.toHaveBeenCalled();
    expect(request).not.toHaveBeenCalled();
  } finally {
    getIssueTree.mockRestore();
    listIssueAskThreads.mockRestore();
    listComments.mockRestore();
    request.mockRestore();
  }
});
