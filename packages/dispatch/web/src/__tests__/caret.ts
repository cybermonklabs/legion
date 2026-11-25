// biome-ignore lint/style/noRestrictedImports: user-event has no form that moves the caret inside an editable root.
import { fireEvent } from "@testing-library/react";

/** One move of the caret, as a browser reports it. The selection is not emptied first: that step
 *  would report a caret outside every link in between. */
export function caretAt(node: Node, offset: number): void {
  document.getSelection()?.collapse(node, offset);
  fireEvent(document, new Event("selectionchange"));
}
