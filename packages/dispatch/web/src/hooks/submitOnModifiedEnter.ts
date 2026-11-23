import type { KeyboardEvent } from "react";

/**
 * Leaves Enter to a textarea while using Ctrl/Cmd+Enter for an explicit submission. A field whose
 * draft is still taking a picture passes `disabled` while the upload is out, and the key then
 * submits nothing: a draft sent before its upload lands would go without the picture. The press
 * is still the field's, so it is prevented either way: a key binding outside the field skips a
 * prevented press (`appKeymap`), so the ask card's send key, around the reply box of its thread,
 * does not take a press the box refused and send the card's own answer in its place.
 */
export function submitOnModifiedEnter(
  event: KeyboardEvent<HTMLElement>,
  { disabled = false, onFormlessSubmit }: { disabled?: boolean; onFormlessSubmit?: () => void } = {}
): boolean {
  if (
    event.key !== "Enter" ||
    event.nativeEvent.isComposing ||
    (!event.ctrlKey && !event.metaKey)
  ) {
    return false;
  }
  event.preventDefault();
  if (disabled) {
    return false;
  }
  const form = event.currentTarget instanceof HTMLTextAreaElement ? event.currentTarget.form : null;
  if (form === null) {
    onFormlessSubmit?.();
  } else {
    form.requestSubmit();
  }
  return true;
}
