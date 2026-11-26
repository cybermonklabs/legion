import type { ReactNode } from "react";

import { RefusableButton } from "../../components/RefusableButton";
import { STOP_TRYING_LABEL, STOPPING_LABEL, STOPPING_REASON } from "./write-words";

/** Stop trying, for a late write - a composer's send, a broadcast, or any other write. Once the
 *  reader has stopped it, and while its last attempt is still out (`stopping`), it reads Stopping…
 *  and is refused (`aria-disabled`): it takes no press and keeps its place in the tab order. */
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
        {STOPPING_LABEL}
      </RefusableButton>
    );
  }
  return (
    <RefusableButton look="secondary-compact" onPress={onStop} refusal={null}>
      {STOP_TRYING_LABEL}
    </RefusableButton>
  );
}
