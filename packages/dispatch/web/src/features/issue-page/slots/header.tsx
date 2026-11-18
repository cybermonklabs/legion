import type { ReactNode } from "react";

import { textPrimaryOnCanvas, textSecondaryOnCanvas } from "../../../theme/classes";
import type { SlotProps } from "../slot-props";

/** Filled by INFRA-1196 part P, "The header". */
export function HeaderSlot({ issue }: SlotProps): ReactNode {
  return (
    <div>
      <p className={`font-mono text-sm font-semibold ${textSecondaryOnCanvas}`}>{issue.key}</p>
      <h1 className={`mt-1 break-words text-xl font-semibold ${textPrimaryOnCanvas}`}>
        {issue.title}
      </h1>
    </div>
  );
}
