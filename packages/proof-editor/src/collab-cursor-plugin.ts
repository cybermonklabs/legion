import { type EditorState, Plugin } from "@milkdown/kit/prose/state";
import type { DecorationSet, EditorView } from "@milkdown/kit/prose/view";
import {
  collabCursorBuilder,
  collabSelectionBuilder,
} from "proof-sdk-upstream/src/editor/plugins/collab-cursors";
import { yCursorPlugin, yCursorPluginKey } from "y-prosemirror";
import type { Awareness } from "y-protocols/awareness";

/** The class `lib.css` hangs a peer's caret label with back from the caret, toward the line's
 *  start. */
const INWARD_LABEL_CLASS = "proof-collab-cursor__label--inward";

/**
 * y-prosemirror's peer-cursor plugin, except that a peer's caret is not drawn while it sits exactly
 * on the local caret of a focused editor, and a caret's name label turns inward where it would pass
 * the end of the box it scrolls in.
 *
 * A peer's caret is a non-editable widget. When the local caret shares its position,
 * prosemirror-view puts the DOM caret directly beside that widget, and Chromium and WebKit then
 * drop or misplace what the local user types: a new line at the end of a code block, the start of
 * a code line, an empty code block. Firefox is unaffected. The local caret already marks that
 * spot, so the peer's caret reappears as soon as the two part. An unfocused editor has no DOM
 * caret, only a stale selection, so it draws every peer; y-prosemirror republishes the local
 * cursor on focusin and focusout, and that awareness change redraws the decorations. Everything
 * else — the awareness updates, the peer's selection highlight, the builders — is
 * y-prosemirror's own plugin.
 */
export function collabCursorPlugin(awareness: Awareness): Plugin {
  const peerCursors: Plugin = yCursorPlugin(awareness, {
    cursorBuilder: collabCursorBuilder,
    selectionBuilder: collabSelectionBuilder,
  });
  const peerCursorsView = peerCursors.spec.view;
  if (peerCursorsView === undefined) throw new Error("y-prosemirror's cursor plugin has no view");
  let editor: EditorView | undefined;
  return new Plugin({
    ...peerCursors.spec,
    props: {
      ...peerCursors.spec.props,
      decorations: (state) =>
        editor?.hasFocus() ? peerDecorationsAwayFromCaret(state) : yCursorPluginKey.getState(state),
    },
    view: (view) => {
      editor = view;
      const peers = peerCursorsView(view);
      const labels = labelPlacement(view);
      return {
        update: (updated, previous) => {
          peers.update?.(updated, previous);
          labels.schedule();
        },
        destroy: () => {
          labels.destroy();
          peers.destroy?.();
        },
      };
    },
  });
}

function peerDecorationsAwayFromCaret(state: EditorState): DecorationSet | undefined {
  const peers: DecorationSet | undefined = yCursorPluginKey.getState(state);
  const { selection } = state;
  if (peers === undefined || !selection.empty) return peers;
  const { head } = selection;
  const atCaret = peers.find(head, head).filter(({ from, to }) => from === head && to === head);
  return atCaret.length === 0 ? peers : peers.remove(atCaret);
}

/**
 * Decides, for each peer's caret label the editor draws, which way it hangs from its caret.
 *
 * Upstream's label (`collab-cursors.ts`) is placed absolutely at the caret and hangs toward the
 * line's end, one line of up to 200 px. With the caret near a line's end it passed the end of the
 * box the editor scrolls in, and that box scrolled sideways for as long as the caret stood there.
 * Where the label would pass the end of the nearest box that clips or scrolls it, and hanging back
 * toward the line's start keeps it inside that box, it takes `INWARD_LABEL_CLASS`, which mirrors
 * it about the caret (`lib.css`); its look and its whole name stay as they were. Where neither way
 * fits, it keeps upstream's way, which a scroll still reaches, rather than lose its start to the
 * box's edge.
 *
 * A widget's DOM is made by its decoration's `toDOM`, before it is laid out, so the decision is
 * read in the next animation frame after any update that may have moved a caret or redrawn a
 * label, and after the editor or a label's box resizes: every read first, in one layout the
 * frame needs anyway, then every class write. An editor no peer's caret is in reads nothing.
 * prosemirror-view ignores what changes inside a widget, so the class is no edit.
 */
function labelPlacement(view: EditorView): { schedule(): void; destroy(): void } {
  const editorWindow = view.dom.ownerDocument.defaultView;
  if (editorWindow === null) return { destroy: () => {}, schedule: () => {} };
  let frame: number | undefined;
  let watched = new Set<Element>();
  const place = () => {
    frame = undefined;
    if (view.isDestroyed) return;
    const labels = view.dom.querySelectorAll<HTMLElement>(".proof-collab-cursor__label");
    const placed = Array.from(labels, (label) => ({ label, ...labelFit(label, editorWindow) }));
    for (const { inward, label } of placed) label.classList.toggle(INWARD_LABEL_CLASS, inward);
    const boxes = new Set<Element>();
    for (const { box } of placed) if (box !== null && box !== view.dom) boxes.add(box);
    for (const box of watched) if (!boxes.has(box)) resizes.unobserve(box);
    for (const box of boxes) if (!watched.has(box)) resizes.observe(box);
    watched = boxes;
  };
  const schedule = () => {
    if (frame !== undefined || view.isDestroyed) return;
    const peers: DecorationSet | undefined = yCursorPluginKey.getState(view.state);
    if (peers === undefined || peers.find().length === 0) return;
    frame = editorWindow.requestAnimationFrame(place);
  };
  const resizes = new editorWindow.ResizeObserver(schedule);
  resizes.observe(view.dom);
  schedule();
  return {
    destroy: () => {
      if (frame !== undefined) editorWindow.cancelAnimationFrame(frame);
      resizes.disconnect();
    },
    schedule,
  };
}

/**
 * Whether `label` hangs inward, and the box it is held inside (null for the window). The label is
 * as wide either way, since it never wraps, and upstream's `left: -1px` puts its start at the
 * middle of the caret's 2 px bar, where the inward rule puts its end.
 */
function labelFit(
  label: HTMLElement,
  editorWindow: Window
): { box: Element | null; inward: boolean } {
  const caret = label.parentElement;
  if (caret === null) return { box: null, inward: false };
  const { box, end, start } = unscrolledBox(caret, editorWindow);
  const bar = caret.getBoundingClientRect();
  const at = bar.left + bar.width / 2;
  const { width } = label.getBoundingClientRect();
  return { box, inward: at + width > end && at - width >= start };
}

/**
 * The nearest box around `element` that clips or scrolls it sideways, and where that box's
 * content starts and ends unscrolled, in the window's coordinates: what a label may fill without
 * the box scrolling sideways. With no such box below the body, the window's own width.
 */
function unscrolledBox(
  element: Element,
  editorWindow: Window
): { box: Element | null; end: number; start: number } {
  const { body, documentElement } = element.ownerDocument;
  for (let box = element.parentElement; box !== null && box !== body; box = box.parentElement) {
    if (editorWindow.getComputedStyle(box).overflowX === "visible") continue;
    const start = box.getBoundingClientRect().left + box.clientLeft - box.scrollLeft;
    return { box, end: start + box.clientWidth, start };
  }
  const start = -editorWindow.scrollX;
  return { box: null, end: start + documentElement.clientWidth, start };
}
