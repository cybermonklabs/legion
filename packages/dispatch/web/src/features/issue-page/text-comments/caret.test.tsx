import { afterEach, beforeAll, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: user-event has no form for a cancelable beforeinput, paste, drop, cut or composition event, nor for a key whose default prevention is read.
import { act, fireEvent, type RenderResult, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DisplayTree } from "../../../api/types";
import { warmMarkdownRenderer } from "../../refs/markdown-engine";
import { node, SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import type { NodeType } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import { GATES } from "./gates";
import {
  answerSeededReads,
  clearSelection,
  drawFrame,
  drawPage,
  drawTree,
  readAgain,
  rowOf,
  STATEMENT,
  selectWords,
  setSelection,
  TEXT_THREADS,
} from "./margin-fixtures";
import { caretIn } from "./read-only-caret";
import { SelectionComment } from "./SelectionComment";
import { TEXT_COMPOSER_ATTRIBUTE } from "./TextMargin";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/**
 * R6: Enter on the row of a read-only node puts a caret in its words, where a person reads,
 * selects and moves, and where nothing can be typed. The caret is `caretIn(row)`: `contenteditable`,
 * `inputmode="none"` and `aria-readonly="true"` on the row's words element, taken away again on
 * Escape, on blur and by the function it answers.
 *
 * Each case drives `caretIn` on a row it draws, as Enter will (t11 wires the key at G5: until then
 * Enter keeps the page's "no text to put the caret in" report, a row of part I's browser file
 * reads). A case that asserts something is not there first sets the caret, so the case fails
 * where `caretIn` sets none.
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

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

/** The row `view` draws for `nodeId`, and its own words element. */
function located(
  view: RenderResult,
  nodeId: string
): { readonly row: HTMLElement; readonly words: HTMLElement } {
  const row = rowOf(view, nodeId);
  if (row === null) throw new Error(`no row is drawn for ${nodeId}`);
  const words = [...row.querySelectorAll<HTMLElement>("[data-node-words]")].find(
    (one) => one.closest("[data-node-id]") === row
  );
  if (words === undefined) throw new Error(`the row of ${nodeId} draws no words element`);
  return { row, words };
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

/** Puts the caret in `row` and says it did, so a case that asserts what the caret does not do
 *  fails where it is not set. Answers what takes it away. */
function setCaret(row: HTMLElement, words: HTMLElement): () => void {
  const holder: { away: (() => void) | null } = { away: null };
  act(() => {
    holder.away = caretIn(row);
  });
  expect(holder.away, "caretIn answers no function that takes the caret away").not.toBeNull();
  expect(hasCaret(words), "caretIn sets no contenteditable, inputmode and aria-readonly").toBe(
    true
  );
  return holder.away as () => void;
}

test("row focused, Enter on an ask, comment, message, lens, finding, risk, constraint or result: the caret is at the start of the text, with its three attributes", () => {
  const view = drawTree(
    READ_ONLY_KINDS.map((kind) =>
      node({ id: `${kind}-row`, text: `Words of the ${kind}.`, type: kind })
    )
  );
  const failed: string[] = [];
  for (const kind of READ_ONLY_KINDS) {
    const { row, words } = located(view, `${kind}-row`);
    act(() => row.focus());
    const holder: { away: (() => void) | null } = { away: null };
    act(() => {
      holder.away = caretIn(row);
    });
    const selection = window.getSelection();
    const atStart =
      selection?.isCollapsed === true &&
      selection.anchorOffset === 0 &&
      selection.anchorNode !== null &&
      words.contains(selection.anchorNode);
    if (!hasCaret(words) || !atStart) failed.push(kind);
    holder.away?.();
    clearSelection();
  }
  expect(failed, "the kinds whose row has no caret at the start of its text").toEqual([]);
});

// Part Y's block editor takes the caret of a block node (a statement, a check, a plan task, a
// decision, a callout, a reading), and the read-only caret is for the others.
test.skipIf(!GATES.PART_Y)(
  "row focused, Enter on a block node: the block's editor takes the caret",
  async () => {
    stubs = answerSeededReads({ threads: TEXT_THREADS });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
  // The caret `caretIn` sets, taken away again, so the case fails where there is none to take.
  setCaret(row, words)();
  expect(lacksCaret(words), "the function caretIn answers left the caret's attributes").toBe(true);
  await userEvent.setup().click(words);
  expect(
    document.activeElement?.closest("[data-node-row]"),
    "the press left the focus off the row"
  ).toBe(row);
  expect(lacksCaret(words), "a press on the text set the caret").toBe(true);
  view.unmount();
});

test("any, a drag over the text: the text is selected and no caret attribute is set", () => {
  const { row, view, words } = drawComment();
  setCaret(row, words)();
  fireEvent.pointerDown(words);
  fireEvent.mouseDown(words);
  selectWords(row, "the comment");
  fireEvent.pointerUp(words);
  fireEvent.mouseUp(words);
  expect(window.getSelection()?.toString(), "the drag selected no text").toBe("the comment");
  expect(lacksCaret(words), "a drag over the text set the caret").toBe(true);
  view.unmount();
});

test("caret, Left, Right, Up, Down, Home and End: the caret moves and the text does not change", () => {
  const { row, view, words } = drawComment();
  setCaret(row, words);
  const before = words.textContent;
  const cancelled: string[] = [];
  for (const key of ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "Home", "End"]) {
    const accepted = fireEvent.keyDown(words, { cancelable: true, key });
    if (!accepted) cancelled.push(key);
  }
  expect(cancelled, "keys the page cancelled, which the browser moves the caret by").toEqual([]);
  expect(words.textContent, "a movement key changed the text").toBe(before);
  view.unmount();
});

test("caret, Shift with an arrow: the selection grows and the control of the selection table shows", async () => {
  stubs = answerSeededReads({ threads: [] });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = drawFrame(client, "conversation", <SelectionComment />);
  try {
    const id = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);
    const { row, words } = await waitFor(() => located(view, id));
    setCaret(row, words);
    const accepted = fireEvent.keyDown(words, {
      cancelable: true,
      key: "ArrowRight",
      shiftKey: true,
    });
    expect(accepted, "Shift with an arrow was cancelled").toBe(true);
    // The browser grows the selection by a character; happy-dom moves none.
    selectWords(row, "The");
    await waitFor(() =>
      expect(
        view.queryByRole("button", { name: "Comment" }),
        "the grown selection shows no control"
      ).not.toBeNull()
    );
  } finally {
    view.finish();
    client.clear();
  }
});

test("caret, a printable key, Backspace, Delete, Enter, a paste, a drop or an IME composition: nothing changes in the text or in the stored text", () => {
  const { row, view, words } = drawComment();
  setCaret(row, words);
  const before = words.textContent;
  const typed: string[] = [];
  for (const inputType of [
    "insertText",
    "deleteContentBackward",
    "deleteContentForward",
    "insertParagraph",
    "insertFromPaste",
    "insertFromDrop",
    "insertCompositionText",
  ]) {
    const accepted = words.dispatchEvent(
      new InputEvent("beforeinput", { bubbles: true, cancelable: true, data: "x", inputType })
    );
    if (accepted) typed.push(inputType);
  }
  for (const type of ["paste", "drop", "cut", "compositionstart"]) {
    const accepted = words.dispatchEvent(new Event(type, { bubbles: true, cancelable: true }));
    if (accepted) typed.push(type);
  }
  expect(typed, "inputs the caret's text let through").toEqual([]);
  expect(words.textContent, "an input changed the text").toBe(before);
  view.unmount();
});

test("caret, $mod+Alt+m with a selection: the composer of the selection table opens", async () => {
  stubs = answerSeededReads({ threads: [] });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = drawFrame(client, "conversation", <SelectionComment />);
  try {
    const id = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);
    const { row, words } = await waitFor(() => located(view, id));
    setCaret(row, words);
    selectWords(row, "page");
    fireEvent.keyDown(words, { altKey: true, code: "KeyM", ctrlKey: true, key: "µ" });
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

test("caret, a press on a link in the text: the link is followed", () => {
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
  setCaret(row, words);
  const link = words.querySelector<HTMLAnchorElement>("a[href]");
  expect(link, "the row's words draw no link").not.toBeNull();
  // Catch the click after the row's handlers ran: we read whether `caretIn` left it uncanceled,
  // then cancel happy-dom's own navigation so `window.location` stays on the test origin for the
  // tests after this one.
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
  view.unmount();
});

test("caret, Escape: the attribute goes and the focus is on the row", () => {
  const { row, view, words } = drawComment();
  setCaret(row, words);
  fireEvent.keyDown(words, { key: "Escape" });
  expect(lacksCaret(words), "Escape left the caret's attributes").toBe(true);
  expect(document.activeElement, "Escape left the focus off the row").toBe(row);
  view.unmount();
});

test("caret, blur: the attribute goes", () => {
  const { row, view, words } = drawComment();
  setCaret(row, words);
  fireEvent.blur(words);
  fireEvent.focusOut(words);
  expect(lacksCaret(words), "blur left the caret's attributes").toBe(true);
  view.unmount();
});

test("caret, the tree read is read again: the caret stays in the same node's text", async () => {
  stubs = answerSeededReads({ threads: TEXT_THREADS });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = drawPage(client, "conversation");
  try {
    const id = blockNodeId(SPEC_DOCUMENT.slug, STATEMENT);
    const { row, words } = await waitFor(() => located(view, id));
    setCaret(row, words);
    setSelection(document.createRange());
    await readAgain(client);
    const again = located(view, id);
    expect(again.words, "the re-read replaced the node's words element").toBe(words);
    expect(hasCaret(again.words), "the re-read took the caret's attributes away").toBe(true);
  } finally {
    await client.cancelQueries();
    view.finish();
    client.clear();
  }
});
