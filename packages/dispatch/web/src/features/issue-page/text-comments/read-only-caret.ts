/**
 * The caret of a read-only row (R6, decision 11): sets `contenteditable="true"`,
 * `inputmode="none"` and `aria-readonly="true"` on the row's words element (`rowTextOf`), puts the
 * caret at its start, cancels `beforeinput`, `paste`, `drop`, `cut` and `compositionstart`,
 * follows a press on a link, and takes all of it away again on Escape (the focus goes back to the
 * row), on blur and when a re-read replaces the element. Answers the function that takes it away,
 * or null where the row has no words element. Built by t10; it does nothing until then.
 */
export function caretIn(_row: Element): (() => void) | null {
  return null;
}
