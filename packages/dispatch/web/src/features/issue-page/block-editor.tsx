import type { EditorView } from "@milkdown/kit/prose/view";
import { createContext, type RefObject, useContext, useMemo } from "react";
import type { Awareness } from "y-protocols/awareness";
import type { AbstractType, Transaction, XmlElement, XmlFragment, YEvent } from "yjs";

import type { Artifact } from "../../api/types";
import { inputMethodKey } from "../../lib/input-method-key";
import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import type { EditorHandle } from "../doc/editor";
import { fragmentOf } from "../doc/roots";
import type { BindTarget, BlockRoom } from "./block-room";
import { EDITOR_BOX_ATTRIBUTE } from "./node-caret";
import { AUTHOR_TEXT_ATTRIBUTE, KEY_REPORTS } from "./page-structure";

/**
 * R7's block editor in a row (decisions 544, 547 and 551): the page's one kept editor
 * (`block-room.ts`), bound to the row's block with `rebindElement` and drawn in a box where the
 * row's words stood, in their size, line and ink (`styles.css`'s `[data-block-editor]`).
 *
 * `bindBlockEditor` moves the editor and binds it: into a row and to that row's block at a bind,
 * and back to the room's hidden box at Escape, at another row's bind, and when another writer
 * deletes the block or merges it into another, which it says in the page's words
 * (`KEY_REPORTS`). `useKeptBlockEditor` is how a row's editing (`useNodeEditing.ts`) asks the room
 * for that bind, and hands the caret to the row's own caret rule once the editor stands in the
 * row. A bind reads nothing: the block is the room's own `Y.XmlElement`, already synced.
 */

/** The mark of the box the block editor is drawn in, beside its `EDITOR_BOX_ATTRIBUTE`, which
 *  `styles.css` draws the editor's text by. */
const BLOCK_EDITOR_ATTRIBUTE = "data-block-editor";

/** Where a row draws its node's words (`NodeRow.tsx`, `spec-filter/SpecFilterBody.tsx`), which
 *  the editor's box stands in place of while the row's block is bound. */
const ROW_WORDS = "[data-node-words]";

/** The issue page's room (`block-room.ts`'s `IssueBlockRoom`, which provides it), as its rows
 *  reach the kept editor through it; undefined outside an issue page that keeps one. */
export const IssueBlockRoomContext = createContext<
  RefObject<BlockRoom<EditorHandle> | null> | undefined
>(undefined);

/** The issue page's room, or undefined outside an issue page that keeps one. */
export function useIssueBlockRoom(): RefObject<BlockRoom<EditorHandle> | null> | undefined {
  return useContext(IssueBlockRoomContext);
}

/** The kept editor as `bindBlockEditor` moves and binds it. */
export interface BlockEditorView<At> {
  /** The element moved between the hidden box and a row: the editor's own root, which holds its
   *  editable. */
  readonly view: { readonly dom: HTMLElement };
  /** Binds the editor to `element`, a block of the room, with that block's own type and
   *  attributes (`rebindElement`). */
  rebindElement(element: XmlElement): void;
  /** Puts the caret where the bind asked, once the editor stands in `row`. */
  setCaret?(at: At, row: HTMLElement): void;
  /** Starts a comment on the selection, as the comment chord does, and answers whether it did. */
  commentOnSelection?(): boolean;
  /** Takes back the block's last edit, for an editor whose own keys do not answer `Mod-z`. */
  undo?(): void;
  /** Draws a collaborator's cursor the room reports in the block, for an editor that does not
   *  read the room's awareness itself. */
  remoteCursor?(id: string): void;
  /** Lets the editor take typing, or stops it, as the room admits the page. */
  setEditable?(editable: boolean): void;
}

/** One row's bind: the row, its drawn words, its block in the room and where its caret goes. */
export interface BlockEditorTarget<At> {
  readonly row: HTMLElement;
  readonly words: HTMLElement;
  readonly element: XmlElement;
  readonly at: At;
  /** The early caret's box, still in the row after its editable steps aside. */
  readonly existingBox?: HTMLElement;
}

export interface BlockEditorOptions<At> {
  /** Where the editor stands while it is bound to no row: the room's hidden box. */
  readonly hidden: HTMLElement;
  readonly editor: BlockEditorView<At>;
  /** Says the line the page reports when the editor ends by itself (`KEY_REPORTS`). */
  readonly report: (line: string) => void;
  /** Where a bind would send a request. It sends none, since the block is the room's own: a
   *  caller passes one to watch that it stays unused (R7). */
  readonly request?: (url: string) => void;
}

export interface BlockEditorBinding<At> {
  /** Moves the editor into `target.row` where its words stood, bound to its block, and lets
   *  the row bound before have its words back. Answers what lets this bind go, which does
   *  nothing once another bind or the editor's end has. */
  bind(target: BlockEditorTarget<At>): () => void;
  /** Takes a key pressed in the editor: Escape ends the bind and gives the row the focus, and
   *  the comment chord and `Mod-z` run where the editor answers them outside its own keys.
   *  Answers whether a block is bound to take it: no key goes into a block once the bind ends. */
  keydown(event: KeyboardEvent): boolean;
  /** Notes text typed in the block that the room has not acknowledged yet. */
  noteUnacknowledgedEdit(text: string): void;
  /** The room has acknowledged every edit: no noted text is in flight. */
  noteAcknowledged(): void;
  /** What the room admits the page to now: a reconnect that admits it to write keeps the bound
   *  block writable, with nothing said; a read-only one stops its typing. */
  admissionChanged(readOnly: boolean): void;
  /** A collaborator's cursor the room reports in the bound block. */
  receiveCursor(id: string): void;
  destroy(): void;
}

/** A type of the room, as far as a bind reads it: the block it stands in and whether it is
 *  deleted. */
interface Placed {
  readonly _item: { readonly deleted: boolean } | null;
  readonly parent: Placed | null;
}

/** The bind in force. */
interface Bound {
  readonly box: HTMLElement;
  readonly element: XmlElement;
  readonly row: HTMLElement;
  /** The room's fragment the block stands in, read for typed text after the block is gone. */
  readonly room: Placed;
  /** Text typed in the block that the room has not acknowledged. */
  readonly unsaved: string[];
  readonly watch: () => void;
  readonly words: HTMLElement;
  readonly wordsHidden: boolean;
}

/**
 * Binds the kept editor to one row's block at a time. The editor binds the block while it still
 * stands in the hidden box, so it is drawn once, with the block's text, before it shows in the
 * row. Its box takes the words' place and their classes; the words stay where they are, hidden,
 * so whatever draws them keeps them, and they show again as the bind ends.
 *
 * Another writer deleting the bound block, or merging it into another and deleting it, ends the
 * bind: the editor goes back to the hidden box, the row takes the focus where the editor held
 * it, so no key typed after goes into another block, and the report says the block was removed,
 * or that typed text was not saved where the room holds a noted text nowhere now.
 */
export function bindBlockEditor<At>({
  hidden,
  editor,
  report,
}: BlockEditorOptions<At>): BlockEditorBinding<At> {
  let bound: Bound | undefined;
  let readOnly = false;
  let alive = true;

  const park = (held: Bound) => {
    held.element.doc?.off("afterTransaction", held.watch);
    if (bound === held) bound = undefined;
    hidden.append(editor.view.dom);
    held.box.remove();
    held.words.hidden = held.wordsHidden;
  };

  const end = (held: Bound) => {
    const room = held.room.toString();
    const lost = held.unsaved.some((text) => !room.includes(text));
    const focused = held.box.contains(held.row.ownerDocument.activeElement);
    park(held);
    if (focused) held.row.focus({ preventScroll: true });
    report(lost ? KEY_REPORTS.editor_text_not_saved : KEY_REPORTS.editor_block_removed);
  };

  return {
    bind({ row, words, element, at, existingBox }) {
      if (!alive) return () => {};
      const doc = element.doc;
      if (doc === null) throw new Error("A block editor binds a block of a room.");
      if (bound !== undefined) park(bound);
      editor.rebindElement(element);
      if (readOnly) {
        readOnly = false;
        editor.setEditable?.(true);
      }
      const box = existingBox ?? row.ownerDocument.createElement("div");
      box.className = words.className;
      box.setAttribute(EDITOR_BOX_ATTRIBUTE, "");
      box.setAttribute(BLOCK_EDITOR_ATTRIBUTE, "");
      box.setAttribute(AUTHOR_TEXT_ATTRIBUTE, "");
      if (existingBox === undefined) words.before(box);
      const wordsHidden = words.hidden;
      words.hidden = true;
      box.append(editor.view.dom);
      const held: Bound = {
        box,
        element,
        room: roomOf(element),
        row,
        unsaved: [],
        watch: () => {
          if (bound === held && removed(element)) end(held);
        },
        words,
        wordsHidden,
      };
      doc.on("afterTransaction", held.watch);
      bound = held;
      editor.setCaret?.(at, row);
      return () => {
        if (bound === held) park(held);
      };
    },
    keydown(event) {
      const held = bound;
      if (held === undefined) return false;
      if (inputMethodKey(event)) return true;
      const chord = (event.ctrlKey || event.metaKey) && !event.shiftKey;
      const key = event.key.toLowerCase();
      // A Mac's Alt+M types `µ`, so the chord is read by the key's place as well.
      const comment = chord && event.altKey && (event.code === "KeyM" || key === "m");
      const undo = chord && !event.altKey && key === "z";
      if (event.key === "Escape") {
        event.preventDefault();
        park(held);
        held.row.focus();
      } else if (comment) {
        if (editor.commentOnSelection?.() === true) event.preventDefault();
      } else if (undo && editor.undo !== undefined) {
        event.preventDefault();
        editor.undo();
      }
      return true;
    },
    noteUnacknowledgedEdit(text) {
      if (text !== "") bound?.unsaved.push(text);
    },
    noteAcknowledged() {
      bound?.unsaved.splice(0);
    },
    admissionChanged(next) {
      if (next === readOnly) return;
      readOnly = next;
      editor.setEditable?.(!next);
    },
    receiveCursor(id) {
      if (bound !== undefined) editor.remoteCursor?.(id);
    },
    destroy() {
      alive = false;
      if (bound !== undefined) park(bound);
    },
  };
}

/** Whether `element`, or a block it stands in, is deleted from its room. */
function removed(element: Placed): boolean {
  for (let type: Placed | null = element; type !== null; type = type.parent) {
    if (type._item?.deleted === true) return true;
  }
  return false;
}

/** The fragment of the room `element` stands in. */
function roomOf(element: Placed): Placed {
  let type = element;
  while (type.parent !== null) type = type.parent;
  return type;
}

/** One row's ask for the kept editor (`useKeptBlockEditor`). */
export interface RowBind {
  readonly artifact: Artifact;
  readonly blockId: string;
  /** The row's element, where the editor is drawn in place of its words. */
  readonly row: HTMLElement;
  /** Run once composition has ended; use the stand-in's box if it is still in the row. */
  readonly beforeBind: (bind: (box?: HTMLElement) => void) => void;
  /** The editor stands in the row, bound to the block: the row's own caret rule puts the caret
   *  in it (`useNodeEditing.ts`), and holds it there, as it does in the whole document's editor. */
  readonly onBound: (view: EditorView) => void;
  /** The bind will not come, or the editor ended by itself: the line the row says why with. */
  readonly onEnd: (line: string) => void;
}

/** The issue page's kept editor, as a row's editing binds it. */
export interface KeptBlockEditor {
  /** Whether the page holds a room to bind a row in now: an issue with a primary spec. */
  available(): boolean;
  /** Asks the room to bind `request.row` to its block: at once where the room is ready, else
   *  once it is, starting its work where it has not started. Answers what cancels the ask, or
   *  lets the bind go and gives the row its words back. */
  bind(request: RowBind): () => void;
}

/** The kept editor of the issue page this is drawn under (`block-room.ts`'s `IssueBlockRoom`),
 *  or undefined outside one, where a row is edited in its document's whole editor. */
export function useKeptBlockEditor(): KeptBlockEditor | undefined {
  const room = useIssueBlockRoom();
  return useMemo(() => (room === undefined ? undefined : keptBlockEditor(room)), [room]);
}

function keptBlockEditor(room: RefObject<BlockRoom<EditorHandle> | null>): KeptBlockEditor {
  return {
    available: () => room.current !== null,
    bind(request) {
      const held = room.current;
      if (held === null) {
        request.onEnd(KEY_REPORTS.editor_closed);
        return () => {};
      }
      let release: (() => void) | undefined;
      let cancelled = false;
      let stopWaiting: (() => void) | undefined;
      const cancel = held.requestBind(
        { id: request.artifact.id, fragment: fragmentOf(request.artifact) },
        {
          onBind: (target) => {
            // The room may readmit the page read-only while the stand-in is still composing.
            stopWaiting = target.watchAdmission((readOnly) => {
              if (!readOnly || cancelled) return;
              cancelled = true;
              stopWaiting?.();
              request.onEnd(KEY_REPORTS.editor_closed);
            });
            request.beforeBind((box) => {
              if (cancelled) return;
              stopWaiting?.();
              release = bindRow(target, request, box);
              if (release !== undefined) request.onBound(target.editor.view);
            });
          },
          onBindRefused: request.onEnd,
        }
      );
      return () => {
        // Park this row before asking the room to drain any later bind.
        cancelled = true;
        stopWaiting?.();
        release?.();
        cancel();
      };
    },
  };
}

/** The binding of one kept editor, which every view's rows share: the row bound now, which the
 *  editor's caret and its end are told to, and that row's room's awareness. */
interface KeptBinding {
  readonly binding: BlockEditorBinding<undefined>;
  awareness: Awareness | null;
  row: RowBind | undefined;
}

const KEPT_BINDINGS = new WeakMap<EditorHandle, KeptBinding>();

function keptBindingOf(target: BindTarget<EditorHandle>): KeptBinding {
  const known = KEPT_BINDINGS.get(target.editor);
  if (known !== undefined) return known;
  const handle = target.editor;
  const kept: KeptBinding = {
    awareness: null,
    row: undefined,
    binding: bindBlockEditor<undefined>({
      hidden: target.hidden,
      editor: {
        view: { dom: target.root },
        rebindElement(element) {
          handle.rebindElement(element, kept.awareness);
          // The editable is the block itself, which the row's caret finds by its id
          // (`node-caret.ts`'s `blockIn`): ProseMirror draws the root node into no element of its
          // own, and reads no attribute of its editable as an edit.
          handle.view.dom.setAttribute(BLOCK_ID_ATTRIBUTE, String(element.getAttribute("blockId")));
        },
        commentOnSelection: () => handle.commentOnSelection(),
        setEditable: (editable) => handle.setAccess(editable ? "edit" : "read"),
      },
      report: (line) => kept.row?.onEnd(line),
    }),
  };
  // After the editor's own keys, which take `Mod-z` and whatever else they answer first.
  target.root.addEventListener("keydown", (event) => {
    if (!event.defaultPrevented) kept.binding.keydown(event);
  });
  KEPT_BINDINGS.set(handle, kept);
  return kept;
}

/** Binds the kept editor to `request`'s block in `target`'s room, and answers what lets that
 *  bind go; undefined where it could not bind, which `request.onEnd` is told. */
function bindRow(
  target: BindTarget<EditorHandle>,
  request: RowBind,
  existingBox?: HTMLElement
): (() => void) | undefined {
  const element = blockOf(
    target.connection.doc.getXmlFragment(target.artifact.fragment),
    request.blockId
  );
  const words = request.row.isConnected ? request.row.querySelector<HTMLElement>(ROW_WORDS) : null;
  if (element === undefined || words === null) {
    request.onEnd(
      element === undefined ? KEY_REPORTS.editor_block_removed : KEY_REPORTS.editor_closed
    );
    return undefined;
  }
  const kept = keptBindingOf(target);
  kept.awareness = target.connection.awareness ?? null;
  kept.row = request;
  let unbind: () => void;
  try {
    unbind = kept.binding.bind({ row: request.row, words, element, at: undefined, existingBox });
  } catch (error) {
    // A block the editor's schema holds no block type for: `rebindElement` refuses it, and the
    // editor stays where it was, bound to no row.
    console.error("Could not bind the block editor to the row's block", error);
    kept.row = undefined;
    request.onEnd(KEY_REPORTS.editor_closed);
    return undefined;
  }
  const stops = [
    target.watchAdmission((readOnly) => kept.binding.admissionChanged(readOnly)),
    target.watchPending((state) => {
      if (state.count === 0) kept.binding.noteAcknowledged();
    }),
    watchTyping(element, (text) => kept.binding.noteUnacknowledgedEdit(text)),
  ];
  return () => {
    for (const stop of stops) stop();
    if (kept.row === request) kept.row = undefined;
    unbind();
  };
}

/** The block of `parent` whose id is `blockId`, at any depth. */
function blockOf(parent: XmlFragment, blockId: string): XmlElement | undefined {
  for (const child of parent.toArray()) {
    // A text or a hook holds no block.
    if (!("nodeName" in child)) continue;
    if (child.getAttribute("blockId") === blockId) return child;
    const inner = blockOf(child, blockId);
    if (inner !== undefined) return inner;
  }
  return undefined;
}

/** Tells `note` each text this page types in `element`: the room's own changes are another
 *  writer's, and a sync's. */
function watchTyping(element: XmlElement, note: (text: string) => void): () => void {
  const typed = (events: YEvent<AbstractType<unknown>>[], transaction: Transaction) => {
    if (!transaction.local) return;
    for (const event of events) {
      for (const change of event.delta) {
        if (typeof change.insert === "string") note(change.insert);
      }
    }
  };
  element.observeDeep(typed);
  return () => element.unobserveDeep(typed);
}
