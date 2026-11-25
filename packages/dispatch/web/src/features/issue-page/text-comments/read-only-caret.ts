import { rowTextOf } from "./row-text-runs";

/**
 * The caret of a read-only row (R6, decision 11): puts `contenteditable="true"`,
 * `inputmode="none"` and `aria-readonly="true"` on the row's words element (`rowTextOf`), places
 * a collapsed selection at the start of its text, cancels `beforeinput`, `paste`, `drop`, `cut`
 * and `compositionstart`, lets a press on a link in the text follow the link, and removes all of
 * it on Escape (the focus goes back to the row), on blur, and when a re-read replaces or removes
 * the element. Answers the function that takes it away, or null where the row has no words
 * element.
 *
 * No key calls this before G5 (t11): Enter on a read-only row keeps B0's "This row has no text to
 * put the caret in." until part I's browser row moves at G5.
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

/** Places a collapsed selection at the start of `words`' text (its first text node at 0, else the
 *  element itself at 0). */
function putCaretAtStart(words: HTMLElement): void {
  const doc = words.ownerDocument;
  const selection = doc.defaultView?.getSelection() ?? doc.getSelection();
  if (selection === null) return;
  const range = doc.createRange();
  const first = firstTextNode(words);
  if (first !== null) range.setStart(first, 0);
  else range.setStart(words, 0);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
}

/** Puts a read-only caret in `row`'s words element (`rowTextOf(row)`). Answers the function that
 *  takes the caret and its listeners away, or null where `row` has no words element. */
export function caretIn(row: Element): (() => void) | null {
  const words = rowTextOf(row);
  if (words === null) return null;
  active.get(words)?.();

  words.setAttribute("contenteditable", "true");
  words.setAttribute("inputmode", "none");
  words.setAttribute("aria-readonly", "true");
  words.focus({ preventScroll: true });
  putCaretAtStart(words);

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

  // Blur takes the caret away when the focus leaves `words`, unless a press on a link inside it
  // is moving the focus to that link so the browser follows it.
  words.addEventListener(
    "blur",
    (event: FocusEvent) => {
      const next = event.relatedTarget;
      if (next instanceof Node && words.contains(next)) return;
      remove();
    },
    { signal: listening.signal }
  );
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
  if (typeof MutationObserver !== "undefined" && root !== null) {
    observer = new MutationObserver(() => {
      if (!words.isConnected || !row.contains(words)) remove();
    });
    observer.observe(root, { childList: true, subtree: true });
  }

  return remove;
}
