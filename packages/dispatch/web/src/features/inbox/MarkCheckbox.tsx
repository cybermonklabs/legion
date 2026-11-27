import type { ReactNode } from "react";

import type { MyReplyRow } from "../../api/types";
import { checkboxAccent, textMutedOnCanvas } from "../../theme/classes";
import { type InboxListRow, rowId } from "./list-row";
import { ThreadLine } from "./ThreadTitle";

/** The row's own checkbox, the pointer's way to the mark `x` makes: one group, **Mark**, the box
 *  and, with a `caption`, the word beside it, which never part. Every row takes one: with rows
 *  marked the bulk bar offers **Snooze** for the asks among them and **Dismiss** for the
 *  threads. With a `caption` it is a 44 px label holding that word beside the box, as the
 *  reading view below 1280 px draws it (`ReadingView`). On a short row (`pressSquare`), which
 *  opens on a press anywhere else, the bare box takes a press anywhere in a 44 px square around
 *  it: its label's padding reaches past the box only into the row's empty space - 20 px to the
 *  left of the row, the 8 px gap before the kind's icon, and 14 px above and below, where line
 *  two's inset and the gap between rows are - and negative margins of the same size give the
 *  padding back, so the label takes the box's 16 px in the row. It covers no other element and
 *  moves nothing, and at 44 px tall it already meets the floor `styles.css` sets for a
 *  checkbox's label below 1280 px. It is positioned, so its padding is hit above line two, and
 *  the box inside it above that. */
export function MarkCheckbox({
  caption,
  id,
  marked,
  name,
  onMark,
  pressSquare = false,
}: {
  caption?: string;
  id: string;
  marked: boolean;
  name: string;
  onMark: (id: string) => void;
  pressSquare?: boolean;
}): ReactNode {
  const box = {
    "aria-label": `Select ${name}`,
    checked: marked,
    className: `size-4 shrink-0 ${checkboxAccent}`,
    onChange: () => onMark(id),
    type: "checkbox",
  } as const;
  if (caption === undefined) {
    if (!pressSquare) return <input {...box} />;
    return (
      <label className="relative -my-3.5 -mr-2 -ml-5 flex shrink-0 py-3.5 pr-2 pl-5">
        <input {...box} />
      </label>
    );
  }
  return (
    <label className={`inline-flex min-h-11 items-center gap-2 text-sm ${textMutedOnCanvas}`}>
      <input {...box} />
      <span aria-hidden="true">{caption}</span>
    </label>
  );
}

/** A thread row's own checkbox, named after the thread it marks. The name is an `aria-label`,
 *  which can hold no markup: it is the row's one plain-text line (`ThreadLine`) - the
 *  projection of the subject by the same reader the row's own line is drawn by, with the record
 *  that line names (`ThreadTitle`), never a second stripper, or a deleted root's deleted line.
 *  `caption` and `pressSquare` are `MarkCheckbox`'s. */
export function ThreadMarkCheckbox({
  caption,
  marked,
  onMark,
  pressSquare,
  row,
  thread,
}: {
  caption?: string;
  marked: boolean;
  onMark: (id: string) => void;
  pressSquare?: boolean;
  row: InboxListRow;
  thread: MyReplyRow;
}): ReactNode {
  return (
    <ThreadLine thread={thread}>
      {(line) => (
        <MarkCheckbox
          caption={caption}
          id={rowId(row)}
          marked={marked}
          name={line ?? ""}
          onMark={onMark}
          pressSquare={pressSquare}
        />
      )}
    </ThreadLine>
  );
}
