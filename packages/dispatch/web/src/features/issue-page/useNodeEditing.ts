import type { EditorView } from "@milkdown/kit/prose/view";
import { type RefObject, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact } from "../../api/types";
import { focusOnDocument } from "../shell/roving";
import { documentOfNode, type NodeDocument } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import {
  caretInBlock,
  caretOutOfBlock,
  confineKey,
  drawsBlock,
  EDITOR_BOX_ATTRIBUTE,
  type HoldParts,
  holdEditorAtBlock,
  holdEditsToBlock,
  loadEditsHold,
  selectInEditor,
  selectionIntoBlock,
  tabStopsFrom,
} from "./node-caret";
import { KEY_REPORTS } from "./page-structure";
import { PRESS_ENDS } from "./press-focus";
import { rowSelectorOf } from "./tree-focus";

/** The editor the page draws for an edited row, as the row takes it: its ProseMirror view, which
 *  the row holds every edit of to the node's block (`node-caret.ts`'s `holdEditsToBlock`), and
 *  whose own selection it reads a key against (`confineKey`). `doc/editor.ts`'s `EditorHandle` is
 *  one. */
export interface RowEditor {
  readonly view: EditorView;
}

/** What the page hands a row's editor to (`OnEditing`). */
export type OnEditor = (editor: RowEditor | undefined) => void;

/** How the tree tells the page which document a row is being edited in - `undefined` while none
 *  is - so the page draws that document's one editor, and how the page hands the row that editor:
 *  `onEditor`, with the editor as it is made, and with `undefined` as it is torn down. */
export type OnEditing = (artifactId: string | undefined, onEditor?: OnEditor) => void;

/** One row the reader is editing in: the document its node's text lives in, the block of that
 *  document the node is, and the key of the row holding the editor (`rowKeyOf`), which is what
 *  tells one placement of a node from another. */
export interface EditedRow {
  readonly artifactId: string;
  readonly blockId: string;
  readonly row: string;
}

export interface NodeEditing {
  /** The row holding the editor, or `undefined` while the reader is editing nothing. */
  readonly editing: EditedRow | undefined;
  /** Puts the caret in one row's node, in the document that node's text lives in. */
  readonly edit: (row: string, node: TreeNode) => void;
  /** Takes the caret out again and gives the row the focus back. */
  readonly stopEditing: () => void;
}

/** A reader's own acts, any of which ends the page's own placing of the focus - the tree's
 *  (`IssueTree.tsx`), and the caret's in the row being edited below: a key, a press, a wheel and
 *  a touch. */
export const READER_ACTS = ["keydown", "pointerdown", "wheel", "touchstart"] as const;

/** A controller the reader's first act (`READER_ACTS`, read on the window in the capture phase)
 *  aborts, calling `then` after it. Aborted any other way, it calls nothing. */
export function abortOnReaderAct(then?: () => void): AbortController {
  const acts = new AbortController();
  for (const type of READER_ACTS) {
    window.addEventListener(
      type,
      () => {
        acts.abort();
        then?.();
      },
      { capture: true, signal: acts.signal }
    );
  }
  return acts;
}

/**
 * Whether a row takes typing, and so draws the cue that it does (`styles.css`), given `place`,
 * its node's place in one of the issue's documents. That place is Enter's own rule
 * (`documentOfNode`): a block of one of the issue's documents, which Enter puts the caret in
 * (`useNodeEditing`'s `edit`), a statement and a check among them, takes typing on an issue that
 * is open. A closed issue's documents take comments by quote and no edit (`ProofDocument`'s
 * `editorAccess`), so none of its blocks takes typing, though Enter still opens the editor
 * there. Every other node has no place - the issue, a child issue, an ask, a comment, a message,
 * a result, a block of another issue's document such as one above it - and takes no caret at all.
 */
export function nodeEditable(place: NodeDocument | undefined, isClosed: boolean): boolean {
  return !isClosed && place !== undefined;
}

/**
 * The caret in a node's own text, where that text lives: the node's block of its own document,
 * with the collaborative cursors and the comment chord it has there.
 *
 * The tree holds no editor of its own. The page draws the one editor of whichever document a
 * row is editing (`slots/body.tsx`, through `onEditing`), `document-stage.tsx` moves it onto the
 * row, and this places the caret in the node's own block of it and opens the editor's box at
 * that block, no taller than the window (`node-caret.ts`), the moment the row holds it with that
 * block drawn: the page mounts that editor in the commit after the row asked for it, and the
 * editor draws the document's text once it has read it. No caret goes anywhere else in the
 * document, where the keys would edit what the reader never chose. A reader who has acted and
 * taken the focus out of the row before then has moved on from it, and the editing ends with no
 * caret placed - for a press, once it has ended, so nothing moves under the pointer; a key that
 * left the focus on the row is one for the statement, and the caret goes in.
 * The editor's own redrawing can drop that caret before the reader has done anything, and this
 * puts it back until they have. From then on, a focus the editor drops while the reader writes
 * goes back to the caret they had, or to the node's block where the editor has redrawn the text
 * that caret was in, and stays where it fell where that block is gone too. Every edit stays
 * inside the node's block while the row holds the editor: the page hands the row its editor as
 * the editor is made (`onEditing`), and the row holds that editor's own edits to the block
 * (`node-caret.ts`'s `holdEditsToBlock`), and its caret (`confineKey`): the editor is the
 * document's whole editor, and free editing of the whole document stays on the document page.
 * That hold is the caret's precondition: no caret goes in, and no key that could edit reaches
 * the editor, until it is on, and the focus stays on the row till then. Where it cannot go on -
 * what it is built of did not load, or the page handed the row no editor - the editing ends,
 * and the row says its editor could not open (`KEY_REPORTS.editor_closed`). A node whose text
 * no document of this issue holds has nothing to put a caret in, and the key says so.
 *
 * Escape leaves the caret for the row it came from. The editor calls `preventDefault` on the
 * key before it reaches the page's keyboard registry, which drops every binding of the press
 * (`features/shell/keymap.ts`'s `handleKeyDown`), so the row listens for it in the capture
 * phase while it is being edited in - before the editor sees it - and the registry's own
 * binding stands for every other surface (`useTreeKeymap`'s `tree-leave-caret`). The editor
 * then asserts its own selection again a moment after the blur (`prosemirror-view`'s focus
 * handler) and its stage moves off the row in the same commit, so the row takes the focus once
 * more on the next frame as well as the moment Escape was pressed, where the focus has fallen to
 * nothing by then and the reader has not acted since: a key or a press after Escape, or a focus
 * something else took, is where the focus stays.
 */
export function useNodeEditing({
  artifacts,
  container,
  onEditing,
  onReport,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The tree's own box, which the rows are found inside. */
  container: RefObject<HTMLElement | null>;
  /** Tells the page which document a row is editing, so it draws that document's one editor and
   *  hands it to the row. */
  onEditing: OnEditing;
  /** What a key reports when the node it was pressed on has no text to put a caret in, or its
   *  editor could not open. */
  onReport: (report: string) => void;
}): NodeEditing {
  const [editing, setEditing] = useState<EditedRow | undefined>(undefined);
  // The row the reader has just left the caret in, so that row takes the focus back.
  const leaving = useRef<string | undefined>(undefined);

  const edit = useCallback(
    (row: string, node: TreeNode) => {
      const place = documentOfNode(node.id, artifacts);
      if (place === undefined) {
        onReport(KEY_REPORTS.no_text);
        return;
      }
      setEditing({ artifactId: place.artifact.id, blockId: place.blockId, row });
    },
    [artifacts, onReport]
  );
  const stopEditing = useCallback(() => {
    setEditing((held) => {
      if (held !== undefined) leaving.current = held.row;
      return undefined;
    });
  }, []);

  const editingArtifact = editing?.artifactId;
  const editingBlock = editing?.blockId;
  // The row the caret went into, not the node it holds: one node may be drawn under two
  // parents, so its id answers with whichever copy the fold drew first, which is the wrong row
  // whenever the reader is editing the second. The row's key is the one thing that tells two
  // placements apart (`tree-keys.ts`'s `rowKeyOf`, `[data-row-key]`).
  const editingRow = editing?.row;

  // The block the editor the page hands over holds every edit to (`holdEditsToBlock`), read as
  // each edit is made: none, while no row is edited, lets no edit in.
  const heldBlock = useRef<string | undefined>(undefined);
  heldBlock.current = editingBlock;
  // What that hold is built of, as the row being edited asked for it, and whether it failed to
  // load.
  const load = useRef<{ failed: boolean; readonly parts: Promise<HoldParts> } | undefined>(
    undefined
  );
  // Where the hold stands: `awaited` while the page has handed the row no editor, `loading`
  // from the editor handed over until the hold is on it, and `on` from then, which is what a
  // caret waits for (below).
  const hold = useRef<"awaited" | "loading" | "on">("awaited");
  // Looks at the row again as the hold goes on or its load fails, while the row is watched.
  const holdChanged = useRef<(() => void) | undefined>(undefined);
  // What takes the hold off the editor the page handed over last.
  const letGo = useRef<(() => void) | undefined>(undefined);
  // The view of the editor the page handed over last, while the page holds it.
  const editorView = useRef<EditorView | undefined>(undefined);
  const onEditor = useCallback((editor: RowEditor | undefined) => {
    letGo.current?.();
    letGo.current = undefined;
    hold.current = "awaited";
    editorView.current = editor?.view;
    const parts = load.current?.parts;
    if (editor === undefined || parts === undefined) return;
    hold.current = "loading";
    letGo.current = holdEditsToBlock(
      editor.view,
      () => heldBlock.current,
      parts,
      () => {
        hold.current = "on";
        holdChanged.current?.();
      }
    );
  }, []);
  // What reports, read as a report is made, so the effect that places the caret runs once for
  // a row whatever the page hands the tree to report with.
  const report = useRef(onReport);
  report.current = onReport;

  useEffect(() => {
    if (editingArtifact !== undefined) {
      // What the hold is built of loads as the row asks for its editor, so the hold goes on in
      // the task the page hands that editor over in, before a key or a press can reach the
      // editor. A load that fails leaves the hold off whichever editor comes, so the row is
      // looked at again, which ends the editing and says so (below); the next edit loads again.
      const asked = { failed: false, parts: loadEditsHold() };
      load.current = asked;
      asked.parts.catch(() => {
        asked.failed = true;
        holdChanged.current?.();
      });
    }
    onEditing(editingArtifact, onEditor);
    // The outline going - a document opened in its place - takes the row's editing with it, so
    // the page holds no editor for a row that is no longer drawn.
    return () => onEditing(undefined);
  }, [editingArtifact, onEditing, onEditor]);

  // The tree going takes the hold off the editor with it, whatever the page does with that editor.
  useEffect(
    () => () => {
      letGo.current?.();
      letGo.current = undefined;
    },
    []
  );

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    // The row's stage is in its words (`NodeRow.tsx`), so the row as a whole is where the
    // editor is looked for. It arrives over several commits, and may draw the document's blocks
    // over more than one frame once it has read the document, others before the node's, so the
    // caret is placed the first time the row holds one that has drawn the node's block
    // (`caretInBlock`), and the editor opened at the block then, and only then. Until then the
    // focus stays on the row, where no key edits the document, and an editor that never draws
    // the block gets no caret at all.
    //
    // Unless the reader has moved on from the row by then: the focus off the row itself and its
    // editor once they have acted (`READER_ACTS`), while the editor was still drawing - `j` to
    // the next row, Tab out of the outline, a press elsewhere, or a control inside the row
    // itself, as `Shift+C` focuses the change's own box and `f` the row's links. A caret placed
    // after that took the focus back from where they had put it, and the letters they typed next
    // went into the statement, which is saved to the shared document. So the first look for the
    // caret after that places none and ends the editing, and no row takes the focus back, as the
    // row does after Escape (`leaving`); where the focus is out of the tree as well, the tree
    // lets go of the row as the editing ends (`IssueTree.tsx`'s `useReleaseFocus`). A key that
    // leaves the focus on the row itself - Delete pressed the moment Enter is - is one for the
    // statement, and the caret goes in all the same, unless a later act takes the focus off the
    // row first. So does a focus a script moves off the row before the reader has acted, since
    // only their own act ends the page's placing of the focus.
    //
    // The row is watched on after the caret is placed, until the reader's first act, for two
    // things the editor does to the caret before the reader has done anything. It changes its
    // plugins after it has drawn the text - Dispatch's own block views as it binds
    // (`doc/editor.ts`'s `installTypedBlocks`), its collaborators' cursors once the room has
    // synced (`@legion/proof-editor`'s `lib.ts`) - and ProseMirror then makes every plugin's view
    // again, Milkdown's among them, which takes the editable out of the box it wraps it in and
    // wraps it in a new one. The browser drops the focus to the page as the editable leaves it:
    // measured in Chromium, a `focusout` with no related target 45 ms after the caret was placed,
    // and the focus on the page's body after, with the caret moved out to where the editable
    // stood. So while the reader has not acted, a focus fallen to nothing and a caret moved out
    // of the node's block go back to the block, while the editor still draws it, which the
    // editor takes as its own in the same task (`caretInBlock`); from their first act the focus
    // and the caret are theirs, inside the node's block: an end of the selection moved out of
    // it - a press on another block of the editor, a drag, a key's move the guard below does
    // not name - goes back to the block's nearer end (`selectionIntoBlock`).
    const rowSelector = rowSelectorOf(editingRow);
    let settled: (() => void) | undefined;
    // The row is looked at apart from the editor's drawing as well, until the caret is placed:
    // no draw need follow a move off the row to look again - an editor that never draws the
    // node's block draws nothing more. So it is looked at a task after the reader's first act,
    // and a task after each focus move off it (a `focusout` on the row): that first act may leave
    // the focus on the row - Shift, Delete - and a later one take it off - Tab, a press - and a
    // tap moves the focus only at the `mousedown` that follows its `touchend`. A press is looked
    // after once it has ended (`PRESS_ENDS`), a task after its click, as the tree lets go of a
    // row a press left (`useReleaseFocus`), and the editing does not end while it is held: until
    // the caret is placed the editor's box stands in the row as tall as what the editor has
    // drawn, and the editing ending a task after the press went down took that box out of the
    // row with the pointer still down - the controls below the row rose under it, and the click
    // landed on something else.
    const looking = new AbortController();
    const options = { capture: true, signal: looking.signal };
    let pressing = false;
    let look: number | undefined;
    const lookSoon = () => {
      if (pressing) return;
      window.clearTimeout(look);
      look = window.setTimeout(changed, 0);
    };
    const pressed = () => {
      pressing = true;
      window.clearTimeout(look);
    };
    const ended = () => {
      if (!pressing) return;
      window.clearTimeout(look);
      look = window.setTimeout(() => {
        pressing = false;
        changed();
      }, 0);
    };
    const movedOff = (event: FocusEvent) => {
      const row = box.querySelector(rowSelector);
      if (row !== null && event.target instanceof Node && row.contains(event.target)) lookSoon();
    };
    window.addEventListener("pointerdown", pressed, options);
    window.addEventListener("mousedown", pressed, options);
    for (const type of PRESS_ENDS) window.addEventListener(type, ended, options);
    box.addEventListener("focusout", movedOff, options);
    const acts = abortOnReaderAct(() => {
      if (settled !== undefined) watch.disconnect();
      else lookSoon();
    });
    const confining = new AbortController();
    const confine = () => {
      if (!acts.signal.aborted) return;
      const row = box.querySelector<HTMLElement>(rowSelector);
      if (row !== null) selectionIntoBlock(row, editingBlock);
    };
    // Whether the editing has ended here, with no caret placed.
    let over = false;
    // Whether the page has handed the row no editor to hold, read a task after the node's block
    // is first seen drawn with none handed over. A page hands its editor over in the task the
    // editor is made in (`doc/ProofDocument.tsx`'s `onEditorHandle`), and an editor made for a
    // document the room already holds has drawn it by then - y-prosemirror draws as its plugin
    // goes on - some microtasks before it is handed over, where a look at the row can fall.
    let unhanded = false;
    let handing: number | undefined;
    // Ends the editing with no caret placed - the reader took the focus out of the row first, or
    // the editor's hold on its edits cannot go on - and answers whether it did: a press ends it
    // once it has ended (`ended`), so nothing moves under it.
    const endUnplaced = (row: HTMLElement, movedOn: boolean): boolean => {
      if (pressing) return false;
      over = true;
      if (movedOn) {
        setEditing(undefined);
        return true;
      }
      // The row says its editor could not open, where the reader is: a focus their press put in
      // the editor's box goes back to the row first, as Escape's does, since that box goes.
      if (holdsRowsFocus(row)) row.focus();
      report.current(KEY_REPORTS.editor_closed);
      stopEditing();
      return true;
    };
    // Places the caret, or places it again where the focus has fallen to nothing or the caret
    // has left the block, and answers whether the row is done with: the caret placed and the
    // reader acted since, or the editing ended with none placed - the reader took the focus out
    // of the row first, or the editor's hold on its edits cannot go on.
    const place = (): boolean => {
      const row = box.querySelector<HTMLElement>(rowSelector);
      if (over) return true;
      if (row === null) return false;
      if (settled !== undefined) {
        // An editor made again in the row's place is no editor a caret goes in until its own
        // hold is on.
        if (hold.current === "on" && (focusOnDocument() || caretOutOfBlock(row, editingBlock))) {
          caretInBlock(row, editingBlock);
        }
        return acts.signal.aborted;
      }
      const movedOn = acts.signal.aborted && !holdsRowsFocus(row);
      if (hold.current === "awaited" && handing === undefined && drawsBlock(row, editingBlock)) {
        handing = window.setTimeout(() => {
          // An editor made for a connection the room then refused draws the block and goes
          // unhanded, and the editor for the next connection comes after a sync: where the
          // block is no longer drawn, the wait starts again with the next editor that draws it.
          const now = box.querySelector<HTMLElement>(rowSelector);
          unhanded = hold.current === "awaited" && now !== null && drawsBlock(now, editingBlock);
          if (unhanded) changed();
          else handing = undefined;
        }, 0);
      }
      // The hold cannot go on where what it is built of did not load, or where the page has
      // handed the row no editor to hold.
      const unheld = load.current?.failed === true || (unhanded && hold.current === "awaited");
      if (movedOn || unheld) return endUnplaced(row, movedOn);
      // No caret goes in an editor that does not hold its edits to the node's block yet: the
      // focus stays on the row, where no key edits the document, and the row is looked at
      // again as the hold goes on (`holdChanged`).
      if (hold.current !== "on" || !caretInBlock(row, editingBlock)) return false;
      settled = holdEditorAtBlock(row, editingBlock);
      window.clearTimeout(look);
      looking.abort();
      // Only a selection moved after the caret was placed is the editor's doing.
      document.addEventListener("selectionchange", changed, { signal: acts.signal });
      document.addEventListener("selectionchange", confine, { signal: confining.signal });
      return acts.signal.aborted;
    };
    const changed = () => {
      if (place()) watch.disconnect();
    };
    // The hold going on, or failing to load, is looked at while the row is watched: until the
    // caret is placed and the reader has acted since.
    holdChanged.current = () => {
      if (settled === undefined || !acts.signal.aborted) changed();
    };
    const watch = new MutationObserver(changed);
    const frame = requestAnimationFrame(() => {
      if (place()) return;
      watch.observe(box, {
        attributeFilter: ["contenteditable"],
        attributes: true,
        childList: true,
        subtree: true,
      });
    });
    return () => {
      holdChanged.current = undefined;
      cancelAnimationFrame(frame);
      window.clearTimeout(handing);
      window.clearTimeout(look);
      looking.abort();
      acts.abort();
      confining.abort();
      watch.disconnect();
      settled?.();
    };
  }, [container, editingBlock, editingRow, stopEditing]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    // The editor goes on changing its plugins after the reader's first act - its collaborators'
    // cursors go in once the room's mapping is ready, which it tries for up to 120 frames
    // (`@legion/proof-editor`'s `lib.ts`) - and each change makes its plugins' views again,
    // Milkdown's among them, which takes the editable out of the page and puts it back: the
    // focus falls to the page's body while the reader writes, and the keys they press next go
    // nowhere. So the editable losing the focus to nothing (a `focusout` naming no element)
    // gets it back on the next frame, at the caret it held as it lost it, which the editor takes
    // as its own at once (`selectInEditor`), where it is still in the row and nothing else has
    // taken the focus by then. The caret is kept as its points, since a range is moved out of a
    // node the page takes out. The editor can redraw its text while it has no focus, and the
    // points are gone then: the caret goes to the start of the node's block instead
    // (`caretInBlock`), which the editor takes as its own at once too. An editable focused with
    // no caret put in it takes the next key where the browser puts its caret - in Chromium, the
    // editable's start, outside the node's block - so where that block is gone as well
    // `caretInBlock` focuses nothing, and the focus stays where it fell. A press is the reader's
    // own: one on blank space takes the focus to nothing in the same task as its `pointerdown`
    // and `mousedown`, and leaves it there. Escape focuses the row, so it takes nothing back
    // either.
    const rowSelector = rowSelectorOf(editingRow);
    const listening = new AbortController();
    const options = { capture: true, signal: listening.signal };
    let pressed = false;
    let pressEnding: number | undefined;
    let frame: number | undefined;
    const press = () => {
      pressed = true;
      window.clearTimeout(pressEnding);
      pressEnding = window.setTimeout(() => {
        pressed = false;
      }, 0);
    };
    const dropped = (event: FocusEvent) => {
      const editable = event.target;
      if (event.relatedTarget !== null || pressed || !(editable instanceof HTMLElement)) return;
      if (editable !== box.querySelector(rowSelector)?.querySelector('[contenteditable="true"]')) {
        return;
      }
      const selection = window.getSelection();
      const anchor = selection?.anchorNode ?? null;
      const anchorOffset = selection?.anchorOffset ?? 0;
      const focus = selection?.focusNode ?? null;
      const focusOffset = selection?.focusOffset ?? 0;
      if (frame !== undefined) cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        if (!focusOnDocument()) return;
        const row = box.querySelector<HTMLElement>(rowSelector);
        if (row === null || !row.contains(editable)) return;
        if (
          anchor !== null &&
          focus !== null &&
          pointIn(editable, anchor, anchorOffset) &&
          pointIn(editable, focus, focusOffset)
        ) {
          editable.focus({ preventScroll: true });
          selectInEditor([anchor, anchorOffset], [focus, focusOffset]);
        } else {
          caretInBlock(row, editingBlock);
        }
      });
    };
    window.addEventListener("pointerdown", press, options);
    window.addEventListener("mousedown", press, options);
    box.addEventListener("focusout", dropped, options);
    return () => {
      listening.abort();
      window.clearTimeout(pressEnding);
      if (frame !== undefined) cancelAnimationFrame(frame);
    };
  }, [container, editingBlock, editingRow]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    // The row's editor is the document's whole editor, and the keys it answers for a whole
    // document - Ctrl+Home, Up from the block's first line, Ctrl+A - took the caret, and the
    // letters typed next, out of the node's block to text the reader cannot see. The editor
    // holds its edits to the block itself (`holdEditsToBlock`, above), and its selection, but
    // only after the browser has moved the page's caret and scrolled the box to it; so each key
    // in the editor is read before the editor's own listener (the capture phase) and keeps the
    // caret inside the node's block (`confineKey`), from the moment the row holds the editor:
    // while the node's block is not the selection - before the caret is placed, or a Tab into
    // the editable before the block is drawn - no key that could edit reaches the editor at all,
    // and none does while the editor's hold is not on, where the row reads as one with no block.
    //
    // Tab and Shift+Tab the editor leaves to the browser - Milkdown answers them in a list item
    // alone, where it sinks and lifts the item - moved the focus on to whatever the editor draws
    // next, the whole document's: driving a preview, Tab from a statement's caret went to a
    // control another block draws, an ask's Copy reference, and the box scrolled to it, away from
    // the statement. So each such key is read again once the editor has had it (the bubble
    // phase): where its default stands, the focus goes where the browser would take it, past
    // every control the editor draws outside the node's block (`tabStopsFrom`): to a control the
    // block draws itself, or the action bar's while it shows the selection's acts, or back to the
    // editable, and past those Tab goes to the first tab stop after the editor's box and Shift+Tab
    // to the row. A stop that refuses the focus, one the page does not draw, is stepped past. Out
    // of the box, the editing ends: Shift+Tab's as Escape's does, the row holding the focus, and
    // Tab's as a Tab out of the outline does, which lets go of the row where the focus is out of
    // the tree (`IssueTree.tsx`'s `useReleaseFocus`); a Tab past the page's last tab stop leaves
    // the focus on the page itself, as a browser's leaves the page. The key's default goes, so
    // the box scrolls to nothing on the way, and nothing of the document changes. While the caret
    // is not in the block the key never gets here (`confineKey`), and goes on to the browser.
    const rowSelector = rowSelectorOf(editingRow);
    const listening = new AbortController();
    box.addEventListener(
      "keydown",
      (event) => {
        const row = box.querySelector<HTMLElement>(rowSelector);
        if (row === null) return;
        confineKey(
          row,
          hold.current === "on" ? editingBlock : undefined,
          event,
          editorView.current
        );
      },
      { capture: true, signal: listening.signal }
    );
    box.addEventListener(
      "keydown",
      (event) => {
        if (
          event.key !== "Tab" ||
          event.defaultPrevented ||
          event.altKey ||
          event.ctrlKey ||
          event.metaKey ||
          hold.current !== "on" ||
          !(event.target instanceof Element)
        ) {
          return;
        }
        const row = box.querySelector<HTMLElement>(rowSelector);
        const stops =
          row === null ? null : tabStopsFrom(row, editingBlock, event.target, event.shiftKey);
        if (row === null || stops === null) return;
        event.preventDefault();
        const to = stops.find((stop) => {
          stop.focus();
          return document.activeElement === stop;
        });
        if (to === undefined) (document.activeElement as HTMLElement | null)?.blur();
        else if (row.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`)?.contains(to) === true) return;
        if (to === row) stopEditing();
        else setEditing(undefined);
      },
      { signal: listening.signal }
    );
    return () => listening.abort();
  }, [container, editingBlock, editingRow, stopEditing]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    const leave = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      (document.activeElement as HTMLElement | null)?.blur();
      box.querySelector<HTMLElement>(rowSelectorOf(editingRow))?.focus();
      stopEditing();
    };
    box.addEventListener("keydown", leave, true);
    return () => box.removeEventListener("keydown", leave, true);
  }, [container, editingRow, stopEditing]);

  useEffect(() => {
    const left = leaving.current;
    if (editingRow !== undefined || left === undefined) return;
    leaving.current = undefined;
    const acted = abortOnReaderAct();
    const frame = requestAnimationFrame(() => {
      if (acted.signal.aborted || !focusOnDocument()) return;
      container.current?.querySelector<HTMLElement>(rowSelectorOf(left))?.focus();
    });
    return () => {
      cancelAnimationFrame(frame);
      acted.abort();
    };
  }, [container, editingRow]);

  return { edit, editing, stopEditing };
}

/** Whether the focus is still the edited row's: on the row itself, or inside the box its editor
 *  is drawn in (`EDITOR_BOX_ATTRIBUTE`). A press of the reader's in that box before the node's
 *  block is drawn is the row's, and the caret goes to that block once it is, since a key may
 *  edit the node's own block alone. A control inside the row - a change's own box, a link `f`
 *  stepped to - is somewhere the reader moved to. */
function holdsRowsFocus(row: HTMLElement): boolean {
  const active = document.activeElement;
  if (active === row) return true;
  return row.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`)?.contains(active) === true;
}

/** Whether `offset` in `node` is still a place in `editable` a caret can be put: the editor can
 *  redraw its text while it has no focus. */
function pointIn(editable: HTMLElement, node: Node, offset: number): boolean {
  const length = node instanceof CharacterData ? node.length : node.childNodes.length;
  return editable.contains(node) && offset <= length;
}
