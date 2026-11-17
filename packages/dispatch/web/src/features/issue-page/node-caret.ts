import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import { refocusWhenSettled } from "../doc/refocus-when-settled";
import { scrollElementToTop } from "../doc/scroll-block";
import { bringOnScreen, FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";

/**
 * Putting the caret in a node's own text, where that text lives, and holding the editor that
 * text is drawn in to the window while the row holds it.
 *
 * The edit key draws the node's document in the row (`document-stage.tsx`) and then calls these:
 * `caretInBlock` places the caret at the start of that node's own block, and `holdEditorAtBlock`
 * opens the editor's box at that block. The caret is a DOM selection inside the editor and
 * nothing else: the editor's own observer reads the selection back as the document's, so this
 * touches neither the room nor the editor.
 *
 * `focusBlock` on the editor handle is neither: it scrolls the block into the middle of the
 * screen - the page as well as the box - and pulses it, which is what a link into a document
 * wants, and it places no caret (`@legion/proof-editor`'s `lib.ts`). A row wants the caret,
 * because editing is what the row opened the document for, and the page around the row where
 * the reader left it.
 */

/** The mark of the box the editor of a row's document scrolls in (`StagedDocument.tsx`). */
export const EDITOR_BOX_ATTRIBUTE = "data-editor-box";

/**
 * Places the caret at the start of `blockId`'s block of the editor in `host`, or at the start of
 * the editor where it draws no such block, and answers whether `host` holds an editor ready for
 * it yet. An editor is ready once it has drawn the document's blocks: it is made editable
 * before it has read the document, and holds no block to put the caret in until then. The focus
 * moves the page by nothing: the box the editor scrolls in is `holdEditorAtBlock`'s to place.
 * Called again, it puts back a caret the editor dropped or moved as it redrew itself
 * (`useNodeEditing.ts`).
 */
export function caretInBlock(host: HTMLElement, blockId: string | undefined): boolean {
  const editable = host.querySelector<HTMLElement>('[contenteditable="true"]');
  if (editable === null || editable.querySelector(`[${BLOCK_ID_ATTRIBUTE}]`) === null) {
    return false;
  }
  editable.focus({ preventScroll: true });
  const start = firstEditableText(blockIn(editable, blockId) ?? editable);
  // One move, rather than a selection emptied and then filled: where a change of the selection
  // is reported the moment it is made (happy-dom), the emptied one reads as a caret out of the
  // block (`caretOutOfBlock`), which places it again, and again.
  if (start !== null) window.getSelection()?.collapse(start, 0);
  return true;
}

/**
 * Whether the editor in `host` holds the focus with the caret out of `blockId`'s block, where
 * `caretInBlock` put it. The editor reads a caret put in it only once the browser reports the
 * new selection, a task later, and until then writes its own selection back over it - as it
 * takes the focus (`prosemirror-view`'s focus handler, 20 ms after it) and as it redraws - which
 * is where its selection was before the caret went into the block. Measured in Chromium, with
 * the page busy drawing the document, the caret put in a statement was in the spec's summary
 * 60 ms later, and the words the reader typed went there. False where the editor draws no such
 * block: `caretInBlock` put the caret at the start of the editor then.
 */
export function caretOutOfBlock(host: HTMLElement, blockId: string | undefined): boolean {
  const editable = host.querySelector<HTMLElement>('[contenteditable="true"]');
  const block = editable === null ? null : blockIn(editable, blockId);
  if (editable === null || block === null || !editable.contains(document.activeElement)) {
    return false;
  }
  const anchor = window.getSelection()?.anchorNode;
  return anchor === null || anchor === undefined || !block.contains(anchor);
}

/**
 * Opens the editor in `host` at `blockId`'s block, in a box no taller than the window. The
 * editor holds the whole document, and drawn whole in the row it made the row as tall as the
 * document - 35,635 px for the spec of a real issue of some 160 rows - and took everything
 * under the row down by as much. So the box it scrolls in (`EDITOR_BOX_ATTRIBUTE`) reaches from
 * where it stands down to the box at the foot of the screen and no further: the page stays where
 * the reader had it, and every line of the box is on screen, so the editor, which keeps its
 * caret in view as the reader writes, scrolls the box and never the page. The box - never the
 * page - is then scrolled so the block stands at its top, where the row's words stood, and once
 * more after the drawing above the block has settled (`refocusWhenSettled`): a picture or a
 * diagram above it draws after the editor opened, and pushes it down.
 *
 * A row so low on the screen that less than half the room between the band at the top and the
 * box at the foot stands under it would leave an editor a line or two tall, so its box takes
 * that half, and the page is scrolled just as far as brings the box on screen (`bringOnScreen`,
 * which leaves a box already on screen where it is).
 *
 * Answers what drops that second scroll.
 */
export function holdEditorAtBlock(host: HTMLElement, blockId: string | undefined): () => void {
  const box = host.querySelector<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  if (box === null) return () => {};
  const published = getComputedStyle(document.documentElement);
  const height = (property: string) => {
    const value = Number.parseFloat(published.getPropertyValue(property));
    return Number.isFinite(value) ? value : 0;
  };
  const floor = window.innerHeight - height(FOOT_HEIGHT_PROPERTY);
  const room = floor - height(IDENTITY_HEIGHT_PROPERTY);
  const under = floor - box.getBoundingClientRect().top;
  // Whole pixels down, so a box that ends on the line the foot's box starts at is not read as
  // reaching under it by a fraction and scrolled for it.
  box.style.maxHeight = `${Math.floor(Math.min(room, Math.max(under, room / 2)))}px`;
  bringOnScreen(box);
  const place = () => {
    const block = blockIn(box, blockId);
    if (block !== null) scrollElementToTop(block, box);
  };
  place();
  return refocusWhenSettled(place, box.querySelector('[contenteditable="true"]') ?? undefined);
}

/** `blockId`'s block of the editor in `root`, or null where it draws none. */
function blockIn(root: HTMLElement, blockId: string | undefined): HTMLElement | null {
  return blockId === undefined
    ? null
    : root.querySelector<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`);
}

/** The first text of `root` a caret can enter: an atom's text and a collaborator's cursor label
 *  sit in `contenteditable="false"`, where a caret cannot go. */
function firstEditableText(root: HTMLElement): Node | null {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node.parentElement?.isContentEditable === true) return node;
  }
  return null;
}
