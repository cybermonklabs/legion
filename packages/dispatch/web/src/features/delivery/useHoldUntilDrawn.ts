import { useEffect, useLayoutEffect, useRef, useState } from "react";

import { holdHandOff, type WritePress } from "../../components/focus-place";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";

/**
 * A hold on the controls of a component whose write has landed while the update that carries
 * the write is still to be drawn. `version` is the write time of the record the component draws.
 * The hold is on while `version` is still the one of the render whose `hold` was called. The
 * update that carries the write brings a new version, or takes the component away, and the hold
 * ends then. So a control the reader pressed stays held until that update. The hold keeps the
 * armed hand-off of the reader's press that made the write live (`holdHandOff`) from the call of
 * `hold` until the commit that ends it, so a loss that update makes is still the press's doing
 * (`causedByReader`). `hold` is handed that press, the one the write began under
 * (`pressOfWrite`), since it is called as the write lands, by when the reader may have pressed
 * again: a later press is no doing of this write's. No component's hand-off answers for the
 * controls of either caller - the Delivery page's settings form, which the timeline replaces,
 * and the Session groups editor, a dialog of its own - so a control that update takes away leaves
 * the focus where the browser leaves it. An update that never comes ends the hold at the app's
 * write deadline (`SEND_DEADLINE_MS`), counted from the latest `hold`, so no control is ever
 * locked. Answers whether the hold is on, and what puts it on: called as the write lands, never on
 * a refusal, whose control comes back with its error at once. A `hold` of a render whose version a
 * later commit has already replaced, or of a component no longer mounted, puts on no hold: the
 * update it would wait for is already drawn, or nothing is left to hold.
 */
export function useHoldUntilDrawn(
  version: string | null
): readonly [held: boolean, hold: (press: WritePress) => void] {
  // The version the hold is on, a new object at every `hold`, so the deadline counts from the
  // latest one.
  const [heldOn, setHeldOn] = useState<{ readonly version: string | null } | null>(null);
  // The hold's part in the press's action, and the version it was begun on. It is kept apart
  // from `heldOn`, so it ends even where the state that put the hold on never commits: the
  // version changed in the same batch, or the component went.
  const open = useRef<{ readonly answered: () => void; readonly version: string | null } | null>(
    null
  );
  // The version the latest commit drew, undefined while the component is not mounted.
  const drawn = useRef<string | null | undefined>(undefined);
  useLayoutEffect(() => {
    drawn.current = version;
  }, [version]);
  useLayoutEffect(
    () => () => {
      drawn.current = undefined;
      open.current?.answered();
      open.current = null;
    },
    []
  );
  // A new version ends the hold. It is let go during render, so the effect's cleanup clears the
  // hold's timer then, and the timer does not run on to its end.
  if (heldOn !== null && heldOn.version !== version) setHeldOn(null);
  useEffect(() => {
    if (heldOn === null) return;
    const bound = window.setTimeout(() => setHeldOn(null), SEND_DEADLINE_MS);
    return () => window.clearTimeout(bound);
  }, [heldOn]);
  // The hold's part in the press's action ends in the commit that lets the hold go, at a new
  // version or at the deadline, and in the commit that draws a version other than the one it was
  // begun on.
  useEffect(() => {
    const begun = open.current;
    if (begun === null || (heldOn !== null && begun.version === version)) return;
    open.current = null;
    begun.answered();
  }, [heldOn, version]);
  return [
    heldOn !== null && heldOn.version === version,
    // Begun at the call, as the write lands, on the press the write began under, rather than in
    // the commit that puts the hold on, which comes a task or more later, by when that press could
    // have ended.
    (press) => {
      if (drawn.current !== version) return;
      open.current?.answered();
      open.current = { answered: holdHandOff(press), version };
      setHeldOn({ version });
    },
  ];
}
