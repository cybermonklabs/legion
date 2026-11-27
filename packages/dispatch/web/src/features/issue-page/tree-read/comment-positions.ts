import type { TreeAnswer } from "./tree-read";

/**
 * Where an outline read's answer stands among the pages of its read: where a comment stands among
 * the pages of its comments, the position the read writes, `<made_at>,<id>` (`comments_before`),
 * and the cursor of the page after it (`cursor`). This module is pure and imports no code of the
 * tree, so the tree's reads (`tree-nodes.ts`, `tree-read.ts`), the decisions' review
 * (`decision-review.ts`) and the e2e helpers read the read's one format.
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

/**
 * The cursor of the page after `page` in its read, or undefined where `page` is the last: one
 * whose cursor is null, left out of the body, or empty, as a server that drops an empty field
 * writes it. The one rule every walk of the read ends by: the page's own (`tree-read.ts`'
 * `readTree`) and the e2e helpers', and the one rule by which a decision's Change may stand on a
 * page the read has not loaded (`decision-review.ts`' `changeMayBeUnloaded`).
 */
export function nextTreeCursor(page: TreeAnswer): string | undefined {
  const { cursor } = page;
  return typeof cursor === "string" && cursor !== "" ? cursor : undefined;
}

/** A comment position's `made_at` and id: the text before its first comma, and the rest. */
export function splitPosition(position: string): readonly [string, string] {
  const comma = position.indexOf(",");
  return comma < 0 ? [position, ""] : [position.slice(0, comma), position.slice(comma + 1)];
}
