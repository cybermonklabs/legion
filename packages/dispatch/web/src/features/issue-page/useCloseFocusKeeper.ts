import { type FocusEventHandler, type RefObject, useEffect, useLayoutEffect, useRef } from "react";

import { focusOnDocument } from "../shell/roving";

/** Whether a close of the issue took `element` from the reader: took it off the page, or
 *  disabled it. The keeper reads this one rule both as the focus leaves an element and as the
 *  issue closes, so what it keeps on the one is what it hands on at the other. */
function closeTook(element: Element): boolean {
  return !element.isConnected || element.matches(":disabled");
}

/**
 * Keeps the focus of Details' Development section through a close made in another session.
 * That close disables some of Development's controls - the route's control and its editor's
 * box, the labels' control, the priority's line, the Repository row, the parent's editor's box,
 * each `disabled` on a closed issue - and takes others off the page - Set or Edit parent issue,
 * Blocked by's control - and each takes the focus it held with it to the page's body. No hand-off
 * of the panel answers that, since each acts only as an element it holds leaves the page or as an
 * editor closes. Add and its menu go too (`IssueAddMenu`), and this hands their focus on as well.
 * The panel's keeper (`useFocusKeeper`) hears each of these losses too, but it waits one task, so
 * it leaves the focus where this put it.
 *
 * So the section notes the element the focus last went to in it (`onFocus`), which a focus
 * anywhere else forgets, as does the reader's leaving it (`onBlur`); and as the issue closes,
 * where the close took that element (`closeTook`) and the focus is still on it or on the body,
 * the focus goes to `headingRef`, Development's heading, where every other hand-off of the
 * section ends. A control that only refuses a press (`aria-disabled`: an editor's Save and
 * Cancel while its save is out, and the parent's and the route's Save on a closed issue) keeps
 * the focus, so a close took nothing from it and the focus stays there, as a focus a hand-off
 * already moved to a control that takes it does. An issue closed as the panel is drawn moves
 * nothing.
 *
 * `isClosed` is whether the issue is closed now. The panel calls this after each of its
 * `useFocusHandOff`s, so that in the commit that closes the issue a hand-off has moved its focus
 * on before this reads where the focus is. It answers the section's `onFocus` and `onBlur`.
 */
export function useCloseFocusKeeper(
  isClosed: boolean,
  headingRef: RefObject<HTMLElement | null>
): { onBlur: FocusEventHandler<HTMLElement>; onFocus: FocusEventHandler<HTMLElement> } {
  const noted = useRef<HTMLElement | null>(null);
  const onFocus: FocusEventHandler<HTMLElement> = (event) => {
    noted.current = event.target;
  };
  // A press on Details' plain text moves the focus to the body and sends no `focusin`, so the
  // listener below never hears the reader leave, and a later close would move the focus to the
  // heading of a section the reader had already left. So an element the focus leaves is
  // forgotten, whether it takes a press or only refuses one while it is busy, unless the close
  // took it: a control a close disables is `disabled` as Chromium blurs it, and React sends no
  // `onBlur` for one a close takes off the page, so it stays noted. A switch to another window or
  // tab blurs the control too while the document itself has no focus, and the focus comes back to
  // the control on return, so a blur while the document has no focus forgets nothing.
  const onBlur: FocusEventHandler<HTMLElement> = (event) => {
    if (document.hasFocus() && !closeTook(event.target)) {
      noted.current = null;
    }
  };
  useEffect(() => {
    // In the capture phase of the document, so it runs before the section's `onFocus`, which
    // React calls from its root and which notes the focus again where it went to Development. It
    // also forgets what the section's `onBlur` keeps - an element the focus left as it was
    // disabled or taken off the page, or as the reader switched windows - once the focus goes to
    // an element again.
    const forget = () => {
      noted.current = null;
    };
    document.addEventListener("focusin", forget, true);
    return () => document.removeEventListener("focusin", forget, true);
  }, []);
  const wasClosed = useRef(isClosed);
  // A layout effect, so it runs in the commit that closes the issue, after the layout effects of
  // the panel's `useFocusHandOff`s in that same commit (an editor's close hands its focus on in a
  // passive effect, which runs after this one), and before a browser that moves the focus off a
  // disabled control only later does so.
  useLayoutEffect(() => {
    const closing = isClosed && !wasClosed.current;
    wasClosed.current = isClosed;
    const left = noted.current;
    if (!closing || left === null || !closeTook(left)) return;
    if (document.activeElement !== left && !focusOnDocument()) return;
    headingRef.current?.focus({ preventScroll: true });
  }, [headingRef, isClosed]);
  return { onBlur, onFocus };
}
