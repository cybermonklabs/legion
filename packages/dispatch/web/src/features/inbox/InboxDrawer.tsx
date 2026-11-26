import { type ReactNode, useCallback, useRef, useState } from "react";

import {
  backdrop50,
  borderDefault,
  card,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useShortcutHelpOpener } from "../issue-page/shortcut-help-opener";
import { DIALOG_SCOPE, KeymapGateContext, useKeymap } from "../shell/keymap";
import { noDialogOver, useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { Inbox } from "./Inbox";
import type { InboxRoute } from "./inbox-route";

/**
 * The Inbox as a peek over whatever page is open (LEGION-547): the same list, cards and actions
 * as the Inbox page, mounted once in the app shell as a sibling of `<main>` so a route change
 * underneath never unmounts the page beneath it - the drawer itself closes instead
 * (`useCloseOnNavigation`), which is also what "open the item" from inside it does.
 *
 * Its filters, sort, grouping, Inbox-or-Dismissed switch and search text are its own `useState`,
 * never the URL: `Inbox`'s `filter`/`onFilterChange` exist exactly so a peek embedded on, say,
 * the Agents page does not navigate it to `/` the moment someone picks a person or types a word.
 * `keymapScope={DIALOG_SCOPE}` puts its j/k/Enter/Escape bindings on the scope a dialog anywhere
 * on the stack exclusively consults, instead of the standalone page's `inbox` scope, which a
 * dialog's keydown handling never reaches. `pane={false}` is the third: a peek is a strip beside
 * the page it covers, not a reader of one row at a time, so it keeps the card in the row at every
 * width, where the Inbox page lists short rows and opens one row's view. The rows scroll in this
 * `aside` rather than in the window, so it is also the box the Inbox's viewport anchor is given
 * (`scroller`): without it the anchor compensates a removed row with `window.scrollBy`, which
 * moves nothing here, and the row the reader's hand is on jumps by a card's height. What the
 * reader's browser keeps - the thread order, and which bands they folded - is theirs rather than
 * the address's, so the drawer and the page agree.
 *
 * Every key of its tree is gated on no dialog standing over the drawer (`KeymapGateContext`):
 * the Inbox's keys share the `dialog` scope with any dialog opened over the drawer - the `?`
 * help, a card's Decline box - and would otherwise act behind it.
 */
export function InboxDrawer({ onClose, open }: { onClose: () => void; open: boolean }): ReactNode {
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  // An empty route is the Inbox as it opens: the reader's own work, newest first, banded by
  // priority, every thread they have not dismissed, and nothing typed in the search box.
  const [filter, setFilter] = useState<InboxRoute>({});
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open });
  useCloseOnNavigation(open, onClose);
  // Whether no dialog stands over the drawer: what every key of its tree needs, and its `?`. That
  // holds from the commit that opens it, before `useDialog`'s effect counts it among the open
  // dialogs, so a key pressed at once after `i` is the drawer's.
  const uncovered = useCallback(
    () => noDialogOver(dialog.containerRef.current),
    [dialog.containerRef]
  );
  // `?` opens the help over the drawer (R13), which lists the Inbox's keys under Inbox. The drawer
  // is the one dialog that registers a page's keys, and the dialog scope it pushes masks the
  // shell's own `?` (`app.tsx`), so it binds the action again in that scope, on the person's keys
  // for it. The key stands down under a dialog opened over the drawer, the help among them, and in
  // a text field, the action not being `inEditable`.
  const openHelp = useShortcutHelpOpener();
  useKeymap(
    DIALOG_SCOPE,
    open && openHelp !== undefined
      ? [{ action: "global.help", run: openHelp, when: uncovered }]
      : []
  );

  if (!open) {
    return null;
  }
  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-30 ${backdrop50}`} onClick={onClose} />
      <aside
        aria-label="Inbox"
        aria-modal="true"
        className={`fixed inset-y-0 right-0 z-40 flex w-full flex-col overflow-y-auto border-l p-4 shadow-2xl md:max-w-md ${card} ${borderDefault}`}
        data-testid="inbox-drawer"
        ref={dialog.containerRef}
        role="dialog"
      >
        <KeymapGateContext.Provider value={uncovered}>
          <div className={`mb-4 flex items-center justify-between border-b pb-3 ${borderDefault}`}>
            <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>Inbox</h2>
            <button
              className={`min-h-11 rounded-lg px-3 text-sm font-medium ${textMutedOnSurface}`}
              onClick={onClose}
              ref={closeButtonRef}
              type="button"
            >
              Close
            </button>
          </div>
          <Inbox
            filter={filter}
            keymapScope={DIALOG_SCOPE}
            onFilterChange={setFilter}
            pane={false}
            scroller={dialog.containerRef}
          />
        </KeymapGateContext.Provider>
      </aside>
    </>
  );
}
