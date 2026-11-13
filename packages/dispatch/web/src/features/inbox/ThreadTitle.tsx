import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { textMutedOnCanvas, textPrimaryOnCanvas, unreadDotBg } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { DeletedBody, deletedLineText, EditedMark } from "../refs/PostBody";
import { postDeletion } from "../refs/post-marks";
import { questionLine } from "./question-line";

/**
 * What a thread row is about as plain text, for every name a row's controls carry - its
 * checkbox's, its Dismiss's, its title's hover text: the first line of the thread's subject, or,
 * for a root its author deleted, whose subject the delete cleared, the deleted line's words
 * (`deletedLineText`). One reading, so every name a row carries says the same thing.
 */
export function useThreadLine(thread: MyReplyRow): string {
  const text = useAgentText();
  const deleted = postDeletion(thread);
  return deleted === null ? questionLine(thread.subject) : deletedLineText(deleted, text);
}

/**
 * What a thread row is about, in one line, bold with a dot while the thread holds a write the
 * reader has not read. A root its author deleted reads as the deleted line, and a root its
 * author edited carries ` · edited` after the line, outside the truncation that would cut it.
 *
 * Both layouts draw it: the short row beside the reading column, where it is the link `o`
 * follows (`data-inbox-owner`), and the card below that width, where **Open** is that link and
 * the title is plain text. Each host sizes its own line, as the three-site owner line does
 * (`ThreadOwnerLine` in `AskOwnerLink.tsx`); what unread looks like is this one place.
 */
export function ThreadTitle({
  className,
  href,
  thread,
}: {
  /** The classes the host's line gives the title; the unread weight is added to them. */
  className: string;
  /** Where the title links, for the host that draws it as a link; plain text without one. */
  href?: string;
  thread: MyReplyRow;
}): ReactNode {
  const title = useThreadLine(thread);
  const deleted = postDeletion(thread);
  const words = deleted === null ? title : <DeletedBody deleted={deleted} />;
  const text = `${className} ${
    thread.unread ? `font-semibold ${textPrimaryOnCanvas}` : textMutedOnCanvas
  }`;
  return (
    <>
      {href === undefined ? (
        <span className={text} title={title}>
          {words}
        </span>
      ) : (
        <a className={text} data-inbox-owner="" href={href} title={title}>
          {words}
        </a>
      )}
      <span className={`shrink-0 text-xs ${textMutedOnCanvas}`}>
        <EditedMark at={thread.edited_at} deleted={deleted} />
      </span>
      {thread.unread ? (
        <span
          aria-label="Unread"
          className={`size-2 shrink-0 rounded-full ${unreadDotBg}`}
          role="img"
        />
      ) : null}
    </>
  );
}
