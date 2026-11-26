import { type ReactNode, useId } from "react";

import type { MyReplyRow } from "../../api/types";
import { checkboxAccent, textMutedOnCanvas } from "../../theme/classes";
import { type InboxListRow, rowId } from "./list-row";
import { ThreadLine } from "./ThreadTitle";

/** The row's own checkbox, the pointer's way to the mark `x` makes: one group, **Mark**, the box
 *  and, with a `caption`, the word beside it, which never part. Every row takes one: with rows
 *  marked the bulk bar offers **Snooze** for the asks among them and **Dismiss** for the
 *  threads. A shift-click takes the range from the anchor to this row (`onMark`'s `range`). With
 *  a `caption` it is a 44 px label holding that word beside the box, as the reading view below
 *  1280 px draws it (`ReadingView`). On a short row (`pressSquare`), which opens on a press
 *  anywhere else, the bare box takes a press anywhere in a 44 px square around it: its label's
 *  padding reaches past the box only into the row's empty space - 20 px to the left of the row,
 *  the 8 px gap before the kind's icon, and 14 px above and below, where line two's inset and
 *  the gap between rows are - and negative margins of the same size give the padding back, so
 *  the label takes the box's 16 px in the row. It covers no other element and moves nothing,
 *  and at 44 px tall it already meets the floor `styles.css` sets for a checkbox's label below
 *  1280 px. It is positioned, so its padding is hit above line two, and the box inside it above
 *  that. */
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
  onMark: (id: string, range: boolean) => void;
  pressSquare?: boolean;
}): ReactNode {
  const inputId = useId();
  const box = (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      id={inputId}
      // React fires a checkbox's change from the browser's click on it, so the event under it is
      // that click, with its shift key: the pointer's, and the space bar's where the engine
      // passes it (Chromium does, Firefox does not).
      onChange={(event) =>
        onMark(id, event.nativeEvent instanceof MouseEvent && event.nativeEvent.shiftKey)
      }
      // On macOS, Safari and Firefox do not focus a checkbox the pointer clicks (MDN, "Clicking a
      // <button>", the focus table), so the shift-press puts the focus on it here: the range's
      // keys, the bar's return and the hold read the row that holds the focus. The press keeps
      // its default action: the three engines the browser suite runs select no text from it.
      onPointerDown={(event) => {
        if (event.shiftKey) event.currentTarget.focus({ preventScroll: true });
      }}
      type="checkbox"
    />
  );
  if (caption === undefined) {
    if (!pressSquare) return box;
    return (
      <label
        className="relative -my-3.5 -mr-2 -ml-5 flex shrink-0 py-3.5 pr-2 pl-5"
        htmlFor={inputId}
      >
        {box}
      </label>
    );
  }
  return (
    <label
      className={`inline-flex min-h-11 items-center gap-2 text-sm ${textMutedOnCanvas}`}
      htmlFor={inputId}
    >
      {box}
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
  onMark: (id: string, range: boolean) => void;
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
