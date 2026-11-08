import { expect, test } from "bun:test";
import { TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { ySyncPluginKey } from "y-prosemirror";
import { withMarksEditor } from "./marks-editor";

// The editor takes a keystroke from the DOM: the browser inserts the character, prosemirror-view
// reads the change back and takes the selection the browser is left holding (`readDOMChange`).
// These cases are that read-back, with the caret each way the browser can leave it.

/**
 * What `readDOMChange` does with a character the browser has just inserted at the caret: the
 * editor's own typing path first (`handleTextInput`, which is where an input rule or this
 * module's plugin sees the keystroke), then the insert, with `caret` saying where the browser
 * left the selection - after the character, as Chromium usually does, or in front of it, as it
 * does for the first character of a block it has just drawn.
 */
function theBrowserTyped(view: EditorView, text: string, caret: "after" | "before"): number {
  const at = view.state.selection.from;
  view.someProp("handleTextInput", (handleTextInput) =>
    handleTextInput(view, at, at, text, () => view.state.tr.insertText(text, at, at))
  );
  const tr = view.state.tr.insertText(text, at, at);
  view.dispatch(
    tr.setSelection(TextSelection.create(tr.doc, caret === "before" ? at : at + text.length))
  );
  return at;
}

/** The text of the document's last block. */
function lastBlock(view: EditorView): string {
  return view.state.doc.lastChild?.textContent ?? "";
}

test("a character the browser leaves the caret in front of still ends with the caret after it", async () => {
  await withMarksEditor("Original paragraph.", ({ press, view }) => {
    view.dispatch(
      view.state.tr.setSelection(
        TextSelection.create(view.state.doc, view.state.doc.content.size - 1)
      )
    );
    press("Enter");
    expect(lastBlock(view)).toBe("");

    const at = theBrowserTyped(view, "P", "before");
    expect(view.state.selection.from).toBe(at + 1);
    // The rest of the word goes in from that caret, which is what the reader relies on.
    theBrowserTyped(view, "a", "after");
    theBrowserTyped(view, "n", "after");
    expect(lastBlock(view)).toBe("Pan");
  });
});

test("a character the browser leaves the caret after is left exactly where it is", async () => {
  await withMarksEditor("Original paragraph.", ({ view }) => {
    view.dispatch(
      view.state.tr.setSelection(
        TextSelection.create(view.state.doc, view.state.doc.content.size - 1)
      )
    );
    const at = theBrowserTyped(view, "x", "after");
    expect(view.state.selection.from).toBe(at + 1);
    expect(lastBlock(view)).toBe("Original paragraph.x");
  });
});

test("another person's insert at the caret leaves the caret in front of it, as it always did", async () => {
  await withMarksEditor("Original paragraph.", ({ view }) => {
    const at = 1;
    view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, at)));
    // A change arriving through the binding takes no one's typing path and must not carry the
    // reader's caret: their words stay in front of what another person writes at their cursor.
    const tr = view.state.tr.insertText("Their words. ", at, at);
    view.dispatch(
      tr.setSelection(TextSelection.create(tr.doc, at)).setMeta(ySyncPluginKey, {
        isChangeOrigin: true,
      })
    );
    expect(view.state.selection.from).toBe(at);
    expect(lastBlock(view)).toBe("Their words. Original paragraph.");
  });
});
