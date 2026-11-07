import type { ReactNode } from "react";

import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { GAP_WORDS, type GapKind } from "./page-words";

/**
 * The small shapes a node wears wherever the page draws one (dispatch://INFRA-1196 R1, R5).
 *
 * A row, the body it opens and the head above the tree all draw the same bordered control - a
 * chip, a gap, an artifact's versions, a decision's Keep and Change - so the shape is written
 * once here rather than once per control, and the gap chip itself is one component, because the
 * head and a row draw the identical thing.
 */

/** The small bordered control a row, a node's body and the head draw. */
export const NODE_CONTROL_CLASS = `rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`;

/**
 * One gap on a node, in the fewest words (`GAP_WORDS`): what the model does not hold about it.
 * The whole sentence is drawn in the node's body, where there is room for it (`GAP_LINES`).
 */
export function GapChip({ gap }: { gap: GapKind }): ReactNode {
  return (
    <span className={NODE_CONTROL_CLASS} data-gap="" data-gap-kind={gap} {...pageChrome}>
      {GAP_WORDS[gap]}
    </span>
  );
}
