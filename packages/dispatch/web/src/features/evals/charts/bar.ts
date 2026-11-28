// The `bar` kind (dispatch://INFRA-2918/artifact/plan-md, D10): each category's newest reading as
// a bar labelled with its number, by value where `order` names none; grouped in clusters, as
// horizontal rows (against each row's total with `of`, as a dot or dots with `mark`, with notes,
// group headings and columns), or as a facet table. The compare range's newest reading of each
// category draws as a tick across its bar, with the change printed over the value (`barsModel`,
// `BarsRenderer`).
import { BarsRenderer } from "./BarsRenderer";
import { barsModel, barsReads } from "./bars";
import type { ChartKind } from "./kind";

export const barKind: ChartKind = {
  model: (spec, input) => barsModel("bar", spec, input),
  reads: barsReads,
  renderer: BarsRenderer,
};
