// The kinds of declared chart the Evals tab draws (dispatch://INFRA-2918/artifact/plan-md, D10,
// T20), by kind: each its items views, its pure model function and its renderer (`ChartKind`,
// kind.ts). A declared chart of a kind this record lacks draws one line naming it
// (`DeclaredEval`). `kinds.test.ts` lists the 14 kinds the validator accepts against it.
import { architectureKind } from "./architecture";
import { bandKind } from "./band";
import { barKind } from "./bar";
import { gridKind } from "./grid";
import { intervalKind } from "./interval";
import type { ChartKind } from "./kind";
import { lanesKind } from "./lanes";
import { networkKind } from "./network";
import { numberKind } from "./number";
import { pendingKind } from "./pending";
import { proseKind } from "./prose";
import { runsKind } from "./runs";
import { stackedBarKind } from "./stacked-bar";
import { textKind } from "./text";
import { xyKind } from "./xy";

export const chartKinds: Readonly<Record<string, ChartKind>> = {
  architecture: architectureKind,
  band: bandKind,
  bar: barKind,
  grid: gridKind,
  interval: intervalKind,
  lanes: lanesKind,
  network: networkKind,
  number: numberKind,
  pending: pendingKind,
  prose: proseKind,
  runs: runsKind,
  "stacked-bar": stackedBarKind,
  text: textKind,
  xy: xyKind,
};
