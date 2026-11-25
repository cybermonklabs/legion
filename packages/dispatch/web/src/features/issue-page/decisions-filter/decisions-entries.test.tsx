import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { api } from "../../../api/client";
import { conversationTreeKey } from "../../../api/queries";
import type { Actor, Comment } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { presetView } from "../presets";
import { outlineQuery, type TreeAnswer, viewKey, viewQuery } from "../tree-read/tree-read";
import { useDecisionsEntries } from "./decisions-entries";
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

/**
 * The seam between the page's reads and the Decisions view.
 * Each case holds the decisions view on the tree's root,
 * as `useIssueTree.ts` sets it there (`TreeNode.views`), and puts the tree read, the asks and the
 * comments in the cache under the keys the page's tree reads them by, as the cache holds them
 * once the tree is drawn, and no query reads again on mount, so any request the seam made would
 * be one of its own. The view is the one the slot passes: the tree as the Decisions view leaves
 * it (`presetView`).
 */

const BOB: Actor = { id: "bob", kind: "user" };

/** A client that holds the page's reads as the cache holds them once the tree is drawn: the
 *  tree read's answer `tree`, `asks` and `comments`, none of which is read again on mount. */
function heldClient(
  asks: readonly DecisionsAsk[],
  comments: readonly Comment[],
  tree: TreeAnswer
): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(conversationTreeKey(KEY), tree);
  client.setQueryData(["asks", KEY, "thread"], asks);
  client.setQueryData(["comments", KEY], comments);
  return client;
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
  const wrapper = ({ children: inside }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{inside}</QueryClientProvider>
  );
  const hook = renderHook(() => useDecisionsEntries({ issue, reader: READER, root, view }), {
    wrapper,
  });
  // A read the mount started has had its turn before the parts are read.
  await act(() => Promise.resolve());
  return hook;
}

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
 *  view held on its root (undefined for a view that has not answered), and `asks`, `comments`
 *  and the tree read's answer `tree` as the cache holds them. */
function entriesOf(
  children: readonly TreeNode[],
  decisions: TreeAnswer | undefined,
  asks: readonly DecisionsAsk[] = [],
  comments: readonly Comment[] = [],
  tree: TreeAnswer = treeAnswer([])
): Promise<DecisionsParts> {
  return partsOver(heldClient(asks, comments, tree), children, decisions);
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
  // spec with its anchor quoting `quote`. The comments read holds it, and the tree read hangs it
  // under the second decision.
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
  const parts = await entriesOf(
    children,
    treeAnswer(made),
    asks,
    [change],
    treeAnswer([
      ...made,
      changeComment(change.id, "asked", change.created_at, change.body, change.author),
    ])
  );
  return { change, parts };
}

// The seam gives the view's four parts from the reads the page's tree already made and the
// decisions view the page holds. Bob's answers stand under the reader's decisions, newest first
// among the reader's own. Two answers of one moment stand in the order their nodes first stand
// in the tree, read depth first: Bob's is under the statement at the tree's top and the reader's
// is the issue's last ask, so Bob's stands first, which neither the reader's entries first nor a
// walk level by level gives. The tree read hangs the reader's Change under the second decision,
// so the second leaves the review list for her decisions and the first still waits.
test("the seam gives four parts from today's reads and makes no request", async () => {
  const getIssueDecisions = spyOn(api, "getIssueDecisions");
  const getIssueTree = spyOn(api, "getIssueTree");
  const listIssueAskThreads = spyOn(api, "listIssueAskThreads");
  const listComments = spyOn(api, "listComments");
  // Any request at all, past the reads the page's tree makes.
  const request = spyOn(globalThis, "fetch");
  try {
    const { change, parts } = await seamOf("A phone draws it over the body.");
    const ids = idsOf(parts);
    expect(ids.yours).toEqual([
      D("asked"),
      "a-late",
      "a-bob",
      "a-early",
      "a-tie-bob",
      "a-tie-mine",
    ]);
    expect(ids.others).toEqual([]);
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
// quotes a decision's first paragraph. The tree read hangs the Change under the decision it was
// asked on, so no quote is matched, and the first decision, whose words hold the same quote,
// carries none.
test("a Change stands under the decision it was asked on, where another decision holds its quote too", async () => {
  const { change, parts } = await seamOf("The rail stays 56 px wide.");
  expect(idsOf(parts).yours[0]).toBe(D("asked"));
  expect(parts.yours[0]).toMatchObject({ change: { id: change.id }, kind: "decision" });
  expect(parts.review[0]).not.toHaveProperty("change");
});

// R3 of dispatch://INFRA-1523: a Change its author deleted asks nothing any more. The tree read
// still hangs it under its decision, with no body and no mark of the delete, and the comments
// read says it was deleted: the decision is back on the review list and carries no Change. The
// same Change not deleted moves the decision to the reader's own.
test("a Change its author deleted asks nothing, and its decision waits for review again", async () => {
  const children = [statement("s1"), decisionNode("asked")];
  const made = [decision("asked", { decision: { links: [shaped(specRef("s1"))] } })];
  const at = "2026-11-15T09:00:00Z";
  const tree = treeAnswer([...made, changeComment("change-asked", "asked", at, "")]);
  const asked = { deleted_at: null, edited_at: null, id: "change-asked" } as Comment;
  const deleted = { ...asked, deleted_at: "2026-11-15T10:00:00Z", deleted_by: READER_ACTOR };

  const standing = await entriesOf(children, treeAnswer(made), [], [asked], tree);
  expect([idsOf(standing).review, idsOf(standing).yours]).toEqual([[], [D("asked")]]);
  const gone = await entriesOf(children, treeAnswer(made), [], [deleted], tree);
  expect([idsOf(gone).review, idsOf(gone).yours]).toEqual([[D("asked")], []]);
  expect(gone.review[0]).not.toHaveProperty("change");
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
    heldClient([closing("a-answered", { at, kind: "answered" })], [], treeAnswer(made)),
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
// otherwise read all three again. Here the three answers - the tree read, which the decisions
// come from, the asks and the comments - were given 31 s ago; the seam reads no document's
// blocks.
test("answers older than the app's staleTime are not read again as the seam mounts", async () => {
  const made = [decision("minor")];
  const names = ["getIssueTree", "listIssueAskThreads", "listComments"] as const;
  // Each of the three reads, held, so a read the mount started reaches no server.
  const reads = names.map((read) =>
    spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise)
  );
  // Any request at all, past those three.
  const request = spyOn(globalThis, "fetch");
  try {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
    });
    const given = { updatedAt: Date.now() - 31_000 };
    client.setQueryData(conversationTreeKey(KEY), treeAnswer(made), given);
    client.setQueryData(["asks", KEY, "thread"], [], given);
    client.setQueryData(["comments", KEY], [], given);
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
      listComments: [],
      listIssueAskThreads: [],
    });
    expect(request).not.toHaveBeenCalled();
    hook.unmount();
  } finally {
    for (const read of reads) read.mockRestore();
    request.mockRestore();
  }
});

// A decisions view that holds no decision gives no decision and no error, and the view draws
// the questions alone; so does a view that has not answered yet over a tree read that holds no
// decision node, which is the read the page's tree is built from. Such a read holds its answer
// all the same (`readsHeld`). While any one of the seam's reads has not answered, the questions
// are drawn all the same. The view knows what the issue holds once the tree read and the asks
// have answered, which say which questions and decisions there are; the decisions view only
// stands over the tree read's decisions and the comments say nothing of an entry, so the view
// knows while those are out. Either of the first two refused before it first answers is the
// seam's to name, with a retry of its own, so the body can say so without naming a read; one
// refused only on a later read keeps the answer it gave, and is none.
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
  const held = { asks: [], comments: [], tree: treeAnswer([]) };
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
    { answer: held.comments, key: ["comments", KEY], knownWhileOut: true, read: "listComments" },
  ] as const;
  for (const { answer, key, knownWhileOut, read } of cold) {
    const started = spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise);
    try {
      const client = heldClient(held.asks, held.comments, held.tree);
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
      const client = heldClient(held.asks, held.comments, held.tree);
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
      const client = heldClient(held.asks, held.comments, held.tree);
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
  const failing = heldClient([], [], tree);
  await failing.prefetchQuery({
    ...viewQuery(KEY, "decisions"),
    queryFn: () => Promise.reject(new Error("offline")),
  });
  expect(failing.getQueryState(viewKey(KEY, "decisions"))?.status).toBe("error");
  const readings = [
    ["the view answered", await entriesOf(children, treeAnswer(made), [], [], tree)],
    ["the view not answered yet", await entriesOf(children, undefined, [], [], tree)],
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
  client.setQueryData(["comments", KEY], [], { updatedAt: minuteAgo });
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
