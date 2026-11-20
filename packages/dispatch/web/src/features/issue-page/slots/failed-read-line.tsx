import { createContext, type ReactNode, useCallback, useContext, useState } from "react";

import { pageFailedText, pageHeaderControl } from "../../../theme/classes";
import { pageChrome } from "../page-structure";
import { CONTROL_LABELS } from "../page-words";

/** The Retry of a failed read's line, the one control the line draws. */
export const FAILED_READ_RETRY = "[data-failed-read] button";

/**
 * What a failed read's line calls as it leaves the page with the focus in it: the box the stage
 * draws its slot in (`StageBox`, `stage.tsx`) - a view's, in `ViewStage`, and the outline's -
 * puts the focus back in that view or that outline. Once the line is off the page the browser
 * has moved the focus to the page's body, where nothing says where it was, so the line says it
 * while it still holds it. Outside a stage there is none, and the focus goes with the line.
 */
export const FailedLineLeaves = createContext<(() => void) | undefined>(undefined);

export interface FailedReadLineProps {
  /** Which failure the line says: a read's `errorUpdateCount`. Where it is not given, the line
   *  counts the failures it sees itself: each run it saw end while it stayed drawn failed again. */
  failure?: number;
  /** What the view says of the read that failed, in the page's words. */
  message: string;
  /** Asks that read alone again. */
  onRetry: () => void;
  /** Whether the read runs again (TanStack Query's `isFetching`). */
  retrying: boolean;
}

/**
 * A read that failed before it first answered, as every view drawn in the stage and the outline
 * are to say it: one line in the page's words and its failed colour, with a Retry that asks that
 * read alone again. A view, and the outline, draw one per failed read, in the order they give
 * their reads, over their tree and never in it: an alert inside a tree would break the tree's
 * own structure of rows.
 *
 * Retry is never disabled, since Chromium moves the focus of a control it disables to the page's
 * body: while the read runs again it reads busy (`aria-busy`, and a quieter look), and a press
 * then asks nothing again, since the run under way already asks it and a second request would
 * only cancel that one and start it over. A view may keep the line drawn while its read runs
 * again, as the outline does, so each failure draws the alert afresh (`failure`), and a screen
 * reader says it again even in the same words; it keys the alert alone, so Retry stays the same
 * button, with its focus.
 *
 * When the line leaves the page with the focus in it, its read answered or out again, the box the
 * stage draws the view or the outline in (`StageBox`) puts the focus on the next failed read's
 * Retry, on the tree's tab-stop row where rows came, or on the tree itself while a read is out,
 * unless something else took the focus in that render.
 */
export function FailedReadLine({
  failure,
  message,
  onRetry,
  retrying,
}: FailedReadLineProps): ReactNode {
  const leaves = useContext(FailedLineLeaves);
  // The failures the line has seen while drawn, for a view that names none: a run it saw end left
  // the line drawn, so the read failed again.
  const [seen, setSeen] = useState({ failures: 0, retrying });
  let next = seen;
  if (seen.retrying !== retrying) {
    next = { failures: seen.retrying ? seen.failures + 1 : seen.failures, retrying };
    setSeen(next);
  }
  // One function for every render, so React runs its cleanup only when the line leaves the page.
  // React runs that cleanup before it takes the line off the page, while the focus is still in it.
  const line = useCallback(
    (element: HTMLDivElement | null) =>
      element === null
        ? undefined
        : () => {
            if (element.contains(document.activeElement)) leaves?.();
          },
    [leaves]
  );
  return (
    <div className="flex flex-wrap items-center gap-3 text-sm" data-failed-read="" ref={line}>
      <p className={pageFailedText} key={failure ?? next.failures} role="alert">
        {message}
      </p>
      <button
        aria-busy={retrying || undefined}
        className={`${pageHeaderControl} aria-busy:opacity-50`}
        onClick={() => {
          if (!retrying) onRetry();
        }}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.retry}
      </button>
    </div>
  );
}
