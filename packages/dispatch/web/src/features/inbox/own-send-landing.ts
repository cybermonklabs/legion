import {
  causedByReader,
  focusableIn,
  focusLost,
  focusOwnStop,
  landFirstInSight,
} from "../../components/focus-place";
import { takesText } from "../shell/keymap";
import { LIST_ATTRIBUTE, ROW_ATTRIBUTE } from "./inbox-row";

/**
 * Where the focus goes when the reader's own Send - an answer's, or a reply box's - takes its row
 * out of the Inbox's list (the own-answer rule, R1): to the first control of what the list draws
 * in that row's place, else to the nearest control before that place, never to the page's body.
 *
 * The form that held the focus calls it as it leaves the page, while its row is still drawn. It
 * reads the list's controls then: the first one after the row, outside it, and those before the
 * row, nearest first. Once the commit that took the row away has run, the first of them the reader
 * can see and that takes no typed text takes the focus (`landFirstInSight`), else the list's own
 * stop, with no scroll (`focusOwnStop`). It moves the focus only where the loss is the reader's
 * own (`causedByReader`): the form arms the hand-off at a press inside it (`armOnPress`), and the
 * Send's hold keeps that press live through the answer and the read after it
 * (`useHeldThroughAnswer`). It moves nothing where the focus is no longer lost by then, so a focus
 * the reader or the page placed meanwhile stays where it is.
 *
 * A form that leaves while its row stays - the card drawing its record in the form's place -
 * hands the focus to its own row, with no scroll, as a card does for any other loss. A form in no
 * row of the list - the reading column's - moves nothing here: the column hands the focus it lost
 * to its own stop (`Inbox.tsx`'s own hand-off).
 */
export function landAfterOwnSend(form: Element): void {
  const lost = document.activeElement;
  if (lost === null || !form.contains(lost)) return;
  const row = form.closest(`[${ROW_ATTRIBUTE}]`);
  if (row === null) return;
  const list = row.closest<HTMLElement>(`[${LIST_ATTRIBUTE}]`);
  if (list === null) return;
  const controls = focusableIn(list).filter((control) => !row.contains(control));
  const after = controls.find(
    (control) => row.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING
  );
  const before = controls
    .filter((control) => row.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_PRECEDING)
    .reverse();
  const own = form.parentElement?.closest<HTMLElement>("[tabindex]") ?? null;
  // After the commit that runs this cleanup has taken the row away.
  queueMicrotask(() => {
    if (!focusLost() || !causedByReader(lost)) return;
    if (row.isConnected) {
      own?.focus({ preventScroll: true });
      return;
    }
    const candidates = (after === undefined ? before : [after, ...before]).filter(
      (control) => control.isConnected && !takesText(control)
    );
    if (!landFirstInSight(candidates)) focusOwnStop(list);
  });
}
