// Mirrors web/src/features/issue-page/text-comments/gates.ts. The two tables move together.
export const GATES = {
  ANCHOR_OCCURRENCE: false,
  COMPOSER: false,
  DASHBOARD: false,
  NODE_ANCHOR: false,
  OTHER_ISSUE_COMMENTS: false,
  PART_J: false,
  PART_M: false,
  PART_N: false,
  PART_U: false,
  PART_X: false,
  PART_Y: false,
  REPLY_QUOTE: false,
  THREADS: false,
  TREE_ARTIFACT_ANCHOR: false,
};

export interface SkippedMarginItem {
  item: string;
  gate: keyof typeof GATES;
}
