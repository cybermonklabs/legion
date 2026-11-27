import type { ReactNode } from "react";

import { borderDefault, surfaceMutedHoverBg } from "../theme/classes";
import { type ControlSize, controlPadding } from "./control-names";
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
  size = "comfortable",
}: {
  readonly ariaDisabled: boolean;
  readonly children: ReactNode;
  readonly onClick: () => void;
  /** How much room the step takes (`ControlSize`): the size of the page it is drawn on, which
   *  the pager and the project List's selection bar hand on. */
  readonly size?: ControlSize;
}): ReactNode {
  return (
    <button
      className={`rounded-lg border text-sm refused:opacity-40 ${controlPadding[size]} ${borderDefault} ${surfaceMutedHoverBg}`}
      {...heldPress(ariaDisabled, onClick)}
      type="button"
    >
      {children}
    </button>
  );
}
