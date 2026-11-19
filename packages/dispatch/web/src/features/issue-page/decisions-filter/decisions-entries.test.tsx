import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { api } from "../../../api/client";
import type { Actor, ArtifactBlock, Comment, Decision, IssueDecisions } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { presetView } from "../presets";
import { useDecisionsEntries } from "./decisions-entries";
import {
  answeredAsk,
  closing,
  DECISIONS_DOCUMENT,
  decision,
  decisionNode,
  decisionsRead,
  issueDetails,
  issueTree,
  KEY,
  openAsk,
  READER,
  READER_ACTOR,
  shaped,
  specRef,
  statement,
} from "./decisions-fixtures";
import type { DecisionsAsk, DecisionsParts } from "./decisions-parts";

/**
 * The seam between today's reads and the Decisions view. Each case puts the three reads and the
 * decisions document's blocks in the cache under the keys the page's tree reads them by, as the
 * cache holds them once the tree is drawn, and no query reads again on mount, so any request the
 * seam made would be one of its own. The view is the one the slot passes: the tree as the
 * Decisions view leaves it (`presetView`).
 */

const BOB: Actor = { id: "bob", kind: "user" };

/** The seam mounted over the issue whose tree holds `children`, with `decisions`, `asks`,
 *  `comments` and the decisions document's `blocks` as the cache holds them, until the case
 *  unmounts it. A read given as null is left out of the cache, as before it answers. */
async function mountEntries(
  children: readonly TreeNode[],
  decisions: IssueDecisions | null,
  asks: readonly DecisionsAsk[] | null = [],
  comments: readonly Comment[] | null = [],
  blocks: readonly ArtifactBlock[] | null = []
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  if (decisions !== null) client.setQueryData(["decisions", KEY], decisions);
  if (asks !== null) client.setQueryData(["asks", KEY], asks);
  if (comments !== null) client.setQueryData(["comments", KEY], comments);
  // The issue names its decisions document by its role (`issueDetails`), so the seam reads that
  // document's blocks, as the tree does.
  if (blocks !== null) client.setQueryData(["artifact", DECISIONS_DOCUMENT.id, "blocks"], blocks);
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
  const parts = hook.result.current;
  hook.unmount();
  return parts;
}

/** The decisions document's blocks as the blocks read answers them, in document order: each
 *  decision's heading, whose block is the decision's own (`Decision.block_id`), then one block
 *  for each paragraph of its words, each at the place it holds in the document's markdown. */
function documentBlocks(made: readonly Decision[]): ArtifactBlock[] {
  const blocks: ArtifactBlock[] = [];
  let at = 0;
  const add = (id: string, type: string, markdown: string) => {
    blocks.push({
      attributes: [],
      from: at,
      id,
      references: { asks: 0, comments: 0 },
      to: at + markdown.length,
      token: id,
      type,
    });
    // A blank line stands between two blocks of the markdown.
    at += markdown.length + 2;
  };
  for (const one of made) {
    add(one.block_id, "heading", `## ${one.heading}`);
    one.text.split("\n\n").forEach((paragraph, index) => {
      add(`${one.block_id}-${index + 1}`, "paragraph", paragraph);
    });
  }
  return blocks;
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
  // The reader's Change on the second decision, made after both last changed, anchored on the
  // block of the second decision that holds `quote`.
  const change: Comment = {
    ...commentDeliveryFields(),
    anchor: {
      artifact_id: DECISIONS_DOCUMENT.id,
      block_id: "asked-1",
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
    decisionsRead(made),
    asks,
    [change],
    documentBlocks(made)
  );
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
  const listIssueAsks = spyOn(api, "listIssueAsks");
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
    expect(listIssueAsks).not.toHaveBeenCalled();
    expect(listComments).not.toHaveBeenCalled();
    expect(request).not.toHaveBeenCalled();
  } finally {
    getIssueDecisions.mockRestore();
    listIssueAsks.mockRestore();
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

// R8: a decisions read that holds no decision, or none of its `decisions` member, gives no
// decision and no error, and the view draws the questions alone. The tree is the one the page
// builds from that same read, which holds no decision node (`tree-drafts.ts`'s `addDecisions`).
// Such a read holds its answer all the same (`readsHeld`). While any one of the four reads has
// not answered, the questions are drawn all the same. The reads hold once the decisions read and
// the asks have answered, which say which questions and decisions there are; the comments and
// the blocks only move a decision between parts, so the reads hold while those are out. Either
// of the first two refused before it first answers is the seam's to name, with a retry of its
// own, so the body can say so without naming a read.
test("an empty decisions member gives the questions alone and no error", async () => {
  const children = [statement("s1"), openAsk("q-open")];
  // A read with no `decisions` member, which its type does not allow and an answer can still be.
  const { document, open_questions } = decisionsRead([]);
  const missing = { document, open_questions } as unknown as IssueDecisions;
  for (const [read, decisions] of [
    ["an empty member", decisionsRead([])],
    ["no member", missing],
  ] as const) {
    const drawn = entriesOf(children, decisions);
    // The hook throws nothing on this read: the promise of its parts settles with them.
    await expect(drawn).resolves.toBeDefined();
    const entries = await drawn;
    expect({ parts: idsOf(entries), read, readsHeld: entries.readsHeld }).toEqual({
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
  const cold = [
    {
      answer: noDecision,
      heldWhileOut: false,
      key: ["decisions", KEY],
      mount: () => mountEntries(children, null),
      read: "getIssueDecisions",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: false,
      key: ["asks", KEY],
      mount: () => mountEntries(children, noDecision, null),
      read: "listIssueAsks",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: true,
      key: ["comments", KEY],
      mount: () => mountEntries(children, noDecision, [], null),
      read: "listComments",
      starts: 1,
    },
    {
      answer: [],
      heldWhileOut: true,
      key: ["artifact", DECISIONS_DOCUMENT.id, "blocks"],
      mount: () => mountEntries(children, noDecision, [], [], null),
      read: "getArtifactBlocks",
      starts: 0,
    },
  ] as const;
  for (const { answer, heldWhileOut, key, mount, read, starts } of cold) {
    const started = spyOn(api, read).mockReturnValue(Promise.withResolvers<never>().promise);
    try {
      const { client, hook } = await mount();
      const reading = () => ({
        open_questions: idsOf(hook.result.current).open_questions,
        read,
        readsHeld: hook.result.current.readsHeld,
      });
      expect(reading()).toEqual({ open_questions: ["q-open"], read, readsHeld: heldWhileOut });
      expect(started).toHaveBeenCalledTimes(starts);
      await act(async () => client.setQueryData(key, answer));
      expect(reading()).toEqual({ open_questions: ["q-open"], read, readsHeld: true });
      hook.unmount();
    } finally {
      started.mockRestore();
    }
  }

  // Each of the two reads that say which entries there are is refused in turn before it first
  // answers, with the other read in the cache. The refused read holds no answer, so the seam
  // says it is unreadable and the reads do not hold; its retry asks that read again, which is
  // refused again, and asks the other read nothing.
  const refused = [
    {
      mount: () => mountEntries(children, null),
      other: "listIssueAsks",
      read: "decisions",
      request: "getIssueDecisions",
    },
    {
      mount: () => mountEntries(children, noDecision, null),
      other: "getIssueDecisions",
      read: "asks",
      request: "listIssueAsks",
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
      await act(async () => hook.result.current.retry(read));
      await waitFor(() => expect(asked).toHaveBeenCalledTimes(2));
      await waitFor(() => expect(reading()).toEqual(unreadable));
      expect(notAsked).not.toHaveBeenCalled();
      hook.unmount();
    } finally {
      asked.mockRestore();
      notAsked.mockRestore();
    }
  }
});
