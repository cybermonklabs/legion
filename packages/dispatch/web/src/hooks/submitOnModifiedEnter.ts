import type { KeyboardEvent } from "react";

import { inputMethodKey } from "./inputMethodKey";

/**
 * The one key rule of every Dispatch text box: Ctrl/Cmd+Enter sends or saves, and Enter alone
 * adds a line, which this function leaves to the textarea. A new text box calls it from its
 * `onKeyDown` rather than reading Enter itself, so every box answers the same keys.
 *
 * The live view's composer is the one exception: there Enter alone sends, as it does in a chat
 * (`ComposerPrimitive` in `features/agent-view/AgentThread.tsx`).
 *
 * A field whose draft is still taking a picture passes `disabled` while the upload is out, and
 * the key then submits nothing: a draft sent before its upload lands would go without the picture.
 * The Enter that confirms an input method's word submits nothing either (`inputMethodKey`).
 */
export function submitOnModifiedEnter(
  event: KeyboardEvent<HTMLElement>,
  { disabled = false, onFormlessSubmit }: { disabled?: boolean; onFormlessSubmit?: () => void } = {}
): boolean {
  if (
    disabled ||
    event.key !== "Enter" ||
    inputMethodKey(event) ||
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
