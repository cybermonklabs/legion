import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { api } from "../../../api/client";
import type { Actor, ArtifactBlock, Comment, IssueDecisions } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { presetView } from "../presets";
import { useDecisionsEntries } from "./decisions-entries";
import {
  AGENT,
  answeredAsk,
  closing,
  decision,
  decisionNode,
  decisionsRead,
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
} from "./decisions-fixtures";
import type { DecisionsAsk, DecisionsParts } from "./decisions-parts";

/**
 * The seam between today's reads and the Decisions view. Each case puts the three reads the seam
 * takes in the cache under the keys the page's tree reads them by, as the cache holds them once
 * the tree is drawn, and no query reads again on mount, so any request the seam made would be one
 * of its own. The seam reads no document's blocks: the spec's are put in the cache too only so
 * that the cold case can show that no read of them starts. The view is the one the slot passes:
 * the tree as the Decisions view leaves it (`presetView`).
 */

const BOB: Actor = { id: "bob", kind: "user" };

/** The seam mounted over the issue whose tree holds `children`, with `decisions`, `asks` and
 *  `comments` as the cache holds them, and the spec's `blocks` there too, which the seam never
 *  reads, until the case unmounts it. A read given as null is left out of the cache, as before it
 *  answers. By default no answer ever goes stale; `staleTime` gives the client the app's own
 *  default instead, and `answeredAt` the moment the cache's answers were given, now where it is
 *  left out. */
async function mountEntries(
  children: readonly TreeNode[],
  decisions: IssueDecisions | null,
  asks: readonly DecisionsAsk[] | null = [],
  comments: readonly Comment[] | null = [],
  blocks: readonly ArtifactBlock[] | null = [],
  {
    answeredAt,
    staleTime = Number.POSITIVE_INFINITY,
  }: { answeredAt?: number; staleTime?: number } = {}
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime } } });
  const given = { updatedAt: answeredAt };
  if (decisions !== null) client.setQueryData(["decisions", KEY], decisions, given);
  if (asks !== null) client.setQueryData(["asks", KEY, "thread"], asks, given);
  if (comments !== null) client.setQueryData(["comments", KEY], comments, given);
  // The seam reads no document's blocks; the spec's are put in the cache only so that the cold
  // case can show that no read of them starts.
  if (blocks !== null) {
    client.setQueryData(["artifact", SPEC_DOCUMENT.id, "blocks"], blocks, given);
  }
  const issue = issueDetails();
  const root = issueTree(children);
  const view = presetView(root, "decisions", "").root;
  const wrapper = ({ children: inside }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{inside}</QueryClientProvider>
  );
  const hook = renderHook(() => useDecisionsEntries({ issue, reader: READER, root, view }), {
    wrapper,
  });
  // A read the mount started has had its turn before the parts are read.
  await act(() => Promise.resolve());
  return { client, hook };
}

/** The seam's answer over the reads `mountEntries` is handed, read once. */
async function entriesOf(
  ...reads: Parameters<typeof mountEntries>
): Promise<ReturnType<typeof useDecisionsEntries>> {
  const { hook } = await mountEntries(...reads);
  const entries = hook.result.current;
  hook.unmount();
  return entries;
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
    decision("review", { links: [shaped(specRef("s1"))], text: reviewWords }),
    decision("asked", { links: [shaped(specRef("s1"))], text: askedWords }),
    decision("minor"),
  ];
  // The reader's Change on the second decision, made after both last changed, anchored in the
  // spec on the second decision's own block, the lowest block holding `quote` that carries an
  // id. The seam reads no document's blocks, so the Change is placed by its quote alone.
  const change: Comment = {
    ...commentDeliveryFields(),
    anchor: {
      artifact_id: SPEC_DOCUMENT.id,
      block_id: "asked",
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
  const { parts } = await entriesOf(children, decisionsRead(made), asks, [change]);
  return { change, parts };
}

// R8: the seam gives the view's four parts from the reads the page's tree already made. Bob's
// answers stand under the reader's decisions, newest first among the reader's own. Two answers
// of one moment stand in the order their nodes first stand in the tree, read depth first: Bob's
// is under the statement at the tree's top and the reader's is the issue's last ask, so Bob's
// stands first, which neither the reader's entries first nor a walk level by level gives. The
// reader's Change quotes words only the second decision holds, so the second leaves the review
// list for her decisions and the first still waits.
test("the seam gives four parts from today's reads and makes no request", async () => {
  const getIssueDecisions = spyOn(api, "getIssueDecisions");
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
    expect(listIssueAskThreads).not.toHaveBeenCalled();
    expect(listComments).not.toHaveBeenCalled();
    expect(request).not.toHaveBeenCalled();
  } finally {
    getIssueDecisions.mockRestore();
    listIssueAskThreads.mockRestore();
    listComments.mockRestore();
    request.mockRestore();
  }
});

// A Change stands under the decision it was asked on, though another decision holds its quote
// too: here the Change quotes the paragraph both decisions open with, as the page's Change
// quotes a decision's first paragraph. This case is marked to fail. The reads the seam takes
// name no decision for a Change - only the tree read's decision nodes do - so the Change stands
// under the first decision whose words hold its quote until the Decisions view draws its
// decisions from the tree read's nodes. The mark makes the case fail the day it passes, so that
// change removes it.
test.failing("a Change stands under the decision it was asked on, where another decision holds its quote too", async () => {
  const { change, parts } = await seamOf("The rail stays 56 px wide.");
  expect(idsOf(parts).yours[0]).toBe(D("asked"));
  expect(parts.yours[0]).toMatchObject({ change: { id: change.id }, kind: "decision" });
  expect(parts.review[0]).not.toHaveProperty("change");
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
  const { parts, writeUps } = await entriesOf(
    [
      answeredAsk("a-answered", { at, text: "Mine." }, { children: [writeUp], held: 1 }),
      decisionNode("minor"),
    ],
    decisionsRead([decision("minor")]),
    [closing("a-answered", { at, kind: "answered" })]
  );
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

// R8: each read the seam makes takes the page's freshness (`EVENT_FRESH`): events refresh it,
// never its age. The stage mounts the view again on every press of Decisions, so under the app's
// own defaults, whose `staleTime` is 30 s (`main.tsx`), a press more than 30 s after the page
// loaded would otherwise read all three again. Here the three answers were given 31 s ago; the
// seam reads no document's blocks.
test("answers older than the app's staleTime are not read again as the seam mounts", async () => {
  const made = [decision("minor")];
  const names = ["getIssueDecisions", "listIssueAskThreads", "listComments"] as const;
  // Each of the three reads, held, so a read the mount started reaches no server.
  const reads = names.map((read) =>
    spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise)
  );
  // Any request at all, past those three.
  const request = spyOn(globalThis, "fetch");
  try {
    const { hook } = await mountEntries(
      [statement("s1"), openAsk("q-open"), decisionNode("minor")],
      decisionsRead(made),
      [],
      [],
      null,
      { answeredAt: Date.now() - 31_000, staleTime: 30_000 }
    );
    expect(hook.result.current.readsHeld).toBe(true);
    expect(
      Object.fromEntries(names.map((read, index) => [read, reads[index]?.mock.calls]))
    ).toEqual({
      getIssueDecisions: [],
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

// R8: a decisions read that holds no decision, or none of its `entries` member, gives no
// decision and no error, and the view draws the questions alone. The tree is the one the page
// builds from that same read, which holds no decision node (`tree-drafts.ts`'s `addDecisions`).
// Such a read holds its answer all the same (`readsHeld`). While any one of the three reads has
// not answered, the questions are drawn all the same. The reads hold once the decisions read and
// the asks have answered, which say which questions and decisions there are; the comments only
// move a decision between parts, so the reads hold while they are out. The seam reads no
// document's blocks, so no read of the spec's starts while they are out of the
// cache, and the reads hold. Either of the first two refused before it first answers is the
// seam's to name, with a retry of its own, so the body can say so without naming a read; one
// refused only on a later read keeps the answer it gave, and is none.
test("an empty decisions member gives the questions alone and no error", async () => {
  const children = [statement("s1"), openAsk("q-open")];
  // A read with no `entries` member, which its type does not allow and an answer can still be.
  const { decisions: none, document, open_questions } = decisionsRead([]);
  const missing = { decisions: none, document, open_questions } as unknown as IssueDecisions;
  for (const [read, decisions] of [
    ["an empty member", decisionsRead([])],
    ["no member", missing],
  ] as const) {
    const drawn = entriesOf(children, decisions);
    // The hook throws nothing on this read: the promise of its answer settles with it.
    await expect(drawn).resolves.toBeDefined();
    const entries = await drawn;
    expect({ parts: idsOf(entries.parts), read, readsHeld: entries.readsHeld }).toEqual({
      parts: {
        agents: [],
        named: [],
        open_questions: ["q-open"],
        others: [],
        review: [],
        yours: [],
      },
      read,
      readsHeld: true,
    });
  }

  // Each read in turn is left out of the cache, and the fetch the mount starts for it never
  // settles; its answer comes into the cache later, as a read that answers puts it there.
  // `heldWhileOut` is whether the reads hold while that read is out, and `starts` how many
  // fetches of it the mount starts: no document plays the decisions part, since decisions are
  // nodes of the spec, so no blocks of one are read.
  const noDecision = decisionsRead([]);
  const oneDecision = decisionsRead([decision("minor")]);
  const cold = [
    {
      answer: noDecision,
      heldWhileOut: false,
      queryKey: ["decisions", KEY],
      mount: () => mountEntries(children, null),
      read: "getIssueDecisions",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: false,
      queryKey: ["asks", KEY, "thread"],
      mount: () => mountEntries(children, noDecision, null),
      read: "listIssueAskThreads",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: true,
      queryKey: ["comments", KEY],
      mount: () => mountEntries(children, noDecision, [], null),
      read: "listComments",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: true,
      queryKey: ["artifact", SPEC_DOCUMENT.id, "blocks"],
      mount: () => mountEntries(children, oneDecision, [], [], null),
      read: "getArtifactBlocks",
      starts: 0,
    },
  ] as const;
  for (const { answer, heldWhileOut, mount, queryKey, read, starts } of cold) {
    const started = spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise);
    try {
      const { client, hook } = await mount();
      const reading = () => ({
        open_questions: idsOf(hook.result.current.parts).open_questions,
        read,
        readsHeld: hook.result.current.readsHeld,
      });
      expect(reading()).toEqual({ open_questions: ["q-open"], read, readsHeld: heldWhileOut });
      expect(started).toHaveBeenCalledTimes(starts);
      await act(async () => client.setQueryData(queryKey, answer));
      expect(reading()).toEqual({ open_questions: ["q-open"], read, readsHeld: true });
      hook.unmount();
    } finally {
      started.mockRestore();
    }
  }

  // Each of the two reads that say which entries there are is refused in turn before it first
  // answers, with the other read in the cache. The refused read holds no answer, so the seam
  // says it is unreadable and the reads do not hold; its retry asks that read again, which is
  // refused again, and asks the other read nothing. The read stays refused while the retry runs,
  // and once that run is refused too; no entry moves, so the parts are the very object they were
  // before the retry.
  const refused = [
    {
      mount: () => mountEntries(children, null),
      other: "listIssueAskThreads",
      read: "issue-decisions",
      request: "getIssueDecisions",
    },
    {
      mount: () => mountEntries(children, noDecision, null),
      other: "getIssueDecisions",
      read: "asks",
      request: "listIssueAskThreads",
    },
  ] as const;
  for (const { mount, other, read, request } of refused) {
    const asked = spyOn(api, request).mockRejectedValue(new Error("refused"));
    const notAsked = spyOn(api, other);
    try {
      const { hook } = await mount();
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
    { queryKey: ["decisions", KEY], read: "issue-decisions", request: "getIssueDecisions" },
    { queryKey: ["asks", KEY, "thread"], read: "asks", request: "listIssueAskThreads" },
  ] as const;
  for (const { queryKey, read, request } of refetched) {
    const answer = Promise.withResolvers<never>();
    const asked = spyOn(api, request).mockReturnValue(answer.promise);
    try {
      const { client, hook } = await mountEntries(children, noDecision);
      const reading = () => ({
        read,
        readsHeld: hook.result.current.readsHeld,
        retrying: hook.result.current.retrying,
        unreadable: hook.result.current.unreadable,
      });
      const held = { read, readsHeld: true, retrying: [], unreadable: [] };
      expect(reading()).toEqual(held);
      const before = hook.result.current.parts;
      await act(async () => hook.result.current.retry(read));
      await waitFor(() => expect(reading()).toEqual({ ...held, retrying: [read] }));
      expect(hook.result.current.parts).toBe(before);
      await act(async () => {
        answer.reject(new Error("refused"));
        await answer.promise.catch(() => undefined);
      });
      await waitFor(() => expect(hook.result.current.retrying).toEqual([]));
      expect(client.getQueryState(queryKey)?.status).toBe("error");
      expect(reading()).toEqual(held);
      expect(asked).toHaveBeenCalledTimes(1);
      hook.unmount();
    } finally {
      asked.mockRestore();
    }
  }
});
