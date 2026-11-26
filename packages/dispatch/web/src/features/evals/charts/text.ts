// The `text` kind (dispatch://INFRA-2918/artifact/plan-md, D10): a table of text and numbers from
// one grid series, a row for each of its rows and a column for each declared column (a value, a
// note or a spark of another series), each cell the newest reading of its row and col; with the
// compare range, each value's compare value and change printed beside it (`textModel`,
// `TextRenderer`).
import type { ChartKind } from "./kind";
import { TextRenderer } from "./TextRenderer";
import { textModel, textReads } from "./text-model";

export const textKind: ChartKind = {
  model: textModel,
  reads: textReads,
  renderer: TextRenderer,
};
