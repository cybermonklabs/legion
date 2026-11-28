// The renderer of a `prose` model (prose.ts): its paragraphs in body text, once, whether or not
// the compare range is shown, since prose holds no number to compare.
import type { ReactNode } from "react";

import { textPrimaryOnSurface } from "../../../theme/classes";
import type { ChartRendererProps } from "../chart-model";

export function ProseRenderer({ model }: ChartRendererProps): ReactNode {
  if (model.kind !== "prose") return null;
  return (
    <div className="flex flex-col gap-2" data-part="prose">
      {(model.series[0]?.points ?? []).map((point) => (
        <p className={`text-sm ${textPrimaryOnSurface}`} data-paragraph={point.x} key={point.x}>
          {point.values.text?.label}
        </p>
      ))}
    </div>
  );
}
