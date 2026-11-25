import { TextSelection } from "@milkdown/kit/prose/state";
import { within } from "@testing-library/react";

import { loadDocumentTransport } from "../features/doc/connection";
import { type ConstructEditor, constructEditor, type EditorHandle } from "../features/doc/editor";
import type { DocumentRuntimeValue } from "../features/doc/runtime";
import { changingEachHandle } from "./document-runtime";

/** The documents' own editor, each handle it bound, and the lookup of one by its name. */
export interface RecordedEditors {
  /** Every handle the editor bound, in the order it bound them. */
  readonly handles: EditorHandle[];
  /** Whether an editor it built is still binding: built or being built, and neither bound,
   *  refused nor torn down yet. The bind ends tasks after a page drew the editor
   *  (`@legion/proof-editor`'s `bind`), so a row that ends then would have the page take the
   *  handle outside `act`: a fixture's teardown turns the event loop until this is false. */
  binding(): boolean;
  /** The editor named `name` once it holds its text, among the elements of `scope` (the whole
   *  document where none is given), with its handle. */
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
  let binding = 0;
  const counting: ConstructEditor = async (root, options) => {
    binding += 1;
    let open = true;
    const close = () => {
      if (open) binding -= 1;
      open = false;
    };
    const construction = await constructEditor(root, options).catch((error: unknown) => {
      close();
      throw error;
    });
    return {
      ...construction,
      bind: () => construction.bind().finally(close),
      destroy: () => {
        close();
        return construction.destroy();
      },
    };
  };
  const recording = changingEachHandle(counting, (handle) => handles.push(handle));
  return {
    binding: () => binding > 0,
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
