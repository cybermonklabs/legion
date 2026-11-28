import type { ReactNode } from "react";

import {
  progressFill,
  progressTrack,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../../theme/classes";
import { type BlockOf, cellNumber, cellText } from "./block-cells";

/** Progress: one bar for each row of the read, named by its label, filled `done` of `total`. */
export function ProgressBlock({ block }: { readonly block: BlockOf<"progress"> }): ReactNode {
  const { done, label, total } = block.spec;
  return (
    <ul className="space-y-2">
      {block.rows.map((row, index) => {
        const name = cellText(row[label]);
        const of = cellNumber(row[total]) ?? 0;
        const reached = cellNumber(row[done]) ?? 0;
        const share = of > 0 ? Math.min(100, Math.max(0, (reached / of) * 100)) : 0;
        return (
          <li
            data-testid="going-block-progress-row"
            // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
            key={index}
          >
            <div className="flex items-baseline justify-between gap-2 text-xs">
              <span className={textPrimaryOnSurface}>{name}</span>
              <span className={textSecondaryOnSurface} data-testid="going-block-progress-count">
                {cellText(row[done])} of {cellText(row[total])}
              </span>
            </div>
            <div
              aria-label={name}
              aria-valuemax={of}
              aria-valuemin={0}
              aria-valuenow={reached}
              className={`mt-1 h-1.5 overflow-hidden rounded-full ${progressTrack}`}
              role="progressbar"
            >
              <div className={`h-full ${progressFill}`} style={{ width: `${share}%` }} />
            </div>
          </li>
        );
      })}
    </ul>
  );
}
