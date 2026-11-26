import type { NodeType } from "../page-words";
import { rowTextOf, type TextRuns, textRuns } from "./row-text-runs";
import { ITEM_PLACE_KINDS } from "./text-comment-write";

/**
 * The caret of a read-only row (R6, decision 11): puts `contenteditable="true"`,
 * `inputmode="none"` and `aria-readonly="true"` on the row's words element (`rowTextOf`), places
 * a collapsed selection at the start of its text, cancels `beforeinput`, `paste`, `drop`, `cut`
 * and `compositionstart`, lets a press on a link in the text follow the link, and removes all of
 * it on Escape (the focus goes back to the row), on blur, and when a re-read replaces or removes
 * the element. Answers the function that takes it away, or null where the row has no words
 * element or draws none of its node's text.
 *
 * Enter on the focused row of a read-only node puts it in (`caretOnEnter`, which the tree's
 * `useNodeEditing` calls before it reports that a row has no text to put the caret in). A press
 * puts none in (decision 420 (1)): it moves the focus to the row, as on any row that takes no
 * editor.
 */

/** The input events that would change the text or start a composition, each cancelled while the
 *  caret is in the words element. */
const BLOCKED_INPUT = ["beforeinput", "paste", "drop", "cut", "compositionstart"] as const;

/** The active teardown for each words element that holds a read-only caret, so a second call on
 *  the same row replaces its own listeners cleanly. */
const active = new WeakMap<HTMLElement, () => void>();

/** The first text node under `root` in document order, or null where it holds none. */
function firstTextNode(root: Node): Text | null {
  const walker = root.ownerDocument?.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const next = walker?.nextNode() ?? null;
  return next instanceof Text ? next : null;
}

/** Places a collapsed selection at the start of `words`' text: where the node's first character
 *  is drawn (`runs`), so what the row draws before it - an ask's card, a typed block's head - holds
 *  no caret; else, where a reference's name is drawn in place of that character, at the first
 *  text node at 0. */
function putCaretAtStart(words: HTMLElement, runs: TextRuns): void {
  const doc = words.ownerDocument;
  const selection = doc.defaultView?.getSelection() ?? doc.getSelection();
  if (selection === null) return;
  const range = doc.createRange();
  const start = runs.pointOf(0);
  if (start !== null) range.setStart(start.node, start.offset);
  else range.setStart(firstTextNode(words) ?? words, 0);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
}

/**
 * Enter on `row`, the focused row of a node of `kind` (R6): where the node is read-only in the
 * room (`ITEM_PLACE_KINDS`) and its row draws its text, puts the caret at the start of that text
 * (`caretIn`) and answers true. Answers false for any other row - a block of a document, whose
 * editor takes the caret, and a row that draws none of its node's text: a lens with no verdict,
 * a deleted post, an empty text, a row drawn on one line - which Enter reports as it always has.
 */
export function caretOnEnter(row: Element | null, kind: NodeType): boolean {
  return row !== null && ITEM_PLACE_KINDS[kind] === true && caretIn(row) !== null;
}

/** Puts a read-only caret in `row`'s words element (`rowTextOf(row)`). Answers the function that
 *  takes the caret and its listeners away, or null where `row` has no words element or the
 *  element draws none of its node's text. */
function caretIn(row: Element): (() => void) | null {
  const words = rowTextOf(row);
  if (words === null) return null;
  const runs = textRuns(words);
  if (runs.text === "") return null;
  active.get(words)?.();

  words.setAttribute("contenteditable", "true");
  words.setAttribute("inputmode", "none");
  words.setAttribute("aria-readonly", "true");
  words.focus({ preventScroll: true });
  putCaretAtStart(words, runs);

  const listening = new AbortController();
  let removed = false;
  let observer: MutationObserver | undefined;

  const remove = (): void => {
    if (removed) return;
    removed = true;
    listening.abort();
    observer?.disconnect();
    if (active.get(words) === remove) active.delete(words);
    words.removeAttribute("contenteditable");
    words.removeAttribute("inputmode");
    words.removeAttribute("aria-readonly");
  };

  active.set(words, remove);

  const cancel = (event: Event): void => {
    event.preventDefault();
  };
  for (const type of BLOCKED_INPUT) {
    words.addEventListener(type, cancel, { signal: listening.signal });
  }

  words.addEventListener(
    "keydown",
    (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented || event.isComposing) return;
      event.preventDefault();
      remove();
      if (row instanceof HTMLElement) row.focus({ preventScroll: true });
    },
    { signal: listening.signal }
  );

  // A press on a link in the words follows the link: a browser follows none inside editable
  // text, so the caret goes first, in the press's own handler, before the browser decides what
  // the press does. The press is never cancelled here.
  words.addEventListener(
    "click",
    (event: MouseEvent) => {
      const target = event.target;
      const link = target instanceof Element ? target.closest("a[href]") : null;
      if (link !== null && words.contains(link)) remove();
    },
    { signal: listening.signal }
  );

  // Focus leaving `words` or anything inside it takes the caret away (`focusout` fires wherever
  // `blur` does, and for a child too), unless a press on a link inside it is moving the focus to
  // that link so the browser follows it.
  words.addEventListener(
    "focusout",
    (event: FocusEvent) => {
      const next = event.relatedTarget;
      if (next instanceof Node && words.contains(next)) return;
      remove();
    },
    { signal: listening.signal }
  );

  // When a re-read replaces or removes `words` from the row or document, take its attributes and
  // listeners away.
  const root = words.ownerDocument.body ?? row.parentNode ?? row;
  if (typeof MutationObserver !== "undefined") {
    observer = new MutationObserver(() => {
      if (!words.isConnected || !row.contains(words)) remove();
    });
    observer.observe(root, { childList: true, subtree: true });
  }

  return remove;
}
