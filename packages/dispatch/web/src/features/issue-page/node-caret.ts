import type { Fragment, Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type * as ProseMirrorState from "@milkdown/kit/prose/state";
import type { EditorState, Plugin, Transaction } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import type * as YProseMirror from "y-prosemirror";

import type { DisplayTree } from "../../api/types";
import { inputMethodKey } from "../../lib/input-method-key";
import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import { refocusWhenSettled } from "../doc/refocus-when-settled";
import { scrollElementToTop } from "../doc/scroll-block";
import { importWhenOnline } from "../shell/DeploymentResilience";
import { canHoldFocus } from "./focus-keeper";
import { bringOnScreen, FOOT_HEIGHT_PROPERTY, IDENTITY_HEIGHT_PROPERTY } from "./identity-block";

/**
 * Putting the caret in a node's own text, where that text lives, and holding the editor that
 * text is drawn in to the window while the row holds it.
 *
 * The edit key draws the node's document in the row (`document-stage.tsx`) and then calls these:
 * `caretInBlock` places the caret at the start of that node's own block, and `holdEditorAtBlock`
 * opens the editor's box at that block. The caret is a DOM selection inside the editor and
 * nothing else: the editor's own observer reads the selection back as the document's, so this
 * touches neither the room nor the editor. While the row holds the editor, `holdEditsToBlock`
 * keeps every edit the editor makes inside that block, and `confineKey` and `selectionIntoBlock`
 * keep the reader's caret there.
 *
 * The editor is the document's whole editor, its box opened at the node's block, and the keys
 * the editor answers for a whole document reached past that block: measured on a preview,
 * Ctrl+Home moved the caret before the spec's `## Summary`, and a `Y` typed then made it
 * `## YSummary`, scrolled out of the box above the row; Up after Left put the caret before a
 * heading above the statement. An edit of text the reader cannot see is the loss the caret
 * placed at the top of the document was, so the row's editor keeps its selection, and every
 * edit, inside the node's block. Free editing of the whole document stays on the document page.
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
 * Places the caret at the start of `blockId`'s block of the editor in `host`, and answers whether
 * it did. An editor that draws no such block gets neither the focus nor a caret: it is made
 * editable before it has read the document and may draw the document's blocks over more than
 * one frame after that, and a caret put anywhere else in it - its first text, where it has drawn
 * other blocks before the node's - sends the keys the reader presses next to the top of the
 * document, which is saved to the shared document. The focus stays where it was then: on the
 * row, as Enter left it, where no key edits the document. A block with no text a caret can
 * enter - an empty paragraph - takes the caret in the block itself, which the editor reads as a
 * place in that block: an editable given the focus with no caret put in it takes the next key
 * wherever the browser puts its own caret. The focus moves the page by nothing: the box the
 * editor scrolls in is `holdEditorAtBlock`'s to place. The editor takes the caret as its own in
 * the task that gives it the focus (`selectInEditor`), so no key reaches it before its selection
 * is the block's. Called again, it puts back a caret the editor dropped or moved as it redrew
 * itself (`useNodeEditing.ts`), in the node's block and nowhere else.
 */
export function caretInBlock(host: HTMLElement, blockId: string | undefined): boolean {
  const drawn = drawnBlock(host, blockId);
  if (drawn === null) return false;
  drawn.editable.focus({ preventScroll: true });
  placingCaret += 1;
  selectInEditor(firstPlace(drawn.block));
  placingCaret -= 1;
  return true;
}

/** Whether the editor in `host` draws `blockId`'s block, which is when a caret can go in it
 *  (`caretInBlock`). */
export function drawsBlock(host: HTMLElement, blockId: string | undefined): boolean {
  return drawnBlock(host, blockId) !== null;
}

/**
 * Puts the selection from `anchor` to `focus` in the editor holding the focus, and has the
 * editor take it as its own at once. The editor reads a selection put in it only once the
 * browser reports the change - its observer listens for the document's `selectionchange`
 * (`prosemirror-view`'s `DOMObserver`) - which the browser does a task later, and later still on
 * a busy page. Until then a key the editor handles itself (Delete, Backspace, Enter, through its
 * keymap) acts on the editor's own selection, and the editor writes that selection back over the
 * DOM's as it takes the focus (its focus handler, 20 ms after it) and as it redraws: measured in
 * Chromium on a busy page, the editor held the focus with its selection at the top of the
 * document for 0.27 to 0.84 s after Enter put the caret in a statement, and ten Deletes pressed
 * then removed the spec's first heading. So the report is made here, in the same task: the
 * editor reads the selection on it, and the browser's own report, when it comes, holds nothing
 * new. One move, rather than a selection emptied and then filled: where a change of the
 * selection is reported the moment it is made (happy-dom), the emptied one reads as a caret out
 * of the block (`caretOutOfBlock`), which places it again, and again.
 */
export function selectInEditor(anchor: Point, focus: Point = anchor): void {
  window.getSelection()?.setBaseAndExtent(anchor[0], anchor[1], focus[0], focus[1]);
  document.dispatchEvent(new Event("selectionchange"));
}

/**
 * Whether the editor in `host` holds the focus with the caret out of `blockId`'s block, where
 * `caretInBlock` put it: the caret moves out with the editable as the editor makes its views
 * again (`useNodeEditing.ts`). False where the editor draws no such block, where `caretInBlock`
 * puts no caret either.
 */
export function caretOutOfBlock(host: HTMLElement, blockId: string | undefined): boolean {
  const drawn = drawnBlock(host, blockId);
  if (drawn === null || !drawn.editable.contains(document.activeElement)) return false;
  const anchor = window.getSelection()?.anchorNode;
  return anchor === null || anchor === undefined || !drawn.block.contains(anchor);
}

/** The keys `confineKey` leaves to a row's editor wherever its selection stands, and has it read
 *  no caret for: Escape, which ends the editing (`useNodeEditing.ts`), and the modifiers, held for
 *  the next key. None of them edits. Tab is no such key: Milkdown answers it in a list item. */
const PASSING_KEYS: Readonly<Record<string, true>> = {
  Alt: true,
  AltGraph: true,
  CapsLock: true,
  Control: true,
  Escape: true,
  Meta: true,
  Shift: true,
};

/** The keys held with Ctrl or Meta that the editor answers with an edit, or the browser does, by
 *  `event.key` in lower case: select-all, the marks (bold, italic, code, a browser's underline),
 *  cut and paste, undo and redo, a Mac's Ctrl+D and Ctrl+H, a list item's sink and lift, the
 *  deletes of a word and Enter's way out of a code block (`prosemirror-commands`' base keymaps,
 *  `@milkdown/preset-commonmark`, `@milkdown/plugin-history`, `@milkdown/plugin-collab`). Every
 *  block shortcut holds Alt as well (`Mod-Alt-0` to `Mod-Alt-8`, `Mod-Alt-c`). */
const EDITING_CHORDS: Readonly<Record<string, true>> = {
  "[": true,
  "]": true,
  a: true,
  b: true,
  backspace: true,
  d: true,
  delete: true,
  e: true,
  enter: true,
  h: true,
  i: true,
  u: true,
  v: true,
  x: true,
  y: true,
  z: true,
};

/** The platforms ProseMirror reads as a Mac (`prosemirror-keymap`). */
export const MAC_PLATFORM = /Mac|iP(hone|[oa]d)/;

/** A place in the page, as each end of a selection is one. */
export type Point = readonly [node: Node, offset: number];

/** The four moves of a node that Graph's editor keys make (INFRA-5705's R4), as the node menu's
 *  entries name them (`graph-edit/placement.ts`): Indent, Outdent, Move up and Move down. */
export type MoveKey = "indent" | "move_down" | "move_up" | "outdent";

/**
 * The move `event` asks for, or undefined: Tab indents, Shift+Tab outdents, Alt+Shift+Up moves
 * up and Alt+Shift+Down moves down (R4), each held with neither Ctrl nor Meta. The key catalogue
 * of cybermonklabs/legion#4304 is not on this branch's base, so each binds as part I's keys do,
 * by the key itself (R10, decision 470 (3)). A key of an input method's composition, or the one
 * that ends it, asks for no move (`inputMethodKey`, and the `Process` key an input method sends),
 * so the input method keeps it.
 */
export function moveKeyOf(event: KeyboardEvent): MoveKey | undefined {
  if (event.ctrlKey || event.metaKey || inputMethodKey(event) || event.key === "Process") {
    return undefined;
  }
  if (event.key === "Tab" && !event.altKey) return event.shiftKey ? "outdent" : "indent";
  if (!event.altKey || !event.shiftKey) return undefined;
  if (event.key === "ArrowUp") return "move_up";
  if (event.key === "ArrowDown") return "move_down";
  return undefined;
}

/**
 * Keeps a key pressed in the editor in `host` from taking its caret out of `blockId`'s block,
 * before the editor acts on it: called on the key in the capture phase, so before ProseMirror's
 * own listener on the editable, which acts on a key on its keydown - its keymap moves its own
 * selection, and a key it leaves to the browser moves the DOM's - and ignores a key whose default
 * is prevented (`prosemirror-view`'s `eventBelongsToView`). Every edit is held to the block by
 * the editor itself (`holdEditsToBlock`), which refuses a move of its selection out of the block
 * too; but by then the browser has moved the page's caret, and scrolled the box to it, and the
 * editor writes its own selection back over that caret a task later. So a key is refused
 * (`preventDefault`) where it would take the caret out of the block:
 *
 * - every key that could edit (`couldEdit`) while the selection is not inside the block, or the
 *   editor draws no such block: before the caret is placed, or a Tab into the editable before the
 *   block is drawn, no key edits the document, while the browser's own keys - find, print, the
 *   address bar, a function key - still reach it. Tab alone is kept from the editor rather than
 *   refused, so the browser still moves the focus on with it;
 * - Up on the block's first line and Down on its last, Left at its start and Right at its end,
 *   PageUp and PageDown, which leave the caret where it is.
 *
 * Ctrl+Home and Ctrl+End (Meta too, and Meta+Up and Meta+Down, a Mac's keys for them) put the
 * caret at the block's start and end, and Ctrl+A (Meta+A on a Mac) selects the block's text
 * alone, which the editor takes as its own at once (`selectInEditor`); with Shift held a move
 * extends the selection to the block's end it stops at. A Mac's Ctrl+A is the editor's own move
 * to the start of the line's paragraph (`prosemirror-commands`' `macBaseKeymap`), which stays in
 * the block, and is left to it.
 *
 * In Graph, with the caret inside the block, the four keys of R4 (`moveKeyOf`) go first to
 * `onMove`, which the page gives a row in Graph alone (`graph-edit/move-keys.ts`), after the editor
 * has read the caret where it has not yet: one it takes - a move of the node, or an entry grey at
 * its edge, whose reason the row shows - goes no further, so neither the editor (a list item's
 * own sink and lift) nor the browser (the focus moving on) acts on it, and the caret stays where it
 * is. One it does not take - a block R2 does not admit - goes on as every key does. Undefined
 * everywhere else, Conversation and every filter among them, where the keys are today's.
 *
 * `view` is that editor's view, as the page handed it to the row (`useNodeEditing.ts`), whose own
 * selection a key the editor answers acts on.
 */
export function confineKey(
  host: HTMLElement,
  blockId: string | undefined,
  event: KeyboardEvent,
  onMove: ((move: MoveKey) => boolean) | undefined,
  view: EditorView | undefined
): void {
  if (event.defaultPrevented || PASSING_KEYS[event.key] === true) return;
  const editable = editableHolding(host, event.target);
  if (editable === null) return;
  const block = blockIn(editable, blockId);
  const selected = block === null ? null : selectionIn(block);
  if (block === null || selected === null) {
    if (event.key === "Tab") {
      // Tab is how the reader leaves an editor with no caret in the block, so its default - the
      // focus moving on - stays. But Milkdown answers Tab and Shift+Tab in a list item, on its own
      // selection wherever that stands (SinkListItem, LiftListItem): an edit no hold may be on to
      // refuse yet (`useNodeEditing.ts`), and where one is, the hold refuses the edit but the
      // keymap still prevents the key's default, which holds the focus in the editor. So the key
      // goes no further than the element this is called at in the capture phase, the row's box
      // (`useNodeEditing.ts`): ProseMirror's listener on the editable never hears it. The
      // listeners that hear a key before the box - the window's and the document's in the
      // capture phase, and the box's own for Escape - hear it as before. The page's keymap
      // listens on the window in the bubble phase, so it no longer hears this Tab, and it binds
      // no Tab in an editable anyway (`shell/keymap.ts`'s `isEditableTarget`).
      event.stopPropagation();
    } else if (couldEdit(event)) {
      event.preventDefault();
    }
    return;
  }
  const move = onMove === undefined ? undefined : moveKeyOf(event);
  if (move !== undefined && onMove !== undefined) {
    // The move keeps the caret the editor holds, which it reads now where the page's selection
    // is not yet its own, as before any key it answers (below).
    if (!holdsSelection(view, ...selected)) document.dispatchEvent(new Event("selectionchange"));
    if (onMove(move)) {
      event.preventDefault();
      event.stopPropagation();
      return;
    }
  }
  const taken = keyAtBlock(event, block, ...selected);
  if (taken === undefined) {
    // The editor answers a key on its own selection, which it reads from the page only once the
    // browser reports a move of the caret, a task later: in a Job like CI's, Backspace right after
    // ArrowRight at a heading's start acted at the start, and the letter before the caret stayed
    // (2 runs of 10). Tab and Shift+Tab are such keys: Milkdown sinks and lifts the list item its
    // own selection is in (SinkListItem, LiftListItem), so a Tab right after ArrowDown to the next
    // item, before the browser reported the move, would sink the item the caret had left. So
    // before each key that goes on to the editor with the selection in the block, Escape and the
    // modifiers aside (`PASSING_KEYS`), the editor reads the caret now where the page's selection
    // is not the editor's own (`holdsSelection`). Only there: the read is the document's
    // `selectionchange`, which every listener on the page hears, and the editor's action bar reads
    // each one on a touch selection as the reader's new selection, which it hides and shows again
    // 400 ms later (`@legion/proof-editor`'s `dispatch-action-bar.ts`): read before every key, the
    // bar blinked at each Ctrl+C on a touch device with a keyboard. Not during a composition: the
    // input method holds the caret then, and ProseMirror reads the page itself as a composition
    // starts and as it ends, and leaves a key pressed in one alone (`inOrNearComposition`).
    if (!event.isComposing && event.key !== "Process" && !holdsSelection(view, ...selected)) {
      document.dispatchEvent(new Event("selectionchange"));
    }
    return;
  }
  event.preventDefault();
  if (taken !== null) selectInEditor(...taken);
}

/** Whether the editor `view` holds the selection from `anchor` to `focus` the page shows as its
 *  own: its anchor and its head stand where those do. A place it cannot read - one outside its
 *  document, or any in an editor torn down, where `posAtDOM` throws - reads as another, as does
 *  every place where the row holds no view. */
function holdsSelection(view: EditorView | undefined, anchor: Point, focus: Point): boolean {
  if (view === undefined) return false;
  try {
    const { selection } = view.state;
    return (
      view.posAtDOM(anchor[0], anchor[1]) === selection.anchor &&
      view.posAtDOM(focus[0], focus[1]) === selection.head
    );
  } catch {
    return false;
  }
}

/** The elements the browser stops at for Tab unless a negative `tabindex` says otherwise, and
 *  every element a `tabindex` puts among them (`isTabStop`). */
const TAB_STOPS = [
  "a[href]",
  "area[href]",
  "button",
  "iframe",
  'input:not([type="hidden"])',
  "select",
  "summary",
  "textarea",
  '[contenteditable="true"]',
  '[contenteditable=""]',
  "[tabindex]",
].join(", ");

/** The class of the action bar the editor draws for its selection (`@legion/proof-editor`'s
 *  `dispatch-action-bar.ts`): one element, hidden with an inline `display: none` while the
 *  selection holds no text, and put in the element that holds the editable as the editor makes
 *  it - on this page, inside the editor's box (`EditorBox`). ProseMirror puts the editable in the
 *  element the page gives the editor (`ProofDocument.tsx`'s root) before it makes any plugin's
 *  view, and Milkdown moves it into a box of its own inside that element, and back, as it makes
 *  its plugins' views and drops them again, so the editable stands in that element whenever the
 *  bar is made. */
const ACTION_BAR = ".dispatch-action-bar";

/** The mark of the button a document draws for each block with comments or asks on it, which
 *  names that block's id (`doc/ProofDocument.tsx`'s "Block references"): the buttons stand in an
 *  aside of their own beside the editor, inside the editor's box, outside every block. */
const BLOCK_REFERENCES_ATTRIBUTE = "data-block-references";

/**
 * Where Tab - Shift+Tab where `back` - may take the focus from `from`, inside the box the editor
 * of the row `row` scrolls in (`EDITOR_BOX_ATTRIBUTE`), while that editor draws `blockId`'s
 * block: the browser's own tab stops in the key's direction, nearest first, in the order it steps
 * through them - the page's order, which nothing in Dispatch changes with a positive `tabindex` -
 * passing over every one the editor draws outside that block: another block's link or button.
 * Its editable and what the node's block draws stay among them, and so do two controls drawn
 * outside every block that act on the node's own:
 *
 * - the action bar while it shows (`ACTION_BAR`): its Comment, Suggest and Ask act on the
 *   selection, which the row holds inside the node's block (`selectionIntoBlock`). A bar hidden
 *   is read by its own inline `display: none`, not through the layout, so a page with none
 *   answers as the browser does, which gives no hidden control the focus;
 * - the references button of the node's block, or of a block inside it
 *   (`BLOCK_REFERENCES_ATTRIBUTE`), which shows the comments and asks on that block and puts the
 *   reader in it. A comment's quote is anchored to the innermost block that holds it: measured
 *   in Chromium, a comment on a statement's words named the paragraph inside the statement's
 *   typed block, by the id the editor gave that paragraph, and its button named the same. Another
 *   block's button takes the reader to that block, and the box with it, away from the node's.
 *
 * Tab reaches those as it did before the rest of the editor's were passed over. Shift+Tab past
 * the box's start goes to the row itself, the outline's one tab stop, where the editing began.
 * Null where `from` is not inside that box, or the editor draws no such block.
 */
export function tabStopsFrom(
  row: HTMLElement,
  blockId: string | undefined,
  from: Element,
  back: boolean
): HTMLElement[] | null {
  const box = row.querySelector<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  const drawn = box === null ? null : drawnBlock(box, blockId);
  if (box === null || drawn === null || !box.contains(from)) return null;
  const { editable, block } = drawn;
  const bar = box.querySelector<HTMLElement>(ACTION_BAR);
  const shownBar = bar !== null && bar.style.display !== "none" ? bar : null;
  // An ancestor of `from` stands before it in the page's order, and a descendant after it.
  const side = back ? Node.DOCUMENT_POSITION_PRECEDING : Node.DOCUMENT_POSITION_FOLLOWING;
  const stops = Array.from(document.querySelectorAll<HTMLElement>(TAB_STOPS)).filter((stop) => {
    if ((from.compareDocumentPosition(stop) & side) === 0 || !isTabStop(stop)) return false;
    if (
      !box.contains(stop) ||
      stop === editable ||
      block.contains(stop) ||
      shownBar?.contains(stop) === true
    ) {
      return true;
    }
    const named = stop.getAttribute(BLOCK_REFERENCES_ATTRIBUTE) ?? undefined;
    return named !== undefined && (named === blockId || blockIn(block, named) !== null);
  });
  return back ? [...stops.filter((stop) => box.contains(stop)).reverse(), row] : stops;
}

/** Whether the browser stops at `element`, one of `TAB_STOPS`, for Tab: no negative `tabindex`,
 *  nothing editable inside an editable - a link in the editor's text takes no focus of its own,
 *  while the editable itself does, and so does a control the editor draws
 *  `contenteditable="false"`, even one right in its text, as a mermaid block's "Mermaid source"
 *  toggle stands in an editable `details` (`@legion/proof-editor`'s `mermaid-code-block-view.ts`)
 *  and a callout may hold that block - and able to take the focus (`canHoldFocus`). A stop the
 *  page draws nothing of - `display: none`, a closed `details` - is one the browser refuses the
 *  focus to, which `useNodeEditing.ts` steps past. */
function isTabStop(element: HTMLElement): boolean {
  return (
    (!element.hasAttribute("tabindex") || element.tabIndex >= 0) &&
    !(element.isContentEditable && element.parentElement?.isContentEditable === true) &&
    canHoldFocus(element)
  );
}

/** Whether `event` is a key the editor or the browser could edit the document with, or move its
 *  caret with: any key but a function key, held with neither Ctrl nor Meta, and those held with
 *  either that the editor binds (`EDITING_CHORDS`, and every one with Alt held as well, which a
 *  Mac's Alt and Windows' AltGr type letters with too). */
function couldEdit(event: KeyboardEvent): boolean {
  if (/^F\d+$/.test(event.key)) return false;
  if (!event.ctrlKey && !event.metaKey) return true;
  return event.altKey || EDITING_CHORDS[event.key.toLowerCase()] === true;
}

/** What the hold on a row's editor is built of: ProseMirror's plugins, and the key y-prosemirror
 *  marks each transaction the collaboration brings with. Both are the editor library's own,
 *  which sits behind its own `import()` (`doc/editor.ts`), so they are imported here only once a
 *  row is edited, and kept from then on. */
export type HoldParts = readonly [state: typeof ProseMirrorState, sync: typeof YProseMirror];
let loadingHoldParts: Promise<HoldParts> | undefined;

/** Loads what `holdEditsToBlock` is built of, once. A row asks for it as it starts editing, so
 *  it has loaded by the time the page hands the row its editor, and the row gives the hold what
 *  this answers. A load that failed is the caller's to answer for, and the next call loads
 *  again. */
export function loadEditsHold(): Promise<HoldParts> {
  loadingHoldParts ??= importWhenOnline(() =>
    Promise.all([import("@milkdown/kit/prose/state"), import("y-prosemirror")])
  ).catch((error: unknown) => {
    loadingHoldParts = undefined;
    throw error;
  });
  return loadingHoldParts;
}

/** The attribute a block of the editor's document holds its id in (`@legion/proof-editor`'s
 *  `BLOCK_ID_ATTR`), which it draws as `BLOCK_ID_ATTRIBUTE`. */
const BLOCK_ID_PROPERTY = "blockId";

/**
 * Holds every edit the editor `view` makes of its own to the block `heldBlock` names, while the
 * row holds the editor: a ProseMirror plugin whose `filterTransaction` refuses a transaction of
 * the editor's own that changes anything outside that block - a join with the block beside it, a
 * split into two, a block made around it or beside it - or changes the block's own type or
 * attributes - a heading lowered a level, a paragraph made a heading, a quote, a list or a code
 * block - or puts a line break in a heading of it, or leaves the selection outside it. The
 * editor's keymap, its input rules, a paste, a drop and every change the browser makes to the page
 * that the editor reads back reach the document as such a transaction, so none of them needs a
 * guard of its own: a refused one changes nothing, and the editor draws the page again from its
 * document.
 *
 * A block holds blocks of its own too - a typed block its paragraphs, a decision its heading and
 * its words, a list its items - and the same rule holds inside it, in two halves. Its paragraphs
 * are its words, which an edit may split and join: Enter in a typed block makes a paragraph
 * inside it, a paste of two paragraphs puts both in, Backspace at a paragraph's start joins it to
 * the paragraph before it, and a list's item sinks into the item before it. Every other block
 * inside it is its structure, which an edit of words never changes: each block inside it the
 * transaction leaves standing - its content kept, wherever the transaction puts it - is still a
 * block of the type and the attributes it was, its id aside, which the editor mints for a block
 * a split or a sink makes or moves (`ownAttributes`). So a decision's heading lowered a level or
 * made a paragraph, a paragraph made a heading, and a paragraph joined to the heading before it
 * are each refused, as they are for the block itself; a block deleted whole with the words a
 * selection held is not standing, and keeps nothing.
 *
 * A heading stores as one line of markdown, and measured in Chromium, two lines pasted at the
 * start of a decision's `## ` heading stored it as a heading of two lines, `Two\`, `lines...` and
 * `---` under them. A heading's `id` is no attribute the reader edits: Milkdown derives it from the
 * heading's words (`syncHeadingIdPlugin`), and writes it in a transaction of its own after every
 * change of them, to each heading whose name that change moves.
 *
 * Every transaction the collaboration brings applies, wherever it lands, each marked with
 * y-prosemirror's `ySyncPluginKey`: what y-prosemirror draws from the room - a collaborator's
 * edit, the span of a mark set there - and the editor's projection of the room's `marks` map
 * (`doc/editor.ts`'s `bindRemoteMarks`, through `proof-sdk-upstream`'s `applyRemoteMarks`, which
 * marks its transaction as y-prosemirror marks its own), which carries the marks' records and
 * takes the spans of the suggestions the room has accepted or rejected out of the text. The
 * block's place is mapped through each transaction, and found again by its id where one moved it
 * elsewhere. Mod-z is answered first by Milkdown's own history, as a local transaction this
 * filter checks like any other: two rows of one document share one editor, so an undo that would
 * take back an earlier row's edit, outside this row's block, is refused. A transaction that
 * changes neither the document nor the selection - a decoration drawn, a mark stored for the next
 * letter - leaves the document as it was, and applies.
 *
 * The plugin goes on once `parts` have loaded - what it is built of, which the row asks for as it
 * starts editing (`loadEditsHold`) - and `onHeld` is called as it goes on: the row puts no caret
 * in the editor before that (`useNodeEditing.ts`). Where they had loaded before the page handed
 * the editor over, that is in the task it was handed over in, before a key or a press can reach
 * the editor; the editor may have drawn the document by then. A load that failed puts no plugin
 * on, and is the row's to answer for, which asked for it. A change of the editor's plugins makes
 * ProseMirror make each plugin's view again (`useNodeEditing.ts`). The collaborators' cursors go
 * on later in the same way, keeping every plugin the editor holds (`@legion/proof-editor`'s
 * `installCollabCursorsWhenReady`). While `heldBlock` names no block, or the document holds no
 * block of its id, no edit of the editor's own goes in. Answers what lets the hold go, or keeps
 * it from going on.
 *
 * A hold let go leaves its plugin on the editor, refusing nothing and looking for no block, as no
 * plugin did. Taking it off would change the editor's plugins again: ProseMirror then makes every
 * plugin's view again (`prosemirror-view`'s `updatePluginViews`), and y-prosemirror's view, made
 * with no mapping, draws the whole document again from the room in one transaction
 * (`sync-plugin.js`'s `_forceRerender`), as read in both libraries' sources. The tree lets go of
 * its hold as it goes (`useNodeEditing.ts`), on an editor the page is about to destroy, where
 * taking the plugin off drew the whole document - a spec of some 160 rows - again for nothing.
 *
 * A change of the document an edit of the reader's makes is reported once the editor has taken
 * it (`onOwnEdit`), with the held block as the edit left it, as `Node.toJSON` writes it. An edit
 * of the reader's is their typing, a paste, an undo, what the editor appends to one of them
 * (ProseMirror's `appendedTransaction`), such as a block id stamped on a block the edit made, and
 * a transaction kept out of the undo history (`addToHistory: false`) that starts from a document
 * such an edit made. Milkdown writes a heading's `id` that way, in a transaction of its own after
 * a change of the heading's words that moves its `id` (`syncHeadingIdPlugin`), dispatched from its
 * plugin's view before this plugin's view hears of the change, so the document the editor has
 * taken by then is the one with the `id`: such an edit of a heading, a decision's `## ` heading
 * among them, is reported through it. An edit that leaves the `id` as it was - of the words' case
 * alone, or of the spaces at their ends - writes none, and is reported itself. A transaction
 * the collaboration brings is not the reader's, nor one appended to it, nor the `id` Milkdown
 * writes after it, which starts from the collaboration's document. The hold notes each document
 * such an edit makes, and its plugin's view reports the document the editor takes where it is
 * one of them: a plugin's state may be worked out for a state the editor never takes, which
 * leaves a note nobody reads. The report goes through this plugin, as a plugin of its own would
 * change the editor's plugins (above). A hold let go reports nothing.
 */
export function holdEditsToBlock(
  view: EditorView,
  heldBlock: () => string | undefined,
  parts: Promise<HoldParts>,
  onHeld: () => void,
  onOwnEdit?: (block: DisplayTree) => void
): () => void {
  let letGo = false;
  void parts.then(
    ([state, sync]) => {
      if (letGo || view.isDestroyed) return;
      // Each document an edit of the reader's made (above).
      const edited = new WeakSet<ProseMirrorNode>();
      // The last document reported: Milkdown's dispatch of a heading's `id` from its plugin's
      // view runs the plugins' views again inside the update that brought the edit, so both
      // updates find the editor holding the same document.
      let reported: ProseMirrorNode | undefined;
      const plugin: Plugin<number | undefined> = new state.Plugin<number | undefined>({
        filterTransaction: (transaction, editorState) =>
          letGo ||
          transaction.getMeta(sync.ySyncPluginKey) !== undefined ||
          (!transaction.docChanged && !transaction.selectionSet) ||
          keepsToBlock(transaction, editorState, heldBlock(), plugin.getState(editorState)),
        // The block's start, as last found: the guess a transaction checks before it searches.
        state: {
          apply: (transaction, start, _, { doc }) => {
            if (letGo) return undefined;
            noteCaret(
              view,
              transaction,
              heldBlock(),
              start,
              transaction.getMeta(sync.ySyncPluginKey) !== undefined
            );
            // A transaction appended to another is read as that one.
            const root: Transaction = transaction.getMeta("appendedTransaction") ?? transaction;
            if (
              root.docChanged &&
              root.getMeta(sync.ySyncPluginKey) === undefined &&
              (root.getMeta("addToHistory") !== false || edited.has(root.before))
            ) {
              edited.add(doc);
            }
            if (!transaction.docChanged) return start;
            return blockStart(
              doc,
              heldBlock(),
              start === undefined ? undefined : transaction.mapping.map(start, 1)
            );
          },
          init: (_, { doc }) => blockStart(doc, heldBlock(), undefined),
        },
        view: () => ({
          update: (editor, before) => {
            const { doc } = editor.state;
            if (letGo || doc === before.doc || doc === reported || !edited.has(doc)) return;
            // The held block, where the plugin's state last found it.
            const start = plugin.getState(editor.state);
            const block = start === undefined ? null : doc.nodeAt(start);
            if (block === null) return;
            reported = doc;
            onOwnEdit?.(block.toJSON());
          },
        }),
      });
      view.updateState(view.state.reconfigure({ plugins: [...view.state.plugins, plugin] }));
      onHeld();
    },
    // The row answers for a load that failed, which asked for it.
    () => undefined
  );
  return () => {
    letGo = true;
  };
}

/** Where `blockId`'s block starts in `doc`: at `guess` where that block still stands there, else
 *  wherever the document holds it, and undefined where it holds no such block. */
function blockStart(
  doc: ProseMirrorNode,
  blockId: string | undefined,
  guess: number | undefined
): number | undefined {
  if (blockId === undefined) return undefined;
  if (guess !== undefined && doc.nodeAt(guess)?.attrs[BLOCK_ID_PROPERTY] === blockId) return guess;
  let found: number | undefined;
  doc.descendants((node, position) => {
    if (found !== undefined) return false;
    if (node.attrs[BLOCK_ID_PROPERTY] !== blockId) return !node.isTextblock;
    found = position;
    return false;
  });
  return found;
}

/** The reader's caret in a held block, its anchor and its head each counted from the block's
 *  start in the editor's document (`keptCaret`). */
export interface KeptCaret {
  readonly anchor: number;
  readonly head: number;
}

/** The caret each editor's hold last saw the reader hold inside its block, by the editor's view,
 *  with that block's id (`noteCaret`). */
const keptCarets = new WeakMap<EditorView, KeptCaret & { readonly block: string }>();

/** How many carets the page itself is putting in a row's editor at once (`caretInBlock`,
 *  `selectionIntoBlock`, `putKeptCaret`): a selection the page makes is no caret of the reader's,
 *  and the hold keeps none of it (`noteCaret`). */
let placingCaret = 0;

/**
 * Keeps the reader's caret in the held block as `transaction` leaves it, from `guess`, the block's
 * start before it: a selection inside the block, from the reader's own edit or move of the caret,
 * or from one the collaboration brings (`remote`), which y-prosemirror keeps where the reader had
 * it. A selection outside the block is kept nowhere: a change of the room that takes the reader's
 * caret out of the block - a move of the node, whose text the server moves in the room - leaves
 * the last caret kept, which the row puts back (`putKeptCaret`). Nor is one the page makes itself
 * (`placingCaret`): the start of the block that `caretInBlock` puts the caret at, and the nearer
 * end that `selectionIntoBlock` brings a stray end back to.
 */
function noteCaret(
  view: EditorView,
  transaction: Transaction,
  blockId: string | undefined,
  guess: number | undefined,
  remote: boolean
): void {
  if (blockId === undefined || (placingCaret > 0 && !remote)) return;
  const { doc, selection } = transaction;
  const mapped =
    transaction.docChanged && guess !== undefined ? transaction.mapping.map(guess, 1) : guess;
  const start = blockStart(doc, blockId, mapped);
  const block = start === undefined ? null : doc.nodeAt(start);
  if (start === undefined || block === null) return;
  if (selection.from <= start || selection.to >= start + block.nodeSize) return;
  keptCarets.set(view, {
    anchor: selection.anchor - start,
    block: blockId,
    head: selection.head - start,
  });
}

/** The reader's caret in `blockId`'s block of the editor `view`, as its hold last saw it
 *  (`noteCaret`), or undefined where it saw none there. */
export function keptCaret(
  view: EditorView | undefined,
  blockId: string | undefined
): KeptCaret | undefined {
  const kept = view === undefined ? undefined : keptCarets.get(view);
  return kept !== undefined && kept.block === blockId ? kept : undefined;
}

/**
 * Puts the caret back at `kept` - by default the reader's caret its hold last saw
 * (`keptCaret`) - in `blockId`'s block of the editor in `host`, wherever the block stands in the
 * document now, as the editor's own at once (`selectInEditor`), giving the editable the focus
 * where it does not hold it; and answers whether it did. Nothing where the editor draws no such
 * block, keeps no caret for it, holds an input method's composition, or where the place no
 * longer stands in the block's text: the caller then places the caret as it does without this.
 */
export function putKeptCaret(
  host: HTMLElement,
  blockId: string | undefined,
  view: EditorView | undefined,
  kept: KeptCaret | undefined = keptCaret(view, blockId)
): boolean {
  const drawn = drawnBlock(host, blockId);
  if (drawn === null || view === undefined || kept === undefined) return false;
  if (view.isDestroyed || view.composing) return false;
  const { doc } = view.state;
  const start = blockStart(doc, blockId, undefined);
  const block = start === undefined ? null : doc.nodeAt(start);
  if (start === undefined || block === null) return false;
  const pointAt = (offset: number): Point | undefined => {
    const at = Math.min(Math.max(start + offset, start + 1), start + block.nodeSize - 1);
    if (!doc.resolve(at).parent.isTextblock) return undefined;
    try {
      const { node, offset: inNode } = view.domAtPos(at);
      return drawn.block.contains(node) ? [node, inNode] : undefined;
    } catch {
      return undefined;
    }
  };
  const anchor = pointAt(kept.anchor);
  const head = pointAt(kept.head);
  if (anchor === undefined || head === undefined) return false;
  if (!drawn.editable.contains(document.activeElement)) {
    drawn.editable.focus({ preventScroll: true });
  }
  placingCaret += 1;
  selectInEditor(anchor, head);
  placingCaret -= 1;
  return true;
}

/** Whether `transaction`, one of the editor's own in `state`, keeps to `blockId`'s block, found
 *  from `guess`: the block stands after it where the mapping puts it, one node of the type and
 *  attributes it had, with no more line breaks in its headings and each block inside it it left
 *  standing of the type and attributes it had (`innerBlocksKept`); what stands before it and
 *  after it reads as it did; and the selection is inside it. */
function keepsToBlock(
  transaction: Transaction,
  state: EditorState,
  blockId: string | undefined,
  guess: number | undefined
): boolean {
  const from = blockStart(state.doc, blockId, guess);
  const block = from === undefined ? null : state.doc.nodeAt(from);
  if (from === undefined || block === null) return false;
  const to = from + block.nodeSize;
  // Content put right before the block, or right after it, is outside it.
  const start = transaction.mapping.map(from, 1);
  const end = transaction.mapping.map(to, -1);
  const kept = transaction.doc.nodeAt(start);
  const { selection } = transaction;
  // A transaction of no step on this very document - a selection moved, which most of them are -
  // leaves the block's lines and all that stands around it as they were, so they are not read.
  const unchanged = !transaction.docChanged && transaction.doc === state.doc;
  return (
    kept !== null &&
    start + kept.nodeSize === end &&
    kept.hasMarkup(block.type, attributesAsRead(block, kept), block.marks) &&
    (unchanged ||
      (headingBreaks(kept) <= headingBreaks(block) &&
        innerBlocksKept(transaction, block, from) &&
        sameReading(state.doc.content.cut(0, from), transaction.doc.content.cut(0, start)) &&
        sameReading(state.doc.content.cut(to), transaction.doc.content.cut(end)))) &&
    selection.from > start &&
    selection.to < end
  );
}

/** Whether every block inside `block`, which starts at `from` in the document before
 *  `transaction`, that the transaction leaves standing is still a block of the type and the
 *  attributes it was (`ownAttributes`): the block that holds the place where its content started,
 *  wherever the transaction put that place - the paragraph before it, for a paragraph a join put
 *  there, or the heading a paragraph was made into. A block the transaction deleted whole stands
 *  nowhere, and one it made had nothing to keep (`holdEditsToBlock`). */
function innerBlocksKept(transaction: Transaction, block: ProseMirrorNode, from: number): boolean {
  let kept = true;
  block.descendants((inner, offset) => {
    if (!kept || !inner.isBlock) return false;
    // `offset` counts from the block's content, one past its start, and the inner block's own
    // content starts one past the inner block. That place is gone only where a deletion took it
    // in (`deletedAcross`): one that starts or ends there - the first letter deleted as `# `
    // makes a heading, or the boundary a join takes - leaves it standing.
    const place = transaction.mapping.mapResult(from + 1 + offset + 1, 1);
    if (!place.deletedAcross) {
      const now = transaction.doc.resolve(place.pos).parent;
      kept = now.hasMarkup(inner.type, ownAttributes(inner, now), inner.marks);
    }
    return kept && !inner.isTextblock;
  });
  return kept;
}

/** `node`'s attributes, but for those no edit of the reader's sets, which read as `other`'s: the
 *  block's id, which the editor mints for a block a split or a sink makes or moves, and a
 *  heading's `id` (`attributesAsRead`). */
function ownAttributes(node: ProseMirrorNode, other: ProseMirrorNode): ProseMirrorNode["attrs"] {
  const attributes = attributesAsRead(node, other);
  return BLOCK_ID_PROPERTY in attributes
    ? { ...attributes, [BLOCK_ID_PROPERTY]: other.attrs[BLOCK_ID_PROPERTY] }
    : attributes;
}

/** `node`'s attributes, but for a heading's `id`, which reads as `other`'s: Milkdown derives it
 *  from the heading's words (`holdEditsToBlock`). */
function attributesAsRead(node: ProseMirrorNode, other: ProseMirrorNode): ProseMirrorNode["attrs"] {
  return node.type.name === "heading" ? { ...node.attrs, id: other.attrs.id } : node.attrs;
}

/** Whether `before` and `after` hold the same nodes, by type, attributes, marks and text, a
 *  heading's `id` aside (`attributesAsRead`). */
function sameReading(before: Fragment, after: Fragment): boolean {
  if (before.eq(after)) return true;
  if (before.childCount !== after.childCount) return false;
  for (let index = 0; index < before.childCount; index += 1) {
    const was = before.child(index);
    const is = after.child(index);
    if (
      !is.hasMarkup(was.type, attributesAsRead(was, is), was.marks) ||
      (was.isText ? was.text !== is.text : !sameReading(was.content, is.content))
    ) {
      return false;
    }
  }
  return true;
}

/** The line breaks the headings of `node` hold: a hard break reads as one
 *  (`@milkdown/preset-commonmark`'s `leafText`), as a line of text does. */
function headingBreaks(node: ProseMirrorNode): number {
  let breaks = 0;
  const count = (each: ProseMirrorNode): boolean => {
    if (each.type.name !== "heading") return !each.isTextblock;
    breaks += each.textContent.split("\n").length - 1;
    return false;
  };
  if (count(node)) node.descendants(count);
  return breaks;
}

/**
 * Brings each end of the selection that stands out of `blockId`'s block, while the editor in
 * `host` holds the focus, back to the block's nearer end - its start for an end above it, its
 * end for one below - as the editor's own at once (`selectInEditor`). The backstop for a move
 * `confineKey` does not name: a press on another block of the editor, a drag across blocks, a
 * platform's own key for a move. A selection whose ends both stand in the block is left alone,
 * so the report of the move this makes, which calls this again, changes nothing. The selection a
 * browser puts at the editable's start as a script gives the editable the focus back is no such
 * move: the row puts the editor's own selection on the page as the focus comes in, before that
 * reset (`useNodeEditing.ts`), so this finds the caret where the reader left it.
 */
export function selectionIntoBlock(host: HTMLElement, blockId: string | undefined): void {
  const drawn = drawnBlock(host, blockId);
  const selection = window.getSelection();
  const anchor = selection?.anchorNode ?? null;
  const focus = selection?.focusNode ?? null;
  if (
    drawn === null ||
    selection === null ||
    anchor === null ||
    focus === null ||
    !drawn.editable.contains(document.activeElement) ||
    (drawn.block.contains(anchor) && drawn.block.contains(focus))
  ) {
    return;
  }
  const { block } = drawn;
  const start = firstPlace(block);
  const end = lastPlace(block);
  const inside = (point: Point): Point => {
    if (block.contains(point[0])) return point;
    return compare(point, start) < 0 ? start : end;
  };
  placingCaret += 1;
  selectInEditor(inside([anchor, selection.anchorOffset]), inside([focus, selection.focusOffset]));
  placingCaret -= 1;
}

/**
 * What `event` does with the selection from `anchor` to `focus` inside `block`: nothing of this
 * guard's (`undefined`), nothing at all (`null`), or the selection it makes instead, as its
 * anchor and its focus (`confineKey`).
 */
function keyAtBlock(
  event: KeyboardEvent,
  block: HTMLElement,
  anchor: Point,
  focus: Point
): readonly [Point, Point] | null | undefined {
  const jump = event.ctrlKey || event.metaKey;
  const order = compare(anchor, focus);
  const collapsed = order === 0;
  const extendTo = (edge: Point): readonly [Point, Point] => [event.shiftKey ? anchor : edge, edge];
  // The end of the selection a vertical move starts from: the focus where Shift extends the
  // selection, and otherwise the end on the move's side, where the browser collapses it to.
  const earlier = order <= 0 ? anchor : focus;
  const later = earlier === anchor ? focus : anchor;
  // The block's edges are looked for by the keys that move to them alone, not by every letter.
  switch (event.key) {
    case "Home":
      return jump ? extendTo(firstPlace(block)) : undefined;
    case "End":
      return jump ? extendTo(lastPlace(block)) : undefined;
    case "a":
    case "A":
      return (event.metaKey || (event.ctrlKey && !MAC_PLATFORM.test(navigator.platform))) &&
        !event.altKey &&
        !event.shiftKey
        ? [firstPlace(block), lastPlace(block)]
        : undefined;
    case "ArrowUp": {
      const start = firstPlace(block);
      if (event.metaKey) return extendTo(start);
      if (!sameLine(event.shiftKey ? focus : earlier, start)) return undefined;
      return event.shiftKey ? extendTo(start) : null;
    }
    case "ArrowDown": {
      const end = lastPlace(block);
      if (event.metaKey) return extendTo(end);
      if (!sameLine(event.shiftKey ? focus : later, end)) return undefined;
      return event.shiftKey ? extendTo(end) : null;
    }
    case "ArrowLeft":
      return (collapsed || event.shiftKey) && compare(focus, firstPlace(block)) <= 0
        ? null
        : undefined;
    case "ArrowRight":
      return (collapsed || event.shiftKey) && compare(focus, lastPlace(block)) >= 0
        ? null
        : undefined;
    case "PageUp":
    case "PageDown":
      return null;
    // Backspace, Delete, Enter and the editor's own block keys are held to the block by the
    // editor itself (`holdEditsToBlock`).
    default:
      return undefined;
  }
}

/**
 * Opens the editor in `host`, the row being edited, at `blockId`'s block, in a box that leaves the
 * row no taller than the window. The editor holds the whole document, and drawn whole in the row
 * it made the row as tall as the document - 35,635 px for the spec of a real issue of some 160
 * rows - and took everything under the row down by as much. So the box it scrolls in
 * (`EDITOR_BOX_ATTRIBUTE`) is held short enough that the whole row - what the row draws over the
 * box, the box, and the body under it: the node's facts, Keep open, Copy reference - fits the
 * room between the band at the top and the box at the foot of the screen, and that the body ends
 * above the foot's box: the room from the box's top down to the foot is shared with the body
 * under it. The page stays where the reader had it, and every line of the row is on screen, so
 * the editor, which keeps its caret in view as the reader writes, scrolls the box and never the
 * page. A box held to the room under its top alone left the body under the window: a long
 * statement the reader moved to made a row of 971 px in a window 900 px tall. The box - never the
 * page - is then scrolled so the block stands at its top, where the row's words stood, and once
 * more after the drawing above the block has settled (`refocusWhenSettled`): a picture or a
 * diagram above it draws after the editor opened, and pushes it down. The box is held, and the
 * row brought on screen, again then: the body under the box can have grown in that time too, as
 * the page's reads answer - a requirement's counts and gaps come with the coverage read. A body
 * that grows where nothing of the drawing is waited on is not held again.
 *
 * A row so low on the screen that the room under its box's top, less the body, is less than half
 * the room the row leaves its box would leave an editor a line or two tall, so its box takes that
 * half, and the page is scrolled just as far as brings the whole row on screen (`bringOnScreen`,
 * which leaves a row already on screen where it is).
 *
 * A body so tall that the room leaves the box fewer than `LEAST_BOX_LINES` of the editor's lines
 * still gets a box of that many lines. The row is then taller than the window, and the page is
 * scrolled only as far as brings the box on screen, the body running on under the foot's box.
 *
 * Answers what drops that second look.
 */
export function holdEditorAtBlock(host: HTMLElement, blockId: string | undefined): () => void {
  const box = host.querySelector<HTMLElement>(`[${EDITOR_BOX_ATTRIBUTE}]`);
  if (box === null) return () => {};
  const editable = box.querySelector<HTMLElement>('[contenteditable="true"]');
  const least = LEAST_BOX_LINES * lineHeightOf(editable ?? box);
  const place = () => {
    holdBoxInRow(host, box, least);
    const block = blockIn(box, blockId);
    if (block !== null) scrollElementToTop(block, box);
  };
  place();
  return refocusWhenSettled(place, editable ?? undefined);
}

/** The fewest of the editor's lines a row's box is held to, however tall the body under it
 *  (`holdEditorAtBlock`): the line the caret is on and one on either side of it, the least that
 *  still shows the words around the caret rather than a slit a line tall. */
const LEAST_BOX_LINES = 3;

/** CSS's `normal` line height, which a computed style keeps as the word rather than a length:
 *  about 1.2 times the font size, as browsers draw it for most faces. */
const NORMAL_LINE_HEIGHT = 1.2;

/**
 * Holds `box`, the editor's box in `row`, to the height `holdEditorAtBlock`'s rules give it and
 * no lower than `least` px, and brings the row on screen: the whole row where it fits the room,
 * and the box alone where `least` makes the row taller than the room. What the row draws over
 * the box and under it is read from where the two stand, so it counts whatever the row draws
 * there: the body under the words, and on a phone the line of the tag, the author and the time
 * under them (`NodeRow.tsx`'s `NODE_ROW_LAYOUT`).
 */
function holdBoxInRow(row: HTMLElement, box: HTMLElement, least: number): void {
  const published = getComputedStyle(document.documentElement);
  const height = (property: string) => {
    const value = Number.parseFloat(published.getPropertyValue(property));
    return Number.isFinite(value) ? value : 0;
  };
  const floor = window.innerHeight - height(FOOT_HEIGHT_PROPERTY);
  const room = floor - height(IDENTITY_HEIGHT_PROPERTY);
  const rowRect = row.getBoundingClientRect();
  const boxRect = box.getBoundingClientRect();
  const below = rowRect.bottom - boxRect.bottom;
  // The room the row leaves its box, and the room under the box's top the body leaves it.
  const fits = room - (boxRect.top - rowRect.top) - below;
  const under = floor - boxRect.top - below;
  const cap = Math.max(least, Math.min(fits, Math.max(under, fits / 2)));
  // Whole pixels down, so a row that ends on the line the foot's box starts at is not read as
  // reaching under it by a fraction and scrolled for it.
  box.style.maxHeight = `${Math.floor(cap)}px`;
  bringOnScreen(cap > fits ? box : row);
}

/** One line of `element`'s text, in px, from its computed style. */
function lineHeightOf(element: Element): number {
  const style = getComputedStyle(element);
  const line = Number.parseFloat(style.lineHeight);
  return Number.isFinite(line) ? line : NORMAL_LINE_HEIGHT * Number.parseFloat(style.fontSize);
}

/** `blockId`'s block of the editor in `root`, or null where it draws none. */
function blockIn(root: HTMLElement, blockId: string | undefined): HTMLElement | null {
  return blockId === undefined
    ? null
    : root.querySelector<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`);
}

/** The editable of the editor in `host`, and `blockId`'s block of it, or null where it draws no
 *  such block. */
function drawnBlock(
  host: HTMLElement,
  blockId: string | undefined
): { readonly editable: HTMLElement; readonly block: HTMLElement } | null {
  const editable = host.querySelector<HTMLElement>('[contenteditable="true"]');
  const block = editable === null ? null : blockIn(editable, blockId);
  return editable === null || block === null ? null : { editable, block };
}

/** The editable of the editor in `host`, where it holds `target`: the element a key or an input
 *  is sent to while the editable holds the focus. */
function editableHolding(host: HTMLElement, target: EventTarget | null): HTMLElement | null {
  const editable = host.querySelector<HTMLElement>('[contenteditable="true"]');
  return editable !== null && target instanceof Node && editable.contains(target) ? editable : null;
}

/** The selection's anchor and focus, where both stand inside `block`, or null. */
function selectionIn(block: HTMLElement): readonly [Point, Point] | null {
  const selection = window.getSelection();
  const anchor = selection?.anchorNode ?? null;
  const focus = selection?.focusNode ?? null;
  if (selection === null || anchor === null || focus === null) return null;
  if (!block.contains(anchor) || !block.contains(focus)) return null;
  return [
    [anchor, selection.anchorOffset],
    [focus, selection.focusOffset],
  ];
}

/** The first place of `block` a caret can stand at: the start of its first text a caret can
 *  enter, or the block itself where it holds none. An atom's text and a collaborator's cursor
 *  label sit in `contenteditable="false"`, where a caret cannot go. The walk stops at that text. */
function firstPlace(block: HTMLElement): Point {
  const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node.parentElement?.isContentEditable === true) return [node, 0];
  }
  return [block, 0];
}

/** The last place of `block` a caret can stand at: the end of its last text a caret can enter,
 *  or the block itself where it holds none (`firstPlace`). */
function lastPlace(block: HTMLElement): Point {
  const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
  let last: Node | null = null;
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node.parentElement?.isContentEditable === true) last = node;
  }
  return last === null ? [block, 0] : [last, last.textContent?.length ?? 0];
}

/** Where `point` stands against `edge` in the page: before it (negative), at it (zero) or after
 *  it (positive). */
function compare(point: Point, edge: Point): number {
  const range = document.createRange();
  range.setStart(edge[0], edge[1]);
  return range.comparePoint(point[0], point[1]);
}

/** Whether a caret at `point` stands on the line a caret at `edge` does: the middle of its box
 *  inside the other's. A place the page draws no box for counts as on that line, so a move whose
 *  line cannot be read stays in the block. */
function sameLine(point: Point, edge: Point): boolean {
  const caret = caretBox(point);
  const line = caretBox(edge);
  if (caret === null || line === null) return true;
  const middle = (caret.top + caret.bottom) / 2;
  return middle >= line.top && middle <= line.bottom;
}

/** The box of a caret at `point`: the collapsed range's own, or the box of the element it is in
 *  where the browser draws the range none - a place between elements, an empty paragraph. */
function caretBox([node, offset]: Point): DOMRect | null {
  const range = document.createRange();
  range.setStart(node, offset);
  const own: DOMRect | undefined = range.getClientRects()[0];
  if (own !== undefined) return own;
  return (node instanceof Element ? node : node.parentElement)?.getBoundingClientRect() ?? null;
}
