import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../../api/client";
import { issueResultsQuery } from "../../../api/queries";
import type { Comment, ReferenceTarget } from "../../../api/types";
import { DESKTOP_WIDTH, stubViewport } from "../../margin/margin-fixture";
import { buildDispatchReference } from "../../refs/routes";
import { KeymapProvider } from "../../shell/KeymapProvider";
import {
  decisionsRead,
  issueDetails,
  KEY,
  SPEC_DOCUMENT,
  SPEC_SLUG,
} from "../decisions-filter/decisions-fixtures";
import { presetPath } from "../issue-address";
import type { TreeNode } from "../issue-tree";
import { readComment, TWO_THREADS } from "../outline-fixtures";
import { FILTER_SIGNAL_WORDS, PRESET_NAMES, PRESET_ORDER, type Preset } from "../page-words";
import { commentNode, treeAnswer, treeNode } from "../tree-read/tree-fixtures";
import { issueTreeQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";
import { ProvidersSlot } from "./providers";
import { ViewRowSlot, viewRowSignals } from "./view-row";

/**
 * The view row's counts. Each is read off the issue's tree, which lacks the nodes of a read
 * still out or failed, so a count read before every read holds its answer is short - often a
 * `0` - and reads as the real one. The row draws no count, unit word or flag until then, and the
 * real counts once it holds them.
 */

/** A typed block of the spec at `position`, with `text` as its words, as the tree read answers
 *  it. */
function specBlock(id: string, supertag: string, position: number, text = ""): TreeAnswerNode {
  return treeNode(id, supertag, KEY, {
    document: { artifact_id: SPEC_DOCUMENT.id, position, slug: SPEC_SLUG },
    text,
  });
}

/** The spec's two `requirement` blocks: statements, which the Spec view counts. */
const STATEMENTS: readonly TreeAnswerNode[] = [
  specBlock("r1", "requirement", 0),
  specBlock("r2", "requirement", 1),
];

/** The address of the spec's step (`STEP_NODE`). The names read is not given it, as it is given
 *  no node's own address: it names the issue's own key, which the root's words write. */
const STEP = buildDispatchReference({
  block: { id: "t1" },
  key: KEY,
  kind: "artifact",
  slug: SPEC_SLUG,
});

/** The step's words, which hold the words filter's `lanterns`. */
const STEP_WORDS = "Hang the lanterns first.";

/** A step of the spec (`STEP`), which the Plan view counts: the tree read answers it with its
 *  words, as it answers every block's. */
const STEP_NODE = specBlock("t1", "plan_task", 2, STEP_WORDS);

/** The tree read's answer for the fixtures' issue: the issue, `blocks` of the spec, and each of
 *  `comments` under the comment it replies to, else under the issue. The outline draws its rows
 *  from it, the spec's blocks and the turns alike. */
function treeOf(comments: readonly Comment[], blocks: readonly TreeAnswerNode[]): TreeAnswer {
  return treeAnswer([
    treeNode(KEY, "issue", null),
    ...blocks,
    ...comments.map((comment) => commentNode(comment)),
  ]);
}

/** What the row draws once every read has answered: the spec's two statements, and the two
 *  threads the reader began, which are Conversation's entries. */
const ANSWERED_COUNTS: Readonly<Record<Preset, string>> = {
  conversation: "2",
  decisions: "0",
  // Graph draws no count (R10), which `toEqual` reads as a member that holds none.
  graph: undefined as never,
  plan: "0",
  spec: "2",
  verification: "0",
};

/** The row's text with each control's name alone, which is all it draws with no signals. */
const NAMES_ALONE = PRESET_ORDER.map((preset) => PRESET_NAMES[preset]).join("");

/** Answers every read the row reaches: the tree's six reads, the issue's tree read, the reader
 *  and the names. A case changes the answer of the read it holds out or fails. */
function answerReads() {
  return {
    asks: spyOn(api, "listIssueAskThreads").mockResolvedValue([] as never),
    comments: spyOn(api, "listComments").mockResolvedValue(TWO_THREADS as never),
    coverage: spyOn(api, "getIssueCoverage").mockResolvedValue({ requirements: [] } as never),
    decisions: spyOn(api, "getIssueDecisions").mockResolvedValue(decisionsRead([])),
    messages: spyOn(api, "listIssueMessages").mockResolvedValue([] as never),
    names: spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] } as never),
    results: spyOn(api, "getIssueResults").mockResolvedValue({ checks: [] } as never),
    tree: spyOn(api, "getIssueTree").mockResolvedValue(treeOf(TWO_THREADS, STATEMENTS)),
    who: spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" } as never),
  };
}

/** The view row of the fixtures' issue at `path`, its own address unless given, under the page's
 *  providers. Each case draws it at 1440 px (`DESKTOP_WIDTH`), from 1280 px up, where the row is
 *  the row of five. */
function renderViewRow(client: QueryClient, path = presetPath(KEY, "conversation")) {
  const issue = issueDetails();
  const route = { key: issue.key, kind: "issue" } as const;
  return render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <ProvidersSlot issue={issue} route={route}>
            <ViewRowSlot issue={issue} route={route} />
          </ProvidersSlot>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Each count the row draws, by the view it is drawn for (its `data-preset-count`). */
function drawnCounts(container: HTMLElement): Record<string, string> {
  return Object.fromEntries(
    [...container.querySelectorAll<HTMLElement>("[data-preset-count]")].map((count) => [
      count.dataset.presetCount ?? "",
      count.textContent ?? "",
    ])
  );
}

/** A client as the page's tests take one: no read tried again, and none read again by its age. */
function newClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

test("the view row draws no count, unit word or flag while a read of the tree is out, and the real counts once every read has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The asks' read, held out until the case answers it.
  const asks = Promise.withResolvers<never[]>();
  reads.asks.mockReturnValue(asks.promise as never);
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(client.isFetching(), "a read other than the asks' is still out").toBe(1)
    );
    expect(client.isFetching({ queryKey: ["asks", KEY] }), "the asks' read is not out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the asks' read out").toEqual({});
    expect(
      view.container.querySelector("[data-preset-flag]"),
      "the row drew a flag with the asks' read out"
    ).toBeNull();
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the asks' read out"
    ).toBe(NAMES_ALONE);

    await act(async () => asks.resolve([]));
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts once every read had answered"
      ).toEqual(ANSWERED_COUNTS)
    );
    expect(
      view.getByRole("toolbar").textContent,
      "the Plan control drew no unit word once every read had answered"
    ).toContain(FILTER_SIGNAL_WORDS.steps);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

test("the view row draws no count while a read of the tree stands failed", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  reads.results.mockRejectedValue(new Error("the results' read failed"));
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(
        client.getQueryState(issueResultsQuery(KEY).queryKey)?.status,
        "the results' read did not fail"
      ).toBe("error")
    );
    await waitFor(() => expect(client.isFetching(), "a read is still out").toBe(0));
    expect(
      drawnCounts(view.container),
      "the row drew a count with the results' read failed"
    ).toEqual({});
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the results' read failed"
    ).toBe(NAMES_ALONE);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// Conversation counts the threads the reader began, so until the reader's login has answered it
// counts none of them: every read of the tree holding its answer is not enough on its own.
test("the view row draws no count while the reader's login is out, and the real counts once it has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The reader's login, held out until the case answers it.
  const who = Promise.withResolvers<never>();
  reads.who.mockReturnValue(who.promise as never);
  const view = renderViewRow(client);
  try {
    await waitFor(() =>
      expect(client.isFetching(), "a read other than the login's is still out").toBe(1)
    );
    expect(client.isFetching({ queryKey: ["whoami"] }), "the login's read is not out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the reader's login out").toEqual(
      {}
    );
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the reader's login out"
    ).toBe(NAMES_ALONE);

    await act(async () => who.resolve({ kind: "user", login: "alice" } as never));
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts once the reader's login had answered"
      ).toEqual(ANSWERED_COUNTS)
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// Under a words filter a plan task's or a callout's words came from the names read alone, so
// until that read answered Plan matched none of the issue's steps and drew 0 as its count, which a
// names read that failed left for good. Under one the row draws no count, unit word or flag until
// the names are held, and the real counts once they are.
// The tree read now answers the step with its words; the names read is not given its address,
// and names the issue's own key, which the root's words write.
test("under a words filter the view row draws no count while the names read is out, and the real counts once it has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The spec holds a step too.
  reads.tree.mockResolvedValue(treeOf(TWO_THREADS, [...STATEMENTS, STEP_NODE]));
  // The names read, held out until the case answers it with the step's words.
  const names = Promise.withResolvers<{ targets: ReferenceTarget[] }>();
  reads.names.mockReturnValue(names.promise as never);
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    // The names are read once every other read of the tree has answered.
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the names read out").toEqual({});
    expect(
      view.getByRole("toolbar").textContent,
      "a control drew more than its name with the names read out"
    ).toBe(NAMES_ALONE);

    await act(async () =>
      names.resolve({
        targets: [{ gone: false, kind: "block", name: "Hang the lanterns first.", ref: STEP }],
      })
    );
    await waitFor(() =>
      expect(
        drawnCounts(view.container).plan,
        "Plan did not count the step its words match once the names read had answered"
      ).toBe("1")
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// The names read is read under the set of addresses the tree holds, so a read that brings a node
// whose words cite a new address reads the names again, of a new set. The names in hand stand
// while that read is out, as the last answer the names read gave, so under a words filter the
// row keeps drawing its counts meanwhile, the new node counted among them.
test("under a words filter the view row keeps drawing its counts while the names read of a new set of addresses is out", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  reads.tree.mockResolvedValue(treeOf(TWO_THREADS, [...STATEMENTS, STEP_NODE]));
  const cited = "dispatch://CORE-7";
  // The names read of the first set answers with the step's words; the one of a set that holds
  // the address a new comment cites is held out for the rest of the case.
  const held = Promise.withResolvers<never>();
  reads.names.mockImplementation((refs: readonly string[]) =>
    refs.includes(cited)
      ? held.promise
      : (Promise.resolve({
          targets: [{ gone: false, kind: "block", name: "Hang the lanterns first.", ref: STEP }],
        }) as never)
  );
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw its counts once the names read of the first set had answered"
      ).toEqual({ conversation: "0", decisions: "0", plan: "1", spec: "0", verification: "0" })
    );

    // The comments are read again, and answer a new thread too, by the reader, whose words hold
    // the filter's word and cite an issue no other node names.
    // The tree read, which the row's turns come from, is read again with them and answers the
    // thread as a node, as the comment's event reads both.
    const comments = [
      ...TWO_THREADS,
      { ...readComment("head-c", 5), body: `Hang the lanterns as ${cited} asks.` },
    ];
    reads.comments.mockResolvedValue(comments as never);
    reads.tree.mockResolvedValue(treeOf(comments, [...STATEMENTS, STEP_NODE]));
    await act(async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ["comments", KEY] }),
        client.invalidateQueries({ queryKey: issueTreeQuery(KEY).queryKey }),
      ]);
    });
    await waitFor(() =>
      expect(
        reads.names.mock.calls.some(([refs]) => refs.includes(cited)),
        "the names were never read for the set that holds the new comment's address"
      ).toBe(true)
    );
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(
      drawnCounts(view.container),
      "the row did not draw its counts with the names read of the new set out"
    ).toEqual({ conversation: "1", decisions: "0", plan: "1", spec: "0", verification: "0" });
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// The names read is always made: the root's words write the issue's own key, which is an address
// it names. So under a words filter an issue with no block, comment or other node draws no count
// while that read is out, and its counts once it has answered.
test("under a words filter the view row of an issue that holds no node draws no count until the names read of its own key has answered", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // No document holds a block and nobody has commented: the tree is the issue alone.
  reads.tree.mockResolvedValue(treeOf([], []));
  reads.comments.mockResolvedValue([] as never);
  const own = buildDispatchReference({ key: KEY, kind: "issue" });
  // The names read, held out until the case answers it with the issue's own name.
  const names = Promise.withResolvers<{ targets: ReferenceTarget[] }>();
  reads.names.mockReturnValue(names.promise as never);
  const view = renderViewRow(client, `${presetPath(KEY, "conversation")}?q=lanterns`);
  try {
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    expect(reads.names.mock.calls, "the names read named more than the issue's own key").toEqual([
      [[own]],
    ]);
    expect(client.isFetching(), "a read other than the names' is still out").toBe(1);
    expect(drawnCounts(view.container), "the row drew a count with the names read out").toEqual({});

    await act(async () =>
      names.resolve({
        targets: [{ gone: false, kind: "issue", name: issueDetails().title, ref: own }],
      })
    );
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw its counts once the names read of the issue's own key had answered"
      ).toEqual({ conversation: "0", decisions: "0", plan: "0", spec: "0", verification: "0" })
    );
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

// With no filter on a count reads no words, so nothing waits on the names: the row draws the real
// counts while the names read is still out.
test("with no filter the view row draws the real counts while the names read is out", async () => {
  const restoreMatchMedia = stubViewport(DESKTOP_WIDTH);
  const client = newClient();
  const reads = answerReads();
  // The names read, held out for the whole case.
  reads.names.mockReturnValue(Promise.withResolvers<never>().promise);
  const view = renderViewRow(client);
  try {
    await waitFor(() => expect(reads.names, "the names were never read").toHaveBeenCalled());
    await waitFor(() =>
      expect(
        drawnCounts(view.container),
        "the row did not draw the real counts with no filter on and the names read out"
      ).toEqual(ANSWERED_COUNTS)
    );
    expect(
      client.isFetching({ queryKey: ["reference-targets"] }),
      "the names read is not out"
    ).toBe(1);
  } finally {
    view.unmount();
    restoreMatchMedia();
    for (const spy of Object.values(reads)) spy.mockRestore();
  }
});

/** A phone's width, the 390 px pictures', below the 1024 px from which the Contents pane stands
 *  beside the stage. */
const PHONE = 390;

/** The Outline control the row draws below 1024 px: the one control of the row that opens a
 *  dialog, the Outline sheet (`contents-frame.tsx`). */
const OUTLINE_CONTROL = 'button[aria-haspopup="dialog"]';

/** The view row of the fixtures' issue at `preset`'s own address, under the page's providers,
 *  with the route the page reads from that address: the issue's in Conversation and the view's
 *  own in every other, where `renderViewRow` hands the row the issue's route at every address. */
function renderRowAt(client: QueryClient, preset: Preset) {
  const issue = issueDetails();
  const route =
    preset === "conversation"
      ? ({ key: issue.key, kind: "issue" } as const)
      : ({ key: issue.key, kind: preset } as const);
  return render(
    <MemoryRouter initialEntries={[presetPath(KEY, preset)]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <ProvidersSlot issue={issue} route={route}>
            <ViewRowSlot issue={issue} route={route} />
          </ProvidersSlot>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// R13: below 1024 px the row draws the Outline control in Conversation, in each filter and in
// Graph, and it opens the Contents list as a sheet, which holds the list's entries and not the
// three sentences, since there the view draws them above its first row. From 1024 px up the pane
// stands beside each of those views and the row draws no Outline control.
test("below 1024 px the view row draws the Outline control in Conversation, each filter and Graph, and its sheet draws the Contents list", async () => {
  const user = userEvent.setup();
  for (const width of [PHONE, DESKTOP_WIDTH]) {
    const restoreMatchMedia = stubViewport(width);
    const reads = answerReads();
    try {
      for (const preset of PRESET_ORDER) {
        const where = `with ${PRESET_NAMES[preset]} on at ${width} px`;
        const client = newClient();
        const view = renderRowAt(client, preset);
        try {
          const control = view.container.querySelector<HTMLElement>(OUTLINE_CONTROL);
          if (width === DESKTOP_WIDTH) {
            expect(control, `the row draws the Outline control ${where}`).toBeNull();
            continue;
          }
          expect(control, `the row draws no Outline control ${where}`).not.toBeNull();
          await user.click(control as HTMLElement);
          const sheet = () => document.querySelector('[data-testid="issue-outline-sheet"]');
          await waitFor(() =>
            expect(
              sheet()?.querySelectorAll('[role="tree"] [data-contents-entry]').length,
              `the Outline sheet does not list the reader's two threads ${where}`
            ).toBe(2)
          );
          expect(
            sheet()?.querySelector('[data-testid="issue-three-things"]'),
            `the Outline sheet draws the three sentences ${where}`
          ).toBeNull();
        } finally {
          view.unmount();
          client.clear();
        }
      }
    } finally {
      restoreMatchMedia();
      for (const spy of Object.values(reads)) spy.mockRestore();
    }
  }
});

/** The issue's own node as a filter loaded cold with no outline read draws it: no node under it,
 *  and beside it the Spec view answer `spec` where one is held. */
function coldRoot(spec?: TreeAnswer): TreeNode {
  return {
    alsoUnder: [],
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id: KEY,
    question: false,
    replaced: false,
    text: KEY,
    transcluded: false,
    type: "issue",
    waiting: false,
    ...(spec === undefined ? {} : { views: { spec } }),
  };
}

// R4 and R13: Spec loaded cold below 1024 px sends its view read and no outline read, so the
// outline's reads never all hold; its count is its view answer's own statements, and it draws
// that count once the answer is held, as no other control draws one.
test("a filter loaded cold with no outline read draws its own count from its view answer, and no other control a count", () => {
  const spec = treeAnswer([
    treeNode(KEY, "issue", null),
    ...["r1", "r2"].map((id, position) =>
      treeNode(id, "requirement", KEY, {
        document: { artifact_id: SPEC_DOCUMENT.id, position, slug: SPEC_SLUG },
        role: "query",
      })
    ),
  ]);
  const cold = { filter: "", namesHeld: true, readsHeld: false, viewer: "alice" } as const;
  expect(
    viewRowSignals({ ...cold, preset: "spec", root: coldRoot(spec) }),
    "Spec loaded cold does not count its view answer's statements"
  ).toEqual({ spec: { count: 2 } });
  expect(
    viewRowSignals({ ...cold, preset: "spec", root: coldRoot() }),
    "Spec loaded cold draws a count before its view answer is held"
  ).toBeUndefined();
  expect(
    viewRowSignals({ ...cold, preset: "conversation", root: coldRoot(spec) }),
    "Conversation draws a count with the outline's reads out"
  ).toBeUndefined();
});
