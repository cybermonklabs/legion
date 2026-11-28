import type { ReactNode } from "react";

import { textMutedOnSurface, textPrimaryOnSurface } from "../../../theme/classes";
import { type BlockOf, cellText } from "./block-cells";

/** A metric: the one number of the read's one row, named by its label, and the one comparison
 *  beside it where the spec names one. */
export function MetricBlock({ block }: { readonly block: BlockOf<"metric"> }): ReactNode {
  const row = block.rows[0];
  if (row === undefined) {
    return null;
  }
  const { compare, compare_label: compareLabel, field, label } = block.spec;
  return (
    <dl className="flex flex-wrap items-baseline gap-x-6 gap-y-1">
      <div>
        <dt className={`text-xs ${textMutedOnSurface}`}>{label}</dt>
        <dd
          className={`text-2xl font-semibold ${textPrimaryOnSurface}`}
          data-testid="going-block-metric-value"
        >
          {cellText(row[field])}
        </dd>
      </div>
      {compare === undefined || compare === "" ? null : (
        <div>
          <dt className={`text-xs ${textMutedOnSurface}`}>{compareLabel ?? compare}</dt>
          <dd
            className={`text-lg ${textPrimaryOnSurface}`}
            data-testid="going-block-metric-compare"
          >
            {cellText(row[compare])}
          </dd>
        </div>
      )}
    </dl>
  );
}
