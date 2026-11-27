import type { TreeAnswer } from "./tree-read";

/**
 * Where a comment stands among the pages of an outline read's comments: the position the read
 * writes, `<made_at>,<id>` (`comments_before`). This module is pure and imports no code of the
 * tree, so the tree's reads (`tree-nodes.ts`, `tree-read.ts`) and the decisions' review
 * (`decision-review.ts`) read the read's one format.
 */

/**
 * Where the next older comment page of an outline read's answer starts (`comments_before`,
 * `<made_at>,<id>` of the oldest comment the answer holds), which `?comments_before=` reads the
 * 200 comments older than; undefined where no older comment remains: null, left out, or empty, as
 * a view's answer, which answers none, leaves it.
 */
export function olderCommentsBefore(answer: TreeAnswer): string | undefined {
  const before = answer.comments_before;
  return typeof before === "string" && before !== "" ? before : undefined;
}

/** A comment position's `made_at` and id: the text before its first comma, and the rest. */
export function splitPosition(position: string): readonly [string, string] {
  const comma = position.indexOf(",");
  return comma < 0 ? [position, ""] : [position.slice(0, comma), position.slice(comma + 1)];
}
