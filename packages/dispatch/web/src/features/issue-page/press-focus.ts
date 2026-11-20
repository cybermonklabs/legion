import { useEffect, useMemo, useRef } from "react";

/** What ends a press: its mouseup, which the click follows in the same task; the drag a press
 *  on a picture or a link starts instead, which sends no mouseup; a pointer the browser
 *  cancelled; and a key, since a press released outside the window sends no mouseup, and a
 *  key's move must never wait on a press that is over. */
const PRESS_ENDS = ["mouseup", "dragend", "pointercancel", "keydown"] as const;

/** A press as `trackPress` follows it. */
export interface Press {
  /** Whether a press is under way: from its start until the task after it ends. */
  readonly pressing: boolean;
  /** Starts a press: what each of its start events does, and what a caller whose press starts
   *  some other way calls - from a React handler, or from a listener that notes what the press
   *  went down on. */
  readonly start: () => void;
}

/**
 * Follows a press from its start until the task after it ends (`PRESS_ENDS`), after its click,
 * and calls `onEnd` then. A press starts at each event of `starts`, read on the window in the
 * capture phase, and at each call of `start`; one that starts while the press before it is
 * still ending puts that end off to its own. Aborting `signal` stops the listening and drops an
 * end not called yet.
 *
 * Each caller says what starts its press, since each reads a press for its own end: the outline's
 * press on a row, from the tree's own `mousedown` (`usePressFocus`); a press anywhere on the
 * page, from the `mousedown` whose default moves the focus (`IssueTree.tsx`'s
 * `useReleaseFocus`); and the presses around a row being edited (`useNodeEditing.ts`).
 */
export function trackPress({
  onEnd,
  signal,
  starts = [],
}: {
  readonly onEnd: () => void;
  readonly signal: AbortSignal;
  readonly starts?: readonly ("mousedown" | "pointerdown")[];
}): Press {
  let pressing = false;
  let ending: number | undefined;
  const start = () => {
    window.clearTimeout(ending);
    pressing = true;
  };
  const ended = () => {
    if (!pressing) return;
    window.clearTimeout(ending);
    ending = window.setTimeout(() => {
      pressing = false;
      onEnd();
    }, 0);
  };
  const options = { capture: true, signal };
  for (const type of starts) window.addEventListener(type, start, options);
  for (const type of PRESS_ENDS) window.addEventListener(type, ended, options);
  signal.addEventListener("abort", () => window.clearTimeout(ending), { once: true });
  return {
    get pressing() {
      return pressing;
    },
    start,
  };
}

/** What `usePressFocus` answers: what a row calls as it takes the focus, and the `mousedown`
 *  handler of the element the rows are drawn in. */
export interface PressFocus<T> {
  /** Takes the focus for `item` at once, or once the press under way has ended. */
  readonly focus: (item: T) => void;
  /** Marks a press in the rows' element as under way. */
  readonly onMouseDown: () => void;
}

/**
 * The focus a press gives a row, taken once the press is over: the outline's press
 * (`IssueTree.tsx`), and the Decisions view's list, which is to take its rows' focus this way
 * too.
 *
 * A press focuses what it lands on as it goes down, before it ends, so the row taking the focus
 * would open its body and draw its acts, and the row the focus left would close its own, while
 * the press is held: the rows would grow and shrink under the pointer. `NodeRow.tsx`'s
 * `useRowOnScreen` holds its own placing for a press, but Chromium's scroll anchoring holds
 * nothing, and moves the page as the rows change. Measured at 1440 by 900 on c-row-layout
 * (`issue-page-g.e2e.ts`): a body closing with the page scrolled to its foot clamped the scroll
 * 46 px up, and the pressed row's body opening gave the page the room to go back, which the
 * anchoring took between the press's mousedown and its mouseup - the picture in the pressed
 * comment rose 46 px, the click went to the words around it, and no picture opened. So the focus
 * a press gives a row is taken once the press is over - after its mouseup and the click that
 * follows it, in a task of its own - and only while that row still holds the focus; a key's move
 * takes it at once. A tap on a phone sends the same mousedown, mouseup and click.
 *
 * `take` takes the focus for one item - a placement of a node, an entry of a list - and `holds`
 * answers whether that item's row holds the focus. The latest of each is the one called, so a
 * render that makes new ones while a press is held does not drop its end.
 */
export function usePressFocus<T>(
  take: (item: T) => void,
  holds: (item: T) => boolean
): PressFocus<T> {
  const latest = useRef({ holds, take });
  latest.current = { holds, take };
  // The press under way in the rows' element (`trackPress`), and the item whose row it focused,
  // which takes the focus once that press has ended.
  const press = useRef<Press | undefined>(undefined);
  const pending = useRef<{ readonly item: T } | undefined>(undefined);
  useEffect(() => {
    const listening = new AbortController();
    press.current = trackPress({
      onEnd: () => {
        const held = pending.current;
        pending.current = undefined;
        if (held !== undefined && latest.current.holds(held.item)) latest.current.take(held.item);
      },
      signal: listening.signal,
    });
    return () => listening.abort();
  }, []);
  return useMemo(
    () => ({
      focus: (item: T) => {
        if (press.current?.pressing === true) pending.current = { item };
        else latest.current.take(item);
      },
      onMouseDown: () => press.current?.start(),
    }),
    []
  );
}
