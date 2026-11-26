import type { ReactNode } from "react";

import { pageLineBorder, pageMutedText } from "../../../theme/classes";
import { authorText, pageChrome } from "../page-structure";
import { DASHBOARD_WORDS } from "../page-words";

/**
 * The control under a choice's rows while rows are left (dispatch://INFRA-5704/spec#b-r-view, R2):
 * it shows the next 20 and says how many are left, as "Show the next 20 · 31 left". It is this
 * part's own control, in the words a tile's list on Dashboard draws for the same act
 * (`DASHBOARD_WORDS`), and it draws nothing once no row is left. Paging is no fold: the rows it
 * shows stay drawn.
 *
 * A press asks the view for the next 20 rows, and the view reads a route's next page only then
 * (R3, R15). While those rows wait on a read the control reads busy, and a press asks for nothing
 * more, as a failed read's Retry does (`FailedReadLine`): the control is never disabled, so the
 * focus stays on it.
 */
export function ShowNext({
  busy,
  left,
  onShow,
}: {
  /** Whether the rows a press asked for wait on a read. */
  readonly busy: boolean;
  /** The number the control says is left. */
  readonly left: number;
  /** Asks for the next 20 rows. */
  readonly onShow: () => void;
}): ReactNode {
  if (left <= 0) return null;
  return (
    <button
      aria-busy={busy || undefined}
      className={`self-start rounded-lg border px-3 py-1 text-sm font-medium aria-busy:opacity-50 ${pageLineBorder} ${pageMutedText}`}
      onClick={() => {
        if (!busy) onShow();
      }}
      type="button"
      {...pageChrome}
    >
      {/* One span holds the words, so the spaces around the number stay where the floor makes
          every button a flex box below 1280 px (`styles.css`). */}
      <span>
        {`${DASHBOARD_WORDS.next_twenty} · `}
        <span {...authorText}>{left}</span>
        {` ${DASHBOARD_WORDS.left}`}
      </span>
    </button>
  );
}
