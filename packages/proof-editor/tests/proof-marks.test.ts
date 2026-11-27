import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { MarkAction, ProofEditorHandle } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

// The paragraph opens at 0 and its text at 1: "quick brown" runs from 5 to 16, and the caret
// after the closing "-" stands at 22, where a typed ">" makes the " ->" comment shortcut.
const SENTENCE = "The quick brown fox -";
const RANGE = { from: 5, to: 16 };
const AFTER_DASH = 22;

/** Out of margin mode, as a host that draws no margin builds the editor: there the default
 *  installs the mark popover and the " ->" shortcut beside the selection bar. */
const NO_MARGIN = { onMarkClick: undefined, onMarkHover: undefined };

function select(view: EditorView, from: number, to: number): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

/** Types `text` at `at` as a keystroke reaches the editor: every plugin's `handleTextInput` is
 *  offered it first, and the text goes in as the browser puts it in where none takes it. */
function type(view: EditorView, at: number, text: string): boolean {
  select(view, at, at);
  const insert = () => view.state.tr.insertText(text, at, at);
  const taken =
    view.someProp("handleTextInput", (handleTextInput) =>
      handleTextInput(view, at, at, text, insert)
    ) ?? false;
  if (!taken) view.dispatch(insert());
  return taken;
}

/** The window functions the mark popover calls as globals, where `withDomWindow` installs only
 *  `document` and `window`. */
const POPOVER_GLOBALS = ["cancelAnimationFrame", "getComputedStyle", "requestAnimationFrame"];

/** Runs `run` with each of `POPOVER_GLOBALS` answered by the window installed at the moment of
 *  the call, as a browser's globals are its window's, then puts back exactly what was there. */
async function withWindowGlobals(run: () => Promise<void>): Promise<void> {
  const previous = POPOVER_GLOBALS.map(
    (key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const
  );
  for (const key of POPOVER_GLOBALS) {
    Object.defineProperty(globalThis, key, {
      configurable: true,
      value: (...args: unknown[]) =>
        (Reflect.get(globalThis.window, key) as (...args: unknown[]) => unknown).apply(
          globalThis.window,
          args
        ),
      writable: true,
    });
  }
  try {
    await run();
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, key);
      else Object.defineProperty(globalThis, key, descriptor);
    }
  }
}

/** The record marks the document holds: every comment, suggestion and ask. */
function recordMarks(view: EditorView): string[] {
  const names: string[] = [];
  view.state.doc.descendants((node) => {
    for (const mark of node.marks) {
      if (["dispatchAsk", "proofComment", "proofSuggestion"].includes(mark.type.name)) {
        names.push(mark.type.name);
      }
    }
    return true;
  });
  return names;
}

/** What the editor offers and writes for a reader who selects "quick brown", types ">" after
 *  the closing "-" and then types " ok" at the end: the selection bar and its buttons, the mark
 *  popover, the popover hook (`window.proof`), Comment from the keyboard (`commentOnSelection`),
 *  whether the ">" was taken by the " ->" shortcut, which deletes the "-" and opens the comment
 *  composer, and whether the markdown carries the authorship of what was typed. */
function offered(view: EditorView, handle: ProofEditorHandle) {
  const document = view.dom.ownerDocument;
  const bar = document.querySelector<HTMLElement>(".dispatch-action-bar");
  select(view, RANGE.from, RANGE.to);
  const buttons = [...(bar?.querySelectorAll("button") ?? [])].map((button) => button.textContent);
  const barShown = bar !== null && bar.style.display !== "none";
  const popover = document.querySelector(".mark-popover") !== null;
  const hook = "proof" in (document.defaultView ?? {});
  const commented = handle.commentOnSelection();
  const marks = recordMarks(view);
  select(view, AFTER_DASH, AFTER_DASH);
  const arrow =
    view.someProp("handleTextInput", (handleTextInput) =>
      handleTextInput(view, AFTER_DASH, AFTER_DASH, ">", () => view.state.tr.insertText(">"))
    ) ?? false;
  type(view, view.state.doc.content.size - 1, " ok");
  const authored = handle.getMarkdown().includes('data-proof="authored"');
  return { arrow, authored, barShown, buttons, commented, hook, marks, popover };
}

test("an editor built with proofMarks off offers no mark action and writes no mark of its own", async () => {
  const actions: MarkAction[] = [];
  await withWindowGlobals(() =>
    withMarksEditor(
      SENTENCE,
      ({ handle, view }) => {
        expect(view.editable).toBe(true);
        expect(offered(view, handle)).toEqual({
          arrow: false,
          authored: false,
          barShown: false,
          buttons: [],
          commented: false,
          hook: false,
          marks: [],
          popover: false,
        });
        expect(view.dom.ownerDocument.querySelector(".dispatch-action-bar")).toBeNull();
        // What was typed is the reader's text and nothing else: no span carries who typed it.
        expect(handle.getMarkdown()).toBe(`${SENTENCE} ok\n`);
        expect(actions).toEqual([]);
      },
      { ...NO_MARGIN, onMarkAction: (action) => void actions.push(action), proofMarks: false }
    )
  );
});

test("an editor built without the setting still offers every mark action and records authorship", async () => {
  const actions: MarkAction[] = [];
  await withWindowGlobals(() =>
    withMarksEditor(
      SENTENCE,
      ({ handle, view }) => {
        expect(offered(view, handle)).toEqual({
          arrow: true,
          authored: true,
          barShown: true,
          buttons: ["Comment", "Suggest", "Ask"],
          commented: true,
          hook: true,
          marks: ["proofComment"],
          popover: true,
        });
        // The shortcut took the ">" and the "-" before it.
        expect(view.state.doc.textContent).toBe("The quick brown fox  ok");
        expect(actions).toMatchObject([{ kind: "comment", quote: "quick brown" }]);
      },
      { ...NO_MARGIN, onMarkAction: (action) => void actions.push(action) }
    )
  );
});
