// The `xy` kind (dispatch://INFRA-2918/artifact/plan-md, D10): a point for each reading of its y
// series, over measured time, against an x series joined by row, or at its steps; with the compare
// range's readings shifted onto the window's axis as a muted second line over time, and as hollow
// muted points against an x series and at steps (`plotModel`, `PlotRenderer`).
import type { ChartKind } from "./kind";
import { PlotRenderer } from "./PlotRenderer";
import { plotModel, plotReads } from "./plot";

export const xyKind: ChartKind = {
  model: (spec, input) => plotModel("xy", spec, input),
  reads: plotReads,
  renderer: PlotRenderer,
};
