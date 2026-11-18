import { type CSSProperties, type KeyboardEvent, type ReactNode, useState } from "react";

import {
  pageFocusedRow,
  pageGuideLineBg,
  pageMutedText,
  pageQuietText,
} from "../../../theme/classes";
import { pageChrome } from "../page-structure";
import { CONTROL_LABELS } from "../page-words";
import type { OvertakenRow } from "../tree-fold";

/**
 * The one line a node's overtaken children fold to, after its other children and at their level
 * (`tree-fold.ts`'s `OvertakenRow`): how many it holds directly, in one text node ("1 overtaken",
 * "2 overtaken"), under the guides of the levels above it, as `TreeRows.tsx`'s line of a deeper
 * branch draws them.
 *
 * Unlike that line it takes the focus as a row does, a `treeitem` with `aria-expanded`, and it
 * is no `[data-branch-more]` and holds no button, so nothing that opens every deeper branch or
 * every fold opens it: the reader does. A press opens or closes it in place, and so do the keys
 * `useTreeKeymap.ts` binds on it (Enter, and the arrows). It stands for no node, so its focus
 * focuses none: Details goes on drawing what it drew, as it does for a deeper branch's line.
 * Its focus makes it the tree's one tab stop, as a row's does (`onStop`), so a key pressed on it
 * that answers in words, which no row holds the focus to draw, is drawn at it (`report`); and the
 * row the focus left lets go of its body, its focused look and its acts. While the line holds the
 * focus it looks as a focused row does (`pageFocusedRow`), in place of the browser's outline.
 */
export function OvertakenLine({
  onOvertaken,
  onStop,
  report,
  row,
  tabStop,
}: {
  /** Opens or closes the line, by the id of the node whose overtaken children it folds. */
  onOvertaken?: (id: string, open: boolean) => void;
  /** Makes the line the tree's one tab stop and lets go of the row the focus left, which its
   *  focus does and nothing else: it opens no row's body and focuses no node (`IssueTree.tsx`'s
   *  `stop`). It is handed the line's own element, which a press is checked against. */
  onStop?: (line: HTMLElement) => void;
  /** What a key pressed on the line answers, where it answers in words: drawn at the line,
   *  last, as a row draws it (`NodeRow`'s `report`). */
  report?: string;
  row: OvertakenRow;
  /** Whether the line is the tree's one tab stop. */
  tabStop: boolean;
}): ReactNode {
  const depth = row.level - 1;
  // Whether the line holds the focus, which draws it with a focused row's look.
  const [focused, setFocused] = useState(false);
  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: the tree's keymap opens and closes it from the keyboard (`useTreeKeymap.ts`'s Enter and arrows), as it moves through every row.
    <div
      aria-expanded={row.open}
      aria-level={row.level}
      className={`relative flex min-w-0 cursor-pointer flex-col pl-[calc(var(--row-depth)*23px+26px)] text-xs leading-7 outline-none max-[761px]:pl-[calc(var(--row-depth)*17px+22px)] ${pageQuietText} ${focused ? pageFocusedRow : ""}`}
      data-overtaken-line=""
      onBlur={() => setFocused(false)}
      onClick={() => onOvertaken?.(row.of.id, !row.open)}
      onFocus={(event) => {
        setFocused(true);
        onStop?.(event.currentTarget);
      }}
      role="treeitem"
      style={{ "--row-depth": depth } as CSSProperties}
      tabIndex={tabStop ? 0 : -1}
    >
      {Array.from({ length: depth }, (_unused, guide) => (
        <span
          aria-hidden="true"
          className={`absolute inset-y-0 w-px left-[calc(var(--guide)*23px+9px)] max-[761px]:left-[calc(var(--guide)*17px+8px)] ${pageGuideLineBg}`}
          // biome-ignore lint/suspicious/noArrayIndexKey: a guide is its level and nothing else.
          key={guide}
          style={{ "--guide": guide } as CSSProperties}
        />
      ))}
      <span className="font-medium" {...pageChrome}>
        {overtakenCount(row.held)}
      </span>
      {report === undefined ? null : (
        <p
          className={`text-xs leading-5 ${pageMutedText}`}
          data-row-report=""
          role="status"
          {...pageChrome}
        >
          {report}
        </p>
      )}
    </div>
  );
}

/** What a line of overtaken nodes says: how many it holds, in one text node ("1 overtaken"). */
export function overtakenCount(held: number): string {
  return `${held} ${CONTROL_LABELS.overtaken_count}`;
}

/** Whether a key pressed on a view's own line of overtaken nodes, which is `open` or closed,
 *  opens or closes it: Enter and the right arrow, and the left arrow on an open one, pressed on
 *  the line itself, since a key on a row it holds is that row's own, and with no Ctrl, Alt or
 *  Meta, since such a key is the browser's or another binding's. */
export function togglesLine(event: KeyboardEvent<HTMLElement>, open: boolean): boolean {
  if (event.target !== event.currentTarget) return false;
  if (event.altKey || event.ctrlKey || event.metaKey) return false;
  return event.key === "Enter" || event.key === "ArrowRight" || (event.key === "ArrowLeft" && open);
}
