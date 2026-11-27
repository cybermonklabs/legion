import { createContext, type ReactNode, useContext } from "react";

import type { MyReplyRow } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { linkHoverText, linkText } from "../../theme/classes";
import type { InboxShow } from "../refs/routes";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { ThreadLine } from "./ThreadTitle";
import { dismissThreadRow, type InboxDismissRow } from "./useInboxDismiss";

/** What a thread row's Dismiss does, as the Inbox hands it to the three surfaces that draw one:
 *  the short row, the reading column's head and the card. */
export interface RowDismiss {
  /** Takes the row out of the view it is drawn in: out of the Inbox, or - in the Dismissed view -
   *  back into it. The row carries what the page drew in it, which the control reads off the row
   *  it is drawn on, so a press names what the reader was shown. */
  dismiss: (row: InboxDismissRow) => void;
  /** The row ids whose own press is still out. */
  pending: ReadonlySet<string>;
  /** The row ids whose own press is out within its deadline, which holds the control. */
  held: ReadonlySet<string>;
  /** Which view the row is drawn in, which is what its control is called. */
  show: InboxShow;
}

/**
 * The dismiss the Inbox hands to the rows it draws, as a context rather than a prop: a thread is
 * drawn by `ThreadRowCard` and by the reading column, which reaches its head through four bodies
 * of its own, and neither route carries a prop of the list's. Each Inbox provides its own - the
 * page's and the drawer's are separate - and with no provider a thread draws no Dismiss at all,
 * which is what a thread drawn outside the list does.
 */
export const RowDismissContext = createContext<RowDismiss | undefined>(undefined);

/**
 * A thread row's **Dismiss**, which takes it out of the Inbox - or, in the Dismissed view, the
 * **Move to Inbox** that puts it back. An Ask or an Approval row draws none: a question leaves the
 * Inbox by being answered or snoozed.
 *
 * It is one control in one group, **Actions**, the host's: the short row's Actions group beside
 * Copy reference, the reading column's head, the drawer card's. It never moves on its own: at a
 * narrow width it moves only as its host's Actions group moves, whole.
 *
 * On the short row it shows while the pointer is on the row or something in it holds the focus,
 * always on the row the reading column is showing (`shown`), and always on a device a reader
 * taps - one with no hover, or with any coarse pointer - at every width: there no hover reveals
 * it, so a hidden Dismiss would be a control a reader taps without seeing it - below 1280 px in a
 * row they meant to open, and beside the column, on a tablet held wide, in a row they meant to
 * select. The column's own head and the card draw it beside their other controls, where there is
 * room for it. It is held while its write is out within its deadline (`heldPress`), and says
 * what it is doing until that write is decided.
 */
export function DismissControl({
  shortcutKeys,
  shown = false,
  thread,
}: {
  /** The keys that press it, where the host draws them on the button (the reading pane's head:
   *  `e`, or the person's own): named in its `aria-keyshortcuts`, the first drawn after its
   *  label (`ControlKeyHint`). */
  shortcutKeys?: readonly string[];
  /** Whether the control is drawn whatever the reader's hand is doing: the head's and the card's
   *  always are, and so is the selected row's. */
  shown?: boolean;
  thread: MyReplyRow;
}): ReactNode {
  const dismiss = useContext(RowDismissContext);
  if (dismiss === undefined) return null;
  const named = dismissThreadRow(thread);
  const restoring = dismiss.show === "dismissed";
  const label = restoring ? "Move to Inbox" : "Dismiss";
  const live = dismiss.pending.has(named.id);
  // The label is an `aria-label`, which can hold no markup: it takes the row's one plain-text
  // line (`ThreadLine`) - the projection of the subject, from the same reader that draws the
  // row's own line and with the record that line names (`ThreadTitle`), or a deleted root's
  // deleted line - so what a reader hears and what they see are the same words: a reference
  // named, not its address.
  return (
    <ThreadLine thread={thread}>
      {(line) => (
        <button
          aria-keyshortcuts={keyshortcutsOf(shortcutKeys)}
          aria-label={`${label}: ${line ?? ""}`}
          className={`min-h-11 shrink-0 text-sm font-medium md:min-h-8 ${linkText} ${linkHoverText} ${
            shown
              ? ""
              : "opacity-0 group-focus-within:opacity-100 group-hover:opacity-100 [@media(hover:none)]:opacity-100 [@media(any-pointer:coarse)]:opacity-100"
          }`}
          data-inbox-dismiss={named.id}
          {...heldPress(dismiss.held.has(named.id), () => dismiss.dismiss(named))}
          type="button"
        >
          {live ? (restoring ? "Moving…" : "Dismissing…") : label}
          <ControlKeyHint keys={shortcutKeys} />
        </button>
      )}
    </ThreadLine>
  );
}
