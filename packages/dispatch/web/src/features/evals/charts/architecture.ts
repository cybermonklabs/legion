// The `architecture` kind (dispatch://INFRA-2918/artifact/plan-md, D10): its value series' `part`
// readings as a diagram of a system's parts, each shaded by its value's bin and placed by its
// attributes, joined by its eval's `edge` readings; with each value's compare value and change
// printed beside it while the compare range is shown (`architectureModel`,
// `ArchitectureRenderer`).
import { ArchitectureRenderer } from "./ArchitectureRenderer";
import { architectureModel, architectureReads } from "./architecture-model";
import type { ChartKind } from "./kind";

export const architectureKind: ChartKind = {
  model: architectureModel,
  reads: architectureReads,
  renderer: ArchitectureRenderer,
};
