// The `number` kind (dispatch://INFRA-2918/artifact/plan-md, D10): an eval's headline and its small
// numbers, each the newest reading with a value of one of its series, at the top as tiles or as
// the headline with the numbers beside it, and in its sections as tiles and counts; with the
// compare range, each value's compare value and change printed beside it (`numberModel`,
// `NumberRenderer`).
import type { ChartKind } from "./kind";
import { NumberRenderer } from "./NumberRenderer";
import { numberModel, numberReads } from "./number-model";

export const numberKind: ChartKind = {
  model: numberModel,
  reads: numberReads,
  renderer: NumberRenderer,
};
