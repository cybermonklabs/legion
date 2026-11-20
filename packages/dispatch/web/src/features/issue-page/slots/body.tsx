import type { ReactNode } from "react";

import { ConversationRows } from "../ConversationRows";
import { NoTooltips } from "../NoTooltips";
import type { SlotProps } from "../slot-props";

/** Filled by INFRA-1196 part G, "A node's row", then part I, "The outline". */
export function BodySlot({ issue }: SlotProps): ReactNode {
  // The conversation's rows, every branch unfolded (part G's R8); part I replaces them with the
  // outline's tree.
  return (
    <NoTooltips>
      <ConversationRows issue={issue} />
    </NoTooltips>
  );
}
