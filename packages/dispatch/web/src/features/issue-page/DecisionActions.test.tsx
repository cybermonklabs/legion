import { beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import { dispatchWritingBox } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { ApiError, api } from "../../api/client";
import type { Artifact, ArtifactText, Decision, IssueDecisions } from "../../api/types";
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

/**
 * The two controls a decision's node draws (R7). The tree is one tab stop,
 * so neither takes the tab order and the row's own keys press them (`useTreeKeymap.ts`); what
 * this file reads is what the controls themselves do, which is what both a pointer and those
 * keys get.
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

const decision: Decision = {
  asks: [],
  block_id: "d1",
  changed_at: null,
  changed_version: 1,
  heading: "One tree, five presets",
  kept: null,
  kind: "agent",
  links: [],
  ref: REFERENCE,
  replaced: false,
  // The first paragraph below the heading is what a change is anchored to, so a decision with
  // one is a decision whose Change control is drawn.
  text: "One tree, five presets\n\nThe presets are queries over the one tree.",
  text_sha256: "sum",
  unsure: false,
  waiting_review: true,
};

const read: IssueDecisions = {
  decisions: [decision],
  document: document_.id,
  open_questions: [],
};

/** The decisions document `read` is the read of: its title, then the one decision. */
const DOCUMENT_TEXT =
  "# Decisions of CORE-1\n\n## One tree, five presets\n\nThe presets are queries over the one tree.";

/** The spy on the decisions document's text read, which a Change's send makes. */
type DocumentTextSpy = Mock<typeof api.getArtifactText>;

/** Answers the decisions document's text read (`api.getArtifactText`) with the document
 *  `markdown` holds and the reading the server sends of it, or refuses it. */
function spyDocumentText(markdown: string | Error): DocumentTextSpy {
  return spyOn(api, "getArtifactText").mockImplementation(async (): Promise<ArtifactText> => {
    if (markdown instanceof Error) throw markdown;
    return { markdown, markdown_tree: displayTree(markdown), token: "token", version: 1 };
  });
}

/** Draws the decision's controls over `answer`, with the decisions document's text read answered
 *  by `documentText`; inside the page's `NoTooltips` where `page` says so, as the issue page
 *  draws them. */
function renderActions(
  answer: IssueDecisions = read,
  documentText: string | Error = DOCUMENT_TEXT,
  { page = false }: { page?: boolean } = {}
): { text: DocumentTextSpy; unmount: () => void } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["decisions", KEY], answer);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(answer);
  const text = spyDocumentText(documentText);
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      {page ? <NoTooltips>{children}</NoTooltips> : children}
    </QueryClientProvider>
  );
  const view = render(
    <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />,
    { wrapper }
  );
  return {
    text,
    unmount: () => {
      view.unmount();
      decisions.mockRestore();
      text.mockRestore();
    },
  };
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
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["decisions", KEY], read);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(read);
  const text = spyDocumentText(DOCUMENT_TEXT);
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
    unmount: () => {
      view.unmount();
      decisions.mockRestore();
      text.mockRestore();
    },
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
      const { unmount } = renderActions(read, DOCUMENT_TEXT, { page: surface === "page" });
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

// The Change's box is the field every box a person writes a comment in mounts, with the
// reference picker in it (the writing list's `decisions-change`), and the line under it is that
// row's own words. Inside `NoTooltips` it takes the page's shape - its box, its name and that
// line in the page's own colours, no `title` anywhere, and no face of its own, so it is drawn in
// the page's typeface - and outside it the field keeps its own box.
test("the Change's box is the shared field, in the page's shape inside NoTooltips", async () => {
  for (const surface of ["page", "main"] as const) {
    const user = userEvent.setup();
    const { unmount } = renderActions(read, DOCUMENT_TEXT, { page: surface === "page" });
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
// named after the heading's plain words, as the Decisions panel names its own (the reading
// list's `decision-heading-words`), and never after its markdown.
test("the Change form is named after the heading's plain words", async () => {
  const text = "## **One** tree, five presets\n\nThe presets are queries over the one tree.";
  const marked: Decision = {
    ...decision,
    heading: "**One** tree, five presets",
    text,
    text_tree: displayTree(text),
  };
  const user = userEvent.setup();
  const { unmount } = renderActions({ ...read, decisions: [marked] }, DOCUMENT_TEXT, {
    page: true,
  });
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

/** Two decisions whose first blocks both hold "Draw it once.", the second's whole: a Change of
 *  the second quotes words that stand more than once in the decisions document. */
const ABOVE: Decision = {
  ...decision,
  block_id: "d0",
  heading: "Keep the model pure",
  ref: `dispatch://${KEY}/artifact/decisions-md#b-d0`,
  text: "## Keep the model pure\n\nKeep the model pure. Draw it once.",
};
const OWN: Decision = {
  ...decision,
  heading: "Draw it once",
  text: "## Draw it once\n\nDraw it once.",
};

/** The decisions document that holds `ABOVE` and `OWN`, below its title and `intro`. */
function twoDecisionsText(intro: string): string {
  return `# Decisions of CORE-1\n\n${intro}\n\n${ABOVE.text}\n\n${OWN.text}`;
}

/** The 409 Dispatch answers a comment whose anchor quote stands in `places` places of the
 *  document and whose occurrence names none of them: `TARGET_AMBIGUOUS` with each place, as
 *  `writeAmbiguousTarget` writes `pmdoc.ErrTargetAmbiguous` (`api/handler_errors.go`). */
function ambiguousQuote(quote: string, places: number): ApiError {
  return new ApiError(409, {
    candidates: Array.from({ length: places }, (_unused, at) => ({
      context: quote,
      from: 40 * at + 1,
      to: 40 * at + 1 + quote.length,
    })),
    code: "TARGET_AMBIGUOUS",
    error: `"${quote}" matches ${places} places; pass a zero-based occurrence, or quote more of the surrounding text`,
  });
}

// Decision 92: two decisions can open with the same sentence, and Dispatch refuses a quote that
// matches more than one place unless the write names which. A Change names its own decision's
// place: here the quote also stands, earlier, in the decision above it, so it is the second.
test("a Change names which place of the quote is its decision's own", async () => {
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const user = userEvent.setup();
  const { unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    twoDecisionsText("The decisions this issue made.")
  );
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]?.[1]?.anchor).toEqual({
      artifact: document_.id,
      occurrence: 1,
      quote: "Draw it once.",
    });
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// Decision 92 as amended: the server counts a quote's places over the whole decisions document,
// so the send reads the document's text once, when the reader sends, and counts over its reading
// (`markdown_tree`), never over the decisions read alone. Here the document's intro, which no
// decision holds, quotes the decision too, so its own place is the third: a count over the
// decisions read says the second, and a send with no reading names none. A refusal Dispatch
// still makes is drawn in the page's words, the draft kept.
test("a Change's send reads the decisions document once and counts its quote's places over it", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 3)
  );
  const user = userEvent.setup();
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    twoDecisionsText("Draw it once. That is the rule of every decision below.")
  );
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    expect(
      text,
      "the document's text was read before the reader sent the Change"
    ).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    const alert = await screen.findByRole("alert");
    expect(text, "the send read the document's text other than once").toHaveBeenCalledTimes(1);
    expect(text).toHaveBeenCalledWith(document_.id);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(
      createComment.mock.calls[0]?.[1]?.anchor,
      "the Change's place was not counted over the decisions document's reading"
    ).toEqual({ artifact: document_.id, occurrence: 2, quote: "Draw it once." });
    expect(alert.textContent).toBe(ANSWER_LINES.change_not_sent);
    expect(document.body.textContent, "the row draws the server's own words").not.toContain(
      "occurrence"
    );
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Say where.");
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// With no reading of the document - its text read failed - the Change names no place: Dispatch
// takes it where the quote stands once, and refuses it where the quote stands more than once,
// which the form says in the page's words with the draft kept, rather than land it on a place
// counted without the document.
test("a Change whose document read fails names no place, and its refusal keeps the draft", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 2)
  );
  const user = userEvent.setup();
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    new ApiError(503, { code: "UNAVAILABLE", error: "the store is not reachable" })
  );
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    const alert = await screen.findByRole("alert");
    expect(text).toHaveBeenCalledTimes(1);
    expect(createComment).toHaveBeenCalledTimes(1);
    const anchor = createComment.mock.calls[0]?.[1]?.anchor;
    expect(anchor?.quote).toBe("Draw it once.");
    expect(anchor?.occurrence, "a Change with no reading named a place").toBeUndefined();
    expect(alert.textContent).toBe(ANSWER_LINES.change_not_sent);
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Say where.");
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The quote the Change form offers for the one decision `fields` describe, which waits for
 *  review so its node draws the Change. */
async function quoteOf(fields: Pick<Decision, "text" | "text_tree">): Promise<string> {
  const user = userEvent.setup();
  const { unmount } = renderActions({ ...read, decisions: [{ ...decision, ...fields }] });
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
// words come from the reading the server sent (`Decision.text_tree`), which the test builds
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

// R6, R8 and the reading list's `decision-change-quote`: the Change quotes the words it is
// anchored to as the reader reads the decision - the plain-text projection of its first block,
// a reference in it read as its target's name - while the change it sends is still anchored to
// the decisions document's own words, which the server matches (`pmdoc.FindQuote`).
test("the Change quotes its block's plain-text projection and anchors to the document's own words", async () => {
  const written = "The presets read dispatch://CORE-7 first.";
  const text = `## One tree, five presets\n\n${written}`;
  const cited: Decision = { ...decision, text, text_tree: displayTree(text) };
  const answer = { ...read, decisions: [cited] };
  // What the page's batched read publishes for the quote's row: the issue `CORE-7`, named.
  const names = publishedNames(new Map([["dispatch://CORE-7", "Draw the rows"]]));
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["decisions", KEY], answer);
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(answer);
  const documentText = spyDocumentText(`# Decisions of CORE-1\n\n${text}`);
  const user = userEvent.setup();
  const view = render(
    <QueryClientProvider client={client}>
      <ReferenceNamesProvider>
        <Publisher published={names} />
        <DecisionActions document={document_} isClosed={false} nodeReference={REFERENCE} />
      </ReferenceNamesProvider>
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
    decisions.mockRestore();
    documentText.mockRestore();
    createComment.mockRestore();
  }
});
