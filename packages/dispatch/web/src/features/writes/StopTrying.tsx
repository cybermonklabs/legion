import type { ReactNode } from "react";

import { RefusableButton } from "../../components/RefusableButton";

/** Why Stop trying takes no press once pressed: what a screen reader reads beside it, and its
 *  title where the surface raises tooltips. */
const STOPPING_REASON = "The attempt already sent is still out.";

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
    // Refused, so the button says why it takes no press: a screen reader reads the reason beside
    // it. A refused button stays in the tab order and keeps the focus of a reader who pressed it
    // by keyboard (`RefusableButton`).
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
