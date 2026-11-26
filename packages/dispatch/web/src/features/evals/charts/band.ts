// The `band` kind (dispatch://INFRA-2918/artifact/plan-md, D10): each reading's value is a median
// on a line, its `ci_low` to `ci_high` the shaded spread around it, and a reading with no interval
// a point with no band; grouped, over time or against an x series, as an xy is. The compare range's
// readings shift onto the window's axis as a muted second line, their band an outline
// (`plotModel`, `PlotRenderer`).
import type { ChartKind } from "./kind";
import { PlotRenderer } from "./PlotRenderer";
import { plotModel, plotReads } from "./plot";

export const bandKind: ChartKind = {
  model: (spec, input) => plotModel("band", spec, input),
  reads: plotReads,
  renderer: PlotRenderer,
};
