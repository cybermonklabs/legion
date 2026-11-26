import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  focusVisibleRing,
  pageHairBorder,
  pageInkText,
  pageMutedText,
  pageQuietText,
  pageRowHoverBg,
  pageWaitingChip,
} from "../../../theme/classes";
import { Timestamp } from "../../refs/Timestamp";
import { authorText, pageChrome } from "../page-structure";
import { THREADS_WORDS } from "../page-words";
import type { RowText } from "./thread-row";

/**
 * One row of Threads (dispatch://INFRA-5704/spec#b-r-row, R7): one link to its record's address
 * (R12), which draws the six things R7 names and no other mark. What it says comes from the rules
 * of `thread-row.ts` and `thread-marks.ts`, which the list hands in: the first line is cut at a
 * word in script there, never by a style, so no `text-overflow` and no line clamp stands here.
 *
 * Its groups, in this order, each on a line of its own:
 * 1. The record's kind, its last activity and its mark, which wrap as words where the list is
 *    narrow.
 * 2. The record's first line, which wraps inside its own line and breaks a long word anywhere.
 * 3. Who is in it, each name a whole element, which wrap as names.
 *
 * Every word of the row is the page's (`THREADS_WORDS`), and every value - the agent a message
 * targets, the first line, a name, the count - stands inside `data-author-text`.
 */
export function ThreadRow({
  href,
  id,
  mark,
  text,
}: {
  /** The record's address, `/issues/<KEY>/threads/<id>`. */
  readonly href: string;
  /** The record's id, which the row carries as `data-thread-row`. */
  readonly id: string;
  /** The count of new writes (`recordMark`), or undefined for a row with no counted write. */
  readonly mark: string | undefined;
  /** What the row says of its record (`threadRow`). */
  readonly text: RowText;
}): ReactNode {
  return (
    <li className={`border-b ${pageHairBorder}`} data-thread-row={id}>
      {/* The link holds one flex item, so its lines stack whether the link is a block or, below
          1280 px, the flex box the floor makes every link (`styles.css`). */}
      <Link
        className={`flex w-full min-w-0 rounded-md px-2 py-2 ${pageRowHoverBg} ${focusVisibleRing}`}
        to={href}
      >
        <span className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-xs">
            <span className={`font-medium ${pageMutedText}`} data-thread-kind="" {...pageChrome}>
              <KindWords text={text} />
            </span>
            {/* The row's contract names the `time` that carries its last activity, and the page's
                `Timestamp` draws that time's words and keeps them current, so it stands inside
                the row's own `time`. */}
            <time className={pageQuietText} data-thread-activity="" dateTime={text.activity}>
              <Timestamp at={text.activity} />
            </time>
            {mark === undefined ? null : (
              <span
                className={`rounded-full border px-1.5 font-semibold tabular-nums ${pageWaitingChip}`}
                data-thread-mark=""
                {...authorText}
              >
                {mark}
              </span>
            )}
          </span>
          <span
            className={`text-sm ${pageInkText} [overflow-wrap:anywhere]`}
            data-thread-first-line=""
            {...authorText}
          >
            {text.firstLine}
          </span>
          <span className="flex min-w-0 flex-wrap gap-x-2 text-xs">
            {text.people.map((name) => (
              <span
                className={`${pageMutedText} [overflow-wrap:anywhere]`}
                data-thread-person=""
                key={name}
                {...authorText}
              >
                {name}
              </span>
            ))}
          </span>
        </span>
      </Link>
    </li>
  );
}

/** The words of a record's kind (R7), with the agent a message targets as a value of the page
 *  (`data-author-text`) after them, as a row and an opened record name it. */
export function KindWords({ text }: { readonly text: RowText }): ReactNode {
  return text.agent === undefined ? (
    text.kind
  ) : (
    <>
      {`${THREADS_WORDS.kinds.message_to} `}
      <span {...authorText}>{text.agent}</span>
    </>
  );
}
