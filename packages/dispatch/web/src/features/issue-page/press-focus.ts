import { useEffect, useMemo, useRef } from "react";

/** What ends a press: its mouseup, which the click follows in the same task; the drag a press
 *  on a picture or a link starts instead, which sends no mouseup; a pointer the browser
 *  cancelled; and a key, since a press released outside the window sends no mouseup, and a
 *  key's move must never wait on a press that is over. */
export const PRESS_ENDS = ["mouseup", "dragend", "pointercancel", "keydown"] as const;

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
  // Whether a press is under way, from its mousedown until the task after it ends, and the item
  // whose row it focused, which takes the focus then.
  const press = useRef<{ ending?: number; focus?: { readonly item: T }; held: boolean }>({
    held: false,
  });
  useEffect(() => {
    const held = press.current;
    const end = () => {
      held.held = false;
      const pending = held.focus;
      held.focus = undefined;
      if (pending !== undefined && latest.current.holds(pending.item)) {
        latest.current.take(pending.item);
      }
    };
    const ended = () => {
      if (!held.held) return;
      window.clearTimeout(held.ending);
      held.ending = window.setTimeout(end, 0);
    };
    const listening = new AbortController();
    for (const type of PRESS_ENDS) {
      window.addEventListener(type, ended, { capture: true, signal: listening.signal });
    }
    return () => {
      listening.abort();
      window.clearTimeout(held.ending);
    };
  }, []);
  return useMemo(
    () => ({
      focus: (item: T) => {
        if (press.current.held) press.current.focus = { item };
        else latest.current.take(item);
      },
      onMouseDown: () => {
        window.clearTimeout(press.current.ending);
        press.current.held = true;
      },
    }),
    []
  );
}
