import { createContext, type ReactNode, useCallback, useContext } from "react";

import { QueryError } from "../../../components/QueryError";

/** The Retry of a failed read's line, the one control the line draws. */
export const FAILED_READ_RETRY = "[data-failed-read] button";

/**
 * What a failed read's line calls as it leaves the page with the focus in it: the stage the view
 * is drawn in (`ViewStage`, `stage.tsx`) puts the focus back in the view. Once the line is off the
 * page the browser has moved the focus to the page's body, where nothing says where it was, so
 * the line says it while it still holds it. Outside a stage there is none, and the focus goes
 * with the line.
 */
export const FailedLineLeaves = createContext<(() => void) | undefined>(undefined);

export interface FailedReadLineProps {
  /** What the view says of the read that failed, in the page's words. */
  message: string;
  /** Asks that read alone again. */
  onRetry: () => void;
  /** Whether the read runs again (TanStack Query's `isFetching`). */
  retrying: boolean;
}

/**
 * A read that failed before it first answered, as every view drawn in the stage is to say it: one
 * line in the page's words with a Retry that asks that read alone again
 * (`components/QueryError.tsx`). A view is to draw one per failed read, in the order it gives its
 * reads, over its tree and never in it: an alert inside a tree would break the tree's own
 * structure of rows.
 *
 * Retry stays enabled while the read runs again, since Chromium moves the focus of a control it
 * disables to the page's body. A press while the read runs asks nothing again: the run under way
 * already asks it, and a second request would only cancel that one and start it over.
 *
 * When the line leaves the page with the focus in it, its read answered or out again, the stage
 * puts the focus on the next failed read's Retry, on the tree's tab-stop row where rows came, or
 * on the tree itself while a read is out, unless something else took the focus in that render.
 */
export function FailedReadLine({ message, onRetry, retrying }: FailedReadLineProps): ReactNode {
  const leaves = useContext(FailedLineLeaves);
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
    <div data-failed-read="" ref={line}>
      <QueryError
        message={message}
        onRetry={() => {
          if (!retrying) onRetry();
        }}
      />
    </div>
  );
}
