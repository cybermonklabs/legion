import {
  type RefCallback,
  type RefObject,
  useCallback,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

/**
 * The ref of an element that may leave the page with the focus inside it, which hands that focus
 * on only where the reader took the element away. A loss the reader caused - their own press, a
 * save of theirs that lands, a Retry, an editor they close - is handed on to the control that
 * answers it. A loss they did not cause - another session's change, a live update, an event, a
 * read answering - leaves the focus on the page's body, as every page of Dispatch does, and moves
 * nothing. So `to` answers a loss of the first kind and returns none for one of the second. An
 * element only the reader's press takes away needs no more than a target; one a change made
 * elsewhere can take away too is handed `pressed`, which its press sets, or has `to` read what
 * the element said as it left: `to` is handed it as its last commit drew it.
 *
 * As React takes the element off the page, the ref forgets the press `pressed` holds, whether or
 * not the focus is inside the element: a press belongs to the element it was made on, so one
 * whose element left without the focus answers no later loss. The ref notes the element only
 * where the focus is inside it and, given `pressed`, a press was held; the hook's next layout
 * effect, which runs in each commit that renders the component calling the hook, then focuses
 * what `to` returns, without scrolling. The commit that takes the element away need not render
 * that component - where another component draws the element, as the repository's control draws
 * the lines the Repository row hands it - so the ref then asks for a render of it, whose layout
 * effect hands the focus on. `to` is called only then, after that commit has changed the page, so
 * it can return an element the commit drew and read what the commit's render read. Where it
 * returns none, or an element that takes no focus, the focus stays where the browser put it; a
 * focus outside the element as it leaves is not moved.
 */
export function useFocusHandOff(
  to: (left: HTMLElement) => HTMLElement | null | undefined,
  pressed?: RefObject<boolean>
): RefCallback<HTMLElement> {
  const left = useRef<HTMLElement | null>(null);
  const [, render] = useState(0);
  useLayoutEffect(() => {
    const element = left.current;
    if (element === null) return;
    left.current = null;
    to(element)?.focus({ preventScroll: true });
  });
  return useCallback(
    (element: HTMLElement | null) => {
      if (element === null) return undefined;
      // React runs this before it takes the element out of the page, so the focus is still where
      // the reader left it.
      return () => {
        const byPress = pressed === undefined || takePress(pressed);
        if (!byPress || !element.contains(document.activeElement)) return;
        left.current = element;
        render((renders) => renders + 1);
      };
    },
    [pressed]
  );
}

/** Whether `pressed` holds a press of the reader's, which the press sets. Reading it forgets the
 *  press, so each read answers one press at most. */
function takePress(pressed: RefObject<boolean>): boolean {
  const held = pressed.current;
  pressed.current = false;
  return held;
}
