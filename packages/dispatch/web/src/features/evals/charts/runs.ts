// The `runs` kind (dispatch://INFRA-2918/artifact/plan-md, D10; spec R20): the eval's runs, each a
// row of its readings with its settings, tags and final numbers, in a table whose runs a reader
// picks to draw over their steps; each run that started in the compare range marked
// (`runsModel`, `RunsRenderer`).
import type { ChartKind } from "./kind";
import { RunsRenderer } from "./RunsRenderer";
import { runsModel, runsReads } from "./runs-model";

export const runsKind: ChartKind = {
  model: runsModel,
  reads: runsReads,
  renderer: RunsRenderer,
};
