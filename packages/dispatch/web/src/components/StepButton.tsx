import type { ReactNode } from "react";

import { borderDefault, surfaceMutedHoverBg } from "../theme/classes";
import { heldPress } from "./held-press";

/**
 * One small step a table offers: the pager's ‹ Previous and Next ›, the project List's selection
 * bar actions. A step that cannot be taken now - the last page reached, an action holding the
 * page busy while its write is out - is held (`heldPress`).
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
      className={`rounded-lg border px-3 py-1 text-sm refused:opacity-40 ${borderDefault} ${surfaceMutedHoverBg}`}
      {...heldPress(ariaDisabled, onClick)}
      type="button"
    >
      {children}
    </button>
  );
}
