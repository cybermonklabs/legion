import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { ApiError, api } from "../../../api/client";
import type { Agent, DispatchUser, ReferenceTarget, ReferenceTargets } from "../../../api/types";
import { agentsQuery } from "../../refs/agent-registry";
import { type PublishedReferences, ReferenceNamesProvider } from "../../refs/reference-names";
import { parseNodeReference, shortForm } from "../../refs/routes";
import { issueDetails } from "../decisions-filter/decisions-fixtures";
import { NODE_REFERENCES_WORDS, sentenceCase } from "../page-words";
import { Publisher, publishedNames } from "../projection-fixture";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import {
  issueTreeQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  type TreeTuple,
} from "../tree-read/tree-read";
import { NamedRunRows, runRows, useRunRows } from "./run-references";

/** The issue the tree read answers, as `treeAnswer` answers it. */
const KEY = "CORE-1";
const COMMENT = "7d1e4c2a-9b3f-4e8d-a6c5-2f0b1d9e7a34";
const SECOND_COMMENT = "8e2f5d3b-0c4a-4f9e-b7d6-3a1c2e0f8b45";
const RUN = "3f2b8c1e-5d4a-4b6f-9e2d-1a7c0b9e8f43";
const OTHER_RUN = "5a4d0e3f-7b6c-4d8e-9f1a-2b3c4d5e6f70";
const FIRST_RUN = "1c9e7a5b-3d2f-4e1a-8b6c-0d4f2a9e7b51";
const ASKED_AT = "2026-11-17T09:00:00Z";
const QUESTION = "When does the **release** ship?\n\nAnd who owns it?";
/** `QUESTION` as plain words: its marks gone, its two paragraphs on one line. */
const QUESTION_WORDS = "When does the release ship? And who owns it?";

/** The issue's own node. */
const ISSUE_NODE = treeNode(KEY, "issue", null, { text: "Ship the release" });

/** A comment of the issue. */
function commentNode(id: string): TreeAnswerNode {
  return treeNode(id, "comment", KEY, { text: "The release waits on the **build**." });
}

/** A command bar run, as the asker's tree read answers it (`issue_tree_read.go`'s
 *  `readContext`): a context node of the `interaction` kind and supertag, under no node, by the
 *  run's id, its asker's alone, with the question as its text. */
function runNode(id: string, madeAt: string | null = ASKED_AT, text = QUESTION): TreeAnswerNode {
  return treeNode(id, "interaction", null, {
    made_at: madeAt,
    role: "context",
    text,
    visibility: "author",
  });
}

/** The tuple a run writes from a node it created or changed to itself: its far end is the run, by
 *  its store kind and id, whether or not the read holds the run's node. */
function changedBy(from: string, run: string): TreeTuple {
  return { ...treeTuple("changed_by_run", from, run), to: { id: run, kind: "interaction" } };
}

/** One page of the tree read, which holds the whole tree where it is the read's one page. */
function treeOf(nodes: readonly TreeAnswerNode[], tuples: readonly TreeTuple[]): TreeAnswer {
  return treeAnswer(nodes, tuples, { converted: true, tree: "tree-core-1" });
}

test("an interaction of the tree read is a row with the page's words and the run's words", () => {
  const rows = runRows([
    treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], [changedBy(COMMENT, RUN)]),
  ]);
  expect(rows).toHaveLength(1);
  const [row] = rows;
  expect(row.key).toBe(RUN);
  expect(row.kind).toBe(sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction));
  expect(row.edge).toBe(NODE_REFERENCES_WORDS.edges.changed_by_run);
  expect(row.excerpt).toBe(QUESTION_WORDS);
  expect(row.at).toBe(ASKED_AT);
  expect(row.href).toBe(`/runs/${RUN}`);
  const { href: _href, key: _key, ...words } = row;
  const drawn = JSON.stringify(words);
  expect(drawn).not.toContain("interaction");
  expect(drawn).not.toContain("changed_by_run");
  expect(drawn).not.toContain(RUN);
});

// A run's row reads its question's words from the server's reading of it where the tree read
// sends one (`text_tree`), not from the question as stored.
test("a run's row reads its question's words from the reading the tree read sends", () => {
  const reading = {
    content: [{ content: [{ text: "Who ships the release?", type: "text" }], type: "paragraph" }],
    type: "doc",
  };
  const rows = runRows([
    treeOf(
      [ISSUE_NODE, commentNode(COMMENT), { ...runNode(RUN), text_tree: reading }],
      [changedBy(COMMENT, RUN)]
    ),
  ]);
  expect(rows.map((row) => row.excerpt)).toEqual(["Who ships the release?"]);
});

// Decision 533 (root spec v184's R33): an issue's address with `?issue_page=new` draws the new
// page for a reader whose setting is off, and the run's page opens for that reader only at a
// run's address that carries it too (`RunRoute.tsx`). The run's page cannot know from its own
// read which issue opened it, so the link names that issue as well (`issue`), and the run's page
// keeps the parameter on that issue's links (`RunPage.tsx`). So where the address the caller
// hands (`useRunRows`' `location`) is one of the issue that carries it, each run's row links to
// its run's page with it and the issue's key, and elsewhere, or with no address handed, by the
// plain address.
test("a run's row links to its run's page with ?issue_page=new and the issue's key where the issue's address carries it, and without them where it does not", () => {
  const issue = issueDetails();
  const linksAt = (location: PageAddress) => {
    const view = drawQuestions([runNode(RUN)], { location });
    try {
      return view.links();
    } finally {
      view.close();
    }
  };
  const newPage = `/runs/${RUN}?issue_page=new&issue=${issue.key}`;
  expect(
    linksAt({ pathname: `/issues/${issue.key}`, search: "?issue_page=new" }),
    "at the issue's address with the parameter"
  ).toEqual([newPage]);
  expect(
    linksAt({ pathname: `/issues/${issue.key}/comments/${COMMENT}`, search: "?issue_page=new" }),
    "at a comment's address of the issue with the parameter"
  ).toEqual([newPage]);
  expect(
    linksAt({ pathname: `/issues/${issue.key}`, search: "" }),
    "at the issue's address without it"
  ).toEqual([`/runs/${RUN}`]);
  expect(
    linksAt({ pathname: `/issues/${issue.key}`, search: "?issue_page=old" }),
    "at the issue's address with another value of it"
  ).toEqual([`/runs/${RUN}`]);
  expect(linksAt(undefined), "with no address handed").toEqual([`/runs/${RUN}`]);
});

/** Where the page is, as `useRunRows`' caller hands it, or undefined where it hands none. */
type PageAddress = Parameters<typeof useRunRows>[2];

/** Details' run rows for `runs`, the runs the issue's tree read answers, each question drawn as
 *  a link of its own to the address its row links to, under the page's store of names, beside
 *  what another page publishes there (`published`) where a case gives it. The rows are read at
 *  the page's address `location`, where a case gives one. The batched read of reference targets
 *  answers by `batch`, and a read of one comment or one block of its own is refused, which a
 *  case counts. `answerRuns` answers the tree read anew, as a refresh of it does. */
function drawQuestions(
  runs: readonly TreeAnswerNode[],
  {
    batch = () => Promise.resolve({ targets: [] }),
    location,
    published,
  }: {
    readonly batch?: (refs: readonly string[]) => Promise<ReferenceTargets>;
    readonly location?: PageAddress;
    readonly published?: PublishedReferences;
  } = {}
) {
  const batched = spyOn(api, "getReferenceTargets").mockImplementation(batch);
  const comments = spyOn(api, "getComment").mockRejectedValue(new ApiError(404, {}));
  const blocks = spyOn(api, "getReferences").mockRejectedValue(new ApiError(404, {}));
  const issue = issueDetails();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const answerRuns = (answered: readonly TreeAnswerNode[]) => {
    client.setQueryData(
      issueTreeQuery(issue.key).queryKey,
      treeOf(
        [ISSUE_NODE, ...answered],
        answered.map((run) => changedBy(KEY, run.id))
      )
    );
  };
  answerRuns(runs);
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        ReferenceNamesProvider,
        null,
        published === undefined ? null : createElement(Publisher, { published }),
        children
      )
    );
  function Questions(): ReactNode {
    const { rows } = useRunRows(issue, false, location);
    return createElement(NamedRunRows, {
      draw: (named) =>
        named.map((row) => createElement("a", { href: row.href, key: row.key }, row.excerpt)),
      issue,
      rows,
    });
  }
  const { container, unmount } = render(createElement(Questions), { wrapper });
  return {
    answerRuns: (answered: readonly TreeAnswerNode[]) => act(() => answerRuns(answered)),
    batch: batched,
    blocks,
    client,
    close: () => {
      unmount();
      client.clear();
      batched.mockRestore();
      comments.mockRestore();
      blocks.mockRestore();
    },
    comments,
    drawn: () => [...container.querySelectorAll("a")].map((row) => row.textContent ?? ""),
    links: () => [...container.querySelectorAll("a")].map((row) => row.getAttribute("href")),
  };
}

/** The short form `reference` draws where no name and no written words stand for it. */
function shortFormOf(reference: string): string {
  const route = parseNodeReference(reference);
  if (route === undefined) throw new Error(`${reference} names no node`);
  return shortForm(route);
}

// A run's question is a one-line place (the reading list's `referenced-by-run-question`): each
// reference it cites is named as every plain line of the page names one (`referenceStandIns`) -
// an issue by its key, a space and its title, and any other target by the page's name for it,
// once the page's batched read has named it, and as its asker wrote it until then - and never by
// its short form.
test("a run's row names each reference in its question by its name once read, and as its asker wrote it until then", async () => {
  const issue = "dispatch://CORE-2";
  const named = `dispatch://${KEY}/comment/${COMMENT}`;
  const unnamed = `dispatch://${KEY}/comment/${SECOND_COMMENT}`;
  const question = `Does ${issue} wait on ${named} or on ${unnamed}?`;
  // The page's batched read names the first comment, and has been asked for the second and
  // holds no answer for it. No page has named the issue, so the run's one batch asks for it.
  const published = publishedNames(new Map([[named, "Warm the cache first"]]), {
    asked: [unnamed],
  });
  const answer = Promise.withResolvers<ReferenceTargets>();
  const view = drawQuestions([runNode(RUN, ASKED_AT, question)], {
    batch: () => answer.promise,
    published,
  });
  try {
    // While the batch is out, the issue reads as its asker wrote it.
    const unread = `Does ${issue} wait on Warm the cache first or on ${unnamed}?`;
    await screen.findByText(unread);
    expect(view.drawn()).toEqual([unread]);
    expect(view.batch.mock.calls).toEqual([[[issue]]]);
    const titled = [
      { gone: false, kind: "issue", name: "Pin the release date", ref: issue },
    ] satisfies ReferenceTarget[];
    await act(async () => answer.resolve({ targets: titled }));
    const expected = `Does CORE-2 Pin the release date wait on Warm the cache first or on ${unnamed}?`;
    await screen.findByText(expected);
    expect(view.drawn()).toEqual([expected]);
    expect(view.drawn()[0]).not.toContain(shortFormOf(unnamed));
    expect(view.batch).toHaveBeenCalledTimes(1);
    expect(
      view.comments.mock.calls.length + view.blocks.mock.calls.length,
      "a read of one target of its own"
    ).toBe(0);
  } finally {
    view.close();
  }
});

// A key a run's question writes bare is named as every one-line place names one: by its key and
// its issue's title, where the page published that issue (`knownIssueKeys`). No other publisher
// asks for it where the run's question is drawn alone, so the run's one batch asks for it as the
// issue's own reference, beside the addresses, and reads no target of its own. A word the text
// reader reads as a key that names no issue (`UTF-16`) is answered gone, and reads as its asker
// wrote it.
test("a run's bare issue key joins the one batch and is named by its key and title once read, and a key-shaped word that names no issue reads as written", async () => {
  const question = "Does CORE-9 ship before the UTF-16 fix?";
  const answer = Promise.withResolvers<ReferenceTargets>();
  const view = drawQuestions([runNode(RUN, ASKED_AT, question)], { batch: () => answer.promise });
  try {
    // While the batch is out, both read as the asker wrote them.
    await screen.findByText(question);
    expect(view.drawn()).toEqual([question]);
    expect(view.batch.mock.calls).toEqual([[["dispatch://CORE-9", "dispatch://UTF-16"]]]);
    const targets = [
      { gone: false, kind: "issue", name: "Pin the release date", ref: "dispatch://CORE-9" },
      { gone: true, kind: "issue", name: "", ref: "dispatch://UTF-16" },
    ] satisfies ReferenceTarget[];
    await act(async () => answer.resolve({ targets }));
    const expected = "Does CORE-9 Pin the release date ship before the UTF-16 fix?";
    await screen.findByText(expected);
    expect(view.drawn()).toEqual([expected]);
    expect(view.batch).toHaveBeenCalledTimes(1);
    expect(
      view.comments.mock.calls.length + view.blocks.mock.calls.length,
      "a read of one target of its own"
    ).toBe(0);
  } finally {
    view.close();
  }
});

// Decision 350: where the batched read answers that nothing answers to a target a run's question
// cites, or answers with no words to name it by, the question draws the reference as its asker
// wrote it, as it draws every reference the read has not answered yet, and no read of its own
// asks for it. It stays in the batch's set while the question cites it (`useRunTargets`'
// `lastAsked`), so a read of the batch under a new set, where the references the lines cite
// change, asks for it again. The read answers a target the reader may not see as it answers one
// nothing addresses, gone with no name (`reference_targets.go`), so the reader learns nothing of
// it, and the question draws it the same way.
test("a run's reference the batch answers gone or with no name reads as its author wrote it", async () => {
  const named = "dispatch://CORE-2/comment/4a6c8e0b-2d4f-4a6c-8e0b-2d4f6a8c0e1a";
  const gone = "dispatch://CORE-2/comment/5b7d9f1c-3e5a-4b7d-9f1c-3e5a7b9d1f2b";
  // A comment of an issue the reader may not see.
  const hidden = "dispatch://SECRET-1/comment/6c8e0a2d-4f6b-4c8e-0a2d-4f6b8c0e2a3c";
  // A block of another issue, whose words the read sends no reading of, so the page has no words
  // to name it by (`tree-drafts.ts`'s `readingOf`) and publishes it unnamed.
  const block = "dispatch://CORE-2/spec#b-r4";
  const question = `Does ${named} wait on ${gone}, on ${hidden} or on ${block} first?`;
  const targets = [
    { gone: false, kind: "comment", name: "Warm the cache first", ref: named },
    { gone: true, kind: "comment", name: "", ref: gone },
    { gone: true, kind: "comment", name: "", ref: hidden },
    { gone: false, kind: "block", name: ":::requirement{#r4}\nShip it.\n:::", ref: block },
  ] satisfies ReferenceTarget[];
  const answer = Promise.withResolvers<ReferenceTargets>();
  const view = drawQuestions([runNode(RUN, ASKED_AT, question)], { batch: () => answer.promise });
  try {
    // While the batch is out, each of the four reads as its asker wrote it.
    await screen.findByText(question);
    expect(view.drawn()).toEqual([question]);
    expect(view.batch.mock.calls).toEqual([[[named, gone, hidden, block].sort()]]);
    await act(async () => answer.resolve({ targets }));
    const expected = `Does Warm the cache first wait on ${gone}, on ${hidden} or on ${block} first?`;
    await screen.findByText(expected);
    expect(view.drawn()).toEqual([expected]);
    for (const reference of [named, gone, block]) {
      expect(view.drawn()[0]).not.toContain(shortFormOf(reference));
    }
    expect(
      view.drawn()[0],
      "a run's reference the reader may not see reads as its author wrote it"
    ).not.toContain(shortFormOf(hidden));
    expect(view.batch).toHaveBeenCalledTimes(1);
    expect(view.comments, "a comment read of its own").toHaveBeenCalledTimes(0);
    expect(view.blocks, "a block read of its own").toHaveBeenCalledTimes(0);
  } finally {
    view.close();
  }
});

// Decision 350: a short reference whose id two or more targets carry names none of them. The
// batched read answers it gone, with the name "" and `ambiguous` (`reference_targets.go`), and the
// question draws it as its asker wrote it, never by a short form and never by a read of its own.
test("a run's reference the batch answers ambiguous reads as its author wrote it", async () => {
  const named = "dispatch://CORE-2/comment/4a6c8e0b-2d4f-4a6c-8e0b-2d4f6a8c0e1a";
  // A comment's id written short, which two comments of CORE-2 carry.
  const ambiguous = "dispatch://CORE-2/comment/9d0e2f4a";
  const question = `Does ${named} answer ${ambiguous}?`;
  const targets = [
    { gone: false, kind: "comment", name: "Warm the cache first", ref: named },
    { ambiguous: true, gone: true, kind: "comment", name: "", ref: ambiguous },
  ] satisfies ReferenceTarget[];
  const view = drawQuestions([runNode(RUN, ASKED_AT, question)], {
    batch: () => Promise.resolve({ targets }),
  });
  try {
    const settled = `Does Warm the cache first answer ${ambiguous}?`;
    // The case's named reference draws its name once the batch has answered.
    await screen.findByText(settled);
    expect(view.drawn()).toEqual([settled]);
    expect(view.drawn()[0]).not.toContain(shortFormOf(ambiguous));
    expect(view.batch).toHaveBeenCalledTimes(1);
    expect(
      view.comments.mock.calls.length + view.blocks.mock.calls.length,
      "a read of one target of its own"
    ).toBe(0);
  } finally {
    view.close();
  }
});

// The batch's key is its set of references, so a run the tree read adds whose question cites a
// comment the set did not hold is a read under a new key, which holds no answer while it is out.
// Meanwhile the name the set before it drew stays drawn, and once the new read answers, both
// comments are named.
test("a run's name already drawn stays drawn while a new run's batch is out, and both are named once it answers", async () => {
  const first = "dispatch://CORE-2/comment/4a6c8e0b-2d4f-4a6c-8e0b-2d4f6a8c0e1a";
  const second = "dispatch://CORE-2/comment/5b7d9f1c-3e5a-4b7d-9f1c-3e5a7b9d1f2b";
  const firstRun = runNode(FIRST_RUN, "2026-11-17T08:00:00Z", `Does the release wait on ${first}?`);
  const firstNamed = { gone: false, kind: "comment", name: "Warm the cache first", ref: first };
  const secondNamed = { gone: false, kind: "comment", name: "Check the logs", ref: second };
  const later = Promise.withResolvers<ReferenceTargets>();
  const view = drawQuestions([firstRun], {
    batch: (refs) =>
      refs.includes(second) ? later.promise : Promise.resolve({ targets: [firstNamed] }),
  });
  try {
    const firstQuestion = "Does the release wait on Warm the cache first?";
    await screen.findByText(firstQuestion);
    // The tree read answers anew with a newer run, whose question cites the second comment.
    await view.answerRuns([firstRun, runNode(RUN, ASKED_AT, `Or on ${second}?`)]);
    await waitFor(() =>
      expect(view.batch.mock.calls).toEqual([[[first]], [[first, second].sort()]])
    );
    expect(view.drawn(), "the first name while the new batch is out").toEqual([
      `Or on ${second}?`,
      firstQuestion,
    ]);
    await act(async () => later.resolve({ targets: [firstNamed, secondNamed] }));
    await screen.findByText("Or on Check the logs?");
    expect(view.drawn()).toEqual(["Or on Check the logs?", firstQuestion]);
  } finally {
    view.close();
  }
});

// Spec R1 (v19), decision 340 with decision 350: a party (an agent, a role or a person) stays out
// of the one batched read and is named by a read of its own. Until that read answers, and wherever
// it names nothing, the party reads as the words its author wrote, never by its short form.
test("a run's party reference reads as its author wrote it until its read answers, and where it names nothing", async () => {
  const person = "dispatch://person/ada@example.com";
  const stranger = "dispatch://person/nobody@example.com";
  const question = `Ask ${person}, not ${stranger}, before the release`;
  const people = Promise.withResolvers<DispatchUser[]>();
  const users = spyOn(api, "listUsers").mockReturnValue(people.promise);
  const batch = spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] });
  const issue = issueDetails();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(
    issueTreeQuery(issue.key).queryKey,
    treeOf([ISSUE_NODE, runNode(RUN, ASKED_AT, question)], [changedBy(KEY, RUN)])
  );
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(
      QueryClientProvider,
      { client },
      createElement(ReferenceNamesProvider, null, children)
    );
  function Questions(): ReactNode {
    const { rows } = useRunRows(issue, false);
    return createElement(NamedRunRows, {
      draw: (named) => named.map((row) => createElement("p", { key: row.key }, row.excerpt)),
      issue,
      rows,
    });
  }
  const { container, unmount } = render(createElement(Questions), { wrapper });
  const drawn = () => [...container.querySelectorAll("p")].map((row) => row.textContent ?? "");
  try {
    // While the read of people is out, each person reads as the asker wrote it.
    await screen.findByText(question);
    expect(drawn()).toEqual([question]);
    expect(users).toHaveBeenCalledTimes(1);
    await act(async () => people.resolve([{ login: "ada@example.com" }]));
    await waitFor(() => expect(drawn()[0]).not.toContain(person));
    const settled = drawn()[0] ?? "";
    // The person Dispatch knows is named; the one it does not know names nothing, so the words
    // the asker wrote stand, and no short form is drawn on its own in their place.
    expect(settled).toContain("ada@example.com");
    expect(settled).toContain(stranger);
    expect(settled.replaceAll(stranger, "")).not.toContain(shortFormOf(stranger));
    expect(batch, "a party in the batched read").not.toHaveBeenCalled();
  } finally {
    unmount();
    client.clear();
    users.mockRestore();
    batch.mockRestore();
  }
});

// Spec R1 (v19), decision 340: a role is named by its own read of the registry, and only while a
// session the registry lists holds it (`reference-target.ts`'s `roleKnown`): a run's question is
// no record of an issue whose collaborators could record it. A role no session holds names
// nothing, so it reads as the words its author wrote while that read is out and after it
// answers, never by its short form.
test("a run's role that no session holds reads as its author wrote it while its read is out and after it answers", async () => {
  const role = "dispatch://role/surveyor";
  const question = `Should ${role} check the release first?`;
  const registry = Promise.withResolvers<Agent[]>();
  const agents = spyOn(api, "listAgents").mockReturnValue(registry.promise);
  const view = drawQuestions([runNode(RUN, ASKED_AT, question)]);
  try {
    await screen.findByText(question);
    expect(view.drawn(), "while the registry's read is out").toEqual([question]);
    expect(agents).toHaveBeenCalledTimes(1);
    await act(async () => registry.resolve([]));
    await waitFor(() =>
      expect(view.client.getQueryState(agentsQuery().queryKey)?.status).toBe("success")
    );
    await act(async () => {});
    expect(view.drawn(), "once the registry answered no holder").toEqual([question]);
    expect(view.batch, "a party in the batched read").not.toHaveBeenCalled();
  } finally {
    view.close();
    agents.mockRestore();
  }
});

test("one run is one row however many writes it made", () => {
  const rows = runRows([
    treeOf(
      [ISSUE_NODE, commentNode(COMMENT), commentNode(SECOND_COMMENT), runNode(RUN)],
      [changedBy(KEY, RUN), changedBy(COMMENT, RUN), changedBy(SECOND_COMMENT, RUN)]
    ),
  ]);
  expect(rows.map((row) => row.key)).toEqual([RUN]);
});

test("rows stand newest first by made_at", () => {
  const newestFirst = runRows([
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, "2026-11-16T09:00:00Z"), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    ),
  ]);
  expect(newestFirst.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);

  // `RUN` sorts before `OTHER_RUN` by id, and stands after it in the read.
  const sameTime = runRows([
    treeOf(
      [ISSUE_NODE, runNode(OTHER_RUN, ASKED_AT), runNode(RUN, ASKED_AT)],
      [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
    ),
  ]);
  expect(sameTime.map((row) => row.key)).toEqual([RUN, OTHER_RUN]);
});

test("a run the read answers no made_at for stands after the runs it answers one for", () => {
  // `FIRST_RUN` sorts before `RUN`, and `RUN` before `OTHER_RUN`, by id: by id alone the run with
  // no time would stand second.
  const rows = runRows([
    treeOf(
      [
        ISSUE_NODE,
        runNode(RUN, null),
        runNode(OTHER_RUN, "2026-11-16T09:00:00Z"),
        runNode(FIRST_RUN, ASKED_AT),
      ],
      [changedBy(KEY, RUN), changedBy(KEY, OTHER_RUN), changedBy(KEY, FIRST_RUN)]
    ),
  ]);
  expect(rows[2]?.at, "the run with no time reads a time").toBeUndefined();
});

test("a tree read with no run draws no row", () => {
  // Another reader's tree: the tuple a run wrote, and no run, since only its asker reads it.
  expect(
    runRows([treeOf([ISSUE_NODE, commentNode(COMMENT)], [changedBy(COMMENT, RUN)])])
  ).toHaveLength(0);
  // A run that changed nothing of this issue.
  expect(
    runRows([
      treeOf(
        [ISSUE_NODE, commentNode(COMMENT), runNode(RUN)],
        [treeTuple("replies_to", COMMENT, KEY)]
      ),
    ])
  ).toHaveLength(0);
  // A tuple of the run's kind whose far end is no run, though a run's node holds its id: the
  // tuples are set after `treeAnswer`, which would name a held end by its node's kind.
  expect(
    runRows([
      {
        ...treeOf([ISSUE_NODE, commentNode(COMMENT), runNode(RUN)], []),
        tuples: [
          {
            from: { id: COMMENT, kind: "comment" },
            kind: "changed_by_run",
            to: { id: RUN, kind: "comment" },
          },
        ],
      },
    ])
  ).toHaveLength(0);
  // A read that holds no page yet.
  expect(runRows([])).toHaveLength(0);
});

test("the runs of the tree read's pages are rows, one per run, and a page of older comments adds none", () => {
  // The first page: the issue, and the two runs that changed it, `RUN` the newer.
  const first = treeOf(
    [ISSUE_NODE, runNode(OTHER_RUN, "2026-11-16T09:00:00Z"), runNode(RUN, ASKED_AT)],
    [changedBy(KEY, OTHER_RUN), changedBy(KEY, RUN)]
  );
  // A page of older comments (`comments_before`): a comment and its tuples and no other node.
  // `RUN` changed the comment too, and `FIRST_RUN` changed that comment and nothing else, so no
  // page holds its node.
  const olderComments = treeOf(
    [commentNode(COMMENT)],
    [changedBy(COMMENT, RUN), changedBy(COMMENT, FIRST_RUN)]
  );
  expect(runRows([first, olderComments]).map((row) => row.key)).toEqual([RUN, OTHER_RUN]);
});
