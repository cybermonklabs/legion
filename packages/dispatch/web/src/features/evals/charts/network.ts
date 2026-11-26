// The `network` kind (dispatch://INFRA-2918/artifact/plan-md, D10): its eval's `node` readings as
// nodes sized, coloured, ringed and named by their attributes, joined by its `edge` readings; with
// the compare range's size and colour of each node as a dashed ring around it and in its table
// beside its own (`networkModel`, `NetworkRenderer`).
import type { ChartKind } from "./kind";
import { NetworkRenderer } from "./NetworkRenderer";
import { networkModel, networkReads } from "./network-model";

export const networkKind: ChartKind = {
  model: networkModel,
  reads: networkReads,
  renderer: NetworkRenderer,
};
