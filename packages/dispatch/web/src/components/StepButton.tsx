import type { ReactNode } from "react";

import { borderDefault, surfaceMutedHoverBg } from "../theme/classes";

/**
 * One small step a table offers: the pager's ‹ Previous and Next ›, the project List's selection
 * bar actions. A step that cannot be taken now is `aria-disabled` rather than `disabled`, and a
 * press on it does nothing. Each of these turns inert under the reader's own press - the step
 * that reaches the last page, an action that holds the page busy while its write is out - and the
 * browser takes the focus off a control that disables itself, dropping a keyboard reader onto the
 * page, while a dialog the press opened would hand the focus back to an opener that can no longer
 * take it. Held, the button keeps the focus and its Tab stop, and the `refused:` variant
 * (`styles.css`) paints it as it paints a disabled one.
 */
export function StepButton({
  ariaDisabled,
  children,
  onClick,
}: {
  readonly ariaDisabled: boolean;
  readonly children: ReactNode;
  readonly onClick: () => void;
}): ReactNode {
  return (
    <button
      aria-disabled={ariaDisabled || undefined}
      className={`rounded-lg border px-3 py-1 text-sm refused:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
      onClick={() => {
        if (!ariaDisabled) onClick();
      }}
      type="button"
    >
      {children}
    </button>
  );
}
