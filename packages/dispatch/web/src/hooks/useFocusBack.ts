import { type RefObject, useEffect, useRef } from "react";

import { focusOnDocument } from "../features/shell/roving";

/**
 * The control that opened an inline editor, and what gives it the focus back once the editor
 * ends: the issue header's title, message route and parent editors (`IssueHeader`), the new
 * issue page's title (`IssueTitleRow`), a priority's fields drawn closed (`InPlaceField`), a
 * decision's Change, whose form its Cancel ends (`DecisionActions`), the Priorities page's New
 * priority, whose form its Cancel and its submit end (`NewPriority`), the Priorities page's empty
 * state, which the first priority added from it ends as it lands, with the table's New priority
 * as its opener (`PrioritiesPage`), a comment's Edit, whose editor a second Escape over `Discard
 * draft?` or its Discard ends, and a margin card's preview, which Escape, Cancel reply and Discard
 * draft in the card's reply box fold it back to (`ThreadCard`), and a Conversation comment's
 * Expand thread, whose thread those three close (`ConversationTab`'s `CommentTurn`). A key or a
 * button inside the editor that ends the edit calls `handBack` before it ends it: the editor
 * leaves the page with the focus it held, and once the commit that ends the edit has drawn
 * `opener` again, the focus goes to it with no scroll. It goes there only from the page's body,
 * where the editor's going left it (`focusOnDocument`): an editor that a key asked to end but
 * that stays open - its refused save holds it (`ThreadCard`) - ends later, and a control the
 * reader put the focus on meanwhile keeps it. An edit the reader ended by moving the focus
 * elsewhere - a click, Tab - calls no `handBack`, so the focus stays where they put it, and a
 * control the page does not draw takes nothing. `editing` is whether the editor `handBack` ends
 * is open. It is read after every commit, not only when it changes, so a host with several
 * editors names in it the one a key ended, whatever other editor stays open, from the commit that
 * closes it on (`ThreadCard`). `letGo` takes back a `handBack` whose editor will not end as it
 * asked, so a commit that ends it later for another reason gives no focus: the Priorities page's
 * empty state, once every add sent from it has ended and the read after it drew no priority
 * (`PrioritiesPage`).
 */
export function useFocusBack<Opener extends HTMLElement = HTMLButtonElement>(
  editing: boolean
): {
  handBack: () => void;
  letGo: () => void;
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
    letGo: () => {
      returning.current = false;
    },
    opener,
  };
}
