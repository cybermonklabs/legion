import { beforeAll, expect, jest, type Mock, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { realClock, settle, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { userOnFakeClock } from "../../__tests__/user";
import {
  ApiError,
  api,
  NetworkError,
  REQUEST_DEADLINE_MS,
  RequestTimeoutError,
} from "../../api/client";
import type { Artifact, ArtifactText, Decision, IssueDecisions } from "../../api/types";
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
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { emulateChromiumFocusFixup } from "./chromium-focus-fixup";
import { ChangeDraftsProvider, DecisionActions } from "./DecisionActions";
import { NoTooltips } from "./NoTooltips";
import { ANSWER_LINES, CONTROL_LABELS, STATE_LINES } from "./page-words";

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

/** The path of the decisions document's text read, which the error of a read that failed names. */
const TEXT_PATH = `/api/v1/artifacts/${document_.id}/text`;

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
 *  draws them. `client` is the page's query client, whose store holds the writes. */
function renderActions(
  answer: IssueDecisions = read,
  documentText: string | Error = DOCUMENT_TEXT,
  { page = false }: { page?: boolean } = {}
): { client: QueryClient; text: DocumentTextSpy; unmount: () => void } {
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
    client,
    text,
    unmount: () => {
      view.unmount();
      client.clear();
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

// A keyboard reader who presses Keep stays on it while the Keep is out: Keep is held
// (`heldPress`, `aria-disabled`), never disabled, which would drop the focus to the page's body in
// a browser and does here under `emulateChromiumFocusFixup`, and a second press keeps nothing more. The
// controls are drawn with no row around them, so the focus stays the control's own: in the tree
// the press hands it to the decision's row (`keepFocusOnRow`).
test("Keep keeps the focus while its Keep is out, held rather than disabled, and a second press keeps nothing", async () => {
  // Dispatch never answers the Keep.
  const keepDecision = spyOn(api, "keepDecision").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const user = userEvent.setup();
  const { unmount } = renderActions();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const keep = await screen.findByRole("button", { name: CONTROL_LABELS.keep_decision });
    act(() => keep.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(keepDecision).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(keep).toHaveAttribute("aria-disabled", "true"));
    expect(keep).toBeEnabled();
    expect(keep, "Keep dropped the focus while its Keep was out").toHaveFocus();
    await user.keyboard("{Enter}");
    await user.click(keep);
    expect(keepDecision, "a second press of Keep kept the decision again").toHaveBeenCalledTimes(1);
  } finally {
    stopFocusFixup();
    unmount();
    keepDecision.mockRestore();
  }
});

// The same for Send change, the form's submit button: while the send reads the decisions document
// and while the comment is out it is held, never disabled, so the focus the press gave it is still
// on it once the comment is out, and a second press - Enter on it, or a click - submits nothing:
// no second read of the document and no second comment.
test("Send change keeps the focus while its change is out, held rather than disabled, and a second press sends nothing", async () => {
  // Dispatch never answers the send.
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const user = userEvent.setup();
  const { text, unmount } = renderActions();
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    const send = screen.getByRole("button", { name: CONTROL_LABELS.send_change });
    act(() => send.focus());
    await user.keyboard("{Enter}");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(send).toHaveAttribute("aria-disabled", "true"));
    expect(send).toBeEnabled();
    expect(send, "Send change dropped the focus while its change was out").toHaveFocus();
    await user.keyboard("{Enter}");
    await user.click(send);
    expect(text, "a second press read the decisions document again").toHaveBeenCalledTimes(1);
    expect(createComment, "a second press sent the change again").toHaveBeenCalledTimes(1);
  } finally {
    stopFocusFixup();
    unmount();
    createComment.mockRestore();
  }
});

/** A write's try that fails at the network, as every try does while Dispatch cannot be reached. */
const failsAtTheNetwork = () =>
  Promise.reject(
    new NetworkError("/api/v1/issues/CORE-1/comments", new TypeError("Failed to fetch"))
  );

// A Keep that never answers - each try fails at the network, and the next waits out its backoff -
// holds Keep until its deadline (`SEND_DEADLINE_MS`), and no longer: past it the Keep is late and
// still tried, and Keep is let go, with the focus on it. A press then sends a Keep of its own.
test("Keep, held until its Keep's deadline, is let go there, and a press then sends a Keep again", async () => {
  const keepDecision = spyOn(api, "keepDecision").mockImplementation(failsAtTheNetwork);
  const { client, unmount } = renderActions();
  try {
    const keep = await screen.findByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.keep_decision,
    });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await heldUntilLate(user, keep, {
      label: `${CONTROL_LABELS.keep_decision} ${decision.heading}`,
      name: "Keep",
      queryClient: client,
      sent: () => pressesOf([keepDecision]),
    });
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(pressesOf([keepDecision]), "a press past the deadline sent no Keep").toBe(2);
  } finally {
    realClock();
    unmount();
    keepDecision.mockRestore();
  }
});

/**
 * Opens the Change form and sends "Keep one preset." by Enter on Send change, to a Dispatch that
 * never answers while `refusing` is false - each try fails at the network - and refuses once it is
 * true, and checks Send change is held until that change's deadline and let go past it
 * (`heldUntilLate`), with the change still out. Answers how many presses posted a change, and
 * what puts the page and the fake clock away (`restore`).
 */
async function changeUntilLate(refusing: () => boolean) {
  const createComment = spyOn(api, "createComment").mockImplementation(() =>
    refusing()
      ? Promise.reject(new ApiError(409, { code: "QUOTE_AMBIGUOUS", error: "two places" }))
      : failsAtTheNetwork()
  );
  const { client, unmount } = renderActions();
  const restore = () => {
    realClock();
    unmount();
    createComment.mockRestore();
  };
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    jest.useFakeTimers();
    const onFakeClock = userOnFakeClock();
    await heldUntilLate(
      onFakeClock,
      screen.getByRole<HTMLButtonElement>("button", { name: CONTROL_LABELS.send_change }),
      {
        label: `${CONTROL_LABELS.change_decision} ${decision.heading}`,
        name: "Send change",
        queryClient: client,
        sent: () => pressesOf([createComment]),
      }
    );
    return { presses: () => pressesOf([createComment]), restore, user: onFakeClock };
  } catch (error) {
    restore();
    throw error;
  }
}

// A change that never answers holds Send change until its deadline, and no longer. A press then
// would post the change a second time while the first is still tried under its key, so it sends
// nothing, and the line beside Send change says why and what comes: the first is still being
// sent, and it shows under the decision once Dispatch posts it.
test("Send change is let go at its change's deadline, and a press while that change is out posts no second one and says so", async () => {
  const page = await changeUntilLate(() => false);
  try {
    expect(screen.queryByRole("status")).toBeNull();
    await page.user.keyboard("{Enter}");
    await tick(1_000);
    expect(page.presses(), "a press while the change is out posted one").toBe(1);
    expect(screen.getByRole("status").textContent).toBe(ANSWER_LINES.change_still_sending);
    expect(screen.getByRole("button", { name: CONTROL_LABELS.send_change })).toHaveFocus();
  } finally {
    page.restore();
  }
});

// The line goes once the first change is decided. Refused, the form says the change was not
// sent, and Send change takes a press again, which posts a change of its own.
test("once a change out past its deadline is refused, Send change posts it again", async () => {
  let refusing = false;
  const page = await changeUntilLate(() => refusing);
  try {
    await page.user.keyboard("{Enter}");
    await tick(1_000);
    expect(screen.getByRole("status").textContent).toBe(ANSWER_LINES.change_still_sending);

    refusing = true;
    // Past the next try, half a minute at the longest backoff.
    await tick(30_000);
    expect(screen.getByRole("alert").textContent).toBe(ANSWER_LINES.change_not_sent);
    expect(screen.queryByRole("status")).toBeNull();
    await page.user.keyboard("{Enter}");
    await tick(1_000);
    expect(page.presses(), "a press after the refusal posted no change").toBe(2);
  } finally {
    page.restore();
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

/** The lines the form says as alerts, in the order it draws them. */
function alertLines(): (string | null)[] {
  return screen.queryAllByRole("alert").map((line) => line.textContent);
}

// With no reading of the document - its text read failed - the Change names no place: Dispatch
// takes it where the quote stands once, and refuses it where the quote stands more than once,
// which the form says in the page's words with the draft kept, rather than land it on a place
// counted without the document. Beside Send change the form says the document could not be
// loaded, and no more: the read's refusal is in the server's words, which the page never shows
// (decision 92).
test("a Change whose document read fails names no place, says the document could not be loaded, and its refusal keeps the draft", async () => {
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
    await screen.findByText(ANSWER_LINES.change_not_sent);
    expect(text).toHaveBeenCalledTimes(1);
    expect(createComment).toHaveBeenCalledTimes(1);
    const anchor = createComment.mock.calls[0]?.[1]?.anchor;
    expect(anchor?.quote).toBe("Draw it once.");
    expect(anchor?.occurrence, "a Change with no reading named a place").toBeUndefined();
    expect(alertLines()).toEqual([
      STATE_LINES.decisions_document_unreadable,
      ANSWER_LINES.change_not_sent,
    ]);
    expect(document.body.textContent, "the row draws the server's own words").not.toContain(
      "the store is not reachable"
    );
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

// Offline, TanStack pauses a fetch the browser starts until the network returns, so a send that
// waited on one for the document's text would post no comment and let go of nothing. The send
// reads the text straight through the API client instead (`captureUploadBase`'s way), which
// reaches the network, or fails there, whatever TanStack's pause says: the comment goes out and
// Send change lets go at that comment's deadline, as it does online, and a press then posts no
// second one.
test("offline, Send change reads the document outside TanStack's pause, posts one change and lets go at its deadline", async () => {
  const online = onlineManager.isOnline();
  onlineManager.setOnline(false);
  try {
    const page = await changeUntilLate(() => false);
    try {
      expect(
        api.getArtifactText,
        "the send read the document's text other than once"
      ).toHaveBeenCalledTimes(1);
      expect(api.getArtifactText).toHaveBeenCalledWith(document_.id);
      await page.user.keyboard("{Enter}");
      await tick(1_000);
      expect(page.presses(), "a press past the deadline posted a second change").toBe(1);
    } finally {
      page.restore();
    }
  } finally {
    onlineManager.setOnline(online);
  }
});

// A document read that never answers is the client's to end at `REQUEST_DEADLINE_MS`, and the
// comment goes out then with no reading of the document (the plain quote). That read and the
// comment are one press with one deadline, `SEND_DEADLINE_MS`: Send lets go there, not a second
// deadline after the read's, and a press after it posts no second change. The read answering
// after that posts nothing either.
test("a document read that never answers does not double Send change's deadline, and its late answer posts no second change", async () => {
  const answer = Promise.withResolvers<ArtifactText>();
  const createComment = spyOn(api, "createComment").mockImplementation(failsAtTheNetwork);
  const { text, unmount } = renderActions();
  text.mockReturnValue(answer.promise);
  const stopFocusFixup = emulateChromiumFocusFixup();
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    const send = screen.getByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.send_change,
    });
    jest.useFakeTimers();
    const onFakeClock = userOnFakeClock();
    act(() => send.focus());
    await onFakeClock.keyboard("{Enter}");
    await tick(0);

    // A second short of the request deadline the read is still out: Send is held with the
    // focus, no comment has gone, and a press reads nothing more.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(send, CONTROL_LABELS.send_change)).toBe(true);
    expect(send).toHaveFocus();
    expect(createComment).not.toHaveBeenCalled();
    await onFakeClock.keyboard("{Enter}");
    await settle();
    expect(text).toHaveBeenCalledTimes(1);

    // At it the comment goes out once, with the plain quote, and Send lets go with the focus on
    // it: the press has spent its deadline, and the comment's own is not waited out after it.
    await tick(1_000);
    await waitFor(() => expect(pressesOf([createComment])).toBe(1));
    const anchor = createComment.mock.calls[0]?.[1]?.anchor;
    expect(anchor?.artifact).toBe(document_.id);
    expect(anchor?.quote).toBe(decision.heading);
    expect(anchor?.occurrence, "a send with no reading named a place").toBeUndefined();
    expect(heldNotDisabled(send, CONTROL_LABELS.send_change)).toBe(false);
    expect(send).toHaveFocus();
    await onFakeClock.keyboard("{Enter}");
    await tick(1_000);
    expect(pressesOf([createComment]), "a press past the deadline posted a second change").toBe(1);
    expect(screen.getByRole("status").textContent).toBe(ANSWER_LINES.change_still_sending);

    // The read answers late, and the press posts nothing for it.
    await act(async () =>
      answer.resolve({
        markdown: DOCUMENT_TEXT,
        markdown_tree: displayTree(DOCUMENT_TEXT),
        token: "token",
        version: 1,
      })
    );
    await tick(SEND_DEADLINE_MS);
    expect(pressesOf([createComment]), "the late read posted a second change").toBe(1);
  } finally {
    stopFocusFixup();
    realClock();
    unmount();
    createComment.mockRestore();
  }
});

// A document read that has not answered by the request deadline sends the change with no place
// counted, and the form says why beside Send change, in the words the page says for a read of the
// decisions document that failed and then the error such a read ends with: here the quote stands
// twice, so Dispatch refuses the change, and the reader sees why it named no place. The line goes
// once that read settles; the refusal and the draft stay.
test("a Change whose document read has not answered by the request deadline says why beside Send change until that read settles", async () => {
  const answer = Promise.withResolvers<ArtifactText>();
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 2)
  );
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    twoDecisionsText("The decisions this issue made.")
  );
  text.mockReturnValue(answer.promise);
  const why = "Could not load the decisions document. Dispatch did not answer within 30 seconds.";
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    const send = screen.getByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.send_change,
    });
    jest.useFakeTimers();
    await userOnFakeClock().click(send);
    await tick(0);

    // A second short of the request deadline the read is still out: no change has gone, and
    // nothing is said.
    await tick(REQUEST_DEADLINE_MS - 1_000);
    expect(createComment).not.toHaveBeenCalled();
    expect(screen.queryByText(why)).toBeNull();

    // At it the change goes with no place, and the form says why beside Send change.
    await tick(1_000);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(
      createComment.mock.calls[0]?.[1]?.anchor?.occurrence,
      "a send with no reading named a place"
    ).toBeUndefined();
    expect(screen.getByText(why).closest("form")).toBe(send.closest("form"));
    expect(screen.getByText(ANSWER_LINES.change_not_sent)).toBeInTheDocument();

    // The read answers late, and the line goes; the refusal and the draft stay.
    await act(async () =>
      answer.resolve({
        markdown: DOCUMENT_TEXT,
        markdown_tree: displayTree(DOCUMENT_TEXT),
        token: "token",
        version: 1,
      })
    );
    await tick(0);
    expect(screen.queryByText(why)).toBeNull();
    expect(screen.getByText(ANSWER_LINES.change_not_sent)).toBeInTheDocument();
    const field = screen.getByLabelText(CONTROL_LABELS.your_change);
    expect(field instanceof HTMLTextAreaElement ? field.value : "").toBe("Say where.");
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    realClock();
    unmount();
    createComment.mockRestore();
  }
});

// A read the bound ended may fail later, as a read whose body trickles in past the client's
// deadline does: the line that said why it had not settled then says why it failed, since the
// change it sent still named no place.
test("a document read the request deadline ended that then fails says why it failed beside Send change", async () => {
  const answer = Promise.withResolvers<ArtifactText>();
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 2)
  );
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    twoDecisionsText("The decisions this issue made.")
  );
  text.mockReturnValue(answer.promise);
  const unread = STATE_LINES.decisions_document_unreadable;
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    const send = screen.getByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.send_change,
    });
    jest.useFakeTimers();
    await userOnFakeClock().click(send);
    await tick(REQUEST_DEADLINE_MS);
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(alertLines()).toEqual([
      `${unread} Dispatch did not answer within 30 seconds.`,
      ANSWER_LINES.change_not_sent,
    ]);

    await act(async () => answer.reject(new NetworkError(TEXT_PATH, new TypeError("network"))));
    await tick(0);
    expect(alertLines(), "the read's failure left no line").toEqual([
      `${unread} Dispatch could not be reached.`,
      ANSWER_LINES.change_not_sent,
    ]);
  } finally {
    realClock();
    unmount();
    createComment.mockRestore();
  }
});

// A document read Dispatch never answers fails at the API client's own deadline
// (`REQUEST_DEADLINE_MS`), which the client sets as the read starts, before the send's bound
// (`readWithinBound`) is set: the read rejects with a `RequestTimeoutError` while that bound still
// runs. The change goes with no place counted, Send change lets go, and the form says why beside
// Send change, in the words the page says for a read of the decisions document that failed and
// then the timeout's own. The quote stands twice, so Dispatch refuses the change, and the line
// stays beside the refusal: it is the press's own.
test("a Change whose document read times out says why beside Send change, and still does once the change is refused", async () => {
  const answer = Promise.withResolvers<ArtifactText>();
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 2)
  );
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    twoDecisionsText("The decisions this issue made.")
  );
  text.mockReturnValue(answer.promise);
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    const send = screen.getByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.send_change,
    });
    await user.click(send);
    expect(heldNotDisabled(send, CONTROL_LABELS.send_change)).toBe(true);
    expect(createComment).not.toHaveBeenCalled();

    await act(async () => answer.reject(new RequestTimeoutError(TEXT_PATH, REQUEST_DEADLINE_MS)));
    await screen.findByText(ANSWER_LINES.change_not_sent);
    expect(createComment).toHaveBeenCalledTimes(1);
    const anchor = createComment.mock.calls[0]?.[1]?.anchor;
    expect(anchor?.quote).toBe("Draw it once.");
    expect(anchor?.occurrence, "a send with no reading named a place").toBeUndefined();
    expect(heldNotDisabled(send, CONTROL_LABELS.send_change)).toBe(false);
    expect(alertLines(), "the form does not say why the change named no place").toEqual([
      `${STATE_LINES.decisions_document_unreadable} Dispatch did not answer within 30 seconds.`,
      ANSWER_LINES.change_not_sent,
    ]);
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// A document read that fails at the network - as one does at once offline, since the send reads
// the text straight through the API client - gives the change no place either, and the form says
// why beside Send change in the network error's own words, while the change is out.
test("a Change whose document read fails at the network says beside Send change that Dispatch could not be reached", async () => {
  // Dispatch never answers the change.
  const createComment = spyOn(api, "createComment").mockImplementation(
    () => new Promise<never>(() => {})
  );
  const { unmount } = renderActions(
    read,
    new NetworkError(TEXT_PATH, new TypeError("Failed to fetch"))
  );
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Keep one preset.");
    await user.click(screen.getByRole("button", { name: CONTROL_LABELS.send_change }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(createComment.mock.calls[0]?.[1]?.anchor?.occurrence).toBeUndefined();
    expect(alertLines(), "the form does not say why the change named no place").toEqual([
      `${STATE_LINES.decisions_document_unreadable} Dispatch could not be reached.`,
    ]);
  } finally {
    unmount();
    createComment.mockRestore();
  }
});

// The line is the press's own: the next press of Send change takes it away as that press reads
// the document again, and a read that answers says nothing. Dispatch refuses each change here, so
// the form stays drawn with its refusal, and only the read's line has gone.
test("the line a failed document read draws beside Send change goes at the next press, whose read answers", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    ambiguousQuote("Draw it once.", 2)
  );
  const { text, unmount } = renderActions(
    { ...read, decisions: [ABOVE, OWN] },
    new NetworkError(TEXT_PATH, new TypeError("Failed to fetch"))
  );
  const failed = `${STATE_LINES.decisions_document_unreadable} Dispatch could not be reached.`;
  try {
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: CONTROL_LABELS.change_decision }));
    await user.type(screen.getByLabelText(CONTROL_LABELS.your_change), "Say where.");
    const send = screen.getByRole<HTMLButtonElement>("button", {
      name: CONTROL_LABELS.send_change,
    });
    await user.click(send);
    await screen.findByText(ANSWER_LINES.change_not_sent);
    expect(alertLines()).toEqual([failed, ANSWER_LINES.change_not_sent]);

    // The next press reads the document again, and the line goes as it does.
    const answer = Promise.withResolvers<ArtifactText>();
    text.mockReturnValue(answer.promise);
    await user.click(send);
    expect(text).toHaveBeenCalledTimes(2);
    expect(screen.queryByText(failed), "the failed read's line outlived the next press").toBeNull();

    // That read answers: the change names its place, and the form says nothing of the read.
    const markdown = twoDecisionsText("The decisions this issue made.");
    await act(async () =>
      answer.resolve({ markdown, markdown_tree: displayTree(markdown), token: "token", version: 1 })
    );
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    expect(createComment.mock.calls[1]?.[1]?.anchor?.occurrence).toBe(1);
    await waitFor(() => expect(heldNotDisabled(send, CONTROL_LABELS.send_change)).toBe(false));
    expect(alertLines()).toEqual([ANSWER_LINES.change_not_sent]);
  } finally {
    unmount();
    createComment.mockRestore();
  }
});
