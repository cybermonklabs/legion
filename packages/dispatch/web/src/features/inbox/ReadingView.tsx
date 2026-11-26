import { type ReactNode, type RefCallback, type RefObject, useLayoutEffect, useRef } from "react";

import type { InboxRow } from "../../api/types";
import { focusVisibleRing, linkHoverText, linkText } from "../../theme/classes";
import { openReplyBox, REPLY_BOX } from "../conversation/turn-keys";
import { PRIORITY_TRIGGER_SELECTOR } from "../issue/PriorityControl";
import { ITEM_COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { closestMatching } from "../shell/roving";
import type { AskApprovalAction } from "./AskOptionRow";
import { ROW_SELECTOR } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
import { MarkCheckbox, ThreadMarkCheckbox } from "./MarkCheckbox";
import { PANE_SCROLLER_SELECTOR } from "./pane-placement";
import { ROW_SNOOZE_SELECTOR } from "./SnoozeControl";
import { useControlName } from "./useControlName";

/** The "Add a note or answer in your own words" disclosure an `AskCard` folds its answer box
 *  behind in its compact variant - the card a decision block hosts, which is what the reading
 *  column shows for a question written as one - while it is still shut. The card carries the
 *  handle (`AskCard.tsx`), as it carries `data-ask-answer` and `data-ask-option`. */
export const NOTE_DISCLOSURE_SELECTOR = '[data-ask-note-disclosure][aria-expanded="false"]';

/** A thread drawn with no reply box in it and none to come while what it read stands, by the
 *  thread's own id (`data-inbox-no-reply-box`): a decision document draws several asks at once,
 *  so only the thread's own mark speaks for it. Each part that leaves the box out writes it, from
 *  the fact it leaves the box out by (`ThreadBody.tsx`): a comment or message thread whose root
 *  the read does not hold or its author deleted; an Agent row's whose session the live list does
 *  not hold or whose conversation the session's read does not hold; any thread whose read
 *  failed, and one that draws nothing at all. So does an ask's card while the question is still
 *  open, which takes no reply (`AskCard`, by `takesReply`), and a decision block's replies whose
 *  read failed (`AskThreadDisclosure`). Below 1280 px `r` lands at the reading view's scroller
 *  there rather than waiting for a box (`landingIn`); a thread whose reads are still out carries
 *  neither the box nor the mark. */
function noReplyBoxSelector(threadId: string): string {
  return `[data-inbox-no-reply-box="${CSS.escape(threadId)}"]`;
}

/** The head's Approve or Request changes (`ApprovalChip`'s `data-approval-action`) while it takes
 *  a press: the head draws the button from its first draw and holds it (`aria-disabled`) until
 *  the document's read returns (`ReadingPaneHead`). What `a` and Shift+R press in the column and
 *  the reading view (`useInboxKeymap.ts`), and what a short row's `a` and Shift+R wait for in the
 *  view they open (`landingIn`). */
export function headApprovalSelector(action: AskApprovalAction): string {
  return `[data-inbox-pane-head] [data-approval-action="${action}"]:not([aria-disabled="true"])`;
}

/** Where the reading view below 1280 px takes the reader once it has drawn the row they opened:
 *  its scroller, the focus stop `l` moves to beside the list; the question's answer box, or the
 *  note box its decision block folds it behind, with the caret in it (Enter, R18); the thread's
 *  reply box, or the "Write a reply" it is folded behind, and the scroller where the thread is
 *  drawn with no box (`r`, `noReplyBoxSelector`); its head's Snooze or priority menu, open (`b`
 *  or `h`, `p`; R4); its head's Approve or Request changes, pressed once the document's read has
 *  enabled it (`a`, Shift+R; R7); its head's Copy reference (`y`); or the question's option at
 *  `index` (the digits), which is pressed, and the scroller where the question has no such
 *  option. */
export type ReadingViewTarget =
  | { readonly kind: "answer" }
  | { readonly kind: "approve" }
  | { readonly kind: "copy-ref" }
  | { readonly kind: "option"; readonly index: number }
  | { readonly kind: "priority" }
  | { readonly kind: "reply"; readonly threadId: string }
  | { readonly kind: "request-changes" }
  | { readonly kind: "scroller" }
  | { readonly kind: "snooze" };

/** The target a pick that names none lands on. */
export const READING_VIEW_SCROLLER: ReadingViewTarget = { kind: "scroller" };

/** What a target comes to once the view has drawn it: an element to focus; a control to press,
 *  from the view's scroller, so what the press opens gives the focus back there; a menu's trigger,
 *  focused and pressed, so the menu gives the focus back to it as it closes; or a "Write a reply"
 *  to open with the caret in its box (`openReplyBox`). */
type Landing =
  | { readonly focus: HTMLElement }
  | { readonly press: HTMLElement }
  | { readonly menu: HTMLElement }
  | { readonly reply: HTMLElement };

/** The landing `target` names in `view` for the row `askId`, or null while the view has not drawn
 *  it yet: a decision block or an approval draws a skeleton first, and a thread draws its box, or
 *  the mark that it has none (`noReplyBoxSelector`), only once its reads have answered. */
function landingIn(view: HTMLElement, askId: string, target: ReadingViewTarget): Landing | null {
  const scroller = (): Landing | null => {
    const element = view.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR);
    return element === null ? null : { focus: element };
  };
  const card = view.querySelector(`[data-ask-card="${CSS.escape(askId)}"]`);
  switch (target.kind) {
    case "scroller":
      return scroller();
    case "answer": {
      if (card === null) return null;
      const box = card.querySelector<HTMLElement>("[data-ask-answer]");
      if (box !== null) return { focus: box };
      const disclosure = card.querySelector<HTMLElement>(NOTE_DISCLOSURE_SELECTOR);
      return disclosure === null ? scroller() : { press: disclosure };
    }
    case "reply": {
      const box = view.querySelector<HTMLElement>(`[data-inbox-thread-body] ${REPLY_BOX}`);
      if (box !== null) return { focus: box };
      const opener = view.querySelector<HTMLElement>(
        `[data-reply-opener="${CSS.escape(target.threadId)}"]`
      );
      if (opener !== null) return { reply: opener };
      // A thread drawn whole with no box lands where any pick does: its own mark, never another
      // thread's, which a decision document draws beside it. One whose reads are still out is
      // waited for.
      return view.querySelector(noReplyBoxSelector(target.threadId)) === null ? null : scroller();
    }
    case "approve":
    case "request-changes": {
      // The press waits for the document's read, which lets the head's button take it
      // (`headApprovalSelector`).
      const element = view.querySelector<HTMLElement>(headApprovalSelector(target.kind));
      return element === null ? null : { press: element };
    }
    case "snooze": {
      const element = view.querySelector<HTMLElement>(ROW_SNOOZE_SELECTOR);
      return element === null ? null : { menu: element };
    }
    case "priority": {
      const element = view.querySelector<HTMLElement>(
        `[data-inbox-pane-head] ${PRIORITY_TRIGGER_SELECTOR}`
      );
      return element === null ? null : { menu: element };
    }
    case "copy-ref": {
      const element = view.querySelector<HTMLElement>(ITEM_COPY_REF_SELECTOR);
      return element === null ? null : { press: element };
    }
    case "option": {
      if (card === null) return null;
      const option = card.querySelectorAll<HTMLElement>("[data-ask-option]")[target.index];
      return option === undefined ? scroller() : { press: option };
    }
  }
}

/**
 * Below 1280 px, the reader's pick of a row by key or click: opens the row's reading view
 * (`open`) and, once it has drawn, takes the reader to `target` in it. The pick hid the row they
 * picked with the list, so the view takes the focus: its scroller by default, the focus stop `l`
 * moves to beside the list, where Enter reaches the answer box; Enter on the row itself lands the
 * caret in that box at once (R18). A decision block or an approval draws a skeleton first, so
 * the reader lands in the first commit or DOM change that draws the target, and never on the
 * view's Inbox control in its place, whose own Enter closes the view. A reader who has put the
 * focus somewhere of their own meanwhile keeps it there, and a selection that moves to another
 * row first drops the landing. Beside the list (`columns`) a pick lands nowhere: the reader
 * stays in the list.
 */
export function useReadingViewLanding({
  columns,
  open,
  openId,
  viewRef,
}: {
  columns: boolean;
  /** Opens the row, the selection's own pick (`PaneSelection.open`). */
  open: (id: string) => void;
  /** The row the reading view shows this commit (`PaneSelection.id`). */
  openId: string | undefined;
  /** The reading view, or the column beside the list: where the target is looked up. */
  viewRef: RefObject<HTMLElement | null>;
}): (id: string, target?: ReadingViewTarget) => void {
  const pending = useRef<{ id: string; target: ReadingViewTarget } | null>(null);
  useLayoutEffect(() => {
    const want = pending.current;
    const view = viewRef.current;
    if (want === null) return;
    if (want.id !== openId) {
      pending.current = null;
      return;
    }
    if (view === null) return;
    const land = (): boolean => {
      const active = document.activeElement;
      if (
        active !== null &&
        active !== document.body &&
        closestMatching(active, ROW_SELECTOR) === null
      ) {
        pending.current = null;
        return true;
      }
      const landing = landingIn(view, want.id, want.target);
      if (landing === null) return false;
      pending.current = null;
      if ("press" in landing) {
        // The press is made from the view's own focus stop, its scroller: the row the key was
        // pressed on went with the list, and by the time a read lets the press land a browser has
        // moved that hidden row's focus to the page's body. So what the press opens - Request
        // changes' box (`useDialog`) - gives the focus back to the scroller as it closes, as it
        // does to the scroller a key pressed inside the view was pressed on.
        view.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR)?.focus({ preventScroll: true });
        landing.press.click();
      } else if ("menu" in landing) {
        landing.menu.focus({ preventScroll: true });
        landing.menu.click();
      } else if ("reply" in landing) openReplyBox(landing.reply);
      else landing.focus.focus({ preventScroll: true });
      return true;
    };
    if (land()) return;
    const observer = new MutationObserver(() => {
      if (land()) observer.disconnect();
    });
    observer.observe(view, { childList: true, subtree: true });
    return () => observer.disconnect();
  });
  return (id, target = READING_VIEW_SCROLLER) => {
    if (!columns) pending.current = { id, target };
    open(id);
  };
}

/** The reading view's own mark for a question, named as the question's row names it
 *  (`useControlName`): the question's plain-text line, with `reads` mounted beside it to read the
 *  names of the references it holds. */
function AskReadingViewMark({
  ask,
  marked,
  onMark,
}: {
  ask: InboxRow;
  marked: boolean;
  onMark: (id: string) => void;
}): ReactNode {
  const { name, reads } = useControlName(ask, undefined);
  return (
    <>
      {reads}
      <MarkCheckbox caption="Select" id={ask.id} marked={marked} name={name} onMark={onMark} />
    </>
  );
}

/**
 * Below 1280 px, the row the reader opened, read in place of the list and its header, in three
 * named groups in this order:
 *
 * - **Back**: the Inbox control, which returns to the list at the row the reader left, as the
 *   browser's Back and Escape do.
 * - **Mark**: the row's own checkbox, which `x` ticks here as on the row. Back and Mark share the
 *   first line; too narrow for both, Mark moves whole to a line of its own.
 * - **Body**: the row's reading pane (`children`), the same one the column beside the list draws
 *   from 1280 px, in the height left under the first line, scrolling in itself.
 */
export function ReadingView({
  children,
  marked,
  onBack,
  onMark,
  row,
  viewRef,
}: {
  children: ReactNode;
  marked: boolean;
  onBack: () => void;
  onMark: (id: string) => void;
  row: InboxListRow;
  /** The view's box, where the Inbox's keys act while it shows (`holdPane`). */
  viewRef: RefCallback<HTMLDivElement>;
}): ReactNode {
  return (
    // Two rows, the view's own line and `minmax(0, 1fr)` for the body, for the reason the
    // columns' grid has one: the body scrolls in itself.
    <div
      className="grid min-h-0 min-w-0 flex-1 grid-rows-[auto_minmax(0,1fr)] gap-2"
      data-inbox-reading-row={rowId(row)}
      data-testid="inbox-reading-view"
      ref={viewRef}
    >
      <div className="flex flex-wrap items-center gap-x-4">
        <button
          aria-label="Back to the Inbox list"
          className={`inline-flex min-h-11 items-center gap-1 rounded text-sm font-medium outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`}
          data-inbox-back=""
          onClick={onBack}
          type="button"
        >
          <span aria-hidden="true">←</span>
          Inbox
        </button>
        {row.kind === "ask" ? (
          <AskReadingViewMark ask={row.ask} marked={marked} onMark={onMark} />
        ) : (
          <ThreadMarkCheckbox
            caption="Select"
            marked={marked}
            onMark={onMark}
            row={row}
            thread={row.thread}
          />
        )}
      </div>
      <div className="min-h-0 min-w-0">{children}</div>
    </div>
  );
}
