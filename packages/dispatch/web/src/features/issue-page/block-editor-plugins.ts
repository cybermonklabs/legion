import type { Schema } from "@milkdown/kit/prose/model";
import { Plugin } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { ySyncPluginKey, yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import type { XmlElement, XmlFragment } from "yjs";

import { quoteAnchor } from "../doc/quote-anchor";
import { keepsBoundBlock } from "./node-caret";

/**
 * The plugin the issue page's kept block editor holds beside the editor library's own (R7,
 * `block-editor.tsx`), put on once as the editor is built (`block-room.ts`'s `IssueBlockRoom`),
 * before its first bind: `rebindElement` keeps every plugin the view holds, so it holds for every
 * block the editor binds after. This module is loaded with the editor's code, after the load, and
 * imports the editor library's ProseMirror, which the page's own chunk holds none of.
 *
 * Every edit of the editor's own keeps the bound block's structure (`keepsBoundBlock`): what the
 * whole document's editor is held to inside the node's block (`holdEditsToBlock`). Each
 * transaction the collaboration brings applies, marked with y-prosemirror's `ySyncPluginKey`, and
 * so does one that changes no text.
 */
export function installKeptEditorPlugins(view: EditorView): void {
  view.updateState(view.state.reconfigure({ plugins: [...view.state.plugins, boundBlockHold()] }));
}

function boundBlockHold(): Plugin {
  return new Plugin({
    filterTransaction: (transaction, state) =>
      transaction.getMeta(ySyncPluginKey) !== undefined ||
      !transaction.docChanged ||
      keepsBoundBlock(transaction, state),
  });
}

/** A read-only comment sends a quote and occurrence counted in the whole document, not
 *  the bound block. Read the live room locally; a quote may occur in earlier blocks too. */
export function boundQuoteAnchor(schema: Schema, element: XmlElement, from: number, to: number) {
  const id = element.getAttribute("blockId");
  if (typeof id !== "string" || id === "") return undefined;
  let fragment: XmlFragment = element;
  while (fragment.parent !== null) fragment = fragment.parent as XmlFragment;
  const doc = schema.nodeFromJSON(yXmlFragmentToProsemirrorJSON(fragment));
  let start: number | undefined;
  doc.descendants((child, pos) => {
    if (start !== undefined) return false;
    if (child.attrs.blockId !== id) return true;
    start = pos + 1;
    return false;
  });
  return start === undefined ? undefined : quoteAnchor(doc, start + from, start + to);
}
