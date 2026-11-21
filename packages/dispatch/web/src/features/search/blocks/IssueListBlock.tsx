import type { ReactNode } from "react";

import {
  badgeLow,
  borderDefault,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../../theme/classes";
import { statusText } from "../../project/board-model";
import { type BlockOf, cellText } from "./block-cells";

/** A list of issues: each issue's key, title and status, as the read listed them. The keys are
 *  text, not links: an answer's links are its item frames alone. */
export function IssueListBlock({ block }: { readonly block: BlockOf<"issue_list"> }): ReactNode {
  return (
    <ul className="text-xs" data-testid="going-block-issues">
      {block.rows.map((row, index) => (
        <li
          className={`flex items-baseline gap-2 border-b py-1 last:border-b-0 ${borderDefault}`}
          data-testid="going-block-issue"
          // biome-ignore lint/suspicious/noArrayIndexKey: a block's rows never reorder.
          key={index}
        >
          <span className={`shrink-0 font-semibold ${textPrimaryOnSurface}`}>
            {cellText(row.key)}
          </span>
          <span className={`min-w-0 flex-1 truncate ${textSecondaryOnSurface}`}>
            {cellText(row.title)}
          </span>
          <span className={`shrink-0 rounded-full px-2 py-0.5 ${badgeLow.bg} ${badgeLow.text}`}>
            {statusText(cellText(row.status))}
          </span>
        </li>
      ))}
    </ul>
  );
}
