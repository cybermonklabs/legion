import type { KeyboardEvent as ReactKeyboardEvent } from "react";

/**
 * Whether `event` is an input method's key, which acts on nothing of the page's: one pressed in
 * a composition, or the one that ends it, which Safari sends after its `compositionend` with
 * `isComposing` false and keyCode 229 (WebKit bug 165004). A reader typing Chinese, Japanese or
 * Korean confirms a word with Enter and drops a candidate with Escape, and those keys are the
 * input method's, not presses of the field's own.
 *
 * Who reads it, and no one keeps a check of their own: every handler of Enter or Escape typed
 * into a text field - an input, a textarea or a contenteditable - React's `onKeyDown` and a
 * native listener alike, and every handler that passes over a composition, the key registry's
 * among them (`features/shell/keymap.ts`), which the page's bound keys go through. Arrow, Tab
 * and Backspace handlers of a field are not in that range, unless they pass over a composition,
 * as the row editor's Tab does (`features/issue-page/useNodeEditing.ts`).
 */
export function inputMethodKey(event: KeyboardEvent | ReactKeyboardEvent): boolean {
  const key = "nativeEvent" in event ? event.nativeEvent : event;
  return key.isComposing || key.keyCode === 229;
}
