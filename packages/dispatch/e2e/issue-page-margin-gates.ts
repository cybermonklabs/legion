import type { GateName } from "../web/src/features/issue-page/text-comments/gates";

// The browser rows read the one gate table the unit tests read.
export { GATES } from "../web/src/features/issue-page/text-comments/gates";

export interface SkippedMarginItem {
  item: string;
  gate: GateName;
}
