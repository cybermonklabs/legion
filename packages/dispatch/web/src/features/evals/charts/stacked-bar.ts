// The `stacked-bar` kind (dispatch://INFRA-2918/artifact/plan-md, D10): a bar per measured date
// with a segment per col or row group (or a stacked area), or with `stack: "row"` a bar per row
// with a segment per col, a frame per week with `frames`. Over time the compare range's readings
// shift onto the window's axis as a muted second stack at each date (a dashed outline on an
// area); by row each bar's compare cells draw as a muted ghost stack beside it, each segment's
// change printed in it (`barsModel`, `BarsRenderer`).
import { BarsRenderer } from "./BarsRenderer";
import { barsModel, barsReads } from "./bars";
import type { ChartKind } from "./kind";

export const stackedBarKind: ChartKind = {
  model: (spec, input) => barsModel("stacked-bar", spec, input),
  reads: barsReads,
  renderer: BarsRenderer,
};
