import type { KeyboardEvent } from "react";

import { inputMethodKey } from "../lib/input-method-key";

/**
 * Leaves Enter to a textarea while using Ctrl/Cmd+Enter for an explicit submission. A field whose
 * draft is still taking a picture passes `disabled` while the upload is out, and the key then
 * submits nothing: a draft sent before its upload lands would go without the picture. An Enter
 * that is an input method's (`inputMethodKey`) submits nothing either: one pressed in a
 * composition, or the one that ends it, which Safari sends with `isComposing` false.
 */
export function submitOnModifiedEnter(
  event: KeyboardEvent<HTMLElement>,
  { disabled = false, onFormlessSubmit }: { disabled?: boolean; onFormlessSubmit?: () => void } = {}
): boolean {
  if (
    disabled ||
    event.key !== "Enter" ||
    inputMethodKey(event.nativeEvent) ||
    (!event.ctrlKey && !event.metaKey)
  ) {
    return false;
  }
  event.preventDefault();
  const form = event.currentTarget instanceof HTMLTextAreaElement ? event.currentTarget.form : null;
  if (form === null) {
    onFormlessSubmit?.();
  } else {
    form.requestSubmit();
  }
  return true;
}
