import { type RefObject, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact } from "../../api/types";
import { focusOnDocument } from "../shell/roving";
import { documentOfNode } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { caretInBlock, caretOutOfBlock, holdEditorAtBlock } from "./node-caret";
import { KEY_REPORTS } from "./page-structure";

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
const READER_ACTS = ["keydown", "pointerdown", "wheel", "touchstart"] as const;

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
 * The caret in a node's own text, where that text lives: the node's block of its own document,
 * with the collaborative cursors and the comment chord it has there.
 *
 * The tree holds no editor of its own. The page draws the one editor of whichever document a
 * row is editing (`slots/body.tsx`, through `onEditing`), `document-stage.tsx` moves it onto the
 * row, and this places the caret in the node's own block of it and opens the editor's box at
 * that block, no taller than the window (`node-caret.ts`), the moment the row holds it: the page
 * mounts that editor in the commit after the row asked for it, and the editor draws the
 * document's text once it has read it. The editor's own redrawing can drop that caret before the
 * reader has done anything, and this puts it back until they have. From then on, a focus the
 * editor drops while the reader writes goes back to the caret they had. A node whose text no
 * document of this issue holds has nothing to put a caret in, and the key says so.
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
  /** Tells the page which document a row is editing, so it draws that document's one editor. */
  onEditing: (artifactId: string | undefined) => void;
  /** What a key reports when the node it was pressed on has no text to put a caret in. */
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

  useEffect(() => {
    onEditing(editingArtifact);
    // The outline going - a document opened in its place - takes the row's editing with it, so
    // the page holds no editor for a row that is no longer drawn.
    return () => onEditing(undefined);
  }, [editingArtifact, onEditing]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    // The row's stage is in its words (`NodeRow.tsx`), so the row as a whole is where the
    // editor is looked for. It arrives over several commits, and is ready for the caret once it
    // has drawn the document's text (`caretInBlock`), so the caret is placed the first time the
    // row holds one that has, and the editor opened at the block then, and only then.
    //
    // The row is watched on after that, until the reader's first act (`READER_ACTS`), for two
    // things the editor does to the caret before the reader has done anything. It changes its
    // plugins after it has drawn the text - Dispatch's own block views as it binds
    // (`doc/editor.ts`'s `installTypedBlocks`), its collaborators' cursors once the room has
    // synced (`@legion/proof-editor`'s `lib.ts`) - and ProseMirror then makes every plugin's view
    // again, Milkdown's among them, which takes the editable out of the box it wraps it in and
    // wraps it in a new one. The browser drops the focus to the page as the editable leaves it:
    // measured in Chromium, a `focusout` with no related target 45 ms after the caret was placed,
    // and the focus on the page's body after. And the editor writes its own selection back over
    // the caret while it has not read the caret yet (`caretOutOfBlock`). So while the reader has
    // not acted, a focus fallen to nothing and a caret moved out of the node's block go back to
    // the block; from their first act the focus and the caret are theirs.
    let settled: (() => void) | undefined;
    const acts = abortOnReaderAct(() => {
      if (settled !== undefined) watch.disconnect();
    });
    // Places the caret, or places it again where the focus has fallen to nothing or the caret
    // has left the block, and answers whether the row is done with: the caret placed and the
    // reader acted since.
    const place = (): boolean => {
      const row = box.querySelector<HTMLElement>(
        `[data-node-row][data-row-key="${CSS.escape(editingRow)}"]`
      );
      if (row === null) return false;
      if (settled === undefined) {
        if (!caretInBlock(row, editingBlock)) return false;
        settled = holdEditorAtBlock(row, editingBlock);
        // Only a selection moved after the caret was placed is the editor's doing.
        document.addEventListener("selectionchange", changed, { signal: acts.signal });
      } else if (focusOnDocument() || caretOutOfBlock(row, editingBlock)) {
        caretInBlock(row, editingBlock);
      }
      return acts.signal.aborted;
    };
    const changed = () => {
      if (place()) watch.disconnect();
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
      cancelAnimationFrame(frame);
      acts.abort();
      watch.disconnect();
      settled?.();
    };
  }, [container, editingBlock, editingRow]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    // The editor goes on changing its plugins after the reader's first act - its collaborators'
    // cursors go in once the room's mapping is ready, which it tries for up to 120 frames
    // (`@legion/proof-editor`'s `lib.ts`) - and each change makes its plugins' views again,
    // Milkdown's among them, which takes the editable out of the page and puts it back: the
    // focus falls to the page's body while the reader writes, and the keys they press next go
    // nowhere. So the editable losing the focus to nothing (a `focusout` naming no element)
    // gets it back on the next frame, at the caret it held as it lost it, where it is still in
    // the row and nothing else has taken the focus by then. The caret is kept as its points,
    // since a range is moved out of a node the page takes out. A press is the reader's own:
    // one on blank space takes the focus to nothing in the same task as its `pointerdown` and
    // `mousedown`, and leaves it there. Escape focuses the row, so it takes nothing back either.
    const rowSelector = `[data-node-row][data-row-key="${CSS.escape(editingRow)}"]`;
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
        if (box.querySelector(rowSelector)?.contains(editable) !== true) return;
        editable.focus({ preventScroll: true });
        if (
          anchor !== null &&
          focus !== null &&
          pointIn(editable, anchor, anchorOffset) &&
          pointIn(editable, focus, focusOffset)
        ) {
          window.getSelection()?.setBaseAndExtent(anchor, anchorOffset, focus, focusOffset);
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
  }, [container, editingRow]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    const leave = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      (document.activeElement as HTMLElement | null)?.blur();
      box
        .querySelector<HTMLElement>(`[data-node-row][data-row-key="${CSS.escape(editingRow)}"]`)
        ?.focus();
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
      container.current
        ?.querySelector<HTMLElement>(`[data-node-row][data-row-key="${CSS.escape(left)}"]`)
        ?.focus();
    });
    return () => {
      cancelAnimationFrame(frame);
      acted.abort();
    };
  }, [container, editingRow]);

  return { edit, editing, stopEditing };
}

/** Whether `offset` in `node` is still a place in `editable` a caret can be put: the editor can
 *  redraw its text while it has no focus. */
function pointIn(editable: HTMLElement, node: Node, offset: number): boolean {
  const length = node instanceof CharacterData ? node.length : node.childNodes.length;
  return editable.contains(node) && offset <= length;
}
