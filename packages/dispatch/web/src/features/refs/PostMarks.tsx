import { type ReactNode, useEffect } from "react";

import { textMutedOnSurface } from "../../theme/classes";
import { AgentName } from "./AgentName";
import { POST_MARK_WORDS, type PostDeletion } from "./post-marks";
import { Timestamp } from "./Timestamp";

// The marks a post carries that draw none of its text. They live apart from `PostBody.tsx`,
// which imports `MarkdownBody`, because `MarkdownBody` draws a reference-only body's cards
// (`Unfurl.tsx`), and those cards mark a post too: here, the cards import no renderer back.

/**
 * What a reader meets where a deleted post's body was: who deleted it and when, and no part of
 * the text. The server clears the body on a delete, so a surface that drew the body alone would
 * draw an empty card; this line stands in its place on every surface, and the post keeps its
 * seat in the thread.
 *
 * The author's name is static here: a preview sits inside the host's own control, so a link
 * inside it would be a link inside a link. A host that names the actors it draws by a rule of its
 * own passes the name that rule gives (`actorName`), and the line draws it in place of
 * `AgentName`: the new issue page's rows name the actor as the page names an author
 * (`AgentText.author`), by INFRA-1196's decision 342, so a session nothing names reads as the
 * page's word for an agent and never as its short id. A host that divides its own words from its
 * authors' passes the attributes it marks each with (`marks`): the line's words are the host's,
 * and the actor's name and the time are not.
 */
export function DeletedBody({
  actorName,
  className = "",
  deleted,
  marks,
  onRendered,
}: {
  /** The actor's name as the host names it, where the host names actors by its own rule; absent
   *  everywhere else, where `AgentName` names the actor. */
  actorName?: string;
  className?: string;
  deleted: PostDeletion;
  /** The attributes a host marks its own words with (`own`), which the line carries, and those
   *  it marks words it did not write with (`theirs`), which the actor's name and the time carry
   *  (the issue page's `pageChrome` and `authorText`). */
  marks?: {
    readonly own: Readonly<Record<string, string>>;
    readonly theirs: Readonly<Record<string, string>>;
  };
  /** Called once the line is on the page, as `MarkdownBody` calls it once a body is. */
  onRendered?: () => void;
}): ReactNode {
  useEffect(() => onRendered?.(), [onRendered]);
  const actor =
    deleted.by === null
      ? undefined
      : (actorName ?? <AgentName actor={deleted.by} inline kind="static" />);
  const time = <Timestamp at={deleted.at} />;
  return (
    <span
      className={`italic ${textMutedOnSurface} ${className}`}
      data-post-deleted=""
      {...marks?.own}
    >
      {actor === undefined ? (
        POST_MARK_WORDS.deleted
      ) : (
        <>
          {POST_MARK_WORDS.deleted_by}{" "}
          {marks === undefined ? actor : <span {...marks.theirs}>{actor}</span>}
        </>
      )}{" "}
      · {marks === undefined ? time : <span {...marks.theirs}>{time}</span>}
    </span>
  );
}

/**
 * The ` · edited` a post's byline carries once its author replaced its body, which the margin's
 * comment card has shown since before this change and which every surface that prints an issue
 * message now shows too. Nothing at all for a post nobody edited, and nothing for a deleted
 * one: the deleted line already says what became of the text.
 *
 * A host that sets the mark apart in an element of its own passes that element's classes as
 * `className`, or the attributes it marks its own words with as `marks` (the issue page's
 * `pageChrome`), and the mark draws the element with itself, so a post with no mark draws no
 * element either. An empty element in a flex row still takes the row's gap, which cut every
 * unedited Inbox title one glyph short.
 */
export function EditedMark({
  at,
  className,
  deleted,
  marks,
}: {
  at: string | null | undefined;
  className?: string;
  deleted?: PostDeletion | null;
  marks?: Readonly<Record<string, string>>;
}): ReactNode {
  if (at === null || at === undefined || (deleted ?? null) !== null) return null;
  const mark = ` · ${POST_MARK_WORDS.edited}`;
  return className === undefined && marks === undefined ? (
    mark
  ) : (
    <span className={className} {...marks}>
      {mark}
    </span>
  );
}
