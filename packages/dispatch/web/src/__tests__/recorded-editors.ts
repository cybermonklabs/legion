import { TextSelection } from "@milkdown/kit/prose/state";
import { within } from "@testing-library/react";

import { loadDocumentTransport } from "../features/doc/connection";
import { type ConstructEditor, constructEditor, type EditorHandle } from "../features/doc/editor";
import type { DocumentRuntimeValue } from "../features/doc/runtime";

/** The documents' own editor, each handle it built, and the lookup of one by its name. */
export interface RecordedEditors {
  /** Every handle the editor built, in the order it built them. */
  readonly handles: EditorHandle[];
  /** The editor named `name` once it holds its text, among the elements of `scope` (the whole
   *  document where none is given), with the handle that built it. */
  named(name: string, scope?: HTMLElement): Promise<{ box: HTMLElement; handle: EditorHandle }>;
  /** The runtime a test provides (`DocumentRuntime`) in place of the page's own. */
  readonly runtime: DocumentRuntimeValue;
}

/**
 * The documents' own editor (`features/doc/editor.ts`'s `constructEditor`), keeping each handle
 * it binds: for a test of a field drawn in the real editor, which it drives through the editor's
 * own transactions, since happy-dom delivers no keystroke to ProseMirror.
 */
export function recordedEditors(): RecordedEditors {
  const handles: EditorHandle[] = [];
  const recording: ConstructEditor = async (root, options) => {
    const construction = await constructEditor(root, options);
    return {
      ...construction,
      bind: async () => {
        const handle = await construction.bind();
        handles.push(handle);
        return handle;
      },
    };
  };
  return {
    handles,
    async named(name, scope = document.body) {
      const box = await within(scope).findByRole("textbox", { name });
      const handle = handles.find((candidate) => candidate.view.dom === box);
      if (handle === undefined) throw new Error(`no editor of this test is named ${name}`);
      return { box, handle };
    },
    runtime: { constructEditor: recording, loadTransport: loadDocumentTransport },
  };
}

/** Where the editor's last text block ends: the caret a click after its last word leaves. */
function endOfText(handle: EditorHandle): number {
  let end = 0;
  handle.view.state.doc.descendants((node, pos) => {
    if (node.isTextblock) end = pos + 1 + node.content.size;
    return true;
  });
  return end;
}

/** Writes `text` after the editor's last word as a keystroke reaches the editor - every
 *  plugin's `handleTextInput` is offered it first, as the authorship tracker of the documents'
 *  editor reads it, and the text goes in as the browser puts it in where none takes it - and
 *  makes the run `bold` of it bold through the editor's own strong mark, as its Mod-b does over
 *  a selection. */
export function writeAtEnd(handle: EditorHandle, text: string, bold?: string): void {
  const { view } = handle;
  const end = endOfText(handle);
  view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, end)));
  const insert = () => view.state.tr.insertText(text, end, end);
  const taken = view.someProp("handleTextInput", (handleTextInput) =>
    handleTextInput(view, end, end, text, insert)
  );
  if (taken !== true) view.dispatch(insert());
  if (bold === undefined) return;
  const from = end + text.indexOf(bold);
  const strong = view.state.schema.marks.strong;
  if (strong === undefined || from < end) throw new Error(`"${bold}" is not in "${text}"`);
  view.dispatch(view.state.tr.addMark(from, from + bold.length, strong.create()));
}

/** Selects the first run of the editor's text that reads `quote`, as a drag over it does. */
export function selectText(handle: EditorHandle, quote: string): void {
  const { view } = handle;
  let from: number | undefined;
  view.state.doc.descendants((node, pos) => {
    if (from !== undefined || !node.isText) return from === undefined;
    const at = node.text?.indexOf(quote) ?? -1;
    if (at >= 0) from = pos + at;
    return false;
  });
  if (from === undefined) throw new Error(`"${quote}" is not in the editor`);
  view.dispatch(
    view.state.tr.setSelection(TextSelection.create(view.state.doc, from, from + quote.length))
  );
}
