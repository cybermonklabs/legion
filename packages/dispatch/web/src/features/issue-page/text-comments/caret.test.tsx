import { afterEach, beforeAll, expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: user-event has no form for a cancelable beforeinput, paste, drop, cut or composition event, nor for a key whose default prevention is read.
import { act, fireEvent, type RenderResult, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { KEY_REPORTS } from "../page-structure";
import type { NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { TEXT_COMPOSER_ATTRIBUTE } from "./composer-store";
import { GATES } from "./gates";
import {
  answerSeededReads,
  clearSelection,
  drawPage,
  drawTree,
  located,
  newClient,
  RISK,
  readAgain,
  rowOf,
  STATEMENT,
  selectWords,
  TEXT_THREADS,
} from "./margin-fixtures";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * R6: Enter on the row of a read-only node puts a caret in its words, where a person reads,
 * selects and moves, and where nothing can be typed: `contenteditable`, `inputmode="none"` and
 * `aria-readonly="true"` on the row's words element (`read-only-caret.ts`), taken away again on
 * Escape and on blur.
 *
 * Each case puts the caret in as a reader does, by Enter on the focused row through the page's
 * keymap (`useTreeKeymap`'s `tree-edit`, then `useNodeEditing`'s `edit`), and calls nothing of
 * the caret itself. A case that asserts something is not there either sets the caret first or
 * sets it after, so the case fails where Enter sets none.
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** The keys the chord is pressed with: under Alt, M types "µ", as a Mac's Option does, so the
 *  composer opens only where the chord is taken by the key's place (`KeyM`), not by the character
 *  it types. */
const CHORD_KEYS = [
  { code: "ControlLeft", key: "Control" },
  { code: "AltLeft", key: "Alt" },
  { code: "KeyM", key: "µ" },
];

/** The kinds whose rows Enter puts a caret in (R6): the nodes of an ask, a comment and a message,
 *  and of a lens, a finding, a risk, a constraint and a result. */
const READ_ONLY_KINDS = [
  "ask",
  "comment",
  "message",
  "lens",
  "finding",
  "risk",
  "constraint",
  "result",
] as const satisfies readonly NodeType[];

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  clearSelection();
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The row of a comment of the issue, drawn by hand in the outline: the node, its row and its
 *  words element. */
function drawComment(
  text = "Words of the comment.",
  tree?: DisplayTree
): {
  readonly row: HTMLElement;
  readonly view: RenderResult;
  readonly words: HTMLElement;
} {
  const view = drawTree([
    node({ id: "post", text, ...(tree === undefined ? {} : { tree }), type: "comment" }),
  ]);
  return { ...located(view, "post"), view };
}

/** Whether `words` carries the three attributes of the caret. */
function hasCaret(words: HTMLElement): boolean {
  return (
    words.getAttribute("contenteditable") === "true" &&
    words.getAttribute("inputmode") === "none" &&
    words.getAttribute("aria-readonly") === "true"
  );
}

/** Whether `words` carries none of the caret's three attributes. */
function lacksCaret(words: HTMLElement): boolean {
  return (
    !words.hasAttribute("contenteditable") &&
    !words.hasAttribute("inputmode") &&
    !words.hasAttribute("aria-readonly")
  );
}

/** What the row says a key could not do, where it says anything (`[data-row-report]`). */
function reportOf(row: HTMLElement): string | undefined {
  return row.querySelector("[data-row-report]")?.textContent ?? undefined;
}

/** Focuses `row` and presses Enter on it, as a reader on the keyboard does. */
async function enterOn(row: HTMLElement): Promise<void> {
  act(() => row.focus());
  await userEvent.setup().keyboard("{Enter}");
}

/** Enter on `row`, and says the caret went into `words` with the focus, so a case that asserts
 *  what the caret does or does not do fails where Enter sets none. */
async function enterTheCaret(row: HTMLElement, words: HTMLElement): Promise<void> {
  await enterOn(row);
  expect(
    hasCaret(words),
    "Enter on the row set no contenteditable, inputmode and aria-readonly on its words"
  ).toBe(true);
  expect(document.activeElement, "Enter left the focus off the row's words").toBe(words);
}

test("row focused, Enter on an ask, comment, message, lens, finding, risk, constraint or result: the caret is at the start of the text, with its three attributes", async () => {
  const view = drawTree(
    READ_ONLY_KINDS.map((kind) =>
      node({ id: `${kind}-row`, text: `Words of the ${kind}.`, type: kind })
    )
  );
  try {
    const failed: string[] = [];
    const reported: string[] = [];
    for (const kind of READ_ONLY_KINDS) {
      const { row, words } = located(view, `${kind}-row`);
      await enterOn(row);
      const selection = window.getSelection();
      const atStart =
        selection?.isCollapsed === true &&
        selection.anchorOffset === 0 &&
        selection.anchorNode !== null &&
        words.contains(selection.anchorNode) &&
        (words.textContent ?? "").startsWith(selection.anchorNode.textContent ?? "\u0000");
      if (!hasCaret(words) || !atStart || document.activeElement !== words) failed.push(kind);
      const report = reportOf(row);
      if (report !== undefined) reported.push(`${kind}: ${report}`);
    }
    expect(failed, "the kinds whose row has no caret at the start of its text").toEqual([]);
    expect(reported, "the rows that said something of Enter").toEqual([]);
  } finally {
    view.unmount();
  }
});

test("row focused, Enter on a row that draws none of its node's text: the row says it has no text to put the caret in, and no caret is set", async () => {
  const view = drawTree([
    node({ id: "empty-comment", text: "", type: "comment" }),
    node({ id: "unjudged-lens", text: "", type: "lens" }),
  ]);
  try {
    const wrong: string[] = [];
    for (const id of ["empty-comment", "unjudged-lens"]) {
      const { row, words } = located(view, id);
      await enterOn(row);
      if (reportOf(row) !== KEY_REPORTS.no_text || !lacksCaret(words)) {
        wrong.push(`${id}: ${reportOf(row) ?? "no report"}`);
      }
    }
    expect(wrong, "the rows with no text that did not say so, or took a caret").toEqual([]);
  } finally {
    view.unmount();
  }
});

// Part Y's block editor takes the caret of a block node (a statement, a check, a plan task, a
// decision, a callout, a reading), and the read-only caret is for the others.
test.skipIf(!GATES.PART_Y)(
  "row focused, Enter on a block node: the block's editor takes the caret",
  async () => {
    stubs = answerSeededReads({ threads: TEXT_THREADS });
    const client = newClient();
    const view = drawPage(client, "conversation");
    try {
      const id = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);
      const row = await waitFor(() => {
        const drawn = rowOf(view, id);
        expect(drawn, "the statement's row is not drawn").not.toBeNull();
        return drawn as HTMLElement;
      });
      act(() => row.focus());
      await userEvent.setup().keyboard("{Enter}");
      const { words } = located(view, id);
      expect(
        words.hasAttribute("aria-readonly"),
        "Enter on a block node put the read-only caret in its words"
      ).toBe(false);
    } finally {
      view.finish();
      client.clear();
    }
  }
);

test("row focused, a press on the text: the focus moves to the row and no caret is set", async () => {
  const { row, view, words } = drawComment();
  try {
    await userEvent.setup().click(words);
    expect(document.activeElement, "the press left the focus off the row").toBe(row);
    expect(lacksCaret(words), "a press on the text set the caret").toBe(true);
    // Enter from where the press left the focus puts the caret in, so the row is one that takes it.
    await enterTheCaret(row, words);
  } finally {
    view.unmount();
  }
});

test("any, a drag over the text: the text is selected and no caret attribute is set", async () => {
  const { row, view, words } = drawComment();
  try {
    // The press lands on the first letter of "the comment", and the release just past its last.
    const from = (words.textContent ?? "").indexOf("the comment");
    await userEvent
      .setup()
      .pointer([
        { keys: "[MouseLeft>]", offset: from, target: words },
        { offset: from + "the comment".length },
        { keys: "[/MouseLeft]" },
      ]);
    expect(window.getSelection()?.toString(), "the drag selected no text").toBe("the comment");
    expect(lacksCaret(words), "a drag over the text set the caret").toBe(true);
    // Enter after the drag puts the caret in, so the row is one that takes it.
    await enterTheCaret(row, words);
  } finally {
    view.unmount();
  }
});

test("caret, Left, Right, Up, Down, Home and End: the caret moves and the text does not change", async () => {
  const { row, view, words } = drawComment();
  try {
    await enterTheCaret(row, words);
    const before = words.textContent;
    const cancelled: string[] = [];
    for (const key of ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"]) {
      // user-event does not answer whether the page cancelled the key, which this case reads.
      const accepted = fireEvent.keyDown(words, { cancelable: true, key });
      if (!accepted) cancelled.push(key);
    }
    expect(cancelled, "keys the page cancelled, which the browser moves the caret by").toEqual([]);
    expect(words.textContent, "a movement key changed the text").toBe(before);
    expect(hasCaret(words), "a movement key took the caret away").toBe(true);
    expect(document.activeElement, "a movement key moved the focus off the words").toBe(words);
  } finally {
    view.unmount();
  }
});

test("caret, Shift with an arrow: the selection grows and the control of the selection table shows", async () => {
  stubs = answerSeededReads({ risk: true, threads: [] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const { row, words } = await waitFor(() => located(view, RISK));
    await enterTheCaret(row, words);
    // user-event does not answer whether the page cancelled the key, which this case reads.
    const accepted = fireEvent.keyDown(words, {
      cancelable: true,
      key: "ArrowRight",
      shiftKey: true,
    });
    expect(accepted, "Shift with an arrow was cancelled").toBe(true);
    // The browser grows the selection from the caret, at the start of the words; happy-dom moves
    // none.
    selectWords(row, "A slow");
    await waitFor(() =>
      expect(
        view.queryByRole("button", { name: "Comment" }),
        "the grown selection shows no control"
      ).not.toBeNull()
    );
  } finally {
    await client.cancelQueries();
    view.finish();
    client.clear();
  }
});

test("caret, a printable key, Backspace, Delete, Enter, a paste, a drop or an IME composition: nothing changes in the text or in the stored text", async () => {
  const { row, view, words } = drawComment();
  try {
    await enterTheCaret(row, words);
    const before = words.textContent;
    const typed: string[] = [];
    for (const inputType of [
      "insertText",
      "deleteContentBackward",
      "deleteContentForward",
      "insertParagraph",
      "insertFromPaste",
      "insertFromDrop",
    ]) {
      const accepted = words.dispatchEvent(
        new InputEvent("beforeinput", { bubbles: true, cancelable: true, data: "x", inputType })
      );
      if (accepted) typed.push(inputType);
    }
    for (const type of ["paste", "drop", "cut"]) {
      const accepted = words.dispatchEvent(new Event(type, { bubbles: true, cancelable: true }));
      if (accepted) typed.push(type);
    }
    expect(typed, "inputs the caret's text let through").toEqual([]);
    // The keys themselves, pressed in the caret: none of them types, and Enter puts no second
    // caret in and reports nothing.
    await userEvent.setup().keyboard("x{Backspace}{Delete}{Enter}");
    expect(words.textContent, "an input changed the text").toBe(before);
    expect(hasCaret(words), "a key took the caret away").toBe(true);
    expect(reportOf(row), "a key in the caret said something at the row").toBeUndefined();
    // An IME composition: an IME may go on after a cancelled `compositionstart` (UI Events), and
    // the `beforeinput` of its text cannot be cancelled (Input Events Level 2), so its start takes
    // the caret away and gives the focus to the row, as Escape does, before any of its text comes.
    // `fireEvent` sends it inside `act`, since the row's focus tells the tree.
    expect(fireEvent.compositionStart(words), "the composition's start was not cancelled").toBe(
      false
    );
    expect(lacksCaret(words), "a composition's start left the caret in the words").toBe(true);
    expect(document.activeElement, "a composition's start left the focus off the row").toBe(row);
    words.dispatchEvent(
      new InputEvent("beforeinput", {
        bubbles: true,
        cancelable: false,
        data: "é",
        inputType: "insertCompositionText",
      })
    );
    expect(words.isContentEditable, "the words still take the composition's text").toBe(false);
    expect(words.textContent, "the composition changed the text").toBe(before);
  } finally {
    view.unmount();
  }
});

test("caret, $mod+Alt+m with a selection: the composer of the selection table opens", async () => {
  stubs = answerSeededReads({ risk: true, threads: [] });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const { row, words } = await waitFor(() => located(view, RISK));
    await enterTheCaret(row, words);
    selectWords(row, "page");
    expect(document.activeElement, "the selection left the focus off the row's words").toBe(words);
    await userEvent
      .setup({ keyboardMap: CHORD_KEYS })
      .keyboard("{Control>}{Alt>}[KeyM]{/Alt}{/Control}");
    await waitFor(() => {
      const column = view.queryByRole("region", { name: COLUMN });
      expect(
        column?.querySelector(`[${TEXT_COMPOSER_ATTRIBUTE}]`),
        "the chord opened no composer in the column"
      ).not.toBeNull();
    });
  } finally {
    await client.cancelQueries();
    view.finish();
    client.clear();
  }
});

test("caret, a press on a link in the text: the link is followed", async () => {
  const linkTree: DisplayTree = {
    content: [
      {
        content: [
          { text: "See ", type: "text" },
          {
            marks: [{ attrs: { href: "https://docs.example.test/page" }, type: "link" }],
            text: "the docs",
            type: "text",
          },
          { text: ".", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const { row, view, words } = drawComment(
    "See [the docs](https://docs.example.test/page).",
    linkTree
  );
  try {
    await enterTheCaret(row, words);
    const link = words.querySelector<HTMLAnchorElement>("a[href]");
    expect(link, "the row's words draw no link").not.toBeNull();
    // Catch the click after the row's handlers ran: we read whether the caret left it uncanceled,
    // then cancel happy-dom's own navigation so `window.location` stays on the test origin for
    // the tests after this one.
    let canceledByCaret: boolean | undefined;
    const catchNavigation = (event: MouseEvent) => {
      canceledByCaret = event.defaultPrevented;
      event.preventDefault();
    };
    document.addEventListener("click", catchNavigation);
    try {
      (link as HTMLAnchorElement).dispatchEvent(
        new MouseEvent("click", { bubbles: true, cancelable: true })
      );
    } finally {
      document.removeEventListener("click", catchNavigation);
    }
    expect(
      canceledByCaret,
      "the press on the link was cancelled, so the browser follows nothing"
    ).toBe(false);
    expect(words.textContent, "the press changed the text").toContain("the docs");
  } finally {
    view.unmount();
  }
});

test("caret, Escape: the attribute goes and the focus is on the row", async () => {
  const { row, view, words } = drawComment();
  try {
    await enterTheCaret(row, words);
    await userEvent.setup().keyboard("{Escape}");
    expect(lacksCaret(words), "Escape left the caret's attributes").toBe(true);
    expect(document.activeElement, "Escape left the focus off the row").toBe(row);
  } finally {
    view.unmount();
  }
});

test("caret, blur: the attribute goes", async () => {
  const { row, view, words } = drawComment();
  try {
    await enterTheCaret(row, words);
    // A press on the page where nothing takes the focus, so the focus leaves the words for none.
    await userEvent.setup().click(document.body);
    expect(lacksCaret(words), "blur left the caret's attributes").toBe(true);
  } finally {
    view.unmount();
  }
});

test("caret, the tree read is read again: the caret stays in the same node's text", async () => {
  stubs = answerSeededReads({ risk: true, threads: TEXT_THREADS });
  const client = newClient();
  const view = drawPage(client, "conversation");
  try {
    const { row, words } = await waitFor(() => located(view, RISK));
    await enterTheCaret(row, words);
    await readAgain(client);
    const again = located(view, RISK);
    expect(again.words, "the re-read replaced the node's words element").toBe(words);
    expect(hasCaret(again.words), "the re-read took the caret's attributes away").toBe(true);
    expect(document.activeElement, "the re-read took the focus out of the words").toBe(words);
  } finally {
    await client.cancelQueries();
    view.finish();
    client.clear();
  }
});
