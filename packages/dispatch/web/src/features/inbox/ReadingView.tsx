import { type ReactNode, type RefCallback, type RefObject, useLayoutEffect, useRef } from "react";

import type { InboxRow } from "../../api/types";
import { focusVisibleRing, linkHoverText, linkText } from "../../theme/shell-classes";
import { COPY_REF_SELECTOR } from "../refs/CopyRefButton";
import { closestMatching } from "../shell/roving";
import { ROW_SELECTOR } from "./inbox-row";
import { type InboxListRow, rowId } from "./list-row";
import { MarkCheckbox, ThreadMarkCheckbox } from "./MarkCheckbox";
import { useControlName } from "./useControlName";

/** The reading column's own scroller: `l` hands it focus and Escape takes it back to the list. */
export const PANE_SCROLLER_SELECTOR = '[data-testid="inbox-pane-scroller"]';

/** The Snooze the reading view's head draws for the question it shows (`ReadingPaneHead`). */
export const SNOOZE_SELECTOR = "[data-inbox-snooze]";

/** Where the reading view below 1280 px takes the reader once it has drawn the row they opened:
 *  its scroller, where the next Enter reaches the answer box; its head's Snooze (`h`); its head's
 *  Copy reference (`y`); or the question's option at `index` (`1`-`9`), which is pressed, and the
 *  scroller where the question has no such option. */
export type ReadingViewTarget =
  | { readonly kind: "copy-ref" }
  | { readonly kind: "option"; readonly index: number }
  | { readonly kind: "scroller" }
  | { readonly kind: "snooze" };

/** The target a pick that names none lands on. */
export const READING_VIEW_SCROLLER: ReadingViewTarget = { kind: "scroller" };

/** What a target comes to once the view has drawn it: an element to focus, or a control to press. */
type Landing = { readonly focus: HTMLElement } | { readonly press: HTMLElement };

/** The landing `target` names in `view` for the row `askId`, or null while the view has not drawn
 *  it yet: a decision block or an approval draws a skeleton first. */
function landingIn(view: HTMLElement, askId: string, target: ReadingViewTarget): Landing | null {
  const scroller = (): Landing | null => {
    const element = view.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR);
    return element === null ? null : { focus: element };
  };
  switch (target.kind) {
    case "scroller":
      return scroller();
    case "snooze": {
      const element = view.querySelector<HTMLElement>(SNOOZE_SELECTOR);
      return element === null ? null : { focus: element };
    }
    case "copy-ref": {
      const element = view.querySelector<HTMLElement>(COPY_REF_SELECTOR);
      return element === null ? null : { press: element };
    }
    case "option": {
      const card = view.querySelector(`[data-ask-card="${CSS.escape(askId)}"]`);
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
 * moves to beside the list, where the next Enter reaches the answer box. A decision block or an
 * approval draws a skeleton first, so the reader lands in the first commit or DOM change that
 * draws the target, and never on the view's Inbox control in its place, whose own Enter closes
 * the view. A reader who has put the focus somewhere of their own meanwhile keeps it there, and
 * a selection that moves to another row first drops the landing. Beside the list (`columns`) a
 * pick lands nowhere: the reader stays in the list.
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
      if ("press" in landing) landing.press.click();
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
