import type { ReactNode } from "react";

import {
  badgeLow,
  borderDefault,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../../theme/classes";
import { type BlockOf, cellText } from "./block-cells";

/** A timeline: each dated event of the read in its lane, oldest first, as the read ordered them. */
export function TimelineBlock({ block }: { readonly block: BlockOf<"timeline"> }): ReactNode {
  return (
    <ol className="text-xs" data-testid="going-block-timeline">
      {block.rows.map((row, index) => {
        const at = cellText(row.at);
        const when = new Date(at);
        return (
          <li
            className={`flex items-baseline gap-2 border-b py-1 last:border-b-0 ${borderDefault}`}
            data-testid="going-block-timeline-event"
            // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
            key={index}
          >
            <time className={`shrink-0 tabular-nums ${textMutedOnSurface}`} dateTime={at}>
              {Number.isNaN(when.getTime()) ? at : when.toLocaleString()}
            </time>
            <span className={`shrink-0 rounded-full px-2 py-0.5 ${badgeLow.bg} ${badgeLow.text}`}>
              {cellText(row.lane)}
            </span>
            <span className={`min-w-0 flex-1 ${textPrimaryOnSurface}`}>{cellText(row.label)}</span>
          </li>
        );
      })}
    </ol>
  );
}
