import type { Actor } from "../../api/types";
import type { AgentText } from "./AgentName";

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

/**
 * The words every surface marks a corrected post with: the deleted line's opening, `Deleted` or
 * `Deleted by` before the actor (`DeletedBody`, `deletedLineText`), and the word after an edited
 * post's byline or name (`EditedMark`, `editedName`). One list, so the issue page declares these
 * among its own words (`page-words.ts`) as every surface draws them.
 */
export const POST_MARK_WORDS = Object.freeze({
  deleted: "Deleted",
  deleted_by: "Deleted by",
  edited: "edited",
} as const);

/** The deleted line's words as plain text, for a place that holds only a string: `Deleted by
 *  <name>`, the actor named by `text.label` (`useAgentText`'s, or `actorLabelText` where no
 *  registry is read), or `Deleted` for a payload that names nobody. The time stays the visible
 *  line's. It lives here rather than beside `DeletedBody` because `PostBody.tsx` imports
 *  `MarkdownBody`, whose reference links draw these words too. */
export function deletedLineText(deleted: PostDeletion, text: Pick<AgentText, "label">): string {
  return deleted.by === null
    ? POST_MARK_WORDS.deleted
    : `${POST_MARK_WORDS.deleted_by} ${text.label(deleted.by)}`;
}

/** The name of a post its author edited - a comment, a message or a block of either - as a link
 *  to it writes the name: `words` then ` · edited`, the mark the post's byline writes
 *  (`EditedMark`). `words` as they are for a post nobody edited (`edited` false), and `""` for no
 *  words, which take no mark. A place that cuts a name to a length cuts `words` and marks what
 *  the cut left, so no cut takes the mark. Every place that writes a marked name as one string
 *  marks it here; one that draws the mark as an element of its own, as the issue page's chip
 *  does, draws `EditedMark`. Both write `POST_MARK_WORDS.edited`, so no two can drift apart. */
export function editedName(words: string, edited: boolean): string {
  return !edited || words === "" ? words : `${words} · ${POST_MARK_WORDS.edited}`;
}
