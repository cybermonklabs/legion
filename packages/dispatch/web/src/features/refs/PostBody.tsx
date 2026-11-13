import { type ComponentProps, type ReactNode, useEffect } from "react";

import { textMutedOnSurface } from "../../theme/classes";
import { AgentName, type AgentText } from "./AgentName";
import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import type { PostDeletion } from "./post-marks";
import { Timestamp } from "./Timestamp";

/**
 * What a reader meets where a deleted post's body was: who deleted it and when, and no part of
 * the text. The server clears the body on a delete, so a surface that drew the body alone would
 * draw an empty card; this line stands in its place on every surface, and the post keeps its
 * seat in the thread.
 *
 * The author's name is static here: a preview sits inside the host's own control, so a link
 * inside it would be a link inside a link.
 */
export function DeletedBody({
  className = "",
  deleted,
  onRendered,
}: {
  className?: string;
  deleted: PostDeletion;
  /** Called once the line is on the page, as `MarkdownBody` calls it once a body is. */
  onRendered?: () => void;
}): ReactNode {
  useEffect(() => onRendered?.(), [onRendered]);
  return (
    <span className={`italic ${textMutedOnSurface} ${className}`} data-post-deleted="">
      {deleted.by === null ? (
        "Deleted"
      ) : (
        <>
          Deleted by <AgentName actor={deleted.by} inline kind="static" />
        </>
      )}{" "}
      · <Timestamp at={deleted.at} />
    </span>
  );
}

/** The deleted line's words as plain text, for a place that holds only a string - an
 *  `aria-label`, a `title`, a checkbox's name: `Deleted by <name>` by the rule `DeletedBody` names
 *  the author by (`useAgentText`), or `Deleted` for a payload that names nobody. The time stays
 *  the visible line's. */
export function deletedLineText(deleted: PostDeletion, text: AgentText): string {
  return deleted.by === null ? "Deleted" : `Deleted by ${text.label(deleted.by)}`;
}

/**
 * One post's body: the Markdown its author wrote, or the deleted line in its place. Every
 * surface that prints a comment's or an issue message's whole body goes through here rather
 * than through `MarkdownBody` itself, so the mark is written once.
 */
export function PostBody({
  deleted,
  ...body
}: ComponentProps<typeof MarkdownBody> & { deleted: PostDeletion | null }): ReactNode {
  return deleted === null ? (
    <MarkdownBody {...body} />
  ) : (
    <DeletedBody deleted={deleted} onRendered={body.onRendered} />
  );
}

/** `PostBody` for the surfaces that clamp one rendered line of a post (`MarkdownPreview`): a
 *  hover card, an unfurl card, a reply quote, the margin's collapsed root. */
export function PostPreview({
  deleted,
  ...preview
}: ComponentProps<typeof MarkdownPreview> & { deleted: PostDeletion | null }): ReactNode {
  return deleted === null ? (
    <MarkdownPreview {...preview} />
  ) : (
    <DeletedBody className={preview.className} deleted={deleted} />
  );
}

/**
 * The ` · edited` a post's byline carries once its author replaced its body, which the margin's
 * comment card has shown since before this change and which every surface that prints an issue
 * message now shows too. Nothing at all for a post nobody edited, and nothing for a deleted
 * one: the deleted line already says what became of the text.
 */
export function EditedMark({
  at,
  deleted,
}: {
  at: string | null | undefined;
  deleted?: PostDeletion | null;
}): ReactNode {
  return at === null || at === undefined || (deleted ?? null) !== null ? null : <> · edited</>;
}
