import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { ApiError, api } from "../../api/client";
import type { Artifact, DecisionKeep } from "../../api/types";
import {
  borderDefault,
  dangerText,
  pageFailedText,
  pageLineBorder,
  pageMutedText,
  pageWordsText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ChangeDraftsProvider, DecisionActions } from "./DecisionActions";
import { NoTooltips } from "./NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import {
  issueTreeKey,
  issueTreeQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  viewKey,
} from "./tree-read/tree-read";

/**
 * The two controls a decision's node draws (R7). The tree is one tab stop,
 * so neither takes the tab order and the row's own keys press them (`useTreeKeymap.ts`); what
 * this file reads is what the controls themselves do, which is what both a pointer and those
 * keys get. Each case holds the read the page holds, the tree read's answer, under the key the
 * page reads it by, and the node finds its decision there without a request of its own.
 */

const KEY = "CORE-1";
const REFERENCE = `dispatch://${KEY}/artifact/decisions-md#b-d1`;

const document_: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-decisions",
  issue_key: KEY,
  kind: "doc",
  name: "decisions.md",
  primary: false,
  project: "CORE",
  ref_key: "artifact:artifact-decisions",
  session_id: null,
  slug: "decisions-md",
  versions: [],
};

/**
 * A decision node of `document_`, as the tree read answers one: an
 * agent's decision that waits for review, unless `extra` says otherwise. Its first paragraph is
 * the heading every accessible name reads and the words a Change is anchored to, so a decision
 * with one is a decision whose Change control is drawn.
 */
function decisionNode(
  id: string,
  text: string,
  extra: Partial<TreeAnswerNode> = {}
): TreeAnswerNode {
  return {
    asks: [],
    attributes: {},
    author: [],
    changed_at: null,
    changed_version: 1,
    document: { artifact_id: document_.id, position: 1, slug: document_.slug },
    id,
    kept: null,
    kind: "agent",
    links: [],
    made_at: "2026-11-05T00:00:00Z",
    parent: KEY,
    rank: null,
    replaced: false,
    state: "live",
    supertag: "decision",
    text,
    text_sha256: `sum-${id}`,
    unsure: false,
    visibility: "project",
    waiting_review: true,
    ...extra,
  };
}

/** The decision at `REFERENCE`. */
const ONE = decisionNode(
  "d1",
  "One tree, five presets\n\nThe presets are queries over the one tree."
);

/** A client that holds what the page holds: the tree read's answer of `nodes`, on one page. It
 *  is not fresh, so an enabled observer would read it again as it mounts. */
function heldReads(nodes: readonly TreeAnswerNode[]): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(issueTreeKey(KEY), {
    converted: false,
    cursor: null,
    nodes: [...nodes],
    tree: null,
    tuples: [],
    view_of: null,
  } satisfies TreeAnswer);
  return client;
}

/** The node at `REFERENCE` among the tree's `nodes`, drawn in a row of `rowDocument`; inside the
 *  page's `NoTooltips` where `page` says so, as the issue page draws them. */
function renderActions(
  nodes: readonly TreeAnswerNode[] = [ONE],
  rowDocument: Artifact = document_,
  { page = false }: { page?: boolean } = {}
): { client: QueryClient; unmount: () => void } {
  const client = heldReads(nodes);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      {page ? <NoTooltips>{children}</NoTooltips> : children}
    </QueryClientProvider>
  );
  const view = render(
    <DecisionActions document={rowDocument} isClosed={false} nodeReference={REFERENCE} />,
    { wrapper }
  );
  return { client, unmount: () => view.unmount() };
}

// R7, WCAG 2.1.1: a reader who asked to change a decision is there to type. The field takes the
// caret as it mounts, so the caret is in it on the commit that draws the form - no frame of its
// own, and no second form on the page to find it among - whether the Change was a pointer's or
// the row's own key (`useTreeKeymap.ts`'s `tree-change`, which presses this very control).
test("the change form opens with the caret in its field", async () => {
  const user = userEvent.setup();
  const { unmount } = renderActions();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field.tagName).toBe("TEXTAREA");
    expect(
      document.activeElement,
      "the form opened with the caret somewhere other than the field a reader opened it to type in"
    ).toBe(field);
  } finally {
    unmount();
  }
});

/** The decision's node body as a view draws it, under the view's `ChangeDraftsProvider`:
 *  `close` takes the body away, as the focus leaving the rows does, and `open` draws it again. */
function renderBody(): { close: () => void; open: () => void; unmount: () => void } {
  const client = heldReads([ONE]);
  const body = (drawn: boolean) => (
    <QueryClientProvider client={client}>
      <ChangeDraftsProvider>
        {drawn ? (
          <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
        ) : null}
      </ChangeDraftsProvider>
    </QueryClientProvider>
  );
  const view = render(body(true));
  return {
    close: () => view.rerender(body(false)),
    open: () => view.rerender(body(true)),
    unmount: () => view.unmount(),
  };
}

// A Change being written survives the body closing: the form and its words come back with the
// body, and the field takes no caret, because the reader came back to the row, not into it.
test("a Change being written comes back with its words when the body opens again", async () => {
  const user = userEvent.setup();
  const body = renderBody();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    body.close();
    expect(screen.queryByLabelText(CONTROL_LABELS.your_change)).toBeNull();
    body.open();
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(
      field instanceof HTMLTextAreaElement ? field.value : "",
      "the words typed before the body closed did not come back"
    ).toBe("Keep one preset.");
    expect(
      screen
        .getByRole("button", { name: CONTROL_LABELS.change_decision })
        .getAttribute("aria-expanded")
    ).toBe("true");
    expect(document.activeElement, "the form that came back took the caret").not.toBe(field);
  } finally {
    body.unmount();
  }
});

// A Change sent and left before Dispatch answers comes back closed: Send cannot be pressed
// again while the first comment is out, so the change is not written twice.
test("a Change sent and left before Dispatch answers comes back closed", async () => {
  // Dispatch never answers the send.
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const user = userEvent.setup();
  const body = renderBody();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    body.close();
    body.open();
    expect(
      screen.queryByLabelText(CONTROL_LABELS.your_change),
      "the sent Change came back open, with Send to press again"
    ).toBeNull();
    expect(
      screen
        .getByRole("button", { name: CONTROL_LABELS.change_decision })
        .getAttribute("aria-expanded")
    ).toBe("false");
  } finally {
    body.unmount();
    createComment.mockRestore();
  }
});

// A Change that Dispatch refuses keeps its words: the refusal gives them back to the draft, so a
// body that closes after it opens again with the Change and the words the reader wrote.
test("a Change that Dispatch refuses keeps its words when the body closes and opens again", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "QUOTE_NOT_FOUND", error: "the quote is not in the document" })
  );
  const user = userEvent.setup();
  const body = renderBody();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await screen.findByRole("alert");
    expect(createComment).toHaveBeenCalledTimes(1);
    body.close();
    body.open();
    const field = screen.queryByLabelText(CONTROL_LABELS.your_change);
    expect(field, "the refused Change came back closed, its words lost").not.toBeNull();
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Keep one preset.");
  } finally {
    body.unmount();
    createComment.mockRestore();
  }
});

// Decision 92: a refused send says so in the page's own words and never in the server's, which
// name what the reader cannot act on - here a quote that matches two places of the document -
// and the words the reader wrote stay in the field, to send again.
test("a refused send shows the plain words and keeps the draft", async () => {
  const refusal =
    "anchor quote matches 2 places; pass a zero-based occurrence to choose one of them";
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "QUOTE_AMBIGUOUS", error: refusal })
  );
  const user = userEvent.setup();
  const body = renderBody();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(ANSWER_LINES.change_not_sent);
    expect(document.body.textContent, "the row draws the server's own words").not.toContain(
      "occurrence"
    );
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Keep one preset.");
  } finally {
    body.unmount();
    createComment.mockRestore();
  }
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The colours of the Change form's lines on each surface: `main`'s on today's page, and the new
 *  page's own pairs inside `NoTooltips`. */
const TONES = {
  main: {
    failed: dangerText,
    muted: textMutedOnCanvas,
    rule: borderDefault,
    words: textSecondaryOnCanvas,
  },
  page: {
    failed: pageFailedText,
    muted: pageMutedText,
    rule: pageLineBorder,
    words: pageWordsText,
  },
} as const;

/** Which of `TONES` each line of the Change form draws. */
const LINE_TONES = {
  cancel: ["rule", "muted"],
  name: ["muted"],
  quote: ["rule", "words"],
  refusal: ["failed"],
  send: ["rule", "muted"],
} as const;

/** Whether `element` carries every class each of `classes` names. */
function wears(element: Element, ...classes: readonly string[]): boolean {
  return classes.every((one) =>
    one.split(/\s+/u).every((token) => element.classList.contains(token))
  );
}

// Inside `NoTooltips`, the new page, the Change form's lines draw the page's own colour pairs,
// the ones `theme/classes.ts` names; on today's page they keep `main`'s: the quote's rule and
// words, the field's name, a refusal, and Send and Cancel.
test("the Change form draws the page's colours inside NoTooltips and main's outside it", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "QUOTE_NOT_FOUND", error: "the quote is not in the document" })
  );
  try {
    for (const surface of ["page", "main"] as const) {
      const own = TONES[surface];
      const other = TONES[surface === "page" ? "main" : "page"];
      const user = userEvent.setup();
      const { unmount } = renderActions([ONE], document_, { page: surface === "page" });
      try {
        await user.click(
          await screen.findByRole("button", { name: CONTROL_LABELS.change_decision })
        );
        await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
        await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
        const lines = {
          cancel: screen.getByRole("button", { name: CONTROL_LABELS.cancel }),
          name: screen.getByText(CONTROL_LABELS.your_change),
          quote: screen.getByTestId("decision-quote"),
          refusal: await screen.findByRole("alert"),
          send: screen.getByRole("button", { name: CONTROL_LABELS.send_change }),
        };
        for (const [line, element] of Object.entries(lines)) {
          const tones = LINE_TONES[line as keyof typeof LINE_TONES];
          expect(
            wears(element, ...tones.map((tone) => own[tone])),
            `on the ${surface} surface the form's ${line} lacks its colours`
          ).toBe(true);
          expect(
            tones.some((tone) => wears(element, other[tone])),
            `on the ${surface} surface the form's ${line} draws the other surface's colours`
          ).toBe(false);
        }
      } finally {
        unmount();
      }
    }
  } finally {
    createComment.mockRestore();
  }
});

/** The quote the Change form offers for the one decision `fields` describe, which waits for
 *  review so its node draws the Change. */
async function quoteOf(fields: Pick<TreeAnswerNode, "text" | "text_tree">): Promise<string> {
  const user = userEvent.setup();
  const { unmount } = renderActions([decisionNode(ONE.id, fields.text, fields)]);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    return screen.getByTestId("decision-quote").textContent ?? "";
  } finally {
    unmount();
  }
}

// A Change anchors on the words of the decision's first block, which the server finds by
// matching them against the document's own flattened text: that text writes one space wherever
// two text runs are not adjacent in the document's positions (`pmdoc.buildFlattenedText`). The
// end of one list item and the start of the next are four positions apart, so the quote the
// form sends must hold that space too, or `pmdoc.FindQuote` finds nothing to anchor on. The
// words come from the reading the server sent (the node's `text_tree`), which the test builds
// with the page's own engine (`__tests__/display-trees`).
test("the quote of a first block that is a list holds one space between its items", async () => {
  const text = "## Name them notices\n\n- **alpha**\n- beta\n\nThat is the whole of it.";
  expect(await quoteOf({ text, text_tree: displayTree(text) })).toBe("alpha beta");
});

// The same rule at a hard break: the break is a leaf of its own and takes a position, so the
// two lines it divides are one space apart in the text the server matches a quote against.
test("the quote of a first block with a hard break holds one space between its lines", async () => {
  const text = "## Wrap the lines\n\nline one  \nline two\n\nThat is the whole of it.";
  expect(await quoteOf({ text, text_tree: displayTree(text) })).toBe("line one line two");
});

// An answer with no reading of its text comes from a pod of the build before during a rolling
// deploy, and the Change still has a quote there: the first paragraph's own markdown, which is
// all the browser has.
test("a decision whose answer carried no reading quotes its first paragraph's markdown", async () => {
  const text = "## Keep a log\n\nA log is enough.\n\nUnsure: the index may be slow.";
  expect(await quoteOf({ text, text_tree: undefined })).toBe("A log is enough.");
});

/** A document of the issue other than the one the decision is a block of. */
const SPEC_DOCUMENT: Artifact = {
  ...document_,
  id: "artifact-spec",
  name: "spec.md",
  ref_key: "artifact:artifact-spec",
  slug: "spec-md",
};

// A Change is a comment anchored to the decision node's own document, whatever document the row
// that draws it stands in, to the node's own block and to the words of its first paragraph: the
// anchor names no place among the quote's matches, and the send reads no document's text before
// it goes.
test("Change anchors to the node's document and quotes its first paragraph", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const text = spyOn(api, "getArtifactText");
  const user = userEvent.setup();
  const { unmount } = renderActions([ONE], SPEC_DOCUMENT);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() =>
      expect(createComment, "the Change was never sent").toHaveBeenCalledTimes(1)
    );
    // Compared as `unknown`: the API's `AnchorInput` does not declare the `block_id` it carries.
    expect<unknown>(createComment.mock.calls[0]?.slice(0, 2)).toEqual([
      KEY,
      {
        anchor: { artifact: document_.id, block_id: ONE.id, quote: "One tree, five presets" },
        body: "Say where.",
      },
    ]);
    expect(
      createComment.mock.calls[0]?.[1]?.anchor,
      "the Change named a place among the quote's matches"
    ).not.toHaveProperty("occurrence");
    expect(text, "the send read the document's text").not.toHaveBeenCalled();
  } finally {
    unmount();
    createComment.mockRestore();
    text.mockRestore();
  }
});

// R3 (dispatch://INFRA-4097/spec#b-r-change): the Change names the decision node's own block as
// `anchor.block_id`, so the server finds the quote inside that block alone. A quote alone is
// refused wherever the decision's first paragraph also stands elsewhere in the document, as a
// converted decision's old heading often does (`pmdoc.FindQuote`'s `ErrTargetAmbiguous`), and
// the reader can never send that Change. This is the case c-unit names.
test("Change anchors to the node's document and block", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const user = userEvent.setup();
  const { unmount } = renderActions([ONE], SPEC_DOCUMENT);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() =>
      expect(createComment, "the Change was never sent").toHaveBeenCalledTimes(1)
    );
    expect(
      createComment.mock.calls[0]?.[1]?.anchor,
      "the Change anchors to its decision's block"
    ).toMatchObject({ artifact: document_.id, block_id: ONE.id, quote: "One tree, five presets" });
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// Keep is written against the decision node's own
// document, block and sum, as the tree read answers them on the node, whatever document the row
// that draws it stands in. Kept, the tree is read again.
test("Keep posts the node's document, block id and sum", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: document_.id,
    block_id: ONE.id,
    changed_version: 1,
    kept_at: "2026-11-17T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const user = userEvent.setup();
  const { client, unmount } = renderActions([ONE], SPEC_DOCUMENT);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision }));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    expect(
      keep.mock.calls[0]?.slice(0, 3),
      "the Keep named something other than the node's own document, block and sum"
    ).toEqual([document_.id, "d1", "sum-d1"]);
    await waitFor(() =>
      expect(
        client.getQueryState(issueTreeKey(KEY))?.isInvalidated,
        "the kept decision's tree was not read again"
      ).toBe(true)
    );
  } finally {
    unmount();
    keep.mockRestore();
  }
});

// Dispatch refuses a Keep of words the agent changed since the node was drawn
// (`409 DECISION_CHANGED`). The decision still waits, so Keep stays drawn, with the alert that
// says why; and the tree is read again and the Decisions view dropped, so the page draws the
// decision in its new words.
test("a refused Keep keeps the decision waiting and reads the tree again", async () => {
  const refusal = "The decision changed since you read it.";
  const keep = spyOn(api, "keepDecision").mockRejectedValue(
    new ApiError(409, { code: "DECISION_CHANGED", error: refusal })
  );
  const user = userEvent.setup();
  const { client, unmount } = renderActions();
  // The Decisions view, held as the page holds it once that view has been open.
  client.setQueryData(viewKey(KEY, "decisions"), client.getQueryData(issueTreeKey(KEY)));
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(refusal);
    expect(keep).toHaveBeenCalledTimes(1);
    const again = screen.getByRole("button", { name: CONTROL_LABELS.keep_decision });
    expect(
      again instanceof HTMLButtonElement && !again.disabled,
      "the refused decision's Keep cannot be pressed again"
    ).toBe(true);
    await waitFor(() =>
      expect(
        client.getQueryState(issueTreeKey(KEY))?.isInvalidated,
        "the refused decision's tree was not read again"
      ).toBe(true)
    );
    expect(
      client.getQueryData(viewKey(KEY, "decisions")),
      "the Decisions view still holds the decision as it was before the refusal"
    ).toBeUndefined();
  } finally {
    unmount();
    keep.mockRestore();
  }
});

/** The tree read as the page holds it, by an observer that is enabled (`useIssueTree.ts`'s
 *  `useTreeRead`), which is what a refresh of the tree reads again. */
function PageTreeRead(): null {
  useQuery(issueTreeQuery(KEY));
  return null;
}

// A Keep reads the tree again as the page does on an event of the issue (`refreshQueries`): a
// read of the tree already in flight - a walk of its pages, which may be long - is waited for,
// never cut off, and a fresh read follows it, so the tree the page draws is one read after the
// Keep landed. An invalidation would abort the read in flight and start it over.
test("Keep leaves a tree read in flight to finish and reads the tree again after it", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: document_.id,
    block_id: ONE.id,
    changed_version: 1,
    kept_at: "2026-11-17T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const client = heldReads([ONE]);
  const held = client.getQueryData<TreeAnswer>(issueTreeKey(KEY));
  if (held === undefined) throw new Error("the client holds no tree read");
  const inFlight = Promise.withResolvers<TreeAnswer>();
  const signals: (AbortSignal | undefined)[] = [];
  const tree = spyOn(api, "getIssueTree").mockImplementation((_key, _query, init) => {
    signals.push(init?.signal);
    return signals.length === 1 ? inFlight.promise : Promise.resolve(held);
  });
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={client}>
      <PageTreeRead />
      <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
    </QueryClientProvider>
  );
  try {
    // The read in flight starts inside `act`, since it moves the tree read the page draws from.
    await act(async () => {
      void client.refetchQueries({ exact: true, queryKey: issueTreeKey(KEY) });
    });
    await waitFor(() =>
      expect(tree, "no tree read was in flight before the Keep").toHaveBeenCalledTimes(1)
    );
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision }));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    // The Keep's mutation lands only once its `onSettled` has run, however that reads the tree.
    await waitFor(() =>
      expect(
        client
          .getMutationCache()
          .getAll()
          .map((mutation) => mutation.state.status),
        "the Keep never landed"
      ).toEqual(["success"])
    );
    expect(signals[0]?.aborted, "the Keep cut off the tree read in flight").toBe(false);
    expect(
      client.getQueryState(issueTreeKey(KEY))?.isInvalidated,
      "the Keep did not mark the tree to be read again"
    ).toBe(true);
    expect(tree, "the Keep started a tree read beside the one in flight").toHaveBeenCalledTimes(1);

    await act(async () => {
      inFlight.resolve(held);
      await inFlight.promise;
    });
    await waitFor(
      () => expect(tree, "no tree read followed the one in flight").toHaveBeenCalledTimes(2),
      { timeout: 3000 }
    );
    expect(signals[0]?.aborted, "the read in flight was cut off").toBe(false);
  } finally {
    view.unmount();
    keep.mockRestore();
    tree.mockRestore();
  }
});

// Decision 227: a write of the new page whose answer changes what the tree draws reads the tree
// itself (`refreshQueries` of the tree's key, as Keep does) and drops the held views, and none
// reaches the tree through another read's invalidation alone. A Change is such a write: the tree
// read hangs it under its decision. TanStack dispatches no `invalidate` for a read that is already
// invalidated (an owed refresh, a failed background read), so where the comments read already
// is, a Change that only invalidates it leaves the page's tree, and the views it holds, drawing
// the decision as it stood.
test("a Change sent while the comments read is already invalidated reads the tree again and drops the held views", async () => {
  const client = heldReads([ONE]);
  const held = client.getQueryData<TreeAnswer>(issueTreeKey(KEY));
  if (held === undefined) throw new Error("the client holds no tree read");
  // The Decisions and Spec views, held as the page holds them once each has been open.
  client.setQueryData(viewKey(KEY, "decisions"), held);
  client.setQueryData(viewKey(KEY, "spec"), held);
  // The comments read, held and already invalidated before the Change is sent.
  client.setQueryData(["comments", KEY], []);
  await client.invalidateQueries({ exact: true, queryKey: ["comments", KEY], refetchType: "none" });
  const tree = spyOn(api, "getIssueTree").mockResolvedValue(held);
  const createComment = spyOn(api, "createComment").mockResolvedValue({ id: "change-1" } as never);
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={client}>
      <PageTreeRead />
      <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
    </QueryClientProvider>
  );
  try {
    expect(
      client.getQueryState(["comments", KEY])?.isInvalidated,
      "the comments read was not invalidated before the Change"
    ).toBe(true);
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() =>
      expect(createComment, "the Change was never sent").toHaveBeenCalledTimes(1)
    );
    await waitFor(
      () =>
        expect(
          tree.mock.calls.some(([, query]) => query?.view === undefined),
          "the Change did not read the tree again"
        ).toBe(true),
      { timeout: 3000 }
    );
    expect(
      [viewKey(KEY, "decisions"), viewKey(KEY, "spec")].map((key) => client.getQueryData(key)),
      "the Change left a held view drawing the decision as it stood"
    ).toEqual([undefined, undefined]);
  } finally {
    view.unmount();
    tree.mockRestore();
    createComment.mockRestore();
  }
});

// Decision 227: Keep reads the tree again, and drops the held views, every one of them: the Spec
// view's column draws how each decision stands from its own read too, so a Spec view held from
// before the Keep would draw the kept decision as still waiting for review.
test("Keep drops every held view, the Spec view's among them", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: document_.id,
    block_id: ONE.id,
    changed_version: 1,
    kept_at: "2026-11-17T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const user = userEvent.setup();
  const { client, unmount } = renderActions();
  const held = client.getQueryData<TreeAnswer>(issueTreeKey(KEY));
  client.setQueryData(viewKey(KEY, "decisions"), held);
  client.setQueryData(viewKey(KEY, "spec"), held);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision }));
    await waitFor(() => expect(keep, "the Keep was never sent").toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        client.getQueryData(viewKey(KEY, "decisions")),
        "the Keep did not drop the held Decisions view"
      ).toBeUndefined()
    );
    expect(
      client.getQueryData(viewKey(KEY, "spec")),
      "the Keep left the held Spec view drawing the decision as it stood"
    ).toBeUndefined();
  } finally {
    unmount();
    keep.mockRestore();
  }
});

// The node finds its decision in the read the page already holds, through an observer that is
// never enabled, so drawing Keep and Change sends no request, though the held answer is not
// fresh and an enabled observer would read it again as it mounts.
test("the node sends no request of its own to draw Keep and Change", async () => {
  const tree = spyOn(api, "getIssueTree").mockImplementation(() => new Promise<never>(() => {}));
  const asks = spyOn(api, "listIssueAsks").mockImplementation(() => new Promise<never>(() => {}));
  const { unmount } = renderActions();
  try {
    await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision });
    expect(screen.getByRole("button", { name: CONTROL_LABELS.change_decision })).toBeTruthy();
    expect(tree, "the node read the tree of its own").not.toHaveBeenCalled();
    expect(asks, "the node read the asks of its own").not.toHaveBeenCalled();
  } finally {
    unmount();
    tree.mockRestore();
    asks.mockRestore();
  }
});
