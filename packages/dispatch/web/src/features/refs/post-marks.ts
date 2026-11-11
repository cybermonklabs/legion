import type { Actor } from "../../api/types";

/**
 * The deletion marks every read of a comment or an issue message carries: `deleted_at` and
 * `deleted_by`, both null on a post nobody deleted. An event payload written before the post
 * corrections carries neither, so both are optional here and a payload reads the same way a
 * row does.
 */
export interface DeletablePost {
  readonly deleted_at?: string | null;
  readonly deleted_by?: Actor | null;
}

/** When a post's author deleted it, and who they are. The server records the actor on every
 *  delete; `by` is null only for a payload that carries the time alone. */
export interface PostDeletion {
  readonly at: string;
  readonly by: Actor | null;
}

/**
 * The post's deletion, or null for a post nobody deleted. One reading of the two fields, so
 * every surface marks the same posts.
 *
 * This module holds no component on purpose: the reference reads (`reference-target.ts`) answer
 * a deletion too, and they are what `MarkdownBody` resolves its references through, so a
 * component here would close an import cycle.
 */
export function postDeletion(post: DeletablePost | undefined): PostDeletion | null {
  const at = post?.deleted_at;
  if (at === undefined || at === null) {
    return null;
  }
  return { at, by: post?.deleted_by ?? null };
}
