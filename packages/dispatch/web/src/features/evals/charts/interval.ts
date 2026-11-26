// The `interval` kind (dispatch://INFRA-2918/artifact/plan-md, D10): each category's newest
// reading as a point with its supplied interval, "no interval" where it has none, by value where
// `order` names none, or grouped in clusters coloured by col. The compare range's newest reading
// of each category draws as a muted ghost point beside it, with the change printed over it
// (`barsModel`, `BarsRenderer`).
import { BarsRenderer } from "./BarsRenderer";
import { barsModel, barsReads } from "./bars";
import type { ChartKind } from "./kind";

export const intervalKind: ChartKind = {
  model: (spec, input) => barsModel("interval", spec, input),
  reads: barsReads,
  renderer: BarsRenderer,
};
