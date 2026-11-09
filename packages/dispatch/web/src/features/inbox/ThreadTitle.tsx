import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { textMutedOnCanvas, textPrimaryOnCanvas, unreadDotBg } from "../../theme/classes";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { ReferenceHostIs } from "../refs/reference-host";
import { textSourceKind, textSourceOf } from "../refs/text-references";

/**
 * What a thread row is about, in one line, bold with a dot while the thread holds a write the
 * reader has not read.
 *
 * The subject is the thread root's own markdown, so it is drawn by the one renderer
 * (`MarkdownPreview`, clamped to one line): a heading, a bold word and a reference read here as
 * they read in the thread itself. `fullTitle` gives a pointer reader the same line as plain
 * text, from the same reader.
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
  const text = `${className} ${
    thread.unread ? `font-semibold ${textPrimaryOnCanvas}` : textMutedOnCanvas
  }`;
  const line = (
    <MarkdownPreview
      className={text}
      fullTitle
      lines={1}
      markdown={thread.subject}
      source={textSourceOf(textSourceKind(thread.kind), thread.thread_id)}
    />
  );
  return (
    <>
      {href === undefined ? (
        line
      ) : (
        // The title is the link the row's `o` follows, so a reference in the subject reads as a
        // link inside it and is none.
        <ReferenceHostIs host="control">
          <a className="min-w-0" data-inbox-owner="" href={href}>
            {line}
          </a>
        </ReferenceHostIs>
      )}
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
