import type { ReactNode } from "react";

import { textSecondaryOnSurface } from "../../theme/classes";

// The frame and the status line of the pending-writes indicator (`PendingWritesIndicator.tsx`),
// which the line that stands in the indicator's place while its chunk cannot load draws too
// (`ChangesNotSent`, `write-hosts.tsx`). That line stands where the indicator stands, so one
// restyle reaches both. The line is in the entry, so these are too; the indicator's chunk imports
// them from the entry at no cost.

/** The frame's place, shape and type, with no colour: each frame adds its own background and
 *  border. The line's border is the warning's at all times (`calloutWarningBorder`), which the
 *  indicator's is not. */
export const PENDING_WRITES_FRAME = "w-full max-w-md rounded-xl border px-3 py-2 text-xs shadow-lg";

/** The frame's status line, which says how many changes are out. Its role is `status`, so a
 *  screen reader says it when it changes, and it takes no focus. */
export function PendingWritesStatus({ children }: { children: ReactNode }): ReactNode {
  return (
    <p aria-live="polite" className={`font-medium ${textSecondaryOnSurface}`} role="status">
      {children}
    </p>
  );
}
