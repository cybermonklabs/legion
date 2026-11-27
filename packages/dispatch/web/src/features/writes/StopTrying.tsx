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
    // Refused rather than `busy`, so that `STOPPING_REASON` reaches a screen reader: a refusal
    // describes the button by a hidden line of its own that holds the reason (`aria-describedby`),
    // and is its title where the surface raises tooltips, where `busy` only changes the label
    // (`RefusableButton`). Either is `aria-disabled`, never `disabled`, so the button keeps its
    // focus and its place in the tab order, and takes no press.
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
