import type { EditorView } from "@milkdown/kit/prose/view";
import type { MouseEvent } from "react";
import { type RefObject, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact, DisplayTree } from "../../api/types";
import { inputMethodKey } from "../../lib/input-method-key";
import { focusOnDocument } from "../shell/roving";
import type { DocumentAdmissions } from "./document-admission";
import { useDocumentAdmissions } from "./document-admission";
import { documentOfNode, type NodeDocument } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { roomTarget, takesCaret, useRoomFacts } from "./model-rows/room-target";
import {
  type CaretAt,
  caretForNewBlock,
  caretInBlock,
  caretOutOfBlock,
  confineKey,
  drawsBlock,
  EDITOR_BOX_ATTRIBUTE,
  type EndKey,
  type HoldParts,
  holdEditorAtBlock,
  holdEditsToBlock,
  loadEditsHold,
  type MoveKey,
  putKeptCaret,
  selectInEditor,
  selectionIntoBlock,
  tabStopsFrom,
} from "./node-caret";
import { useOutlineWriter } from "./outline-writer";
import { KEY_REPORTS } from "./page-structure";
import { trackPress } from "./press-focus";
import { caretOnEnter } from "./text-comments/read-only-caret";
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
  readonly at?: CaretAt;
  readonly blockId: string;
  readonly row: string;
}

export interface NodeEditing {
  /** The row holding the editor, or `undefined` while the reader is editing nothing. */
  readonly editing: EditedRow | undefined;
  /** Puts the caret in one row's node, in the document that node's text lives in. */
  readonly edit: (row: string, node: TreeNode, at?: CaretAt) => void;
  /** Takes the caret out again and gives the row the focus back. */
  readonly stopEditing: () => void;
}

/**
 * What the tree and Graph's move keys (`graph-edit/move-keys.ts`, INFRA-5705's R4) hand each other
 * through a row's editing: `take`, Graph's, takes a move key pressed with the caret in the node's
 * block of the edited row `row` and answers whether it took it (`node-caret.ts`'s `confineKey`),
 * and so Enter and Shift+Enter with the caret at the end of that block (R3, `EndKey`);
 * `follow`, the editing's own, moves the editing of row `from` onto row `to`, the same node drawn
 * by a move under another row's key, with the document's one editor, and puts the caret back where
 * the reader had it in the node's block; `restore`, the editing's own too, puts that caret back in
 * the edited row with the focus, where a move's drawing dropped them. Graph's too
 * (`graph-edit/new-row-editing.ts`, R3): `fresh`, whether `row` is a new row, whose block the
 * document may not hold yet - its caret goes in the editor with none drawn, the box shut, and once
 * the block is drawn where the reader's typing left it - and `write`, which writes that block with
 * the reader's first character `words` in the editor `view`, and answers whether it did. And
 * Graph's (R5): `caretIn`, told as the caret goes into the block `blockId` of the edited row
 * `row`, the editor's hold on, with the editor's `view`, which a retype the node menu asked for
 * writes in. Outside Graph nothing sets `take`, `fresh`, `write` or `caretIn`.
 */
export interface MoveKeyBridge {
  take?: (row: string, move: MoveKey | EndKey) => boolean;
  follow?: (from: string, to: string) => void;
  restore?: () => void;
  fresh?: (row: string) => boolean;
  write?: (row: string, words: string, view: EditorView) => boolean;
  caretIn?: (row: string, blockId: string, view: EditorView) => void;
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
 * its node's place in one of the issue's documents, and `admissions`, what each document's room
 * last admitted the page to (`document-admission.tsx`). That place is Enter's own rule
 * (`documentOfNode`): a block of one of the issue's documents, which Enter puts the caret in
 * (`useNodeEditing`'s `edit`), a statement and a check among them, takes typing on an issue that
 * is open, unless its document's room admitted the page read-only, as the server does a page whose
 * schema it cannot trust with writes; before any room of that document has answered, Enter's rule
 * alone decides. A closed issue's documents take comments by quote and no edit (`ProofDocument`'s
 * `editorAccess`), so none of its blocks takes typing, though Enter still opens the editor
 * there. Every other node has no place - the issue, a child issue, an ask, a comment, a message,
 * a result, a block of another issue's document such as one above it - and takes no caret at all.
 */
export function nodeEditable(
  place: NodeDocument | undefined,
  isClosed: boolean,
  admissions: DocumentAdmissions
): boolean {
  return !isClosed && place !== undefined && admissions.get(place.artifact.id) !== true;
}

/** What takes a pointer's press inside a row's words as its own rather than the words': each
 *  control a drawn text can hold - a link in the words, a code block's copy control, the link of
 *  a parent line - a disclosure's summary, which a drawn diagram carries for its source, a label,
 *  and an element that plays a button. It is `RowText.tsx`'s `FOCUSABLE` with `summary`, `label`
 *  and `[role="button"]` added, which take a press of their own, and without its `[tabindex]`:
 *  `RowText` gives each element of the words that scrolls, a wide code block or a table's wrapper,
 *  a tab index of its own (`useOutOfTabOrder`), and such an element acts on no press, so a press
 *  on its text is the words'. */
const OWN_PRESS =
  'a[href], area[href], button, input, select, textarea, iframe, summary, label, [role="button"]';

/** Whether a pointer's click on a row's words asks for the caret in them, as Enter on the row
 *  does, given the document the row's text is being edited in while it is the row edited
 *  (`editing`). While the row is edited no click on its words asks: the editor is drawn in their
 *  place, and its own presses never reach the words' handler, since React sends a portal's events
 *  up the portal's own parents, so a click the handler hears then is on what stands beside the
 *  editor. A click on a control inside the element that heard it (`OWN_PRESS`) is that control's
 *  own. A click that ends a selection the reader made - a drag over the words, which leaves the
 *  window's selection not collapsed - asks for nothing, nor does the second click of a double
 *  click or a later one, which selects a word or a line: the editor drawn in the words' place
 *  would drop that selection and put the caret at the start. The Verification and Decisions views
 *  read it, and ask for the caret where the row takes one (`useWordsPress`). */
export function pressAsksCaret(
  event: MouseEvent<HTMLElement>,
  editing: string | undefined
): boolean {
  if (editing !== undefined || event.detail > 1 || window.getSelection()?.isCollapsed === false) {
    return false;
  }
  const owner = event.target instanceof Element ? event.target.closest(OWN_PRESS) : null;
  return owner === null || !event.currentTarget.contains(owner);
}

/** A pointer's press on a row's words, as the Verification and Decisions views take it once it
 *  asks for the caret (`pressAsksCaret`): the caret asked for in the node of the row with this key
 *  (`edit`, `useNodeEditing`'s), as Enter asks for it, and what a key reported before let go
 *  (`onReport`, with no line), but only where the row takes a caret, by the outline's one rule
 *  (`room-target.ts`'s `takesCaret`): over the room of the issue the page's writer is of, read as
 *  Enter reads it (`useNodeEditing`), and what each document's room admitted the page to
 *  (`document-admission.tsx`), given each artifact of the issue by its slug (`artifacts`) and
 *  whether the issue is closed (`isClosed`). A row that takes none keeps the focus the press gave
 *  it, and nothing is said: Enter is what says why. */
export function useWordsPress({
  artifacts,
  edit,
  isClosed,
  onReport,
}: {
  artifacts: ReadonlyMap<string, Artifact>;
  edit: NodeEditing["edit"];
  isClosed: boolean;
  onReport: (report: string) => void;
}): (row: string, node: TreeNode) => void {
  const room = useRoomFacts(useOutlineWriter()?.issueKey);
  const admissions = useDocumentAdmissions();
  return (row, node) => {
    if (!takesCaret(node, artifacts, room, isClosed, admissions)) return;
    onReport("");
    edit(row, node);
  };
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
 * left the focus on the row is one for the statement, and the caret goes in. Unless the act that
 * took the focus out was a press outside the tree: such a press keeps the editing, as it does
 * once the caret is placed (below), and puts off the caret - none goes in while the reader is out
 * there, and the focus back in the row or its editor's box puts it in the node's block, as Enter
 * does. That act is the reader's latest press, or a key pressed after that press had ended: a key
 * pressed while a press is held is part of the press, and a key after it - Tab out, `j` - or a
 * press in the tree moves on from the row.
 * A press starts at the pressed text without moving the page; Enter starts at the block's start.
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
 * what it is built of did not load, or the page handed the row no editor - the editing ends, and
 * the row says its editor could not open (`KEY_REPORTS.editor_closed`) where the focus is the
 * row's or inside the tree. With the focus out of the tree - moved there by a script before the
 * reader acted, or by a press outside the tree that put off the caret (above) - the row says
 * nothing: the tree lets go of the row, and of what was reported at it, as the editing ends
 * (`IssueTree.tsx`'s `useReleaseFocus`), so a report said then would stand at the tree's tab stop
 * for one commit and go. While the caret is put off by a press outside the tree, such a hold ends
 * the editing at once too, with no focus taken from where the reader is - Details, or the page's
 * body where that press left the focus on nothing. At once is once no press is under way, a press
 * held outside the tree included, since the editing ends with no caret placed only once a press
 * has ended, so nothing moves under the pointer (above). Seen while a press in the tree that ends
 * the editing (below) is held, such a hold ends the editing as that press does, once it is over:
 * the row the press focused says nothing of an editor it never asked for. A node whose text no
 * document of this issue holds has nothing to put a caret in, and the key says so. Enter on the
 * row while it is edited, the focus back on the row itself, puts the caret in the node's block
 * again, as the first Enter did, once that hold is on; before then the first Enter's caret is
 * still to come.
 *
 * A press that went down in the tree outside the editor's box ends the editing once it has ended,
 * as Tab out of the box does, unless the focus is back in that box by then, and leaves the focus
 * where the press put it (below); a press in the box is the editor's own - the action bar the
 * editor draws for its selection stands in that box too - and so is a press on a control that
 * keeps the focus where it was, in the editor, as the fold control does. A press that went down
 * outside the tree ends nothing, before the caret is placed as after it: Details draws the
 * document's threads, and the composer a reader opens on their selection, only while the page
 * draws the document's editor, and a press anywhere else outside the tree leaves the editing as
 * it is too. The caret keeps its place in the node's block while the reader is out there, and is
 * back at that place when the focus is given back to the editor - by Details' Close - wherever
 * the page's selection went meanwhile; a caret not placed yet goes in as Enter puts it, once the
 * focus is back in the row or its editor's box (above). The focus entering another row of the
 * tree with no press - an address's landing, any script's focus - ends the editing.
 *
 * Escape leaves the caret for the row it came from. The editor calls `preventDefault` on the
 * key before it reaches the page's keyboard registry, which drops every binding of the press
 * (`features/shell/keymap.ts`'s `handleKeyDown`), so the row listens for it in the capture
 * phase while it is being edited in - before the editor sees it - for an Escape pressed inside
 * the row, its editor's box included. Any other Escape in the tree is left to the registry: on
 * another row it leaves the outline, and on a link or a control `f` stopped on it gives that
 * control's row back (`useTreeKeymap`'s `tree-leave`). In a Change field nothing answers it: the
 * registry offers `tree-leave` nowhere one types, `tree-leave-caret` answers in a row's editor
 * alone, and the field has no key of its own, so the focus and the words stay in the field; Tab
 * to its Send change or Cancel, then Escape, gives the row back. The registry's own binding for
 * the key in a row's editor (`tree-leave-caret`) names it in the key table and is offered only
 * in an editable inside the tree, where this listener has taken the key first. The editor then
 * asserts its own selection again a moment after the blur (`prosemirror-view`'s focus handler)
 * and its stage moves off the row in the same commit, so the row takes the focus once more on
 * the next frame as well as the moment Escape was pressed, where the focus has fallen to nothing
 * by then and the reader has not acted since: a key or a press after Escape, or a focus something
 * else took, is where the focus stays.
 */
export function useNodeEditing({
  artifacts,
  container,
  moveKeys,
  onEditing,
  onOwnEdit,
  onReport,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The tree's own box, which the rows are found inside. */
  container: RefObject<HTMLElement | null>;
  /** Graph's move keys and the editing's way of following a node they moved (`MoveKeyBridge`),
   *  where the tree gives them. */
  moveKeys?: MoveKeyBridge;
  /** Tells the page which document a row is editing, so it draws that document's one editor and
   *  hands it to the row. */
  onEditing: OnEditing;
  /** Called at each edit of the row's editor's own that changes the document - the reader's
   *  typing in a heading as in a paragraph, never a collaborator's (`holdEditsToBlock`'s
   *  `onOwnEdit`) - with the key of the row being edited and the row's block as the edit left
   *  it, so the tree reads a read that drops the row while its note of the edit lasts as the
   *  reader's (`IssueTree.tsx`'s `ownAct`). A list that keeps no such note - the Spec filter's
   *  editing of a statement in place (`spec-filter/SpecFilterBody.tsx`) - passes none. */
  onOwnEdit?: (row: string, block: DisplayTree) => void;
  /** What a key reports when the node it was pressed on has no text to put a caret in, or its
   *  editor could not open. */
  onReport: (report: string) => void;
}): NodeEditing {
  const [editing, setEditing] = useState<EditedRow | undefined>(undefined);
  // The row the reader has just left the caret in, so that row takes the focus back.
  const leaving = useRef<string | undefined>(undefined);
  // The room of the issue's tree read, read through a ref so `edit` keeps its dependencies, by
  // the issue the page's writer is of (`OutlineWriter.issueKey`); with no writer there is none,
  // and a row is edited as it is without a room.
  const room = useRoomFacts(useOutlineWriter()?.issueKey);
  const roomNow = useRef(room);
  roomNow.current = room;
  // Graph's move keys, read as a key is pressed (`MoveKeyBridge`). The block a move key of this
  // editing was taken in, whose caret the row puts back where a change takes it out of the block
  // (`putKeptCaret`) rather than where Enter puts it; and the row a move drew the node at anew,
  // whose caret comes back as it is placed there (`follow`). Enter on a row, and the editing's
  // end, put the caret where they always do.
  const bridge = useRef(moveKeys);
  bridge.current = moveKeys;
  const keyMoved = useRef<string | undefined>(undefined);
  const followed = useRef<string | undefined>(undefined);
  if (editing === undefined) keyMoved.current = undefined;
  if (moveKeys !== undefined) {
    moveKeys.follow = (from, to) => {
      followed.current = to;
      setEditing((held) => (held?.row === from ? { ...held, row: to } : held));
    };
    moveKeys.restore = () => {
      const selector = editing === undefined ? undefined : rowSelectorOf(editing.row);
      const row =
        selector === undefined ? null : container.current?.querySelector<HTMLElement>(selector);
      if (hold.current !== "on" || row === null || row === undefined) return;
      putKeptCaret(row, editing?.blockId, editorView.current);
    };
  }

  const edit = useCallback(
    (row: string, node: TreeNode, at?: CaretAt) => {
      keyMoved.current = undefined;
      // An issue whose tree read the cache holds is edited in its room: a node of another issue,
      // and on a converted issue the conversation, take no caret there.
      const facts = roomNow.current;
      if (facts !== undefined) {
        const target = roomTarget(node, artifacts, facts);
        if ("report" in target) {
          onReport(KEY_REPORTS[target.report]);
          return;
        }
        // Enter on the row already edited before the editor's hold is on leaves the first Enter's
        // look for the caret under way, as it does on an issue with no room (below).
        if (editing?.row === row && hold.current !== "on") return;
        setEditing({ artifactId: target.artifactId, at, blockId: target.blockId, row });
        return;
      }
      const place = documentOfNode(node.id, artifacts);
      if (place === undefined) {
        // The row of a node read-only in the room - an ask, a comment, a message, a lens, a
        // finding, a risk, a constraint or a result - takes a caret that moves and selects in its
        // text and types nothing (R6, `read-only-caret.ts`); a row that draws none of its text
        // still has nothing to put a caret in. Enter is taken on the row itself
        // (`useTreeKeymap`'s `tree-edit`), so the row is the element that holds the focus.
        const focused = document.activeElement;
        const drawn = focused?.matches(rowSelectorOf(row)) === true ? focused : null;
        if (caretOnEnter(drawn, node.type)) return;
        onReport(KEY_REPORTS.no_text);
        return;
      }
      // Enter on the row already edited before the editor's hold is on leaves the first Enter's
      // look for the caret under way.
      if (editing?.row === row && hold.current !== "on") return;
      setEditing({ artifactId: place.artifact.id, at, blockId: place.blockId, row });
    },
    [artifacts, editing, onReport]
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
  // The row each edit of the editor's own is reported for, and what hears of it (`onOwnEdit`),
  // read as each edit is made.
  const ownEdit = useRef({ onOwnEdit, row: editingRow });
  ownEdit.current = { onOwnEdit, row: editingRow };
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
      },
      (block) => {
        const { onOwnEdit: heard, row } = ownEdit.current;
        if (row !== undefined) heard?.(row, block);
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

  // Each `editing` is one ask for the caret, which this effect runs for: Enter on a row, and a
  // new `editing` for the same row that places the caret again - for Enter on the row already
  // edited once the editor's hold is on (`edit`), and for the focus back in the row after a press
  // outside the tree put off its caret (below).
  useEffect(() => {
    const box = container.current;
    if (editing === undefined || box === null) return;
    const { blockId, row: rowKey } = editing;
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
    // the next row, Tab out of the outline, a press elsewhere in the tree, or a control inside the
    // row itself, as `Shift+C` focuses the change's own box and `f` the row's links. A caret
    // placed after that took the focus back from where they had put it, and the letters they
    // typed next went into the statement, which is saved to the shared document. So the first
    // look for the caret after that places none and ends the editing, and no row takes the focus
    // back, as the row does after Escape (`leaving`); where the focus is out of the tree as well,
    // the tree lets go of the row as the editing ends (`IssueTree.tsx`'s `useReleaseFocus`). A key
    // that leaves the focus on the row itself - Delete pressed the moment Enter is - is one for the
    // statement, and the caret goes in all the same, unless a later act takes the focus off the
    // row first. So does a focus a script moves off the row before the reader has acted onto
    // anything but another row - a control out of the outline, or one inside the row - since
    // only their own act ends the page's placing of the focus. A focus that enters another row
    // of the tree ends the editing whoever moved it, a script included, as a press there does
    // (the press rule's `focusin` listener on the tree, below), so no caret goes in after it.
    //
    // A press that went down outside the tree and took the focus out of the row - to Details, to
    // the Keys control, to nothing - ends nothing, as it does once the caret is placed (the press
    // rule, below): Details draws the document's threads only while the page draws the document's
    // editor. It puts off the caret instead: the look for it places no caret, so nothing takes
    // the focus back from where the press put it, and the editor's box stays in the row. A key
    // the reader presses while that press is held is part of it, and the caret is put off all the
    // same once the press has ended; a key after that is an act of its own, as above. The focus
    // back in the row or its editor's box - Details' Close gives it back, or the reader focuses
    // the row or presses in the box - asks for the caret again, as Enter on the edited row does
    // (`edit`): a new `editing` for the row, which this effect runs for afresh, counting the
    // reader's acts from then, and which puts the caret where Enter puts it. A press in the tree
    // outside the box brings no caret back: it ends the editing (the press rule). Where the
    // editor's hold cannot go on while the caret is put off - what it is built of did not load,
    // or the page handed the row no editor - the editing ends at once, as it does with no caret
    // put off (`endUnplaced`): once no press is under way, as there, a press held outside the
    // tree included. No focus is taken from where the reader is, the page's body included, where
    // the row taking the focus would scroll the page to it. As wherever such a hold ends the
    // editing, the row says nothing unless the focus is inside the tree: with the focus out of it,
    // the tree lets go of the row, and of what was reported at it, as the editing ends
    // (`IssueTree.tsx`'s `useReleaseFocus`), so a report said then would stand at the tree's tab
    // stop for one commit and go. Where a press in the tree that ends the editing (the press rule)
    // is held as that is seen, the editing ends as that press ends it once it is over - the reader
    // moved on - and the row it focused says nothing either. So the editor's drawing is watched on
    // while the caret is put off, until the hold is on, for an editor never handed over that draws
    // the node's block.
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
    // not name - goes back to the block's nearer end (`selectionIntoBlock`), and a focus that
    // comes back to the editor from outside it comes back at the caret the editor kept (below).
    const rowSelector = rowSelectorOf(rowKey);
    let settled: (() => void) | undefined;
    // The row is looked at apart from the editor's drawing as well, until the caret is placed:
    // no draw need follow a move off the row to look again - an editor that never draws the
    // node's block draws nothing more. So it is looked at a task after the reader's first act,
    // and a task after each focus move off it (a `focusout` on the row): that first act may leave
    // the focus on the row - Shift, Delete - and a later one take it off - Tab, a press - and a
    // tap moves the focus only at the `mousedown` that follows its `touchend`. A press, from its
    // `pointerdown` or its `mousedown`, is looked after once it has ended (`trackPress`), a task
    // after its click, as the tree lets go of a row a press left (`useReleaseFocus`), and the
    // editing does not end while it is held: until the caret is placed the editor's box stands
    // in the row as tall as what the editor has drawn, and the editing ending a task after the
    // press went down took that box out of the row with the pointer still down - the controls
    // below the row rose under it, and the click landed on something else.
    const looking = new AbortController();
    const press = trackPress({ onEnd: () => changed(), signal: looking.signal });
    // What the reader's latest press went down on, read as it starts, or `undefined` from a key
    // of theirs pressed with no press under way: the act that took the focus off the row, or
    // brought it back (above). A key pressed while a press is held - a key ends a press
    // (`trackPress`) - leaves it as it is, since that press, whose look is still to come, is what
    // took the focus where it is.
    let pressedOn: Node | null | undefined;
    const pressed = (event: Event) => {
      pressedOn = event.target instanceof Node ? event.target : null;
      press.start();
    };
    const lookOptions = { capture: true, signal: looking.signal };
    window.addEventListener("pointerdown", pressed, lookOptions);
    window.addEventListener("mousedown", pressed, lookOptions);
    window.addEventListener(
      "keydown",
      () => {
        if (!press.pressing) pressedOn = undefined;
      },
      lookOptions
    );
    let look: number | undefined;
    const lookSoon = () => {
      if (press.pressing) return;
      window.clearTimeout(look);
      // A press that starts before this look comes is looked after as it ends instead.
      look = window.setTimeout(() => {
        if (!press.pressing) changed();
      }, 0);
    };
    const movedOff = (event: FocusEvent) => {
      const row = box.querySelector(rowSelector);
      if (row !== null && event.target instanceof Node && row.contains(event.target)) lookSoon();
    };
    box.addEventListener("focusout", movedOff, lookOptions);
    const acts = abortOnReaderAct(() => {
      if (settled !== undefined) watch.disconnect();
      else lookSoon();
    });
    const confining = new AbortController();
    const confine = () => {
      if (!acts.signal.aborted) return;
      const row = box.querySelector<HTMLElement>(rowSelector);
      // After a move key, a caret a change of the room took out of the block goes back where the
      // reader had it, not to the block's nearer end (`keyMoved`).
      if (
        row !== null &&
        keyMoved.current === blockId &&
        caretOutOfBlock(row, blockId) &&
        putKeptCaret(row, blockId, editorView.current)
      ) {
        return;
      }
      if (row !== null) selectionIntoBlock(row, blockId);
    };
    // The focus coming back into the editor from outside it - Details' Close gives back the
    // focus its opening took (`margin/Margin.tsx`), as a dialog closing does - finds the page's
    // selection wherever the reader left it out there: a press on Details' words put it in those
    // words. A browser puts a selection that stands outside an editable at the editable's start
    // as a script focuses it, which ProseMirror reads as that reset and answers with its own
    // selection (`prosemirror-view`'s `DOMObserver.flush`); but ProseMirror listens again each
    // time it writes its selection to the page, so the backstop above heard the reset first and
    // took it to the block's start, which the editor then read as its own: driven on a preview,
    // a press on Details' heading and then its Close put the words typed next at the start of
    // the statement, not at the caret six letters in. The editor keeps its own selection while
    // it has no focus - it reads no move of the page's then - inside the node's block, where its
    // hold keeps it (`holdEditsToBlock`). So that selection goes on the page as the editable
    // takes the focus, before the browser's reset (`EditorView.focus`, which moves no focus where
    // the editable holds it already), and a browser leaves a selection already inside the
    // editable it focuses where it is. A press in the editor puts its own caret after this, once
    // it has moved the focus, and the backstop holds that one to the block as before.
    const cameBack = (event: FocusEvent) => {
      const view = editorView.current;
      if (
        hold.current === "on" &&
        view !== undefined &&
        event.target === view.dom &&
        view.hasFocus()
      ) {
        view.focus();
      }
    };
    // Whether the editing has ended here, with no caret placed.
    let over = false;
    // Whether a press outside the tree has put off the caret (above): no caret goes in, until
    // the focus is back in the row or its editor's box, and the editing ends where the editor's
    // hold cannot go on.
    let putOff = false;
    // Whether the page has handed the row no editor to hold, read a task after the node's block
    // is first seen drawn with none handed over. A page hands its editor over in the task the
    // editor is made in (`doc/ProofDocument.tsx`'s `onEditorHandle`), and an editor made for a
    // document the room already holds has drawn it by then - y-prosemirror draws as its plugin
    // goes on - some microtasks before it is handed over, where a look at the row can fall.
    let unhanded = false;
    let handing: number | undefined;
    // Ends the editing with no caret placed - the reader moved on from the row first, or the
    // editor's hold on its edits cannot go on - and answers whether it did: a press ends it once
    // it has ended (`press`), so nothing moves under it. `rowsFocus` is whether the focus is the
    // row's (`holdsRowsFocus`), as the look that ends it read it (`place`).
    const endUnplaced = (row: HTMLElement, rowsFocus: boolean, movedOn: boolean): boolean => {
      if (press.pressing) return false;
      over = true;
      if (movedOn) {
        setEditing(undefined);
        return true;
      }
      // The row says its editor could not open where the focus is the row's or inside the tree:
      // a focus the reader's press put in the editor's box goes back to the row first, as
      // Escape's does, since that box goes, and the row takes it again on the next frame where
      // it has fallen to nothing by then (`leaving`). With the focus out of the tree - moved
      // there by a script before the reader acted, or by a press outside the tree that put off
      // the caret - it says nothing: the tree lets go of the row, and of what was reported at
      // it, as the editing ends (`IssueTree.tsx`'s `useReleaseFocus`), and the report stood at
      // the tree's tab stop for one commit and was gone. With the caret put off and the focus
      // elsewhere - in Details, on the page's body - the reader is out of the row, and the
      // editing's end takes no focus: the row taking it from the page's body moved the reader
      // off where they were, and scrolled the page to it.
      if (rowsFocus) row.focus();
      if (rowsFocus || box.contains(document.activeElement)) {
        report.current(KEY_REPORTS.editor_closed);
      }
      if (putOff && !rowsFocus) setEditing(undefined);
      else stopEditing();
      return true;
    };
    // Places the caret, or places it again where the focus has fallen to nothing or the caret
    // has left the block, and answers whether the row is done with: the caret placed and the
    // reader acted since, the caret put off by a press outside the tree with the editor's hold
    // on, or the editing ended with none placed - the reader moved on from the row first, or the
    // editor's hold on its edits cannot go on.
    const place = (): boolean => {
      const row = box.querySelector<HTMLElement>(rowSelector);
      if (over) return true;
      if (row === null) return false;
      if (settled !== undefined) {
        // An editor made again in the row's place is no editor a caret goes in until its own
        // hold is on.
        // After a move key, the caret goes back where the reader had it (`keyMoved`).
        if (
          hold.current === "on" &&
          keyMoved.current === blockId &&
          (focusOnDocument() || caretOutOfBlock(row, blockId)) &&
          putKeptCaret(row, blockId, editorView.current)
        ) {
          return acts.signal.aborted;
        }
        if (hold.current === "on" && (focusOnDocument() || caretOutOfBlock(row, blockId))) {
          caretInBlock(row, blockId, editing.at);
        }
        return acts.signal.aborted;
      }
      // Whether the focus is the row's, read once for this look: nothing in it moves the focus
      // before the row takes it back as the editing ends (`endUnplaced`).
      const rowsFocus = holdsRowsFocus(row);
      // The reader has acted and the focus is off the row: a press outside the tree puts off
      // the caret, once it has ended (above), and any other act moves on from the row.
      const offRow = acts.signal.aborted && !rowsFocus;
      if (!putOff && offRow && pressedOn !== undefined && !box.contains(pressedOn)) {
        if (press.pressing) return false;
        putOff = true;
        // Each focus move into the tree is looked at, a task on or once a press has ended.
        box.addEventListener("focusin", lookSoon, lookOptions);
      }
      if (hold.current === "awaited" && handing === undefined && drawsBlock(row, blockId)) {
        handing = window.setTimeout(() => {
          // An editor made for a connection the room then refused draws the block and goes
          // unhanded, and the editor for the next connection comes after a sync: where the
          // block is no longer drawn, the wait starts again with the next editor that draws it.
          const now = box.querySelector<HTMLElement>(rowSelector);
          unhanded = hold.current === "awaited" && now !== null && drawsBlock(now, blockId);
          if (unhanded) changed();
          else handing = undefined;
        }, 0);
      }
      // The hold cannot go on where what it is built of did not load, or where the page has
      // handed the row no editor to hold.
      const unheld = load.current?.failed === true || (unhanded && hold.current === "awaited");
      if (putOff) {
        // A hold that cannot go on ends the editing at once, as with no caret put off (below):
        // as the reader moving on from the row where the press that is ending ends the editing
        // by the press rule's own test - one in the tree outside the box (`pressEndsEditing`) -
        // and else as a hold that cannot go on, which the row says.
        if (unheld) {
          return endUnplaced(
            row,
            rowsFocus,
            pressedOn !== undefined && pressEndsEditing(box, row, pressedOn)
          );
        }
        // The focus back in the row or its editor's box asks for the caret again, unless a press
        // that ends the editing brought it there: one in the tree outside the box, by the press
        // rule's own test (`pressEndsEditing`).
        if (rowsFocus && (pressedOn === undefined || !pressEndsEditing(box, row, pressedOn))) {
          setEditing((held) => (held === editing ? { ...held } : held));
        }
        // Until the hold is on, the editor's drawing is watched on, so an editor never handed
        // over is seen as it draws the node's block (above).
        return hold.current === "on";
      }
      if (offRow || unheld) return endUnplaced(row, rowsFocus, offRow);
      // A new row of Graph's (`MoveKeyBridge.fresh`, INFRA-5705's R3) whose block the editor does
      // not draw yet takes the caret with none - the box shut, the editable focused once the hold
      // is on - and is looked at again as the reader's first character draws the block; then the
      // caret stays where their typing left it, in the block, rather than at its start.
      const fresh = bridge.current?.fresh?.(rowKey) === true;
      if (fresh && !drawsBlock(row, blockId)) {
        caretForNewBlock(row, hold.current === "on");
        return false;
      }
      // No caret goes in an editor that does not hold its edits to the node's block yet: the
      // focus stays on the row, where no key edits the document, and the row is looked at
      // again as the hold goes on (`holdChanged`).
      if (hold.current !== "on") return false;
      const inNewBlock =
        fresh &&
        document.activeElement?.getAttribute("contenteditable") === "true" &&
        row.contains(document.activeElement) &&
        !caretOutOfBlock(row, blockId);
      if (
        !(inNewBlock || (fresh && putKeptCaret(row, blockId, editorView.current))) &&
        !caretInBlock(row, blockId, editing.at)
      ) {
        return false;
      }
      settled = holdEditorAtBlock(row, blockId, editing.at);
      // The row a move drew the node at anew (`follow`): the caret goes back where the reader had
      // it in the node's block, not to the block's start where Enter puts it (R4).
      if (followed.current === rowKey) {
        followed.current = undefined;
        putKeptCaret(row, blockId, editorView.current);
      }
      window.clearTimeout(look);
      looking.abort();
      // Only a selection moved after the caret was placed is the editor's doing.
      document.addEventListener("selectionchange", changed, { signal: acts.signal });
      document.addEventListener("selectionchange", confine, { signal: confining.signal });
      box.addEventListener("focusin", cameBack, { capture: true, signal: confining.signal });
      // Graph's retype waits for this caret, and writes in the editor that holds it (R5).
      const view = editorView.current;
      if (view !== undefined) bridge.current?.caretIn?.(rowKey, blockId, view);
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
  }, [container, editing, stopEditing]);

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
          // After a move key, the caret the reader had in the node's block (`keyMoved`).
          if (
            keyMoved.current === editingBlock &&
            putKeptCaret(row, editingBlock, editorView.current)
          ) {
            return;
          }
          // A new row with no block drawn yet takes the focus back in its editor (R3).
          if (!caretInBlock(row, editingBlock) && bridge.current?.fresh?.(editingRow) === true) {
            caretForNewBlock(row, true);
          }
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
        // Graph's move keys, where the tree gives them (`MoveKeyBridge`): a key taken in the
        // node's block marks the block, so the caret comes back where the reader had it.
        const take = bridge.current?.take;
        const onMove =
          take === undefined
            ? undefined
            : (move: MoveKey | EndKey) => {
                const taken = take(editingRow, move);
                if (taken) keyMoved.current = editingBlock;
                return taken;
              };
        // A new row's first character writes its block, where Graph gives the way (R3).
        const write = bridge.current?.write;
        const view = editorView.current;
        const onFirst =
          write === undefined || view === undefined || hold.current !== "on"
            ? undefined
            : (words: string) => write(editingRow, words, view);
        confineKey(
          row,
          hold.current === "on" ? editingBlock : undefined,
          event,
          onMove,
          view,
          onFirst
        );
      },
      { capture: true, signal: listening.signal }
    );
    box.addEventListener(
      "keydown",
      (event) => {
        // A Tab pressed in an input method's composition is the input method's, and moves no
        // focus and ends no editing (`inputMethodKey`).
        if (
          event.key !== "Tab" ||
          event.defaultPrevented ||
          event.altKey ||
          event.ctrlKey ||
          event.metaKey ||
          inputMethodKey(event) ||
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
        else if (inEditorBox(row, to)) return;
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
    // A press that goes down in the tree outside the editor's box, and leaves the focus outside
    // that box, ends the editing, as Tab out of the box does (above): the reader has gone on from
    // the node's text - to another row, a control of one, the tree's blank space, or the row's own
    // words and controls around the box - and left the row edited, where a later Escape there was
    // taken for the edited row and gave it the focus back, and Enter on the row found it edited
    // already and put no caret in. It ends once the press has ended (`trackPress`), a task after
    // its click, as the tree lets go of a row a press left (`IssueTree.tsx`'s `useReleaseFocus`),
    // so the box leaving the row moves nothing under the pointer while it is down; and with the
    // focus wherever the press put it - on another row, on a control, on nothing - since the
    // editing's end takes none. A press is told by what it went down on, read at its `mousedown`,
    // the event whose default moves the focus, which a tap sends as well and a swipe that scrolls
    // the page does not, and by where the focus is once it has ended. The fold control, Let it
    // close and the row's other controls prevent their `mousedown`'s default, so a press on one
    // leaves the focus where it was: where that is the editor, the editing goes on, since ending
    // it would take the editor's box off the page with the focus in it, which the browser drops
    // to the page's body. A press in the box is the editor's own, the action bar the editor draws
    // for its selection included: the editor puts the bar beside its editable, inside the box
    // (`node-caret.ts`'s `ACTION_BAR`).
    //
    // A press that goes down outside the tree ends nothing. Details draws the document's threads,
    // and the composer a reader opens on their selection, only while the page draws the document's
    // editor (the margin's `documentBridge`): a press there that ended the editing took the
    // composer and the thread the reader pressed off the page under the pointer. The tree keeps
    // the node's facts in Details as the focus goes there, too (`useReleaseFocus`). A press
    // anywhere else outside the tree - the header, a view's control - leaves the editing as it is.
    // So does such a press before the caret is placed, which puts off the caret until the focus
    // is back in the row (the caret's look, above).
    //
    // The focus entering another row of the tree, outside the editor's box, ends the editing with
    // no press as well: an address's landing - a link to a node of this issue followed from the
    // editor's text, by a press that is the editor's own, whose row the landing focuses a frame
    // later - or any other script's focus. Left on, the tree held every row the focus came to as
    // the one being edited (`useReleaseFocus`): a Tab out of the tree kept that row's body and its
    // facts in Details. The focus a press moves there is the press's, whose end ends the editing,
    // as above.
    const rowSelector = rowSelectorOf(editingRow);
    const listening = new AbortController();
    // Whether the press under way, the latest, ends the editing once it is over: it went down in
    // the tree outside the editor's box, or the focus entered another row while it was held.
    let ends = false;
    const press = trackPress({
      onEnd: () => {
        if (!ends) return;
        const row = box.querySelector<HTMLElement>(rowSelector);
        if (row === null || !inEditorBox(row, document.activeElement)) setEditing(undefined);
      },
      signal: listening.signal,
    });
    window.addEventListener(
      "mousedown",
      (event) => {
        const on = event.target instanceof Node ? event.target : null;
        ends = pressEndsEditing(box, box.querySelector<HTMLElement>(rowSelector), on);
        press.start();
      },
      { capture: true, signal: listening.signal }
    );
    box.addEventListener(
      "focusin",
      (event) => {
        const to = event.target;
        if (!(to instanceof Element)) return;
        // A row of the tree: a node's, or a control's that draws more of it (`TreeRows.tsx`).
        const entered = to.closest('[role="treeitem"]');
        if (entered === null) return;
        const row = box.querySelector<HTMLElement>(rowSelector);
        if (entered === row || (row !== null && inEditorBox(row, to))) return;
        if (press.pressing) ends = true;
        else setEditing(undefined);
      },
      { signal: listening.signal }
    );
    return () => listening.abort();
  }, [container, editingRow]);

  useEffect(() => {
    const box = container.current;
    if (editingRow === undefined || box === null) return;
    const leave = (event: KeyboardEvent) => {
      // An Escape that ends an input method's composition is the input method's, and ends no
      // editing: it acts on nothing, as every key that ends a composition does
      // (`inputMethodKey`).
      if (event.key !== "Escape" || inputMethodKey(event) || event.defaultPrevented) return;
      // An Escape pressed anywhere else in the tree is the keymap's, as it is while no row is
      // edited: on another row it leaves the outline, on a link or a control `f` stopped on it
      // gives that control's row back (`useTreeKeymap`'s `tree-leave`), and in a Change field,
      // where the keymap offers neither binding of the tree's, nothing answers it.
      const row = box.querySelector<HTMLElement>(rowSelectorOf(editingRow));
      if (row === null || !(event.target instanceof Node) || !row.contains(event.target)) return;
      event.preventDefault();
      (document.activeElement as HTMLElement | null)?.blur();
      row.focus();
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

/** Whether `node` is inside the box the editor of `row` is drawn in (`EDITOR_BOX_ATTRIBUTE`),
 *  which holds the editor's editable and the action bar it draws for its selection beside that
 *  editable (`node-caret.ts`'s `ACTION_BAR`). */
function inEditorBox(row: HTMLElement, node: Node | null): boolean {
  return row.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`)?.contains(node) === true;
}

/** Whether a press that went down on `on` ends the editing of `row`'s node: it went down in the
 *  tree (`box`), outside the row's editor box, or with the row no longer drawn. The press rule
 *  reads it (`useNodeEditing`'s `mousedown` listener), and so does a caret put off (`place`). */
function pressEndsEditing(box: HTMLElement, row: HTMLElement | null, on: Node | null): boolean {
  return box.contains(on) && (row === null || !inEditorBox(row, on));
}

/** Whether the focus is still the edited row's: on the row itself, or inside the box its editor
 *  is drawn in (`EDITOR_BOX_ATTRIBUTE`). A press of the reader's in that box before the node's
 *  block is drawn is the row's, and the caret goes to that block once it is, since a key may
 *  edit the node's own block alone. A control inside the row - a change's own box, a link `f`
 *  stepped to - is somewhere the reader moved to. */
function holdsRowsFocus(row: HTMLElement): boolean {
  const active = document.activeElement;
  if (active === row) return true;
  return inEditorBox(row, active);
}

/** Whether `offset` in `node` is still a place in `editable` a caret can be put: the editor can
 *  redraw its text while it has no focus. */
function pointIn(editable: HTMLElement, node: Node, offset: number): boolean {
  const length = node instanceof CharacterData ? node.length : node.childNodes.length;
  return editable.contains(node) && offset <= length;
}
