import { expect, test } from "bun:test";
import { TextSelection, type Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type { EditorReferencePicker, ReferencePickerCaret } from "../src/lib";
import { withMarksEditor } from "./marks-editor";

/** The host's side of the picker: every caret it was told about, in order, and the keys it was
 *  offered, with the keys its list takes. */
interface TestHost {
  readonly picker: EditorReferencePicker;
  readonly carets: Array<ReferencePickerCaret | undefined>;
  readonly keys: string[];
}

function hostPicker(takes: readonly string[] = []): TestHost {
  const carets: Array<ReferencePickerCaret | undefined> = [];
  const keys: string[] = [];
  return {
    carets,
    keys,
    picker: {
      onCaret: (caret) => void carets.push(caret),
      pressKey: (key) => {
        keys.push(key);
        return takes.some((taken) => taken === key);
      },
    },
  };
}

/** Focuses or blurs the editor as the browser does: ProseMirror offers the event to every
 *  plugin's handler first. happy-dom delivers no synthetic focus event to ProseMirror's
 *  listener, so the test makes the call itself (`reference-decorations.test.ts` does the same). */
function setFocus(view: EditorView, focused: boolean): void {
  const event = new window.FocusEvent(focused ? "focus" : "blur");
  view.someProp("handleDOMEvents", (handlers) => {
    handlers[focused ? "focus" : "blur"]?.(view, event);
  });
}

function select(view: EditorView, from: number, to = from): void {
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, from, to)));
}

/** The key as ProseMirror's own listener offers it to the plugins, answering whether a plugin
 *  took it - which is what keeps it from reaching the document. */
function press(view: EditorView, key: string): boolean {
  const event = new window.KeyboardEvent("keydown", { key }) as unknown as KeyboardEvent;
  return view.someProp("handleKeyDown", (handleKeyDown) => handleKeyDown(view, event)) === true;
}

/** Counts what the handle dispatches, as ProseMirror's own `dispatch` is called: one pick must
 *  be one transaction, since that is what reaches a collaborator and the stored text. */
function countDispatches(view: EditorView): () => number {
  let count = 0;
  const original = view.dispatch.bind(view);
  view.dispatch = (transaction: Transaction) => {
    count += 1;
    original(transaction);
  };
  return () => count;
}

test("the editor reports the text before the caret, and nothing while it is not focused", async () => {
  const host = hostPicker();
  await withMarksEditor("See the spec", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    // Nothing is reported for an unfocused document: there is nothing to pick for.
    expect(host.carets).toEqual([undefined]);

    setFocus(view, true);
    // "See the spec" opens its paragraph at 0, so its text runs from 1: a caret at 5 stands
    // after "See ".
    select(view, 5);
    expect(host.carets.at(-1)?.text).toBe("See ");

    select(view, 13);
    expect(host.carets.at(-1)?.text).toBe("See the spec");

    // A selection is not a caret, and nothing stands before it to read a trigger from.
    select(view, 5, 8);
    expect(host.carets.at(-1)).toBeUndefined();

    select(view, 13);
    expect(host.carets.at(-1)?.text).toBe("See the spec");
    setFocus(view, false);
    expect(host.carets.at(-1)).toBeUndefined();
  });
});

test("a caret in code is no door: its trigger characters are the author's own text", async () => {
  const host = hostPicker();
  await withMarksEditor("    const a = 1;", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    expect(view.state.doc.child(0).type.name).toBe("code_block");
    select(view, 5);
    expect(host.carets.at(-1)).toBeUndefined();
  });
});

test("a pick is one dispatched transaction, and the stored markdown then holds the reference", async () => {
  const host = hostPicker();
  await withMarksEditor("See the spec", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    select(view, 13);
    // The two characters of the `[[` door, as the reader types them.
    view.dispatch(view.state.tr.insertText(" [[", 13));
    expect(host.carets.at(-1)?.text).toBe("See the spec [[");

    const dispatches = countDispatches(view);
    // The row the host's picker took: its address, in place of the trigger the reader typed.
    expect(
      handle.insertReference({ end: 15, start: 13, text: "dispatch://CORE-1", trigger: "[[" })
    ).toBe(true);
    expect(dispatches()).toBe(1);

    expect(handle.getMarkdown().trim()).toBe("See the spec dispatch://CORE-1");
    // The caret follows the words written, so the reader types on after the reference.
    expect(view.state.selection.head).toBe(31);
    expect(host.carets.at(-1)?.text).toBe("See the spec dispatch://CORE-1");
  });
});

test("an insertion the caret's text no longer fits is refused, and nothing is dispatched", async () => {
  const host = hostPicker();
  await withMarksEditor("See the spec", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    select(view, 13);
    const stored = handle.getMarkdown();

    const dispatches = countDispatches(view);
    // Offsets past the text the editor last reported: the reader deleted what the row stood
    // over while the list was open.
    expect(
      handle.insertReference({ end: 40, start: 38, text: "dispatch://CORE-1", trigger: "[[" })
    ).toBe(false);
    expect(
      handle.insertReference({ end: 2, start: 5, text: "dispatch://CORE-1", trigger: "[[" })
    ).toBe(false);
    expect(dispatches()).toBe(0);

    // A selection is not a caret: a pick has no stretch of one block's text to replace. The
    // selection itself is a transaction, so the count starts again after it.
    select(view, 5, 8);
    const afterSelect = dispatches();
    expect(
      handle.insertReference({ end: 5, start: 4, text: "dispatch://CORE-1", trigger: "[[" })
    ).toBe(false);
    expect(dispatches()).toBe(afterSelect);
    expect(handle.getMarkdown()).toBe(stored);
  });
});

// A document other writers share can move under an open list: a collaborator's edit landing
// between the caret report and the pick leaves the offsets naming somebody else's words.
test("a pick whose stretch is no longer its own trigger is refused", async () => {
  const host = hostPicker();
  await withMarksEditor("See the spec", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    select(view, 13);
    view.dispatch(view.state.tr.insertText(" [[", 13));

    // A collaborator's word written ahead of the trigger: the offsets the host answers with
    // still fit the block's text, and the text under them is now somebody else's.
    view.dispatch(view.state.tr.insertText("Now ", 1));
    expect(host.carets.at(-1)?.text).toBe("Now See the spec [[");
    const stored = handle.getMarkdown();

    const dispatches = countDispatches(view);
    expect(
      handle.insertReference({ end: 15, start: 13, text: "dispatch://CORE-1", trigger: "[[" })
    ).toBe(false);
    expect(dispatches()).toBe(0);
    expect(handle.getMarkdown()).toBe(stored);
  });
});

// What the document's host answers for a party row (`DocumentRefPicker`): the words it shows
// and the address it names, which is a link mark here and `[words](address)` in the stored
// Markdown - the same mention every other writing box stores (R4). Written as plain text, that
// Markdown would be stored escaped, and every reader of the document would see `[Planner](`.
test("a pick naming an address is stored as the link that names it", async () => {
  const host = hostPicker();
  await withMarksEditor("Ask now", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    select(view, 5);
    view.dispatch(view.state.tr.insertText("@plan ", 5));

    const dispatches = countDispatches(view);
    expect(
      handle.insertReference({
        end: 9,
        href: "dispatch://agent/abc",
        start: 4,
        text: "Planner",
        trigger: "@",
      })
    ).toBe(true);
    expect(dispatches()).toBe(1);
    expect(handle.getMarkdown().trim()).toBe("Ask [Planner](dispatch://agent/abc) now");

    // The reader types on after the mention, not inside it: the link mark is left behind.
    view.dispatch(view.state.tr.insertText("!", view.state.selection.head));
    expect(handle.getMarkdown().trim()).toBe("Ask [Planner](dispatch://agent/abc)! now");
  });
});

test("a key the host's list takes never reaches the document, and every other key does", async () => {
  const host = hostPicker(["ArrowDown", "Enter"]);
  await withMarksEditor("See the spec", ({ handle, view }) => {
    handle.setReferencePicker(host.picker);
    setFocus(view, true);
    select(view, 13);

    expect(press(view, "ArrowDown")).toBe(true);
    expect(press(view, "Enter")).toBe(true);
    // The document still holds one paragraph: neither key reached it.
    expect(view.state.doc.childCount).toBe(1);

    expect(press(view, "ArrowLeft")).toBe(false);
    expect(host.keys).toEqual(["ArrowDown", "Enter", "ArrowLeft"]);

    // With no picker hosted the document takes its keys back, unoffered.
    handle.setReferencePicker(null);
    expect(press(view, "ArrowDown")).toBe(false);
    expect(host.keys).toEqual(["ArrowDown", "Enter", "ArrowLeft"]);
  });
});
