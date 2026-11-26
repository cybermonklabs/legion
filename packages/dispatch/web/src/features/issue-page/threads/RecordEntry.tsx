import type { CSSProperties, ReactNode, Ref } from "react";

import type { CommentWithTrees, MessageWithTrees } from "../../../api/types";
import {
  focusVisibleRing,
  pageInkText,
  pageMutedText,
  pageQuietText,
} from "../../../theme/classes";
import { postDeletion } from "../../refs/post-marks";
import { Timestamp } from "../../refs/Timestamp";
import { textSourceOf } from "../../refs/text-references";
import type { DwellRef } from "../../shell/useDwell";
import { DeletedLine } from "../node-line";
import { authorText, pageChrome } from "../page-structure";
import { CONTROL_LABELS } from "../page-words";
import { RowText } from "../RowText";

/** Where an entry stands in its record: the outline's indent, 23 px a depth and 17 px on a
 *  phone (`NodeRow.tsx`), taken as a margin so each reply's box stands to the right of the entry
 *  it answers (R9). */
export const ENTRY_INDENT =
  "ml-[calc(var(--row-depth)*23px)] max-[761px]:ml-[calc(var(--row-depth)*17px)]";

/** The depth an element of a record stands at, for `ENTRY_INDENT`. */
export function depthStyle(depth: number): CSSProperties {
  return { "--row-depth": depth } as CSSProperties;
}

/** A control of an entry: Reply, Edit, and the ask's own Reply in its record. */
export const ENTRY_CONTROL = `rounded-md px-1 font-medium underline ${pageMutedText} ${focusVisibleRing}`;

/** The one pixel at the foot of the record's newest entry that the read mark watches
 *  (`useMarkThreadRead`), as the outline's `ThreadReadMark` does: an entry taller than the screen
 *  is never three-quarters on it, and its foot is. */
export function ReadMarkFoot({ markRef }: { readonly markRef: DwellRef }): ReactNode {
  return (
    <span
      aria-hidden="true"
      className="pointer-events-none block h-px"
      data-thread-read=""
      ref={markRef}
    />
  );
}

/**
 * One comment or message of an opened record (dispatch://INFRA-5704/spec#b-r-open, R9): its
 * author, its time and its whole words, drawn through the outline's renderer (`RowText`), then its
 * reply control and, on the reader's own comment, Edit. A message carries no Edit. A post its
 * author deleted draws the deleted line in its words' place and takes neither, as the server
 * refuses a reply to one and an edit of one. The entry stands at the indent of its depth, and the
 * entries of a record are siblings in document order (`OpenedRecord`).
 *
 * The author is named as a row of Threads names one (`nameOf`, root R9), and the words of the
 * entry are the author's, so each stands inside `data-author-text`. The entry an address focuses
 * (`focusRef`) takes the focus from a script alone, so it adds no tab stop (R12).
 */
export function RecordEntry({
  author,
  depth,
  edit,
  focusRef,
  kind,
  markRef,
  onEdit,
  onReply,
  post,
}: {
  /** The author's name, as the page names an author. */
  readonly author: string;
  readonly depth: number;
  /** The editor that stands in the words' place while the reader edits them. */
  readonly edit?: ReactNode;
  /** The entry's element, where the address the record opened at focuses it. */
  readonly focusRef?: Ref<HTMLLIElement>;
  readonly kind: "comment" | "message";
  /** The read mark's watch, on the record's newest entry. */
  readonly markRef?: DwellRef;
  /** Opens the reader's own comment to edit; none where the entry takes no edit. */
  readonly onEdit?: () => void;
  readonly onReply: () => void;
  readonly post: CommentWithTrees | MessageWithTrees;
}): ReactNode {
  const deleted = postDeletion(post);
  return (
    <li
      className={`min-w-0 space-y-1 ${ENTRY_INDENT} ${focusRef === undefined ? "" : `rounded-md ${focusVisibleRing}`}`}
      data-depth={depth}
      data-thread-entry={post.id}
      ref={focusRef}
      style={depthStyle(depth)}
      tabIndex={focusRef === undefined ? undefined : -1}
    >
      <p className={`flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs ${pageQuietText}`}>
        <span className={`font-medium ${pageMutedText} [overflow-wrap:anywhere]`} {...authorText}>
          {author}
        </span>
        <Timestamp at={post.created_at} />
      </p>
      {deleted !== null ? (
        <DeletedLine deleted={deleted} />
      ) : (
        (edit ?? (
          <div className={`text-sm ${pageInkText}`}>
            <RowText
              markdown={post.body}
              source={textSourceOf(kind, post.id, post.issue_key)}
              tabStops
              tree={post.body_tree}
            />
          </div>
        ))
      )}
      {deleted !== null || edit !== undefined ? null : (
        <p className="flex gap-2 text-xs">
          <button className={ENTRY_CONTROL} onClick={onReply} type="button" {...pageChrome}>
            {CONTROL_LABELS.reply}
          </button>
          {onEdit === undefined ? null : (
            <button className={ENTRY_CONTROL} onClick={onEdit} type="button" {...pageChrome}>
              {CONTROL_LABELS.edit}
            </button>
          )}
        </p>
      )}
      {markRef === undefined ? null : <ReadMarkFoot markRef={markRef} />}
    </li>
  );
}
