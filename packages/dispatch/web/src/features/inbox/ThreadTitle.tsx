import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { textMutedOnCanvas, unreadDotBg } from "../../theme/classes";
import { textPrimaryOnCanvas } from "../../theme/shell-classes";
import { useAgentText } from "../refs/AgentName";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { EditedMark } from "../refs/PostMarks";
import { PlainText } from "../refs/plain-text";
import { deletedLineText, postDeletion } from "../refs/post-marks";
import { ReferenceHostIs } from "../refs/reference-host";
import { textSourceKind, textSourceOf } from "../refs/text-references";

/**
 * What a thread row is about as plain text, for every name a row's controls carry - its
 * checkbox's, its Dismiss's: the one plain-text projection of the thread's subject, from the
 * same reader that draws the row's own line and with the record that line names
 * (`ThreadTitle`), or, for a root its author deleted, whose subject the delete cleared, the
 * deleted line's words (`deletedLineText`). One reading, so every name a row carries says the
 * same thing. A name holds no markup, so `children` takes the line as words: undefined until
 * the projection has answered.
 */
export function ThreadLine({
  children,
  thread,
}: {
  children: (line: string | undefined) => ReactNode;
  thread: MyReplyRow;
}): ReactNode {
  const text = useAgentText();
  const deleted = postDeletion(thread);
  return deleted === null ? (
    <PlainText
      markdown={thread.subject}
      source={textSourceOf(textSourceKind(thread.kind), thread.thread_id, thread.issue?.key)}
      surface="inbox-thread-title"
      tree={thread.subject_tree}
    >
      {children}
    </PlainText>
  ) : (
    children(deletedLineText(deleted, text))
  );
}

/**
 * What a thread row is about, in one line, bold with a dot while the thread holds a write the
 * reader has not read. A root its author deleted reads as the deleted line, and a root its
 * author edited carries ` · edited` after the line, outside the truncation that would cut it.
 * The deleted line here is its words alone (`deletedLineText`), in the deleted line's own italic,
 * with no time: the row already shows its own age, and that age is the one time a row holds, the
 * part of a short row that is neither the owner link nor a control, where the pointer selects
 * the row (`selectRow` in the browser suite).
 *
 * The subject is the thread root's own markdown, so it is drawn by the one renderer
 * (`MarkdownPreview`, clamped to one line): a heading, a bold word and a reference read here as
 * they read in the thread itself. `fullTitle` gives a pointer reader the same line as plain
 * text, from the same reader; a deleted root's hover text is the deleted line's words.
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
  /** The classes the host's line gives the title; the unread weight is added to them. The host
   *  cuts the line (`truncate`), as it did when it drew the subject as words, so the preview is
   *  drawn as the host's inline text (`inline`), and the ellipsis falls where it did. */
  className: string;
  /** Where the title links, for the host that draws it as a link; plain text without one. */
  href?: string;
  thread: MyReplyRow;
}): ReactNode {
  const agentText = useAgentText();
  const deleted = postDeletion(thread);
  const deletedWords = deleted === null ? undefined : deletedLineText(deleted, agentText);
  const text = `${className} ${
    thread.unread ? `font-semibold ${textPrimaryOnCanvas}` : textMutedOnCanvas
  }`;
  const line =
    deletedWords === undefined ? (
      <MarkdownPreview
        className="inline"
        fullTitle
        lines={1}
        markdown={thread.subject}
        source={textSourceOf(textSourceKind(thread.kind), thread.thread_id, thread.issue?.key)}
        surface="inbox-thread-title"
        tree={thread.subject_tree}
      />
    ) : (
      <span className="italic" data-post-deleted="">
        {deletedWords}
      </span>
    );
  return (
    <>
      {href === undefined ? (
        <span className={text} title={deletedWords}>
          {line}
        </span>
      ) : (
        // The title is the link the row's `o` follows, so a reference in the subject reads as a
        // link inside it and is none. The link keeps the row's own classes, the weight that
        // says the thread is unread among them, as it did when it drew the words itself.
        <ReferenceHostIs host="control">
          <a className={text} data-inbox-owner="" href={href} title={deletedWords}>
            {line}
          </a>
        </ReferenceHostIs>
      )}
      <EditedMark
        at={thread.edited_at}
        className={`shrink-0 text-xs ${textMutedOnCanvas}`}
        deleted={deleted}
      />
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
