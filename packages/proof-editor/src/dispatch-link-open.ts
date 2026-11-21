/**
 * How the links of a document open, in place of upstream's `markdownLinkClickPlugin`, which left a
 * plain click in an editable view to place the caret and showed an Open link card instead
 * (dispatch://INFRA-2160, R1 and R2). The editor has no router, so the host opens each target
 * itself through `openLink` (`CreateProofEditorOptions`): a `dispatch://` reference and a link to
 * one of the host's own pages in the same tab, another site in a new one. Without `openLink` this
 * plugin is not installed and a link is the browser's.
 *
 * It acts on the links the editor draws: a link mark's anchor, a reference title
 * (`./reference-decorations.ts`), a link a node view draws as its own content (Dispatch's
 * picture fallback), and a pointer a typed block's header draws (the `serves` and `needs` links
 * of a plan's task or a Verification document's check, Dispatch's `typed-block-drawing.ts`). An
 * anchor deeper inside a host island - below an element of the editor that is
 * `contenteditable="false"` and is neither the anchor, the anchor's parent nor a typed block's
 * header, such as the slot a host draws a thread into - is the host's, and left to it.
 * ProseMirror sets `contenteditable="false"` on a widget's own element and on a leaf node view's
 * root, so a title and the picture fallback are no island to themselves. A typed block's header
 * (`[data-proof-block-summary]`) is `contenteditable="false"` so that the reader cannot edit it,
 * and is the block's own drawing rather than a slot, so its pointers are the editor's; one in a
 * block a host draws inside its island, such as a comment's in a thread's slot, is the host's.
 *
 * - A primary click with no modifier opens the link, in every access mode and inside commented
 *   text, unless a drag selected text across it; Ctrl or Cmd opens it in a new tab. ProseMirror has
 *   no built-in `click` handler, so this one runs for every click inside the editor, a widget's and
 *   a node view's included, whatever their `stopEvent` says.
 * - A middle click (`auxclick`, button 1) opens it in a new tab, which also cancels the browser's
 *   own new tab for an anchor with a real `href`. Its press and its release are prevented, so the
 *   browser pastes nothing into the document (Linux pastes its primary selection on a middle
 *   button's release). The press reaches a link mark and a header's pointer here; a title prevents
 *   every press itself, and the picture fallback prevents a middle one (Dispatch's
 *   `picture-view.ts`).
 * - Alt+Enter (Option+Enter on a Mac) is the host's key, not the editor's: the host's keymap binds
 *   it to the handle's `openLinkAtCaret`, so a reader finds it in the host's list of keys and can
 *   change it. ProseMirror prevents the default of every Enter it does not handle, and a host
 *   listener that skips a prevented press would never hear it, so with a link at the caret the
 *   press is left to the host; should nothing after the editor take it, it is prevented then, so
 *   the browser writes no new line into the text. The caret is read where the page's selection
 *   holds it (`caretPos`), since the editor's state can still hold it where it was before the key
 *   pressed just before.
 */

import type { Mark } from "@milkdown/kit/prose/model";
import { Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

import { asElement } from "./record-mark-target";
import { referenceAt } from "./reference-decorations";

/** Opens a link's own target for the host - a `dispatch://` reference, a web address, a `mailto:`
 *  address - in a new tab when `newTab` says so: a Ctrl, Cmd or middle click. Where the target
 *  goes and how is the host's to decide. */
export type OpenLink = (target: string, how: { readonly newTab: boolean }) => void;

const linkOpenKey = new PluginKey("dispatch-link-open");

/** The attribute of the header a typed block's drawing puts above its content
 *  (Dispatch's `typed-block-drawing.ts`): `contenteditable="false"`, and no host island. */
const TYPED_BLOCK_HEADER = "data-proof-block-summary";

/** Whether an element between `anchor` and the editor's root, other than the anchor itself, its
 *  parent and a typed block's header, is `contenteditable="false"`: the anchor is inside a host
 *  island. */
function inIsland(view: EditorView, anchor: Element): boolean {
  const parent = anchor.parentElement;
  if (parent === null || parent === view.dom) {
    return false;
  }
  for (
    let node = parent.parentElement;
    node !== null && node !== view.dom;
    node = node.parentElement
  ) {
    if (
      node.getAttribute("contenteditable") === "false" &&
      !node.hasAttribute(TYPED_BLOCK_HEADER)
    ) {
      return true;
    }
  }
  return false;
}

/** A link the editor drew, and the target it opens: the `dispatch://` reference it carries as
 *  `data-dispatch-href` (a link mark to a reference, a title, the picture fallback and a header's
 *  pointer carry one), else its `href`. Empty for a link whose target the sanitizer emptied (a
 *  scheme it refuses), which opens nothing. */
interface EditorLink {
  readonly anchor: Element;
  readonly target: string;
}

/** The link the editor drew that `target` is inside, if any; a target that is no element is
 *  inside none. */
function linkAt(view: EditorView, target: EventTarget | null): EditorLink | undefined {
  const anchor = asElement(target)?.closest("a") ?? null;
  if (anchor === null || !view.dom.contains(anchor) || inIsland(view, anchor)) {
    return undefined;
  }
  return {
    anchor,
    target: anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href") ?? "",
  };
}

/** The link a primary click opens, or undefined for a click that opens none: the one a drag that
 *  ended on it selected text across - which the click must not throw away by opening the link -
 *  opens nothing. A title prevents its own press, so an older selection elsewhere may stand while
 *  it is clicked; only one across the link counts. A Ctrl or Cmd click is never a drag's end:
 *  ProseMirror reads its press as selecting the node it lands in, which leaves a selection across
 *  the link that the reader never made. */
function clickedLink(view: EditorView, event: MouseEvent): EditorLink | undefined {
  if (event.button !== 0 || event.shiftKey || event.altKey) {
    return undefined;
  }
  const link = linkAt(view, event.target);
  if (link === undefined || event.ctrlKey || event.metaKey) {
    return link;
  }
  const selection = view.dom.ownerDocument.getSelection();
  const dragged =
    selection !== null && !selection.isCollapsed && selection.containsNode(link.anchor, true);
  return dragged ? undefined : link;
}

/** Whether the editor opens a link for `event`, a primary click or the release ProseMirror reads
 *  as one (`handleClick`): what `./dispatch-mark-events.ts` stands down on, so a click on a link
 *  inside commented text opens the link rather than the comment. False where this plugin is not
 *  installed. */
export function opensLinkOn(view: EditorView, event: MouseEvent): boolean {
  return linkOpenKey.get(view.state) !== undefined && clickedLink(view, event) !== undefined;
}

/** Where the caret is: in an editor that has the focus, the head of the page's selection, else
 *  the head of the editor's own. A caret an arrow key moved natively reaches ProseMirror's state
 *  only once the page's `selectionchange` for it has run, or once ProseMirror's own handler of the
 *  next key reads the page's selection, before that key's keymaps; a `handleDOMEvents` handler
 *  runs before that read, and a busy page can run the next key before the `selectionchange`
 *  (dispatch://INFRA-2160, k-link-keys). Read from the state alone, Alt+Enter pressed straight
 *  after an arrow found the caret where it stood before the arrow, beside no link, so it was left
 *  to ProseMirror, which prevents every Enter it does not handle, and the host's key never heard
 *  it. Without the focus the page's selection is not the editor's, and the state's caret is. */
function caretPos(view: EditorView): number {
  const selection = view.dom.ownerDocument.getSelection();
  const node = selection?.focusNode ?? null;
  if (selection !== null && node !== null && view.hasFocus() && view.dom.contains(node)) {
    try {
      return view.posAtDOM(node, selection.focusOffset);
    } catch {
      // A point inside something the editor draws no document position for: the state's caret.
    }
  }
  return view.state.selection.head;
}

/** What Alt+Enter opens at the caret (`caretPos`): the target of the link the caret is in or at
 *  either end of, else the `dispatch://` reference of the title drawn over the text the caret
 *  holds or touches (`referenceAt`: the title gives way to that text while the caret is there),
 *  else undefined. A link mark keeps its target as written, a `dispatch://` one included; only
 *  its drawn `href` is sanitized. */
export function linkAtCaret(view: EditorView): string | undefined {
  const { state } = view;
  const $head = state.doc.resolve(caretPos(view));
  const isLink = (mark: Mark) => mark.type.name === "link";
  const href: unknown = (
    $head.nodeAfter?.marks.find(isLink) ?? $head.nodeBefore?.marks.find(isLink)
  )?.attrs.href;
  if (typeof href === "string" && href !== "") {
    return href;
  }
  return referenceAt(state, $head.pos)?.href;
}

/** Prevents the browser's own handling of `event` - a new line in the text - once the press has
 *  reached the window, unless a listener there took it first: the host's keymap listens on the
 *  window, and so heard it before this one, which is added later. */
function preventUnlessTaken(view: EditorView, event: KeyboardEvent): void {
  const window = view.dom.ownerDocument.defaultView;
  if (window === null) {
    event.preventDefault();
    return;
  }
  window.addEventListener(
    "keydown",
    (seen) => {
      if (seen === event && !event.defaultPrevented) {
        event.preventDefault();
      }
    },
    { once: true }
  );
}

/** A middle button's press or release on a link: prevented, so the browser pastes nothing. */
function preventMiddleButton(view: EditorView, event: MouseEvent): boolean {
  if (event.button !== 1 || linkAt(view, event.target) === undefined) {
    return false;
  }
  event.preventDefault();
  return true;
}

/** The plugin that opens the document's links through the host's `openLink` (the module's doc
 *  says which links and how). */
export function dispatchLinkOpenPlugin(openLink: OpenLink) {
  return $prose(
    () =>
      new Plugin({
        key: linkOpenKey,
        props: {
          handleDOMEvents: {
            click(view, event) {
              const link = clickedLink(view, event);
              if (link === undefined) {
                return false;
              }
              event.preventDefault();
              if (link.target !== "") {
                openLink(link.target, { newTab: event.ctrlKey || event.metaKey });
              }
              return true;
            },
            auxclick(view, event) {
              const link = event.button === 1 ? linkAt(view, event.target) : undefined;
              if (link === undefined) {
                return false;
              }
              event.preventDefault();
              if (link.target !== "") {
                openLink(link.target, { newTab: true });
              }
              return true;
            },
            mousedown: preventMiddleButton,
            mouseup: preventMiddleButton,
            keydown(view, event) {
              const altEnter =
                event.key === "Enter" &&
                event.altKey &&
                !event.ctrlKey &&
                !event.metaKey &&
                !event.shiftKey;
              if (!view.editable || !altEnter || linkAtCaret(view) === undefined) {
                return false;
              }
              preventUnlessTaken(view, event);
              return true;
            },
          },
        },
      })
  );
}
