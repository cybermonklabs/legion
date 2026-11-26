import { type RefObject, useEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";

/**
 * The control that opened an inline editor, and what gives it the focus back once the editor
 * ends: the issue header's title, message route and parent editors (`IssueHeader`), the new
 * issue page's title (`IssueTitleRow`), a priority's fields drawn closed (`InPlaceField`), a
 * decision's Change and the Priorities page's New priority, whose forms their Cancel ends
 * (`DecisionActions`, `NewPriority`), a comment's Edit and a margin card's preview, which Escape,
 * Cancel reply and Discard draft in its reply box fold it back to (`ThreadCard`), and a
 * Conversation comment's Expand thread, whose thread those close (`ConversationTab`'s
 * `CommentTurn`). A key or a button inside the editor that ends the edit calls `handBack` before
 * it ends it: the editor leaves the page with the focus it held, and once the commit that ends
 * the edit has drawn `opener` again, the focus goes to it with no scroll. It goes there only from
 * the page's body, where the editor's going left it (`focusOnDocument`): an editor that a key
 * asked to end but that stays open - its refused save holds it (`ThreadCard`) - ends later, and a
 * control the reader put the focus on meanwhile keeps it. An edit the reader ended by moving the
 * focus elsewhere - a click, Tab - calls no `handBack`, so the focus stays where they put it, and
 * a control the page does not draw takes nothing. `editing` is whether the editor `handBack` ends
 * is open. It is read after every commit, not only when it changes, so a host with several
 * editors names in it the one a key ended, whatever other editor stays open, from the commit that
 * closes it on (`ThreadCard`).
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
    if (focusOnDocument()) opener.current?.focus({ preventScroll: true });
  });
  return {
    handBack: () => {
      returning.current = true;
    },
    opener,
  };
}
