import { beforeAll, expect, spyOn, test } from "bun:test";
import { dispatchWritingBox } from "@legion/contracts";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { conversationTreeKey } from "../../api/queries";
import type { Artifact, DecisionKeep } from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageWordsText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { ChangeDraftsProvider, DecisionActions } from "./DecisionActions";
import { NoTooltips } from "./NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { Publisher, publishedNames } from "./projection-fixture";
import { type TreeNodeExtra, treeAnswer, treeNode } from "./tree-read/tree-fixtures";
import { outlineQuery, type TreeAnswer, type TreeAnswerNode, viewKey } from "./tree-read/tree-read";

/**
 * The two controls a decision's node draws (R7). The tree is one tab stop,
 * so neither takes the tab order and the row's own keys press them (`useTreeKeymap.ts`); what
 * this file reads is what the controls themselves do, which is what both a pointer and those
 * keys get. Each case holds the read the page holds, the tree read's answer, under the key the
 * page reads it by, and the node finds its decision there without a request of its own.
 */

const KEY = "CORE-1";

/** The issue's spec, whose `decision` blocks are the agent's decisions: the document a Keep is
 *  written against and a Change is anchored in. */
const document_: Artifact = {
  created_at: "2026-11-05T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-spec",
  issue_key: KEY,
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  ref_key: "artifact:artifact-spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

/** The decision's own address, as the tree read addresses a typed block of a document
 *  (`decision-nodes.ts`' `nodeReference`). */
const REFERENCE = `dispatch://${KEY}/artifact/${document_.slug}#b-d1`;

/**
 * A decision node of `document_`, as the tree read answers one (`treeNode`), carrying the model's
 * decision (`decision`): an agent's decision that waits for review, unless `extra` says
 * otherwise. Its first paragraph is the heading every accessible name reads and the words a
 * Change is anchored to, so a decision with one is a decision whose Change control is drawn.
 */
function decisionNode(id: string, text: string, extra: TreeNodeExtra = {}): TreeAnswerNode {
  const { decision, ...rest } = extra;
  return treeNode(id, "decision", KEY, {
    document: { artifact_id: document_.id, position: 1, slug: document_.slug },
    text,
    ...rest,
    decision: { text_sha256: `sum-${id}`, waiting_review: true, ...decision },
  });
}

/** The decision at `REFERENCE`. */
const ONE = decisionNode(
  "d1",
  "One tree, five presets\n\nThe presets are queries over the one tree."
);

/** A client that holds what the page holds: the outline read's answer of `nodes`, on one page,
 *  Conversation's, the read the page holds at the address each case draws (`MemoryRouter`'s
 *  `/`). It is not fresh, so an enabled observer would read it again as it mounts. */
function heldReads(nodes: readonly TreeAnswerNode[]): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(conversationTreeKey(KEY), treeAnswer(nodes));
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
      <MemoryRouter>{page ? <NoTooltips>{children}</NoTooltips> : children}</MemoryRouter>
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
      <MemoryRouter>
        <ChangeDraftsProvider>
          {drawn ? (
            <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
          ) : null}
        </ChangeDraftsProvider>
      </MemoryRouter>
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
// name what the reader cannot act on - here the block the anchor names, which does not hold the
// quote (`400 INVALID_ANCHOR`) - and the words the reader wrote stay in the field, to send again.
test("a refused send shows the plain words and keeps the draft", async () => {
  const refusal = 'anchor block_id "d1" names a block that does not hold the quote';
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(400, { code: "INVALID_ANCHOR", error: refusal })
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
      "block_id"
    );
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Keep one preset.");
  } finally {
    body.unmount();
    createComment.mockRestore();
  }
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
    expect(createComment.mock.calls[0]?.slice(0, 2)).toEqual([
      KEY,
      {
        anchor: {
          artifact: document_.id,
          block_id: ONE.block_id ?? ONE.id,
          occurrence: 0,
          quote: "One tree, five presets",
        },
        body: "Say where.",
      },
    ]);
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
    ).toMatchObject({
      artifact: document_.id,
      block_id: ONE.block_id ?? ONE.id,
      quote: "One tree, five presets",
    });
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// R3 and root R42: the Change's anchor carries its `occurrence`, 0, beside the block and the
// quote, so the server takes the first match of the quote inside the decision's own block, which
// is its first paragraph itself, though those words come again further down the block: with no
// occurrence it refuses such a Change (`409 TARGET_AMBIGUOUS`). No client counts the occurrence
// over a document, so the send reads no document's text.
test("a Change's anchor carries occurrence 0 inside its block", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const text = spyOn(api, "getArtifactText");
  const repeated = decisionNode(
    "d1",
    "One tree, five presets\n\nThe presets are queries over the one tree.\n\nOne tree, five presets"
  );
  const user = userEvent.setup();
  const { unmount } = renderActions([repeated]);
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() =>
      expect(createComment, "the Change was never sent").toHaveBeenCalledTimes(1)
    );
    expect(
      createComment.mock.calls[0]?.[1]?.anchor,
      "the Change's anchor names its block, its quote and the quote's first match there"
    ).toEqual({
      artifact: document_.id,
      block_id: repeated.block_id ?? repeated.id,
      occurrence: 0,
      quote: "One tree, five presets",
    });
    expect(text, "the send read the document's text").not.toHaveBeenCalled();
  } finally {
    unmount();
    createComment.mockRestore();
    text.mockRestore();
  }
});

// A Change quotes the decision's heading, the words of its first paragraph, and Dispatch refuses
// an anchor whose quote is empty (`api/anchors.go`: "anchor quote must not be empty"). A decision
// whose first paragraph holds no words - here an image alone - still waits for review, so it
// draws Keep, and it draws no Change, which no reader could ever send.
test("a decision whose first paragraph holds no words draws Keep but no Change", async () => {
  const words = "The presets are queries over the one tree.";
  const pictured = decisionNode("d1", `![The five presets](presets.png)\n\n${words}`, {
    text_tree: {
      content: [
        { content: [{ type: "image" }], type: "paragraph" },
        { content: [{ text: words, type: "text" }], type: "paragraph" },
      ],
      type: "doc",
    },
  });
  const { unmount } = renderActions([pictured]);
  try {
    await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision });
    expect(
      screen.queryByRole("button", { name: CONTROL_LABELS.change_decision }),
      "a decision with no words to quote drew a Change whose empty quote Dispatch refuses"
    ).toBeNull();
  } finally {
    unmount();
  }
});

/** The statement the decision below cites in its first paragraph, and the name the page's one
 *  batched read brought for it. */
const STATEMENT = `dispatch://${KEY}/spec#b-r1`;
const STATEMENT_NAME = "Every property of the issue";

// Decision 448, root R22: the Change form draws its quote by the page's plain-text projection of
// the decision's first paragraph (`usePlainHeadline`), so the statement it cites reads by the
// name the page published for it, and so does the form's accessible name, never as a raw
// `dispatch://` address. The anchor still quotes the words as the server's text holds them,
// address and all, which is what `pmdoc.FindQuote` matches.
test("the Change form draws its quote with each reference by its name", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const citing = decisionNode("d1", `Shapes ${STATEMENT}.\n\nWhy it holds.`);
  const client = heldReads([citing]);
  const names = publishedNames(new Map([[STATEMENT, STATEMENT_NAME]]));
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Publisher published={names} />
          <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    const quote = screen.getByTestId("decision-quote");
    const form = quote.closest("form");
    await waitFor(() => {
      const name = form?.getAttribute("aria-label");
      expect(
        {
          name,
          quote: quote.textContent,
          raw: `${name} ${form?.textContent}`.includes("dispatch://"),
        },
        "the Change form draws its quote and its name with the statement by its name, no raw address"
      ).toEqual({
        name: `${CONTROL_LABELS.change_decision} Shapes ${STATEMENT_NAME}.`,
        quote: `Shapes ${STATEMENT_NAME}.`,
        raw: false,
      });
    });
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() =>
      expect(createComment, "the Change was never sent").toHaveBeenCalledTimes(1)
    );
    expect(
      createComment.mock.calls[0]?.[1]?.anchor?.quote,
      "the Change quotes the words as the server's text holds them, the address kept"
    ).toBe(`Shapes ${STATEMENT}.`);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// Keep is written against the decision node's own
// document, block and sum, as the tree read answers them on the node, whatever document the row
// that draws it stands in. Kept, the tree is read again.
test("Keep posts the node's document, block id and sum", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: document_.id,
    block_id: ONE.block_id ?? ONE.id,
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
        client.getQueryState(conversationTreeKey(KEY))?.isInvalidated,
        "the kept decision's tree was not read again"
      ).toBe(true)
    );
  } finally {
    unmount();
    keep.mockRestore();
  }
});

// Conversation's tree read is paged: on a large issue its first page ends before the last
// decisions, which the Decisions view read holds (measured on a preview of 3aa8782b: on
// INFRA-1196's copy the Decisions view drew the agent's decision under an answered question in
// the review list, waiting, with no Keep and no Change, where every decision on the first page
// drew both). A decision that waits for review draws Keep and Change wherever the page draws it
// (spec R2, R3, R4, R8), whether or not the outline read's first page holds its node.
test("a waiting decision past the outline read's first page draws Keep and Change from the view read", async () => {
  const other = decisionNode("d0", "Another decision\n\nOn the first page.");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(
    conversationTreeKey(KEY),
    treeAnswer([other], [], { cursor: "next-page", remaining: 30 })
  );
  client.setQueryData(viewKey(KEY, "decisions"), treeAnswer([ONE]));
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(
    <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />,
    { wrapper }
  );
  try {
    await waitFor(() =>
      expect(
        screen.queryAllByRole("button").map((button) => button.textContent),
        "a waiting decision the Decisions view read holds draws no Keep and no Change"
      ).toEqual(
        expect.arrayContaining([CONTROL_LABELS.keep_decision, CONTROL_LABELS.change_decision])
      )
    );
  } finally {
    view.unmount();
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
  // The Decisions view, held as the page holds it once that view has been open. The node watches
  // it too, so it is drawn again as the view lands.
  await act(async () => {
    client.setQueryData(viewKey(KEY, "decisions"), client.getQueryData(conversationTreeKey(KEY)));
  });
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
        client.getQueryState(conversationTreeKey(KEY))?.isInvalidated,
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

/** The outline read as the page holds it, Conversation's, by an observer that is enabled
 *  (`useIssueTree.ts`'s `useTreeRead`), which is what a refresh of the outline reads again. */
function PageTreeRead(): null {
  useQuery(outlineQuery(KEY, "conversation"));
  return null;
}

// A Keep reads the tree again as the page does on an event of the issue (`refreshQueries`): a
// read of the tree already in flight - a walk of its pages, which may be long - is waited for,
// never cut off, and a fresh read follows it, so the tree the page draws is one read after the
// Keep landed. An invalidation would abort the read in flight and start it over.
test("Keep leaves a tree read in flight to finish and reads the tree again after it", async () => {
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: document_.id,
    block_id: ONE.block_id ?? ONE.id,
    changed_version: 1,
    kept_at: "2026-11-17T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const client = heldReads([ONE]);
  const held = client.getQueryData<TreeAnswer>(conversationTreeKey(KEY));
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
      <MemoryRouter>
        <PageTreeRead />
        <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    // The read in flight starts inside `act`, since it moves the tree read the page draws from.
    await act(async () => {
      void client.refetchQueries({ exact: true, queryKey: conversationTreeKey(KEY) });
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
      client.getQueryState(conversationTreeKey(KEY))?.isInvalidated,
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
  const held = client.getQueryData<TreeAnswer>(conversationTreeKey(KEY));
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
      <MemoryRouter>
        <PageTreeRead />
        <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
      </MemoryRouter>
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
    block_id: ONE.block_id ?? ONE.id,
    changed_version: 1,
    kept_at: "2026-11-17T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  const user = userEvent.setup();
  const { client, unmount } = renderActions();
  const held = client.getQueryData<TreeAnswer>(conversationTreeKey(KEY));
  await act(async () => {
    client.setQueryData(viewKey(KEY, "decisions"), held);
    client.setQueryData(viewKey(KEY, "spec"), held);
  });
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
  const asks = spyOn(api, "listIssueAskThreads").mockImplementation(
    () => new Promise<never>(() => {})
  );
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

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// The Change's box is the field every box a person writes a comment in mounts, with the
// reference picker in it (the writing list's `decisions-change`), and the line under it is that
// row's own words. Inside `NoTooltips` it takes the page's shape - its box, its name and that
// line in the page's own colours, no `title` anywhere, and no face of its own, so it is drawn in
// the page's typeface - and outside it the field keeps its own box.
test("the Change's box is the shared field, in the page's shape inside NoTooltips", async () => {
  for (const surface of ["page", "main"] as const) {
    const user = userEvent.setup();
    const { unmount } = renderActions([ONE], document_, { page: surface === "page" });
    try {
      await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
      const box = screen.getByLabelText(CONTROL_LABELS.your_change);
      const hint = document.getElementById(
        box.getAttribute("aria-describedby")?.split(" ")[0] ?? ""
      );
      expect(hint?.textContent).toBe(dispatchWritingBox("decisions-change", "comment").hint);
      const name = box.closest("label");
      if (surface === "page") {
        expect(wears(box, pageHairBorder, pagePaperBg, pageInkText), "the box's colours").toBe(
          true
        );
        expect(wears(box, inputClasses(true)), "the box draws main's colours").toBe(false);
        expect(name !== null && wears(name, pageMutedText), "the name's colour").toBe(true);
        expect(hint !== null && wears(hint, pageMutedText), "the line's colour").toBe(true);
        const form = box.closest("form");
        expect(
          form?.querySelectorAll("[title]").length,
          "an element of the form sets a title"
        ).toBe(0);
        const faces = [form, ...(form?.querySelectorAll("*") ?? [])].filter((element) =>
          [...(element?.classList ?? [])].some((one) => /^font-(?:sans|serif|mono)$/u.test(one))
        );
        expect(faces.length, "an element of the form draws in a face of its own").toBe(0);
      } else {
        expect(wears(box, inputClasses(true)), "the box keeps the field's own").toBe(true);
      }
    } finally {
      unmount();
    }
  }
});

// A write's label and a form's name hold no markup, so Keep's write and the Change form are
// named after the heading's plain words - the decision's first paragraph, the words a Change
// quotes (decision 448) - as the Decisions panel names its own (the reading list's
// `decision-heading-words`), and never after its markdown.
test("the Change form is named after the heading's plain words", async () => {
  const marked = decisionNode(
    "d1",
    "**One** tree, five presets\n\nThe presets are queries over the one tree."
  );
  const user = userEvent.setup();
  const { unmount } = renderActions([marked], document_, { page: true });
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    expect(
      await screen.findByRole("form", {
        name: `${CONTROL_LABELS.change_decision} One tree, five presets`,
      })
    ).toBeTruthy();
  } finally {
    unmount();
  }
});

// R6, R8 and the reading list's `decision-change-quote`: the Change quotes the words it is
// anchored to as the reader reads the decision - the plain-text projection of its first
// paragraph, a reference in it read as its target's name - while the change it sends is still
// anchored to that paragraph's words as the server's text holds them, which the server matches
// (`pmdoc.FindQuote`).
test("the Change quotes its block's plain-text projection and anchors to the document's own words", async () => {
  const written = "The presets read dispatch://CORE-7 first.";
  const cited = decisionNode("d1", `## One tree, five presets\n\n${written}`);
  // What the page's batched read publishes for the quote's row: the issue `CORE-7`, named.
  const names = publishedNames(new Map([["dispatch://CORE-7", "Draw the rows"]]));
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const client = heldReads([cited]);
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ReferenceNamesProvider>
          <Publisher published={names} />
          <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await waitFor(() =>
      expect(screen.getByTestId("decision-quote").textContent).toBe(
        "The presets read CORE-7 Draw the rows first."
      )
    );
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]?.[1]?.anchor?.quote).toBe(written);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});
