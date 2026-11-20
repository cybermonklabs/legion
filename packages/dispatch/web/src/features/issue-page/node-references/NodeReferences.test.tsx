import { afterEach, beforeEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../../__tests__/comment-fixture";
import { graphNode } from "../../../__tests__/graph-fixture";
import { api } from "../../../api/client";
import type { CommentWithTrees, GraphEdge, GraphReferences } from "../../../api/types";
import { emulateChromiumFocusFixup } from "../chromium-focus-fixup";
import { MarginSlotProvider } from "../margin-slot";
import { ANSWER_LINES, CONTROL_LABELS, NODE_REFERENCES_WORDS } from "../page-words";
import { NodeReferences } from "./NodeReferences";

const STATEMENT = "dispatch://LAY-2/spec#b-r1";

const ANSWER: GraphReferences = {
  edges: [
    {
      created_at: "2026-11-15T10:00:00Z",
      direction: "in",
      excerpt: { text: "Thread 1 opens here. Its second paragraph is a block of its own." },
      kind: "mentions",
      node: graphNode({
        id: "11111111-1111-4111-8111-111111111111",
        issue_key: "LAY-2",
        kind: "comment",
        ref: "dispatch://LAY-2/comment/11111111-1111-4111-8111-111111111111",
      }),
      source_seq: null,
    },
    {
      created_at: "2026-11-15T09:00:00Z",
      direction: "in",
      excerpt: {
        block_id: "c1",
        text:
          ':::check{#c1 serves="dispatch://LAY-2/spec#b-r1"}\n' +
          "Check 1 of the redesign holds its own row and its own state.\n" +
          ":::",
      },
      kind: "serves",
      node: graphNode({
        block_id: "c1",
        document: "dispatch://LAY-2/artifact/verification-md",
        id: "verification-md#c1",
        issue_key: "LAY-2",
        kind: "block",
        ref: "dispatch://LAY-2/artifact/verification-md#b-c1",
      }),
      source_seq: null,
    },
  ],
  node: graphNode({
    block_id: "r1",
    id: "spec#r1",
    issue_key: "LAY-2",
    kind: "block",
    ref: STATEMENT,
  }),
};

/** The comment `ANSWER` names first, as this issue's comments read answers it: the outline has
 *  read it before Details opens. Its body is the markdown its author typed, and its reading the
 *  words its row in the outline draws. */
const COMMENTS: readonly CommentWithTrees[] = [
  {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "Thread **1** opens here.\n\nIts second paragraph is a block of its own.",
    body_tree: {
      content: [
        {
          content: [
            { text: "Thread ", type: "text" },
            { marks: [{ type: "strong" }], text: "1", type: "text" },
            { text: " opens here.", type: "text" },
          ],
          type: "paragraph",
        },
        {
          content: [{ text: "Its second paragraph is a block of its own.", type: "text" }],
          type: "paragraph",
        },
      ],
      type: "doc",
    },
    created_at: "2026-11-15T10:00:00Z",
    edited_at: null,
    id: "11111111-1111-4111-8111-111111111111",
    issue_key: "LAY-2",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  },
];

// A reference's day is drawn without its year when it is the reader's year (`node-time.ts`), so
// "14 Nov" reads the clock. Each case runs at one fixed midday of the fixtures' year, so none
// reads the wall clock and the days the cases read hold after that year ends. `setSystemTime`
// holds `Date` alone; the timers the cases wait on run on the real clock as before.
const NOON = new Date(2026, 10, 19, 12, 0, 0, 0);
beforeEach(() => setSystemTime(NOON));
afterEach(() => setSystemTime());

/** `hour` o'clock of the reader's own `day` of November 2026, as the read writes a moment. A
 *  reference's day is the reader's local day, so a case that names it builds the moment here: a
 *  UTC instant falls on another local day in a zone far from UTC. */
function november(day: number, hour: number): string {
  return new Date(2026, 10, day, hour).toISOString();
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

/** The props of one drawing: the row the reader is on and its own address. */
interface Drawn {
  readonly node: { readonly id: string; readonly type: "issue" | "requirement" };
  readonly reference: string | undefined;
}

const STATEMENT_ROW: Drawn = {
  node: { id: "spec#b-r1", type: "requirement" },
  reference: STATEMENT,
};

/** The component inside the shell it is drawn in: Details is the margin's drawer, open or
 *  shut, and the panel is inside it (`features/margin/margin-slot.tsx`). The outline has read
 *  the issue's comments under their key (`useIssueTree.ts`), and the item reads are watched so a
 *  case can say the list never asks for one. */
function draw(detailsOpen: boolean, drawn: Drawn = STATEMENT_ROW, answer = ANSWER) {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(answer);
  const itemReads = [
    spyOn(api, "listIssueAsks").mockResolvedValue([]),
    spyOn(api, "listComments").mockResolvedValue([]),
    spyOn(api, "listIssueMessages").mockResolvedValue([]),
  ];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["comments", "LAY-2"], COMMENTS);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginSlotProvider drawer={{ open: detailsOpen, setOpen: () => {}, setPage: () => {} }}>
          <NodeReferences node={drawn.node} reference={drawn.reference} />
        </MarginSlotProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  restore = () => {
    getReferences.mockRestore();
    for (const read of itemReads) read.mockRestore();
    view.unmount();
    client.clear();
  };
  return { client, getReferences, itemReads, view };
}

test("the first drawing of a focused row reads nothing", async () => {
  const { getReferences } = draw(true);
  expect(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  ).toBeDefined();
  expect(getReferences.mock.calls).toHaveLength(0);
});

test("the reader opening the list reads it once, and every node that references the row is a link to it", async () => {
  const { getReferences, itemReads } = draw(true);
  const control = await screen.findByRole("button", {
    name: NODE_REFERENCES_WORDS.what_references,
  });
  await userEvent.click(control);
  expect(getReferences.mock.calls).toEqual([[STATEMENT]]);
  expect(control.getAttribute("aria-expanded")).toBe("true");

  // The mention, which the outline draws nowhere, by the first line of the comment's words as
  // the outline's read of it holds them, and the check of the Verification document by the
  // word of its tie.
  const mention = await screen.findByRole("link", {
    name: "Thread 1 opens here.",
  });
  expect(mention.getAttribute("href")).toBe(
    "/issues/LAY-2/comments/11111111-1111-4111-8111-111111111111"
  );
  const check = screen.getByRole("link", { name: NODE_REFERENCES_WORDS.edges.serves });
  expect(check.getAttribute("href")).toBe("/issues/LAY-2/artifacts/verification-md#b-c1");
  // What each edge is, in the page's own words: a tuple of the model by its own word, a
  // mention by the kind of node that holds it.
  const list = screen.getByRole("list");
  expect(list.textContent).toContain(NODE_REFERENCES_WORDS.edges.serves);
  expect(list.textContent).toContain(NODE_REFERENCES_WORDS.kinds.comment);
  // The comment's words are the outline's read, which the list never asks for.
  expect(itemReads.map((read) => read.mock.calls.length)).toEqual([0, 0, 0]);
});

test("a list the reader opens while Details is shut reads nothing", async () => {
  const { getReferences } = draw(false);
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  expect(getReferences.mock.calls).toHaveLength(0);
  expect(screen.queryByRole("list")).toBeNull();
});

test("the focus moving to another row closes the list, so the next row costs no read", async () => {
  const { getReferences, view } = draw(true);
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  expect(getReferences.mock.calls).toHaveLength(1);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onRow = (drawn: Drawn) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MarginSlotProvider drawer={{ open: true, setOpen: () => {}, setPage: () => {} }}>
          <NodeReferences node={drawn.node} reference={drawn.reference} />
        </MarginSlotProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  view.rerender(
    onRow({
      node: { id: "spec#b-r2", type: "requirement" },
      reference: "dispatch://LAY-2/spec#b-r2",
    })
  );
  const control = await screen.findByRole("button", {
    name: NODE_REFERENCES_WORDS.what_references,
  });
  expect(control.getAttribute("aria-expanded")).toBe("false");
  expect(getReferences.mock.calls).toHaveLength(1);
  // The reader coming back to the row they opened it on finds it closed too: the row it
  // was opened on was let go of as the focus left it.
  view.rerender(onRow(STATEMENT_ROW));
  expect(
    screen
      .getByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
      .getAttribute("aria-expanded"),
    "the list opened again as the focus came back to its row"
  ).toBe("false");
  expect(getReferences.mock.calls).toHaveLength(1);
});

test("the issue itself draws no list here, because Details' References row holds it", () => {
  const { getReferences } = draw(true, {
    node: { id: "LAY-2", type: "issue" },
    reference: "dispatch://LAY-2",
  });
  expect(screen.queryByRole("button", { name: NODE_REFERENCES_WORDS.what_references })).toBeNull();
  expect(getReferences.mock.calls).toHaveLength(0);
});

test("a row the model gives no address draws no list", () => {
  const { getReferences } = draw(true, {
    node: { id: "spec#b-r1", type: "requirement" },
    reference: undefined,
  });
  expect(screen.queryByRole("button", { name: NODE_REFERENCES_WORDS.what_references })).toBeNull();
  expect(getReferences.mock.calls).toHaveLength(0);
});

// With the list held, a failed read of it keeps its line and its Retry drawn while that Retry
// reads it again, so the Retry is never disabled then: Chromium would drop its focus to the page
// (`chromium-focus-fixup.ts`), and a read that failed again would leave it there.
test("the references' Retry pressed by keyboard over the list held keeps the focus when its read fails again", async () => {
  const { client, getReferences } = draw(true);
  const restoreFocus = emulateChromiumFocusFixup();
  const key = ["references", "to", STATEMENT];
  try {
    await userEvent.click(
      await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
    );
    await screen.findByRole("link", { name: "Thread 1 opens here." });
    getReferences.mockRejectedValueOnce(new Error("the references' read failed"));
    await act(() => client.invalidateQueries({ queryKey: key }));
    const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<GraphReferences>();
    getReferences.mockImplementationOnce(() => again.promise);

    retry.focus();
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(3));
    expect(
      document.activeElement,
      "the references' Retry left the focus on the page while it read"
    ).toBe(retry);

    again.reject(new Error("the references' read failed again"));
    await waitFor(() => expect(client.getQueryState(key)?.fetchStatus).toBe("idle"));
    expect(screen.getByRole("alert").textContent).toBe(ANSWER_LINES.references_unreadable);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.retry })).toBe(retry);
    expect(
      document.activeElement,
      "the references' Retry whose read failed again left the focus on the page"
    ).toBe(retry);
    expect(screen.queryByRole("link", { name: "Thread 1 opens here." })).not.toBeNull();
  } finally {
    restoreFocus();
  }
});

// A second press of Retry made before React draws the read the first one sent found the render's
// `isFetching` still false, so it read the list again: a refetch of a read that holds an answer
// ends the request that is out and sends another. A press while the read is out joins it.
test("two presses of the references' Retry before a render send one read", async () => {
  const { client, getReferences } = draw(true);
  const key = ["references", "to", STATEMENT];
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  await screen.findByRole("link", { name: "Thread 1 opens here." });
  getReferences.mockRejectedValueOnce(new Error("the references' read failed"));
  await act(() => client.invalidateQueries({ queryKey: key }));
  const retry = await screen.findByRole("button", { name: CONTROL_LABELS.retry });
  // Every read sent from here is held out until the case lets it land.
  const again = Promise.withResolvers<GraphReferences>();
  getReferences.mockImplementation(() => again.promise);

  // Both presses in one task inside one `act`, so React draws nothing between them.
  act(() => {
    retry.click();
    retry.click();
  });
  expect(
    getReferences,
    "the second press ended the read the first one sent and sent another"
  ).toHaveBeenCalledTimes(3);
  await waitFor(() => expect(retry).toHaveAttribute("aria-busy", "true"));

  await act(async () => again.resolve(ANSWER));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(getReferences).toHaveBeenCalledTimes(3);
});

// A first read of the list that failed holds no answer, and a fetch of such a read clears its
// failure as it starts, so the press of its Retry took the line, and the Retry with it, away while
// the read it sent was out: the focus of a reader who pressed Retry by keyboard went with it. The
// line stays, its Retry busy and holding the focus, through a read that fails again, and once a
// read answers the focus goes to the control that opens the list.
test("a first read of the references that failed keeps its line and its Retry, with the focus, while Retry reads again", async () => {
  const { getReferences } = draw(true);
  getReferences.mockRejectedValueOnce(new Error("the references' read failed"));
  const restoreFocus = emulateChromiumFocusFixup();
  try {
    const control = await screen.findByRole("button", {
      name: NODE_REFERENCES_WORDS.what_references,
    });
    await userEvent.click(control);
    const failed = await screen.findByRole("alert");
    expect(failed.textContent).toBe(ANSWER_LINES.references_unreadable);
    const retry = screen.getByRole("button", { name: CONTROL_LABELS.retry });
    // The read the Retry sends is held out until the case fails it.
    const again = Promise.withResolvers<GraphReferences>();
    getReferences.mockImplementationOnce(() => again.promise);

    act(() => retry.focus());
    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(getReferences).toHaveBeenCalledTimes(2));
    await waitFor(() => {
      expect(
        retry,
        "the line and its Retry went while the read Retry sent again is out"
      ).toBeInTheDocument();
      expect(retry, "Retry does not read busy while the read it sent again is out").toHaveAttribute(
        "aria-busy",
        "true"
      );
    });
    expect(retry, "Retry turned disabled while the read it sent again is out").toBeEnabled();
    expect(document.activeElement, "Retry lost the focus while the read it sent again is out").toBe(
      retry
    );
    expect(
      screen.queryByText(ANSWER_LINES.references_loading),
      "the list says it is loading beside the failure its Retry reads again"
    ).toBeNull();

    await act(async () => again.reject(new Error("the references' read failed")));
    await waitFor(() =>
      expect(retry, "Retry still reads busy after the read failed again").not.toHaveAttribute(
        "aria-busy"
      )
    );
    const failedAgain = screen.getByRole("alert");
    expect(failedAgain.textContent).toBe(ANSWER_LINES.references_unreadable);
    expect(
      failedAgain,
      "the second failure was not drawn afresh, so a screen reader would not say it"
    ).not.toBe(failed);
    expect(
      screen.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the line came back with another Retry"
    ).toBe(retry);
    expect(document.activeElement, "Retry lost the focus when the read failed again").toBe(retry);

    await userEvent.keyboard("{Enter}");
    await screen.findByRole("link", { name: "Thread 1 opens here." });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(
      document.activeElement,
      "the list answered, and the focus fell to the page rather than to the control that opens it"
    ).toBe(control);
  } finally {
    restoreFocus();
  }
});

// During a release a newer Dispatch can answer with an edge kind, or a node kind at the other
// end, that this page does not know. The list leaves such an edge out, as the Referenced-by panel
// does (`drawsEdge`), rather than draw it under the word a mention takes.
test("an edge of a kind or to a node this page does not know is left out", async () => {
  const [mention, check] = ANSWER.edges ?? [];
  if (mention === undefined || check === undefined) throw new Error("the answer lost its edges");
  draw(true, STATEMENT_ROW, {
    ...ANSWER,
    edges: [
      mention,
      { ...check, kind: "verifies" } as unknown as GraphEdge,
      {
        ...mention,
        node: { id: "future-1", kind: "future", ref: "dispatch://LAY-2/future/future-1" },
      } as unknown as GraphEdge,
    ],
  });
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  await screen.findByRole("link", { name: "Thread 1 opens here." });
  expect(screen.getAllByRole("listitem")).toHaveLength(1);
});

// The graph resolves a mention in a comment's words to that comment's block, with the comment
// as its parent and no document. The list reads it as the comment it is: named "comment", not
// "document", opened at the comment, once however many of its blocks hold the mention. A citing
// node of another issue is named by that word alone, says which issue, and each row says when
// the reference was made.
test("a comment's block that mentions the row reads as that comment, with where it is from and when", async () => {
  const block = (id: string, text: string): GraphEdge => ({
    created_at: november(14, 10),
    direction: "in",
    excerpt: { text },
    kind: "mentions",
    node: graphNode({
      id,
      issue_key: "LAY-9",
      kind: "block",
      parent: {
        id: "22222222-2222-4222-8222-222222222222",
        kind: "comment",
        ref: "dispatch://LAY-9/comment/22222222-2222-4222-8222-222222222222",
      },
      ref: `dispatch://LAY-9/comment/22222222-2222-4222-8222-222222222222#${id}`,
    }),
    source_seq: null,
  });
  const [mention] = ANSWER.edges ?? [];
  if (mention === undefined) throw new Error("the answer lost its edges");
  draw(true, STATEMENT_ROW, {
    ...ANSWER,
    edges: [
      block("b1", "It cites the statement from another issue."),
      block("b2", "Twice."),
      { ...mention, created_at: november(15, 10) },
    ],
  });
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  const link = await screen.findByRole("link", { name: NODE_REFERENCES_WORDS.kinds.comment });
  expect(link.getAttribute("href")).toBe(
    "/issues/LAY-9/comments/22222222-2222-4222-8222-222222222222"
  );
  const items = screen.getAllByRole("listitem");
  expect(items).toHaveLength(2);
  const text = (item: HTMLElement | undefined) => (item?.textContent ?? "").replace(/\s+/g, " ");
  expect(text(items[0])).toContain(NODE_REFERENCES_WORDS.kinds.comment);
  expect(text(items[0])).not.toContain(NODE_REFERENCES_WORDS.kinds.block);
  // The other issue's key and the day the reference was made; a node of this issue names none.
  expect(items[0]?.querySelector("[data-reference-source]")?.textContent).toBe("LAY-9");
  expect(items[0]?.querySelector("[data-reference-time]")?.textContent).toBe("14 Nov");
  expect(items[1]?.querySelector("[data-reference-source]")).toBeNull();
  expect(items[1]?.querySelector("[data-reference-time]")?.textContent).toBe("15 Nov");
});

// The read's excerpt is the citing node's stored text, the markdown its author typed with the
// addresses it cites, and the page holds no words of a node of another issue. Such a node is
// named by its word, with its issue and its time.
test("a citing node of another issue is drawn by its kind's word with its issue and its time, never its excerpt", async () => {
  const stored = "The **rail** follows [the statement](dispatch://LAY-2/spec#b-r1) closely.";
  const elsewhere: GraphEdge = {
    created_at: november(13, 10),
    direction: "in",
    excerpt: { block_id: "r4", text: stored },
    kind: "mentions",
    node: graphNode({
      block_id: "r4",
      document: "dispatch://CORE-2/spec",
      id: "spec#r4",
      issue_key: "CORE-2",
      kind: "block",
      ref: "dispatch://CORE-2/spec#b-r4",
      text: stored,
    }),
    source_seq: null,
  };
  draw(true, STATEMENT_ROW, { ...ANSWER, edges: [elsewhere] });
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  const [item, ...more] = await screen.findAllByRole("listitem");
  expect(more).toHaveLength(0);
  const text = (item?.textContent ?? "").replace(/\s+/g, " ");
  for (const held of ["dispatch://", "**", "rail", "follows", "statement", "closely"]) {
    expect(text, `the row draws "${held}" of the excerpt`).not.toContain(held);
  }
  expect(item?.querySelector("[data-reference-source]")?.textContent).toBe("CORE-2");
  expect(item?.querySelector("[data-reference-time]")?.textContent).toBe("13 Nov");
  // The word is the link, which names the node.
  expect(screen.getByRole("link", { name: NODE_REFERENCES_WORDS.kinds.block })).toBeDefined();
});

// A node the read gives no words for - no excerpt and no text of its own - is named by its
// word alone, which is then its link. Its address, the slug of its document and its block id
// are no name.
test("a node the read gives no words for is drawn by its word alone, with no address", async () => {
  const wordless: GraphEdge = {
    created_at: "2026-11-15T08:00:00Z",
    direction: "in",
    kind: "serves",
    node: graphNode({
      block_id: "c2",
      document: "dispatch://LAY-2/artifact/verification-md",
      id: "verification-md#c2",
      issue_key: "LAY-2",
      kind: "block",
      ref: "dispatch://LAY-2/artifact/verification-md#b-c2",
    }),
    source_seq: null,
  };
  draw(true, STATEMENT_ROW, { ...ANSWER, edges: [wordless] });
  await userEvent.click(
    await screen.findByRole("button", { name: NODE_REFERENCES_WORDS.what_references })
  );
  const link = await screen.findByRole("link", { name: NODE_REFERENCES_WORDS.edges.serves });
  expect(link.getAttribute("href")).toBe("/issues/LAY-2/artifacts/verification-md#b-c2");
  const items = screen.getAllByRole("listitem");
  expect(items).toHaveLength(1);
  const text = (items[0]?.textContent ?? "").replace(/\s+/g, " ").trim();
  for (const name of ["dispatch://", "verification-md", "verification", "c2", "#b-", "LAY-2"]) {
    expect(text, `the row names the node by "${name}"`).not.toContain(name);
  }
  // The word is drawn once, as the link, and not beside it as well.
  expect(text.startsWith(NODE_REFERENCES_WORDS.edges.serves)).toBe(true);
  expect(text.split(NODE_REFERENCES_WORDS.edges.serves)).toHaveLength(2);
});
