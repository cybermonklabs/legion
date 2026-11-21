import { useEffect, useState } from "react";

import { SEND_DEADLINE_MS } from "../writes/keyed-write";

/**
 * A hold on the controls of a component whose write has landed while the live update that
 * carries the write is still to be drawn. That update draws the component anew under a new key,
 * or takes it away, and the hold goes with it, so a control the reader pressed stays held until
 * then and the page's rescue reads its going as the press's doing (`causedByReader`). An update
 * that never comes ends the hold at the app's write deadline (`SEND_DEADLINE_MS`), so no control
 * is ever locked. Answers whether the hold is on, and what puts it on: called as the write lands,
 * never on a refusal, whose control comes back with its error at once.
 */
export function useHoldUntilDrawn(): readonly [held: boolean, hold: () => void] {
  const [held, setHeld] = useState(false);
  useEffect(() => {
    if (!held) return;
    const bound = window.setTimeout(() => setHeld(false), SEND_DEADLINE_MS);
    return () => window.clearTimeout(bound);
  }, [held]);
  return [held, () => setHeld(true)];
}
