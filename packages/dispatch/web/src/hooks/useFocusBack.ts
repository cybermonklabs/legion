import { type RefObject, useEffect, useRef } from "react";

/**
 * The control that opened an inline editor, and what gives it the focus back once the editor
 * ends: the issue header's title, message route and parent editors (`IssueHeader`), the new
 * issue page's title (`IssueTitleRow`) and a priority's fields drawn closed (`InPlaceField`). A
 * key or a button inside the editor that ends the edit calls `handBack` before it ends it: the
 * editor leaves the page with the focus it held, and once the commit that ends the edit has drawn
 * `opener` again, the focus goes to it with no scroll. An edit the reader ended by moving the
 * focus elsewhere - a click, Tab - calls no `handBack`, so the focus stays where they put it, and
 * a control the page does not draw takes nothing. `editing` is whether the editor is open.
 */
export function useFocusBack<Opener extends HTMLElement = HTMLButtonElement>(
  editing: boolean
): {
  handBack: () => void;
  opener: RefObject<Opener | null>;
} {
  const opener = useRef<Opener>(null);
  const returning = useRef(false);
  useEffect(() => {
    if (editing || !returning.current) return;
    returning.current = false;
    opener.current?.focus({ preventScroll: true });
  }, [editing]);
  return {
    handBack: () => {
      returning.current = true;
    },
    opener,
  };
}
