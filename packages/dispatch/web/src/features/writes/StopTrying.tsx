import type { ReactNode } from "react";

import { RefusableButton } from "../../components/RefusableButton";

/** Why Stop trying takes no press once pressed: what a screen reader reads beside it, and its
 *  title where the surface raises tooltips. */
const STOPPING_REASON = "The attempt already sent is still out.";

/** Stop trying, for a late write - a composer's send, a broadcast, or any other write - busy while
 *  the reader has stopped it and its last attempt is still out (`stopping`). */
export function StopTrying({
  onStop,
  stopping,
}: {
  readonly onStop: () => void;
  readonly stopping: boolean;
}): ReactNode {
  if (stopping) {
    // Refused rather than `busy`, which would render it `disabled`: Chromium moves the focus of a
    // control that turns disabled to the page's body, which would lose a reader who pressed it by
    // keyboard for as long as the last attempt is out. A refusal keeps it in the tab order, reads
    // the same and takes no press (`RefusableButton`).
    return (
      <RefusableButton look="secondary-compact" refusal={STOPPING_REASON}>
        Stopping…
      </RefusableButton>
    );
  }
  return (
    <RefusableButton look="secondary-compact" onPress={onStop} refusal={null}>
      Stop trying
    </RefusableButton>
  );
}
