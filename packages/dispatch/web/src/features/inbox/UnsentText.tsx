import type { ReactNode } from "react";

import { textPrimaryOnSurface } from "../../theme/classes";
import { borderDefault, textMutedOnSurface } from "../../theme/shell-classes";

/**
 * Text the reader wrote under an ask that no form there will send any more, drawn read-only
 * where its box was, so they can still read and copy it: an answer typed under a question that
 * closed (`AskCard`, `unsentAnswer`), and a reply typed under a settled ask that opened again
 * (`AskThread`, `useUnsentReply`). `why` says what happened to the box, and the text keeps its
 * line breaks as typed.
 */
export function UnsentText({
  className = "",
  testId,
  text,
  why,
}: {
  /** The caller's own spacing around the block. */
  className?: string;
  testId: string;
  text: string;
  why: string;
}): ReactNode {
  return (
    <div className={`rounded-xl border p-3 ${borderDefault} ${className}`} data-testid={testId}>
      <p className={`text-xs ${textMutedOnSurface}`}>{why}</p>
      <p className={`mt-1 text-sm whitespace-pre-wrap ${textPrimaryOnSurface}`}>{text}</p>
    </div>
  );
}
